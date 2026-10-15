import { expect, test } from "vitest";
import { chromium, firefox } from "playwright";
import {
  buildBrowserScenario,
  createBrowserPage,
  openBrowserPage,
  readBrowserScenario,
} from "./support/browser.js";
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
          "/": '<!doctype html><script type="module" src="/scenario.js"></script>',
          "/scenario.js": new URL("./support/runtime-browser.js", import.meta.url),
          "/initial": initial.bytes,
          "/replacement": replacement.bytes,
        },
        browserType,
      );
      page.on("console", (message) => console.debug(`${browserName}: ${message.text()}`));
      page.on("pageerror", (error) => console.debug(`${browserName}: ${error.stack ?? error}`));
      const observed = await readBrowserScenario(page);
      expect(observed.before).toBe(initial.result_integer);
      expect(observed.after).toBe(replacement.result_integer);
      expect(observed.generation).toBe("2");
      expect(BigInt(observed.memory)).toBeGreaterThanOrEqual(0n);
    },
  );

  test(
    `${browserName}: VM and generated RPC interoperate with Go`,
    { timeout: 90_000 },
    async (t) => {
      const address = await startPeer(t);
      const routes = await buildBrowserScenario(
        new URL("./support/rpc-browser.js", import.meta.url),
      );
      const page = await openBrowserPage(
        t,
        `${address}/playground/runtime-typescript/tests/support/index.html`,
        browserType,
      );
      await page.addScriptTag({ content: routes["/scenario.js"], type: "module" });
      await readBrowserScenario(page);
    },
  );
}
