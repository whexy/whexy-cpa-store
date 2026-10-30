# Zed

A CLIProxyAPI provider for a [Zed](https://zed.dev) account's hosted models.
Clients use the Claude, GPT and Gemini models of the account's plan through
CLIProxyAPI's Messages, Responses, Chat Completions and Gemini endpoints,
whichever protocol each client speaks. Requests go to Zed's own model
gateway, so usage draws on the plan's included credit and its spend limit.

Zed's hosted-model API is undocumented and not a product API. It is the same
one the Zed editor uses; a Zed release can change it at any time.

## Set up

1. Install `zed` from Management Center → Plugin Store.
2. Open Management Center → OAuth Login → Zed and approve the sign-in in the
   browser. The account is saved as `zed-oauth-<username>.json` in the auth
   directory. On a remote CLIProxyAPI host the final `127.0.0.1` page cannot
   load in your browser; use the Zed page's callback box instead (see below).
3. Request models as `zed/<model>`, for example
   `zed/claude-sonnet-5-5`. `GET /v1/models` lists them.

The Zed page adds accounts without the OAuth tile: **Start sign-in** opens the
same page, and **Finish sign-in** completes it. If the browser runs on another
machine, copy the `127.0.0.1` URL it lands on into the callback box; the plugin
decrypts it with the keypair it kept.

<details>
<summary>Adding an account as an auth file instead</summary>

Put a file of type `zed` in the auth directory:

```json
{
  "type": "zed",
  "user_id": "12345",
  "access_token": "…",
  "system_id": "11111111-2222-4333-8444-555555555555",
  "organization_id": "org_…",
  "username": "whexy",
  "plan": "zed_student"
}
```

`user_id` and `access_token` are what Zed receives at sign-in; `system_id` is
optional and derived from the token when absent. `organization_id` is read
from the account when absent. Zed's access token does not rotate, so there is
no refresh token.

</details>

## Models

The plugin reads `GET /models` with the account's short-lived LLM token, so the
list is exactly the plan's entitlement: a Student account does not see Claude
Opus, Claude Fable or the `-pro` GPT models, and a plan change appears on the
next refresh. The catalog is cached for five minutes; **Refresh list** on the
page reads it again and makes CLIProxyAPI register the result.

Each ID carries the `zed/` prefix by default, so Zed never takes requests
meant for another provider serving the same model. **Serve bare model IDs**
removes the prefix from every ID and shares routing by priority and weight
with every other provider serving that ID. A model can be kept out of
CLIProxyAPI with its toggle; the list lives in the `disabled_models` setting.

The wire protocol comes from the model's Zed provider:

| Zed provider | Upstream request       | Client protocols                  |
| ------------ | ---------------------- | --------------------------------- |
| `anthropic`  | Anthropic Messages     | Messages, Responses, Chat, Gemini |
| `open_ai`    | OpenAI Responses       | Responses, Chat, Messages, Gemini |
| `google`     | Gemini generateContent | Gemini, Chat, Messages, Responses |
| `x_ai`       | Chat Completions       | Chat, Messages, Responses, Gemini |

Zed no longer lists xAI models, but the route stays for a future return.
Conversions use CLIProxyAPI's own translators in process. Non-streaming
clients are served by folding Zed's event stream:

- Claude requests have block content, no `temperature`/`top_p`/`top_k` on
  Sonnet 5 and later, and `thinking.type: adaptive` with `output_config.effort`
  in place of the removed `thinking.type: enabled`.
- Responses requests get `type: message` on input items the client left bare.
- Gemini requests drop `safetySettings`, which Zed's parser rejects.

## Settings

- `base_url` — Zed cloud base, default `https://cloud.zed.dev`.
- `sign_in_url` — the native-app sign-in page, default
  `https://zed.dev/native_app_signin`.
- `model_prefix` — the exposed prefix, default `zed`.
- `disable_prefix` — serve bare IDs only.
- `disabled_models` — upstream model IDs kept out of CLIProxyAPI.
- `user_agent` — sent to Zed; the default is the current Zed release string
  the edge identifies.
- `dashboard_cookie` — optional `Cookie` header from a signed-in
  `dashboard.zed.dev` session, used only for the Plugin tab.

## Quota

Zed has no usage API for the account token: plan spend is visible only to the
dashboard. With `dashboard_cookie` set, the Plugin tab shows monthly token
spend, the spend limit and edit-prediction usage from
`/frontend/billing/usage`. Without it the plan card still renders, with no
meters. The plugin never sends the cookie to the model gateway.

## Management API

Routes take the management key as `Authorization: Bearer <key>` and live under
`/v0/management/zed`. No route returns an access token.

- `GET /accounts` — every Zed auth file with its username, plan, state and
  auth index.
- `PATCH /accounts` with `{"id": "<auth_index>", "label": "…", "disabled": true}`
  — rename, enable or disable an account.
- `GET /models` — the first enabled account's catalog with enabled state and
  the prefix.
- `POST /models/refresh` — read every account's catalog again and make
  CLIProxyAPI register the lists.
- `POST /login/start` — start a sign-in; answers `{url, state, expires_at}`.
- `POST /login/finish` with `{"state": "…"}` — finish a sign-in whose callback
  reached this host.
- `POST /login/complete` with `{"state": "…", "callback_url": "…"}` — finish a
  sign-in from a remote browser.

## Notes

Zed bills hosted usage at provider list price plus 10%. Pro includes $5 of
monthly credit, Student $10; usage beyond it follows the account's spend
limit. A model outside the plan answers 403 `not included in your plan`; the
plugin reports that as a 400 so the account is not cooled down. 401, 429 and
5xx follow CLIProxyAPI's normal cooldown.
