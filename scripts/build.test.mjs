import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  copyFile,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const repository = fileURLToPath(new URL("../", import.meta.url));

async function fixture(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "mini-go-build-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const bin = path.join(directory, "tools");
  await mkdir(bin);
  const log = path.join(directory, "commands.jsonl");
  await writeFile(log, "");
  return {
    directory,
    bin,
    env: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      COMMAND_LOG: log,
    },
    async commands() {
      return (await readFile(log, "utf8"))
        .trim()
        .split("\n")
        .filter(Boolean)
        .map(JSON.parse);
    },
  };
}

async function tool(context, name, body) {
  await writeFile(
    path.join(context.bin, name),
    `#!${process.execPath}
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(process.env.COMMAND_LOG, JSON.stringify({tool: ${JSON.stringify(name)}, args,
  peer: process.env.MINIGO_RPC_RUST_PEER}) + '\\n');
${body}
`,
    { mode: 0o755 },
  );
}

test("fuzz discovers packages and functions, applies filtering and propagates failures", async (t) => {
  for (const scenario of [
    "run",
    "list",
    "filtered",
    "empty",
    "list-failure",
    "discovery-failure",
    "run-failure",
  ]) {
    await t.test(scenario, async (t) => {
      const context = await fixture(t);
      context.env.SCENARIO = scenario;
      context.env.FUZZTIME = "2x";
      context.env.FUZZ_PARALLEL = "1";
      context.env.FUZZ_PATTERN = scenario === "filtered" ? "Added" : "^Fuzz";
      await tool(
        context,
        "go",
        `
const scenario = process.env.SCENARIO;
if (args[0] === 'list') {
  if (scenario === 'list-failure') process.exit(7);
  console.log('example/first\\nexample/new-package');
} else if (args.includes('-list')) {
  if (scenario === 'discovery-failure') process.exit(9);
  const name = args[1] === 'example/first' ? 'Fuzz' : 'FuzzAdded';
  if (scenario !== 'empty' && new RegExp(args[args.indexOf('-list') + 1]).test(name)) console.log(name);
  console.log('ok  ' + args[1]);
} else if (args.includes('-fuzz') && scenario === 'run-failure') process.exit(11);
`,
      );
      const result = spawnSync(
        "bash",
        [
          path.join(repository, "scripts/fuzz.sh"),
          ...(scenario === "list" ? ["--list"] : []),
          "./...",
        ],
        {
          cwd: context.directory,
          env: context.env,
          encoding: "utf8",
        },
      );
      const commands = await context.commands();
      const runs = commands.filter(({ args }) => args.includes("-fuzz"));
      if (["run", "list", "filtered"].includes(scenario)) {
        assert.equal(result.status, 0, result.stderr);
        assert.match(result.stdout, /example\/new-package FuzzAdded/);
        const expected =
          scenario === "list"
            ? []
            : scenario === "filtered"
              ? ["^FuzzAdded$"]
              : ["^Fuzz$", "^FuzzAdded$"];
        assert.deepEqual(
          runs.map(({ args }) => args[args.indexOf("-fuzz") + 1]),
          expected,
        );
        for (const { args } of runs) {
          assert.ok(args.includes("-fuzztime=2x"));
          assert.ok(args.includes("-parallel=1"));
        }
      } else {
        assert.equal(
          result.status,
          {
            empty: 1,
            "list-failure": 7,
            "discovery-failure": 9,
            "run-failure": 11,
          }[scenario],
        );
        if (scenario === "empty")
          assert.match(result.stderr, /No fuzz targets/);
        assert.equal(runs.length, scenario === "run-failure" ? 1 : 0);
      }
    });
  }
});

test("Make consumes prepared artifacts and uses Cargo's target directory for peers", async (t) => {
  const context = await fixture(t);
  await copyFile(
    path.join(repository, "Makefile"),
    path.join(context.directory, "Makefile"),
  );
  for (const name of [
    "testdata/runtime/execution.json.gz",
    "testdata/runtime/stdlib.json.gz",
    "playground/runtime-rust/assets/compiler.json.gz",
  ]) {
    const file = path.join(context.directory, name);
    await mkdir(path.dirname(file), { recursive: true });
    await writeFile(file, "prepared fixture");
  }
  context.env.CARGO_TARGET_DIR = path.join(context.directory, "custom target");
  await tool(
    context,
    "cargo",
    `if (args[0] === 'metadata') {
  console.log(JSON.stringify({target_directory: process.env.CARGO_TARGET_DIR}));
  if (process.env.FAIL_METADATA) process.exit(23);
}`,
  );
  await tool(context, "go", "");
  const result = spawnSync("make", ["test-interop"], {
    cwd: context.directory,
    env: context.env,
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  const commands = await context.commands();
  const identity = commands.find(({ args }) =>
    args.includes("compiler-identity"),
  );
  assert.ok(identity.args.includes("-check"));
  const call = commands.find(
    ({ tool, args }) => tool === "go" && args[0] === "test",
  );
  assert.equal(
    call.peer,
    path.join(context.env.CARGO_TARGET_DIR, "debug/mini-go-rpc-peer-rust"),
  );

  context.env.FAIL_METADATA = "1";
  const failed = spawnSync("make", ["test-interop"], {
    cwd: context.directory,
    env: context.env,
    encoding: "utf8",
  });
  assert.notEqual(failed.status, 0);
  assert.equal(
    (await context.commands())
      .slice(commands.length)
      .some(({ tool, args }) => tool === "go" && args[0] === "test"),
    false,
  );
  delete context.env.FAIL_METADATA;

  await rm(path.join(context.directory, "testdata/runtime/stdlib.json.gz"));
  const missing = spawnSync("make", ["test"], {
    cwd: context.directory,
    env: context.env,
    encoding: "utf8",
  });
  assert.notEqual(missing.status, 0);
  assert.match(
    missing.stderr,
    /Missing testdata\/runtime\/stdlib.json.gz; run make artifacts/,
  );
});

test("Make packs the completed SDK build and preserves shared caches during clean", async (t) => {
  const context = await fixture(t);
  await copyFile(
    path.join(repository, "Makefile"),
    path.join(context.directory, "Makefile"),
  );
  for (const name of [
    "testdata/runtime/execution.json.gz",
    "testdata/runtime/stdlib.json.gz",
    "playground/runtime-rust/assets/compiler.json.gz",
    "build/result",
    ".cache/retained",
  ]) {
    const file = path.join(context.directory, name);
    await mkdir(path.dirname(file), { recursive: true });
    await writeFile(file, "fixture");
  }
  await mkdir(
    path.join(context.directory, "playground/runtime-rust/runtime-wasm"),
  );
  await tool(context, "go", "");
  await tool(context, "npm", "");
  const result = spawnSync("make", ["pack-wasm", "clean"], {
    cwd: context.directory,
    env: context.env,
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  const commands = (await context.commands()).filter(
    ({ tool }) => tool === "npm",
  );
  assert.deepEqual(
    commands.map(({ args }) => args),
    [
      ["--prefix", "playground/runtime-rust/runtime-wasm", "run", "build"],
      [
        "pack",
        "--ignore-scripts",
        "--pack-destination",
        path.join(context.directory, "build/npm"),
      ],
    ],
  );
  assert.equal(
    await readFile(path.join(context.directory, ".cache/retained"), "utf8"),
    "fixture",
  );
  await assert.rejects(readFile(path.join(context.directory, "build/result")), {
    code: "ENOENT",
  });
});
