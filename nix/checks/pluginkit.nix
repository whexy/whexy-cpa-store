{ pkgs, flake, ... }:

# Every plugin links the shared kit, so its tests run once here rather than
# with each plugin.
pkgs.buildGoModule {
  pname = "pluginkit";
  version = "0";
  src = flake + "/pluginkit";
  vendorHash = "sha256-jYz4LzepIwrqI0aoBiAr+dRVQrhV4dUs+5CjDal8tZM=";
  # kit.js is tested under node.
  nativeCheckInputs = [ pkgs.nodejs_24 ];
  installPhase = ''
    touch "$out"
  '';
}
