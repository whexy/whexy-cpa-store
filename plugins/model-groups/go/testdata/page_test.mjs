// Tests the model catalog block of management.html, the page's first
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
const loadModelCatalog = vm.runInContext("loadModelCatalog", page);

// cpa answers get(path, missing) from v8 Management API responses, and
// records the paths it was asked for.
function cpa(responses) {
  const paths = [];
  const get = async (path, missing) => {
    paths.push(path);
    if (path in responses) return structuredClone(responses[path]);
    if (missing !== undefined) return missing;
    throw new Error(path + " · HTTP 404");
  };
  return { get, paths };
}

// models answers list(key) with each key's /v1/models data.
function models(byKey) {
  const keys = [];
  const list = async (key) => {
    keys.push(key);
    const value = byKey[key];
    if (value instanceof Error) throw value;
    return value.map((id) =>
      typeof id === "string" ? { id, object: "model", owned_by: "cpa" } : id,
    );
  };
  return { list, keys };
}

// providers reduces a catalog to model → provider → usable credentials.
function providers(catalog) {
  return JSON.parse(
    JSON.stringify(
      Object.fromEntries(
        [...catalog].map(([id, entry]) => [
          id,
          Object.fromEntries(
            [...entry.providers].map(([provider, counts]) => [
              provider,
              counts.credentials,
            ]),
          ),
        ]),
      ),
    ),
  );
}

async function load(responses, byKey) {
  const api = cpa({ "/credentials": { files: [] }, ...responses });
  const served = models(byKey);
  const errors = [];
  const catalog = await loadModelCatalog({
    get: api.get,
    list: served.list,
    errors,
  });
  return { catalog, errors, paths: api.paths, keys: served.keys };
}

test("config providers join auth files, and groups are not members", async () => {
  const { catalog, errors } = await load(
    {
      "/config/access/api-keys": ["client-a"],
      "/credentials": {
        files: [
          {
            name: "opencode-go.json",
            provider: "opencode-go",
            status: "active",
          },
          {
            name: "claude.json",
            provider: "claude",
            status: "error",
            unavailable: true,
          },
        ],
      },
      "/credentials/models?name=opencode-go.json": {
        models: [{ id: "deepseek-v4.1-flash" }],
      },
      "/credentials/models?name=claude.json": {
        models: [{ id: "claude-opus-5-5" }, { id: "claude-haiku-5-5" }],
      },
      "/config/api-keys": {
        "openai-compatibility": [
          {
            name: "Deepseek",
            "base-url": "https://api.deepseek.com",
            keys: [{ "api-key": "sk-1", "auth-index": "a1" }],
            models: [
              { name: "deepseek-chat", alias: "deepseek-v4.1-flash" },
              { name: "deepseek-v4-pro" },
            ],
          },
          {
            name: "Off",
            disabled: true,
            "base-url": "https://off.example",
            keys: [{ "api-key": "sk-2" }],
            models: [{ name: "deepseek-v4-pro" }],
          },
        ],
        claude: [
          {
            name: "claude-1",
            keys: [{ "api-key": "sk-ant", "excluded-models": ["*HAIKU*"] }],
          },
        ],
      },
      "/routing/model-definitions/claude": {
        channel: "claude",
        models: [{ id: "claude-opus-5-5" }, { id: "claude-haiku-5-5" }],
      },
    },
    {
      "client-a": [
        "deepseek-v4.1-flash",
        "deepseek-v4-pro",
        "claude-opus-5-5",
        "claude-haiku-5-5",
        { id: "auto-group/cheap", object: "model", owned_by: "model-groups" },
        "served-by-nobody-known",
      ],
    },
  );
  assert.deepEqual(errors, []);
  assert.deepEqual(providers(catalog), {
    "deepseek-v4.1-flash": {
      "opencode-go": 1,
      "openai-compatible-deepseek": 1,
    },
    "deepseek-v4-pro": { "openai-compatible-deepseek": 1 },
    "claude-opus-5-5": { claude: 2 },
    "claude-haiku-5-5": { claude: 1 },
    "served-by-nobody-known": {},
  });
  assert.equal(
    catalog.get("claude-opus-5-5").providers.get("claude").cooling,
    1,
  );
});

test("a prefix serves bare and prefixed IDs unless force-model-prefix", async () => {
  const config = {
    "/config/access/api-keys": ["k"],
    "/config/api-keys": {
      "openai-compatibility": [
        {
          name: "OpenRouter",
          prefix: "/or/",
          "base-url": "https://openrouter.ai/api/v1",
          keys: [{ "api-key": "a" }, { "api-key": "b" }],
          models: [
            { name: "moonshotai/kimi-k2", alias: "kimi-k2" },
            { name: "glm-5" },
          ],
        },
      ],
    },
  };
  const served = { k: ["kimi-k2", "or/kimi-k2", "glm-5", "or/glm-5"] };
  const both = { "openai-compatible-openrouter": 2 };

  const loose = await load(config, served);
  assert.deepEqual(providers(loose.catalog), {
    "kimi-k2": both,
    "or/kimi-k2": both,
    "glm-5": both,
    "or/glm-5": both,
  });

  const forced = await load(
    { ...config, "/config/routing/force-model-prefix": true },
    served,
  );
  assert.deepEqual(providers(forced.catalog), {
    "kimi-k2": {},
    "or/kimi-k2": both,
    "glm-5": {},
    "or/glm-5": both,
  });
});

test("a key serves its own models, its group's, or its family's defaults", async () => {
  const { catalog, paths } = await load(
    {
      "/config/access/api-keys": ["k"],
      "/config/api-keys": {
        codex: [
          {
            name: "codex-keys",
            models: [{ name: "gpt-6-luna" }],
            keys: [
              { "api-key": "a" },
              {
                "api-key": "b",
                models: [{ name: "upstream-astra", alias: "gpt-6-astra" }],
              },
              { "api-key": "c", "excluded-models": ["*"] },
              { "api-key": "" },
            ],
          },
        ],
        gemini: [{ name: "g", keys: [{ "api-key": "g1" }] }],
      },
      "/routing/model-definitions/gemini": {
        channel: "gemini",
        models: [{ id: "gemini-3.8-flash", display_name: "Gemini 3.8 Flash" }],
      },
    },
    { k: ["gpt-6-luna", "gpt-6-astra", "gemini-3.8-flash"] },
  );
  assert.deepEqual(providers(catalog), {
    "gpt-6-luna": { codex: 1 },
    "gpt-6-astra": { codex: 1 },
    "gemini-3.8-flash": { gemini: 1 },
  });
  assert.equal(
    catalog.get("gemini-3.8-flash").display_name,
    "Gemini 3.8 Flash",
  );
  assert.deepEqual(
    paths.filter((path) => path.startsWith("/routing/model-definitions/")),
    ["/routing/model-definitions/gemini"],
  );
});

test("every client key reads /v1/models and the lists are merged", async () => {
  const { catalog, errors, keys } = await load(
    { "/config/access/api-keys": ["a", " a ", { "api-key": "b" }, "c"] },
    {
      a: ["only-for-a"],
      b: ["only-for-b", "only-for-a"],
      c: new Error("/v1/models · Invalid API key"),
    },
  );
  assert.deepEqual(keys, ["a", "b", "c"]);
  assert.deepEqual([...catalog.keys()], ["only-for-a", "only-for-b"]);
  assert.deepEqual(errors, ["/v1/models · Invalid API key"]);
});

test("without client keys /v1/models is read without one", async () => {
  const { catalog, keys } = await load({}, { "": ["open-model"] });
  assert.deepEqual(keys, [""]);
  assert.deepEqual([...catalog.keys()], ["open-model"]);
});

test("the catalog fails when no key can read /v1/models", async () => {
  await assert.rejects(
    load(
      { "/config/access/api-keys": ["a"] },
      { a: new Error("/v1/models · HTTP 500") },
    ),
    /HTTP 500/,
  );
});
