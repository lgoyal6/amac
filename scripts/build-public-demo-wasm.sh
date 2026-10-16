#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
goroot="$(go env GOROOT)"
runtime="$goroot/lib/wasm/wasm_exec.js"

if [[ ! -f "$runtime" ]]; then
  runtime="$goroot/misc/wasm/wasm_exec.js"
fi
if [[ ! -f "$runtime" ]]; then
  echo "Go's wasm_exec.js runtime was not found under $goroot" >&2
  exit 1
fi

cd "$repo_root"
GOOS=js GOARCH=wasm go build -trimpath -ldflags='-s -w' \
  -o docs/public-demo/health.wasm ./cmd/amac-health-wasm
cp "$runtime" docs/public-demo/wasm_exec.js
