// Tests the card logic of management.html, the page's first <script>, which
// touches no DOM. Run by TestManagementPageLogic.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const html = readFileSync(
  new URL("../management.html", import.meta.url),
  "utf8",
);
const logic = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const page = vm.createContext({});
vm.runInContext(logic, page);
const cardModel = vm.runInContext("cardModel", page);
const span = vm.runInContext("span", page);
const used = vm.runInContext("used", page);

const now = Date.parse("2026-10-26T12:00:00Z");
const at = (ms) => new Date(now + ms).toISOString();
const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

const plain = (value) =>
  JSON.parse(JSON.stringify(value).replaceAll("\u00a0", " "));

test("durations name their two largest units", () => {
  assert.equal(span(30 * 1000), "<1\u00a0min");
  assert.equal(span(12 * MINUTE), "12\u00a0min");
  assert.equal(span(3 * HOUR + 20 * MINUTE), "3\u00a0h 20\u00a0min");
  assert.equal(span(4 * DAY + 3 * HOUR + 59 * MINUTE), "4\u00a0d 3\u00a0h");
  assert.equal(span(2 * DAY), "2\u00a0d");
});

test("a percentage never rounds up to 100 while some is left", () => {
  assert.equal(used(99.6), "99.6%");
  assert.equal(used(99.96), "99.9%");
  assert.equal(used(99.4), "99%");
  assert.equal(used(100), "100%");
  assert.equal(used(12.4), "12%");
});

test("a card names its accounts, their quota and why one was chosen", () => {
  const card = {
    model: "gpt-5-codex",
    at: at(-2 * MINUTE),
    since: at(-3 * HOUR),
    chosen: "a",
    reason: "soonest",
    served: 51,
    failed: 2,
    accounts: [
      {
        id: "a",
        label: "a@example.com",
        provider: "codex",
        plan: "plus",
        offered: true,
        kind: "spare",
        decides: 10080,
        reset_at: at(4 * DAY + 3 * HOUR),
        read_at: at(-5 * MINUTE),
        windows: [
          { minutes: 300, used: 12.4, reset_at: at(2 * HOUR) },
          { minutes: 10080, used: 40, reset_at: at(4 * DAY + 3 * HOUR) },
        ],
      },
      {
        id: "b",
        label: "b@example.com",
        provider: "claude",
        offered: true,
        kind: "spent",
        decides: 10080,
        back_at: at(2 * HOUR + 10 * MINUTE),
        windows: [
          { minutes: 300, used: 100, reset_at: at(2 * HOUR + 10 * MINUTE) },
          { minutes: 10080, used: 20, reset_at: at(6 * DAY) },
        ],
      },
      {
        id: "openai-compatibility:team:0",
        label: "openai-compatibility:team:0",
        provider: "openai-compatible-team",
        offered: true,
        kind: "unknown",
      },
      {
        id: "c",
        label: "c@example.com",
        provider: "codex",
        offered: false,
        kind: "spare",
        decides: 10080,
        reset_at: at(DAY),
        windows: [{ minutes: 10080, used: 99, reset_at: at(DAY) }],
      },
    ],
  };
  const m = plain(cardModel(card, now));
  assert.equal(m.model, "gpt-5-codex");
  assert.equal(m.reason, "Resets soonest");
  assert.equal(m.meta, "51 served · 2 failed · 2 min ago");

  const [a, b, team, c] = m.rows;
  assert.equal(a.chosen, true);
  assert.equal(a.chosenText, "Chosen: ");
  assert.equal(a.sub, "Codex · plus");
  assert.deepEqual(
    a.meters.map((meter) => [meter.label, meter.value, meter.decides]),
    [
      ["5h", "12%", false],
      ["7d", "40%", true],
    ],
  );
  assert.equal(a.when, "resets in 4 d 3 h");
  assert.equal(
    a.meters[1].title,
    "7d window, which orders accounts · 40% used · resets in 4 d 3 h · read 3 min before this decision",
  );
  assert.equal(
    a.meters[0].title,
    "5h window · 12% used · resets in 2 h · read 3 min before this decision",
  );

  assert.equal(b.chosen, false);
  assert.equal(b.meters[0].spent, true);
  assert.equal(b.when, "back in 2 h 10 min");
  assert.equal(b.whenAlert, true);
  // A spent account is ordered by when it is back, not by its longest window.
  assert.equal(b.meters[1].decides, false);
  assert.doesNotMatch(b.meters[1].title, /orders accounts/);

  assert.equal(team.sub, "OpenAI-compatible");
  assert.equal(team.note, "quota unknown");
  assert.equal(
    team.noteTitle,
    "Only Codex and Claude accounts report their quota.",
  );
  assert.equal(team.meters.length, 0);

  assert.equal(c.offered, false);
  assert.equal(c.absent, "not offered");
  assert.equal(c.when, "resets in 1 d");
});

test("a renewed window and a failed answer say so", () => {
  const card = {
    model: "claude-sonnet-5",
    at: at(-30 * 1000),
    since: at(-30 * 1000),
    chosen: "a",
    reason: "only",
    served: 1,
    failed: 0,
    outcome: { failed: true, status: 429, at: at(-10 * 1000) },
    accounts: [
      {
        id: "a",
        label: "a@example.com",
        provider: "claude",
        offered: true,
        kind: "spare",
        decides: 10080,
        reset_at: at(7 * DAY),
        windows: [
          { minutes: 300, used: 0, reset_at: at(5 * HOUR), renewed: true },
          { minutes: 10080, used: 0, reset_at: at(7 * DAY), renewed: true },
        ],
      },
    ],
  };
  const m = plain(cardModel(card, now));
  assert.equal(m.meta, "1 served · just now");
  assert.equal(m.reason, "Only account offered");
  assert.equal(m.rows[0].when, "renewed");
  assert.equal(m.rows[0].meters[0].value, "—");
  assert.equal(
    m.rows[0].meters[0].title,
    "5h window · reset since it was read",
  );
  assert.equal(m.rows[0].failed, "failed · HTTP 429");
  assert.equal(m.rows[0].when, "renewed");
});

test("when CPA chose, the card says so and marks the account that answered", () => {
  const card = {
    model: "gemini-3-pro",
    at: at(-HOUR),
    since: at(-2 * HOUR),
    chosen: "key-b",
    by_cpa: true,
    reason: "no_quota",
    served: 4,
    failed: 0,
    accounts: [
      { id: "key-a", provider: "gemini", offered: true, kind: "unknown" },
      {
        id: "key-b",
        provider: "gemini",
        offered: true,
        kind: "unknown",
        problem: "",
      },
    ],
  };
  const m = plain(cardModel(card, now));
  assert.equal(
    m.reason,
    "No offered account known to have quota left, CPA chose",
  );
  assert.equal(m.meta, "4 served · 1 h ago");
  assert.deepEqual(
    m.rows.map((row) => [row.label, row.chosen, row.chosenText]),
    [
      ["key-a", false, ""],
      ["key-b", true, "CPA's choice: "],
    ],
  );
});

test("an old card says which of its resets have passed", () => {
  const card = {
    model: "gpt-5",
    at: at(-6 * HOUR),
    since: at(-6 * HOUR),
    chosen: "a",
    reason: "only_spare",
    served: 3,
    failed: 0,
    accounts: [
      {
        id: "a",
        label: "a@example.com",
        provider: "codex",
        offered: true,
        kind: "spare",
        decides: 300,
        reset_at: at(-2 * HOUR),
        windows: [{ minutes: 300, used: 70, reset_at: at(-2 * HOUR) }],
      },
      {
        id: "b",
        label: "b@example.com",
        provider: "codex",
        offered: true,
        kind: "spent",
        decides: 10080,
        back_at: at(-3 * HOUR - 50 * MINUTE),
        windows: [
          { minutes: 300, used: 100, reset_at: at(-3 * HOUR - 50 * MINUTE) },
          { minutes: 10080, used: 10, reset_at: at(5 * DAY) },
        ],
      },
      {
        id: "c",
        label: "c@example.com",
        provider: "claude",
        offered: true,
        kind: "unknown",
        problem: "token rejected (HTTP 401)",
      },
      {
        id: "d",
        label: "d@example.com",
        provider: "codex",
        offered: true,
        kind: "unknown",
      },
      {
        id: "e",
        label: "e@example.com",
        provider: "claude",
        offered: true,
        kind: "spare",
        decides: 10080,
        windows: [{ minutes: 10080, used: 30 }],
      },
    ],
  };
  const m = plain(cardModel(card, now));
  assert.equal(m.reason, "Only offered account known to have quota left");
  assert.equal(m.rows[0].when, "reset 2 h ago");
  assert.equal(
    m.rows[0].meters[0].title,
    "5h window, which orders accounts · 70% used · reset 2 h ago",
  );
  assert.equal(m.rows[1].when, "reset 3 h 50 min ago");
  assert.equal(m.rows[1].whenAlert, false);
  assert.equal(m.rows[2].note, "quota unknown · token rejected (HTTP 401)");
  assert.equal(m.rows[2].noteTitle, m.rows[2].note);
  assert.equal(m.rows[3].noteTitle, "Not read yet");
  assert.equal(m.rows[4].when, "reset not reported");
  assert.equal(
    m.rows[4].meters[0].title,
    "7d window, which orders accounts · 30% used",
  );
});
