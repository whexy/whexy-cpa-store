import { pathToFileURL } from "node:url";
import { githubRequest, githubToken } from "./github-app.mjs";
import {
  compareVersions,
  readPlugins,
  RELEASE_VERSION,
  tagName,
  UNRELEASED,
} from "./plugins.mjs";

export function taggedVersions(id, refs) {
  const prefix = `refs/tags/${tagName(id, "")}`;
  return refs
    .map((ref) => ref.ref)
    .filter((ref) => ref.startsWith(prefix))
    .map((ref) => ref.slice(prefix.length))
    .filter((version) => RELEASE_VERSION.test(version));
}

// Tags every plugin version declared on the default branch that has no tag
// yet, at the commit this pipeline just validated. Each tag starts the
// pipeline that builds and publishes that version.
export async function tagReleases(
  env,
  plugins,
  { request = githubRequest, tokenFor = githubToken } = {},
) {
  const branch = env.CI_REPO_DEFAULT_BRANCH || "main";
  if (
    !["push", "manual"].includes(env.CI_PIPELINE_EVENT) ||
    env.CI_COMMIT_BRANCH !== branch
  )
    throw new Error(`Tagging requires a push or manual CI run on ${branch}`);
  if (!/^[0-9a-f]{40}$/.test(env.CI_COMMIT_SHA ?? ""))
    throw new Error("Set CI_COMMIT_SHA to the validated commit");
  const root = `/repos/${env.CI_REPO}`;
  const token = await tokenFor(env);
  const released = async (id) =>
    taggedVersions(
      id,
      await request(
        token,
        `${root}/git/matching-refs/tags/${encodeURIComponent(tagName(id, ""))}`,
      ),
    );
  const created = [];
  for (const plugin of plugins) {
    if (plugin.version === UNRELEASED) continue;
    const tag = tagName(plugin.id, plugin.version);
    const versions = await released(plugin.id);
    if (versions.includes(plugin.version)) continue;
    const newer = versions.find(
      (version) => compareVersions(version, plugin.version) > 0,
    );
    if (newer)
      throw new Error(
        `${plugin.path} declares ${plugin.version}, below the released ${tagName(plugin.id, newer)}`,
      );
    try {
      await request(token, `${root}/git/refs`, {
        method: "POST",
        body: { ref: `refs/tags/${tag}`, sha: env.CI_COMMIT_SHA },
      });
    } catch (error) {
      // Pipelines of consecutive pushes can race to the same tag. Either
      // commit passed validation and declares this version, so the first wins.
      if (
        error.status !== 422 ||
        !(await released(plugin.id)).includes(plugin.version)
      )
        throw error;
      console.log(`${tag} was tagged by a concurrent pipeline`);
      continue;
    }
    console.log(`Tagged ${tag} at ${env.CI_COMMIT_SHA}`);
    created.push(tag);
  }
  return created;
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  if (!process.env.CI_REPO) throw new Error("Set CI_REPO to <owner>/<name>");
  await tagReleases(process.env, await readPlugins());
}
