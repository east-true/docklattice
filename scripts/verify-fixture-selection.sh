#!/bin/sh
set -eu

# Fixture selection is the boundary that keeps a matrix from driving its
# writes, backups, restores, and Compose runs into projects the operator owns.
# It is therefore tested directly rather than only through a full matrix run:
# the functions below are extracted from the runners themselves, so this
# verifier fails if a runner's real text stops behaving.
#
# It needs no Docker and creates no containers.

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

failures=0

note_failure() {
    printf 'fixture selection verification failed: %s\n' "$*" >&2
    failures=$((failures + 1))
}

# extract_functions prints the named shell functions from a runner. The runners
# stay self-contained; this reads their text instead of making them source a
# shared file.
extract_functions() {
    file=$1
    shift
    for want_name in "$@"; do
        awk -v want="$want_name" '
            $0 == want "() {" { inside = 1 }
            inside { print }
            inside && $0 == "}" { inside = 0; found = 1 }
            END { if (!found) exit 1 }
        ' "$file" || {
            printf 'fixture selection verification failed: %s lacks %s()\n' "$file" "$want_name" >&2
            exit 1
        }
    done
}

# uid_for mirrors the Agent: sha256(agent_id || NUL || working directory).
uid_for() {
    printf '%s\000%s' "$1" "$2" | sha256sum | awk '{ print $1 }'
}

agent="11111111-1111-4111-8111-111111111111"
root="/tmp/docklattice-fixture-selection/projects"
name="docklattice-fixture"
fixture_uid_value=$(uid_for "$agent" "$root")
foreign_uid=$(uid_for "$agent" "/home/operator/real-project")
same_name_other_root_uid=$(uid_for "$agent" "/home/operator/also-named")

project_json() {
    printf '{"uid":"%s","name":"%s","working_dir":"%s"}' "$1" "$2" "$3"
}

dashboard() {
    printf '{"hosts":[{"id":"%s","state":"ACTIVE"}],"projects":[%s]}' "$agent" "$1"
}

fixture_entry=$(project_json "$fixture_uid_value" "$name" "$root")
foreign_entry=$(project_json "$foreign_uid" "operator-stack" "/home/operator/real-project")
# A uid that sorts ahead of anything the fixture can produce.
sorts_first_entry=$(project_json "0000000000000000000000000000000000000000000000000000000000000000" \
    "aaa-operator-stack" "/home/operator/sorts-first")
same_name_entry=$(project_json "$same_name_other_root_uid" "$name" "/home/operator/also-named")
impostor_entry=$(project_json "$foreign_uid" "$name" "$root")

# run_case and run_guard evaluate the extracted functions against one input
# and print the outcome. Each runs as its own sh process: a subshell placed in
# an AND-OR list has errexit disabled, which would hide exactly the aborts
# these cases exist to prove.
run_script() {
    library=$1
    shift
    {
        printf 'set -eu\n'
        printf "compose_project='%s'\n" "$name"
        printf "agent_id='%s'\n" "$agent"
        printf "fixture_root='%s'\n" "$root"
        printf 'fixture_uids=\n'
        printf "fail() { printf 'fail: %%s\\n' \"\$*\" >&2; exit 1; }\n"
        printf ". '%s'\n" "$library"
        cat
    } >"$work/case.sh"
    sh "$work/case.sh" >"$work/out.txt" 2>"$work/err.txt" || printf 'fail\n' >"$work/out.txt"
    cat "$work/out.txt"
}

run_case() {
    library=$1
    projects=$2
    dashboard "$projects" >"$work/dashboard.json"
    run_script "$library" <<EOF
selected=\$(select_fixture_project '$work/dashboard.json' "\$fixture_root")
printf 'ok %s\n' "\$selected"
EOF
}

run_guard() {
    library=$1
    url=$2
    body=$3
    run_script "$library" <<EOF
allow_fixture_uid '$fixture_uid_value'
guard_project_target '$url' '$body'
printf 'allowed\n'
EOF
}

expect() {
    label=$1
    want=$2
    got=$3
    [ "$want" = "$got" ] || note_failure "$label: want [$want], got [$got]"
}

for runner in run-hardening-matrix-e2e.sh run-abuse-matrix-e2e.sh; do
    path="$repo_dir/scripts/$runner"
    library="$work/$runner.functions"
    extract_functions "$path" \
        expected_fixture_uid find_fixture_project select_fixture_project \
        allow_fixture_uid is_fixture_uid guard_project_target >"$library"
    # The extracted text must be usable on its own.
    sh -n "$library" || note_failure "$runner: extracted fixture functions are not valid shell"

    # A clean host: the fixture is the only project.
    expect "$runner: sole project" "ok $fixture_uid_value" \
        "$(run_case "$library" "$fixture_entry")"

    # A working host: several unrelated projects are present.
    expect "$runner: multiple projects" "ok $fixture_uid_value" \
        "$(run_case "$library" "$foreign_entry,$fixture_entry,$sorts_first_entry")"

    # Position must not matter, including when another project sorts first.
    expect "$runner: fixture last" "ok $fixture_uid_value" \
        "$(run_case "$library" "$sorts_first_entry,$foreign_entry,$fixture_entry")"
    expect "$runner: fixture first" "ok $fixture_uid_value" \
        "$(run_case "$library" "$fixture_entry,$sorts_first_entry,$foreign_entry")"

    # Same project name at a different root is a different project.
    expect "$runner: same name other root" "fail" \
        "$(run_case "$library" "$same_name_entry,$foreign_entry")"
    expect "$runner: same name other root does not shadow" "ok $fixture_uid_value" \
        "$(run_case "$library" "$same_name_entry,$fixture_entry")"

    # Absence and ambiguity both fail closed.
    expect "$runner: fixture absent" "fail" "$(run_case "$library" "$foreign_entry")"
    expect "$runner: no projects at all" "fail" "$(run_case "$library" "")"
    expect "$runner: duplicate identity" "fail" \
        "$(run_case "$library" "$fixture_entry,$fixture_entry")"

    # A project claiming the fixture identity with a uid the fixture root
    # cannot derive is refused rather than trusted.
    expect "$runner: underivable uid" "fail" "$(run_case "$library" "$impostor_entry")"

    # The request guard accepts only identities the harness registered.
    expect "$runner: guard allows fixture url" "allowed" \
        "$(run_guard "$library" "https://h/api/v1/projects/$fixture_uid_value/files?path=compose.yaml" '')"
    expect "$runner: guard refuses foreign url" "fail" \
        "$(run_guard "$library" "https://h/api/v1/projects/$foreign_uid/files?path=compose.yaml" '')"
    expect "$runner: guard refuses foreign write url" "fail" \
        "$(run_guard "$library" "https://h/api/v1/projects/$foreign_uid/backups" '{"operation_id":"x"}')"
    expect "$runner: guard allows fixture body" "allowed" \
        "$(run_guard "$library" "https://h/api/v1/operations" \
            "{\"operation_id\":\"x\",\"project_uid\":\"$fixture_uid_value\",\"kind\":\"compose.up\"}")"
    expect "$runner: guard refuses foreign body" "fail" \
        "$(run_guard "$library" "https://h/api/v1/operations" \
            "{\"operation_id\":\"x\",\"project_uid\":\"$foreign_uid\",\"kind\":\"compose.down\"}")"
    expect "$runner: guard refuses unregistered fixture" "fail" \
        "$(run_guard "$library" "https://h/api/v1/projects/$same_name_other_root_uid/files" '')"
done

# The runners must not reintroduce a position-based project target, in any of
# its spellings. The one permitted [0] is inside find_fixture_project, which
# has already proved the match is unique.
for runner in run-hardening-matrix-e2e.sh run-abuse-matrix-e2e.sh; do
    if grep -F -- '.projects[0]' "$repo_dir/scripts/$runner" >/dev/null; then
        note_failure "$runner: selects a project by list position"
    fi
    # Filtering hosts by an exact id is an identity match, not a position, so
    # only project lists are counted here.
    positional=$(grep -- '\]\[0\]' "$repo_dir/scripts/$runner" | grep -c -- '\.projects\[\]' || true)
    [ "$positional" -le 1 ] ||
        note_failure "$runner: $positional project list positions are used to pick a target, want at most the guarded lookup"
    grep -F -- 'select(.collision == true)][0]' "$repo_dir/scripts/$runner" >/dev/null &&
        note_failure "$runner: picks a colliding project by list position" || true
done

# The clean-host gate keeps .projects[0], but only behind an assertion that
# there is exactly one project and that it is the fixture.
clean_host="$repo_dir/scripts/run-clean-host-install-e2e.sh"
grep -F -- '(.projects | length) == 1' "$clean_host" >/dev/null ||
    note_failure "run-clean-host-install-e2e.sh: .projects[0] is not pinned to a single project"
grep -F -- 'SKIPPED_NOT_CLEAN' "$clean_host" >/dev/null ||
    note_failure "run-clean-host-install-e2e.sh: a host that is not clean is not reported as such"

[ "$failures" -eq 0 ] || exit 1
printf 'fixture selection is identity-based and fails closed\n'
