#!/usr/bin/env node

import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";

const [version, repository = process.cwd()] = process.argv.slice(2);
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
  const expression = new RegExp(`(name = "${name}"\\nversion = ")[^"]+("\\n)`);
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
