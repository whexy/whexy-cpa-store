{ pkgs, perSystem, ... }:

# The committed kit.css must be what kit.src.css compiles to.
pkgs.runCommand "kit-css-current" { } ''
  if ! cmp -s ${perSystem.self.kit-css} ${../../pluginkit/ui/kit.css}; then
    echo "pluginkit/ui/kit.css is stale; run:" >&2
    echo "  nix build .#kit-css && install -m644 result pluginkit/ui/kit.css" >&2
    exit 1
  fi
  touch "$out"
''
