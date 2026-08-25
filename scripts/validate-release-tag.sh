#!/bin/sh
set -eu

fail() {
    printf 'release tag validation failed: %s\n' "$*" >&2
    exit 1
}

[ "$#" -eq 1 ] || fail "expected exactly one tag"
tag=$1

printf '%s\n' "$tag" |
    grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$' ||
    fail "unsupported SemVer tag: $tag"

version=${tag#v}
case "$version" in
    *-*) prerelease=${version#*-} ;;
    *) prerelease= ;;
esac

if [ -n "$prerelease" ]; then
    printf '%s\n' "$prerelease" |
        awk -F. '
            {
                for (field = 1; field <= NF; field++) {
                    identifier = $field
                    if (identifier !~ /[0-9A-Za-z]/) {
                        exit 1
                    }
                    if (identifier ~ /^[0-9]+$/ &&
                        length(identifier) > 1 &&
                        substr(identifier, 1, 1) == "0") {
                        exit 1
                    }
                }
            }
        ' || fail "invalid prerelease identifier in tag: $tag"
fi

printf '%s\n' "$version"
