_: {
  projectRootFile = "flake.nix";

  programs = {
    nixfmt.enable = true;
    gofmt.enable = true;
    shfmt.enable = true;
    prettier.enable = true;
  };
}
