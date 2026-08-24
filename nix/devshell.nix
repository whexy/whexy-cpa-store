{ inputs, pkgs, ... }:
let
  pre-commit-check = import ./checks/pre-commit-check.nix { inherit inputs pkgs; };
in
pkgs.mkShell {
  packages = [
    pkgs.go
    pkgs.zip
    pkgs.jq
    pkgs.github-cli
  ];

  env = { };

  shellHook = ''
    ${pre-commit-check.shellHook}
  '';
}
