import assert from "node:assert/strict";
import { test } from "vitest";
import { LanguageService } from "../src/compiler/language-service";

function frame(metadata) {
  const encoded = new TextEncoder().encode(JSON.stringify(metadata));
  const result = new Uint8Array(20 + encoded.byteLength);
  result.set([0x4d, 0x47, 0x54, 0x33, 0x0d, 0x0a, 0x1a, 0x0a]);
  new DataView(result.buffer).setUint32(8, encoded.byteLength, true);
  result.set(encoded, 20);
  return result;
}

test("compiler owner restarts after cancellation and restores confirmed state", async () => {
  let workers = 0;
  let restores = 0;
  let hold = false;
  const factory = () => {
    workers++;
    let receive;
    let failure;
    return {
      listen(onMessage, onError) {
        receive = onMessage;
        failure = onError;
      },
      terminate() {
        failure?.(new Error("compiler worker terminated"));
      },
      send(message) {
        if (message.kind === "compilerCreate") {
          queueMicrotask(() =>
            receive({ kind: "compilerResponse", generation: message.generation, id: 0 }),
          );
          return;
        }
        if (message.kind !== "compilerRequest") return;
        const request = JSON.parse(new TextDecoder().decode(message.input));
        if (hold && request.Operation === "held") return;
        if (request.Operation === "restore") restores++;
        const response = {
          Format: "mini-go-tools",
          Version: 3,
          CompilerID: "test",
          Session: "session",
          Revision: "revision",
          Value: request.Operation,
          Recovery: { Operation: "restore" },
        };
        queueMicrotask(() =>
          receive({
            kind: "compilerResponse",
            generation: message.generation,
            id: message.id,
            value: frame(response),
          }),
        );
      },
    };
  };

  const service = await LanguageService.create(factory, { timeoutMs: 1_000 });
  assert.equal((await service.request({ Operation: "first" })).Value, "first");
  hold = true;
  const controller = new AbortController();
  const pending = service.request({ Operation: "held" }, controller.signal);
  await new Promise((resolve) => setImmediate(resolve));
  controller.abort(new Error("stop"));
  await assert.rejects(pending, /stop/);
  hold = false;
  assert.equal((await service.request({ Operation: "after" })).Value, "after");
  assert.equal(workers, 2);
  assert.equal(restores, 1);
  await service.dispose();
  await assert.rejects(service.request({ Operation: "closed" }), { code: "closed" });
});

test("compiler owner validates deadlines and queue budgets", async () => {
  const factory = () => {
    let receive;
    return {
      listen(handler) {
        receive = handler;
      },
      terminate() {},
      send(message) {
        if (message.kind === "compilerCreate")
          queueMicrotask(() =>
            receive({ kind: "compilerResponse", generation: message.generation, id: 0 }),
          );
        else if (message.kind === "compilerRequest")
          queueMicrotask(() =>
            receive({
              kind: "compilerResponse",
              generation: message.generation,
              id: message.id,
              value: frame({
                Format: "mini-go-tools",
                Version: 3,
                CompilerID: "test",
                Session: "session",
              }),
            }),
          );
      },
    };
  };
  await assert.rejects(LanguageService.create(factory, { timeoutMs: 0 }), {
    code: "invalid_argument",
  });
  const service = await LanguageService.create(factory);
  assert.throws(() => service.request({ Value: "x".repeat(65 << 20) }), { code: "budget" });
  await assert.rejects(service.request({ Operation: "late", Deadline: "0" }), {
    code: "deadline",
  });
  await service.dispose();
});
