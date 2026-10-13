import assert from "node:assert/strict";
import { generateKeyPairSync, verify } from "node:crypto";
import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import {
  changelogEntry,
  nextVersion,
  parseCommit,
  prependSection,
} from "./bump.mjs";
import { githubToken } from "./github-app.mjs";
import { pluginForTag } from "./plugin-tag.mjs";
import {
  changelogSection,
  declaredVersion,
  readPlugins,
  setDeclaredVersion,
  UNRELEASED,
} from "./plugins.mjs";
import { publishRelease, readArtifacts } from "./publish-release.mjs";
import {
  commitTitle,
  latestArtifacts,
  publishRegistry,
  registryEntry,
  renderRegistry,
} from "./registry.mjs";
import { tagReleases } from "./tag-releases.mjs";
import {
  nixArguments,
  pendingReleases,
  validatedPlugins,
  validationScope,
} from "./validate.mjs";

const repository = "whexy/example";
const sha = (char) => char.repeat(64);
const asset = (name, char, extra = {}) => ({
  name,
  digest: `sha256:${sha(char)}`,
  size: 10,
  browser_download_url: `https://github.com/${repository}/releases/download/x/${name}`,
  ...extra,
});
const release = (tag_name, assets, extra = {}) => ({
  tag_name,
  draft: false,
  prerelease: false,
  assets,
  ...extra,
});

test("App authentication signs a valid JWT and restricts the installation token to this repository", async () => {
  const { privateKey, publicKey } = generateKeyPairSync("rsa", {
    modulusLength: 2048,
  });
  const token = await githubToken(
    {
      GITHUB_APP_ID: "123",
      GITHUB_APP_INSTALLATION_ID: "456",
      GITHUB_APP_PRIVATE_KEY: privateKey,
      CI_REPO_NAME: "example",
    },
    async (url, options) => {
      assert.equal(
        url,
        "https://api.github.com/app/installations/456/access_tokens",
      );
      assert.deepEqual(JSON.parse(options.body), { repositories: ["example"] });
      const [header, payload, signature] = options.headers.authorization
        .slice(7)
        .split(".");
      assert.equal(JSON.parse(Buffer.from(payload, "base64url")).iss, "123");
      assert.ok(
        verify(
          "RSA-SHA256",
          Buffer.from(`${header}.${payload}`),
          publicKey,
          Buffer.from(signature, "base64url"),
        ),
      );
      return Response.json({ token: "installation-token" });
    },
  );
  assert.equal(token, "installation-token");
});

test("every plugin declares its version in go/main.go, with release notes for a released one", async () => {
  for (const plugin of await readPlugins()) {
    assert.equal(plugin.metadata.id, plugin.id, `${plugin.path}/plugin.json`);
    // The registry takes versions from published assets; a version in
    // plugin.json would be a second source.
    assert.equal(
      plugin.metadata.version,
      undefined,
      `${plugin.path}/plugin.json`,
    );
    if (plugin.version !== UNRELEASED)
      assert.ok(
        changelogSection(plugin.changelog, plugin.version),
        `${plugin.path}/CHANGELOG.md has no ${plugin.version} section`,
      );
  }
});

test("the version is the single pluginVersion constant", () => {
  const source =
    'const (\n\tpluginVersion = "0.2.0"\n\tother = "1.0.0"\n)\n\nfunc v() string { return pluginVersion }\n';
  assert.equal(declaredVersion(source, "main.go"), "0.2.0");
  const bumped = setDeclaredVersion(source, "0.3.0");
  assert.equal(declaredVersion(bumped, "main.go"), "0.3.0");
  assert.equal(bumped.replace("0.3.0", "0.2.0"), source);
  assert.throws(
    () => declaredVersion("const other = 1\n", "main.go"),
    /exactly once/,
  );
  assert.throws(
    () =>
      declaredVersion(
        source.replace("\tother", '\tpluginVersion = "0.4.0"\n\tother'),
        "main.go",
      ),
    /exactly once/,
  );
  assert.throws(
    () => declaredVersion('pluginVersion = "dev"\n', "main.go"),
    /"dev"/,
  );
});

test("release notes are the version's changelog section", () => {
  const changelog = [
    "# Changelog",
    "",
    "## [0.2.0](https://example.com/compare/foo-v0.1.0...foo-v0.2.0) (2026-10-13)",
    "",
    "",
    "### Features",
    "",
    "* **foo:** two",
    "",
    "## 0.1.0 (2026-10-12)",
    "",
    "* **foo:** one",
    "",
  ].join("\n");
  assert.equal(
    changelogSection(changelog, "0.2.0"),
    "## [0.2.0](https://example.com/compare/foo-v0.1.0...foo-v0.2.0) (2026-10-13)\n\n\n### Features\n\n* **foo:** two",
  );
  assert.equal(
    changelogSection(changelog, "0.1.0"),
    "## 0.1.0 (2026-10-12)\n\n* **foo:** one",
  );
  assert.equal(changelogSection(changelog, "0.1.1"), undefined);
});

test("a version is bumped from the conventional commits since its last tag", () => {
  const commits = (...subjects) =>
    subjects.map((subject) => parseCommit(subject));
  assert.equal(nextVersion(undefined, []), "0.1.0");
  assert.equal(
    nextVersion("0.2.0", commits("fix(foo): a", "docs: b")),
    "0.2.1",
  );
  assert.equal(nextVersion("0.2.1", commits("feat(foo): a")), "0.3.0");
  assert.equal(nextVersion("0.2.1", commits("feat(foo)!: a")), "0.3.0");
  assert.equal(nextVersion("1.2.3", commits("feat(foo)!: a")), "2.0.0");
  assert.equal(
    nextVersion("1.2.3", [parseCommit("fix: a", "BREAKING CHANGE: b")]),
    "2.0.0",
  );
  assert.equal(nextVersion("0.2.0", commits("chore(foo): a")), undefined);
  assert.equal(parseCommit("Merge branch main"), undefined);
});

test("a changelog entry groups the releasable commits under its version", () => {
  const entry = changelogEntry({
    id: "foo",
    version: "0.3.0",
    previous: "0.2.0",
    repository: "https://github.com/whexy/example",
    date: "2026-10-13",
    commits: [
      parseCommit("feat(foo)!: drop a setting"),
      parseCommit("fix(foo): correct a bug"),
      parseCommit("feat: add a page"),
    ],
  });
  assert.equal(
    entry,
    [
      "## [0.3.0](https://github.com/whexy/example/compare/foo-v0.2.0...foo-v0.3.0) (2026-10-13)",
      "",
      "",
      "### ⚠ BREAKING CHANGES",
      "",
      "* **foo:** drop a setting",
      "",
      "### Features",
      "",
      "* **foo:** drop a setting",
      "* add a page",
      "",
      "### Bug Fixes",
      "",
      "* **foo:** correct a bug",
      "",
    ].join("\n"),
  );
  const first = changelogEntry({
    id: "foo",
    version: "0.1.0",
    date: "2026-10-13",
    commits: [],
  });
  assert.equal(
    prependSection("", first),
    "# Changelog\n\n## 0.1.0 (2026-10-13)\n",
  );
  assert.equal(
    prependSection(prependSection("", first), entry).split("\n## ").length,
    3,
  );
  assert.equal(
    changelogSection(prependSection("", entry), "0.3.0"),
    entry.trimEnd(),
  );
});

test("a tag pipeline only publishes the version declared in its tree", () => {
  const plugins = [
    { id: "foo", version: "0.2.0" },
    { id: "foo-bar", version: "1.0.0" },
    { id: "new", version: UNRELEASED },
  ];
  assert.equal(pluginForTag("foo-bar-v1.0.0", plugins).id, "foo-bar");
  assert.equal(pluginForTag("foo-v0.2.0", plugins).id, "foo");
  assert.throws(() => pluginForTag("foo-v0.1.0", plugins), /pluginVersion/);
  assert.throws(() => pluginForTag("new-v0.0.0", plugins), /pluginVersion/);
  assert.throws(() => pluginForTag("v0.2.0", plugins), /pluginVersion/);
});

const headSha = "c".repeat(40);
const pushEnv = {
  CI_PIPELINE_EVENT: "push",
  CI_COMMIT_BRANCH: "main",
  CI_REPO_DEFAULT_BRANCH: "main",
  CI_COMMIT_SHA: headSha,
  CI_REPO: repository,
};

function tagFixture(tags, { racedBy = [] } = {}) {
  const calls = [];
  const existing = new Set(tags);
  const request = async (_token, path, options = {}) => {
    const method = options.method ?? "GET";
    calls.push({ method, path, body: options.body });
    const route = path.slice(`/repos/${repository}`.length);
    if (method === "GET" && route.startsWith("/git/matching-refs/tags/")) {
      const prefix = `refs/tags/${decodeURIComponent(route.slice("/git/matching-refs/tags/".length))}`;
      return [...existing]
        .map((tag) => ({ ref: `refs/tags/${tag}` }))
        .filter(({ ref }) => ref.startsWith(prefix));
    }
    if (method === "POST" && route === "/git/refs") {
      const tag = options.body.ref.slice("refs/tags/".length);
      if (racedBy.includes(tag)) existing.add(tag);
      if (existing.has(tag)) {
        const error = new Error("HTTP 422");
        error.status = 422;
        throw error;
      }
      existing.add(tag);
      return {};
    }
    throw new Error(`Unexpected request: ${method} ${route}`);
  };
  return { calls, deps: { request, tokenFor: async () => "app-token" } };
}

test("main pipelines tag each untagged plugin version at the validated commit", async () => {
  const f = tagFixture(["foo-v0.1.0", "foo-bar-v1.0.0", "v0.3.0"]);
  const created = await tagReleases(
    pushEnv,
    [
      { id: "foo", path: "plugins/foo", version: "0.2.0" },
      { id: "foo-bar", path: "plugins/foo-bar", version: "1.0.0" },
      { id: "new", path: "plugins/new", version: UNRELEASED },
    ],
    f.deps,
  );
  assert.deepEqual(created, ["foo-v0.2.0"]);
  assert.deepEqual(
    f.calls.filter((call) => call.method === "POST").map((call) => call.body),
    [{ ref: "refs/tags/foo-v0.2.0", sha: headSha }],
  );
});

test("a version tagged by a concurrent pipeline is not an error", async () => {
  const f = tagFixture(["foo-v0.1.0"], { racedBy: ["foo-v0.2.0"] });
  assert.deepEqual(
    await tagReleases(
      pushEnv,
      [{ id: "foo", path: "plugins/foo", version: "0.2.0" }],
      f.deps,
    ),
    [],
  );
});

test("a version below a released one is never tagged", async () => {
  const f = tagFixture(["foo-v0.3.0"]);
  await assert.rejects(
    tagReleases(
      pushEnv,
      [{ id: "foo", path: "plugins/foo", version: "0.2.0" }],
      f.deps,
    ),
    /below the released foo-v0.3.0/,
  );
  assert.ok(f.calls.every((call) => call.method === "GET"));
});

test("only default-branch pushes and manual runs tag releases", async () => {
  const plugins = [{ id: "foo", path: "plugins/foo", version: "0.2.0" }];
  for (const env of [
    { ...pushEnv, CI_PIPELINE_EVENT: "pull_request" },
    { ...pushEnv, CI_COMMIT_BRANCH: "feat/foo" },
  ]) {
    const f = tagFixture([]);
    await assert.rejects(tagReleases(env, plugins, f.deps), /push or manual/);
    assert.equal(f.calls.length, 0);
  }
});

const scopePlugins = [
  { id: "foo", version: "0.2.0" },
  { id: "foo-bar", version: "1.0.0" },
  { id: "new", version: UNRELEASED },
];
const scopeRegistry = {
  plugins: [
    { id: "foo", version: "0.2.0" },
    { id: "foo-bar", version: "0.9.0" },
  ],
};
const scopeOf = (files, event = "pull_request") =>
  validationScope(
    {
      CI_PIPELINE_EVENT: event,
      CI_PIPELINE_FILES: files && JSON.stringify(files),
    },
    scopePlugins,
    scopeRegistry,
  );

test("a pipeline validates only the plugins and checks its changed files affect", () => {
  assert.deepEqual(
    scopeOf(["plugins/foo/go/main.go", "plugins/foo/README.md", "README.md"]),
    { checks: ["pre-commit-check", "release"], plugins: ["foo"] },
  );
  assert.deepEqual(scopeOf(["plugins/new/plugin.json", "registry.json"]), {
    checks: ["pre-commit-check", "registry", "release"],
    plugins: ["new"],
  });
  assert.deepEqual(scopeOf(["tools/registry-check/main.go"]), {
    checks: ["pre-commit-check", "registry", "release"],
    plugins: [],
  });
  assert.deepEqual(scopeOf(["release/bump.mjs", "plugins/gone/go/main.go"]), {
    checks: ["pre-commit-check", "release"],
    plugins: [],
  });
  assert.deepEqual(
    nixArguments(scopeOf(["plugins/foo/go/main.go", "registry.json"])),
    [
      "build",
      "--no-link",
      "--print-build-logs",
      ".#checks.x86_64-linux.pre-commit-check",
      ".#checks.x86_64-linux.registry",
      ".#checks.x86_64-linux.release",
      ".#plugins.foo",
    ],
  );
});

test("shared files, manual runs and unlisted changes validate everything", () => {
  for (const files of [
    ["plugins/foo/go/main.go", "flake.lock"],
    ["nix/packages/plugins/default.nix"],
    [".woodpecker.yaml"],
    [],
    undefined,
  ])
    assert.equal(scopeOf(files), null, JSON.stringify(files));
  assert.equal(scopeOf([], "manual"), null);
  assert.deepEqual(nixArguments(null), [
    "flake",
    "check",
    "--print-build-logs",
  ]);
});

test("main pipelines also validate every release the registry does not list yet", () => {
  assert.deepEqual(
    pendingReleases(scopePlugins, scopeRegistry).map((plugin) => plugin.id),
    ["foo-bar"],
  );
  assert.deepEqual(scopeOf(["plugins/foo/go/main.go"], "push").plugins, [
    "foo",
    "foo-bar",
  ]);
  assert.deepEqual(scopeOf(["README.md"], "push").plugins, ["foo-bar"]);
  assert.deepEqual(scopeOf(["README.md"]).plugins, []);
});

test("main pipelines only tag the plugins they validated", () => {
  assert.deepEqual(
    validatedPlugins(scopeOf(["README.md"], "push"), scopePlugins).map(
      (plugin) => plugin.id,
    ),
    ["foo-bar"],
  );
  assert.equal(validatedPlugins(null, scopePlugins), scopePlugins);
});

test("release artifacts are the plugin's zips in the build output", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dist-"));
  await writeFile(join(dir, "foo-v0.2.0-linux-amd64.zip"), "zip");
  await writeFile(join(dir, "foo-v0.1.0-linux-amd64.zip"), "old");
  const artifacts = await readArtifacts(dir, { id: "foo", version: "0.2.0" });
  assert.deepEqual(
    artifacts.map(({ name, sha256 }) => ({ name, sha256 })),
    [
      {
        name: "foo-v0.2.0-linux-amd64.zip",
        sha256:
          "4a70fe9aa6436e02c2dea340fbd1e352e4ef2d8ce6ca52ad25d4b95471fc8bf2",
      },
    ],
  );
  await assert.rejects(
    readArtifacts(dir, { id: "foo", version: "0.3.0" }),
    /No foo-v0.3.0-/,
  );
});

const tagEnv = {
  CI_PIPELINE_EVENT: "tag",
  CI_REPO: repository,
  CI_COMMIT_TAG: "foo-v0.2.0",
};
const tagPlugin = {
  id: "foo",
  path: "plugins/foo",
  version: "0.2.0",
  changelog:
    "# Changelog\n\n## 0.2.0 (2026-10-13)\n\n* **foo:** two\n\n## 0.1.0 (2026-10-12)\n",
};
const artifact = {
  name: "foo-v0.2.0-linux-amd64.zip",
  data: Buffer.from("zip"),
  sha256: sha("a"),
};

function releaseFixture({ draft = true, missing = false, assets = [] } = {}) {
  const calls = [];
  const draftRelease = {
    id: 7,
    tag_name: tagEnv.CI_COMMIT_TAG,
    draft,
    assets,
    upload_url:
      "https://uploads.github.com/repos/whexy/example/releases/7/assets{?name,label}",
  };
  return {
    calls,
    deps: {
      tokenFor: async () => "app-token",
      upload: async (_token, uploadUrl, uploaded) => {
        calls.push({ path: uploadUrl, options: { method: "UPLOAD" } });
        return { name: uploaded.name, digest: `sha256:${uploaded.sha256}` };
      },
      request: async (_token, path, options) => {
        calls.push({ path, options });
        if (path.includes("?per_page=")) return missing ? [] : [draftRelease];
        if (options?.method === "POST")
          return { ...draftRelease, draft: true, assets: [] };
        if (options?.method === "DELETE") return null;
        if (options?.method === "PATCH")
          return { html_url: "https://github.com/whexy/example/releases/7" };
        throw new Error(`Unexpected request: ${path}`);
      },
    },
  };
}

test("publication creates a draft with the changelog section, uploads the build, then publishes it", async () => {
  const f = releaseFixture({ missing: true });
  await publishRelease(tagEnv, tagPlugin, [artifact], f.deps);
  assert.deepEqual(
    f.calls.map((call) => call.options?.method ?? "GET"),
    ["GET", "POST", "UPLOAD", "PATCH"],
  );
  assert.deepEqual(f.calls[1].options.body, {
    tag_name: "foo-v0.2.0",
    name: "foo: v0.2.0",
    body: "## 0.2.0 (2026-10-13)\n\n* **foo:** two",
    draft: true,
  });
  assert.deepEqual(f.calls.at(-1).options.body, { draft: false });
});

test("a release without a changelog section is not created", async () => {
  const f = releaseFixture({ missing: true });
  await assert.rejects(
    publishRelease(
      tagEnv,
      { ...tagPlugin, changelog: "# Changelog\n" },
      [artifact],
      f.deps,
    ),
    /has no 0.2.0 section/,
  );
  assert.ok(f.calls.every((call) => !call.options?.method));
});

test("a rerun reuses the draft and replaces a partial upload", async () => {
  const f = releaseFixture({ assets: [{ id: 3, name: artifact.name }] });
  await publishRelease(tagEnv, tagPlugin, [artifact], f.deps);
  assert.ok(!f.calls.some((call) => call.options?.method === "POST"));
  assert.equal(f.calls[1].path, `/repos/${repository}/releases/assets/3`);
  assert.equal(f.calls[1].options.method, "DELETE");
});

test("a published release is only confirmed, never changed", async () => {
  const same = releaseFixture({
    draft: false,
    assets: [{ name: artifact.name, digest: `sha256:${artifact.sha256}` }],
  });
  await publishRelease(tagEnv, tagPlugin, [artifact], same.deps);
  assert.equal(same.calls.length, 1);
  const different = releaseFixture({
    draft: false,
    assets: [{ name: artifact.name, digest: `sha256:${sha("b")}` }],
  });
  await assert.rejects(
    publishRelease(tagEnv, tagPlugin, [artifact], different.deps),
    /release a new version/,
  );
  assert.equal(different.calls.length, 1);
});

test("only tag pipelines publish releases", async () => {
  const f = releaseFixture();
  await assert.rejects(
    publishRelease(
      { ...tagEnv, CI_PIPELINE_EVENT: "pull_request" },
      tagPlugin,
      [artifact],
      f.deps,
    ),
    /tag event/,
  );
  assert.equal(f.calls.length, 0);
});

test("the registry lists each plugin's highest published asset, including shared legacy releases", () => {
  const latest = latestArtifacts(
    ["foo", "foo-bar"],
    [
      release("foo-v0.4.0", [asset("foo-v0.4.0-linux-amd64.zip", "d")], {
        draft: true,
      }),
      release(
        "foo-v0.3.1-rc.1",
        [asset("foo-v0.3.1-rc.1-linux-amd64.zip", "e")],
        {
          prerelease: true,
        },
      ),
      release("foo-bar-v1.0.0", [
        asset("foo-bar-v1.0.0-linux-arm64.zip", "f"),
        asset("foo-bar-v1.0.0-linux-amd64.zip", "a"),
      ]),
      release("gone-v9.0.0", [asset("gone-v9.0.0-linux-amd64.zip", "c")]),
      release("v0.3.0", [
        asset("foo-v0.3.0-linux-amd64.zip", "b"),
        asset("gone-v0.3.0-linux-amd64.zip", "c"),
      ]),
      release("v0.10.0", [asset("foo-v0.10.0-linux-amd64.zip", "9")], {
        draft: true,
      }),
      release("v0.2.2", [asset("foo-v0.2.2-linux-amd64.zip", "1")]),
    ],
  );
  assert.deepEqual(latest, [
    {
      id: "foo-bar",
      version: "1.0.0",
      tag: "foo-bar-v1.0.0",
      artifacts: [
        {
          goos: "linux",
          goarch: "amd64",
          url: `https://github.com/${repository}/releases/download/x/foo-bar-v1.0.0-linux-amd64.zip`,
          sha256: sha("a"),
          size: 10,
        },
        {
          goos: "linux",
          goarch: "arm64",
          url: `https://github.com/${repository}/releases/download/x/foo-bar-v1.0.0-linux-arm64.zip`,
          sha256: sha("f"),
          size: 10,
        },
      ],
    },
    {
      id: "foo",
      version: "0.3.0",
      tag: "v0.3.0",
      artifacts: [
        {
          goos: "linux",
          goarch: "amd64",
          url: `https://github.com/${repository}/releases/download/x/foo-v0.3.0-linux-amd64.zip`,
          sha256: sha("b"),
          size: 10,
        },
      ],
    },
  ]);
});

test("versions compare numerically and prereleases sort below releases", () => {
  const latest = latestArtifacts(
    ["foo"],
    ["0.9.0", "0.10.0", "0.10.0-rc.2"].map((version) =>
      release(`foo-v${version}`, [
        asset(`foo-v${version}-linux-amd64.zip`, "a"),
      ]),
    ),
  );
  assert.equal(latest[0].version, "0.10.0");
});

test("ambiguous or unverifiable assets stop registry generation", () => {
  assert.throws(
    () =>
      latestArtifacts(
        ["foo"],
        [
          release("v0.1.0", [asset("foo-v0.1.0-linux-amd64.zip", "a")]),
          release("foo-v0.1.0", [asset("foo-v0.1.0-linux-amd64.zip", "a")]),
        ],
      ),
    /more than one release/,
  );
  assert.throws(
    () =>
      latestArtifacts(
        ["foo"],
        [
          release("foo-v0.1.0", [
            asset("foo-v0.1.0-linux-amd64.zip", "a", { digest: null }),
          ]),
        ],
      ),
    /no sha256 digest/,
  );
});

test("registry entries take metadata from plugin.json and the version from the asset", () => {
  const latest = { id: "foo", version: "0.3.0", tag: "v0.3.0", artifacts: [] };
  assert.deepEqual(
    registryEntry({ id: "foo", name: "Foo", version: "0.2.2" }, latest),
    {
      id: "foo",
      name: "Foo",
      version: "0.3.0",
      install: { type: "direct", artifacts: [] },
    },
  );
  assert.throws(
    () => registryEntry({ id: "bar", name: "Bar" }, latest),
    /declares bar/,
  );
  assert.equal(
    renderRegistry([{ id: "b" }, { id: "a" }]),
    '{\n  "schema_version": 2,\n  "plugins": [\n    {\n      "id": "a"\n    },\n    {\n      "id": "b"\n    }\n  ]\n}\n',
  );
});

test("registry commit titles name the listed and delisted versions", () => {
  const registry = (plugins) => JSON.stringify({ plugins });
  assert.equal(
    commitTitle(
      registry([
        { id: "a", version: "1.0.0" },
        { id: "gone", version: "1.0.0" },
      ]),
      registry([
        { id: "a", version: "1.1.0" },
        { id: "new", version: "0.1.0" },
      ]),
    ),
    "chore(registry): list a 1.1.0, list new 0.1.0, delist gone",
  );
  assert.equal(
    commitTitle(
      registry([{ id: "a", version: "1.0.0" }]),
      registry([{ id: "a", version: "1.0.0" }]),
    ),
    "chore(registry): refresh entries from published releases",
  );
});

function registryFixture({ registry = "", refConflicts = 0 } = {}) {
  const calls = [];
  const encode = (text) => Buffer.from(text).toString("base64");
  const metadata = {
    foo: { id: "foo", name: "Foo", description: "Foo.", author: "Whexy" },
  };
  const request = async (_token, path, options = {}) => {
    const method = options.method ?? "GET";
    calls.push({ method, path, body: options.body });
    const route = `${method} ${path.slice(`/repos/${repository}`.length)}`;
    switch (route) {
      case "GET /git/ref/heads/main":
        return { object: { sha: "head" } };
      case "GET /git/commits/head":
        return { tree: { sha: "tree" } };
      case "GET /git/trees/tree?recursive=1":
        return {
          truncated: false,
          tree: [
            { path: "plugins/foo/plugin.json" },
            { path: "plugins/foo/go/main.go" },
            { path: "registry.json", sha: "registry-blob" },
          ],
        };
      case "GET /git/blobs/registry-blob":
        return { content: encode(registry) };
      case "GET /releases?per_page=100&page=1":
        return [
          release("foo-v0.2.0", [asset("foo-v0.2.0-linux-amd64.zip", "a")]),
        ];
      case "GET /contents/plugins/foo/plugin.json?ref=foo-v0.2.0":
        return { content: encode(JSON.stringify(metadata.foo)) };
      case "POST /git/blobs":
        return { sha: "new-blob" };
      case "POST /git/trees":
        return { sha: "new-tree" };
      case "POST /git/commits":
        return { sha: "new-commit" };
      case "PATCH /git/refs/heads/main":
        if (refConflicts-- > 0) {
          const error = new Error("HTTP 422");
          error.status = 422;
          throw error;
        }
        return {};
      default:
        throw new Error(`Unexpected request: ${route}`);
    }
  };
  return {
    calls,
    deps: { request, tokenFor: async () => "app-token" },
    expected: renderRegistry([
      registryEntry(metadata.foo, {
        id: "foo",
        version: "0.2.0",
        tag: "foo-v0.2.0",
        artifacts: [
          {
            goos: "linux",
            goarch: "amd64",
            url: `https://github.com/${repository}/releases/download/x/foo-v0.2.0-linux-amd64.zip`,
            sha256: sha("a"),
            size: 10,
          },
        ],
      }),
    ]),
  };
}

const registryEnv = {
  CI_PIPELINE_EVENT: "tag",
  CI_REPO: repository,
  CI_REPO_DEFAULT_BRANCH: "main",
};

test("registry publication fast-forwards main with one commit on top of the commit it read", async () => {
  const f = registryFixture();
  assert.equal(await publishRegistry(registryEnv, f.deps), "new-commit");
  const blob = f.calls.find((call) => call.path.endsWith("/git/blobs"));
  assert.equal(blob.body.content, f.expected);
  const commit = f.calls.find((call) => call.path.endsWith("/git/commits"));
  assert.deepEqual(commit.body.parents, ["head"]);
  assert.equal(commit.body.message, "chore(registry): list foo 0.2.0");
  const ref = f.calls.find((call) => call.method === "PATCH");
  assert.deepEqual(ref.body, { sha: "new-commit", force: false });
});

test("a current registry is left untouched", async () => {
  const f = registryFixture();
  const current = registryFixture({ registry: f.expected });
  assert.equal(await publishRegistry(registryEnv, current.deps), null);
  assert.ok(current.calls.every((call) => call.method === "GET"));
});

test("registry publication regenerates when main moves underneath it", async () => {
  const f = registryFixture({ refConflicts: 1 });
  assert.equal(await publishRegistry(registryEnv, f.deps), "new-commit");
  assert.equal(
    f.calls.filter((call) => call.path.endsWith("/git/ref/heads/main")).length,
    2,
  );
});

test("pull request pipelines cannot publish the registry", async () => {
  const f = registryFixture();
  await assert.rejects(
    publishRegistry(
      { ...registryEnv, CI_PIPELINE_EVENT: "pull_request" },
      f.deps,
    ),
    /tag or manual/,
  );
  assert.equal(f.calls.length, 0);
});
