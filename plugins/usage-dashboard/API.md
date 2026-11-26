# Usage Dashboard API

The Usage page reads these two routes of CLIProxyAPI's Management API, and
any other client can read them the same way. Field names are stable; new
fields may be added. The plugin's other routes, `GET quota` and `GET`/`PUT
prices`, serve the page and are not covered here.

Every route takes the management key, as `Authorization: Bearer <key>` or
`X-Management-Key: <key>`. CLIProxyAPI blocks an address for 30 minutes after
five wrong keys, and answers only local requests unless
`remote-management.allow-remote` is on. Answers carry
`Cache-Control: no-store`; each call reads the plugin's state anew. A route
answers any other method with 405 and an `Allow` header.

Times are Unix milliseconds unless they are RFC 3339 strings.

## Usage summary

```
GET /v0/management/usage-dashboard/summary?range=7d&group=family&tz=Europe/London
```

| Parameter | Values                    | Default  |
| --------- | ------------------------- | -------- |
| `range`   | `24h`, `7d`, `30d`, `90d` | `30d`    |
| `group`   | `family`, `account`       | `family` |
| `tz`      | an IANA zone              | `UTC`    |

`24h` has one point per hour ending with the current hour. The day ranges
have one point per day in `tz`, ending with today. A range needs that many
days of retention; `range.ranges` lists the ones available.

```json
{
  "generated_at": "2026-10-12T18:00:00Z",
  "range": {
    "key": "7d",
    "step": "day",
    "from": "2026-10-06T00:00:00Z",
    "to": "2026-10-13T00:00:00Z",
    "tz": "UTC",
    "ranges": ["24h", "7d", "30d"]
  },
  "group_by": "family",
  "totals": {
    "requests": 4,
    "failed": 1,
    "sessions": 3,
    "input": 2000500,
    "cache_read": 2000000,
    "cache_write": 0,
    "output": 1000000,
    "reasoning": 0,
    "processed": 5000500,
    "cost": 36,
    "cache_savings": 9,
    "unpriced_tokens": 500
  },
  "groups": [
    {
      "key": "Claude",
      "label": "Claude",
      "detail": "1 model",
      "color": 2,
      "requests": 3,
      "…": "the other totals fields",
      "series_tokens": [0, 0, 0, 0, 0, 1000000, 4000000],
      "series_cost": [0, 0, 0, 0, 0, 5, 31]
    }
  ],
  "models": [
    {
      "model": "claude-opus-5-5",
      "family": "Claude",
      "color": 2,
      "priced": true,
      "custom_priced": false,
      "requests": 3,
      "…": "the other totals fields"
    }
  ],
  "points": [
    { "point": "2026-10-06", "requests": 0, "…": "the other totals fields" }
  ],
  "pricing": { "enabled": true, "…": "" },
  "store": { "data_dir": "…", "…": "" }
}
```

- `totals`, each group, model and point hold the same counters. `processed`
  is input, cache reads, cache writes and output together; `cost` and
  `cache_savings` are USD, estimated from prices, and leave out the
  `unpriced_tokens`.
- `groups` are model families or accounts, largest first. Past eight, the
  rest fold into one with key `__other__` and color 0. `color` is a palette
  slot from 1 to 8 that a family or account keeps across ranges.
  `series_tokens` and `series_cost` have one value per point.
- `points[].point` is an RFC 3339 hour for `24h` and a `YYYY-MM-DD` day
  otherwise.
- `pricing` says whether cost is estimated (`enabled`), from which catalog
  (`source`, `fetched_at`) and how many custom prices are set (`custom`);
  `pricing.error` appears when prices did not load.
- `store` names the usage directory (`data_dir`), `retention_days` and
  whether usage is being written (`persisting`); `store.error` appears when it
  cannot be.

## Quota pools

```
GET /v0/management/usage-dashboard/quota/pools?burn=24h&usage=1&from=2026-11-14T00:00:00Z&until=2026-11-28T00:00:00Z
```

Each provider's pool: the longest window its accounts report, usually the
weekly limit, summed over its enabled accounts, as recorded and as projected.
The projection is the one the Usage page draws: the burn over the lookback
continues, spread evenly over the accounts with quota left, and each reset
restores its account to 100%.

All parameters are optional:

| Parameter | Meaning                                                  | Default            |
| --------- | -------------------------------------------------------- | ------------------ |
| `burn`    | lookback the burn is measured over, `1h` to `168h`       | `24h`              |
| `usage`   | factor on the burn for the projection, `0` to `100`      | `1`                |
| `from`    | start of the span the series cover, RFC 3339             | 3 days before now  |
| `until`   | end of that span, at most 92 days after `from`           | 14 days after from |
| `step`    | spacing of the recorded and drain series, `10m` to `24h` | `10m`              |
| `series`  | `accounts`, `pool` (no per-account arrays) or `none`     | `accounts`         |

The stats do not depend on the span or step: they always look 14 days ahead
of now. A widget that shows only the stats should ask for `series=none`,
about 1 KB per provider; `series=pool&step=1h` adds a chartable line at a few
KB.

```json
{
  "generated_at": "2026-10-26T12:00:00Z",
  "now": 1793016000000,
  "from": 1792929600000,
  "until": 1793275200000,
  "horizon": 1794225600000,
  "burn_hours": 3,
  "usage_factor": 1,
  "step_minutes": 60,
  "providers": [
    {
      "provider": "claude",
      "label": "Claude",
      "window": { "id": "7d", "minutes": 10080, "label": "7-day limit" },
      "account_count": 2,
      "accounts": [
        {
          "label": "a@example.com",
          "remaining": 81,
          "reset_at": 1793196000000
        },
        { "label": "b@example.com", "remaining": 60, "reset_at": 1793196000000 }
      ],
      "unreported": ["c@example.com"],
      "capacity": 200,
      "remaining": 141,
      "burn": { "per_hour": 3, "source": "recorded", "covered_ms": 10800000 },
      "projected_burn": 3,
      "pool_empty_at": 1793185200000,
      "first_empty": { "at": 1793160000000, "account": "b@example.com" },
      "next_reset": { "at": 1793196000000, "gain": 200 },
      "jumps": [
        { "at": 1793196000000, "gain": 200, "top": 200, "projected": true }
      ],
      "recorded": {
        "times": [
          1792929600000, 1793001600000, 1793005200000, 1793008800000,
          1793012400000, 1793016000000
        ],
        "pool": [60, 60, 150, 147, 144, 141],
        "accounts": [
          [null, null, 90, 87, 84, 81],
          [60, 60, 60, 60, 60, 60]
        ]
      },
      "drain": {
        "times": [1792933200000, 1793005200000, 1793008800000, 1793016000000],
        "values": [0, 0, 3, 3]
      },
      "projected": {
        "times": [
          1793016000000, 1793160000000, 1793185200000, 1793196000000,
          1793196000000, 1793436000000, 1793800800000, 1793800800000,
          1794040800000, 1794225600000
        ],
        "pool": [141, 21, 0, 0, 200, 0, 0, 200, 0, 0],
        "accounts": [
          [81, 21, 0, 0, 100, 0, 0, 100, 0, 0],
          [60, 0, 0, 0, 100, 0, 0, 100, 0, 0]
        ]
      }
    },
    {
      "provider": "xai",
      "label": "Grok",
      "window": null,
      "account_count": 0,
      "accounts": [],
      "unreported": ["x@example.com"],
      "capacity": 0,
      "remaining": 0,
      "burn": null,
      "projected_burn": null,
      "pool_empty_at": null,
      "first_empty": null,
      "next_reset": null,
      "jumps": []
    }
  ]
}
```

Figures are percentage points: an account holds 0 to 100 and a pool 100 per
account. Rates are points per hour.

- `now` is the plugin's clock, or its latest reading when that is later.
  `horizon` is 14 days after it.
- `providers` come in the order Claude, Codex, Grok (`xai`), Meta,
  OpenCode Go (`opencode-go`), each only when CLIProxyAPI has an enabled
  account of it. A provider none of whose accounts has a reading yet has
  `window: null`, its accounts in `unreported` and nothing else.
- `accounts` are the accounts in the pool, in the order of every
  per-account array; `account_count` is their number and `capacity` is 100
  for each. `reset_at` is the account's next reset, null when unknown.
  `unreported` names enabled accounts with no reading of this window.
- `remaining` is the pool now.
- `burn` is null when nothing is known. Its `source` is `recorded` when it was
  measured over the lookback, and `covered_ms` says how much of the lookback
  the readings covered; it is `window`, with `covered_ms` 0, when less than
  15 minutes was recorded and the burn is the average since each window
  opened.
- `projected_burn` is `burn.per_hour` times `usage`, and null with `burn`.
- `pool_empty_at` is when every account is projected to be dry, at or before
  now when it already is; null when not before `horizon`.
- `first_empty` is the first account to run dry and when; `at` equals `now`
  when one already is. Null when none does before `horizon`.
- `next_reset` is the next instant at which resets restore at least one
  point, with the points they restore together. With no projection it comes
  from the accounts' `reset_at`.
- `jumps` are the resets inside the span that restored at least one point:
  recorded ones and projected ones, with the pool just after (`top`).

The series cover the span and are omitted with `series=none`; `projected` is
also omitted when `burn` is null.

- `recorded` is the pool and each account on the `step` grid from `from` up
  to now or `until`, whichever is earlier, then at now when it is inside the
  span. A value holds until the next point, so a point
  in the middle of a run of equal values is left out. Null means no reading
  yet.
- `drain` is the burn over the lookback ending at each step, null where too
  little was recorded. It also holds until the next point.
- `projected` runs from now to `horizon` or `until`, whichever is later. It
  is linear between points, and a point that lies on that line is left out.
  A reset repeats its instant: before and after. The projected drain is
  `projected_burn` while the pool is above 0, and 0 after.
