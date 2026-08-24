#!/usr/bin/env bash
# Builds all plugins with nix and copies the artifacts into ./dist plus the
# generated registry.json to the repo root.
set -euo pipefail
cd "$(dirname "$0")/.."

nix build .#plugins --print-build-logs --out-link dist-out
rm -rf dist
mkdir -p dist
cp -L dist-out/*.zip dist/
cp -L dist-out/registry.json registry.json
rm -f dist-out
echo "==> dist/ and registry.json updated"
