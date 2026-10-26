// Tests the Quota section's model, the page's first <script>, which touches
// no DOM. Run by TestQuotaPageModel.
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
const projectPool = get("projectPool");
const projectionAt = get("projectionAt");
const quotaTrack = get("quotaTrack");
const trackValueAt = get("trackValueAt");
const usageBuckets = get("usageBuckets");
const buildQuotaSection = get("buildQuotaSection");
const buildQuotaChart = get("buildQuotaChart");
const quotaSpan = get("quotaSpan");
const formatQuotaDuration = get("formatQuotaDuration");
const formatQuotaRate = get("formatQuotaRate");
const quotaWindowLabel = get("quotaWindowLabel");

const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const WEEK = 7 * DAY;
const NOW = Date.UTC(2026, 9, 26, 12);
const plain = (value) => JSON.parse(JSON.stringify(value));

// One route sample: a 5-hour window and a weekly one.
const sample = (at, weekly, weeklyReset) => ({
  at,
  windows: [
    { id: "5h", remaining: 50, reset_at: at + HOUR, period_minutes: 300 },
    {
      id: "7d",
      remaining: weekly,
      reset_at: weeklyReset,
      period_minutes: 7 * 24 * 60,
    },
  ],
});

test("the projection splits the burn and hands a dry account's share on", () => {
  const projection = projectPool(
    [
      { remaining: 50, resetAt: NOW + 30 * HOUR, period: WEEK },
      { remaining: 10, resetAt: NOW + 100 * HOUR, period: WEEK },
    ],
    4,
    NOW,
    NOW + 72 * HOUR,
    10 * MINUTE,
  );
  // 2 points an hour each until the small one is dry at 5h, then 4 an hour.
  assert.deepEqual(
    plain(
      projection.empties.map((event) => [
        (event.at - NOW) / HOUR,
        event.account,
      ]),
    ),
    [
      [5, 1],
      [15, 0],
      [55, 0],
    ],
  );
  assert.equal((projection.poolEmptyAt - NOW) / HOUR, 15);
  assert.deepEqual(plain(projection.resets), [
    { at: NOW + 30 * HOUR, account: 0, gain: 100 },
  ]);
  assert.deepEqual(plain(projectionAt(projection, NOW + 30 * HOUR)), [100, 0]);
});

test("the projection repeats resets each period", () => {
  const projection = projectPool(
    [{ remaining: 80, resetAt: NOW + HOUR, period: 5 * HOUR }],
    2,
    NOW,
    NOW + 12 * HOUR,
    10 * MINUTE,
  );
  assert.deepEqual(
    plain(
      projection.resets.map((reset) => [
        (reset.at - NOW) / HOUR,
        Math.round(reset.gain),
      ]),
    ),
    [
      [1, 22],
      [6, 10],
      [11, 10],
    ],
  );
  assert.equal(projection.poolEmptyAt, null);
});

test("a reading holds until the next one, or until its reset", () => {
  const track = quotaTrack(
    [
      sample(NOW + 2 * HOUR, 60, NOW + 3 * HOUR),
      sample(NOW, 70, NOW + 3 * HOUR),
    ],
    "7d",
  );
  assert.deepEqual(plain(track.map((point) => point.at)), [
    NOW,
    NOW + 2 * HOUR,
  ]);
  assert.equal(trackValueAt(track, NOW - 1), null);
  assert.equal(trackValueAt(track, NOW + HOUR), 70);
  assert.equal(trackValueAt(track, NOW + 2 * HOUR), 60);
  assert.equal(trackValueAt(track, NOW + 3 * HOUR), 100);
});

test("use is spread over the time requests were served, not the idle hours", () => {
  const reset = NOW + WEEK;
  // Read at 80 before a night off, again unchanged when requests resume, and
  // at 78 half an hour later.
  const track = quotaTrack(
    [
      sample(NOW, 80, reset),
      sample(NOW + 10 * HOUR, 80, reset),
      sample(NOW + 10.5 * HOUR, 78, reset),
    ],
    "7d",
  );
  const { used, covered } = usageBuckets(track, NOW, HOUR, 11, NOW + 11 * HOUR);
  assert.deepEqual(plain(used.slice(0, 10)), new Array(10).fill(0));
  assert.equal(used[10], 2);
  assert.deepEqual(plain(covered), new Array(11).fill(HOUR));
});

test("across a reset only the use after it counts", () => {
  const track = quotaTrack(
    [sample(NOW, 10, NOW + 30 * MINUTE), sample(NOW + HOUR, 96, NOW + WEEK)],
    "7d",
  );
  const { used } = usageBuckets(track, NOW, 30 * MINUTE, 2, NOW + HOUR);
  assert.deepEqual(plain(used), [0, 4]);
});

const options = {
  now: NOW,
  start: NOW - 3 * DAY,
  end: NOW + 11 * DAY,
  lookbackHours: 3,
  usageFactor: 1,
};

// a falls 3 points an hour over the last three hours; b was last read two
// days ago and has not been used since.
const resetAt = NOW + 50 * HOUR;
const accounts = [
  {
    label: "a@example.com",
    samples: Array.from({ length: 10 }, (_, index) => {
      const at = NOW - 3 * HOUR + index * 20 * MINUTE;
      return sample(at, 81 + (3 * (NOW - at)) / HOUR, resetAt);
    }),
  },
  { label: "b@example.com", samples: [sample(NOW - 2 * DAY, 60, resetAt)] },
  { label: "c@example.com", samples: [] },
];

test("a section pools the weekly window and measures its burn", () => {
  const section = buildQuotaSection(accounts, options);
  assert.equal(section.windowMinutes, 7 * 24 * 60);
  assert.deepEqual(plain(section.names), ["a@example.com", "b@example.com"]);
  assert.deepEqual(plain(section.unseen), ["c@example.com"]);
  assert.equal(section.capacity, 200);
  assert.equal(section.poolNow, 141);
  assert.equal(section.burn.source, "recorded");
  assert.ok(Math.abs(section.burn.perHour - 3) < 1e-9);
  assert.equal(section.burn.coveredMs, 3 * HOUR);
  // 1.5 points an hour each: b is dry at 40h, then a's 21 last 7h more.
  assert.deepEqual(plain(section.firstEmpty), {
    at: NOW + 40 * HOUR,
    name: "b@example.com",
  });
  assert.ok(Math.abs(section.poolEmptyAt - (NOW + 47 * HOUR)) < MINUTE);
  assert.deepEqual(plain(section.nextReset), { at: resetAt, gain: 200 });
  const times = section.recorded.times;
  assert.equal(times[times.length - 1], NOW);
  assert.equal(section.recorded.pool[times.length - 1], 141);
});

test("a section without enough recording burns at the window average", () => {
  const section = buildQuotaSection(
    [{ label: "a", samples: [sample(NOW - 5 * MINUTE, 70, resetAt)] }],
    options,
  );
  const opened = resetAt - WEEK;
  assert.equal(section.burn.source, "window");
  assert.ok(
    Math.abs(section.burn.perHour - 30 / ((NOW - opened) / HOUR)) < 1e-9,
  );
});

test("a recorded reset is labelled once with what it restored", () => {
  const section = buildQuotaSection(
    [
      {
        label: "a",
        samples: [
          sample(NOW - 2 * HOUR, 12, NOW - 90 * MINUTE),
          sample(NOW - HOUR, 100, NOW - 90 * MINUTE + WEEK),
        ],
      },
    ],
    { ...options, usageFactor: 0 },
  );
  assert.deepEqual(plain(section.jumps.filter((jump) => !jump.projected)), [
    { at: NOW - 90 * MINUTE, gain: 88, top: 100, projected: false },
  ]);
});

test("no section without a reading", () => {
  assert.equal(buildQuotaSection([{ label: "a", samples: [] }], options), null);
});

test("the chart places now and thins day labels when narrow", () => {
  const section = buildQuotaSection(accounts, options);
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
  assert.equal(wide.yMax, 200);
  assert.equal(wide.accountLines.length, 1);
  assert.ok(wide.days.every((day) => day.labelled));
  const narrow = chart(600).days;
  assert.equal(narrow.length, 14);
  assert.equal(narrow.filter((day) => day.labelled).length, 7);
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
  assert.equal(quotaWindowLabel(10080), "7-day limit");
  assert.equal(quotaWindowLabel(300), "5-hour limit");
});
