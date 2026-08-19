#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
runner="$repo_dir/scripts/run-resource-matrix.sh"
documentation="$repo_dir/docs/release/resource-gate.md"

[ -x "$runner" ] || {
    printf 'resource harness verification failed: runner is not executable\n' >&2
    exit 1
}
sh -n "$runner"

require_literal() {
    literal=$1
    file=$2
    grep -F -- "$literal" "$file" >/dev/null || {
        printf 'resource harness verification failed: %s lacks %s\n' "$file" "$literal" >&2
        exit 1
    }
}

for literal in \
    'for trial in 1 2 3' \
    '--memory 1g --memory-swap 1g' \
    '--memory 512m --memory-swap 512m' \
    'memory.current memory.peak memory.max memory.events.local memory.stat memory.pressure' \
    'rss_kib' \
    'fd_count' \
    'GODEBUG=gctrace=1' \
    'docker buildx version' \
    'prototype_acceptance_reused=false' \
    'compose-child-processes.tsv' \
    'p0-p1-latency.jsonl' \
    'RESOURCE_ARTIFACT_MAX_BYTES' \
    'all four window quarters slope upward' \
    'anonymous memory increased persistently across the observation window' \
    'post-GC heap did not recover within 120 percent'; do
    require_literal "$literal" "$runner"
done

if grep -F 'transport-prototype' "$runner" >/dev/null; then
    printf 'resource harness verification failed: prototype command is referenced by the product gate\n' >&2
    exit 1
fi

if "$runner" >/dev/null 2>&1; then
    printf 'resource harness verification failed: missing arguments returned success\n' >&2
    exit 1
else
    status=$?
    [ "$status" -eq 2 ] || {
        printf 'resource harness verification failed: missing arguments returned %s, want 2\n' "$status" >&2
        exit 1
    }
fi
require_literal 'Status: PASS' "$documentation"
require_literal 'prototype_acceptance_reused  false' "$documentation"
require_literal 'operation_progress_event_latency_ms' "$documentation"
require_literal 'does not reuse Appendix A prototype acceptance' "$documentation"

printf 'resource harness syntax and static contracts are valid\n'
