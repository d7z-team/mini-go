#!/usr/bin/env node

import { execFileSync, spawnSync } from "node:child_process";
import { appendFileSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import process from "node:process";

export function isDocumentation(name) {
  if (name.startsWith("docs/")) return true;
  if (!/\.(md|mdx|rst)$/i.test(name)) return false;
  // Markdown fixtures are inputs; only their README guides are documentation.
  if (/(^|\/)(testdata|fixtures|assets)\//.test(name)) {
    return /^readme(?:[_-][\w-]+)?\.(md|mdx|rst)$/i.test(
      path.posix.basename(name),
    );
  }
  return true;
}

export function codeChanges(directory, base, target) {
  return execFileSync(
    "git",
    [
      "-C",
      directory,
      "diff",
      "--no-renames",
      "--name-only",
      "-z",
      base,
      target,
    ],
    { encoding: "utf8" },
  )
    .split("\0")
    .filter((name) => name && !isDocumentation(name));
}

export async function requestJSON(
  url,
  { allowMissing = false, headers = {} } = {},
) {
  const response = await fetch(url, {
    headers: {
      "User-Agent": "mini-go-ci (https://github.com/d7z-team/mini-go)",
      ...headers,
    },
    signal: AbortSignal.timeout(30_000),
  });
  if (response.status === 404 && allowMissing) return null;
  if (!response.ok) throw new Error(`${url}: HTTP ${response.status}`);
  return response.json();
}

export async function planCI({
  directory,
  repository,
  workflow,
  branch,
  eventName,
  event,
  request = requestJSON,
}) {
  const git = (...args) =>
    execFileSync("git", ["-C", directory, ...args], {
      encoding: "utf8",
    }).trim();
  const target = git("rev-parse", "HEAD");
  let base;
  let compared = target;
  if (eventName === "pull_request") {
    const { base: source, head } = event.pull_request ?? {};
    if (![source?.sha, head?.sha].every((sha) => /^[a-f0-9]{40}$/.test(sha)))
      throw new Error("Invalid PR commit identity");
    compared = head.sha;
    base = git("merge-base", source.sha, head.sha);
  } else if (["push", "schedule", "workflow_dispatch"].includes(eventName)) {
    if (!/^[\w.-]+\.yml$/.test(workflow))
      throw new Error("Invalid workflow filename");
    // Successful doc-only runs can advance the baseline because their code tree is unchanged.
    // Failed/cancelled runs cannot certify the next documentation-only commit.
    for (let page = 1; !base; page++) {
      const query = new URLSearchParams({
        branch,
        status: "success",
        per_page: "100",
        page: String(page),
      });
      if (eventName === "push") query.set("event", "push");
      const data = await request(
        `https://api.github.com/repos/${repository}/actions/workflows/${workflow}/runs?${query}`,
        {
          headers: { Authorization: `Bearer ${process.env.GH_TOKEN}` },
        },
      );
      if (!Array.isArray(data?.workflow_runs))
        throw new Error("Invalid workflow history response");
      for (const run of data.workflow_runs) {
        if (
          run.status !== "completed" ||
          run.conclusion !== "success" ||
          run.head_branch !== branch ||
          (eventName === "push" && run.event !== "push")
        )
          continue;
        if (!/^[a-f0-9]{40}$/.test(run.head_sha))
          throw new Error("Invalid CI commit identity");
        const ancestor = spawnSync("git", [
          "-C",
          directory,
          "merge-base",
          "--is-ancestor",
          run.head_sha,
          target,
        ]);
        // Old rewritten histories may no longer be available; they cannot be used as a baseline.
        if (ancestor.status === 0) {
          base = run.head_sha;
          break;
        }
        if (ancestor.error) throw ancestor.error;
      }
      if (data.workflow_runs.length < 100) break;
    }
  } else {
    throw new Error(`Unsupported CI event: ${eventName}`);
  }
  if (!base)
    return {
      changed: true,
      reason: "No successful ancestor run; full validation required",
    };
  const files = codeChanges(directory, base, compared);
  return {
    changed: files.length > 0,
    base,
    target: compared,
    files,
    reason: files.length
      ? "Non-document changes require validation"
      : "Code is unchanged; skip expensive jobs",
  };
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  try {
    const plan = await planCI({
      directory: process.cwd(),
      repository: process.env.GITHUB_REPOSITORY,
      workflow: process.env.CI_WORKFLOW,
      branch: process.env.CI_BRANCH,
      eventName: process.env.GITHUB_EVENT_NAME,
      event: JSON.parse(readFileSync(process.env.GITHUB_EVENT_PATH, "utf8")),
    });
    console.log(JSON.stringify(plan, null, 2));
    if (process.env.GITHUB_OUTPUT)
      appendFileSync(process.env.GITHUB_OUTPUT, `changed=${plan.changed}\n`);
    if (process.env.GITHUB_STEP_SUMMARY)
      appendFileSync(
        process.env.GITHUB_STEP_SUMMARY,
        `### CI changes\n\n${plan.reason}\n`,
      );
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
