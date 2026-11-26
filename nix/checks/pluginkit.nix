{ pkgs, flake, ... }:

# Every plugin links the shared kit, so its tests run once here rather than
# with each plugin.
pkgs.buildGoModule {
  pname = "pluginkit";
  version = "0";
  src = flake + "/pluginkit";
  vendorHash = "sha256-Knt5+h4VxfeeCg7Dl9dUzpidDPExE0VasAFID0CWDYU=";
  # kit.js is tested under node.
  nativeCheckInputs = [ pkgs.nodejs_24 ];
  installPhase = ''
    touch "$out"
  '';
}
