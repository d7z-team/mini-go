import test from "node:test";
import { readFile } from "node:fs/promises";
import * as tools from "@d7z-team/mini-go/tools";
import { exerciseTools } from "./support/language-service.js";
import { createBrowserPage } from "./support/browser.js";
import { exerciseCompilerWorkload } from "./support/compiler-workloads.js";

const workloads = JSON.parse(
  await readFile(new URL("../../../testdata/language/workloads.json", import.meta.url), "utf8"),
);

for (const workload of workloads) {
  test(
    `Node compiler workload ${workload.Name} preserves warm snapshots`,
    { timeout: 600_000 },
    async (t) => {
      t.diagnostic(JSON.stringify(await exerciseCompilerWorkload(tools, workload, t.signal)));
    },
  );
  test(
    `browser compiler workload ${workload.Name} preserves warm snapshots`,
    { timeout: 600_000 },
    async (t) => {
      const page = await createBrowserPage(t, {
        "/workloads.js": new URL("./support/compiler-workloads.js", import.meta.url),
      });
      const result = await page.evaluate(async (workload) => {
        const { exerciseCompilerWorkload } = await import("/workloads.js");
        return exerciseCompilerWorkload(await import("/tools.browser.js"), workload);
      }, workload);
      t.diagnostic(JSON.stringify(result));
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
    t.diagnostic(
      JSON.stringify(
        await exerciseTools(tools, workspace, sourceFixture, debugFixture, queries, t.signal),
      ),
    );
  },
);
test(
  "browser tools use the distributed compiler and shared language/source fixtures",
  { timeout: 600_000 },
  async (t) => {
    const page = await createBrowserPage(t, {
      "/scenario.js": new URL("./support/language-service.js", import.meta.url),
    });
    const measurements = await page.evaluate(
      async ({ workspace, sourceFixture, debugFixture, queries }) => {
        const tools = await import("/tools.browser.js");
        const { exerciseTools } = await import("/scenario.js");
        return exerciseTools(tools, workspace, sourceFixture, debugFixture, queries);
      },
      { workspace, sourceFixture, debugFixture, queries },
    );
    t.diagnostic(JSON.stringify(measurements));
  },
);
