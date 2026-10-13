# Usage Dashboard

A CLIProxyAPI usage plugin that adds a Usage page to Management Center:
processed tokens and estimated cost over the past 24 hours, 7, 30 or 90 days,
grouped by model family or by account, with sessions, cache savings and a
per-model or per-day breakdown. CLIProxyAPI cannot tell which client sent a
request, so the page groups by what it does know: the model and the
credential that served it.

## Set up

1. Install `usage-dashboard` from Management Center → Plugin Store.
2. Open Management Center → Usage. The page reuses the management key when
   Management Center remembers it; otherwise it asks once per tab.

Usage is recorded from the moment the plugin loads; there is no backfill.

## Grouping

**Family** puts every model of a vendor line together: `claude-opus-5-5` and
`claude-sonnet-5-5` are Claude, `deepseek-v4-pro` and `deepseek-v4-flash` are
DeepSeek. A routing prefix such as `opencode-go/` is ignored. Known families
are Claude, GPT (including `o3`, `codex-*`), Gemini, Gemma, Grok, DeepSeek,
Kimi, Qwen, GLM, MiniMax, Mistral, Llama, Doubao, ERNIE, Hunyuan, Nova and
Command; any other model is grouped by the first word of its ID. Add rules
with `model_families`.

**Account** is the credential that served the request: an OAuth account by
its email, an API key by its label (or "API key") and its last four
characters. An account removed from CLIProxyAPI keeps the name it had.

Each family or account keeps its colour across ranges. Past eight, the
smallest fold into Other.

## Data

Usage is kept as hourly totals per account and model, one file per UTC day
(`usage-<YYYY-MM-DD>.json`), written every 10 seconds and on shutdown. By
default the files go to `data/usage-dashboard` beside the `plugins` directory
the plugin was installed into, so they land on the same volume; in a
container that mounts `/data/plugins`, that is `/data/data/usage-dashboard`.
Days past `retention_days` are deleted. Prompts, responses and client API
keys are never stored, and session IDs only as hashes.

Cost is estimated at the time of viewing from the
[models.dev](https://models.dev) price list, fetched daily and cached as
`prices.json` in the same directory. Prices are the base rates; long-context
tiers are not applied. Models missing from models.dev show no cost.

## Settings

All optional, under `plugins.configs.usage-dashboard` or in the plugin's
configuration form in Management Center:

- `data_dir`: where the usage files go.
- `retention_days`: days to keep, 1 to 400. Default 30. The 90-day range
  appears from 90.
- `pricing`: `false` turns cost estimates off. Default `true`.
- `pricing_url`: a models.dev-format catalog. Default
  `https://models.dev/api.json`.
- `model_families`: globs over the model ID mapped to a family, checked
  before the built-in list:

  ```yaml
  model_families:
    "my-coder-*": Qwen
  ```

<details>
<summary>Management API route behind the page</summary>

`GET /v0/management/usage-dashboard/summary?range=24h|7d|30d|90d&group=family|account&tz=<IANA zone>`
takes the management key as `Authorization: Bearer <key>` and answers the
totals, one series per group, per-model rows and per-point rows for the
range. Day ranges end with today in `tz`.

</details>
