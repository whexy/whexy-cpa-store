# Session Credential Guard

A CLIProxyAPI request interceptor that stops a client session from silently
moving between credentials. CLIProxyAPI still selects credentials and fails
over as usual. When it selects a different credential for a bound session,
this plugin blocks the request before it goes upstream and returns
`409 Conflict`. The session moves only after the client acknowledges that exact
migration.

It runs as the after-auth request interceptor (`request.intercept_after`). That
hook runs after CLIProxyAPI has picked a credential and before the executor
sends the request upstream. A rejection there ends the whole request, including
any failover attempts left in it.

## Opting in

Only requests that carry `X-CPA-Session-Guard` (any value) or
`X-CPA-Session-Migration-Ack` are guarded. Every other request passes through
unchanged. A client should opt in only if it can handle the 409 described below.

A session is identified by the host's `canonical_session_id`, the client API key
(`caller_scope`), and the requested model. This matches the granularity of
CLIProxyAPI's own session affinity. Requests for which the host publishes no
session identity or selected credential pass through unguarded.

## Protocol

| Session state   | CLIProxyAPI selects | Request                  | Result                                                              |
| --------------- | ------------------- | ------------------------ | ------------------------------------------------------------------- |
| unbound         | X                   | plain                    | bind to X (epoch 1), pass                                           |
| bound to A      | A                   | plain                    | pass                                                                |
| bound to A      | B                   | plain                    | open migration `m` A→B, 409 `SESSION_MIGRATION_REQUIRED`            |
| pending `m` A→B | B                   | plain                    | 409 `SESSION_MIGRATION_REQUIRED` with the same `m`                  |
| pending `m` A→B | B                   | ack `m`                  | bind to B, epoch+1, pass                                            |
| pending `m` A→B | A                   | plain                    | drop `m`, pass                                                      |
| pending `m` A→B | C                   | plain                    | drop `m`, open migration `m'` A→C, 409 `SESSION_MIGRATION_REQUIRED` |
| pending `m` A→B | not B               | ack `m`                  | drop `m`, stay on A, 409 `SESSION_MIGRATION_TARGET_UNAVAILABLE`     |
| committed `m`   | any                 | ack `m` again            | treated as a plain request                                          |
| any             | any                 | ack of any other/unknown | no state change, 409 `SESSION_MIGRATION_ACK_INVALID`                |

A migration commits only to the credential that was selected when it was
opened. If CLIProxyAPI stops selecting that credential, the migration is
dropped, and any later move needs a new migration id. The migration id is the
compare-and-swap token for the commit. Replaying the acknowledgement of the
migration that produced the current binding is harmless, so a client can
safely retry a commit request whose response it lost.

Send the acknowledgement on the retried request itself:

```
X-CPA-Session-Migration-Ack: smig_4f0c...
```

The binding moves when that request is dispatched to the new credential.

## Rejection response

Status `409`, `Content-Type: application/json`. When the session has a pending
migration, the `X-CPA-Session-Migration` response header carries its id. The
body parses as both an Anthropic and an OpenAI error:

```json
{
  "type": "error",
  "error": {
    "type": "session_migration_required",
    "code": "SESSION_MIGRATION_REQUIRED",
    "message": "CLIProxyAPI selected a different credential for this session. ..."
  },
  "session_binding": {
    "auth_index": "8c1f0e2a9b7d3c64",
    "epoch": 1,
    "pending_migration": {
      "id": "smig_4f0c...",
      "to_auth_index": "2d9e4b7a1c0f8e53"
    }
  }
}
```

`session_binding` describes the session's current state after this decision.
It is absent when the session has no binding. `pending_migration` is absent
when nothing is pending. Credentials appear only as CLIProxyAPI's opaque auth
indexes, never as auth ids or file names.

## Requirements

- A CLIProxyAPI host that publishes `canonical_session_id`, `selected_auth_id`,
  and `selected_auth_index` to request interceptors. Verified against v7.3.17.
- `routing.session-affinity: true`. Without affinity, CLIProxyAPI rotates
  credentials on every request and the guard reports each rotation as a
  migration. With affinity, a failover rebinds CLIProxyAPI's own cache to the
  new credential, so retries keep selecting the migration target.
- `routing.session-affinity-ttl` at least `binding_ttl`. Otherwise, once
  CLIProxyAPI forgets an idle session, it may pick a new credential even though
  the bound one is healthy, and the guard surfaces that as a migration.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    session-credential-guard:
      enabled: true
      binding_ttl: 24h # idle time after which a session's binding is forgotten
```

Bindings are kept in memory. They are lost when CLIProxyAPI restarts, and after
that the next request binds the session to whatever credential is selected.
