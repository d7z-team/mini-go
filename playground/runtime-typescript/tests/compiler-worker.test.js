import assert from "node:assert/strict";
import { test } from "vitest";
import { runCompilerWorker } from "../src/compiler/worker";

test("compiler worker transfers owned output and preserves control and error replies", async () => {
  let receive;
  let output;
  const replies = [];
  runCompilerWorker(
    {
      listen(handler) {
        receive = handler;
      },
      send(message, transfer = []) {
        replies.push(structuredClone(message, { transfer }));
      },
    },
    async () => ({
      wasmBytes: 42,
      call(input) {
        if (input[0] === 0) throw new Error("invalid input");
        output = new Uint8Array([input[0], 7]);
        return output;
      },
    }),
  );
  await receive({ kind: "compilerCreate", generation: 1n });
  await receive({ kind: "compilerRequest", generation: 1n, id: 1, input: new Uint8Array([3]) });
  assert.equal(output.byteLength, 0);
  assert.deepEqual(replies[1].value, new Uint8Array([3, 7]));
  await receive({ kind: "compilerRequest", generation: 1n, id: 2, input: new Uint8Array([0]) });
  assert.equal(replies[2].error.message, "invalid input");
  await receive({ kind: "compilerStats", generation: 1n, id: 3 });
  assert.equal(replies[3].stats.requests, 1n);
  assert.equal(replies[3].stats.wasmBytes, 42);
  await receive({ kind: "compilerRequest", generation: 2n, id: 4, input: new Uint8Array([3]) });
  assert.equal(replies.length, 4);
  await receive({ kind: "compilerClose", generation: 1n, id: 5 });
});
