# Disable Response API

A CLIProxyAPI request interceptor that rejects OpenAI Responses API calls
(`/v1/responses` and `/v1/responses/compact`) from selected clients. Matching
requests get `404 Not Found` with an empty body before credential selection, so
no upstream call is made. Clients that probe `/v1/responses` and fall back on a
missing route, such as n8n's Vercel AI SDK agents, then switch to
`/v1/chat/completions`. Requests to other APIs pass through unchanged.

A Responses API request is rejected when either:

- it authenticates with a client API key listed in `api_keys`, or
- it carries the `WHEXY_CPA_DISABLE_RESPONSE_API` header (any value).

## Selecting clients by API key

Give the client its own CLIProxyAPI API key and list it in `api_keys`. This
works for clients that cannot send custom headers. n8n 2.41's agent runtime, for
example, drops the OpenAI credential's custom header.

The key is read from the same headers CLIProxyAPI authenticates with:
`Authorization: Bearer <key>`, `X-Api-Key`, and `X-Goog-Api-Key`. Keys passed as
`?key=` or `?auth_token=` query parameters are not visible to request
interceptors and are not matched.

## Selecting requests by header

The header name is matched case-insensitively, and the hyphenated spelling
`WHEXY-CPA-DISABLE-RESPONSE-API` is accepted too. Reverse proxies such as nginx
drop header names containing underscores by default
(`underscores_in_headers off`), so use the hyphenated spelling when CLIProxyAPI
sits behind one.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    disable-response-api:
      enabled: true
      api_keys:
        - sk-n8n-only-key
```

The plugin advertises RPC schema v1 because the request interceptor capability
does not require newer schema features.

## Example

```bash
curl -i http://localhost:8317/v1/responses \
  -H "Authorization: Bearer sk-n8n-only-key" \
  -d '{"model":"gpt-5.4","input":"hi"}'
# HTTP/1.1 404 Not Found  (empty body)
```
