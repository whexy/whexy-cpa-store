# whexy-cpa-store

Private plugin store registry for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).
CLIProxyAPI's Management Center reads this repo's `registry.json` and installs
plugins directly from this repo's GitHub release assets.

## Plugins

- `claude-web-search-router` — routes Claude Code built-in web searches across supported backends.
- `usage-insights` — records per-call token usage, cache ratios, latency, failures, and quota headers, then estimates equivalent raw API spend from live models.dev pricing.

## How it works

- `registry.json` is a CLIProxyAPI plugin store registry (`schema_version` 2,
  `install.type: direct`). Every entry pins per-platform artifact URLs +
  sha256.
- Artifacts are zips attached to this repo's GitHub releases. Each zip holds
  the plugin's shared library at its root, named `<id>-v<version>.so`.
- Everything is built by nix: `nix build .#plugins` compiles every plugin,
  runs its tests, packs the zips, and writes a fresh `registry.json` with
  correct hashes and sizes.

## Layout

```
plugins/<id>/
  plugin.json        # store metadata (id, name, description, version, ...)
  vendor-hash.nix    # fixed-output hash of the vendored Go dependencies
  go/                # plugin source (Go module)
nix/
  packages/plugins/  # builds all plugins -> zips + registry.json
scripts/
  dist.sh            # nix build + copy artifacts to ./dist and ./registry.json
  check-tag-version.sh  # CI guard: plugin.json versions must match the tag
store.json           # GitHub "owner/repo" used for artifact download URLs
```

## Module path caveat

Plugins that import `github.com/router-for-me/CLIProxyAPI/v7/internal/...` are
constrained by Go's `internal` visibility rule: the module path must stay
rooted at `github.com/router-for-me/CLIProxyAPI/v7/...`. The pinned
CLIProxyAPI release in `go.mod` supplies the module; no upstream checkout is
needed at build time.

## Adding a plugin

1. Copy the plugin source to `plugins/<id>/go/` and set the module path as
   described above (`github.com/router-for-me/CLIProxyAPI/v7/whexy-cpa-store/plugins/<id>`).
2. Add `plugins/<id>/plugin.json` (id must equal the directory name).
3. Add `plugins/<id>/vendor-hash.nix` with `lib.fakeHash`, run
   `nix build .#plugins`, and paste the hash from the error message.
4. Bump `version` in `plugin.json`, commit, and tag `v<version>`.

## Release flow (Woodpecker CI)

Tagging `v<version>` runs `.woodpecker.yaml`:

1. `nix flake check` — formatting (treefmt), lint (nil/statix), plugin build +
   unit tests.
2. Tag/version guard, then `nix build .#plugins` produces the zips and a
   regenerated `registry.json`.
3. A GitHub release for the tag is created with the zips as assets.
4. The regenerated `registry.json` is committed back to `main`.

Required Woodpecker secret: `GITHUB_TOKEN` (repo scope, used by `gh release`
and the registry push).

## Consuming the store

In the CLIProxyAPI `config.yaml`:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/whexy/whexy-cpa-store/main/registry.json"
```

Then open Management Center → Plugin Store and install. The store install
writes the shared library into `plugins/<goos>/<goarch>/`, enables the plugin
in `plugins.configs.<id>`, and reloads the config. In containers, make sure
the config file and the plugins directory are writable volumes.

## Local development

```bash
nix develop            # go, zip, jq, gh + pre-commit hooks
nix fmt                # treefmt: nixfmt, gofmt, shfmt, prettier
nix flake check        # lint + build + test everything
./scripts/dist.sh      # build and stage ./dist + registry.json
```
