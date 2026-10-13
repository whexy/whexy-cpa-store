{ pkgs, flake, ... }:

# The release tooling's tests also check every plugin's version and changelog,
# so a local flake check catches what the pipeline would reject.
pkgs.runCommand "release-tests" { nativeBuildInputs = [ pkgs.nodejs_24 ]; } ''
  node --test ${flake}/release/release.test.mjs
  touch "$out"
''
