import { test } from "vitest";
import { readFile } from "node:fs/promises";
import * as tools from "@d7z-team/mini-go/tools";
import { exerciseTools } from "./support/language-service.js";
import { createBrowserPage, readBrowserScenario } from "./support/browser.js";
import { exerciseCompilerWorkload } from "./support/compiler-workloads.js";

const workloads = JSON.parse(
  await readFile(new URL("../../../testdata/language/workloads.json", import.meta.url), "utf8"),
);

for (const workload of workloads) {
  test(
    `Node compiler workload ${workload.Name} preserves warm snapshots`,
    { timeout: 600_000 },
    async (t) => {
      await exerciseCompilerWorkload(tools, workload, t.signal);
    },
  );
  test(
    `browser compiler workload ${workload.Name} preserves warm snapshots`,
    { timeout: 600_000 },
    async (t) => {
      const page = await createBrowserPage(t, {
        "/": '<!doctype html><script type="module" src="/scenario.js"></script>',
        "/scenario.js": new URL("./support/compiler-browser.js", import.meta.url),
        "/workloads.js": new URL("./support/compiler-workloads.js", import.meta.url),
        "/workload.json": JSON.stringify(workload),
      });
      const result = await readBrowserScenario(page);
      return result;
    },
  );
}

const workspace = JSON.parse(
  await readFile(new URL("../../../testdata/language/workspace.json", import.meta.url), "utf8"),
);
const queries = JSON.parse(
  await readFile(new URL("../../../testdata/language/queries.json", import.meta.url), "utf8"),
);
const sourceFixture = JSON.parse(
  await readFile(new URL("../../../testdata/workspace/sources.json", import.meta.url), "utf8"),
);
const debugFixture = JSON.parse(
  await readFile(new URL("../../../testdata/debug/breakpoint.json", import.meta.url), "utf8"),
);
test(
  "Node tools use the distributed compiler and shared language/source fixtures",
  { timeout: 600_000 },
  async (t) => {
    await exerciseTools(tools, workspace, sourceFixture, debugFixture, queries, t.signal);
  },
);
test(
  "browser tools use the distributed compiler and shared language/source fixtures",
  { timeout: 600_000 },
  async (t) => {
    const page = await createBrowserPage(t, {
      "/": '<!doctype html><script type="module" src="/scenario.js"></script>',
      "/scenario.js": new URL("./support/language-browser.js", import.meta.url),
      "/language-scenario.js": new URL("./support/language-service.js", import.meta.url),
      "/workspace.json": JSON.stringify(workspace),
      "/source.json": JSON.stringify(sourceFixture),
      "/debug.json": JSON.stringify(debugFixture),
      "/queries.json": JSON.stringify(queries),
    });
    const measurements = await readBrowserScenario(page);
    return measurements;
  },
);
