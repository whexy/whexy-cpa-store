#!/usr/bin/env bash
# Ensures the plugin selected by a <plugin-id>-v<version> tag declares that version.
set -euo pipefail

release_tag="${1:?usage: check-tag-version.sh <plugin-id>-v<version>}"
plugin_id="${release_tag%-v*}"
version="${release_tag##*-v}"
metadata="plugins/${plugin_id}/plugin.json"

if [ -z "$plugin_id" ] || [ -z "$version" ] || [ "$plugin_id" = "$release_tag" ] || [ "$version" = "$release_tag" ]; then
  echo "error: tag must have the form <plugin-id>-v<version>: $release_tag" >&2
  exit 1
fi
if [ ! -f "$metadata" ]; then
  echo "error: tag selects unknown plugin: $plugin_id" >&2
  exit 1
fi

declared_id="$(jq -r .id "$metadata")"
declared_version="$(jq -r .version "$metadata")"
if [ "$declared_id" != "$plugin_id" ] || [ "$declared_version" != "$version" ]; then
  echo "error: $metadata declares ${declared_id}-v${declared_version}, not $release_tag" >&2
  exit 1
fi

printf '%s\t%s\n' "$plugin_id" "$version"
