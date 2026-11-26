# pluginkit

The library every plugin in this store builds on. Plugins require
`github.com/whexy/whexy-cpa-store/pluginkit` and replace it with
`../../../pluginkit`; `plugins/system-prompt` is the smallest complete example.

| Package         | Provides                                                                                           |
| --------------- | -------------------------------------------------------------------------------------------------- |
| `plugin`        | The C ABI exports. `plugin.Serve`, called from `init`, installs the method handler.                |
| `rpc`           | Envelopes (`rpc.OK`, `rpc.Fail`), registration and capabilities, store metadata, secret redaction. |
| `host`          | Host callbacks: HTTP through the host, logs, stream output, credentials.                           |
| `host/hosttest` | An in-process fake host for plugin tests.                                                          |
| `manage`        | Management routes, the page and logo resources, and the page's CSP.                                |
| `ui`            | `kit.css` and `kit.js`, the tokens, components and runtime every Management Center page shares.    |
| `config`        | Decoding the plugin's settings and validating URL and model-prefix settings.                       |
| `datadir`       | The plugin's data directory beside the host's plugins directory.                                   |
| `background`    | Work that outlives a call, stopped before the host unloads the library.                            |

## Pages

`manage` serves a plugin's page with `kit.css` inlined ahead of its first
`<style>` and `kit.js` ahead of its first `<script>`. Pages run under a CSP
that admits only hashed inline code, so the kit is inlined rather than linked.

A page therefore holds only its own rules and logic:

- Styles use the kit's tokens and components, both listed at the top of
  `ui/kit.css`. A page defines tokens only for its own subject, such as chart
  series.
- Scripts reach the shared runtime through the global `kit`: paths, the
  management key (`kit.auth.init`), requests (`kit.api`, `kit.cpa`) and DOM
  helpers (`kit.$`, `kit.h`).
- Page logic tests that run a page's first `<script>` under `node:vm` load
  `ui/kit.js` into the same context first.

## Testing

```bash
go test ./...    # needs a C compiler for the cgo bridge, as in nix develop
```
