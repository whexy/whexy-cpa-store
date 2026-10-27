# Reset First

A CLIProxyAPI plugin that sends each request to the account whose quota resets
soonest. When several accounts serve the same model, what an account has left
at its reset is lost, so the one that resets first is used first.

## Set up

1. Install `reset-first` from Management Center → Plugin Store. It works as
   soon as it is enabled.
2. Open Management Center → Reset First to see, for every model routed in the
   past 24 hours, which account its latest request went to and why.

## How an account is chosen

CLIProxyAPI offers the plugin the accounts that can serve the request right
now: at the highest priority that has a ready account, not cooling down, not
already tried for this request. Of those:

1. An account whose quota was never read gets one request, at most every 10
   minutes, so its response reports its quota.
2. Otherwise the plugin picks the account with quota to spare in every window
   whose longest window resets soonest. The weekly window decides between
   accounts; one whose 5-hour window is used up waits until that window resets.
3. A model stays on its account until another one resets more than an hour
   sooner, so readings that differ by minutes do not move sessions between
   accounts and their prompt caches.
4. When no offered account has quota known to spare, CLIProxyAPI chooses as it
   would without the plugin, session affinity included.

A request that fails is retried by CLIProxyAPI on the next account offered.

## Where quota comes from

Only Codex and Claude accounts report their quota. Every response they send
carries it, and the plugin also reads each account's usage endpoint every 10
minutes and shortly after a window resets, so accounts that serve no requests,
or that are used elsewhere, stay current. It reads the access token CLIProxyAPI
keeps and never refreshes one itself.

A usage endpoint that rejects a token is not read again until CLIProxyAPI
stores another one, by refreshing it or by a new login. After a 429 the
plugin waits two intervals before the next read, doubling with each further
429 up to 16 intervals, or longer when the endpoint names a `Retry-After`.

An account with its own `proxy_url` or `base_url` is never polled, because the
plugin's requests would reach the provider from somewhere CLIProxyAPI never
sends that account's traffic; its quota comes from its responses alone. The
quota of other providers' accounts is unknown, so they serve only when no
Codex or Claude account has quota to spare.

## Settings

```yaml
plugins:
  configs:
    reset-first:
      enabled: true
      poll_interval: 10m # at least 1m; off reads quota from responses only
```

## Notes

- CLIProxyAPI consults one scheduler plugin: of those enabled, the one with
  the highest `priority` in `plugins.configs`.
- The latest decisions and readings are kept in `data/reset-first/state.json`
  beside the plugins directory, so they survive a restart.
- Built against the v8.0.27 plugin SDK and tested on CLIProxyAPI v8.0.44.
