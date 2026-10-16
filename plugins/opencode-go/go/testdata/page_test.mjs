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

test("Bare ID aliases are a Go prefix plus the alias, in any spelling", () => {
  const isBareAlias = fn("isBareAlias");
  const prefixes = ["go", "opencode-go"];
  const bare = [
    {
      name: "opencode-go/deepseek-v4-pro",
      alias: "deepseek-v4-pro",
      fork: true,
    },
    { name: "OpenCode-Go/DeepSeek-V4-Pro", alias: " DEEPSEEK-V4-PRO " },
    { name: "go/kimi-k3", alias: "kimi-k3", fork: true },
  ];
  for (const entry of bare) assert.equal(isBareAlias(entry, prefixes), true);
  const others = [
    { name: "opencode-go/kimi-k3", alias: "k3", fork: true },
    { name: "opencode/kimi-k3", alias: "kimi-k3", fork: true },
    { name: "other/kimi-k3", alias: "kimi-k3" },
    { name: "opencode-go/", alias: "" },
    { name: "opencode-go/undefined" },
    null,
    "opencode-go/kimi-k3",
  ];
  for (const entry of others)
    assert.equal(isBareAlias(entry, prefixes), false, JSON.stringify(entry));
});

test("an exact entry is never a rule", () => {
  const isExcluded = fn("isExcluded");
  assert.equal(isExcluded(["OPENCODE-GO/DEEPSEEK-V4-PRO"], deepseek.id), true);
  assert.equal(isExcluded(["opencode-go/deepseek-*"], deepseek.id), false);
});

test("withModelIDs adds and removes IDs once", () => {
  const withModelIDs = fn("withModelIDs");
  const list = ["kimi-k3", "glm-5.3"];
  assert.deepEqual(plain(withModelIDs(list, ["deepseek-v4-pro"], true)), [
    "kimi-k3",
    "glm-5.3",
    "deepseek-v4-pro",
  ]);
  assert.deepEqual(plain(withModelIDs(list, ["KIMI-K3", "x"], true)), [
    "kimi-k3",
    "glm-5.3",
    "x",
  ]);
  assert.deepEqual(plain(withModelIDs(list, ["KIMI-K3"], false)), ["glm-5.3"]);
  assert.deepEqual(plain(fn("stringList")([" a ", "", 3, null, "b"])), [
    "a",
    "b",
  ]);
  assert.deepEqual(plain(fn("stringList")("kimi-k3")), []);
});

test("disabled models are those with an entry or a matching rule", () => {
  const isModelDisabled = fn("isModelDisabled");
  const enabledFirst = fn("enabledFirst");
  const excluded = ["opencode-go/claude-*", "opencode-go/step-5-preview-free"];
  const disabled = ["kimi-k3"];
  const catalog = [
    { id: "opencode-go/kimi-k3", upstream_id: "kimi-k3" },
    { id: "opencode-go/claude-haiku-5-5", upstream_id: "claude-haiku-5-5" },
    { id: "opencode-go/glm-5.3", upstream_id: "glm-5.3" },
    {
      id: "opencode-go/step-5-preview-free",
      upstream_id: "step-5-preview-free",
    },
    { id: "opencode-go/hy3", upstream_id: "hy3" },
  ];
  const isOff = (model) => isModelDisabled(excluded, disabled, model);
  assert.deepEqual(catalog.map(isOff), [true, true, false, true, false]);
  // The models still served keep the catalog's order and come first.
  assert.deepEqual(
    plain(enabledFirst(catalog, isOff).map((model) => model.upstream_id)),
    ["glm-5.3", "hy3", "kimi-k3", "claude-haiku-5-5", "step-5-preview-free"],
  );
});

test("list settings writes touch only their key", () => {
  const listWrite = fn("listWrite");
  for (const key of [
    "disabled_models",
    "zen_models",
    "free_disabled_models",
    "zen_bare_models",
  ]) {
    const node = { enabled: true, model_prefix: "go", [key]: ["a"] };
    assert.deepEqual(plain(listWrite(node, key, ["a", "b"])), {
      method: "PATCH",
      path: "",
      body: { [key]: ["a", "b"] },
    });
    assert.equal(listWrite(node, key, ["a"]), null);
    assert.deepEqual(plain(listWrite(node, key, [])), {
      method: "DELETE",
      path: "/" + key,
    });
    assert.equal(listWrite({ enabled: true }, key, []), null);
    assert.equal(listWrite({}, key, []), null);
    assert.deepEqual(plain(listWrite({}, key, ["a"])), {
      method: "PATCH",
      path: "",
      body: { [key]: ["a"] },
    });
  }
});

test("bare IDs name the families of the plugin serving them", () => {
  const bareFamilies = fn("bareFamilies");
  // With the prefix off, every family lists its models under the bare ID.
  const model = (family) => (id) => ({ id, upstream_id: id, family });
  const go = model("go");
  const zen = model("zen");
  const free = model("free");
  const catalog = [
    go("kimi-k3"),
    go("glm-5.3"),
    go("qwen3.8-max"),
    zen("kimi-k3"),
    zen("glm-5.3"),
    zen("gpt-6-luna"),
    free("cat-jam-free"),
    free("big-pickle"),
  ];
  const state = {
    excluded: ["glm-*"],
    disabled: ["qwen3.8-max"],
    zen: ["kimi-k3", "gpt-6-luna"],
    freeDisabled: ["big-pickle"],
    prefixOff: true,
    serving: ["go", "zen", "free"],
  };
  const served = (map) =>
    Object.fromEntries([...map].map(([id, set]) => [id, [...set].sort()]));
  // glm-5.3 is disabled in Go by a rule and not enabled in Zen;
  // qwen3.8-max is disabled in Go; big-pickle is disabled in Free.
  assert.deepEqual(served(bareFamilies(catalog, state)), {
    "kimi-k3": ["OpenCode Go", "OpenCode Zen"],
    "gpt-6-luna": ["OpenCode Zen"],
    "cat-jam-free": ["OpenCode Free"],
  });
  // With the prefix on, no family serves a bare ID.
  assert.deepEqual(
    served(bareFamilies(catalog, { ...state, prefixOff: false })),
    {},
  );
  // A family nothing has on serves no bare ID.
  assert.deepEqual(
    served(bareFamilies(catalog, { ...state, serving: ["go"] })),
    {
      "kimi-k3": ["OpenCode Go"],
    },
  );
});

test("the routing body sends the shown families, empty fields as null", () => {
  const routingBody = fn("routingBody");
  const fields = {
    goPriority: "-1",
    goWeight: " ",
    zenPriority: "-2",
    zenWeight: "3",
    freePriority: "-3",
  };
  assert.deepEqual(plain(routingBody(fields, ["go", "zen"])), {
    go: { priority: -1, weight: null },
    zen: { priority: -2, weight: 3 },
  });
  assert.deepEqual(plain(routingBody(fields, ["free"])), {
    free: { priority: -3, weight: null },
  });
});

test("prefixes are the configured one and the default", () => {
  const modelPrefixes = fn("modelPrefixes");
  assert.deepEqual(plain(modelPrefixes(undefined)), ["opencode-go"]);
  assert.deepEqual(plain(modelPrefixes(" /go/ ")), ["go", "opencode-go"]);
  assert.deepEqual(plain(modelPrefixes("OpenCode-Go")), ["opencode-go"]);
  assert.deepEqual(plain(modelPrefixes(null)), ["opencode-go"]);
});

test("migration moves exact entries naming a Go model and keeps the rest", () => {
  const migration = fn("migration");
  const ids = ["kimi-k3", "glm-5.3", "deepseek-v4-pro", "claude-haiku-5-5"];
  const prefixes = ["go", "opencode-go"];
  const excluded = [
    "go/kimi-k3",
    "OPENCODE-GO/GLM-5.3",
    "deepseek-v4-pro",
    "opencode-go/claude-*",
    "go/kimi-k3",
    "other/claude-haiku-5-5",
    "opencode-go/retired-model",
    "gpt-6",
  ];
  assert.deepEqual(plain(migration(excluded, ids, prefixes)), {
    moved: ["kimi-k3", "glm-5.3", "deepseek-v4-pro"],
    kept: [
      "opencode-go/claude-*",
      "other/claude-haiku-5-5",
      "opencode-go/retired-model",
      "gpt-6",
    ],
  });
  // The second write removes only what the first one disabled.
  assert.deepEqual(
    plain(migration(excluded, ["kimi-k3"], prefixes).kept),
    excluded.filter((entry) => !/kimi-k3$/.test(entry)),
  );
  assert.deepEqual(plain(migration([], ids, prefixes)), {
    moved: [],
    kept: [],
  });
});

test("prefixed rules are recognized for the disabled-prefix note", () => {
  const prefixedRule = fn("prefixedRule");
  const prefixes = ["go", "opencode-go"];
  assert.equal(prefixedRule("opencode-go/claude-*", prefixes), true);
  assert.equal(prefixedRule(" GO/kimi-* ", prefixes), true);
  assert.equal(prefixedRule("claude-*", prefixes), false);
  assert.equal(prefixedRule("*/claude-*", prefixes), false);
  assert.equal(prefixedRule("gopher/*", prefixes), false);
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
