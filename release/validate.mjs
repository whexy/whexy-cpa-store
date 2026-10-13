import { spawnSync } from "node:child_process";
import { readFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import { readPlugins, UNRELEASED } from "./plugins.mjs";

const SYSTEM = "x86_64-linux";
const DOCUMENTATION = ["AGENTS.md", "CLAUDE.md", "README.md"];

// Releases that a commit declares and whexy-bot has not listed yet: tagged
// releases still being published, and untagged ones.
export function pendingReleases(plugins, registry) {
  const listed = new Map(
    registry.plugins.map((entry) => [entry.id, entry.version]),
  );
  return plugins.filter(
    (plugin) =>
      plugin.version !== UNRELEASED && listed.get(plugin.id) !== plugin.version,
  );
}

// The checks and plugins a pipeline validates for its changed files, or null
// for everything nix flake check runs. Formatting and lint, which read every
// file, and the release tooling's tests, which read every plugin, always run.
// A change in a plugin's directory builds and tests that plugin. A file
// outside the plugins, the registry, the release tooling and the docs may
// affect every build.
export function validationScope(env, plugins, registry) {
  // Woodpecker lists no files for manual runs and omits lists above 500.
  const files = JSON.parse(env.CI_PIPELINE_FILES || "[]");
  if (files.length === 0) return null;
  const ids = new Set(plugins.map((plugin) => plugin.id));
  const checks = new Set(["pre-commit-check", "release"]);
  const validated = new Set();
  for (const file of files) {
    const plugin = /^plugins\/([^/]+)\/(.+)$/.exec(file);
    if (plugin) {
      // A deleted plugin has nothing left to build.
      if (ids.has(plugin[1])) validated.add(plugin[1]);
      if (plugin[2] === "plugin.json") checks.add("registry");
    } else if (
      file === "registry.json" ||
      file.startsWith("tools/registry-check/")
    ) {
      checks.add("registry");
    } else if (!DOCUMENTATION.includes(file) && !file.startsWith("release/")) {
      return null;
    }
  }
  // tag-releases may tag any pending release at this commit, including one
  // whose own push pipeline a later push cancelled.
  if (env.CI_PIPELINE_EVENT === "push")
    for (const plugin of pendingReleases(plugins, registry))
      validated.add(plugin.id);
  return { checks: [...checks].sort(), plugins: [...validated].sort() };
}

export function nixArguments(scope) {
  if (scope === null) return ["flake", "check", "--print-build-logs"];
  return [
    "build",
    "--no-link",
    "--print-build-logs",
    ...scope.checks.map((check) => `.#checks.${SYSTEM}.${check}`),
    ...scope.plugins.map((id) => `.#plugins.${id}`),
  ];
}

export const validatedPlugins = (scope, plugins) =>
  scope === null
    ? plugins
    : plugins.filter((plugin) => scope.plugins.includes(plugin.id));

export async function readScope(env, root = new URL("../", import.meta.url)) {
  const plugins = await readPlugins(root);
  const registry = JSON.parse(
    await readFile(new URL("registry.json", root), "utf8"),
  );
  return { plugins, scope: validationScope(env, plugins, registry) };
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(process.argv[1]).href
) {
  const args = nixArguments((await readScope(process.env)).scope);
  console.log(`nix ${args.join(" ")}`);
  const { status, error } = spawnSync("nix", args, { stdio: "inherit" });
  if (error) throw error;
  process.exitCode = status ?? 1;
}
