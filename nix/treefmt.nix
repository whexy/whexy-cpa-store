_: {
  projectRootFile = "flake.nix";

  programs = {
    nixfmt.enable = true;
    gofmt.enable = true;
    shfmt.enable = true;
    prettier.enable = true;
  };

  # whexy-bot commits registry.json and release/bump.mjs writes changelogs with
  # their own serializers; formatting them here would fail the next pull
  # request's format check.
  settings.global.excludes = [
    "registry.json"
    "plugins/*/CHANGELOG.md"
  ];
}
