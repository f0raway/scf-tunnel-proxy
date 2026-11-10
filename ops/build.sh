#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/scf-tunnel-linux-amd64 .
printf 'Built dist/scf-tunnel-linux-amd64\n'
