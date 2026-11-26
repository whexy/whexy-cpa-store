{ inputs, pkgs, ... }:
let
  pre-commit-check = import ./checks/pre-commit-check.nix { inherit inputs pkgs; };
in
pkgs.mkShell {
  packages = [
    pkgs.go
    # Plugins are cgo shared libraries, so go test needs a C compiler.
    pkgs.gcc
    pkgs.zip
    pkgs.jq
    pkgs.nodejs_24
    pkgs.github-cli
  ];

  env = { };

  shellHook = ''
    ${pre-commit-check.shellHook}
  '';
}
