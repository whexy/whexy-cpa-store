# OpenCode Go

A CLIProxyAPI provider for an [OpenCode Go](https://opencode.ai/v2/docs/console/go/)
subscription. Clients use the workspace's Go models through CLIProxyAPI's
Chat Completions, Responses and Messages endpoints, whichever protocol each
model speaks upstream. Requests go to the Go gateway only, so they draw on
the subscription and never on Console credits.

## Set up

1. Install `opencode-go` from Management Center → Plugin Store.
2. In the OpenCode Console, create a service-account key (`oc_sk_…`) that may
   call models, in the workspace that holds the Go subscription.
3. Open Management Center → OpenCode Go and add the key. The page checks it
   against the gateway, then stores it in `opencode-go.json` in the auth
   directory. Add more keys the same way; CLIProxyAPI rotates between them.
   The same page renames, disables, tests and removes keys.
4. Request models as `opencode-go/<model>`, for example `opencode-go/kimi-k3`.
   `GET /v1/models` lists them.

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
A key is named by the `id` the list returns; no route returns a full key.
Failures answer `{"error": "<message>"}`.

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

</details>

## Models

The model list is the workspace's OpenCode catalog, read with the key when
CLIProxyAPI loads it. If the catalog cannot be read, the last list read
stays in use.

Every ID carries the `opencode-go/` prefix, so Go never takes requests meant
for another provider that serves the same model, such as `kimi-k3` through
Kimi. With `model_prefix: none` the IDs are bare and share those requests.

Models the catalog prices at zero keep working after a key reaches its usage
limit.

## Settings

All optional, under `plugins.configs.opencode-go` or in the plugin's
configuration form in Management Center:

- `base_url`: Go gateway base URL. Default `https://opencode.ai/inference/go`.
- `model_prefix`: prefix of model IDs, or `none`. Default `opencode-go`.
- `user_agent`: sent upstream when the client sends none. Default
  `whexy-cpa-opencode-go/<version>`.

## Quota

The Plugin tab of Management Center's quota page shows each key's 5-hour,
weekly and monthly usage. A key that reaches a limit gets no further paid
requests until the window resets.
