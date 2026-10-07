# Model Capabilities

`model-capabilities` exposes CLIProxyAPI's live model registry as a small
client-facing catalog. The plugin asks CPA's authenticated `/v1/models`
endpoint through the host HTTP callback for every catalog request, so models
disappear when their last credential is unavailable and reappear when a
credential is registered again. It does not use `models.dev` or maintain a
second model roster.

The plugin cannot register a top-level `/v1/model-capabilities` route through
the current CLIProxyAPI plugin ABI. It provides the same document at these
ABI-owned paths instead:

- `GET /v0/resource/plugins/model-capabilities/catalog` (unauthenticated)
- `GET /v0/management/plugins/model-capabilities/catalog` (Management API auth)

The resource URL is intended for local integrations. It returns an `ETag` and
honors `If-None-Match`; the tag changes only when the model set or configured
protocol metadata changes. A `304` response means the client can keep its
cached document.

Each model preserves registry metadata and adds these fields:

```json
{
  "id": "gpt-6",
  "protocol": "openai-responses",
  "protocols": ["openai-responses"],
  "endpoints": ["/v1/responses"],
  "capabilities": { "tools": true },
  "protocol_source": "config"
}
```

Unknown protocols are represented by `protocol: null`, empty `protocols` and
`endpoints`, `capabilities: null`, and `protocol_source: "unknown"`. CPA's
registry knows which models are currently available, but it does not expose a
reliable per-model client wire protocol. Configure that information explicitly
under `plugins.configs.model-capabilities` when an integration needs it:

```yaml
plugins:
  configs:
    model-capabilities:
      enabled: true
      base_url: http://127.0.0.1:8317
      models_path: /v1/models
      api_key: ""
      protocols:
        gpt-6:
          preferred: openai-responses
          supported: [openai-responses]
          capabilities:
            tools: true
            reasoning: true
        claude-sonnet:
          preferred: anthropic-messages
          endpoints: [/v1/messages]
```

`base_url` and `api_key` are only used by the plugin's host callback request to
CPA; the key is never included in the catalog. Supported protocol names are
client-facing labels. The built-in endpoint
defaults are `openai-responses` → `/v1/responses`,
`openai-chat-completions` → `/v1/chat/completions`,
`anthropic-messages` → `/v1/messages`, and
`google-generative-ai` → `/v1beta/models/*:generateContent`. Set `endpoints`
explicitly when CPA is fronted by a path prefix or a compatibility route.
