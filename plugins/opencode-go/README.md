# OpenCode Go

A CLIProxyAPI provider for an [OpenCode Go](https://opencode.ai/v2/docs/console/go/)
subscription. Clients use the workspace's Go models through CLIProxyAPI's
Chat Completions, Responses and Messages endpoints, whichever protocol each
model speaks upstream. Requests go to the Go gateway only, so they draw on
the subscription and never on Console credits.

## Set up

1. Install `opencode-go` from Management Center → Plugin Store.
2. Open Management Center → OAuth Login → OpenCode Go and approve the request
   on the OpenCode page that opens, in the workspace that holds the Go
   subscription. The login is saved as `opencode-go-oauth-<workspace id>.json`
   in the auth directory, and CLIProxyAPI refreshes its token on its own.
   Logging in to the same workspace again replaces that login.
3. Request models as `opencode-go/<model>`, for example `opencode-go/kimi-k3`.
   `GET /v1/models` lists them.

Service-account keys (`oc_sk_…`, allowed to call models) work instead of or
alongside logins: add them on Management Center → OpenCode Go, which checks
each key and stores it in `opencode-go.json`. CLIProxyAPI rotates between all
logins and keys. The same page shows each login's account and token expiry
and disables logins, and renames, disables, tests and removes keys. Delete a
login on the Auth Files page.

<details>
<summary>Adding a key as an auth file instead</summary>

Put a file of type `opencode-go` in the auth directory:

```json
{ "type": "opencode-go", "api_key": "oc_sk_…", "label": "Work" }
```

</details>

<details>
<summary>Management API routes behind the page</summary>

The routes take the management key like the rest of the Management API, as
`Authorization: Bearer <key>`. Paths are under `/v0/management/opencode-go`.
A key or login is named by the `id` its list returns; no route returns a full
key or a token. Failures answer `{"error": "<message>"}`.

- `GET /keys`: every key with its label, masked key, enabled state,
  CLIProxyAPI routing state and workspace.
- `POST /keys` with `{"label": "…", "api_key": "oc_sk_…"}`: checks the key
  against the gateway's usage and workspace catalog, then adds it.
- `PATCH /keys` with `{"id": "…", "label": "…", "disabled": true}`: renames,
  disables or enables; send either field or both.
- `DELETE /keys?id=…`: removes the key.
- `POST /keys/test` with `{"id": "…"}`: checks a stored key and answers
  `{"ok": true, "usage": …}` or `{"ok": false, "error": "…"}`.
- `GET /keys/usage?id=…`: the key's 5-hour, weekly and monthly usage with
  reset times.
- `GET /logins`: every login with its workspace, account email, token
  expiry, enabled state and CLIProxyAPI routing state.
- `PATCH /logins` with `{"id": "…", "disabled": true}`: disables or enables a
  login.
- `GET /logins/usage?id=…`: the login's usage, as for a key.
- `GET /models`: the catalog of the first login or key that can read it,
  and the models CLIProxyAPI has not registered yet or still lists.
- `POST /models/refresh`: reads every catalog again, makes CLIProxyAPI
  register the lists, and answers the models added and removed.
- `GET /routing`: the priority and weight of every OpenCode Go auth file.
- `PUT /routing` with `{"priority": 5, "weight": 2}`: writes both into every
  OpenCode Go auth file; `null` restores the default.

Bare ID is a CLIProxyAPI setting, which the page edits through
`/v8/management/config/oauth/model-alias/opencode-go`. Disabled is the
plugin setting `disabled_models`, which the page edits through
`/v8/management/config/plugins/configs/opencode-go`.

</details>

## Models

The OpenCode Go page lists the models of the workspace's OpenCode catalog.
By default every ID carries the `opencode-go/` prefix, so Go never takes
requests meant for another provider that serves the same model, such as
`kimi-k3` through Kimi. **Disabled** removes a model from CLIProxyAPI, with
the prefix on or off, and is stored in the plugin setting `disabled_models`.
The page also lists the wildcard rules of Management Center's OAuth Model
Disablement page that match Go models; a rule written with the prefix, such
as `opencode-go/claude-*`, matches nothing while `disable_prefix` is on.

The plugin reads the catalog again every three hours and on **Refresh
models**, and CLIProxyAPI then lists the models OpenCode added and drops those
it removed. If the catalog cannot be read, the last list read stays in use.

Models the catalog prices at zero keep working after a login or key reaches
its usage limit.

## Sharing a model with another provider

To serve `deepseek-v4-pro` from both OpenCode Go and another provider of that
ID, such as an `openai-compatibility` entry for the DeepSeek API, turn on
**Bare ID** for the model on the OpenCode Go page. Clients then request
`deepseek-v4-pro`, and `opencode-go/deepseek-v4-pro` keeps working. The page
names the other providers that serve a model's bare ID.

CLIProxyAPI sends each request to the highest priority that has a ready
credential and fails over to the next one on errors. Within a priority,
`round-robin` rotates per credential and `weighted-round-robin` splits by
credential weight. Set OpenCode Go's priority and weight under **Routing** on
the page, which writes them into every OpenCode Go auth file; the other
provider's priority is in its own config entry.

Bare ID is this config entry, which also works written by hand:

```yaml
oauth:
  model-alias:
    opencode-go:
      - name: opencode-go/deepseek-v4-pro
        alias: deepseek-v4-pro
        fork: true
```

To serve every Go model under its bare ID instead, turn on `disable_prefix`
in the plugin settings. Each model then shares routing with every provider
of the same ID, so disable the ones you do not want on the page.

## Settings

All optional, under `plugins.configs.opencode-go` or in the plugin's
configuration form in Management Center:

- `base_url`: Go gateway base URL. Default `https://opencode.ai/inference/go`.
- `console_url`: OpenCode Console base URL, for login, token refresh and the
  model catalog. Default: the `base_url` host followed by `/console`.
- `model_prefix`: prefix of every model ID. Default `opencode-go`.
- `disable_prefix`: serve every model under its bare ID only, ignoring
  `model_prefix`. Default off.
- `disabled_models`: bare Go model IDs kept out of CLIProxyAPI, such as
  `[kimi-k3]`. The page's Disabled toggles edit it; the form does not show it.
- `user_agent`: sent upstream when the client sends none. Default
  `whexy-cpa-opencode-go/<version>`.

## Quota

The Plugin tab of Management Center's quota page shows each login's and
key's 5-hour, weekly and monthly usage. A login or key that reaches a limit
gets no further paid requests until the window resets.
