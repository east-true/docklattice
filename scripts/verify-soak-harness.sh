#!/bin/sh
set -eu

# The soak harness runs for hours against a host that is also doing the
# operator's work, so its safety boundary and its verdict rule are checked
# statically before it is trusted. This needs no Docker and starts nothing.

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
runner="$repo_dir/scripts/run-soak-e2e.sh"

[ -x "$runner" ] || {
    printf 'soak harness verification failed: runner is not executable\n' >&2
    exit 1
}
sh -n "$runner"

require_literal() {
    grep -F -- "$1" "$runner" >/dev/null || {
        printf 'soak harness verification failed: runner lacks %s\n' "$1" >&2
        exit 1
    }
}

# The safety boundary is the same one every other matrix keeps, plus the
# fixture identity rules, because a soak mutates the same host for hours.
for literal in \
    'all image arguments must be exact local sha256 image IDs' \
    '[ ! -e "$evidence_dir" ]' \
    'command -v "$tool"' \
    "'{{.OSType}}'" \
    "'{{.CgroupVersion}}'" \
    '/var/run/docker.sock' \
    '--pull never' \
    'select_fixture_project' \
    'find_fixture_project' \
    'allow_fixture_uid' \
    'is_fixture_uid' \
    'guard_project_target' \
    'which this harness did not create' \
    'dashboard projects claim the fixture identity' \
    'label=com.docker.compose.project=$compose_project' \
    'status=PASS' \
    'seal_evidence'; do
    require_literal "$literal"
done

# The measurements the soak exists to take, and the closing invariants.
for literal in \
    'rss_kib' 'threads' 'fds' 'state_kib' 'audit.lag' 'coverage_revision' \
    'monotonic_rise' 'tolerance_percent' \
    'the Server logged SQLite contention during the run' \
    'the run recorded an OOM event' \
    'invariant: the project lock is still held by nothing' \
    'invariant: a restore journal survived a settled soak' \
    'invariant: staging files were orphaned in the project directory' \
    'invariant: the acknowledged cursor passed the Server delivery cursor' \
    'invariant: the project secret leaked into recorded evidence'; do
    require_literal "$literal"
done

# A soak must never reach for a project by list position.
if grep -F -- '.projects[0]' "$runner" >/dev/null; then
    printf 'soak harness verification failed: a project is selected by list position\n' >&2
    exit 1
fi

# Nothing may be built, pulled, or fetched during a soak.
for forbidden in 'docker build' 'docker pull' 'docker push' 'buildx'; do
    if grep -F -- "$forbidden" "$runner" >/dev/null; then
        printf 'soak harness verification failed: forbidden command: %s\n' "$forbidden" >&2
        exit 1
    fi
done

# Missing arguments must fail before anything is created.
if "$runner" >/dev/null 2>&1; then
    printf 'soak harness verification failed: missing arguments returned success\n' >&2
    exit 1
else
    status=$?
    [ "$status" -eq 2 ] || {
        printf 'soak harness verification failed: missing arguments returned %s, want 2\n' "$status" >&2
        exit 1
    }
fi

# The verdict rule is the point of the harness, so it is exercised rather than
# only read. A leak must be caught, ordinary noise must not be, and a metric
# that rises and then settles is a warm-up rather than a leak.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

program=$(awk '
    /^jq -s --argjson tolerance 30 / { inside = 1; sub(/^jq -s --argjson tolerance 30 .\r?/, ""); }
    inside { print }
    inside && /^    }.\ "\$samples" >"\$trend_report"$/ { exit }
' "$runner" | sed "s|' \"\$samples\" >\"\$trend_report\"||")
[ -n "$program" ] || {
    printf 'soak harness verification failed: the trend program could not be extracted\n' >&2
    exit 1
}

sample_line() {
    printf '{"phase":"active","host_state":"ACTIVE","http_error":0,'
    printf '"server":{"rss_kib":30000,"threads":12,"fds":20,"oom":0,"oom_kill":0},'
    printf '"agent":{"rss_kib":%s,"threads":10,"fds":15,"oom":0,"oom_kill":0,"state_kib":1000},' "$1"
    printf '"audit":{"lag":%s,"coverage_revision":1}}\n' "$2"
}

verdict_for() {
    jq -s --argjson tolerance 30 "$program" "$1" |
        jq -r '[.metrics[] | select(.verdict == "FAIL") | .metric] | join(",")'
}

# A steadily growing Agent RSS is a leak.
: >"$work/leak.jsonl"
i=0
while [ "$i" -lt 24 ]; do
    sample_line $((20000 + i * 900)) 0 >>"$work/leak.jsonl"
    i=$((i + 1))
done
[ "$(verdict_for "$work/leak.jsonl")" = "agent.rss_kib" ] || {
    printf 'soak harness verification failed: a steady memory leak was not caught\n' >&2
    exit 1
}

# Noise around a flat level is not.
: >"$work/noise.jsonl"
i=0
while [ "$i" -lt 24 ]; do
    sample_line $((21000 + (i % 5) * 300 - 600)) $((i % 3)) >>"$work/noise.jsonl"
    i=$((i + 1))
done
[ -z "$(verdict_for "$work/noise.jsonl")" ] || {
    printf 'soak harness verification failed: ordinary noise was reported as a leak\n' >&2
    exit 1
}

# A rise that settles is a warm-up.
: >"$work/warmup.jsonl"
i=0
while [ "$i" -lt 24 ]; do
    if [ "$i" -lt 6 ]; then rss=20000; else rss=26000; fi
    sample_line "$rss" 0 >>"$work/warmup.jsonl"
    i=$((i + 1))
done
[ -z "$(verdict_for "$work/warmup.jsonl")" ] || {
    printf 'soak harness verification failed: a settled warm-up was reported as a leak\n' >&2
    exit 1
}

# Audit lag that climbs from nothing is a stall, even though it starts at zero.
: >"$work/lag.jsonl"
i=0
while [ "$i" -lt 24 ]; do
    case $((i / 6)) in
        0) lag=0 ;;
        1) lag=1000 ;;
        2) lag=5000 ;;
        *) lag=20000 ;;
    esac
    sample_line 21000 "$lag" >>"$work/lag.jsonl"
    i=$((i + 1))
done
[ "$(verdict_for "$work/lag.jsonl")" = "audit.lag" ] || {
    printf 'soak harness verification failed: a growing Audit lag was not caught\n' >&2
    exit 1
}

printf 'soak harness safety boundary and leak verdict are valid\n'
