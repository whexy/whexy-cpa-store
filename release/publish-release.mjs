import { createHash } from "node:crypto";
import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { setTimeout as sleep } from "node:timers/promises";
import { githubRequest, githubToken } from "./github-app.mjs";
import { pluginForTag, readReleaseState } from "./plugin-tag.mjs";

export async function readArtifacts(dir, plugin) {
  const prefix = `${plugin.id}-v${plugin.version}-`;
  const names = (await readdir(dir))
    .filter((name) => name.startsWith(prefix) && name.endsWith(".zip"))
    .sort();
  if (names.length === 0) throw new Error(`No ${prefix}*.zip in ${dir}`);
  return Promise.all(
    names.map(async (name) => {
      const data = await readFile(join(dir, name));
      const sha256 = createHash("sha256").update(data).digest("hex");
      return { name, data, sha256 };
    }),
  );
}

export async function githubUpload(
  token,
  uploadUrl,
  artifact,
  fetcher = fetch,
) {
  const url = `${uploadUrl.replace(/\{.*\}$/, "")}?name=${encodeURIComponent(artifact.name)}`;
  const response = await fetcher(url, {
    method: "POST",
    headers: {
      authorization: `Bearer ${token}`,
      accept: "application/vnd.github+json",
      "content-type": "application/zip",
      "x-github-api-version": "2022-11-28",
    },
    body: artifact.data,
    signal: AbortSignal.timeout(120_000),
  });
  if (!response.ok)
    throw new Error(`GitHub upload ${artifact.name}: HTTP ${response.status}`);
  return response.json();
}

export async function publishRelease(
  env,
  artifacts,
  {
    request = githubRequest,
    upload = githubUpload,
    tokenFor = githubToken,
    wait = sleep,
  } = {},
) {
  if (env.CI_PIPELINE_EVENT !== "tag")
    throw new Error("Release publication requires a tag event");
  const token = await tokenFor(env);
  const root = `/repos/${env.CI_REPO}`;
  let release;
  // Forced tag creation triggers CI before release-please creates its draft.
  for (let attempt = 0; attempt < 6 && !release; attempt++) {
    for (let page = 1; ; page++) {
      const releases = await request(
        token,
        `${root}/releases?per_page=100&page=${page}`,
      );
      release = releases.find(
        (candidate) => candidate.tag_name === env.CI_COMMIT_TAG,
      );
      if (release || releases.length < 100) break;
    }
    if (!release && attempt < 5) await wait(1000 * 2 ** attempt);
  }
  if (!release)
    throw new Error(
      `No release-please draft found for ${env.CI_COMMIT_TAG}; rerun the tag pipeline after preparation succeeds`,
    );
  if (!release.draft) {
    // Published assets are immutable, so a rerun can only confirm that the
    // release already holds this build.
    for (const artifact of artifacts) {
      const asset = release.assets.find(
        (candidate) => candidate.name === artifact.name,
      );
      if (asset?.digest !== `sha256:${artifact.sha256}`)
        throw new Error(
          `${env.CI_COMMIT_TAG} is published without ${artifact.name} at sha256:${artifact.sha256}; release a new version instead`,
        );
    }
    console.log(`${env.CI_COMMIT_TAG} is already published with this build`);
    return;
  }
  for (const artifact of artifacts) {
    // A failed earlier run may have left a partial upload on the draft.
    const stale = release.assets.find(
      (candidate) => candidate.name === artifact.name,
    );
    if (stale)
      await request(token, `${root}/releases/assets/${stale.id}`, {
        method: "DELETE",
      });
    const asset = await upload(token, release.upload_url, artifact);
    if (asset.digest && asset.digest !== `sha256:${artifact.sha256}`)
      throw new Error(
        `GitHub stored ${artifact.name} as ${asset.digest}, not sha256:${artifact.sha256}`,
      );
    console.log(`Uploaded ${artifact.name} (sha256:${artifact.sha256})`);
  }
  const result = await request(token, `${root}/releases/${release.id}`, {
    method: "PATCH",
    body: { draft: false },
  });
  console.log(`Published ${result.html_url}`);
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  const plugin = pluginForTag(
    process.env.CI_COMMIT_TAG ?? "",
    await readReleaseState(),
  );
  await publishRelease(process.env, await readArtifacts("dist", plugin));
}
