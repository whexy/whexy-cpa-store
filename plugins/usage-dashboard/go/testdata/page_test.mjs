// Tests the Quota section's chart model, the page's first <script>, which
// touches no DOM. Run by TestQuotaPageModel. The pools it draws are worked
// out by the quota/pools route and tested in pools_test.go.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

process.env.TZ = "UTC";

const html = readFileSync(
  new URL("../dashboard.html", import.meta.url),
  "utf8",
);
const model = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const page = vm.createContext({});
vm.runInContext(model, page);
const get = (name) => vm.runInContext(name, page);
const projectionAt = get("projectionAt");
const projectedDrain = get("projectedDrain");
const quotaReadout = get("quotaReadout");
const buildQuotaChart = get("buildQuotaChart");
const quotaSpan = get("quotaSpan");
const formatQuotaDuration = get("formatQuotaDuration");
const formatQuotaRate = get("formatQuotaRate");

const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const NOW = Date.UTC(2026, 9, 26, 12);
const plain = (value) => JSON.parse(JSON.stringify(value));

// One provider of the route, as it sends two accounts: a fell from 90 to
// 81 over the last three hours, b has stood at 60. The projection burns 3
// points an hour, 1.5 from each, until b is dry at 40h; a's 21 then last 7h
// more, and a resets at 50h.
const section = {
  provider: "claude",
  label: "Claude",
  window: { id: "7d", minutes: 10080, label: "7-day limit" },
  account_count: 2,
  accounts: [
    { label: "a@example.com", remaining: 81, reset_at: NOW + 50 * HOUR },
    { label: "b@example.com", remaining: 60, reset_at: NOW + 50 * HOUR },
  ],
  unreported: [],
  capacity: 200,
  remaining: 141,
  burn: { per_hour: 3, source: "recorded", covered_ms: 3 * HOUR },
  projected_burn: 3,
  pool_empty_at: NOW + 47 * HOUR,
  first_empty: { at: NOW + 40 * HOUR, account: "b@example.com" },
  next_reset: { at: NOW + 50 * HOUR, gain: 200 },
  jumps: [{ at: NOW + 50 * HOUR, gain: 200, top: 200, projected: true }],
  recorded: {
    times: [NOW - 3 * DAY, NOW - 3 * HOUR, NOW],
    pool: [null, 150, 141],
    accounts: [
      [null, 90, 81],
      [null, 60, 60],
    ],
  },
  drain: {
    times: [NOW - 3 * DAY + 10 * MINUTE, NOW - 3 * HOUR, NOW],
    values: [null, 0, 3],
  },
  projected: {
    times: [
      NOW,
      NOW + 40 * HOUR,
      NOW + 47 * HOUR,
      NOW + 50 * HOUR,
      NOW + 50 * HOUR,
      NOW + 14 * DAY,
    ],
    pool: [141, 21, 0, 0, 200, 0],
    accounts: [
      [81, 21, 0, 0, 100, 0],
      [60, 0, 0, 0, 100, 0],
    ],
  },
};

test("the projection is linear between points and takes a reset's after", () => {
  assert.deepEqual(
    plain(projectionAt(section.projected, NOW + 20 * HOUR)),
    [51, 30],
  );
  assert.deepEqual(
    plain(projectionAt(section.projected, NOW + 50 * HOUR)),
    [100, 100],
  );
  assert.equal(projectionAt(section.projected, NOW - 1), null);
  assert.equal(projectionAt(section.projected, NOW + 15 * DAY), null);
});

test("the readout holds a recorded point until the next", () => {
  const before = quotaReadout(section, "remaining", NOW - 2 * HOUR, NOW);
  assert.deepEqual(plain(before), {
    kind: "recorded",
    value: 150,
    accounts: [90, 60],
  });
  assert.deepEqual(
    plain(quotaReadout(section, "remaining", NOW - 4 * DAY, NOW)),
    { kind: "recorded", value: null, accounts: [null, null] },
  );
  assert.equal(quotaReadout(section, "drain", NOW, NOW).value, 3);
  assert.equal(quotaReadout(section, "drain", NOW - HOUR, NOW).value, 0);
});

test("the readout projects past now, and the drain stops with the pool", () => {
  const later = quotaReadout(section, "remaining", NOW + 20 * HOUR, NOW);
  assert.deepEqual(plain(later), {
    kind: "projected",
    value: 81,
    accounts: [51, 30],
  });
  assert.equal(quotaReadout(section, "drain", NOW + 20 * HOUR, NOW).value, 3);
  assert.equal(quotaReadout(section, "drain", NOW + 48 * HOUR, NOW).value, 0);
  const drain = projectedDrain(section);
  assert.deepEqual(
    plain(drain.times.map((at) => (at - NOW) / HOUR)),
    [0, 40, 47, 47, 50, 50, 50, 336, 336],
  );
  assert.deepEqual(plain(drain.values), [3, 3, 3, 0, 0, 0, 3, 3, 0]);
});

test("the chart places now and thins day labels when narrow", () => {
  const span = quotaSpan(0, NOW);
  assert.equal(span.start, Date.UTC(2026, 9, 23));
  assert.equal(span.end, Date.UTC(2026, 10, 6));
  const chart = (width) =>
    buildQuotaChart({
      section,
      mode: "remaining",
      start: span.start,
      end: span.end,
      now: NOW,
      width,
      showAccounts: true,
    });
  const wide = chart(1456);
  assert.ok(Math.abs(wide.now.x - wide.x(NOW)) < 1e-9);
  assert.equal(wide.now.y, wide.y(141));
  assert.equal(wide.yMax, 200);
  assert.equal(wide.accountLines.length, 1);
  assert.ok(wide.days.every((day) => day.labelled));
  assert.equal(wide.empty.at, NOW + 47 * HOUR);
  assert.deepEqual(plain(wide.jumps.map((jump) => jump.gain)), [200]);
  const narrow = chart(600).days;
  assert.equal(narrow.length, 14);
  assert.equal(narrow.filter((day) => day.labelled).length, 7);
});

test("the drain chart scales to the larger of recorded and projected burn", () => {
  const drain = buildQuotaChart({
    section,
    mode: "drain",
    start: NOW - 3 * DAY,
    end: NOW + 11 * DAY,
    now: NOW,
    width: 800,
    showAccounts: false,
  });
  assert.equal(drain.yMax, 5);
  assert.equal(drain.empty, null);
  assert.deepEqual(plain(drain.jumps), []);
});

test("a pool without a projection draws only what was recorded", () => {
  const recordedOnly = {
    ...section,
    burn: null,
    projected_burn: null,
    projected: undefined,
  };
  const chart = buildQuotaChart({
    section: recordedOnly,
    mode: "remaining",
    start: NOW - 3 * DAY,
    end: NOW + 11 * DAY,
    now: NOW,
    width: 800,
    showAccounts: true,
  });
  assert.equal(chart.projectedLine, "");
  assert.notEqual(chart.recordedLine, "");
  assert.deepEqual(
    plain(quotaReadout(recordedOnly, "remaining", NOW + HOUR, NOW)),
    { kind: "projected", value: null, accounts: [null, null] },
  );
});

test("formatting", () => {
  assert.equal(
    formatQuotaDuration(2 * DAY + 18 * HOUR + 10 * MINUTE),
    "2d 18h",
  );
  assert.equal(formatQuotaDuration(15 * HOUR + 49 * MINUTE), "15h 49m");
  assert.equal(formatQuotaDuration(40 * MINUTE), "40m");
  assert.equal(formatQuotaRate(0), "0");
  assert.equal(formatQuotaRate(3.74), "3.7");
  assert.equal(formatQuotaRate(12.4), "12");
});
