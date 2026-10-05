# SPDX-License-Identifier: Apache-2.0
# Copyright (c) wattsy
#
# build.ps1: build ownable from this checkout.
#
#   .\build.ps1         bin\ownable.exe for this machine
#   .\build.ps1 -All    bin\ownable-<version>-<os>-<arch> for every release
#                       platform, and bin\SHA256SUMS
#
# The binaries are static (CGO_ENABLED=0) and built with -trimpath, so the
# same commit and Go version give the same bytes on any machine: compare
# your SHA256SUMS with someone else's.
param([switch]$All)
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'build.ps1: Go is not on PATH; install Go 1.25 or later from https://go.dev/dl/'
}

# The version: the tag when HEAD is tagged, else the source's version and the commit.
$src = (Select-String -Path cmd/ownable/main.go -Pattern '^var version = "(.*)"$').Matches[0].Groups[1].Value
$version = $src
$tag = git describe --tags --exact-match 2>$null
if ($LASTEXITCODE -eq 0 -and $tag) {
    $version = $tag
} else {
    $sha = git rev-parse --short HEAD 2>$null
    if ($LASTEXITCODE -eq 0 -and $sha) {
        $version = "$src+$sha"
        if (git status --porcelain 2>$null) { $version += '.dirty' }
    }
}
$global:LASTEXITCODE = 0

$env:CGO_ENABLED = '0'
$flags = "-s -w -X main.version=$version"
New-Item -ItemType Directory -Force bin | Out-Null

function Build($os, $arch, $out) {
    $env:GOOS = $os; $env:GOARCH = $arch
    go build -trimpath -ldflags $flags -o $out ./cmd/ownable
    if ($LASTEXITCODE -ne 0) { throw "go build failed for $os/$arch" }
}

try {
    if (-not $All) {
        $os = go env GOOS; $arch = go env GOARCH
        $exe = if ($os -eq 'windows') { 'bin/ownable.exe' } else { 'bin/ownable' }
        Build $os $arch $exe
        "built $exe $version ($os/$arch)"
        return
    }
    Remove-Item bin/ownable-*, bin/SHA256SUMS -ErrorAction SilentlyContinue
    foreach ($target in 'windows/amd64', 'windows/arm64', 'linux/amd64', 'linux/arm64', 'darwin/amd64', 'darwin/arm64') {
        $os, $arch = $target -split '/'
        $out = "bin/ownable-$version-$os-$arch"
        if ($os -eq 'windows') { $out += '.exe' }
        Build $os $arch $out
        "built $out"
    }
    # The same format as sha256sum, so `sha256sum -c SHA256SUMS` checks it anywhere.
    $lines = Get-ChildItem bin/ownable-* | Sort-Object Name | ForEach-Object {
        "$((Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower())  $($_.Name)"
    }
    [IO.File]::WriteAllText("$PSScriptRoot/bin/SHA256SUMS", ($lines -join "`n") + "`n")
    'wrote bin/SHA256SUMS'
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}
