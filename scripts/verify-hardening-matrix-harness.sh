#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
runner="$repo_dir/scripts/run-hardening-matrix-e2e.sh"
documentation="$repo_dir/docs/release/hardening-matrix-e2e.md"

[ -x "$runner" ] || {
    printf 'hardening harness verification failed: runner is not executable\n' >&2
    exit 1
}
sh -n "$runner"

require_literal() {
    grep -F -- "$1" "$2" >/dev/null || {
        printf 'hardening harness verification failed: %s lacks %s\n' "$2" "$1" >&2
        exit 1
    }
}

# The harness must keep its safety boundary and must keep every failure it
# injects paired with the assertion that makes the injection meaningful.
for literal in \
    'all image arguments must be exact local sha256 image IDs' \
    '[ ! -e "$evidence_dir" ]' \
    'command -v docker' \
    "'{{.OSType}}'" \
    '/var/run/docker.sock' \
    '--pull never' \
    'docker kill --signal KILL "$agent"' \
    'docker kill --signal KILL "$server"' \
    'docker network disconnect' \
    'docker network connect' \
    'incarnation did not advance' \
    'no AUDIT_CONTINUITY_UNCERTAIN was recorded for the killed incarnation' \
    'did not come back interrupted' \
    'did not admit possible partial effects' \
    'the canonical cursor regressed' \
    'never stopped reporting the partitioned Agent as ACTIVE' \
    'recovery consumed a Join Token' \
    'a Compose child process survived the cancelled operation' \
    'does not identify a conflict and the current digest' \
    'the refused write still modified the file' \
    'racing writes succeeded; exactly one must win' \
    'race markers; a serialized write leaves exactly one' \
    'acknowledged cursor stayed below its pre-restore watermark' \
    'degraded storage disabled a capability instead of annotating it' \
    'coverage gap is missing its precision, source, or ordering' \
    'HARDENING_ALLOW_DOCKER_DAEMON_RESTART' \
    'SKIPPED_NOT_AUTHORIZED' \
    'rm -f /state/join-token' \
    'check_invariants ' \
    'guard_project_target' \
    'is_fixture_uid' \
    'allow_fixture_uid' \
    'which this harness did not create' \
    'select_fixture_project' \
    'dashboard projects claim the fixture identity' \
    'invariant: operation ' \
    'invariant: the project lock is still held by nothing' \
    'invariant: a restore journal survived a settled scenario' \
    'invariant: staging files were orphaned in the project directory' \
    'invariant: the acknowledged cursor passed the Server delivery cursor' \
    'invariant: the project secret leaked into a container log' \
    'status=PASS' \
    'seal_evidence'; do
    require_literal "$literal" "$runner"
done

for literal in \
    'Status: PASS' \
    'Recorded assertion results' \
    'agent_sigkill_continuity_uncertain' \
    'operation_interrupt_partial_effects_admitted' \
    'network_partition_reconnect_without_token' \
    'concurrent_operations_single_winner' \
    'db_restore_ack_watermark_not_regressed' \
    'disk_pressure_reason_reported' \
    'audit_gap_every_gap_is_described' \
    'invariants_agent_sigkill' \
    'docker_daemon_restart'; do
    require_literal "$literal" "$documentation"
done

printf 'hardening harness syntax and static contracts are valid\n'
