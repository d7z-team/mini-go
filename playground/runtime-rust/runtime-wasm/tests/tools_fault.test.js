import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createWorker } from "../dist/node.js";
import { LanguageService } from "@d7z-team/mini-go/tools";

test(
  "compiler restores confirmed deliveries across worker loss and candidate failure",
  { timeout: 600_000 },
  async () => {
    const image = await readFile(new URL("../dist/tools/compiler.json.gz", import.meta.url));
    const workspace = JSON.parse(
      await readFile(
        new URL("../../../../testdata/language/workspace.json", import.meta.url),
        "utf8",
      ),
    );
    let loss;
    const resources = {
      limits: { maxSteps: 0, maxHeapBytes: 192 * 1024 * 1024 },
      load: { maxImageBytes: 60 * 1024 * 1024 },
    };
    const expectedResources = structuredClone(resources);
    let cleanupFailure = false;
    const factory = (options) => {
      const connection = createWorker(options);
      let failed;
      let operation;
      return {
        ...connection,
        terminate() {
          connection.terminate();
          if (cleanupFailure) {
            cleanupFailure = false;
            throw new Error("injected previous owner cleanup error");
          }
        },
        send(message) {
          if (message.kind === "compilerCreate")
            assert.deepEqual(message.resources, expectedResources);
          if (message.kind === "compilerRequest") operation = JSON.parse(message.input).Operation;
          connection.send(message);
          if (message.kind === "compilerAck" && loss === "confirmed") {
            loss = undefined;
            queueMicrotask(() => failed(new Error("injected confirmed worker loss")));
          }
        },
        listen(receive, failure) {
          failed = failure;
          connection.listen((reply) => {
            if (reply.kind === "compilerResponse") {
              receive({
                ...reply,
                generation: reply.generation + 1n,
                error: { message: "late owner" },
              });
              receive({ ...reply, id: reply.id + 1, error: { message: "late request" } });
            }
            if (reply.kind === "compilerResponse" && reply.restore && loss === operation) {
              loss = undefined;
              failure(new Error("injected unconfirmed delivery loss"));
              return;
            }
            receive(reply);
          }, failure);
        },
      };
    };
    const service = await LanguageService.create(factory, image, {
      ...resources,
      timeoutMs: 300_000,
    });
    resources.limits.maxSteps = 1;
    resources.load.maxImageBytes = 1;
    image.fill(0);
    try {
      loss = "workspace/open";
      await assert.rejects(service.open(workspace), /unconfirmed delivery loss/);
      await assert.rejects(service.analyze(), /workspace|session/i);
      await service.open(workspace);
      const first = await service.analyze();
      const uri = workspace.Packages[0].Files[0].URI;
      loss = "document/update";
      await assert.rejects(
        service.update([
          {
            Operation: "open",
            Identity: { URI: uri, ModulePath: "sample", Path: "main.mgo" },
            Version: 1,
            Text: "package main\nfunc Changed() int { return 99 }\n",
          },
        ]),
        /unconfirmed delivery loss/,
      );
      const restored = await service.analyze();
      assert.notEqual(restored.Snapshot, first.Snapshot);
      const hover = await service.query("hover", { URI: uri, Position: { line: 2, character: 6 } });
      assert.match(hover.contents.value, /Answer/);
      loss = "confirmed";
      await service.analyze();
      await service.analyze();
      await assert.rejects(service.upgrade(Uint8Array.of(1, 2, 3)));
      await service.query("hover", { URI: uri, Position: { line: 2, character: 6 } });
      const replacement = await readFile(
        new URL("../dist/tools/compiler.json.gz", import.meta.url),
      );
      loss = "hello";
      await assert.rejects(service.upgrade(replacement), /unconfirmed delivery loss/);
      await service.query("hover", { URI: uri, Position: { line: 2, character: 6 } });
      const upgrading = service.upgrade(replacement);
      cleanupFailure = true;
      replacement.fill(0);
      const queued = service.sources([]);
      assert.match((await upgrading).cleanupError.message, /previous owner cleanup/);
      assert.deepEqual((await queued).Packages, []);
      await assert.rejects(
        service.query("hover", {
          Snapshot: restored.Snapshot,
          URI: uri,
          Position: { line: 2, character: 6 },
        }),
        { code: "stale" },
      );
    } finally {
      await service.dispose();
    }
  },
);

test("compiler admission bounds bytes and honors queued and delivery deadlines", async (t) => {
  t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: 1_700_000_000_000 });
  t.mock.method(performance, "now", () => Date.now() - 1_700_000_000_000);
  let receive;
  let blocked = false;
  let delay = 0;
  let restored;
  const operations = [];
  const factory = () => ({
    listen(handler) {
      receive = handler;
    },
    terminate() {},
    send(message) {
      if (message.kind === "compilerCreate") {
        restored = message.restore.slice();
        queueMicrotask(() =>
          receive({ kind: "compilerResponse", generation: message.generation, id: 0 }),
        );
      }
      if (message.kind === "compilerRequest") {
        operations.push(message.input);
        if (delay) t.mock.timers.setTime(Date.now() + delay);
        if (!blocked)
          queueMicrotask(() =>
            receive({
              kind: "compilerResponse",
              generation: message.generation,
              id: message.id,
              value: "{}",
              restore: new Uint8Array([delay === 40 ? 2 : 1]),
            }),
          );
      }
    },
  });
  const service = await LanguageService.create(factory, new Uint8Array(), { timeoutMs: 300_000 });
  delay = 60_000;
  await service.request({ Operation: "slow" });
  delay = 40;
  await assert.rejects(
    service.request({ Operation: "late", Deadline: String(BigInt(Date.now() + 20) * 1_000_000n) }),
    { code: "deadline" },
  );
  delay = 0;
  await service.request({ Operation: "hello" });
  assert.deepEqual(restored, Uint8Array.of(1));
  blocked = true;
  const pending = service.request({ Value: "x".repeat(33 << 20) });
  const failure = assert.rejects(pending, /closed/);
  try {
    await assert.rejects(service.request({ Value: "x".repeat(33 << 20) }), { code: "budget" });
    const expired = assert.rejects(
      service.request({
        Operation: "query",
        Deadline: String(BigInt(Date.now() + 20) * 1_000_000n),
      }),
      { code: "deadline" },
    );
    t.mock.timers.tick(20);
    await expired;
    assert.equal(
      operations.some((input) => JSON.parse(input).Operation === "query"),
      false,
    );
  } finally {
    const queued = assert.rejects(service.request({ Operation: "queued" }), /closed/);
    const closing = service.dispose();
    assert.equal(service.dispose(), closing);
    t.mock.timers.tick(2_000);
    await closing;
    await queued;
    await failure;
  }
});

test("compiler delivery owns decoded values and preserves confirmed inputs on failure", async () => {
  let restored;
  const factory = () => {
    let receive;
    return {
      listen(handler) {
        receive = handler;
      },
      terminate() {},
      send(message) {
        if (message.kind === "compilerCreate") {
          restored = message.restore.slice();
          queueMicrotask(() =>
            receive({ kind: "compilerResponse", generation: message.generation, id: 0 }),
          );
        }
        if (message.kind === "compilerRequest") {
          const malformed = JSON.parse(message.input).Operation === "malformed";
          queueMicrotask(() => {
            const reply = {
              kind: "compilerResponse",
              generation: message.generation,
              id: message.id,
              value: malformed ? "{" : '{"Value":{"name":"confirmed"}}',
              restore: Uint8Array.of(malformed ? 2 : 1),
            };
            receive(reply);
            reply.value = '{"Value":{"name":"changed after delivery"}}';
            reply.restore.fill(3);
          });
        }
      },
    };
  };
  const service = await LanguageService.create(factory, new Uint8Array());
  try {
    await assert.rejects(service.request({ Operation: "malformed" }), SyntaxError);
    assert.deepEqual((await service.request({ Operation: "hello" })).Value, { name: "confirmed" });
    assert.deepEqual(restored, Uint8Array.of(1));
  } finally {
    await service.dispose();
  }
});

test("compiler timeout configuration covers initialization and queue waiting", async (t) => {
  t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: 1_700_000_000_000 });
  t.mock.method(performance, "now", () => Date.now() - 1_700_000_000_000);
  let receive;
  let hold = false;
  const requests = [];
  const factory = () => ({
    listen(handler) {
      receive = handler;
    },
    terminate() {},
    send(message) {
      if (message.kind === "compilerCreate") {
        t.mock.timers.setTime(Date.now() + 1_000);
        queueMicrotask(() =>
          receive({ kind: "compilerResponse", generation: message.generation, id: 0 }),
        );
      }
      if (message.kind === "compilerRequest") {
        requests.push(message);
        if (!hold)
          queueMicrotask(() =>
            receive({
              kind: "compilerResponse",
              generation: message.generation,
              id: message.id,
              value: "{}",
              restore: Uint8Array.of(1),
            }),
          );
      }
    },
  });
  for (const timeoutMs of [null, false, "1000", 0, -1, 0.5, NaN, Infinity, 2_147_483_648])
    await assert.rejects(LanguageService.create(factory, new Uint8Array(), { timeoutMs }), {
      code: "invalid_argument",
    });
  const defaults = await LanguageService.create(factory, new Uint8Array());
  assert.equal(requests.at(-1).timeout, 29_000);
  await defaults.dispose();
  const service = await LanguageService.create(factory, new Uint8Array(), { timeoutMs: 300_000 });
  assert.equal(requests.at(-1).timeout, 299_000);
  try {
    hold = true;
    const first = service.request({ Operation: "first" });
    const second = service.request({ Operation: "second" });
    t.mock.timers.setTime(Date.now() + 60_000);
    const active = requests.at(-1);
    assert.equal(JSON.parse(active.input).Operation, "first");
    hold = false;
    receive({
      kind: "compilerResponse",
      generation: active.generation,
      id: active.id,
      value: "{}",
      restore: Uint8Array.of(1),
    });
    await first;
    await second;
    assert.equal(requests.at(-1).timeout, 240_000);
    await service.upgrade(Uint8Array.of(1));
    assert.equal(requests.at(-1).timeout, 299_000);
  } finally {
    await service.dispose();
  }
});

test(
  "compiler cancels a delivered worker request before confirmation and recovers",
  { timeout: 600_000 },
  async () => {
    const image = await readFile(new URL("../dist/tools/compiler.json.gz", import.meta.url));
    const canceled = new AbortController();
    let cancelNext = false;
    let canceledID;
    let confirmedCanceled = false;
    const factory = (options) => {
      const connection = createWorker(options);
      return {
        ...connection,
        send(message) {
          if (message.kind === "compilerAck" && message.id === canceledID) confirmedCanceled = true;
          connection.send(message);
          if (message.kind === "compilerRequest" && cancelNext) {
            cancelNext = false;
            canceledID = message.id;
            // The request is delivered before cancel; guest execution is covered by
            // the Rust poll-driven cancellation tests.
            canceled.abort();
          }
        },
      };
    };
    const service = await LanguageService.create(factory, image, { timeoutMs: 300_000 });
    try {
      cancelNext = true;
      await assert.rejects(service.request({ Operation: "hello" }, canceled.signal), {
        name: "AbortError",
      });
      assert.equal(confirmedCanceled, false);
      await service.request({ Operation: "hello" });
      const stats = await service.stats();
      assert.equal(stats.activeScopes, 0n);
      assert.equal(stats.ffiCalls, 0n);
    } finally {
      await service.dispose();
    }
  },
);
