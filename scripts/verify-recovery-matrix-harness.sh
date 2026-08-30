#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
runner="$repo_dir/scripts/run-recovery-matrix-e2e.sh"
documentation="$repo_dir/docs/release/recovery-matrix-e2e.md"

[ -x "$runner" ] || {
    printf 'recovery matrix harness verification failed: runner is not executable\n' >&2
    exit 1
}
sh -n "$runner"

require_literal() {
    literal=$1
    file=$2
    grep -F -- "$literal" "$file" >/dev/null || {
        printf 'recovery matrix harness verification failed: %s lacks %s\n' "$file" "$literal" >&2
        exit 1
    }
}

# The harness must keep its safety boundary, must keep the control case that
# separates a reconnect defect from a recovery defect, and must keep the three
# outcomes section 6.1 of the architecture distinguishes apart from each other.
for literal in \
    'both image arguments must be exact local sha256 image IDs' \
    '[ ! -e "$evidence_dir" ]' \
    'command -v docker' \
    "'{{.OSType}}'" \
    '/var/run/docker.sock' \
    '--pull never' \
    'server issue-token --state-dir /var/lib/docklattice --ttl 15m' \
    'chmod 0700 /recovery/server /recovery/agent /recovery/server/tls' \
    'restart_server control' \
    'rm -f /state/server.db /state/server.db-wal /state/server.db-shm' \
    'rm -rf /state/identity' \
    'archive_generation did not advance after Audit database loss' \
    'the existing Agent did not reconnect automatically with its original identity' \
	'event=audit_archive_refused' \
	'ARCHIVE_ROLLBACK_DETECTED' \
	'archive_rollback_local_diagnostic=PASS' \
    'the Server did not fail closed after losing only its Identity State' \
    'another Server Identity' \
    'server_identity_id was reused after both stores were lost' \
    'the Agent was accepted although the Server identity changed' \
    'manual re-registration did not produce an ACTIVE host' \
    'rm -f /agent/join-token' \
    'status=PASS' \
    'seal_evidence'; do
    require_literal "$literal" "$runner"
done

for literal in \
    'Status: PASS' \
    'Recorded assertion results' \
    'plain_restart_reconnect' \
    'database_loss_automatic_reconnect' \
    'identity_loss_with_database_fails_closed' \
    'both_stores_lost_manual_reregistration'; do
    require_literal "$literal" "$documentation"
done

printf 'recovery matrix harness syntax and static contracts are valid\n'
