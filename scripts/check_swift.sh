#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
command -v swift >/dev/null
build_dir=$(mktemp -d)
trap 'rm -rf "$build_dir"' EXIT
cp scripts/check_swift.swift "$build_dir/main.swift"
swiftc docs/contract/SwiftModels.swift "$build_dir/main.swift" -o "$build_dir/decode"
"$build_dir/decode" "${1:-.local/contract-responses.json}"
