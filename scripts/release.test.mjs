import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
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

test("release scripts report missing option values as usage errors", () => {
  for (const script of ["release-package.sh", "release-verify.sh"]) {
    const result = spawnSync("bash", [path.join(scripts, script), "--output"], {
      encoding: "utf8",
    });
    assert.equal(result.status, 2, `${script}: ${result.stderr}`);
    assert.match(result.stderr, /usage:/);
  }
});

test("release version uses the complete commit count and seven-character identity", async (t) => {
  const repository = await mkdtemp(
    path.join(tmpdir(), "mini-go-release-version-"),
  );
  t.after(() => rm(repository, { recursive: true, force: true }));
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
  t.after(() => rm(shallow, { recursive: true, force: true }));
  run(
    "git",
    ["clone", "-q", "--depth=1", `file://${repository}`, shallow],
    repository,
  );
  assert.throws(() =>
    run(process.execPath, [path.join(scripts, "release-version.mjs")], shallow),
  );
});

test("release manifest rewrite keeps Cargo and npm versions aligned", async (t) => {
  const repository = await mkdtemp(
    path.join(tmpdir(), "mini-go-release-manifest-"),
  );
  t.after(() => rm(repository, { recursive: true, force: true }));
  const source = path.resolve(scripts, "../playground/runtime-rust");
  const sourceTypescript = path.resolve(scripts, "../playground/runtime-typescript");
  const destination = path.join(repository, "playground/runtime-rust");
  const typescript = path.join(repository, "playground/runtime-typescript");
  await mkdir(destination, { recursive: true });
  await mkdir(typescript, { recursive: true });
  for (const file of ["Cargo.toml", "Cargo.lock"]) {
    await cp(path.join(source, file), path.join(destination, file));
  }
  for (const file of ["package.json", "package-lock.json"]) {
    await cp(
      path.join(sourceTypescript, file),
      path.join(typescript, file),
    );
  }
  const version = "0.0.42-git.g123abcd";
  run(
    process.execPath,
    [
      path.join(scripts, "release-version.mjs"),
      "--set",
      version,
      "--repository",
      repository,
    ],
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
        "playground/runtime-typescript/package.json",
      ),
      "utf8",
    ),
  );
  const packageLock = JSON.parse(
    await readFile(
      path.join(
        repository,
        "playground/runtime-typescript/package-lock.json",
      ),
      "utf8",
    ),
  );
  assert.match(
    manifest,
    new RegExp(`version = "${version.replaceAll(".", "\\.")}"`),
  );
  for (const name of [
    "mini-go",
    "mini-go-tools",
    "mini-go-rpc-peer-rust",
  ]) {
    assert.ok(lock.includes(`name = "${name}"\nversion = "${version}"`));
  }
  assert.ok(
    manifest.includes(`mini-go = { path = ".", version = "=${version}" }`),
  );
  assert.equal(packageJSON.version, version);
  assert.equal(packageLock.version, version);
  assert.equal(packageLock.packages[""].version, version);
});

test("release verification accepts the bundled compiler asset and package test entry", async (t) => {
  const root = await mkdtemp(path.join(tmpdir(), "mini-go-release-verify-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const output = path.join(root, "release");
  const sourceTypescript = path.join(
    output,
    "source/playground/runtime-typescript",
  );
  const version = "0.0.42-git.g123abcd";
  const runtimeDirectory = path.join(root, `mini-go-${version}`);
  const npmDirectory = path.join(root, "npm/package");
  const compiler = Buffer.from("compiler wasm fixture");
  const compilerSHA256 = createHash("sha256").update(compiler).digest("hex");
  await mkdir(path.join(output, "source"), { recursive: true });
  await mkdir(path.join(runtimeDirectory, "assets"), { recursive: true });
  await writeFile(
    path.join(runtimeDirectory, "assets/compiler.json.gz"),
    "runtime",
  );
  await writeFile(path.join(runtimeDirectory, "LICENSE-Go"), "go license");
  await mkdir(path.join(npmDirectory, "dist/assets"), { recursive: true });
  await writeFile(
    path.join(npmDirectory, "dist/assets/mini-go-compiler.wasm"),
    compiler,
  );
  await writeFile(
    path.join(npmDirectory, "package.json"),
    JSON.stringify({ name: "@d7z-team/mini-go", version }),
  );
  await writeFile(path.join(npmDirectory, "LICENSE-Go"), "go license");
  await mkdir(sourceTypescript, { recursive: true });
  await writeFile(path.join(sourceTypescript, "package.json"), "{}");

  const crate = path.join(output, `mini-go-${version}.crate`);
  const npmTarball = path.join(output, `d7z-team-mini-go-${version}.tgz`);
  await mkdir(output, { recursive: true });
  run("tar", ["-cf", crate, "-C", root, `mini-go-${version}`], root);
  run(
    "tar",
    ["-czf", npmTarball, "-C", path.join(root, "npm"), "package"],
    root,
  );
  await writeFile(
    path.join(output, "mini-go.files"),
    run("tar", ["-tf", crate], root) + "\n",
  );
  await writeFile(
    path.join(output, "npm.files"),
    run("tar", ["-tzf", npmTarball], root) + "\n",
  );

  const runtimeSHA256 = createHash("sha256")
    .update(
      await readFile(path.join(runtimeDirectory, "assets/compiler.json.gz")),
    )
    .digest("hex");
  const npmIntegrity = `sha512-${createHash("sha512")
    .update(await readFile(npmTarball))
    .digest("base64")}`;
  await writeFile(
    path.join(output, "release.json"),
    JSON.stringify({
      version,
      runtimeCompilerSHA256: runtimeSHA256,
      typescriptCompilerSHA256: compilerSHA256,
      npmIntegrity,
    }),
  );
  await writeFile(
    path.join(output, "SHA256SUMS"),
    run(
      "sha256sum",
      [path.basename(crate), path.basename(npmTarball)],
      output,
    ) + "\n",
  );

  const bin = path.join(root, "bin");
  const log = path.join(root, "npm.log");
  const makeLog = path.join(root, "make.log");
  await mkdir(bin, { recursive: true });
  await writeFile(path.join(bin, "cargo"), "#!/bin/sh\nexit 0\n", {
    mode: 0o755,
  });
  await writeFile(
    path.join(bin, "make"),
    "#!/bin/sh\nprintf '%s|%s\\n' \"$GOFLAGS\" \"$*\" >> \"$MINIGO_TEST_MAKE_LOG\"\nmkdir -p \"$MINIGO_TEST_SOURCE_ROOT/bin\"\nprintf '#!/bin/sh\\nexit 0\\n' > \"$MINIGO_TEST_SOURCE_ROOT/bin/mini-go-rpc-peer-go\"\nchmod +x \"$MINIGO_TEST_SOURCE_ROOT/bin/mini-go-rpc-peer-go\"\n",
    { mode: 0o755 },
  );
  await writeFile(
    path.join(bin, "npm"),
    "#!/bin/sh\nprintf '%s|%s\\n' \"$MINIGO_RPC_GO_PEER\" \"$*\" >> \"$MINIGO_TEST_NPM_LOG\"\nexit 0\n",
    { mode: 0o755 },
  );
  execFileSync(
    "bash",
    [path.join(scripts, "release-verify.sh"), "--output", output],
    {
      cwd: root,
      env: {
        ...process.env,
        MINIGO_TEST_MAKE_LOG: makeLog,
        MINIGO_TEST_SOURCE_ROOT: path.join(output, "source"),
        MINIGO_TEST_NPM_LOG: log,
        PATH: `${bin}:${process.env.PATH}`,
      },
      stdio: "pipe",
    },
  );
  const makeCommands = await readFile(makeLog, "utf8");
  assert.match(makeCommands, /-buildvcs=false\|/);
  assert.match(makeCommands, /-C .*source _rpc-go-peer/);
  const npmCommands = await readFile(log, "utf8");
  assert.match(npmCommands, /\/source\/bin\/mini-go-rpc-peer-go\|.*run test:package/);
  assert.match(npmCommands, /run test:package/);
  assert.match(npmCommands, /publish .*--dry-run/);
});
