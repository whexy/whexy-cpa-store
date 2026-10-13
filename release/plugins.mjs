import { readdir, readFile } from "node:fs/promises";

export const UNRELEASED = "0.0.0";
export const RELEASE_VERSION = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;
const PLUGIN_VERSION = /^(\s*pluginVersion\s*=\s*")([^"]*)(")/;
const SECTION_HEADING = /^## (?:\[([^\]]+)\]|(\S+))/;

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

export const tagName = (id, version) => `${id}-v${version}`;

// The plugin reports pluginVersion to the host, and CLIProxyAPI offers an
// update whenever it differs from the registry version, so this constant is
// the one version source: a pull request that changes it releases the plugin.
export function declaredVersion(source, path) {
  const versions = source
    .split("\n")
    .map((line) => PLUGIN_VERSION.exec(line)?.[2])
    .filter((version) => version !== undefined);
  if (versions.length !== 1)
    throw new Error(`${path} must declare pluginVersion exactly once`);
  if (!RELEASE_VERSION.test(versions[0]))
    throw new Error(`${path} declares pluginVersion "${versions[0]}"`);
  return versions[0];
}

export function setDeclaredVersion(source, version) {
  return source
    .split("\n")
    .map((line) => line.replace(PLUGIN_VERSION, `$1${version}$3`))
    .join("\n");
}

// A section runs from its "## <version>" or "## [<version>](...)" heading to
// the next one, and becomes the release notes.
export function changelogSection(changelog, version) {
  const lines = changelog.split("\n");
  const versionOf = (line) => {
    const match = SECTION_HEADING.exec(line);
    return match && (match[1] ?? match[2]);
  };
  const start = lines.findIndex((line) => versionOf(line) === version);
  if (start === -1) return undefined;
  const end = lines.findIndex(
    (line, index) => index > start && versionOf(line),
  );
  return lines
    .slice(start, end === -1 ? undefined : end)
    .join("\n")
    .trim();
}

export async function readPlugins(root = new URL("../", import.meta.url)) {
  const entries = await readdir(new URL("plugins/", root), {
    withFileTypes: true,
  });
  const ids = entries
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .sort();
  return Promise.all(
    ids.map(async (id) => {
      const path = `plugins/${id}`;
      const read = (name) => readFile(new URL(`${path}/${name}`, root), "utf8");
      const changelog = await read("CHANGELOG.md").catch((error) => {
        if (error.code === "ENOENT") return "";
        throw error;
      });
      return {
        id,
        path,
        version: declaredVersion(
          await read("go/main.go"),
          `${path}/go/main.go`,
        ),
        changelog,
        metadata: JSON.parse(await read("plugin.json")),
      };
    }),
  );
}
