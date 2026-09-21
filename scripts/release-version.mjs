#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import process from "node:process";

let repository = process.cwd();
let json = false;
for (let index = 2; index < process.argv.length; index += 1) {
  const argument = process.argv[index];
  if (argument === "--json") {
    json = true;
  } else if (argument === "--repository" && process.argv[index + 1]) {
    repository = process.argv[(index += 1)];
  } else {
    throw new Error(`usage: release-version.mjs [--json] [--repository PATH]`);
  }
}

function git(...parameters) {
  return execFileSync("git", ["-C", repository, ...parameters], {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  }).trim();
}

if (git("rev-parse", "--is-shallow-repository") !== "false") {
  throw new Error("release version requires a complete Git history");
}

const commit = git("rev-parse", "HEAD");
const count = Number.parseInt(git("rev-list", "--count", "HEAD"), 10);
if (!Number.isSafeInteger(count) || count < 1) {
  throw new Error("Git commit count is not a positive safe integer");
}
const shortCommit = commit.slice(0, 7);
const matches = git("rev-parse", `--disambiguate=${shortCommit}`)
  .split("\n")
  .filter(Boolean);
if (matches.length !== 1 || matches[0] !== commit) {
  throw new Error(`seven-character Git prefix ${shortCommit} is not unique`);
}

const release = {
  version: `0.0.${count}-git.g${shortCommit}`,
  commit,
  commitCount: count,
  shortCommit,
};
process.stdout.write(
  json ? `${JSON.stringify(release)}\n` : `${release.version}\n`,
);
