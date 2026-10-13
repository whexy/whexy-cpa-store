# Client keys

A CLIProxyAPI plugin that sets policies per client API key, the keys in
`access.api-keys` that clients authenticate with. A key can be limited to
some models and kept off the OpenAI Responses API. Keys without policies are
not affected.

## Set up

1. Install `client-keys` from Management Center → Plugin Store.
2. Open Management Center → Client keys and set each key's policies. They
   are saved to `plugins.configs.client-keys` in `config.yaml` and apply when
   CLIProxyAPI reloads it.

## Policies

### models

Limits the models a key may use, with one of:

- `allow: [patterns]`: only models matching a pattern.
- `deny: [patterns]`: every model except those matching a pattern.

A pattern matches the whole model name, ignoring case, and `*` matches any
run of characters. The name is the one the client sends, including any
`prefix/`, without a thinking suffix such as `(high)` and without Gemini's
`models/`. A request for `auto` is judged by the model CLIProxyAPI picks.

A deny list also refuses a denied model reached through an alias, a model
pool or a model route. An allow list only checks the names clients send, so
an allowed alias keeps working whatever model it resolves to.

A refused request gets 404 with the client protocol's own model-not-found
error (OpenAI, Claude or Gemini shape). Turns on the Responses API WebSocket
get 400 instead, because CLIProxyAPI closes the socket without a message on
other statuses.

Model lists read with the key (`GET /v1/models` in its OpenAI, Claude, Codex
and Grok shapes, and `GET /v1beta/models`) show only the models it may use.

### responses-api

`enabled: false` answers the key's OpenAI Responses API requests
(`/v1/responses`, `/v1/responses/compact`, the Responses WebSocket and
`/backend-api/codex`) with an empty 404. Clients that probe `/v1/responses`
and fall back on a missing route, such as n8n's agents, switch to
`/v1/chat/completions`.

### The disable header

A Responses API request carrying the `WHEXY_CPA_DISABLE_RESPONSE_API` header,
with any value and any key or none, gets the same empty 404. The hyphenated
`WHEXY-CPA-DISABLE-RESPONSE-API` works too; use it behind nginx, which drops
header names containing underscores by default.

This and the `responses-api` policy cover what the `disable-response-api`
plugin does: its `api_keys` become `responses-api: {enabled: false}` entries.

## Configuration

For editing `config.yaml` by hand:

```yaml
plugins:
  configs:
    client-keys:
      enabled: true
      keys:
        <caller scope>:
          name: Cursor # optional label
          note: work laptop # optional
          policies:
            models:
              allow: [claude-*, gpt-5*] # or deny: [...]
            responses-api:
              enabled: false
```

Entries are keyed by the key's caller scope, the hash CLIProxyAPI derives
from a client key, so the file never holds the key itself. Its 64 hex
characters may be in either case:

```bash
printf 'cli-proxy-api:caller-scope:v1\0%s' "$KEY" | sha256sum
```

`models` takes exactly one of `allow` and `deny`, a list of at most 500
patterns of at most 200 characters each. `responses-api` requires `enabled`.
A policy that is absent allows everything.

## When the configuration is wrong

Mistakes refuse requests instead of silently lifting the limits:

- An entry that cannot be read (unknown policy type, misspelt field, invalid
  pattern) refuses every model request made with its key with 403 and
  empties its model lists.
- If `keys` cannot be read at all, every model request made with a client key
  gets 503 and every model list read with one is empty.
- An entry whose name is not a caller scope matches no request.

The Client keys page shows each problem.

## Limits

- Codex Live and realtime (`/v1/live`, `/v1/realtime*`) and `/v1/alpha/search`
  offer plugins no hook, so no policy applies to them.
- Model lists are filtered only for keys sent in `Authorization`, `X-Api-Key`
  or `X-Goog-Api-Key`. A key sent as `?key=` sees full lists, though its
  requests are still enforced. `GET /v1beta/models/<id>` is not filtered.
- Model lists show aliases by their own name, so a key with a deny list
  still sees an alias that resolves to a denied model, and is refused when
  it uses it.
- A request whose body exceeds about 1.5 GiB is too large for the plugin to
  read and is refused with 413.
- While the plugin is disabled or fails to load, nothing is enforced.
- Management Center's System page and dashboard list models with the first
  client key, so they show that key's view.

<details>
<summary>Management API routes behind the page</summary>

The routes take the management key like the rest of the Management API, as
`Authorization: Bearer <key>`. Paths are under `/v0/management/client-keys`.
Failures answer `{"error": "<message>"}`. No route returns a client key.

- `GET /status`: the configuration as applied: `generation` (counts
  applies), `applied_at`, a global `error`, `response_interceptor` (whether
  model lists are being filtered) and each entry's `policies` or `error`.
- `GET /models`: the model IDs of the last unfiltered OpenAI model list and
  `captured_at`. Lists are only seen while some key has a `models` policy.
- `POST /fingerprints` with `{"keys": ["…"], "api_base": "…"}`: each key's
  caller scope and, with `api_base`, the fingerprint Management Center stores
  the key's name under. At most 1000 keys.

</details>
