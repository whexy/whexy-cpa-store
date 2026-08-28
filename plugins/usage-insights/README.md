# Usage Insights

A CLIProxyAPI usage plugin that records one local event for every completed model call and uses the live [models.dev](https://models.dev) catalog to estimate equivalent raw API spend.

The plugin advertises RPC schema v1 because its Usage and Management API capabilities do not require newer stream or WebSocket schema features. This keeps it compatible with older CLIProxyAPI plugin hosts.

## Collected data

Each JSONL event includes:

- provider, executor, model, requested alias, normalized credential identifier, auth type, and request source;
- input, uncached input, cache-read, cache-write, output, reasoning, and total tokens;
- cache-read, cache-write, and combined cache ratios;
- request time, latency, time to first token, generation flag, and failure details;
- quota-related response headers such as `X-Codex-*`, `Anthropic-Ratelimit-*`, and `Retry-After`.

The plugin does not persist request or response bodies. The frontend API key identifier is omitted by default and requires `include_api_key: true`.

## Live pricing

Pricing is fetched asynchronously from `https://models.dev/api.json` at startup and every `pricing_refresh_hours`. The last successful catalog is cached beside the JSONL ledger by default, so estimates continue when models.dev is temporarily unavailable. Calls received before the first successful fetch are recorded as unpriced.

models.dev prices are USD per million tokens. Usage Insights applies separate input, output, reasoning, cache-read, and cache-write rates when the catalog publishes them, including context pricing tiers. If a cache-specific price is absent, it falls back to the model's ordinary input rate. Reasoning tokens are only split out when models.dev publishes a dedicated reasoning price, preventing double counting on providers that include reasoning inside output tokens.

Provider aliases are mapped automatically for common CLIProxyAPI providers, including `codex` → `openai`, `claude` → `anthropic`, `gemini` → `google`, and `xai` → `xai`. Use `pricing_provider_map` and `pricing_model_map` for custom gateways or model aliases.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    usage-insights:
      enabled: true
      priority: 0
      data_file: ./data/usage-insights.jsonl
      recent_records: 200
      include_api_key: false
      pricing_enabled: true
      pricing_url: https://models.dev/api.json
      pricing_refresh_hours: 6
      # pricing_cache_file: ./data/models-dev.json
      # pricing_provider_map:
      #   custom-provider: openrouter
      # pricing_model_map:
      #   custom-provider/my-alias: openrouter/openai/gpt-5.4
```

Relative `data_file` paths resolve from CLIProxyAPI's working directory. The file is append-only JSONL and is created with mode `0600`.

## Dashboard and exports

The dashboard shows overall and grouped totals plus recent model calls. Open it through the authenticated Management API:

- `GET /v0/management/plugins/usage-insights/dashboard`
- `GET /v0/management/plugins/usage-insights/summary`
- `GET /v0/management/plugins/usage-insights/export.csv`
- `POST /v0/management/plugins/usage-insights/pricing/refresh`

The CSV endpoint exports the recent in-memory window configured by `recent_records`. The JSON summary aggregates the entire JSONL ledger loaded at startup.

## Cache accounting

CLIProxyAPI providers do not all report cache tokens with identical semantics:

- Anthropic/Claude reports uncached input independently from cache reads and cache writes.
- OpenAI/Codex/Gemini-family records generally report cached tokens as a subset of the input count.

Usage Insights normalizes both into mutually exclusive `uncached_input_tokens`, `cache_read_tokens`, and `cache_write_tokens` fields before calculating cache ratios.

## Subscription comparison

The ledger and dashboard now include estimated raw API cost based on the models.dev catalog version fetched for each new call. Subscription prices and billing periods remain user-specific, so the plugin does not hard-code plan prices. The aggregated `estimated_api_cost_usd` value can be compared directly with the subscription amount for the same time period. Existing JSONL rows created before pricing was enabled remain unpriced rather than being silently recalculated with today's rates.
