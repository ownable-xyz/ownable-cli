#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright (c) wattsy
#
# build.sh: build ownable from this checkout.
#
#   ./build.sh          bin/ownable for this machine (bin/ownable.exe on Windows)
#   ./build.sh --all    bin/ownable-<version>-<os>-<arch> for every release
#                       platform, and bin/SHA256SUMS
#
# The binaries are static (CGO_ENABLED=0) and built with -trimpath, so the
# same commit and Go version give the same bytes on any machine: compare
# your SHA256SUMS with someone else's.
set -eu
cd "$(dirname "$0")"

command -v go >/dev/null 2>&1 || { echo "build.sh: Go is not on PATH; install Go 1.25 or later from https://go.dev/dl/" >&2; exit 1; }

# The version: the tag when HEAD is tagged, else the source's version and the commit.
src=$(sed -n 's/^var version = "\(.*\)"$/\1/p' cmd/ownable/main.go)
if tag=$(git describe --tags --exact-match 2>/dev/null); then
	version=$tag
elif sha=$(git rev-parse --short HEAD 2>/dev/null); then
	version="$src+$sha"
	[ -z "$(git status --porcelain 2>/dev/null)" ] || version="$version.dirty"
else
	version=$src
fi

export CGO_ENABLED=0
flags="-s -w -X main.version=$version"
mkdir -p bin

if [ "${1:-}" != "--all" ]; then
	out="bin/ownable$(go env GOEXE)"
	go build -trimpath -ldflags "$flags" -o "$out" ./cmd/ownable
	echo "built $out $version ($(go env GOOS)/$(go env GOARCH))"
	exit 0
fi

rm -f bin/ownable-* bin/SHA256SUMS
for target in windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os=${target%/*} arch=${target#*/}
	out="bin/ownable-$version-$os-$arch"
	[ "$os" = windows ] && out="$out.exe"
	GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$flags" -o "$out" ./cmd/ownable
	echo "built $out"
done
(cd bin && if command -v sha256sum >/dev/null 2>&1; then sha256sum ownable-*; else shasum -a 256 ownable-*; fi) | sed 's/ [*]/  /' > bin/SHA256SUMS
echo "wrote bin/SHA256SUMS"
