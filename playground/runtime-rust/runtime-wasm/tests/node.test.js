import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { MiniGo, values } from "@d7z-team/mini-go";
import * as nativeRPC from "@d7z-team/mini-go/rpc";
import { root, startPeer } from "./test_helpers.js";
import { exerciseNativeRPC } from "./native_rpc_scenario.js";
import { exerciseRPC } from "./rpc_scenario.js";

const fixtures = process.env.MINIGO_WASM_FIXTURES;
if (!fixtures) throw new Error("MINIGO_WASM_FIXTURES is required");
const load = (name) => readFile(path.join(fixtures, `${name}.json`));
const providerModule = new URL("./provider.js", import.meta.url);

test("Node worker: default and full-width finite step budgets", async (t) => {
  for (const maxSteps of [0, 9223372036854775807n]) {
    const vm = await MiniGo.create(await load("answer"), { maxSteps });
    t.after(() => vm.terminate());
    const call = vm.start("default");
    assert.equal((await call.result).roots[0].data.Integer, 42n);
    await call.settled;
    await vm.close();
  }
});

test(
  "Node worker: queued cancellation preserves the paused foreground call",
  { timeout: 30_000 },
  async (t) => {
    const vm = await MiniGo.create(await load("loop"), { maxSteps: -1 });
    t.after(() => vm.terminate());
    const first = vm.start("default");
    // stats travels behind start and lets the worker publish its first driver turn.
    while (!(await vm.stats()).activeScopes) await new Promise((resolve) => setTimeout(resolve, 1));
    await vm.pause();
    const second = vm.start("default");
    second.cancel();
    await assert.rejects(second.settled, /cancel/i);
    assert.ok((await vm.stack()).length);
    first.cancel();
    await assert.rejects(first.settled, /cancel/i);
    await vm.close();
  },
);

test(
  "Node worker: host waits for queued calls and their scopes before closing",
  { timeout: 30_000 },
  async (t) => {
    for (const name of ["answer", "timer", "background", "host"]) {
      const vm = await MiniGo.create(await load(name), { providerModule, maxSteps: -1 });
      t.after(() => vm.terminate());
      const calls = Array.from({ length: 4 }, () => vm.start("default"));
      for (const call of calls) {
        const result = await call.result;
        await call.settled;
        if (name !== "host") assert.equal(result.roots[0].data.Integer, 42n);
      }
      await vm.close();
    }
  },
);

test("Node worker: RPC duration configuration validates integer bounds", async () => {
  const image = await load("answer");
  for (const milliseconds of [0, -1, 0.5, Number.MAX_SAFE_INTEGER]) {
    await assert.rejects(MiniGo.create(image, { rpcOptions: { leaseTtlMs: milliseconds } }));
  }
});

test(
  "Node worker: values, async initialization, scopes and patch",
  { timeout: 30_000 },
  async (t) => {
    const instances = [];
    t.after(() => {
      for (const vm of instances) vm.terminate();
    });
    for (const name of ["answer", "init", "host", "timer", "background"]) {
      const image = await load(name),
        copy = Buffer.from(image);
      const vm = await MiniGo.create(image, { providerModule });
      instances.push(vm);
      assert.deepEqual(image, copy, "the caller's Node Buffer remains owned and unchanged");
      const execution = vm.start("default");
      const snapshot = await execution.result;
      await execution.settled;
      if (name !== "host") assert.equal(snapshot.roots[0].data.Integer, 42n);
      assert.equal((await vm.stats()).ffiCalls, 0n);
      await vm.close();
    }
    for (const [name, input] of [
      ["echo", values.int(9223372036854775807n)],
      ["string", values.string("中".repeat(100_000))],
    ]) {
      const vm = await MiniGo.create(await load(name));
      instances.push(vm);
      const execution = vm.start("default", [input]);
      assert.deepEqual((await execution.result).roots[0], input);
      await execution.settled;
      await vm.close();
    }
    const patched = await MiniGo.create(await load("answer"));
    instances.push(patched);
    await assert.rejects(patched.patch(new Uint8Array([1, 2, 3])));
    assert.equal((await patched.stats()).generation, 1n);
    const revisions = [await load("answer"), await load("answer-patch")];
    for (let i = 0; i < 31; i++) await patched.patch(revisions[(i + 1) % 2]);
    assert.equal((await patched.stats()).generation, 32n);
    const updated = patched.start("default");
    assert.equal((await updated.result).roots[0].data.Integer, 43n);
    await updated.settled;
    await patched.close();
  },
);

test(
  "Node worker: cancellation, debug, host failures and shutdown ownership",
  { timeout: 30_000 },
  async (t) => {
    const instances = [];
    t.after(() => {
      for (const vm of instances) vm.terminate();
    });
    const vm = await MiniGo.create(await load("loop"));
    instances.push(vm);
    const execution = vm.start("default");
    await new Promise((resolve) => setTimeout(resolve, 20));
    await vm.pause();
    const frames = await vm.stack();
    assert.ok(frames.length);
    await vm.bindings(frames[0].reference);
    await vm.resume();
    await assert.rejects(vm.bindings(frames[0].reference));
    execution.cancel();
    await assert.rejects(execution.result);
    await execution.settled.catch(() => {});
    const timed = vm.start("default", [], { timeoutMs: 20 });
    await assert.rejects(timed.result, /cancel/i);
    await timed.settled.catch(() => {});
    assert.equal((await vm.stats()).activeScopes, 0n);
    await vm.close();

    for (const [expected, provider] of [
      [0n, () => Buffer.from([42])],
      [
        2n,
        () => {
          throw new Error("sync failure");
        },
      ],
      [
        2n,
        async () => {
          throw new Error("async failure");
        },
      ],
    ]) {
      const host = await MiniGo.create(await load("host-result"), { provider });
      instances.push(host);
      const call = host.start("default");
      const result = await call.result;
      await call.settled;
      const message = new TextDecoder().decode(result.roots[1].data.String);
      assert.equal(result.roots[2].data.Integer, expected);
      if (expected === 2n) assert.match(message, /failure/);
      await host.close();
    }
    let entered, release;
    const enteredHost = new Promise((resolve) => {
      entered = resolve;
    });
    const held = new Promise((resolve) => {
      release = resolve;
    });
    const host = await MiniGo.create(await load("host"), {
      provider: async () => {
        entered();
        await held;
        return new Uint8Array();
      },
    });
    instances.push(host);
    const active = host.start("default");
    await enteredHost;
    const queued = host.start("default");
    queued.cancel();
    await assert.rejects(queued.result);
    await assert.rejects(host.close({ signal: AbortSignal.abort() }));
    release();
    await host.close();
    await active.result.catch(() => {});

    const late = await MiniGo.create(await load("host"), { providerModule });
    instances.push(late);
    const call = late.start("default");
    await new Promise((resolve) => setTimeout(resolve, 5));
    call.cancel();
    await late.close();
    await call.result.catch(() => {});
    const aborted = new AbortController();
    const initializing = MiniGo.create(await load("init"), {
      signal: aborted.signal,
      providerModule,
    });
    aborted.abort();
    await assert.rejects(initializing);

    const fault = await MiniGo.create(await load("host"), {
      providerModule: new URL("./exit-provider.js", import.meta.url),
    });
    instances.push(fault);
    const abandoned = fault.start("default");
    await assert.rejects(abandoned.result, /worker exited/);
    await assert.rejects(abandoned.settled, /worker exited/);
    await assert.rejects(fault.close(), /worker exited/);
  },
);

test(
  "Node WebSocket RPC interoperates with Go in both directions",
  { timeout: 30_000 },
  async (t) => {
    const address = await startPeer(t);
    await exerciseRPC(
      MiniGo,
      (name) => readFile(path.join(root, `testdata/rpc/images/${name}.json.gz`)),
      address,
    );
  },
);

test(
  "Node generated TypeScript RPC interoperates with Go in both directions",
  { timeout: 30_000 },
  async (t) => {
    const address = await startPeer(t);
    await exerciseNativeRPC(nativeRPC, address);
  },
);
