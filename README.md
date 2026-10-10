# whexy-cpa-store

Private plugin store registry for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).
CLIProxyAPI's Management Center reads this repo's `registry.json` and installs
plugins directly from this repo's GitHub release assets.

## Plugins

- `claude-web-search-router` — routes Claude Code built-in web searches across supported backends.
- `disable-response-api` — returns an empty 404 for OpenAI Responses API requests from configured client API keys or carrying the `WHEXY_CPA_DISABLE_RESPONSE_API` header, so clients fall back to Chat Completions.
- `usage-insights` — records per-call token usage, cache ratios, latency, failures, and quota headers, then estimates equivalent raw API spend from live models.dev pricing.

## How it works

- `registry.json` is a CLIProxyAPI plugin store registry (`schema_version` 2,
  `install.type: direct`). Every entry pins per-platform artifact URLs and
  sha256 digests.
- Artifacts are zips attached to this repo's GitHub releases. Each zip holds
  the plugin's shared library at its root, named `<id>-v<version>.so`.
- The registry lists only what has been published. For every plugin directory
  on `main`, whexy-bot takes the highest version among release assets named
  `<id>-v<version>-<goos>-<goarch>.zip`, the digest GitHub recorded for each
  asset, and `plugin.json` as of that release's tag. Never edit
  `registry.json` by hand.
- release-please owns plugin versions: `.release-please-manifest.json` and the
  `pluginVersion` constant in each `go/main.go`. The plugin reports that
  constant to the host, and CLIProxyAPI offers an update whenever it differs
  from the registry version, so the two must come from the same release.

## Layout

```
plugins/<id>/
  plugin.json          # store metadata (id, name, description, ...), no version
  vendor-hash.nix      # fixed-output hash of the vendored Go dependencies
  CHANGELOG.md         # written by release-please
  go/                  # plugin source (Go module)
tools/registry-check/  # validates registry.json and plugin.json with the host's parser
release/               # release-please, release publication and registry tooling
nix/packages/plugins/  # .#plugins.<id> builds, tests and zips one plugin
release-please-config.json
.release-please-manifest.json
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
2. Add `plugins/<id>/plugin.json` without a `version` (id must equal the
   directory name).
3. In `go/main.go`, declare `pluginVersion = "0.0.0" // x-release-please-version`
   and register it as the plugin's `Metadata.Version`.
4. Add `plugins/<id>` to `release-please-config.json` with `component: <id>`
   and `extra-files: ["go/main.go"]`. Leave the manifest alone: the first
   release is `initial-version`, 0.1.0.
5. Add `plugins/<id>/vendor-hash.nix` with `lib.fakeHash`, run
   `nix build .#plugins.<id>`, and paste the hash from the error message.
6. Open a pull request with a `feat(<id>): ...` commit.

## Release flow

Merging pull requests is the only manual step; never create tags or releases
by hand. Woodpecker runs `.woodpecker.yaml`:

1. Pull requests run `nix flake check` (formatting, lint, the registry check,
   and every plugin's build and tests) and the release tooling tests. `main`
   only accepts rebase-merged pull requests that passed on an up-to-date
   branch.
2. Each push to `main` runs release-please. It keeps one release pull request
   per plugin with releasable commits (`feat`, `fix`, `perf`, `revert`) touching
   `plugins/<id>/`. Before 1.0, `feat` and breaking changes bump the minor
   version and `fix` the patch.
3. Merging a release pull request makes release-please tag
   `<id>-v<version>` and create a draft release.
4. The tag pipeline checks the tag against the manifest, builds only that
   plugin, uploads its zip to the draft and publishes the release.
5. whexy-bot regenerates `registry.json` and commits it to `main`. This is the
   only commit that bypasses the pull request rule.

A failed tag pipeline leaves the release as a draft: fix the cause and rerun
the pipeline. Published releases are immutable, so a broken version is fixed
by releasing a new one. To republish the registry, start a manual pipeline on
`main`.

The pipeline acts as whexy-bot through the `github_app_id`,
`github_app_installation_id` and `github_app_private_key` organization
secrets.

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

Keep the URL exactly as written. CLIProxyAPI ties each installed plugin to the
URL of the registry it came from and stops offering updates when it changes.

## Local development

```bash
nix develop                          # go, node, jq, gh + pre-commit hooks
nix fmt                              # treefmt: nixfmt, gofmt, shfmt, prettier
nix flake check                      # lint, registry check, build + test every plugin
nix build .#plugins.usage-insights   # one plugin's release zip in ./result
npm ci --prefix release && node --test release/release.test.mjs
```

Preview what CI would do next with your own GitHub token; neither command
writes anything:

```bash
export CI_REPO=whexy/whexy-cpa-store GITHUB_TOKEN="$(gh auth token)"
node release/release-please.mjs --dry-run  # pending releases and release PRs
node release/registry.mjs --dry-run        # registry.json after the next publication
```
