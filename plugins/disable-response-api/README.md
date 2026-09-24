# Disable Response API

A CLIProxyAPI request interceptor that rejects OpenAI Responses API calls
(`/v1/responses` and `/v1/responses/compact`) when the client sends the
`WHEXY_CPA_DISABLE_RESPONSE_API` header. The header value is ignored. Matching
requests get `404 Not Found` with an empty body before credential selection, so
no upstream call is made. Requests to other APIs, and Responses API requests
without the header, pass through unchanged.

The header name is matched case-insensitively, and the hyphenated spelling
`WHEXY-CPA-DISABLE-RESPONSE-API` is accepted too. Reverse proxies such as nginx
drop header names containing underscores by default
(`underscores_in_headers off`), so use the hyphenated spelling when CLIProxyAPI
sits behind one.

The plugin advertises RPC schema v1 because the request interceptor capability
does not require newer schema features.

## Configuration

The plugin has no options.

```yaml
plugins:
  enabled: true
  configs:
    disable-response-api:
      enabled: true
```

## Example

```bash
curl -i http://localhost:8317/v1/responses \
  -H "Authorization: Bearer $KEY" \
  -H "WHEXY_CPA_DISABLE_RESPONSE_API: 1" \
  -d '{"model":"gpt-5.4","input":"hi"}'
# HTTP/1.1 404 Not Found  (empty body)
```
