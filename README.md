# whexy-cpa-store

Private plugin store registry for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI).
CLIProxyAPI's Management Center reads this repo's `registry.json` and installs
plugins directly from this repo's GitHub release assets.

## Plugins

- `client-keys` — policies per client API key, set on a Management Center page: which models a key may use and see listed, and whether it may use the OpenAI Responses API.
- `extra-models` — adds model IDs to one built-in provider before CLIProxyAPI's model catalog lists them, so that provider's accounts serve a new model right away; entries use the catalog's format and are set in the plugin's settings.
- `model-groups` — gives one model ID to a group of models, for example `auto-group/fast`, and routes each request to one of them by round robin, weighted round robin or fill first; groups are managed on a Management Center page and CLIProxyAPI keeps choosing the provider account.
- `opencode-go` — serves an OpenCode Go subscription: the workspace's Go models under `opencode-go/<model>`, an OpenCode login from the OAuth Login page or keys managed on a Management Center page that also disables models, one setting that serves every model under its bare ID to share it with other providers, quota in the Plugin tab.
- `system-prompt` — appends your own text to the system prompt of requests for the models you choose, before CLIProxyAPI translates or cloaks them; rules pairing model IDs or patterns with a prompt are managed on a Management Center page.
- `usage-dashboard` — adds a Usage page to Management Center: tokens, estimated cost and sessions over the past day to quarter, grouped by model family or account, kept on disk.

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
- A plugin's version is the `pluginVersion` constant in its `go/main.go`. The
  plugin reports that constant to the host, and CLIProxyAPI offers an update
  whenever it differs from the registry version, so the release is built from
  the commit that declares it.

## Layout

```
plugins/<id>/
  plugin.json          # store metadata (id, name, description, ...), no version
  vendor-hash.nix      # fixed-output hash of the vendored Go dependencies
  CHANGELOG.md         # written by release/bump.mjs
  go/                  # plugin source (Go module); main.go declares pluginVersion
tools/registry-check/  # validates registry.json and plugin.json with the host's parser
release/               # validation scope, version bumps, tagging, release publication and registry tooling
nix/packages/plugins/  # .#plugins.<id> builds, tests and zips one plugin
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
3. In `go/main.go`, declare `pluginVersion = "0.0.0"` in a `const` block and
   register it as the plugin's `Metadata.Version`. 0.0.0 means unreleased.
4. Add `plugins/<id>/vendor-hash.nix` with `lib.fakeHash`, run
   `nix build .#plugins.<id>`, and paste the hash from the error message.
5. Open a pull request with a `feat(<id>): ...` commit. To release it in the
   same pull request, see [Release flow](#release-flow).

## Release flow

A plugin is released by the pull request that raises its `pluginVersion`.
Plugins release independently: a release touches only its own directory, and
pull requests merge without being rebased onto the latest `main`, so
concurrent releases of different plugins never wait for each other.

To release, finish the pull request's changes, then run:

```bash
node release/bump.mjs <id>          # next version from the commits since <id>'s last tag
node release/bump.mjs <id> 0.2.1    # or name the version
```

It writes the version into `go/main.go` and a section into `CHANGELOG.md`
listing the `feat`, `fix`, `perf` and `revert` commits that touch
`plugins/<id>/`. Before 1.0, `feat` and breaking changes bump the minor
version and `fix` the patch. Commit both files as
`chore(<id>): release <version>` and merge the pull request; changes merged
without a bump wait for the next one.

Woodpecker runs `.woodpecker.yaml`, and nothing else is manual; never create
tags or releases by hand:

1. Pull requests run the part of `nix flake check` their changed files
   affect, as chosen by `release/validate.mjs`. Formatting, lint and the
   release tooling's tests, which also require a changelog section for every
   declared version, always run. A change in `plugins/<id>/` builds and tests
   that plugin, and a change to `registry.json`, a `plugin.json` or
   `tools/registry-check/` runs the registry check. Changes outside these,
   `release/` and the top-level docs run all of `nix flake check`.
2. Each push to `main` runs the same check for its own changes and for every
   plugin whose declared version `registry.json` does not list yet, then tags
   `<id>-v<version>` at that commit for each of those versions that has no tag
   yet.
3. The tag pipeline checks the tag against the declared version, builds only
   that plugin, creates a draft release with its changelog section, uploads
   the zip and publishes the release.
4. whexy-bot regenerates `registry.json` and commits it to `main`. This is the
   only commit that bypasses the pull request rule, and it starts no pipeline.

The Nix steps share a build cache across pipelines
([whexy/ci-cache](https://github.com/whexy/ci-cache)): a plugin or check none
of whose inputs changed is downloaded instead of built, so a tag pipeline
normally downloads the plugin its `main` push built. CI builds without the Nix
sandbox, which its step pods cannot host, so only a local `nix flake check`
catches a build that reaches the network.

A failed tag pipeline leaves the release as a draft or missing: fix the cause
and rerun the pipeline. A failed `main` pipeline tags nothing; the next
passing push or a manual pipeline on `main` tags what it missed. Published
releases are immutable, so a broken version is fixed by releasing a new one.
To republish the registry, start a manual pipeline on `main`.

A tag without any pipeline means Woodpecker missed the tag's webhook delivery,
which GitHub does not retry. In the repository's webhook settings, open Recent
Deliveries, find the push for that tag and redeliver it.

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
nix develop                                # go, node, jq, gh + pre-commit hooks
nix fmt                                    # treefmt: nixfmt, gofmt, shfmt, prettier
nix flake check                            # lint, registry check, release tests, build + test every plugin
nix build .#plugins.usage-dashboard        # one plugin's release zip in ./result
node --test release/release.test.mjs       # the release tooling's tests alone
```

Preview the registry the next publication would commit, with your own GitHub
token; it writes nothing:

```bash
CI_REPO=whexy/whexy-cpa-store GITHUB_TOKEN="$(gh auth token)" node release/registry.mjs --dry-run
```
