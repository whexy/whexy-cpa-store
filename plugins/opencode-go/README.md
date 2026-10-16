# OpenCode Go

A CLIProxyAPI provider for an [OpenCode Go](https://opencode.ai/v2/docs/console/go/)
subscription. Clients use the workspace's Go models through CLIProxyAPI's
Chat Completions, Responses and Messages endpoints, whichever protocol each
model speaks upstream. Go requests go to the Go gateway only, so they draw on
the subscription and never on Console credits.

The same workspace's [OpenCode Zen](https://opencode.ai/v2/docs/console/models)
models — the pay-as-you-go ones — can be served alongside them as
`opencode/<model>`. They stay out of CLIProxyAPI until **Enabled** is turned on
for one on the page, because those calls bill Console credits. OpenCode's free
models also work with no account at all, as `opencode-free/<model>`; see
[OpenCode Free](#opencode-free).

## Set up

1. Install `opencode-go` from Management Center → Plugin Store.
2. Open Management Center → OAuth Login → OpenCode Go and approve the request
   on the OpenCode page that opens, in the workspace that holds the Go
   subscription. The login is saved as `opencode-go-oauth-<workspace id>.json`
   in the auth directory, and the plugin refreshes its token on its own.
   Logging in to the same workspace again replaces that login.
3. Request models as `opencode-go/<model>`, for example `opencode-go/kimi-k3`.
   `GET /v1/models` lists them.

Each login and key serves OpenCode Go by default. Its **OpenCode Go** and
**OpenCode Zen** switches on the OpenCode Go page turn each on or off, and
each one on is an auth of its own in CLIProxyAPI. With OpenCode Zen on, turn on
**Enabled** for a Zen model to request it as `opencode/<model>`.

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

`"zen": true` turns on OpenCode Zen for the key, and `"go": false` turns off
OpenCode Go; a login file takes the same two fields.

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
- `PATCH /keys` with `{"id": "…", "zen": true}`: renames (`label`), disables or
  enables (`disabled`), and turns OpenCode Go or Zen on or off (`go`, `zen`);
  send any of the fields.
- `DELETE /keys?id=…`: removes the key.
- `POST /keys/test` with `{"id": "…"}`: checks a stored key and answers
  `{"ok": true, "usage": …}` or `{"ok": false, "error": "…"}`.
- `GET /keys/usage?id=…`: the key's 5-hour, weekly and monthly usage with
  reset times.
- `GET /logins`: every login with its workspace, account email, token
  expiry, enabled state and CLIProxyAPI routing state.
- `PATCH /logins` with `{"id": "…", "zen": true}`: disables or enables a login
  (`disabled`), and turns OpenCode Go or Zen on or off (`go`, `zen`); send any
  of the fields.
- `GET /logins/usage?id=…`: the login's usage, as for a key.
- `GET /models`: the catalog of the first login or key that can read it —
  both the OpenCode Go and the OpenCode Zen lists — and the models
  CLIProxyAPI has not registered yet or still lists.
- `POST /models/refresh`: reads every catalog again, makes CLIProxyAPI
  register the lists, and answers the models added and removed.
- `GET /routing`: the OpenCode Go, Zen and Free priority and weight of every
  OpenCode Go auth file.
- `PUT /routing` with `{"go": {"priority": -1}, "zen": {"priority": -2}}`:
  writes Go and Zen into every login and key file as `go_priority`,
  `go_weight`, `zen_priority` and `zen_weight`, and `free` into
  `opencode-free.json` as its `priority` and `weight`; `null` restores the
  default, and a field left out keeps its value.
- `GET /free`: OpenCode Free's auth file, enabled state and free models.
- `PUT /free` with `{"enabled": true}`: turns OpenCode Free on, creating its
  file, or off.

The page's model toggles are plugin settings, which it edits through
`/v8/management/config/plugins/configs/opencode-go`. It also removes each
entry of `/v8/management/config/oauth/model-alias/opencode-go` that aliases a
prefixed Go model to its bare ID, which version 0.5.0's Bare ID toggle wrote,
because such an entry lists the model under both IDs.

</details>

## Models

The plugin reads the workspace's OpenCode catalog, which holds two lists, and
the OpenCode Go page shows both.

**OpenCode Go models** are the subscription models. By default every ID
carries the `opencode-go/` prefix, so Go never takes requests meant for
another provider that serves the same model, such as `kimi-k3` through
Kimi. **Disabled** removes a model from CLIProxyAPI, with the prefix on or
off, and is stored in the plugin setting `disabled_models`. **Disable all**
turns it on for every listed model at once, and the page lists the disabled
models after the ones still served.
The page also lists the wildcard rules of Management Center's OAuth Model
Disablement page that match Go models; a rule written with the prefix, such
as `opencode-go/claude-*`, matches nothing while `disable_prefix` is on.

**OpenCode Zen models** are the workspace's pay-as-you-go models, served as
`opencode/<model>` by every login and key with OpenCode Zen on. They are off by
default: turn on **Enabled** for one, or use **Enable all**, and the plugin
registers it and sends its requests to the OpenCode Zen gateway, which bills
Console credits. The enabled IDs are stored
in the plugin setting `zen_models`. The two lists share upstream IDs: `kimi-k3`
is both `opencode-go/kimi-k3` and `opencode/kimi-k3`. Only the API families the
plugin speaks are listed; Gemini and SystemOne models are left out.

A Zen model the catalog prices at zero is free, but OpenCode's free tier
answers only its own client. The plugin therefore presents those requests as
OpenCode — see [Free models](#free-models) — so they work through CLIProxyAPI
like any other model.

## OpenCode Free

OpenCode serves its free models without an account, and so does the plugin:
turn on **OpenCode Free** on the OpenCode Go page and request them as
`opencode-free/<model>`. It needs no login or key, and its requests carry none
of yours: they send the key OpenCode's own client sends without an account,
`public`, and are presented as that client, the only caller the free tier
answers (see [Free models](#free-models)).

The models are the OpenCode models [models.dev](https://models.dev) prices at
zero and does not mark deprecated, among those the gateway lists as live. The
plugin reads both lists with the other catalogs, every three hours and on
**Refresh models**. **Disabled** removes a model and is stored in the plugin
setting `free_disabled_models`.

Turning OpenCode Free on creates `opencode-free.json` in the auth directory, an
auth of its own with its own priority and weight under **Routing**. Turning it
off disables that auth; delete the file on the Auth Files page to remove it.

## Free models

OpenCode's free tier refuses every caller but its own client, with
`403 FreeTierError`. The gate is a fingerprint on the request, and it was
reproduced against the live endpoint one factor at a time:

- the `User-Agent` names OpenCode with a version of at least 1.18.0, in any of
  the shapes the client uses;
- a session header carries a well-formed OpenCode session id, which the gate
  decodes;
- the body asks for streaming, and the gateway answers with SSE even for a
  request that asks for JSON;
- the body's tool list contains a tool **named** `read`.

That last check reads the name and nothing else, which is what keeps it
harmless. The plugin appends a placeholder whose description says it is
reserved by the transport and must not be called, and models leave it alone: in
live runs, a model given the placeholder beside Codex's tools reasoned that
"the read tool is explicitly unavailable" and ran `shell` with `cat` instead,
and one given it beside Pi's tools called Pi's own `read`. A caller that
already declares `read`, as Pi does, keeps its tool untouched and gets nothing
added.

A caller that is OpenCode itself is left alone entirely: its own user agent and
session are what the gate wants, so they pass through rather than being
replaced, which keeps the gateway's per-session metering and cache affinity.

Two consequences are worth knowing. The placeholder still reaches the model, so
one that ignores the description would emit a call no client can run; and
OpenCode meters the free tier on its side, so a request can be refused even
when everything above holds. Turn `cloak_free_models` off to send free models
as the plugin itself, which is what any other client gets.

The plugin reads the catalog again every three hours and on **Refresh
models**, and CLIProxyAPI then lists the models OpenCode added and drops those
it removed. If the catalog cannot be read, the last list read stays in use.

Models the catalog prices at zero keep working after a login or key reaches
its usage limit.

## Sharing a model with another provider

Every model ID carries its family's prefix by default: `opencode-go/`,
`opencode/` or `opencode-free/`. Turn on `disable_prefix` in the plugin
settings to serve every OpenCode Go, Zen and Free model under its bare ID
instead, such as `deepseek-v4-pro`. A model is listed under one ID or the
other, never both: with `disable_prefix` on, `opencode-go/deepseek-v4-pro` is
gone.

A bare ID is shared with every provider that serves the same ID, such as an
`openai-compatibility` entry for the DeepSeek API, and between the plugin's
own families: `kimi-k3` reaches both OpenCode Go and OpenCode Zen. The page
names the other providers and families that serve a model's bare ID; disable
the models you do not want there.

CLIProxyAPI sends each request to the highest priority that has a ready
credential and fails over to the next one on errors. Within a priority,
`round-robin` rotates per credential and `weighted-round-robin` splits by
credential weight. OpenCode Go, OpenCode Zen and OpenCode Free route as
separate credentials: each login and key is an auth per family it has on,
`<auth>` for Go and `<auth>#zen` labelled `· OpenCode Zen` for Zen, and
OpenCode Free is the auth of `opencode-free.json`. Set each family's priority
and weight under **Routing** on the page. With OpenCode Go at `-1` and
OpenCode Zen at `-2`, `kimi-k3` goes to OpenCode Go and reaches OpenCode Zen
only while no Go credential is ready, such as after a usage limit. The other
provider's priority is in its own config entry.

A top-level `priority` or `weight` in a login or key file applies to both Go
and Zen, because CLIProxyAPI applies it to every auth of the file; saving
**Routing** replaces it with `go_priority`, `go_weight`, `zen_priority` and
`zen_weight`.

## Settings

All optional, under `plugins.configs.opencode-go` or in the plugin's
configuration form in Management Center:

- `base_url`: Go gateway base URL. Default `https://opencode.ai/inference/go`.
  The OpenCode Zen gateway is the same base without its `/go` path.
- `console_url`: OpenCode Console base URL, for login, token refresh and the
  model catalog. Default: the `base_url` host followed by `/console`.
- `model_prefix`: prefix of every OpenCode Go model ID. Default `opencode-go`.
- `zen_model_prefix`: prefix of every OpenCode Zen model ID, as in
  `opencode/gpt-5.5`. Default `opencode`.
- `disable_prefix`: serve every OpenCode Go, Zen and Free model under its bare
  ID only, ignoring `model_prefix` and `zen_model_prefix`. Default off. See
  [Sharing a model](#sharing-a-model-with-another-provider).
- `disabled_models`: bare Go model IDs kept out of CLIProxyAPI, such as
  `[kimi-k3]`. The page's Disabled toggles edit it; the form does not show it.
- `zen_models`: bare OpenCode Zen model IDs served as `opencode/<id>`, such as
  `[gpt-5.5]`. Empty serves none. The page's Enabled toggles edit it; the form
  does not show it.
- `free_disabled_models`: OpenCode Free model IDs kept out of CLIProxyAPI. The
  OpenCode Free list's Disabled toggles edit it; the form does not show it.
- `cloak_free_models`: present free models, of OpenCode Zen and OpenCode Free,
  as the OpenCode client, which is the only caller the free tier answers.
  Default on; OpenCode Free does not work with it off. See
  [Free models](#free-models).
- `user_agent`: sent upstream when the client sends none. Default
  `whexy-cpa-opencode-go/<version>`.

## Quota

The Plugin tab of Management Center's quota page shows each login's and
key's 5-hour, weekly and monthly usage. A login or key that reaches a limit
gets no further paid Go requests until the window resets. Its OpenCode Zen
auth shows no windows, because Zen bills Console credits, and neither does
OpenCode Free.
