{
  pkgs,
  flake,
  ...
}:

let
  inherit (pkgs) lib;
  pluginsDir = flake + "/plugins";
  store = builtins.fromJSON (builtins.readFile (flake + "/store.json"));
  repo = store.repository;
  legacyReleaseVersions = store.legacy_release_versions or [ ];

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

  buildPlugin =
    id:
    let
      meta = metaFor id;
    in
    pkgs.buildGoModule {
      pname = meta.id;
      inherit (meta) version;
      # Rename the source root: the directory is called "go", which would
      # otherwise land on buildGoModule's GOPATH and make Go ignore go.mod.
      src = builtins.path {
        name = meta.id;
        path = pluginsDir + "/${id}/go";
      };
      vendorHash = import (pluginsDir + "/${id}/vendor-hash.nix");

      # Plugins are loaded through the CLIProxyAPI C ABI, so the artifact is a
      # cgo-produced shared library, not a regular binary.
      buildPhase = ''
        runHook preBuild
        go build -trimpath -buildmode=c-shared -o "${meta.id}.so" .
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
        install -Dm755 "${meta.id}.so" "$out/${meta.id}.so"
        runHook postInstall
      '';

      # Releases only ship linux/amd64 artifacts.
      meta.platforms = [ "x86_64-linux" ];
    };

  plugins = lib.genAttrs pluginIds buildPlugin;

  pluginOutEntries = lib.concatMapStringsSep "\n" (id: ''["${id}"]="${plugins.${id}}"'') pluginIds;
in
pkgs.stdenvNoCC.mkDerivation {
  pname = "whexy-cpa-store";
  version = if pluginIds == [ ] then "0" else (metaFor (builtins.head pluginIds)).version;

  nativeBuildInputs = [
    pkgs.zip
    pkgs.jq
  ];

  dontUnpack = true;

  meta.platforms = [ "x86_64-linux" ];

  buildPhase = ''
    runHook preBuild

    mkdir -p zips

    declare -A plugin_out=(
      ${pluginOutEntries}
    )

    plugins_json='[]'
    for meta_path in "${pluginsDir}"/*/plugin.json; do
      id=$(jq -r .id "$meta_path")
      version=$(jq -r .version "$meta_path")

      # cp -p keeps the store's normalized mtime and zip -X drops extended
      # attributes, so the archive (and the sha256 recorded in the registry)
      # is reproducible.
      cp -p "''${plugin_out[$id]}/''${id}.so" "''${id}-v''${version}.so"
      zip -X -j "zips/''${id}-v''${version}-linux-amd64.zip" "''${id}-v''${version}.so"

      artifacts='[]'
      for zipfile in zips/"$id"-v"$version"-*.zip; do
        base=$(basename "$zipfile" .zip)
        platform="''${base#"$id"-v"$version"-}"
        goos="''${platform%%-*}"
        goarch="''${platform##*-}"
        sha256=$(sha256sum "$zipfile" | cut -d' ' -f1)
        size=$(stat -c %s "$zipfile")
        release_tag="$id-v$version"
        case " ${lib.concatStringsSep " " legacyReleaseVersions} " in
          *" $version "*) release_tag="v$version" ;;
        esac
        url="https://github.com/${repo}/releases/download/$release_tag/''${base}.zip"
        artifacts=$(jq \
          --arg goos "$goos" --arg goarch "$goarch" --arg url "$url" \
          --arg sha256 "$sha256" --argjson size "$size" \
          '. + [{goos: $goos, goarch: $goarch, url: $url, sha256: $sha256, size: $size}]' \
          <<<"$artifacts")
      done

      entry=$(jq --argjson artifacts "$artifacts" \
        '. + {install: {type: "direct", artifacts: $artifacts}}' "$meta_path")
      plugins_json=$(jq --argjson entry "$entry" '. + [$entry]' <<<"$plugins_json")
    done

    jq --argjson plugins "$plugins_json" '{schema_version: 2, plugins: $plugins}' <<< '{}' > registry.json

    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall
    mkdir -p "$out"
    cp zips/*.zip "$out/"
    cp registry.json "$out/registry.json"
    runHook postInstall
  '';
}
