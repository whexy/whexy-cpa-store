# Model Groups

A CLIProxyAPI plugin that gives one model ID to a group of models and routes
each request to one of them. A client asks for `auto-group/fast`; the plugin
answers with one of that group's models; CLIProxyAPI then serves that model
exactly as it serves it on its own, including its own choice of provider
account.

## Set up

1. Install `model-groups` from Management Center → Plugin Store.
2. Open Management Center → Model Groups and create a group. The editor lists
   the models CLIProxyAPI serves right now with the providers that serve each
   one, and stores that with the group.
3. Point a client at the group's model ID, for example `auto-group/fast`.

Group IDs are listed by `GET /v1/models` like any other model, and they work on
every endpoint CLIProxyAPI serves — Messages, Chat Completions, Responses
(including count tokens), Gemini, images — because the request itself is served
as the member model.

## How a request is routed

The group's policy chooses the model; CLIProxyAPI chooses the account.

| Policy               | Choice                                                  |
| -------------------- | ------------------------------------------------------- |
| Round robin          | the next member, in the order the group lists them      |
| Weighted round robin | members share requests in proportion to weight (1–1000) |
| Fill first           | the first member that can serve                         |

A member is skipped when none of its providers has credentials, or when it is
inside a cooling window: two consecutive failures keep a member out of new
decisions for 30 seconds, doubling up to 10 minutes. A session that already
holds a member keeps it, so one failure does not move a conversation.

## Sessions and accounts

A session keeps the member it first used until that member has no credentials
left, which is what keeps CLIProxyAPI on one account: its sticky routing binds
`(session, model)` to a credential, so a session that changes model also
changes account. The group's editor can instead move a session as soon as its
member fails.

CLIProxyAPI's sticky routing is off by default. Turn it on, and give it and the
plugin the same lifetime, in `config.yaml`:

```yaml
routing:
  session-affinity: true
  session-affinity-ttl: "1h"
```

The page reads that setting and warns when it is off, because without it
CLIProxyAPI spreads requests across every credential of the chosen provider,
per request. The plugin's own session lifetime is `affinity.ttl` (default 1h).

## Notes

- The editor's models are what `GET /v1/models` lists, read with every client
  key in `access.api-keys`. Each model's providers come from auth files and
  from `api-keys` (OpenAI-compatible providers and API keys); a model neither
  accounts for shows "provider unknown" and cannot be added.
- The plugin resolves a member's providers when the group is saved, and stores
  them. After adding a provider for one of a member's models, open the group in
  the editor and save it: every member's providers are resolved again against
  what CLIProxyAPI serves then.
- A routed request names one provider, so failover inside a request happens
  between that provider's credentials. A model served by several providers
  carries all of them as that member's providers, and a session stays on the
  one it started with.
- Sessions are kept in `state.json` under `data/model-groups/` next to the
  plugin directory, and are re-read after a reload or a restart.
- Keep a group's slug clear of model IDs CLIProxyAPI already serves: the group
  answers that ID while it is enabled.

<details>
<summary>Configuration file</summary>

The plugin reads its own node of `config.yaml`. The page writes this for you;
edit it by hand when you want the groups in version control.

```yaml
plugins:
  configs:
    model-groups:
      prefix: auto-group
      affinity:
        enabled: true
        ttl: "1h"
        rebind_on_failure: false
      groups:
        - name: Fast
          slug: fast
          policy: weighted-round-robin
          enabled: true
          members:
            - model: claude-haiku-5-5
              providers: [claude]
              weight: 3
            - model: opencode-go/kimi-k3
              providers: [opencode-go]
              weight: 1
```

`policy` is `round-robin`, `weighted-round-robin` or `fill-first`. A member's
`providers` is the set of provider keys that serve that model, in preference
order; `provider` (one key) is accepted as shorthand. A member without
providers stays in the group but is not routable. A config that does not parse
keeps the previous groups running and reports the error on the page.

</details>

<details>
<summary>Management API routes behind the page</summary>

The routes take the management key like the rest of the Management API, as
`Authorization: Bearer <key>`. Paths are under `/v0/management/model-groups`.
Failures answer `{"error": "<message>"}`.

- `GET /state`: the groups, their members, live per-member state, request
  counts over the last hour, and the plugin's affinity settings.
- `GET /sessions?group=<slug>&limit=<n>`: the sessions a group holds, the
  member and provider each is pinned to, and the credential CLIProxyAPI bound
  it to.
- `POST /sessions/release` with `{"group": "fast", "session": "<session_full>"}`
  or `{"group": "fast", "all": true}`: let those sessions decide again.

Groups themselves are written through the config API at
`/v8/management/config/plugins/configs/model-groups`.

</details>
