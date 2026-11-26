import { execFileSync } from "node:child_process";
import { readdir, readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import {
  compareVersions,
  declaredVersion,
  isMain,
  readPlugins,
  RELEASE_VERSION,
  setDeclaredVersion,
  tagName,
  UNRELEASED,
} from "./plugins.mjs";

const KIT_MODULE = "github.com/whexy/whexy-cpa-store/pluginkit";
const KIT_IMPORT = new RegExp(`"${KIT_MODULE}((?:/[\\w.-]+)*)"`, "g");

const INITIAL_VERSION = "0.1.0";
const CONVENTIONAL = /^(\w+)(?:\(([^)]*)\))?(!)?: (.+)$/;
const SECTIONS = [
  ["feat", "Features"],
  ["fix", "Bug Fixes"],
  ["perf", "Performance Improvements"],
  ["revert", "Reverts"],
];

export function parseCommit(subject, body = "") {
  const match = CONVENTIONAL.exec(subject);
  if (!match) return undefined;
  const [, type, scope, bang, description] = match;
  return {
    type,
    scope,
    description,
    breaking: Boolean(bang) || /^BREAKING[ -]CHANGE:/m.test(body),
  };
}

const releasable = (commit) =>
  commit.breaking || SECTIONS.some(([type]) => type === commit.type);

// Before 1.0, breaking changes and features bump the minor version and fixes
// the patch.
export function nextVersion(released, commits) {
  if (!released) return INITIAL_VERSION;
  const changes = commits.filter(releasable);
  if (changes.length === 0) return undefined;
  const [major, minor, patch] = released.split("-")[0].split(".").map(Number);
  if (changes.some((commit) => commit.breaking))
    return major === 0 ? `0.${minor + 1}.0` : `${major + 1}.0.0`;
  if (changes.some((commit) => commit.type === "feat"))
    return `${major}.${minor + 1}.0`;
  return `${major}.${minor}.${patch + 1}`;
}

// Entries carry no commit links: rebase merges rewrite the branch's commits.
export function changelogEntry({
  id,
  version,
  previous,
  repository,
  date,
  commits,
}) {
  const heading = previous
    ? `## [${version}](${repository}/compare/${tagName(id, previous)}...${tagName(id, version)}) (${date})`
    : `## ${version} (${date})`;
  const groups = [
    ["⚠ BREAKING CHANGES", commits.filter((commit) => commit.breaking)],
    ...SECTIONS.map(([type, title]) => [
      title,
      commits.filter((commit) => commit.type === type),
    ]),
  ].filter(([, list]) => list.length > 0);
  const bullet = (commit) =>
    `* ${commit.scope ? `**${commit.scope}:** ` : ""}${commit.description}`;
  return [
    heading,
    "",
    "",
    ...groups.flatMap(([title, list]) => [
      `### ${title}`,
      "",
      ...list.map(bullet),
      "",
    ]),
  ].join("\n");
}

export function prependSection(changelog, section) {
  const rest = changelog.replace(/^# Changelog\n*/, "");
  return `${`# Changelog\n\n${section.trimEnd()}\n\n${rest}`.trimEnd()}\n`;
}

// linkedPackages lists the pluginkit package directories a plugin's build
// links, following imports through pluginkit. sources maps each non-test Go
// file, by repository path, to its contents; the plugin's own files are
// those under pluginDir.
export function linkedPackages(pluginDir, sources) {
  const importsIn = (inScope) =>
    Object.entries(sources)
      .filter(([path]) => inScope(path.slice(0, path.lastIndexOf("/"))))
      .flatMap(([, source]) =>
        [...source.matchAll(KIT_IMPORT)].map(([, sub]) => "pluginkit" + sub),
      );
  const linked = new Set();
  const pending = importsIn(
    (dir) => dir === pluginDir || dir.startsWith(pluginDir + "/"),
  );
  while (pending.length > 0) {
    const dir = pending.pop();
    if (linked.has(dir)) continue;
    linked.add(dir);
    pending.push(...importsIn((other) => other === dir));
  }
  return [...linked].sort();
}

async function goSources(root, dir) {
  const sources = {};
  const walk = async (path) => {
    const entries = await readdir(new URL(path + "/", root), {
      withFileTypes: true,
    }).catch((error) => {
      if (error.code === "ENOENT") return [];
      throw error;
    });
    for (const entry of entries) {
      const child = `${path}/${entry.name}`;
      if (entry.isDirectory() && entry.name !== "testdata") await walk(child);
      else if (entry.name.endsWith(".go") && !entry.name.endsWith("_test.go"))
        sources[child] = await readFile(new URL(child, root), "utf8");
    }
  };
  await walk(dir);
  return sources;
}

// sourcePaths lists what a plugin's library is built from: its own
// directory and the pluginkit packages it links. A commit to any of them is
// a change to the plugin.
export async function sourcePaths(id, root = new URL("../", import.meta.url)) {
  const sources = {
    ...(await goSources(root, `plugins/${id}/go`)),
    ...(await goSources(root, "pluginkit")),
  };
  return [`plugins/${id}`, ...linkedPackages(`plugins/${id}/go`, sources)];
}

// NothingToRelease means a plugin has no releasable commits since its last
// tag; PendingRelease, that it already declares a version not tagged yet.
export class NothingToRelease extends Error {}
export class PendingRelease extends Error {}

// Writes the next version of plugins/<id> into go/main.go and CHANGELOG.md,
// so the pull request that carries the change also releases it.
export async function bump(
  id,
  requested,
  {
    root = new URL("../", import.meta.url),
    date = new Date().toISOString().slice(0, 10),
    fetchTags = true,
  } = {},
) {
  const git = (...args) =>
    execFileSync("git", args, { cwd: fileURLToPath(root), encoding: "utf8" });
  const path = `plugins/${id}`;
  if (fetchTags) git("fetch", "--quiet", "--tags", "origin");
  const released = git("tag", "--list", tagName(id, "*"))
    .split("\n")
    .map((tag) => tag.slice(tagName(id, "").length))
    .filter((version) => RELEASE_VERSION.test(version))
    .sort(compareVersions)
    .at(-1);
  const mainFile = new URL(`${path}/go/main.go`, root);
  const source = await readFile(mainFile, "utf8");
  const current = declaredVersion(source, `${path}/go/main.go`);
  if (current !== (released ?? UNRELEASED))
    throw new PendingRelease(
      `${path} already declares ${current}; its latest tag is ${released ? tagName(id, released) : "none"}`,
    );
  const commits = git(
    "log",
    "--format=%s%x1f%b%x1e",
    released ? `${tagName(id, released)}..HEAD` : "HEAD",
    "--",
    ...(await sourcePaths(id, root)),
  )
    .split("\x1e")
    .map((entry) => entry.trim())
    .filter(Boolean)
    .map((entry) => parseCommit(...entry.split("\x1f")))
    .filter(Boolean);
  const version = requested ?? nextVersion(released, commits);
  if (!version)
    throw new NothingToRelease(
      `${path} has no feat, fix, perf or revert commits since ${tagName(id, released)}; name a version to release anyway`,
    );
  if (
    !RELEASE_VERSION.test(version) ||
    (released && compareVersions(version, released) <= 0)
  )
    throw new Error(`${version} is not a version above ${released}`);
  const metadata = JSON.parse(
    await readFile(new URL(`${path}/plugin.json`, root), "utf8"),
  );
  const changelogFile = new URL(`${path}/CHANGELOG.md`, root);
  const changelog = await readFile(changelogFile, "utf8").catch((error) => {
    if (error.code === "ENOENT") return "";
    throw error;
  });
  await writeFile(mainFile, setDeclaredVersion(source, version));
  await writeFile(
    changelogFile,
    prependSection(
      changelog,
      changelogEntry({
        id,
        version,
        previous: released,
        repository: metadata.repository,
        date,
        commits: commits.filter(releasable),
      }),
    ),
  );
  return version;
}

// bumpAffected bumps every plugin with releasable commits since its last
// tag, so one pull request that changes pluginkit also releases each plugin
// the change reaches. A plugin that already declares an unreleased version
// is left as it is.
export async function bumpAffected({
  root = new URL("../", import.meta.url),
  date,
} = {}) {
  execFileSync("git", ["fetch", "--quiet", "--tags", "origin"], {
    cwd: fileURLToPath(root),
  });
  const bumped = [];
  for (const { id } of await readPlugins(root)) {
    try {
      bumped.push({
        id,
        version: await bump(id, undefined, { root, date, fetchTags: false }),
      });
    } catch (error) {
      if (
        !(error instanceof NothingToRelease || error instanceof PendingRelease)
      )
        throw error;
    }
  }
  return bumped;
}

if (isMain(import.meta.url)) {
  const [id, version] = process.argv.slice(2);
  if (!id) {
    console.error(
      "Usage: node release/bump.mjs <plugin-id> [<version>]\n       node release/bump.mjs --affected",
    );
    process.exit(2);
  }
  if (id === "--affected") {
    const bumped = await bumpAffected();
    if (bumped.length === 0) {
      console.log("No plugin has releasable commits since its last tag.");
    } else {
      const list = bumped.map((b) => `${b.id} ${b.version}`).join(", ");
      console.log(
        `Commit plugins/ as "chore(release): release ${list}"; merging it releases ${bumped.map((b) => tagName(b.id, b.version)).join(", ")}.`,
      );
    }
  } else {
    const next = await bump(id, version);
    console.log(
      `Commit plugins/${id} as "chore(${id}): release ${next}"; merging it releases ${tagName(id, next)}.`,
    );
  }
}
