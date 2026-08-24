#!/usr/bin/env bash
# Ensures every plugins/*/plugin.json version matches the release tag.
# Usage: check-tag-version.sh <version>   (version without leading "v")
set -euo pipefail

tag_version="${1:?usage: check-tag-version.sh <version>}"

versions="$(jq -r .version plugins/*/plugin.json | sort -u)"
count="$(printf '%s\n' "$versions" | wc -l)"
if [ "$count" != "1" ] || [ "$versions" != "$tag_version" ]; then
  echo "error: plugin.json version(s) do not match tag v${tag_version}:" >&2
  printf '%s\n' "$versions" >&2
  exit 1
fi
echo "==> all plugin versions match tag v${tag_version}"
