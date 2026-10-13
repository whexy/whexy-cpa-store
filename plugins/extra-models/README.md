# Extra Models

A CLIProxyAPI plugin that adds model IDs to one built-in provider, such as
Codex, Claude or Antigravity, before CLIProxyAPI's model catalog lists them.
The provider's own accounts serve the new models, exactly as they serve the
catalog's.

## Set up

1. Install `extra-models` from Management Center → Plugin Store.
2. In Management Center → Plugins → Extra Models, choose the **provider** and
   enter the **models** as a JSON list, then save:

   ```json
   [
     {
       "id": "gemini-4-argon-high",
       "display_name": "Gemini 4 Argon",
       "context_length": 1048576,
       "max_completion_tokens": 1048576,
       "thinking": {
         "min": 1,
         "max": 65535,
         "dynamic_allowed": true,
         "levels": ["low", "medium", "high"]
       }
     }
   ]
   ```

The models appear in `/v1/models` as soon as the settings are saved, without a
restart. Saving writes them to `config.yaml`, where you can also edit them:

```yaml
plugins:
  configs:
    extra-models:
      enabled: true
      provider: antigravity
      models:
        - id: gemini-4-argon-high
          display_name: Gemini 4 Argon
```

## Model entries

An entry uses the field names of CLIProxyAPI's model catalog,
[`models.json`](https://github.com/router-for-me/models/blob/main/models.json),
so an entry copied from the catalog works unchanged. Only `id` is required, and
it is sent to the upstream as written.

`thinking` declares the model's reasoning controls: a token budget (`min`,
`max`, `zero_allowed`, `dynamic_allowed`) or named `levels`. CLIProxyAPI fits
each request's reasoning setting into them. Without `thinking`, requests keep
the client's reasoning setting and the upstream validates it.

Fields an entry leaves out get the catalog's value for the provider: `type`,
`owned_by`, and for Gemini providers `name: models/<id>`. Catalog fields the
plugin cannot pass to CLIProxyAPI, such as `native_capabilities` and
`supports_web_search`, are ignored.
An entry the plugin cannot use is skipped and reported in CLIProxyAPI's log.

## Limits

- The plugin adds models to one provider. CLIProxyAPI accepts one provider per
  plugin.
- Once the catalog lists the same ID, the catalog's entry is used and the
  plugin's is ignored, so an entry can stay until you clean it up.
- Codex models are added to every Codex account, whatever its plan.
