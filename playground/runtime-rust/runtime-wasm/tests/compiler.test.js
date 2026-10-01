import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { gunzipSync } from "node:zlib";
import { MiniGo, values } from "@d7z-team/mini-go";
import { exerciseCompiler, exerciseCompiledRPC } from "./compiler_scenario.js";
import { createBrowserPage, openBrowserPage } from "./browser_helpers.js";
import { startPeer } from "./test_helpers.js";

const compilerImage = new URL("../dist/tools/compiler.json.gz", import.meta.url);

test("compiler response preserves exact image bytes when loading the compiled program", async () => {
  const image = String.raw`{"constants":[9007199254740993,1e+09,-0,"\u2028","brace } and quote \""],"nested":{"2":true,"1":null}}`;
  let creations = 0;
  const api = {
    async create(bytes) {
      if (++creations === 1) {
        let calls = 0;
        return {
          start() {
            const response =
              ++calls === 1
                ? '{"Format":"test","Version":11,"Error":"handshake"}'
                : `{"Image":${image},"Error":""}`;
            return {
              result: Promise.resolve({
                roots: [{ data: { String: new TextEncoder().encode(response) } }],
              }),
              settled: Promise.resolve(),
            };
          },
          async close() {},
          terminate() {},
        };
      }
      assert.equal(new TextDecoder().decode(bytes), image);
      return {
        start() {
          return {
            result: Promise.resolve({ roots: [{ data: { Integer: 42n } }] }),
            settled: Promise.resolve(),
          };
        },
        async stats() {
          return { activeScopes: 0n, tasks: 0n, ffiCalls: 0n };
        },
        async close() {},
        terminate() {},
      };
    },
  };
  await exerciseCompiledRPC(api, values, new Uint8Array(), "source", "http://localhost");
  assert.equal(creations, 2);
});

test(
  "compiler workload: Node default-budget RPC compilation and execution",
  { timeout: 60_000 },
  async (t) => {
    const address = await startPeer(t);
    const workloads = JSON.parse(
      await readFile(
        new URL("../../../../testdata/language/workloads.json", import.meta.url),
        "utf8",
      ),
    );
    await exerciseCompiledRPC(
      MiniGo,
      values,
      await readFile(compilerImage),
      workloads.find((w) => w.Name === "rpc").Source,
      address,
    );
  },
);

test(
  "compiler workload: browser default-budget RPC compilation and execution",
  { timeout: 60_000 },
  async (t) => {
    const address = await startPeer(t);
    const page = await openBrowserPage(
      t,
      `${address}/playground/runtime-rust/runtime-wasm/tests/index.html`,
    );
    await page.evaluate(async () => {
      const { MiniGo, values } = await import(
        "/playground/runtime-rust/runtime-wasm/dist/browser.js"
      );
      const { exerciseCompiledRPC } = await import(
        "/playground/runtime-rust/runtime-wasm/tests/compiler_scenario.js"
      );
      const workloads = await (await fetch("/testdata/language/workloads.json")).json();
      const image = new Uint8Array(
        await (
          await fetch("/playground/runtime-rust/runtime-wasm/dist/tools/compiler.json.gz")
        ).arrayBuffer(),
      );
      await exerciseCompiledRPC(
        MiniGo,
        values,
        image,
        workloads.find((w) => w.Name === "rpc").Source,
        location.origin,
      );
    });
  },
);

test(
  "compiler workload: Node gzip image initialization and repeated source checks",
  { timeout: 60_000 },
  async () => {
    await exerciseCompiler(MiniGo, values, await readFile(compilerImage));
  },
);

test(
  "compiler workload: browser image initialization and repeated source checks",
  { timeout: 60_000 },
  async (t) => {
    const page = await createBrowserPage(t, {
      "/scenario.js": new URL("./compiler_scenario.js", import.meta.url),
      "/image": gunzipSync(await readFile(compilerImage)),
    });
    await page.evaluate(async () => {
      const { MiniGo, values } = await import("/browser.js");
      const { exerciseCompiler } = await import("/scenario.js");
      await exerciseCompiler(
        MiniGo,
        values,
        new Uint8Array(await (await fetch("/image")).arrayBuffer()),
      );
    });
  },
);
