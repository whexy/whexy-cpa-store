_: {
  projectRootFile = "flake.nix";

  programs = {
    nixfmt.enable = true;
    gofmt.enable = true;
    shfmt.enable = true;
    prettier.enable = true;
  };

  # CI bots write these files with their own serializers; formatting them here
  # would fail the next pull request's format check on a bot commit.
  settings.global.excludes = [
    "registry.json"
    ".release-please-manifest.json"
    "plugins/*/CHANGELOG.md"
  ];
}
