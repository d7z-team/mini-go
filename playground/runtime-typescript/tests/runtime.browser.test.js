import test from "node:test";
import { chromium, firefox } from "playwright";
import { createBrowserPage, openBrowserPage } from "./support/browser.js";
import { runtimeVector } from "./support/runtime-vectors.js";
import { startPeer } from "./support/peer.js";

for (const browserName of (process.env.MINIGO_BROWSERS ?? "chromium,firefox").split(",")) {
  const browserType = { chromium, firefox }[browserName];
  if (!browserType) throw new Error(`unsupported test browser ${browserName}`);

  test(
    `${browserName}: TypeScript worker executes, patches and closes`,
    { timeout: 90_000 },
    async (t) => {
      const initial = runtimeVector("any_reassign");
      const replacement = runtimeVector("arithmetic");
      const page = await createBrowserPage(
        t,
        {
          "/initial": initial.bytes,
          "/replacement": replacement.bytes,
        },
        browserType,
      );
      page.on("console", (message) => t.diagnostic(`${browserName}: ${message.text()}`));
      page.on("pageerror", (error) => t.diagnostic(`${browserName}: ${error.stack ?? error}`));
      const observed = await page.evaluate(async () => {
        const { createRuntime } = await import("/index.browser.js");
        const load = async (path) => new Uint8Array(await (await fetch(path)).arrayBuffer());
        const vm = await createRuntime(await load("/initial"));
        try {
          const first = vm.start("default");
          const before = (await first.result).roots[0].data.Integer;
          await first.settled;
          await vm.patch(await load("/replacement"));
          const second = vm.start("default");
          const after = (await second.result).roots[0].data.Integer;
          await second.settled;
          const stats = await vm.stats();
          return {
            before: before.toString(),
            after: after.toString(),
            generation: stats.generation.toString(),
            memory: stats.memoryAllocatedBytes.toString(),
          };
        } finally {
          await vm.close();
        }
      });
      t.assert.equal(observed.before, initial.result_integer);
      t.assert.equal(observed.after, replacement.result_integer);
      t.assert.equal(observed.generation, "2");
      t.assert.ok(BigInt(observed.memory) >= 0n);
    },
  );

  test(
    `${browserName}: VM and generated RPC interoperate with Go`,
    { timeout: 90_000 },
    async (t) => {
      const address = await startPeer(t);
      const page = await openBrowserPage(
        t,
        `${address}/playground/runtime-typescript/tests/support/index.html`,
        browserType,
      );
      await page.evaluate(async () => {
        const { createRuntime } = await import(
          "/playground/runtime-typescript/dist/index.browser.js"
        );
        const { exerciseRPC } = await import("/playground/runtime-typescript/tests/support/rpc.js");
        const nativeRPC = await import("/playground/runtime-typescript/dist/rpc.browser.js");
        const { exerciseNativeRPC } = await import(
          "/playground/runtime-typescript/tests/support/native-rpc.js"
        );
        const load = async (name) =>
          new Uint8Array(await (await fetch(`/testdata/rpc/images/${name}.json.gz`)).arrayBuffer());
        await exerciseRPC(createRuntime, load, location.origin);
        await exerciseNativeRPC(nativeRPC, location.origin);
      });
    },
  );
}
