{ pkgs, ... }:

# pluginkit/ui/kit.css, compiled from kit.src.css and the Frost sources it
# imports. Pages inline it, so the result is committed; regenerate it with
#   nix build .#kit-css && install -m644 result pluginkit/ui/kit.css
pkgs.runCommand "kit.css"
  {
    src = pkgs.lib.fileset.toSource {
      root = ../../../pluginkit/ui;
      fileset = pkgs.lib.fileset.unions [
        ../../../pluginkit/ui/kit.src.css
        ../../../pluginkit/ui/frost
      ];
    };
    nativeBuildInputs = [ pkgs.tailwindcss_4 ];
  }
  ''
    cd "$src"
    tailwindcss --input kit.src.css --output "$out" --minify
  ''
