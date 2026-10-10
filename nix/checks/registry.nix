{
  pkgs,
  flake,
  perSystem,
  ...
}:

pkgs.runCommand "registry-check" { } ''
  ${pkgs.lib.getExe perSystem.self.registry-check} ${flake}/registry.json ${flake}/plugins
  touch "$out"
''
