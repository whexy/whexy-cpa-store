#!/usr/bin/env bash
# Builds all plugins to regenerate the registry, then stages only the selected
# plugin artifact for a plugin-scoped release tag.
set -euo pipefail
cd "$(dirname "$0")/.."

release_tag="${1:?usage: dist-plugin.sh <plugin-id>-v<version>}"
IFS=$'\t' read -r plugin_id version < <(./scripts/check-tag-version.sh "$release_tag")

nix build .#plugins --print-build-logs --out-link dist-out
artifact="dist-out/${plugin_id}-v${version}-linux-amd64.zip"
if [ ! -f "$artifact" ]; then
  echo "error: expected release artifact not found: $artifact" >&2
  exit 1
fi

rm -rf dist
mkdir -p dist
cp -L "$artifact" dist/
cp -L dist-out/registry.json registry.json
# Keep generated state consistent with the repository formatter so the registry
# commit does not make the next release fail its format check.
nix fmt -- registry.json
rm -f dist-out
echo "==> staged $release_tag and regenerated registry.json"
