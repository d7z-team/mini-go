import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { cp, mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const scripts = fileURLToPath(new URL("./", import.meta.url));

function run(command, parameters, cwd) {
  return execFileSync(command, parameters, {
    cwd,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: "Release Test",
      GIT_AUTHOR_EMAIL: "release@example.invalid",
      GIT_COMMITTER_NAME: "Release Test",
      GIT_COMMITTER_EMAIL: "release@example.invalid",
    },
  }).trim();
}

test("release version uses the complete commit count and seven-character identity", async () => {
  const repository = await mkdtemp(
    path.join(tmpdir(), "mini-go-release-version-"),
  );
  run("git", ["init", "-q"], repository);
  for (const value of ["first", "second"]) {
    await writeFile(path.join(repository, "value"), value);
    run("git", ["add", "value"], repository);
    run("git", ["commit", "-qm", value], repository);
  }
  const result = JSON.parse(
    run(
      process.execPath,
      [path.join(scripts, "release-version.mjs"), "--json"],
      repository,
    ),
  );
  assert.equal(result.commitCount, 2);
  assert.equal(result.shortCommit, result.commit.slice(0, 7));
  assert.equal(result.version, `0.0.2-git.g${result.shortCommit}`);

  const shallow = await mkdtemp(
    path.join(tmpdir(), "mini-go-release-shallow-"),
  );
  run(
    "git",
    ["clone", "-q", "--depth=1", `file://${repository}`, shallow],
    repository,
  );
  assert.throws(() =>
    run(process.execPath, [path.join(scripts, "release-version.mjs")], shallow),
  );
});

test("release manifest rewrite keeps Cargo and npm versions aligned", async () => {
  const repository = await mkdtemp(
    path.join(tmpdir(), "mini-go-release-manifest-"),
  );
  const source = path.resolve(scripts, "../playground/runtime-rust");
  const destination = path.join(repository, "playground/runtime-rust");
  await mkdir(path.join(destination, "runtime-wasm"), { recursive: true });
  for (const file of ["Cargo.toml", "Cargo.lock"]) {
    await cp(path.join(source, file), path.join(destination, file));
  }
  for (const file of ["package.json", "package-lock.json"]) {
    await cp(
      path.join(source, "runtime-wasm", file),
      path.join(destination, "runtime-wasm", file),
    );
  }
  const version = "0.0.42-git.g123abcd";
  run(
    process.execPath,
    [path.join(scripts, "set-release-version.mjs"), version, repository],
    repository,
  );
  const manifest = await readFile(
    path.join(repository, "playground/runtime-rust/Cargo.toml"),
    "utf8",
  );
  const lock = await readFile(
    path.join(repository, "playground/runtime-rust/Cargo.lock"),
    "utf8",
  );
  const packageJSON = JSON.parse(
    await readFile(
      path.join(
        repository,
        "playground/runtime-rust/runtime-wasm/package.json",
      ),
      "utf8",
    ),
  );
  const packageLock = JSON.parse(
    await readFile(
      path.join(
        repository,
        "playground/runtime-rust/runtime-wasm/package-lock.json",
      ),
      "utf8",
    ),
  );
  assert.match(
    manifest,
    new RegExp(`version = "${version.replaceAll(".", "\\.")}"`),
  );
  assert.equal(
    (
      lock.match(
        new RegExp(`version = "${version.replaceAll(".", "\\.")}"`, "g"),
      ) ?? []
    ).length,
    5,
  );
  assert.equal(packageJSON.version, version);
  assert.equal(packageLock.version, version);
  assert.equal(packageLock.packages[""].version, version);
});
