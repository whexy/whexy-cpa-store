{ pkgs, flake, ... }:

pkgs.buildGoModule {
  pname = "registry-check";
  version = "0";
  src = builtins.path {
    name = "registry-check";
    path = flake + "/tools/registry-check";
  };
  vendorHash = "sha256-+Supej8yvKIOnVYODlNebyYigWjhfVMDRozynRfABts=";
  meta.mainProgram = "registry-check";
}
