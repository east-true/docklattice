#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
runner="$repo_dir/scripts/run-clean-host-install-e2e.sh"
documentation="$repo_dir/docs/release/clean-host-install-e2e.md"

[ -x "$runner" ] || {
    printf 'clean-host harness verification failed: runner is not executable\n' >&2
    exit 1
}
sh -n "$runner"

require_literal() {
    literal=$1
    file=$2
    grep -F -- "$literal" "$file" >/dev/null || {
        printf 'clean-host harness verification failed: %s lacks %s\n' "$file" "$literal" >&2
        exit 1
    }
}

for literal in \
    'all image arguments must be exact local sha256 image IDs' \
    '[ ! -e "$evidence_dir" ]' \
    'command -v docker' \
    "'{{.OSType}}'" \
    "'{{.CgroupVersion}}'" \
    '/var/run/docker.sock' \
    '--pull never' \
    'server issue-token --state-dir /var/lib/dockpilot --ttl 15m' \
    'chmod 0700 /clean-host/server /clean-host/agent' \
    'chmod 0600 /clean-host/server/tls/server.crt /clean-host/server/tls/server.key' \
    'capabilities.discovery.enabled == true' \
    'kind:"compose.up"' \
    '/backups' \
    'start_agent false' \
    'Agent identity changed across restart' \
    'rm -f /agent/join-token' \
    'status=PASS' \
    'find "$evidence_dir" -type f -exec chmod 0444' \
    'CLEAN_HOST_EVIDENCE_MAX_BYTES' \
    '--log-opt max-size=1m --log-opt max-file=1 --log-opt compress=false'; do
    require_literal "$literal" "$runner"
done

for forbidden in 'docker build' 'docker push' 'docker pull' 'buildx' 'curl http://'; do
    if grep -F -- "$forbidden" "$runner" >/dev/null; then
        printf 'clean-host harness verification failed: forbidden command/reference: %s\n' "$forbidden" >&2
        exit 1
    fi
done

if "$runner" >/dev/null 2>&1; then
    printf 'clean-host harness verification failed: missing arguments returned success\n' >&2
    exit 1
else
    status=$?
    [ "$status" -eq 2 ] || {
        printf 'clean-host harness verification failed: missing arguments returned %s, want 2\n' "$status" >&2
        exit 1
    }
fi

# A missing Docker executable must fail before any evidence path is created.
probe_parent=$(mktemp -d)
probe_evidence="$probe_parent/must-not-exist"
server_id=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
agent_id=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
fixture_id=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
if PATH=/path-that-does-not-exist /bin/sh "$runner" "$probe_evidence" "$server_id" "$agent_id" "$fixture_id" >/dev/null 2>&1; then
    printf 'clean-host harness verification failed: Docker absence returned success\n' >&2
    rmdir "$probe_parent"
    exit 1
fi
[ ! -e "$probe_evidence" ] || {
    printf 'clean-host harness verification failed: Docker absence created evidence\n' >&2
    exit 1
}
rmdir "$probe_parent"

require_literal 'Status: PASS' "$documentation"
require_literal 'Recorded assertion results' "$documentation"
require_literal 'No image is built, pulled, pushed, or downloaded' "$documentation"
require_literal 'read-only after completion' "$documentation"

printf 'clean-host install harness syntax and static contracts are valid\n'
