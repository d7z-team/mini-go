import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmod, cp, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const scripts = fileURLToPath(new URL("./", import.meta.url));

test("release requires successful push CI for the exact commit", async (t) => {
  const directory = await mkdtemp(path.join(tmpdir(), "mini-go-release-ci-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const gh = path.join(directory, "gh");
  await writeFile(gh, '#!/bin/sh\ncat "$CI_TEST_RESPONSE"\n');
  await chmod(gh, 0o755);
  const response = path.join(directory, "response.json");
  const success = {
    head_sha: "candidate",
    head_branch: "main",
    event: "push",
    status: "completed",
    conclusion: "success",
  };
  for (const [name, run, accepted] of [
    ["success", success, true],
    ["missing", null, false],
    ["other commit", { ...success, head_sha: "other" }, false],
    ["other branch", { ...success, head_branch: "topic" }, false],
    ["pull request", { ...success, event: "pull_request" }, false],
    ["running", { ...success, status: "in_progress" }, false],
    ["failed", { ...success, conclusion: "failure" }, false],
  ]) {
    await writeFile(response, JSON.stringify({ workflow_runs: run ? [run] : [] }));
    const invoke = () => execFileSync("bash", [path.join(scripts, "release-check-ci.sh")], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        PATH: `${directory}:${process.env.PATH}`,
        CI_TEST_RESPONSE: response,
        GITHUB_REPOSITORY: "d7z-team/mini-go",
        GITHUB_SHA: "candidate",
      },
    });
    if (accepted) {
      const output = invoke();
      assert.match(output, /go-test.yml passed/);
      assert.match(output, /runtime-rust.yml passed/);
    } else {
      assert.throws(invoke, (error) => error.status === 1 && error.stdout.includes("::error::"), name);
    }
  }
});

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
  for (const name of ["mini-go", "mini-go-tools", "mini-go-rpc-peer-rust", "mini-go-wasm"]) {
    assert.ok(lock.includes(`name = "${name}"\nversion = "${version}"`));
  }
  assert.ok(manifest.includes(`mini-go = { path = ".", version = "=${version}" }`));
  assert.equal(packageJSON.version, version);
  assert.equal(packageLock.version, version);
  assert.equal(packageLock.packages[""].version, version);
});
