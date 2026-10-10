{
  pkgs,
  flake,
  ...
}:

let
  inherit (pkgs) lib;
  pluginsDir = flake + "/plugins";
  # release-please owns plugin versions; a plugin builds as 0.0.0 until its
  # first release adds it to the manifest.
  releasedVersions = builtins.fromJSON (builtins.readFile (flake + "/.release-please-manifest.json"));
  versionOf = id: releasedVersions."plugins/${id}" or "0.0.0";

  entries = builtins.readDir pluginsDir;
  discoveredIds = builtins.filter (
    id:
    (entries.${id} or null) == "directory"
    && builtins.pathExists (pluginsDir + "/${id}/plugin.json")
    && builtins.pathExists (pluginsDir + "/${id}/go/go.mod")
  ) (builtins.attrNames entries);

  metaFor = id: builtins.fromJSON (builtins.readFile (pluginsDir + "/${id}/plugin.json"));

  # The plugin.json id is used in artifact and registry paths; it must match
  # the directory name so per-plugin lookups stay consistent.
  pluginIds =
    if lib.all (id: (metaFor id).id == id) discoveredIds then
      discoveredIds
    else
      throw "plugin.json id must match its directory name under plugins/";

  buildLibrary =
    id:
    pkgs.buildGoModule {
      pname = id;
      version = versionOf id;
      # Rename the source root: the directory is called "go", which would
      # otherwise land on buildGoModule's GOPATH and make Go ignore go.mod.
      src = builtins.path {
        name = id;
        path = pluginsDir + "/${id}/go";
      };
      vendorHash = import (pluginsDir + "/${id}/vendor-hash.nix");

      # Plugins are loaded through the CLIProxyAPI C ABI, so the artifact is a
      # cgo-produced shared library, not a regular binary.
      buildPhase = ''
        runHook preBuild
        go build -trimpath -buildmode=c-shared -o "${id}.so" .
        runHook postBuild
      '';

      doCheck = true;
      checkPhase = ''
        runHook preCheck
        go test ./...
        runHook postCheck
      '';

      installPhase = ''
        runHook preInstall
        install -Dm755 "${id}.so" "$out/${id}.so"
        runHook postInstall
      '';

      meta.platforms = [ "x86_64-linux" ];
    };

  # The store installs <id>-v<version>-<goos>-<goarch>.zip holding
  # <id>-v<version>.so at its root.
  packZip =
    id:
    let
      name = "${id}-v${versionOf id}";
    in
    pkgs.runCommand "${name}-linux-amd64" { nativeBuildInputs = [ pkgs.zip ]; } ''
      # cp -p keeps the store's normalized mtime and zip -X drops extended
      # attributes, so a rerun of a release reproduces the published bytes.
      cp -p "${buildLibrary id}/${id}.so" "${name}.so"
      mkdir -p "$out"
      zip -X -j "$out/${name}-linux-amd64.zip" "${name}.so"
    '';

  zips = lib.genAttrs pluginIds packZip;
in
# Release pipelines build one plugin as .#plugins.<id>; the joined output keeps
# nix flake check building and testing every plugin.
pkgs.symlinkJoin {
  name = "whexy-cpa-store-plugins";
  paths = builtins.attrValues zips;
  passthru = zips;
  meta.platforms = [ "x86_64-linux" ];
}
