// Tests kit.js without a DOM, as page logic tests load it. Run by TestKitScript.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("kit.js", import.meta.url), "utf8");

function memoryStorage(entries = {}) {
  const data = new Map(Object.entries(entries));
  return {
    getItem: (key) => (data.has(key) ? data.get(key) : null),
    setItem: (key, value) => data.set(key, String(value)),
    removeItem: (key) => data.delete(key),
  };
}

// load evaluates kit.js in a fresh context whose fetch answers from respond.
function load({ local = {}, session = {}, respond } = {}) {
  const calls = [];
  const context = vm.createContext({
    localStorage: memoryStorage(local),
    sessionStorage: memoryStorage(session),
    fetch: async (url, init) => {
      calls.push({ url, init });
      const { status = 200, body } = respond(url, init);
      return {
        status,
        ok: status >= 200 && status < 300,
        text: async () =>
          body === undefined
            ? ""
            : typeof body === "string"
              ? body
              : JSON.stringify(body),
      };
    },
  });
  vm.runInContext(source, context);
  return { kit: context.kit, calls, context };
}

test("without a DOM the kit exposes its helpers and empty paths", () => {
  const { kit } = load();
  assert.equal(kit.paths.plugin, "/v0/management/");
  assert.equal(typeof kit.h, "function");
  assert.ok(Object.isFrozen(kit));
});

test("a typed key wins over Management Center's", () => {
  const { kit } = load({
    session: { ".management-key": "typed" },
    local: { "cli-proxy-auth": '{"state":{"managementKey":"mc"}}' },
  });
  assert.equal(kit.auth.resolve(), "typed");
  assert.equal(kit.auth.source, "session");
});

test("Management Center's key is used until it is rejected", async () => {
  const { kit, calls } = load({
    local: { "cli-proxy-auth": '{"state":{"managementKey":" mc "}}' },
    respond: () => ({ status: 401, body: { error: "bad" } }),
  });
  assert.equal(kit.auth.resolve(), "mc");
  const rejected = [];
  kit.auth.onRejected((message) => rejected.push(message));
  await assert.rejects(kit.api("/status"), kit.AuthError);
  assert.equal(calls[0].init.headers.Authorization, "Bearer mc");
  assert.equal(rejected.length, 1);
  assert.equal(kit.auth.key, "");
  assert.equal(kit.auth.resolve(), "", "a rejected key is not offered again");
});

test("request encodes bodies and maps failures", async () => {
  const answers = {
    "/missing": { status: 404, body: { error: "not_found" } },
    "/elsewhere": { status: 404 },
    "/proxied": {
      status: 404,
      body: { error: "not_found", message: "No matching proxy route" },
    },
    "/html": { body: "<!doctype html><title>Sign in</title>" },
    "/empty": {},
    "/no-content": { status: 204 },
    "/broken": { status: 500, body: { error: "it broke" } },
    "/bare": { status: 502 },
    "/config": {
      status: 422,
      body: { error: "invalid_config", message: "rules must be a list" },
    },
  };
  const { kit, calls } = load({
    session: { ".management-key": "k" },
    respond: (url) =>
      answers[url.slice(url.lastIndexOf("/"))] ?? { body: { saved: true } },
  });
  kit.auth.resolve();
  assert.deepEqual(
    { ...(await kit.api("/rules", { method: "PUT", body: { a: 1 } })) },
    { saved: true },
  );
  assert.equal(calls[0].init.body, '{"a":1}');
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
  assert.equal(calls[0].init.credentials, "omit");
  assert.deepEqual(await kit.cpa("/missing", { missing: [] }), []);
  for (const path of ["/elsewhere", "/proxied"])
    await assert.rejects(kit.cpa(path, { missing: [] }), (err) => {
      assert.equal(err.status, 404);
      return true;
    });
  for (const path of ["/html", "/empty", "/no-content"])
    await assert.rejects(kit.cpa(path, { missing: [] }), /without JSON/);
  await assert.rejects(kit.api("/broken"), (err) => {
    assert.ok(err instanceof kit.ApiError);
    assert.equal(err.status, 500);
    assert.equal(err.message, "it broke");
    return true;
  });
  await assert.rejects(kit.api("/bare"), /answered HTTP 502/);
  await assert.rejects(kit.cpa("/config"), /^Error: rules must be a list$/);
});

test("request without a key fails before fetching", async () => {
  const { kit, calls } = load({ respond: () => ({}) });
  await assert.rejects(kit.api("/status"), kit.AuthError);
  assert.equal(calls.length, 0);
});
