// Tests the config-editing block of management.html, the page's first
// <script>, which touches no DOM. Run by TestManagementPageLogic.
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
const fn = (name) => vm.runInContext(name, page);
// Values from the page's realm compare by structure only after a JSON trip.
const plain = (value) => JSON.parse(JSON.stringify(value));

const deepseek = {
  id: "opencode-go/deepseek-v4-pro",
  upstream_id: "deepseek-v4-pro",
};

test("Bare ID adds a fork alias and keeps every other entry", () => {
  const withBareID = fn("withBareID");
  const hasBareID = fn("hasBareID");
  const others = [
    { name: "opencode-go/kimi-k3", alias: "k3", "force-mapping": true },
    { name: "opencode-go/glm-5.3", alias: "glm" },
  ];
  const on = plain(withBareID(others, deepseek, true));
  assert.deepEqual(on, [
    ...others,
    {
      name: "opencode-go/deepseek-v4-pro",
      alias: "deepseek-v4-pro",
      fork: true,
    },
  ]);
  assert.equal(hasBareID(on, deepseek), true);
  assert.equal(
    hasBareID(on, { id: "opencode-go/kimi-k3", upstream_id: "kimi-k3" }),
    false,
  );
  assert.deepEqual(plain(withBareID(on, deepseek, false)), others);
  // Turning it on twice leaves one entry.
  assert.equal(withBareID(on, deepseek, true).length, 3);
});

test("Bare ID off removes the entry whatever its spelling or fork flag", () => {
  const withBareID = fn("withBareID");
  const aliases = [
    { name: "OpenCode-Go/DeepSeek-V4-Pro", alias: "DEEPSEEK-V4-PRO" },
  ];
  assert.equal(fn("hasBareID")(aliases, deepseek), true);
  assert.deepEqual(plain(withBareID(aliases, deepseek, false)), []);
});

test("Bare ID refuses an alias another model already uses", () => {
  const taken = [
    { name: "opencode-go/deepseek-v4-flash", alias: "deepseek-v4-pro" },
  ];
  assert.throws(
    () => fn("withBareID")(taken, deepseek, true),
    /already the alias of opencode-go\/deepseek-v4-flash/,
  );
});

test("Disabled adds and removes the exact ID and keeps rules", () => {
  const withExcluded = fn("withExcluded");
  const isExcluded = fn("isExcluded");
  const list = ["opencode-go/claude-*", "opencode-go/kimi-k3"];
  const on = plain(withExcluded(list, deepseek.id, true));
  assert.deepEqual(on, [...list, deepseek.id]);
  assert.equal(isExcluded(on, deepseek.id), true);
  assert.deepEqual(plain(withExcluded(on, deepseek.id, false)), list);
  assert.deepEqual(
    plain(withExcluded(["OPENCODE-GO/DEEPSEEK-V4-PRO"], deepseek.id, false)),
    [],
  );
  // A rule is never an exact entry, even when it would match.
  assert.equal(isExcluded(["opencode-go/deepseek-*"], deepseek.id), false);
  assert.deepEqual(
    plain(withExcluded(["opencode-go/*"], "opencode-go/*", false)),
    ["opencode-go/*"],
  );
});

test("rules match like CPA's matchWildcard", () => {
  const matchRule = fn("matchRule");
  const ruleFor = fn("ruleFor");
  const cases = [
    ["opencode-go/claude-*", "opencode-go/claude-haiku-5-5", true],
    ["opencode-go/claude-*", "opencode-go/kimi-k3", false],
    ["*-preview-free", "opencode-go/step-5-preview-free", true],
    ["opencode-go/*flash*", "opencode-go/deepseek-v4-flash-vision-exp", true],
    ["opencode-go/*flash*", "opencode-go/deepseek-v4-pro", false],
    ["*", "anything", true],
    ["a*a", "a", false],
    ["OPENCODE-GO/KIMI-*", "opencode-go/kimi-k3", true],
    ["opencode-go/kimi-k3", "opencode-go/kimi-k3", true],
    ["", "", false],
  ];
  for (const [pattern, id, want] of cases)
    assert.equal(matchRule(pattern, id), want, pattern + " vs " + id);
  assert.equal(
    ruleFor(["opencode-go/kimi-k3", "opencode-go/deep*"], deepseek.id),
    "opencode-go/deep*",
  );
  assert.equal(ruleFor(["opencode-go/deepseek-v4-pro"], deepseek.id), "");
});

test("channel entries merge every spelling of the provider key", () => {
  const channelEntries = fn("channelEntries");
  assert.deepEqual(
    plain(
      channelEntries({
        "opencode-go": ["a"],
        " OpenCode-Go ": ["b"],
        codex: ["c"],
      }),
    ),
    ["a", "b"],
  );
  assert.deepEqual(plain(channelEntries(null)), []);
  assert.deepEqual(plain(channelEntries({ "opencode-go": "x" })), []);
});

test("channel writes touch only the provider key", () => {
  const channelWrite = fn("channelWrite");
  const map = { codex: [{ name: "gpt", alias: "g" }], "opencode-go": ["x"] };
  assert.deepEqual(plain(channelWrite(map, ["y"])), {
    method: "PUT",
    path: "/opencode-go",
    body: ["y"],
  });
  assert.deepEqual(plain(channelWrite(map, [])), {
    method: "DELETE",
    path: "/opencode-go",
  });
  assert.deepEqual(plain(channelWrite({ codex: [] }, ["y"])), {
    method: "PUT",
    path: "/opencode-go",
    body: ["y"],
  });
  assert.equal(channelWrite({ codex: [] }, []), null);
  assert.equal(channelWrite({}, []), null);
});

test("channel writes fold other spellings into one key and keep the rest", () => {
  const channelWrite = fn("channelWrite");
  const map = {
    codex: [{ name: "gpt", alias: "g" }],
    "OpenCode-Go": ["a"],
    "opencode-go": ["b"],
  };
  assert.deepEqual(plain(channelWrite(map, ["a", "b", "c"])), {
    method: "PUT",
    path: "",
    body: {
      codex: [{ name: "gpt", alias: "g" }],
      "opencode-go": ["a", "b", "c"],
    },
  });
  assert.deepEqual(plain(channelWrite(map, [])), {
    method: "PUT",
    path: "",
    body: { codex: [{ name: "gpt", alias: "g" }] },
  });
});

test("served-by reads provider entries that list their models", () => {
  const into = fn("configServedBy")(
    {
      "openai-compatibility": [
        {
          name: "deepseek",
          models: [{ name: "deepseek-chat", alias: "deepseek-v4-pro" }],
        },
        { name: "off", disabled: true, models: [{ name: "kimi-k3" }] },
      ],
      claude: [{ name: "claude-1", models: [{ name: "claude-haiku-5-5" }] }],
      codex: [{ name: "codex-1" }],
    },
    new (fn("Map"))(),
  );
  fn("addServedBy")(into, "Deepseek-V4-Pro", "kimi");
  assert.deepEqual(
    [...into].map(([id, providers]) => [id, [...providers]]),
    [
      ["deepseek-v4-pro", ["deepseek", "kimi"]],
      ["claude-haiku-5-5", ["claude-1"]],
    ],
  );
});

test("strategy names follow CPA's aliases", () => {
  const routingStrategy = fn("routingStrategy");
  assert.equal(routingStrategy(undefined), "round-robin");
  assert.equal(routingStrategy("WRR"), "weighted-round-robin");
  assert.equal(routingStrategy(" weightedroundrobin "), "weighted-round-robin");
  assert.equal(routingStrategy("ff"), "fill-first");
  assert.equal(routingStrategy("nonsense"), "round-robin");
});

test("token counts are short", () => {
  const tokens = fn("tokens");
  assert.equal(tokens(1000000), "1M");
  assert.equal(tokens(1050000), "1.05M");
  assert.equal(tokens(262144), "262K");
  assert.equal(tokens(0), "");
});
