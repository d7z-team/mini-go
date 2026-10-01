import assert from "node:assert/strict";
import { getEventListeners } from "node:events";
import { readFileSync } from "node:fs";
import test from "node:test";
import { Runtime } from "../dist/runtime.js";

const segment = 2_147_483_647;

const stepLimits = JSON.parse(
  readFileSync(new URL("../../../../testdata/runtime/step_limits.json", import.meta.url), "utf8"),
);
for (const vector of stepLimits) {
  test(`shared step limit ${vector.input}`, async (t) => {
    const options = { limits: { maxSteps: BigInt(vector.input) } };
    if (vector.error) await assert.rejects(harness(t, options), /maxSteps/);
    else await harness(t, options);
  });
}

async function harness(t, options = {}) {
  let now = 0,
    next = 0,
    receive,
    fail;
  const timers = new Map(),
    sent = [];
  t.mock.method(performance, "now", () => now);
  t.mock.method(globalThis, "setTimeout", (callback, delay) => {
    assert.ok(delay >= 0 && delay <= segment);
    const id = ++next;
    timers.set(id, { callback, delay, due: now + delay });
    return id;
  });
  t.mock.method(globalThis, "clearTimeout", (id) => timers.delete(id));
  const worker = {
    send(message) {
      if (worker.rejectLifecycle && message.kind === "close") throw new Error("send failed");
      if (worker.rejectStart && message.kind === "start") throw new Error("send failed");
      sent.push(message);
      if (message.kind === "create")
        queueMicrotask(() => {
          receive({
            kind: "limits",
            maxInputBytes: 8 * 1024 * 1024,
            maxHostResultBytes: 4 * 1024 * 1024,
            maxImageBytes: 32 * 1024 * 1024,
          });
          receive({ kind: "ready" });
        });
      if (message.kind === "close" && !worker.holdClose)
        queueMicrotask(() => receive({ kind: "closed" }));
    },
    listen(onMessage, onError) {
      receive = onMessage;
      fail = onError;
    },
    terminate() {},
  };
  const vm = await Runtime.create(new Uint8Array(), options, () => worker);
  t.after(async () => {
    await vm.close().catch(() => {});
    assert.equal(timers.size, 0);
  });
  return {
    vm,
    worker,
    timers,
    sent,
    receive: (message) => receive(message),
    fail: (error) => fail(error),
    advance(ms) {
      now += ms;
      for (const [id, timer] of [...timers]) {
        if (timer.due <= now) {
          timers.delete(id);
          timer.callback();
        }
      }
    },
    cancels: () => sent.filter((message) => message.kind === "cancel"),
  };
}

test("close waiters detach and repeated close observes completion", async (t) => {
  const h = await harness(t);
  h.worker.holdClose = true;
  for (let i = 0; i < 64; i++) {
    const controller = new AbortController();
    const wait = h.vm.close({ signal: controller.signal });
    controller.abort();
    await assert.rejects(wait);
    assert.equal(h.vm.closeWaiters.size, 0);
    assert.equal(getEventListeners(controller.signal, "abort").length, 0);
  }
  h.receive({ kind: "closed" });
  await h.vm.close();
});

test("close send failure rejects all later waits", async (t) => {
  const h = await harness(t);
  h.worker.rejectLifecycle = true;
  await assert.rejects(h.vm.close(), /send failed/);
  await assert.rejects(h.vm.close(), /send failed/);
});

for (const failed of [false, true]) {
  test(`close shares ${failed ? "failure" : "success"} without retaining canceled waiters`, async (t) => {
    const h = await harness(t);
    h.worker.holdClose = true;
    const canceled = new AbortController();
    const active = new AbortController();
    const detached = h.vm.close({ signal: canceled.signal });
    const waiting = h.vm.close({ signal: active.signal });
    const shared = h.vm.close();
    canceled.abort(new Error("stop waiting"));
    await assert.rejects(detached, /stop waiting/);
    if (failed) {
      h.fail(new Error("worker failed"));
      await assert.rejects(waiting, /worker failed/);
      await assert.rejects(shared, /worker failed/);
      await assert.rejects(h.vm.close(), /worker failed/);
    } else {
      h.receive({ kind: "closed" });
      await Promise.all([waiting, shared]);
      h.vm.terminate(new Error("late failure"));
      await h.vm.close();
    }
    assert.equal(getEventListeners(active.signal, "abort").length, 0);
    assert.equal(getEventListeners(canceled.signal, "abort").length, 0);
    assert.equal(h.sent.filter((message) => message.kind === "close").length, 1);
  });
}

test("provider replies during initialization use the resolved Rust budget", async () => {
  for (const maxHostResultBytes of [1, 2]) {
    let receive;
    const worker = {
      listen(onMessage) {
        receive = onMessage;
      },
      terminate() {},
      send(message) {
        if (message.kind === "create")
          queueMicrotask(() => {
            receive({ kind: "limits", maxInputBytes: 16, maxImageBytes: 16, maxHostResultBytes });
            receive({ kind: "host", id: 1, route: "init", payload: new Uint8Array() });
          });
        else if (message.kind === "hostResult")
          queueMicrotask(() => {
            if (message.error) receive({ kind: "fatal", error: { message: message.error } });
            else {
              assert.deepEqual(message.payload, Uint8Array.of(1, 2));
              receive({ kind: "ready" });
            }
          });
        else if (message.kind === "close") queueMicrotask(() => receive({ kind: "closed" }));
      },
    };
    const pending = Runtime.create(
      new Uint8Array(),
      { provider: () => Uint8Array.of(1, 2) },
      () => worker,
    );
    if (maxHostResultBytes === 1) await assert.rejects(pending, /host result exceeds limit/);
    else await (await pending).close();
  }
});

test("invalid step budgets reject before constructing a worker", async () => {
  for (const maxSteps of [-2, 0.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, 1n << 63n]) {
    await assert.rejects(
      Runtime.create(new Uint8Array(), { limits: { maxSteps } }, () => {
        assert.fail("worker constructed for invalid budget");
      }),
      /maxSteps/,
    );
  }
});

test("invalid byte and count budgets reject before constructing a worker", async () => {
  for (const options of [
    { limits: { maxHeapBytes: 0 } },
    { limits: { maxHeapBytes: 1n << 64n } },
    { limits: { maxPendingCalls: -1 } },
    { limits: { maxSequenceElements: 1n } },
    { load: { maxImageBytes: Number.MAX_SAFE_INTEGER + 1 } },
    { load: { maxPackages: 0.5 } },
  ]) {
    await assert.rejects(
      Runtime.create(new Uint8Array(), options, () => {
        assert.fail("worker constructed for invalid resource budget");
      }),
      /invalid/,
    );
  }
});

for (const timeoutMs of [
  0,
  0.5,
  segment - 1,
  segment,
  segment + 1,
  365 * 24 * 3600 * 1000,
  Number.MAX_SAFE_INTEGER,
]) {
  test(`deadline ${timeoutMs} uses bounded timers and monotonic elapsed time`, async (t) => {
    const h = await harness(t);
    h.vm.start("default", [], { timeoutMs });
    assert.equal(h.cancels().length, 0);
    assert.equal(h.timers.size, 1);
    assert.equal([...h.timers.values()][0].delay, Math.min(segment, timeoutMs));
    if (timeoutMs > segment) {
      h.advance(segment);
      assert.equal(h.cancels().length, 0);
      assert.equal(h.timers.size, 1);
      assert.equal([...h.timers.values()][0].delay, Math.min(segment, timeoutMs - segment));
      // A delayed event loop skips elapsed segments instead of extending the deadline.
      h.advance(timeoutMs - segment);
    } else h.advance(timeoutMs);
    assert.equal(h.cancels().length, 1);
    assert.equal(h.timers.size, 0);
  });
}

test("invalid deadlines reject before sending work; missing deadline allocates no timer", async (t) => {
  const h = await harness(t);
  for (const timeoutMs of [-1, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
    assert.throws(() => h.vm.start("default", [], { timeoutMs }));
  }
  assert.equal(h.sent.filter((message) => message.kind === "start").length, 0);
  h.vm.start("default");
  assert.equal(h.timers.size, 0);
});

test("entry result retains the scope deadline until settled", async (t) => {
  const h = await harness(t),
    controller = new AbortController();
  const execution = h.vm.start("default", [], {
    timeoutMs: segment + 1,
    signal: controller.signal,
  });
  const id = h.sent.at(-1).id;
  h.receive({ kind: "result", id, value: { roots: [], objects: [] } });
  await execution.result;
  assert.equal(h.timers.size, 1);
  assert.equal(getEventListeners(controller.signal, "abort").length, 1);
  h.receive({ kind: "settled", id });
  await execution.settled;
  assert.equal(h.timers.size, 0);
  assert.equal(getEventListeners(controller.signal, "abort").length, 0);
  h.advance(segment + 1);
  assert.equal(h.cancels().length, 0);
});

test("queued work and returned entries still expire while their scopes are pending", async (t) => {
  const h = await harness(t);
  h.vm.start("default");
  const call = h.vm.start("default", [], { timeoutMs: 10 });
  const id = h.sent.at(-1).id;
  h.advance(5);
  h.receive({ kind: "result", id, value: { roots: [], objects: [] } });
  await call.result;
  h.advance(5);
  assert.deepEqual(h.cancels(), [{ kind: "cancel", id }]);
  assert.equal(h.timers.size, 0);
});

for (const finish of [
  "cancel",
  "abort",
  "preaborted",
  "send failure",
  "worker failure",
  "terminate",
  "close",
]) {
  test(`${finish} clears deadline and abort listener`, async (t) => {
    const h = await harness(t),
      controller = new AbortController();
    if (finish === "preaborted") controller.abort();
    h.worker.rejectStart = finish === "send failure";
    const start = () =>
      h.vm.start("default", [], { timeoutMs: segment + 1, signal: controller.signal });
    let callback;
    if (finish === "send failure") assert.throws(start, /send failed/);
    else {
      const execution = start();
      h.advance(segment);
      callback = [...h.timers.values()][0]?.callback;
      if (finish === "cancel") {
        execution.cancel();
        execution.cancel();
      }
      if (finish === "abort") controller.abort();
      if (finish === "worker failure") h.fail(new Error("worker failed"));
      if (finish === "terminate") h.vm.terminate();
      if (finish === "close") await h.vm.close();
    }
    assert.equal(h.timers.size, 0);
    assert.equal(getEventListeners(controller.signal, "abort").length, 0);
    callback?.();
    assert.equal(h.timers.size, 0);
    assert.equal(h.cancels().length, ["cancel", "abort", "preaborted"].includes(finish) ? 1 : 0);
  });
}
