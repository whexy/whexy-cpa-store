# AGENTS.md

Several agent sessions work in this repository at once, usually one per
plugin, each in its own worktree. [Release flow](README.md#release-flow)
describes how a plugin is released.

- Change only `plugins/<id>/` of the plugin you were asked to work on. Put
  changes to shared files (`nix/`, `release/`, `tools/`, `flake.lock`,
  `.woodpecker.yaml`) in a pull request of their own, because they change
  every other session's build.
- To release your plugin, run `node release/bump.mjs <id>` as the last commit
  of your pull request. Merging that pull request is the release; CI tags,
  builds, publishes and lists it in `registry.json` without anyone's help.
- Never touch another plugin's version, changelog, pull request or branch,
  never close or merge a pull request you did not open, and never create tags
  or releases by hand.
- `main` does not require an up-to-date branch. Rebase an open pull request
  only when GitHub reports a conflict, not because `main` moved.
- `nix flake check` runs everything the pull request pipeline runs; run it
  before you push.
