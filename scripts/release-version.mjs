#!/usr/bin/env node

import { execFileSync } from "node:child_process";
import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
import process from "node:process";

export function releaseVersion(repository = process.cwd()) {
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

  return {
    version: `0.0.${count}-git.g${shortCommit}`,
    commit,
    commitCount: count,
    shortCommit,
  };
}

export async function setReleaseVersion(version, repository = process.cwd()) {
  if (!/^0\.0\.\d+-git\.g[0-9a-f]{7}(?:\.dirty)?$/.test(version ?? "")) {
    throw new Error("expected release version 0.0.<count>-git.g<sha7>");
  }

  const rust = path.join(repository, "playground/runtime-rust");
  const manifestPath = path.join(rust, "Cargo.toml");
  let manifest = await readFile(manifestPath, "utf8");
  const workspaceVersion = 'version = "0.0.0-dev"';
  if (manifest.split(workspaceVersion).length !== 2) {
    throw new Error("Cargo workspace development version is not canonical");
  }
  manifest = manifest.replace(workspaceVersion, `version = "${version}"`);
  const dependencyVersion = 'version = "=0.0.0-dev"';
  if (manifest.split(dependencyVersion).length !== 2) {
    throw new Error("Cargo workspace dependency versions are not canonical");
  }
  manifest = manifest.replaceAll(dependencyVersion, `version = "=${version}"`);
  await writeFile(manifestPath, manifest);

  const lockPath = path.join(rust, "Cargo.lock");
  let lock = await readFile(lockPath, "utf8");
  for (const name of [
    "mini-go",
    "mini-go-rpc-peer-rust",
    "mini-go-tools",
    "mini-go-wasm",
  ]) {
    const expression = new RegExp(
      `(name = "${name}"\\nversion = ")[^"]+("\\n)`,
    );
    if (!expression.test(lock))
      throw new Error(`Cargo.lock is missing workspace package ${name}`);
    lock = lock.replace(expression, `$1${version}$2`);
  }
  await writeFile(lockPath, lock);

  const npmDirectory = path.join(rust, "runtime-wasm");
  const packagePath = path.join(npmDirectory, "package.json");
  const packageJSON = JSON.parse(await readFile(packagePath, "utf8"));
  if (packageJSON.version !== "0.0.0-dev") {
    throw new Error("npm package development version is not canonical");
  }
  packageJSON.version = version;
  await writeFile(packagePath, `${JSON.stringify(packageJSON, null, 2)}\n`);

  const packageLockPath = path.join(npmDirectory, "package-lock.json");
  const packageLock = JSON.parse(await readFile(packageLockPath, "utf8"));
  if (
    packageLock.version !== "0.0.0-dev" ||
    packageLock.packages?.[""]?.version !== "0.0.0-dev"
  ) {
    throw new Error("npm lockfile development version is not canonical");
  }
  packageLock.version = version;
  packageLock.packages[""].version = version;
  await writeFile(packageLockPath, `${JSON.stringify(packageLock, null, 2)}\n`);
}

if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  let repository = process.cwd();
  let json = false;
  let version;
  for (let index = 2; index < process.argv.length; index++) {
    const argument = process.argv[index];
    if (argument === "--json") json = true;
    else if (argument === "--repository" && process.argv[index + 1])
      repository = process.argv[++index];
    else if (argument === "--set" && process.argv[index + 1])
      version = process.argv[++index];
    else
      throw new Error(
        "usage: release-version.mjs [--json | --set VERSION] [--repository PATH]",
      );
  }
  if (version !== undefined) {
    if (json) throw new Error("--json and --set cannot be combined");
    await setReleaseVersion(version, repository);
  } else {
    const release = releaseVersion(repository);
    process.stdout.write(
      json ? `${JSON.stringify(release)}\n` : `${release.version}\n`,
    );
  }
}
