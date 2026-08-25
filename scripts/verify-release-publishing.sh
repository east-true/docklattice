#!/bin/sh
set -eu

repo_dir=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
workflow=$repo_dir/.github/workflows/release.yml
ci_workflow=$repo_dir/.github/workflows/ci.yml
scanner=$repo_dir/scripts/scan-release-images.sh
assets=$repo_dir/scripts/prepare-release-assets.sh
install_doc=$repo_dir/docs/operations/install.md
ignore_file=$repo_dir/distribution/trivyignore.yaml

fail() {
    printf 'release publishing verification failed: %s\n' "$*" >&2
    exit 1
}

require_literal() {
    literal=$1
    file=$2
    grep -F -- "$literal" "$file" >/dev/null || fail "$file lacks $literal"
}

for file in "$workflow" "$ci_workflow" "$scanner" "$assets" "$install_doc" "$ignore_file"; do
    [ -f "$file" ] || fail "missing $file"
done

require_literal 'workflow_call:' "$ci_workflow"
require_literal 'needs: verify' "$workflow"
require_literal 'attestations: write' "$workflow"
require_literal 'contents: write' "$workflow"
require_literal 'id-token: write' "$workflow"
require_literal 'packages: write' "$workflow"
require_literal 'persist-credentials: false' "$workflow"
require_literal 'platforms: linux/amd64,linux/arm64' "$workflow"
require_literal 'push-by-digest=true' "$workflow"
require_literal 'provenance: false' "$workflow"
require_literal 'cosign sign --yes' "$workflow"
require_literal 'cosign verify' "$workflow"
require_literal 'push-to-registry: true' "$workflow"
require_literal 'gh release create' "$workflow"
require_literal 'refusing to replace existing Image tag' "$workflow"
require_literal 'refusing to replace existing GitHub Release' "$workflow"
require_literal 'docker buildx imagetools create' "$workflow"

if grep -Eq 'uses: [^#[:space:]]+@(main|master|v[0-9])([[:space:]]|$)' "$workflow"; then
    fail "every release Action must be pinned to a full commit SHA"
fi

require_literal 'for platform in linux/amd64 linux/arm64' "$scanner"
require_literal '--format cyclonedx' "$scanner"
require_literal '--format json' "$scanner"
require_literal '--severity HIGH,CRITICAL' "$scanner"
require_literal '--ignore-unfixed' "$scanner"
require_literal "--ignorefile \"\$ignore_file\"" "$scanner"
require_literal '--show-suppressed' "$scanner"
require_literal '--exit-code 1' "$scanner"

ignore_count=$(grep -c '^[[:space:]]*- id:' "$ignore_file")
[ "$ignore_count" -eq 10 ] || fail "release vulnerability policy must enumerate exactly 10 reviewed findings"
[ "$(grep -c 'expired_at: 2026-09-15' "$ignore_file")" -eq "$ignore_count" ] ||
    fail "every release vulnerability exception must have the reviewed expiry"
[ "$(grep -c '^[[:space:]]*paths:' "$ignore_file")" -eq "$ignore_count" ] ||
    fail "every release vulnerability exception must be path-scoped"

require_literal 'release-images.json' "$assets"
require_literal 'SHA256SUMS' "$assets"
require_literal 'generate-license-inventory.sh' "$assets"
require_literal 'distribution/trivyignore.yaml' "$assets"
require_literal 'gh attestation verify' "$install_doc"
require_literal 'cosign verify' "$install_doc"
require_literal "trap 'rm -f \"\$join_token_file\"' EXIT HUP INT TERM" "$install_doc"

sh -n "$scanner"
sh -n "$assets"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir -p "$work/bin"
cat >"$work/bin/trivy" <<'STUB'
#!/bin/sh
set -eu

output=
platform=
format=
severity=

while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) output=$2; shift 2 ;;
        --platform) platform=$2; shift 2 ;;
        --format) format=$2; shift 2 ;;
        --severity) severity=$2; shift 2 ;;
        *) shift ;;
    esac
done

printf '%s|%s|%s\n' "$platform" "$format" "$severity" >>"$TRIVY_STUB_LOG"
[ -z "$output" ] || printf '{}\n' >"$output"
STUB
chmod 0700 "$work/bin/trivy"

TRIVY_STUB_LOG=$work/trivy.log \
    PATH=$work/bin:$PATH \
    "$scanner" \
    "$work/assets" \
    ghcr.io/east-true/dockpilot-server \
    sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    ghcr.io/east-true/dockpilot-agent \
    sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

[ "$(wc -l <"$work/trivy.log")" -eq 12 ] || fail "scanner did not execute all report and gate scans"
[ "$(find "$work/assets" -name '*.cdx.json' | wc -l)" -eq 4 ] || fail "scanner did not write four SBOMs"
[ "$(find "$work/assets" -name '*.vulnerabilities.json' | wc -l)" -eq 4 ] ||
    fail "scanner did not write four vulnerability reports"
[ "$(grep -c 'HIGH,CRITICAL' "$work/trivy.log")" -eq 4 ] ||
    fail "scanner did not gate all four component/platform pairs"

"$assets" \
    "$work/assets" \
    0.1.0-rc.1 \
    aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    1700000000 \
    ghcr.io/east-true/dockpilot-server \
    sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
    ghcr.io/east-true/dockpilot-agent \
    sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

(
    cd "$work/assets"
    sha256sum --check SHA256SUMS >/dev/null
)

[ -s "$work/assets/dockpilot-0.1.0-rc.1-go-licenses.tar.gz" ] ||
    fail "asset preparation did not write the Go license archive"
[ -s "$work/assets/trivyignore.yaml" ] ||
    fail "asset preparation did not retain the exact scan policy"
[ "$(jq -r '.images.server.reference' "$work/assets/release-images.json")" = \
    'ghcr.io/east-true/dockpilot-server@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' ] ||
    fail "release manifest does not preserve the Server digest"
[ "$(jq -r '.images.agent.reference' "$work/assets/release-images.json")" = \
    'ghcr.io/east-true/dockpilot-agent@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' ] ||
    fail "release manifest does not preserve the Agent digest"

printf 'release publishing workflow and asset contracts are valid\n'
