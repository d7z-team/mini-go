#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import { appendFileSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import process from "node:process";
import { codeChanges, requestJSON } from "./changes.mjs";
import { releaseVersion } from "../../scripts/release-version.mjs";

const crateVersionsURL = "https://crates.io/api/v1/crates/mini-go/versions";
const npmURL = "https://registry.npmjs.org/@d7z-team%2fmini-go";

export async function planRelease({
  directory,
  repository,
  event,
  eventName,
  request = requestJSON,
}) {
  if (
    repository !== "d7z-team/mini-go" ||
    event.repository?.full_name !== repository ||
    eventName !== "push" ||
    event.ref !== "refs/heads/main" ||
    event.deleted
  ) {
    return { publish: false, reason: "Not a main push event" };
  }
  const sha = event.after;
  if (!/^[a-f0-9]{40}$/.test(sha)) throw new Error("Invalid release commit");
  const git = (...args) =>
    execFileSync("git", ["-C", directory, ...args], {
      encoding: "utf8",
    }).replace(/\n$/, "");
  if (git("rev-parse", "HEAD") !== sha)
    throw new Error("Checkout does not match the CI commit");
  if (git("rev-parse", "origin/main") !== sha) {
    return {
      publish: false,
      sha,
      reason: "A newer main commit superseded this candidate",
    };
  }
  if (git("status", "--porcelain=v1", "--untracked-files=all")) {
    throw new Error("Release planning requires a clean worktree");
  }

  const release = releaseVersion(directory);
  const npm = await request(npmURL, { allowMissing: true });
  if (
    npm !== null &&
    (!npm?.versions ||
      typeof npm.versions !== "object" ||
      Array.isArray(npm.versions))
  ) {
    throw new Error("Invalid npm versions response");
  }
  const npmVersions = new Set(Object.keys(npm?.versions ?? {}));
  const crateVersions = new Set();
  const pages = new Set();
  let url = `${crateVersionsURL}?per_page=100`;
  while (url) {
    if (pages.has(url)) throw new Error("Repeated crates.io pagination cursor");
    pages.add(url);
    const data = await request(url, { allowMissing: pages.size === 1 });
    if (data === null) break;
    if (
      !Array.isArray(data?.versions) ||
      !data.meta ||
      !("next_page" in data.meta)
    ) {
      throw new Error("Invalid crates.io versions response");
    }
    for (const version of data.versions) {
      if (typeof version.num !== "string")
        throw new Error("Invalid crate version");
      crateVersions.add(version.num);
    }
    if (data.meta.next_page === null) break;
    if (
      typeof data.meta.next_page !== "string" ||
      !data.meta.next_page.startsWith("?")
    ) {
      throw new Error("Invalid crates.io pagination cursor");
    }
    url = `${crateVersionsURL}${data.meta.next_page}`;
  }

  const result = { sha, version: release.version };
  if (npmVersions.has(release.version) && crateVersions.has(release.version)) {
    return {
      ...result,
      publish: false,
      reason: "This version is already published in both registries",
    };
  }
  // A yanked crate version still exists and cannot be uploaded again.
  if (npmVersions.has(release.version) || crateVersions.has(release.version)) {
    return {
      ...result,
      publish: true,
      reason: "Complete a partially published version",
    };
  }
  const common = [...npmVersions]
    .filter((version) => crateVersions.has(version))
    .map((version) => /^0\.0\.([1-9][0-9]*)-git\.g([a-f0-9]{7})$/.exec(version))
    .filter(Boolean)
    .sort((a, b) => Number(b[1]) - Number(a[1]));
  if (!common.length)
    return {
      ...result,
      publish: true,
      reason: "No common snapshot has been published",
    };

  const [, count, short] = common[0];
  if (!Number.isSafeInteger(Number(count)))
    throw new Error("Invalid published commit count");
  const matches = git("rev-parse", `--disambiguate=${short}`)
    .split("\n")
    .filter(Boolean);
  if (matches.length !== 1 || git("cat-file", "-t", matches[0]) !== "commit") {
    throw new Error(
      "Published commit is missing or ambiguous in this repository",
    );
  }
  const base = matches[0];
  if (git("rev-list", "--count", base) !== count)
    throw new Error("Published commit count does not match history");
  git("merge-base", "--is-ancestor", base, sha);
  const nonDocs = codeChanges(directory, base, sha);
  return {
    ...result,
    base,
    publish: nonDocs.length > 0,
    reason: nonDocs.length
      ? "Unpublished non-document changes"
      : "Only documentation differs from the published snapshot",
    changed: nonDocs,
  };
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    const repository = process.env.GITHUB_REPOSITORY;
    const plan = await planRelease({
      directory: process.cwd(),
      repository,
      event: JSON.parse(readFileSync(process.env.GITHUB_EVENT_PATH, "utf8")),
      eventName: process.env.GITHUB_EVENT_NAME,
    });
    console.log(JSON.stringify(plan, null, 2));
    if (process.env.GITHUB_OUTPUT) {
      appendFileSync(
        process.env.GITHUB_OUTPUT,
        `publish=${plan.publish}\nsha=${plan.sha ?? ""}\n`,
      );
    }
    if (process.env.GITHUB_STEP_SUMMARY) {
      appendFileSync(
        process.env.GITHUB_STEP_SUMMARY,
        `### Release decision\n\n${plan.reason}\n\nCommit: \`${plan.sha ?? "none"}\`\n\nVersion: \`${plan.version ?? "none"}\`\n`,
      );
    }
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
