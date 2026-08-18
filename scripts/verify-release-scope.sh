#!/bin/sh
set -eu

# Phase 9 release-scope audit.
#
# Architecture section 18 classifies every behaviour as CORE, OPTIONAL, FUTURE,
# or DO NOT BUILD. This script proves that no FUTURE or DO NOT BUILD behaviour
# reached the release path.
#
# "Release path" is not a directory: it is the transitive dependency graph of
# the single binary the Dockerfile builds (./cmd/dockpilot). Auditing the graph
# rather than a directory means the disposable Appendix A prototype under
# cmd/transport-prototype, internal/candidate, and internal/contract is out of
# scope for the right reason - the release binary does not link it - and it
# would immediately come into scope if anything ever imported it.
#
# Each check is a regular expression over the release path's non-test sources.
# A match fails the gate: the reviewer must remove the behaviour or record why
# it is in scope.

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"

command -v go >/dev/null 2>&1 || {
    printf 'release scope verification failed: go toolchain is required\n' >&2
    exit 1
}

release_binary=./cmd/dockpilot
module=$(go list -m)

# The Dockerfile must build exactly the binary this audit covers.
# The go build invocation spans several continuation lines, so match the
# target itself and confirm it is the only cmd/ package the image builds.
grep -F -- "-o /out/dockpilot $release_binary" Dockerfile >/dev/null || {
    printf 'release scope verification failed: Dockerfile does not build %s\n' "$release_binary" >&2
    exit 1
}
if [ "$(grep -c -E '\./cmd/[a-z-]+' Dockerfile)" -ne 1 ]; then
    printf 'release scope verification failed: Dockerfile builds more than one cmd package\n' >&2
    exit 1
fi

packages=$(go list -deps "$release_binary" | grep "^$module") || {
    printf 'release scope verification failed: cannot resolve release dependency graph\n' >&2
    exit 1
}

# No prototype package may be reachable from the release binary.
prototype=$(printf '%s\n' "$packages" | grep -E '/(prototype|candidate|contract)(/|$)' || true)
if [ -n "$prototype" ]; then
    printf 'release scope verification failed: release binary links prototype packages:\n' >&2
    printf '%s\n' "$prototype" | sed 's/^/  /' >&2
    exit 1
fi

sources=$(printf '%s\n' "$packages" |
    sed "s|^$module|.|" |
    while read -r dir; do
        find "$dir" -maxdepth 1 -name '*.go' ! -name '*_test.go'
    done | sort)
[ -n "$sources" ] || {
    printf 'release scope verification failed: release path has no sources\n' >&2
    exit 1
}
source_count=$(printf '%s\n' "$sources" | wc -l | tr -d ' ')
package_count=$(printf '%s\n' "$packages" | wc -l | tr -d ' ')

status=0

check() {
    name=$1
    pattern=$2
    hits=$(printf '%s\n' "$sources" | xargs grep -nEi -- "$pattern" 2>/dev/null || true)
    if [ -z "$hits" ]; then
        printf '  ok         %s\n' "$name"
        return 0
    fi
    printf '  VIOLATION  %s\n' "$name"
    printf '%s\n' "$hits" | sed 's/^/             /'
    status=1
}

printf 'Release path: %s packages, %s sources reachable from %s\n\n' \
    "$package_count" "$source_count" "$release_binary"

printf 'DO NOT BUILD (architecture section 18)\n'
check 'arbitrary shell / SSH / exec terminal' \
    'exec[_a-z]*terminal|attach[_a-z]*terminal|"/(exec|shell|terminal)"|arbitrary shell|\bssh\b'
check 'host OS metric collection' \
    'node_exporter|/proc/(stat|meminfo|loadavg|uptime)|host_?cpu|host_?memory|host_?disk'
check 'filesystem watcher / OS audit integration' \
    'fsnotify|inotify|\bauditd\b|watch[_a-z]*(dir|file|path)'
check 'application-aware database backup' \
    'mysqldump|pg_dump|mongodump|xtrabackup|innobackup'
check 'self-healing / auto retry / auto reapply' \
    'self[_ -]?heal|auto[_ -]?retry|auto[_ -]?restart|auto[_ -]?reapply'
check 'image build platform / CI pipeline editor' \
    'ImageBuild|docker build|buildkit|pipeline[_a-z]*editor'
check 'Prometheus / Grafana / Loki replacement' \
    'prometheus|grafana|\bloki\b|promhttp|expfmt'
check 'Agent self-update' \
    'self[_ -]?update|auto[_ -]?update'
check 'mTLS / own CA / certificate rotation' \
    'RequireAndVerifyClientCert|ClientCAs|mutual[_ -]?tls|\bmtls\b|rotate[_a-z]*cert|cert[_a-z]*rotat'
check 'Kubernetes / Swarm orchestration' \
    'kubernetes|\bk8s\b|kubectl|docker swarm|SwarmMode'
check 'Project Lock force-release API' \
    'force[_-]?release'
check 'config-hash recomputation / dry-run parsing' \
    'compose config --hash|--dry-run|dry[_-]?run|recompute[_a-z]*hash'
check 'Delta Coverage API' \
    'delta[_ -]?coverage'
check 'prototype-only metric names' \
    'candidate_[ab]_[a-z]|proto(type)?_metric_'

printf 'FUTURE (architecture section 18)\n'
check 'metrics history / timeseries storage' \
    'metric[_a-z]*history|timeseries|time_series|metric[_a-z]*retention'
check 'discovery max_depth' \
    'discovery[_a-z]*max[_ ]?depth|max[_ ]?depth[_a-z]*discovery'
check '.dockpilotignore file support' \
    'dockpilotignore'
check 'discovery boundary marker' \
    'boundary[_ -]?marker|dockpilotroot'
check 'Native Agent packaging' \
    'native[_ -]?agent'
check 'Key Rotation' \
    'key[_ -]?rotation|rotate[_a-z]*key'
check 'remote backup storage' \
    '"s3"|/s3/|minio|azblob|"gcs"|remote[_ -]?backup'
check 'notifications' \
    'slack|\bsmtp\b|sendmail|notification[_a-z]*(send|deliver|dispatch)'
check 'authentication / RBAC' \
    '\brbac\b|role[_ -]?binding|permission[_a-z]*check|login[_a-z]*handler'
check 'CLI / TUI frontend' \
    'rivo/tview|bubbletea|gizak/termui|gdamore/tcell|AlecAivazis/survey'
check 'audit external export' \
    'audit[_a-z]*export|export[_a-z]*audit'
check 'bind mount directory backup' \
    'bind[_ -]?mount[_a-z]*backup'
check 'symlink allow policy' \
    'allow[_a-z]*symlink|follow[_a-z]*symlink'
check 'container-level lock granularity' \
    'container[_ -]?lock'
check 'socket-proxy / DOCKER_HOST support' \
    'DOCKER_HOST|socket[_ -]?proxy'

if [ "$status" -ne 0 ]; then
    printf '\nrelease scope verification failed\n' >&2
    exit 1
fi

printf '\nrelease scope is clean: no FUTURE or DO NOT BUILD behaviour reaches %s\n' "$release_binary"
