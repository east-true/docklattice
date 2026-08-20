#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
runner="$repo_dir/scripts/run-abuse-matrix-e2e.sh"
documentation="$repo_dir/docs/release/abuse-matrix-e2e.md"

[ -x "$runner" ] || {
    printf 'abuse harness verification failed: runner is not executable\n' >&2
    exit 1
}
sh -n "$runner"

require_literal() {
    grep -F -- "$1" "$2" >/dev/null || {
        printf 'abuse harness verification failed: %s lacks %s\n' "$2" "$1" >&2
        exit 1
    }
}

# Each abusive input must stay paired with the refusal it is supposed to prove.
for literal in \
    'all image arguments must be exact local sha256 image IDs' \
    '[ ! -e "$evidence_dir" ]' \
    'command -v docker' \
    '--pull never' \
    '../../etc/passwd' \
    '/etc/passwd' \
    'escape attempts were refused' \
    'a write escaped the project root' \
    'the unrevealed read returned the secret value' \
    'the Server database contains the secret value' \
    'the Audit record contains the secret value' \
    'instead of 409' \
    'the refused attempts disturbed the original operation' \
    'the dashboard answered HTTP' \
    "against the Agent's own container succeeded" \
    'the Agent container is no longer running' \
    'malformed-json' \
    'unknown-field' \
    'oversized-body' \
    'a refusal answered with a server error' \
    'were not both marked as colliding' \
    'a mutation on a colliding project answered HTTP' \
    'PROJECT_BUSY' \
    'guard_project_target' \
    'is_fixture_uid' \
    'allow_fixture_uid' \
    'which this harness did not create' \
    'select_fixture_project' \
    'dashboard projects claim the fixture identity' \
    'a second mutation ran while the project was locked' \
    'an evicted record must be reported as gone' \
    'DENY_PROTECTED_PROJECT' \
    'refused a Compose mutation outside the protected project' \
    'com.docker.compose.project=' \
    'the replaying Agent exited successfully' \
    'a replayed token registered a second Agent' \
    'an Agent trusting a foreign CA registered' \
    'a restore from a modified archive succeeded' \
    'the refused restore still replaced the project file' \
    'filesystem write capability stayed enabled' \
    'status=PASS' \
    'seal_evidence'; do
    require_literal "$literal" "$runner"
done

for literal in \
    'Status: PASS' \
    'Recorded assertion results' \
    'path_abuse_all_refused' \
    'secret_exposure_absent_from_server_storage' \
    'operation_id_reuse_refused' \
    'self_protection_refused' \
    'request_abuse_all_refused_with_client_status' \
    'name_collision_mutation_refused' \
    'operation_bounds_project_busy' \
    'operation_bounds_ring_evicts_oldest' \
    'protected_compose_project_denied' \
    'token_single_use_replay_refused' \
    'wrong_server_ca_never_registers' \
    'backup_tamper_restore_refused' \
    'non_identical_bind_fs_write_disabled'; do
    require_literal "$literal" "$documentation"
done

printf 'abuse harness syntax and static contracts are valid\n'
