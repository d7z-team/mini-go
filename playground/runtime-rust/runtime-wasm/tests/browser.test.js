import test from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { gunzipSync, gzipSync } from "node:zlib";
import { chromium, firefox, webkit } from "playwright";
import { startPeer } from "./test_helpers.js";

const root = path.resolve(fileURLToPath(new URL("../../../../", import.meta.url)));
const fixtureRoot = process.env.MINIGO_WASM_FIXTURES;
if (!fixtureRoot)
  throw new Error("MINIGO_WASM_FIXTURES must point to the native wasm_driver test output");
const vectors = JSON.parse(
  gunzipSync(await readFile(path.join(root, "testdata/runtime/execution.json.gz"))),
);
const artifact = await readFile(
  path.join(root, "playground/runtime-rust/runtime-wasm/dist/wasm/mini_go_wasm_bg.wasm"),
);
console.log("WASM artifact bytes", artifact.length, "gzip bytes", gzipSync(artifact).length);

for (const browserName of (process.env.MINIGO_BROWSERS ?? "chromium,firefox").split(",")) {
  test(
    `${browserName}: worker VM, async initialization, cancellation and cleanup`,
    { timeout: 90_000 },
    async (t) => {
      const server = http.createServer(async (request, response) => {
        try {
          const url = new URL(request.url, "http://localhost");
          if (url.pathname === "/") {
            response.setHeader("Content-Type", "text/html");
            response.end("<!doctype html><title>Mini-Go browser test</title>");
            return;
          }
          if (url.pathname.startsWith("/fixture/")) {
            response.setHeader("Content-Type", "application/json");
            response.end(await readFile(path.join(fixtureRoot, path.basename(url.pathname))));
            return;
          }
          if (url.pathname === "/vector") {
            const optimization = Number(url.searchParams.get("optimization"));
            const vector = vectors.find(
              (v) => v.name === url.searchParams.get("name") && v.optimization === optimization,
            );
            assert.ok(vector, "shared vector exists");
            // Serve the original JSON subtree without JS Number conversion.
            const source = gunzipSync(
              await readFile(path.join(root, "testdata/runtime/execution.json.gz")),
            ).toString();
            response.end(extractImage(source, vector.name, vector.optimization));
            return;
          }
          const filename = path.resolve(root, "." + decodeURIComponent(url.pathname));
          if (!filename.startsWith(root + path.sep)) throw new Error("invalid path");
          response.setHeader(
            "Content-Type",
            filename.endsWith(".wasm") ? "application/wasm" : "text/javascript",
          );
          response.end(await readFile(filename));
        } catch (error) {
          response.statusCode = 404;
          response.end(String(error));
        }
      });
      await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
      let browser;
      t.signal.addEventListener("abort", () => {
        void browser?.close();
        server.closeAllConnections();
        server.close();
      });
      try {
        browser = await { chromium, firefox, webkit }[browserName].launch({ headless: true });
        const page = await browser.newPage();
        page.on("console", (message) => console.log(browserName, message.text()));
        await page.goto(`http://127.0.0.1:${server.address().port}`);
        const metrics = await page.evaluate(
          async (expectations) => {
            const { MiniGo, values } = await import(
              "/playground/runtime-rust/runtime-wasm/dist/browser.js"
            );
            const load = async (name) =>
              new Uint8Array(await (await fetch(`/fixture/${name}.json`)).arrayBuffer());
            const check = (value, message) => {
              if (!value) throw new Error(message);
            };
            const metrics = { scenarios: [], cancelMs: 0 };
            for (const name of ["answer", "init", "host", "timer", "background"]) {
              const before = performance.now();
              const vm = await MiniGo.create(await load(name), {
                providerModule: `${location.origin}/playground/runtime-rust/runtime-wasm/tests/provider.js`,
              });
              const initialized = performance.now();
              const execution = vm.start("default");
              const result = await execution.result;
              await execution.settled;
              metrics.scenarios.push({
                name,
                createMs: initialized - before,
                executeMs: performance.now() - initialized,
                wasmBytes: (await vm.stats()).wasmBytes,
              });
              if (name !== "host") check(result.roots[0].data.Integer === 42n, "64-bit result");
              await vm.close();
            }
            const vm = await MiniGo.create(await load("loop"));
            const execution = vm.start("default");
            await new Promise((resolve) => setTimeout(resolve, 30));
            await vm.pause();
            const frames = await vm.stack();
            check(frames.length > 0, "paused stack available");
            await vm.bindings(frames[0].reference);
            await vm.resume();
            check(
              await vm.bindings(frames[0].reference).then(
                () => false,
                () => true,
              ),
              "stale debug frame rejected",
            );
            const canceled = performance.now();
            execution.cancel();
            check(
              await execution.result.then(
                () => false,
                () => true,
              ),
              "running execution cancellation",
            );
            await execution.settled.catch(() => {});
            metrics.cancelMs = performance.now() - canceled;
            await vm.close();
            const host = await MiniGo.create(await load("host"), {
              provider: async () => {
                await new Promise((resolve) => setTimeout(resolve, 25));
                return new Uint8Array();
              },
            });
            const pending = host.start("default");
            pending.cancel();
            await pending.result.catch(() => {});
            await host.close();
            const late = await MiniGo.create(await load("host"), {
              providerModule: `${location.origin}/playground/runtime-rust/runtime-wasm/tests/provider.js`,
            });
            const lateCall = late.start("default");
            await new Promise((resolve) => setTimeout(resolve, 5));
            lateCall.cancel();
            await late.close();
            await lateCall.result.catch(() => {});
            const initAbort = new AbortController();
            const initializing = MiniGo.create(await load("init"), {
              signal: initAbort.signal,
              providerModule: `${location.origin}/playground/runtime-rust/runtime-wasm/tests/provider.js`,
            });
            initAbort.abort();
            check(
              await initializing.then(
                () => false,
                () => true,
              ),
              "initialization cancellation rejects creation",
            );
            for (const [expected, provider] of [
              [0n, () => new Uint8Array([42])],
              [
                2n,
                () => {
                  throw new Error("synchronous host failure");
                },
              ],
              [
                2n,
                async () => {
                  throw new Error("asynchronous host failure");
                },
              ],
            ]) {
              const host = await MiniGo.create(await load("host-result"), { provider });
              const call = host.start("default");
              const result = await call.result;
              await call.settled;
              const message = new TextDecoder().decode(result.roots[1].data.String);
              check(result.roots[2].data.Integer === expected, "host errors preserve FFI status");
              if (expected === 2n)
                check(message.includes("host failure"), "host error message delivered");
              await host.close();
            }
            const releaseChannel = new BroadcastChannel("browser-ownership-budget");
            const bounded = await MiniGo.create(await load("host-result"), {
              maxPendingCalls: 1,
              providerModule: `${location.origin}/playground/runtime-rust/runtime-wasm/tests/provider.js?releaseChannel=browser-ownership-budget`,
            });
            const accepted = bounded.start("default");
            check((await accepted.result).roots[2].data.Integer === 0n, "first host call succeeds");
            await accepted.settled;
            const overCapacity = bounded.start("default");
            check(
              (await overCapacity.result).roots[2].data.Integer === 2n,
              "pending resource decision retains capacity",
            );
            await overCapacity.settled;
            releaseChannel.postMessage("release");
            await bounded.close();
            releaseChannel.close();
            let entered, release;
            const enteredHost = new Promise((resolve) => {
              entered = resolve;
            });
            const retainedHost = new Promise((resolve) => {
              release = resolve;
            });
            const closing = await MiniGo.create(await load("host"), {
              provider: async () => {
                entered();
                await retainedHost;
                return new Uint8Array();
              },
            });
            const closingCall = closing.start("default");
            await enteredHost;
            const waitingCall = closing.start("default");
            waitingCall.cancel();
            check(
              await waitingCall.result.then(
                () => false,
                () => true,
              ),
              "queued call cancels independently",
            );
            const stopWaiting = new AbortController();
            stopWaiting.abort();
            check(
              await closing.close({ signal: stopWaiting.signal }).then(
                () => false,
                () => true,
              ),
              "close wait can be canceled",
            );
            release();
            await closing.close();
            await closingCall.result.catch(() => {});
            const terminated = await MiniGo.create(await load("loop"));
            const abandoned = terminated.start("default");
            terminated.terminate();
            check(
              await abandoned.result.then(
                () => false,
                () => true,
              ),
              "termination rejects result",
            );
            check(
              await abandoned.settled.then(
                () => false,
                () => true,
              ),
              "termination rejects unsettled scope",
            );
            for (const name of ["echo", "string"]) {
              const vm = await MiniGo.create(await load(name));
              if (name === "echo") {
                let rejected = false;
                try {
                  vm.start("default", [() => {}]);
                } catch {
                  rejected = true;
                }
                check(rejected, "uncloneable input rejected without retaining a call");
              }
              const input =
                name === "echo"
                  ? values.int(9223372036854775807n)
                  : values.string("中".repeat(100_000));
              const call = vm.start("default", [input]);
              const snapshot = await call.result;
              await call.settled;
              if (name === "echo")
                check(
                  snapshot.roots[0].data.Integer === 9223372036854775807n,
                  "large integer fidelity",
                );
              else
                check(
                  snapshot.roots[0].data.String.length === 300_000,
                  "owned string snapshot across allocation",
                );
              await vm.close();
            }
            const patched = await MiniGo.create(await load("answer"));
            check(
              await patched.patch(new Uint8Array([1, 2, 3])).then(
                () => false,
                () => true,
              ),
              "invalid patch fails",
            );
            check((await patched.stats()).generation === 1n, "failed patch preserves revision");
            await patched.patch(await load("answer-patch"));
            check((await patched.stats()).generation === 2n, "patch revision published");
            const updated = patched.start("default");
            check((await updated.result).roots[0].data.Integer === 43n, "new revision executes");
            await updated.settled;
            await patched.close();
            for (const name of [
              "arithmetic",
              "closure",
              "channel_buffer",
              "channel_rendezvous",
              "select_wait",
              "recover_panic",
              "semantic_boundaries",
            ]) {
              for (const optimization of [0, 1, 2]) {
                const image = new Uint8Array(
                  await (
                    await fetch(`/vector?name=${name}&optimization=${optimization}`)
                  ).arrayBuffer(),
                );
                const shared = await MiniGo.create(image);
                const call = shared.start("default");
                check(
                  (await call.result).roots[0].data.Integer === BigInt(expectations[name]),
                  `${name} O${optimization}: shared corpus result`,
                );
                await call.settled;
                await shared.close();
              }
            }
            return metrics;
          },
          Object.fromEntries(
            vectors
              .filter((vector) => vector.optimization === 2)
              .map((vector) => [vector.name, vector.result_integer]),
          ),
        );
        console.log(browserName, "observations", JSON.stringify(metrics));
      } finally {
        await browser?.close();
        await new Promise((resolve) => server.close(resolve));
      }
    },
  );

  test(
    `${browserName}: Go RPC values, resources and reverse guest calls`,
    { timeout: 90_000 },
    async (t) => {
      const address = await startPeer(t);
      let browser;
      t.signal.addEventListener("abort", () => {
        void browser?.close();
      });
      try {
        browser = await { chromium, firefox, webkit }[browserName].launch({ headless: true });
        const page = await browser.newPage();
        page.on("console", (message) => console.log(browserName, message.text()));
        await page.goto(`${address}/playground/runtime-rust/runtime-wasm/tests/index.html`);
        await page.evaluate(async () => {
          const { MiniGo } = await import("/playground/runtime-rust/runtime-wasm/dist/browser.js");
          const { exerciseRPC } = await import(
            "/playground/runtime-rust/runtime-wasm/tests/rpc_scenario.js"
          );
          const load = async (name) =>
            new Uint8Array(
              await (await fetch(`/testdata/rpc/images/${name}.json.gz`)).arrayBuffer(),
            );
          await exerciseRPC(MiniGo, load, location.origin);
        });
      } finally {
        await browser?.close();
      }
    },
  );

  test(
    `${browserName}: generated TypeScript RPC interoperates with Go in both directions`,
    { timeout: 90_000 },
    async (t) => {
      const address = await startPeer(t);
      let browser;
      t.signal.addEventListener("abort", () => {
        void browser?.close();
      });
      try {
        browser = await { chromium, firefox, webkit }[browserName].launch({ headless: true });
        const page = await browser.newPage();
        page.on("console", (message) => console.log(browserName, message.text()));
        await page.goto(`${address}/playground/runtime-rust/runtime-wasm/tests/index.html`);
        await page.evaluate(async () => {
          const api = await import("/playground/runtime-rust/runtime-wasm/dist/browser-rpc.js");
          const { exerciseNativeRPC } = await import(
            "/playground/runtime-rust/runtime-wasm/tests/native_rpc_scenario.js"
          );
          await exerciseNativeRPC(api, location.origin);
        });
      } finally {
        await browser?.close();
      }
    },
  );
}

// Preserve raw integer tokens while selecting one image from the shared corpus.
function extractImage(source, name, optimization) {
  let depth = 0,
    quoted = false,
    escaped = false,
    start = -1;
  for (let i = 0; i < source.length; i++) {
    const char = source[i];
    if (quoted) {
      if (escaped) escaped = false;
      else if (char === "\\") escaped = true;
      else if (char === '"') quoted = false;
      continue;
    }
    if (char === '"') {
      quoted = true;
      continue;
    }
    if (char === "{") {
      if (depth === 0) start = i;
      depth++;
    }
    if (char === "}" && --depth === 0 && start >= 0) {
      const item = source.slice(start, i + 1),
        parsed = JSON.parse(item);
      if (parsed.name !== name || parsed.optimization !== optimization) continue;
      const imageStart = item.indexOf('"image":') + 8;
      let level = 0,
        string = false,
        escape = false;
      for (let j = imageStart; j < item.length; j++) {
        const c = item[j];
        if (string) {
          if (escape) escape = false;
          else if (c === "\\") escape = true;
          else if (c === '"') string = false;
          continue;
        }
        if (c === '"') string = true;
        else if (c === "{") level++;
        else if (c === "}" && --level === 0) return item.slice(imageStart, j + 1);
      }
    }
  }
  throw new Error("shared image not found");
}
