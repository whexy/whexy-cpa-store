import { pathToFileURL } from "node:url";
import { githubRequest, githubToken } from "./github-app.mjs";

const REGISTRY_PATH = "registry.json";
const PLUGIN_METADATA = /^plugins\/([^/]+)\/plugin\.json$/;
const RELEASE_VERSION = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;
const ASSET_SUFFIX = /^(.+)-([^-]+)-([^-]+)\.zip$/;
const DIGEST = /^sha256:([0-9a-f]{64})$/;

export function compareVersions(left, right) {
  const parse = (version) => {
    const [core, pre] = version.split(/-(.*)/s);
    return { core: core.split(".").map(Number), pre: pre ?? "" };
  };
  const a = parse(left);
  const b = parse(right);
  for (let index = 0; index < 3; index++)
    if (a.core[index] !== b.core[index]) return a.core[index] - b.core[index];
  if (a.pre === b.pre) return 0;
  if (!a.pre) return 1;
  if (!b.pre) return -1;
  return a.pre.localeCompare(b.pre, "en", { numeric: true });
}

// The registry lists, for every plugin directory on the default branch, the
// highest version among assets named <id>-v<version>-<goos>-<goarch>.zip on
// published releases. Matching asset names rather than tags also covers the
// early releases that shipped several plugins under one v<version> tag.
export function latestArtifacts(ids, releases) {
  // Longest first, so a plugin id that prefixes another cannot claim its assets.
  const candidates = [...ids].sort((a, b) => b.length - a.length);
  const versions = new Map();
  const assetNames = new Set();
  for (const release of releases) {
    if (release.draft || release.prerelease) continue;
    for (const asset of release.assets) {
      const id = candidates.find((candidate) =>
        asset.name.startsWith(`${candidate}-v`),
      );
      const match = id && ASSET_SUFFIX.exec(asset.name.slice(id.length + 2));
      if (!match || !RELEASE_VERSION.test(match[1])) continue;
      if (assetNames.has(asset.name))
        throw new Error(`${asset.name} is attached to more than one release`);
      assetNames.add(asset.name);
      const digest = DIGEST.exec(asset.digest ?? "");
      if (!digest)
        throw new Error(
          `${release.tag_name}/${asset.name} has no sha256 digest`,
        );
      const [, version, goos, goarch] = match;
      const key = `${id}@${version}`;
      const entry = versions.get(key) ?? {
        id,
        version,
        tag: release.tag_name,
        artifacts: [],
      };
      if (entry.tag !== release.tag_name)
        throw new Error(
          `${id} ${version} is split across ${entry.tag} and ${release.tag_name}`,
        );
      entry.artifacts.push({
        goos,
        goarch,
        url: asset.browser_download_url,
        sha256: digest[1],
        size: asset.size,
      });
      versions.set(key, entry);
    }
  }
  const latest = new Map();
  for (const entry of versions.values()) {
    const current = latest.get(entry.id);
    if (!current || compareVersions(entry.version, current.version) > 0)
      latest.set(entry.id, entry);
  }
  return [...latest.values()].map((entry) => ({
    ...entry,
    artifacts: entry.artifacts.sort((a, b) =>
      `${a.goos}/${a.goarch}`.localeCompare(`${b.goos}/${b.goarch}`),
    ),
  }));
}

// plugin.json holds the store metadata; the published asset decides the version.
export function registryEntry(metadata, latest) {
  if (metadata.id !== latest.id)
    throw new Error(
      `plugin.json at ${latest.tag} declares ${metadata.id}, not ${latest.id}`,
    );
  const { version: _version, ...listing } = metadata;
  return {
    ...listing,
    version: latest.version,
    install: { type: "direct", artifacts: latest.artifacts },
  };
}

export function renderRegistry(entries) {
  const plugins = [...entries].sort((a, b) => a.id.localeCompare(b.id));
  return `${JSON.stringify({ schema_version: 2, plugins }, null, 2)}\n`;
}

export function commitTitle(previous, next) {
  const versions = (text) => {
    try {
      return new Map(
        JSON.parse(text).plugins.map((plugin) => [plugin.id, plugin.version]),
      );
    } catch {
      return new Map();
    }
  };
  const before = versions(previous);
  const after = versions(next);
  const changes = [];
  for (const [id, version] of after)
    if (before.get(id) !== version) changes.push(`list ${id} ${version}`);
  for (const id of before.keys())
    if (!after.has(id)) changes.push(`delist ${id}`);
  return `chore(registry): ${changes.length > 0 ? changes.join(", ") : "refresh entries from published releases"}`;
}

async function readBranch(request, token, root, branch) {
  const ref = await request(token, `${root}/git/ref/heads/${branch}`);
  const commit = await request(token, `${root}/git/commits/${ref.object.sha}`);
  const tree = await request(
    token,
    `${root}/git/trees/${commit.tree.sha}?recursive=1`,
  );
  if (tree.truncated)
    throw new Error(`The ${branch} tree listing is truncated`);
  const ids = tree.tree
    .map((entry) => PLUGIN_METADATA.exec(entry.path)?.[1])
    .filter(Boolean);
  const blob = tree.tree.find((entry) => entry.path === REGISTRY_PATH);
  const registry = blob
    ? Buffer.from(
        (await request(token, `${root}/git/blobs/${blob.sha}`)).content,
        "base64",
      ).toString("utf8")
    : "";
  return { sha: ref.object.sha, treeSha: commit.tree.sha, ids, registry };
}

async function listReleases(request, token, root) {
  const releases = [];
  for (let page = 1; ; page++) {
    const batch = await request(
      token,
      `${root}/releases?per_page=100&page=${page}`,
    );
    releases.push(...batch);
    if (batch.length < 100) return releases;
  }
}

async function readMetadata(request, token, root, id, tag) {
  const file = await request(
    token,
    `${root}/contents/plugins/${id}/plugin.json?ref=${encodeURIComponent(tag)}`,
  );
  return JSON.parse(Buffer.from(file.content, "base64").toString("utf8"));
}

export async function publishRegistry(
  env,
  { request = githubRequest, tokenFor = githubToken, dryRun = false } = {},
) {
  if (
    !dryRun &&
    env.CI_PIPELINE_EVENT !== "tag" &&
    env.CI_PIPELINE_EVENT !== "manual"
  )
    throw new Error("Registry publication requires a tag or manual CI run");
  const branch = env.CI_REPO_DEFAULT_BRANCH || "main";
  const root = `/repos/${env.CI_REPO}`;
  const token = await tokenFor(env);
  for (let attempt = 1; ; attempt++) {
    const head = await readBranch(request, token, root, branch);
    const latest = latestArtifacts(
      head.ids,
      await listReleases(request, token, root),
    );
    const entries = await Promise.all(
      latest.map(async (entry) =>
        registryEntry(
          await readMetadata(request, token, root, entry.id, entry.tag),
          entry,
        ),
      ),
    );
    const registry = renderRegistry(entries);
    if (registry === head.registry) {
      console.log(`${REGISTRY_PATH} on ${branch} is current`);
      return null;
    }
    const message = commitTitle(head.registry, registry);
    if (dryRun) {
      console.log(`${message}\n\n${registry}`);
      return null;
    }
    const blob = await request(token, `${root}/git/blobs`, {
      method: "POST",
      body: { content: registry, encoding: "utf-8" },
    });
    const tree = await request(token, `${root}/git/trees`, {
      method: "POST",
      body: {
        base_tree: head.treeSha,
        tree: [
          { path: REGISTRY_PATH, mode: "100644", type: "blob", sha: blob.sha },
        ],
      },
    });
    const commit = await request(token, `${root}/git/commits`, {
      method: "POST",
      body: { message, tree: tree.sha, parents: [head.sha] },
    });
    try {
      // Without force the update fails if the branch moved, so the published
      // registry always matches the plugin list of its parent commit.
      await request(token, `${root}/git/refs/heads/${branch}`, {
        method: "PATCH",
        body: { sha: commit.sha, force: false },
      });
      console.log(`${message} (${commit.sha})`);
      return commit.sha;
    } catch (error) {
      if (error.status !== 422 || attempt === 5) throw error;
      console.log(`${branch} moved during publication; regenerating`);
    }
  }
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  if (!process.env.CI_REPO) throw new Error("Set CI_REPO to <owner>/<name>");
  await publishRegistry(process.env, {
    dryRun: process.argv.includes("--dry-run"),
  });
}
