import assert from "node:assert/strict";
import { generateKeyPairSync, verify } from "node:crypto";
import { mkdtemp, readdir, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { Manifest, setLogger } from "release-please";
import { buildStrategy } from "release-please/build/src/factory.js";
import { Version } from "release-please/build/src/version.js";
import { TagName } from "release-please/build/src/util/tag-name.js";
import { githubToken } from "./github-app.mjs";
import { pluginForTag, readReleaseState } from "./plugin-tag.mjs";
import { publishRelease, readArtifacts } from "./publish-release.mjs";
import {
  commitTitle,
  latestArtifacts,
  publishRegistry,
  registryEntry,
  renderRegistry,
} from "./registry.mjs";

const repository = "whexy/example";
const quiet = () => {};
setLogger({
  error: quiet,
  warn: quiet,
  info: quiet,
  debug: quiet,
  trace: quiet,
});

const root = new URL("../", import.meta.url);
const readText = (path) => readFile(new URL(path, root), "utf8");
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

async function pluginDirectories() {
  const entries = await readdir(new URL("plugins/", root), {
    withFileTypes: true,
  });
  return entries
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .sort();
}

function goVersion(source) {
  const line = source
    .split("\n")
    .find((candidate) => candidate.includes("x-release-please-version"));
  return /"(\d+\.\d+\.\d+[^"]*)"/.exec(line ?? "")?.[1];
}

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

test("every plugin is a release-please package whose shipped version matches the manifest", async () => {
  const { config, manifest } = await readReleaseState();
  const ids = await pluginDirectories();
  assert.deepEqual(
    Object.keys(config.packages).sort(),
    ids.map((id) => `plugins/${id}`),
  );
  for (const id of ids) {
    const path = `plugins/${id}`;
    assert.equal(config.packages[path].component, id);
    assert.deepEqual(config.packages[path]["extra-files"], ["go/main.go"]);
    const metadata = JSON.parse(await readText(`${path}/plugin.json`));
    assert.equal(metadata.id, id);
    // The registry takes versions from published assets; a version here
    // would be a second source that release-please does not update.
    assert.equal(metadata.version, undefined, `${path}/plugin.json`);
    // CLIProxyAPI offers an update whenever the loaded plugin reports a
    // different version than the registry.
    assert.equal(
      goVersion(await readText(`${path}/go/main.go`)),
      manifest[path] ?? "0.0.0",
      `${path}/go/main.go`,
    );
  }
});

test("a release PR tags <id>-v<version> and advances the version the plugin reports", async () => {
  const contents = async (path) => {
    const parsedContent = await readText(path);
    return {
      parsedContent,
      content: Buffer.from(parsedContent).toString("base64"),
      sha: "test",
    };
  };
  const github = {
    repository: { owner: "whexy", repo: "example", defaultBranch: "main" },
    getFileJson: async (path) => JSON.parse(await readText(path)),
    getFileContentsOnBranch: contents,
    getFileContents: contents,
    findFilesByFilenameAndRef: async () => [],
    findFilesByGlobAndRef: async () => [],
  };
  const manifest = await Manifest.fromManifest(github, "main");
  for (const [path, config] of Object.entries(manifest.repositoryConfig)) {
    // The tag pipeline builds from the forced tag while the release stays a draft.
    assert.equal(config.draft, true);
    assert.equal(config.forceTag, true);
    const current = manifest.releasedVersions[path]?.toString();
    const strategy = await buildStrategy({
      ...config,
      github,
      path,
      targetBranch: "main",
    });
    const pullRequest = await strategy.buildReleasePullRequest(
      [
        {
          sha: "new-sha",
          message: "fix: correct a bug",
          type: "fix",
          scope: null,
          bareMessage: "correct a bug",
          breaking: false,
          notes: [],
          references: [],
          files: [],
        },
      ],
      current && {
        tag: new TagName(Version.parse(current), config.component, "-", true),
        sha: "old-sha",
        notes: "",
      },
    );
    const next = pullRequest.version.toString();
    if (current) assert.notEqual(next, current);
    else assert.equal(next, config.initialVersion);
    assert.equal(
      new TagName(pullRequest.version, config.component, "-", true).toString(),
      `${config.component}-v${next}`,
    );
    const update = pullRequest.updates.find(
      (candidate) => candidate.path === `${path}/go/main.go`,
    );
    assert.ok(update, `Missing release update for ${path}/go/main.go`);
    const updated = update.updater.updateContent(
      await readText(`${path}/go/main.go`),
    );
    assert.equal(goVersion(updated), next);
  }
});

test("only the manifest version of a plugin can be released", () => {
  const state = {
    config: {
      packages: {
        "plugins/foo": { component: "foo" },
        "plugins/foo-bar": { component: "foo-bar" },
      },
    },
    manifest: { "plugins/foo": "0.2.0", "plugins/foo-bar": "1.0.0" },
  };
  assert.deepEqual(pluginForTag("foo-bar-v1.0.0", state), {
    id: "foo-bar",
    path: "plugins/foo-bar",
    version: "1.0.0",
  });
  assert.equal(pluginForTag("foo-v0.2.0", state).id, "foo");
  assert.throws(() => pluginForTag("foo-v0.1.0", state), /manifest version/);
  assert.throws(() => pluginForTag("v0.2.0", state), /manifest version/);
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
const artifact = {
  name: "foo-v0.2.0-linux-amd64.zip",
  data: Buffer.from("zip"),
  sha256: sha("a"),
};

function releaseFixture({ draft = true, missing = false, assets = [] } = {}) {
  const calls = [];
  let waits = 0;
  return {
    calls,
    get waits() {
      return waits;
    },
    deps: {
      tokenFor: async () => "app-token",
      wait: async () => {
        waits++;
      },
      upload: async (_token, uploadUrl, uploaded) => {
        calls.push({ path: uploadUrl, options: { method: "UPLOAD" } });
        return { name: uploaded.name, digest: `sha256:${uploaded.sha256}` };
      },
      request: async (_token, path, options) => {
        calls.push({ path, options });
        if (path.includes("?per_page="))
          return missing
            ? []
            : [
                {
                  id: 7,
                  tag_name: tagEnv.CI_COMMIT_TAG,
                  draft,
                  assets,
                  upload_url:
                    "https://uploads.github.com/repos/whexy/example/releases/7/assets{?name,label}",
                },
              ];
        if (options?.method === "DELETE") return null;
        if (options?.method === "PATCH")
          return { html_url: "https://github.com/whexy/example/releases/7" };
        throw new Error(`Unexpected request: ${path}`);
      },
    },
  };
}

test("publication uploads the build to the draft, then publishes it", async () => {
  const f = releaseFixture();
  await publishRelease(tagEnv, [artifact], f.deps);
  assert.deepEqual(
    f.calls.map((call) => call.options?.method ?? "GET"),
    ["GET", "UPLOAD", "PATCH"],
  );
  assert.deepEqual(f.calls.at(-1).options.body, { draft: false });
});

test("a rerun replaces a partial upload left on the draft", async () => {
  const f = releaseFixture({ assets: [{ id: 3, name: artifact.name }] });
  await publishRelease(tagEnv, [artifact], f.deps);
  assert.equal(f.calls[1].path, `/repos/${repository}/releases/assets/3`);
  assert.equal(f.calls[1].options.method, "DELETE");
});

test("a published release is only confirmed, never changed", async () => {
  const same = releaseFixture({
    draft: false,
    assets: [{ name: artifact.name, digest: `sha256:${artifact.sha256}` }],
  });
  await publishRelease(tagEnv, [artifact], same.deps);
  assert.equal(same.calls.length, 1);
  const different = releaseFixture({
    draft: false,
    assets: [{ name: artifact.name, digest: `sha256:${sha("b")}` }],
  });
  await assert.rejects(
    publishRelease(tagEnv, [artifact], different.deps),
    /release a new version/,
  );
  assert.equal(different.calls.length, 1);
});

test("missing draft waits for the tag/draft creation race, then fails without publishing", async () => {
  const f = releaseFixture({ missing: true });
  await assert.rejects(
    publishRelease(tagEnv, [artifact], f.deps),
    /No release-please draft/,
  );
  assert.equal(f.waits, 5);
  assert.ok(!f.calls.some((call) => call.options?.method === "PATCH"));
});

test("only tag pipelines publish releases", async () => {
  const f = releaseFixture();
  await assert.rejects(
    publishRelease(
      { ...tagEnv, CI_PIPELINE_EVENT: "pull_request" },
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
