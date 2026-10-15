import { test } from "vitest";
import assert from "node:assert/strict";
import { DebugSession } from "../src/runtime/debug";

for (const command of ["disconnect", "terminate"]) {
  for (const owned of [false, true]) {
    test(`debug ${command}: ${owned ? "owned" : "borrowed"} runtime lifecycle`, async () => {
      const requests = [];
      let closes = 0;
      const runtime = {
        async debugOpen() {},
        async debugRequest(command, arguments_) {
          requests.push({ command, arguments_ });
          return { accepted: true };
        },
        async close() {
          closes++;
        },
      };
      const session = owned
        ? await DebugSession.create(async () => runtime, new Uint8Array())
        : await DebugSession.attach(runtime);
      const arguments_ = { restart: false };
      const ending = session.request(command, arguments_);
      await session.dispose();
      assert.deepEqual(await ending, { accepted: true });
      assert.deepEqual(requests, [{ command, arguments_ }]);
      assert.equal(closes, owned ? 1 : 0);
      await assert.rejects(session.request("threads"), /disposed/);
    });
  }
}

test("debug terminal request failure leaves the session available for disposal", async () => {
  const requests = [];
  let closes = 0;
  const runtime = {
    async debugOpen() {},
    async debugRequest(command) {
      requests.push(command);
      if (command === "terminate") throw new Error("target rejected termination");
      return {};
    },
    async close() {
      closes++;
    },
  };
  const session = await DebugSession.create(async () => runtime, new Uint8Array());
  await assert.rejects(session.request("terminate"), /rejected termination/);
  assert.equal(closes, 0);
  await session.dispose();
  assert.deepEqual(requests, ["terminate", "disconnect"]);
  assert.equal(closes, 1);
});
