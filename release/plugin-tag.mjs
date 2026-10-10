import { readFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";

export async function readReleaseState(root = new URL("../", import.meta.url)) {
  const read = async (name) =>
    JSON.parse(await readFile(new URL(name, root), "utf8"));
  return {
    config: await read("release-please-config.json"),
    manifest: await read(".release-please-manifest.json"),
  };
}

// release-please tags a plugin release <component>-v<version>; the tag pipeline
// may only publish the version the merged release PR wrote to the manifest.
export function pluginForTag(tag, { config, manifest }) {
  for (const [path, settings] of Object.entries(config.packages)) {
    const version = manifest[path];
    if (version && tag === `${settings.component}-v${version}`)
      return { id: settings.component, path, version };
  }
  throw new Error(
    `Tag ${tag} is not the manifest version of any plugin in .release-please-manifest.json`,
  );
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  const plugin = pluginForTag(
    process.env.CI_COMMIT_TAG ?? "",
    await readReleaseState(),
  );
  console.log(`Releasing ${plugin.id} ${plugin.version}`);
}
