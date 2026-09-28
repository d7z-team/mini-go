import test from "node:test";
import { readFile } from "node:fs/promises";
import * as tools from "@d7z-team/mini-go/tools";
import { exerciseTools } from "./tools_scenario.js";
import { createBrowserPage } from "./browser_helpers.js";
import { exerciseCompilerWorkloads } from "./compiler_workloads_scenario.js";

const workloads = JSON.parse(
  await readFile(new URL("../../../../testdata/language/workloads.json", import.meta.url), "utf8"),
).filter(({ Name }) => Name === "pure" || Name === "typed-view");

test("Node compiler shares typed workloads and warm snapshots", { timeout: 60_000 }, async (t) => {
  t.diagnostic(JSON.stringify(await exerciseCompilerWorkloads(tools, workloads)));
});

test(
  "browser compiler shares typed workloads and warm snapshots",
  { timeout: 60_000 },
  async (t) => {
    const page = await createBrowserPage(t, {
      "/workloads.js": new URL("./compiler_workloads_scenario.js", import.meta.url),
    });
    const result = await page.evaluate(async (workloads) => {
      const { exerciseCompilerWorkloads } = await import("/workloads.js");
      return exerciseCompilerWorkloads(await import("/browser-tools.js"), workloads);
    }, workloads);
    t.diagnostic(JSON.stringify(result));
  },
);

const workspace = JSON.parse(
  await readFile(new URL("../../../../testdata/language/workspace.json", import.meta.url), "utf8"),
);
const queries = JSON.parse(
  await readFile(new URL("../../../../testdata/language/queries.json", import.meta.url), "utf8"),
);
const sourceFixture = JSON.parse(
  await readFile(new URL("../../../../testdata/workspace/sources.json", import.meta.url), "utf8"),
);
const debugFixture = JSON.parse(
  await readFile(new URL("../../../../testdata/debug/breakpoint.json", import.meta.url), "utf8"),
);
test(
  "Node tools use the distributed compiler and shared language/source fixtures",
  { timeout: 180_000 },
  async (t) => {
    t.diagnostic(
      JSON.stringify(await exerciseTools(tools, workspace, sourceFixture, debugFixture, queries)),
    );
  },
);
test(
  "browser tools use the distributed compiler and shared language/source fixtures",
  { timeout: 180_000 },
  async (t) => {
    const page = await createBrowserPage(t, {
      "/scenario.js": new URL("./tools_scenario.js", import.meta.url),
    });
    const measurements = await page.evaluate(
      async ({ workspace, sourceFixture, debugFixture, queries }) => {
        const tools = await import("/browser-tools.js");
        const { exerciseTools } = await import("/scenario.js");
        return exerciseTools(tools, workspace, sourceFixture, debugFixture, queries);
      },
      { workspace, sourceFixture, debugFixture, queries },
    );
    t.diagnostic(JSON.stringify(measurements));
  },
);
