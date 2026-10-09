#!/bin/sh
# Read Go build metadata instead of relying on tool-specific --version flags.
set -eu

bin=$1
package=$2
module=$3
version=$4
target="$bin/${package##*/}"
go_version=$(go env GOVERSION)
go_os=$(go env GOOS)
go_arch=$(go env GOARCH)

matches() {
    test -x "${1:-$target}" || return 1
    metadata=$(go version -m "${1:-$target}" 2>/dev/null) || return 1
    printf '%s\n' "$metadata" | awk \
        -v package="$package" -v module="$module" -v version="$version" \
        -v compiler="$go_version" -v os="$go_os" -v arch="$go_arch" '
        NR == 1 { compiler_ok = ($NF == compiler) }
        $1 == "path" { package_ok = ($2 == package) }
        $1 == "mod" { module_ok = ($2 == module && $3 == version) }
        $1 == "build" && $2 == "GOOS=" os { os_ok = 1 }
        $1 == "build" && $2 == "GOARCH=" arch { arch_ok = 1 }
        END { exit !(compiler_ok && package_ok && module_ok && os_ok && arch_ok) }
    '
}

if matches; then
    printf 'Using %s@%s (%s, %s/%s)\n' "$package" "$version" "$go_version" "$go_os" "$go_arch"
else
    # Stage the replacement: go install refuses to overwrite a corrupt binary.
    # A failed download/build must also leave the previous tool intact.
    mkdir -p "$bin"
    staging=$(mktemp -d "$bin/.install-tool.XXXXXX")
    trap 'rm -rf "$staging"' EXIT
    GOBIN="$staging" go install "$package@$version"
    matches "$staging/${package##*/}" || { printf 'Installed tool metadata mismatch: %s\n' "$target" >&2; exit 1; }
    mv "$staging/${package##*/}" "$target"
fi
