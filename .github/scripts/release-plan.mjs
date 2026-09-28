#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import { appendFileSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import process from "node:process";
import { setTimeout as delay } from "node:timers/promises";
import { codeChanges, requestJSON } from "./changes.mjs";

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

  const release = JSON.parse(
    execFileSync(
      process.execPath,
      [
        fileURLToPath(
          new URL("../../scripts/release-version.mjs", import.meta.url),
        ),
        "--json",
        "--repository",
        directory,
      ],
      { encoding: "utf8" },
    ),
  );
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

export async function waitForCI({
  repository,
  sha,
  request = requestJSON,
  now = () => performance.now(),
  pause = delay,
}) {
  const deadline = now() + 50 * 60_000;
  const headers = { Authorization: `Bearer ${process.env.GH_TOKEN}` };
  const query = new URLSearchParams({
    head_sha: sha,
    branch: "main",
    event: "push",
    per_page: "1",
  });
  while (now() < deadline) {
    const ref = await request(
      `https://api.github.com/repos/${repository}/git/ref/heads/main`,
      { headers },
    );
    if (!/^[a-f0-9]{40}$/.test(ref?.object?.sha))
      throw new Error("Invalid main ref response");
    if (ref.object.sha !== sha)
      return {
        publish: false,
        reason: "A newer main commit superseded this candidate",
      };
    let pending = false;
    for (const workflow of ["go-test.yml", "runtime-rust.yml"]) {
      const data = await request(
        `https://api.github.com/repos/${repository}/actions/workflows/${workflow}/runs?${query}`,
        { headers },
      );
      if (!Array.isArray(data?.workflow_runs))
        throw new Error(`Invalid CI response for ${workflow}`);
      const latest = data.workflow_runs[0];
      if (!latest) {
        pending = true;
        continue;
      }
      if (
        latest.head_sha !== sha ||
        latest.head_branch !== "main" ||
        latest.event !== "push"
      ) {
        throw new Error(
          `${workflow} response does not match the candidate push`,
        );
      }
      if (latest.status !== "completed") {
        pending = true;
        continue;
      }
      if (latest.conclusion !== "success")
        return {
          publish: false,
          reason: `${workflow} finished with ${latest.conclusion}`,
        };
    }
    if (now() >= deadline) break;
    if (!pending)
      return {
        publish: true,
        reason: "Both push workflows succeeded for this commit",
      };
    console.log(`Waiting for Go/Rust push CI for ${sha}`);
    await pause(Math.min(30_000, deadline - now()));
  }
  throw new Error("Timed out waiting for Go/Rust push CI after 50 minutes");
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    const repository = process.env.GITHUB_REPOSITORY;
    let plan = await planRelease({
      directory: process.cwd(),
      repository,
      event: JSON.parse(readFileSync(process.env.GITHUB_EVENT_PATH, "utf8")),
      eventName: process.env.GITHUB_EVENT_NAME,
    });
    console.log(JSON.stringify(plan, null, 2));
    if (plan.publish) {
      plan = { ...plan, ...(await waitForCI({ repository, sha: plan.sha })) };
      console.log(JSON.stringify(plan, null, 2));
    }
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
