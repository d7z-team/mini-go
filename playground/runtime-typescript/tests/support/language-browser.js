import * as tools from "/browser/tools.js";
import { exerciseTools } from "/language-scenario.js";

try {
  const [workspace, sourceFixture, debugFixture, queries] = await Promise.all(
    ["workspace", "source", "debug", "queries"].map(async (name) =>
      (await fetch(`/${name}.json`)).json(),
    ),
  );
  globalThis.__miniGoResult = await exerciseTools(
    tools,
    workspace,
    sourceFixture,
    debugFixture,
    queries,
  );
} catch (error) {
  globalThis.__miniGoError = String(error?.stack ?? error);
}
