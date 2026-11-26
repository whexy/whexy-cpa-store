import { isMain, readPlugins, tagName, UNRELEASED } from "./plugins.mjs";

// A tag pipeline may only publish the version its own tree declares.
export function pluginForTag(tag, plugins) {
  const plugin = plugins.find(
    (candidate) =>
      candidate.version !== UNRELEASED &&
      tag === tagName(candidate.id, candidate.version),
  );
  if (!plugin)
    throw new Error(
      `Tag ${tag} is not <id>-v<pluginVersion> of any plugin in its tree`,
    );
  return plugin;
}

if (isMain(import.meta.url)) {
  const plugin = pluginForTag(
    process.env.CI_COMMIT_TAG ?? "",
    await readPlugins(),
  );
  console.log(`Releasing ${plugin.id} ${plugin.version}`);
}
