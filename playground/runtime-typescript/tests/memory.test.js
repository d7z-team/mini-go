import assert from "node:assert/strict";
import { test } from "vitest";
import { GuestMemory } from "../src/vm/guest-memory";
import { VirtualMachine } from "../src/vm/vm";
import { runtimeVector } from "./support/runtime-vectors.js";

function initialize(vm) {
  for (let count = 0; count < 100_000; count++) {
    const state = vm.pump(4096);
    if (state.error) throw new Error(state.error);
    if (state.ready) return;
  }
  throw new Error("initialization did not settle");
}

function execute(vm, id) {
  for (let count = 0; count < 100_000; count++) {
    const state = vm.pump(4096);
    if (state.error) throw new Error(state.error);
    if (state.executions.find((execution) => execution.id === id)?.settled) return vm.result(id);
  }
  throw new Error("execution did not settle");
}

async function runVector(name, limits) {
  const vm = await VirtualMachine.create(runtimeVector(name).bytes, { limits });
  try {
    initialize(vm);
    return execute(vm, vm.start("default", []));
  } finally {
    vm.close();
    vm.pump(1);
  }
}

test("memory census becomes clean and is retriggered by a reference change", () => {
  const memory = new GuestMemory({ maxHeapBytes: 1024n, maxAllocatedBytes: 4096n });
  memory.publish({ liveHeapBytes: 32n, liveHeapObjects: 1n, liveGuestBytes: 64n }, true);
  assert.equal(memory.needsCensus(), false);
  memory.markDirty();
  assert.equal(memory.needsCensus(), true);
  memory.publish({ liveHeapBytes: 32n, liveHeapObjects: 1n, liveGuestBytes: 64n }, true);
  assert.equal(memory.needsCensus(), false);
});

test("repeated reservations are checked before a census and release clears state", () => {
  const memory = new GuestMemory({ maxAllocatedBytes: 100n });
  memory.publish({ liveHeapBytes: 0n, liveHeapObjects: 0n, liveGuestBytes: 50n }, true);
  memory.reserve(40n);
  assert.throws(() => memory.reserve(11n), { code: "allocation_limit" });
  assert.equal(memory.stats().totalGuestAllocatedBytes, 90n);
  memory.publish({ liveHeapBytes: 0n, liveHeapObjects: 0n, liveGuestBytes: 50n }, true);
  memory.release();
  assert.deepEqual(memory.stats(), {
    liveHeapBytes: 0n,
    liveHeapObjects: 0n,
    peakHeapBytes: 0n,
    totalHeapAllocatedBytes: 0n,
    liveGuestBytes: 0n,
    peakGuestBytes: 50n,
    totalGuestAllocatedBytes: 90n,
    guestAllocatedSinceCensusBytes: 0n,
    guestAllocations: 2n,
  });
});

test("known reservations trace at the exact remaining logical budget", () => {
  const memory = new GuestMemory({ maxAllocatedBytes: 100n });
  memory.publish({ liveHeapBytes: 0n, liveHeapObjects: 0n, liveGuestBytes: 90n }, true);
  assert.equal(memory.reservationNeedsCensus(10n), false);
  assert.equal(memory.reservationNeedsCensus(11n), true);
  memory.markDirty();
  assert.equal(memory.needsCensus(), true);
  memory.reserve(10n);
  memory.publish({ liveHeapBytes: 0n, liveHeapObjects: 0n, liveGuestBytes: 100n }, true);
  assert.equal(memory.stats().liveGuestBytes, 100n);
});

test("discarding a pending frame reservation never rolls back allocation totals", () => {
  const memory = new GuestMemory({ maxAllocatedBytes: 100n });
  memory.publish({ liveHeapBytes: 0n, liveHeapObjects: 0n, liveGuestBytes: 0n }, true);
  memory.reserveFrame(8n);
  memory.releaseFrame(8n);
  assert.equal(memory.stats().guestAllocatedSinceCensusBytes, 0n);
  assert.equal(memory.stats().totalGuestAllocatedBytes, 8n);
  assert.equal(memory.stats().guestAllocations, 1n);
  assert.equal(memory.stats().liveGuestBytes, 0n);
});

test("explicit and default memory policies use the same dirty-state transition", () => {
  const defaults = new GuestMemory(undefined);
  const explicit = new GuestMemory({ maxHeapBytes: 64n * 1024n * 1024n });
  const census = { liveHeapBytes: 16n, liveHeapObjects: 1n, liveGuestBytes: 24n };
  defaults.publish(census, true);
  explicit.publish(census, true);
  assert.equal(defaults.needsCensus(), explicit.needsCensus());
  defaults.markDirty();
  explicit.markDirty();
  assert.equal(defaults.needsCensus(), explicit.needsCensus());
});

test("equivalent default and explicit limits preserve VM results", async () => {
  const vector = runtimeVector("arithmetic");
  const defaultResult = await runVector("arithmetic", undefined);
  const explicitResult = await runVector("arithmetic", {
    maxHeapBytes: 64n * 1024n * 1024n,
    maxObjects: 100_000,
    maxAllocatedBytes: 8n * 1024n * 1024n * 1024n,
    maxStringBytes: 64 * 1024 * 1024,
    maxValueDepth: 128,
    maxSequenceElements: 1_000_000,
  });
  assert.deepEqual(explicitResult, defaultResult);
  assert.equal(defaultResult.roots[0].data.Integer, BigInt(vector.result_integer));
});

test("scalar VM storage is still charged against maxAllocatedBytes", async () => {
  await assert.rejects(
    runVector("arithmetic", { maxAllocatedBytes: 1n }),
    /allocation limit|logical allocation limit/,
  );
});

test("real VM workloads enforce heap, object, string, and depth limits", async () => {
  for (const [name, limits, pattern] of [
    ["stdlib_reflect_collections", { maxHeapBytes: 1n }, /heap byte limit|allocation limit/],
    ["stdlib_reflect_collections", { maxObjects: 1 }, /heap object limit|allocation limit/],
    ["string_conversion", { maxStringBytes: 1 }, /string byte limit|string limit/],
    ["stdlib_reflect_cycles", { maxValueDepth: 1 }, /value depth|allocation limit/],
  ])
    await assert.rejects(runVector(name, limits), pattern, name);
});

test("pending string, heap, and object reservations fail at their exact budgets", () => {
  const strings = new GuestMemory({ maxStringBytes: 4n });
  strings.publish(
    { liveHeapBytes: 0n, liveHeapObjects: 0n, liveGuestBytes: 0n, stringBytes: 3n },
    true,
  );
  assert.throws(() => strings.reserve(1n, 2n), { code: "string_limit" });

  const heap = new GuestMemory({ maxHeapBytes: 8n });
  heap.publish({ liveHeapBytes: 7n, liveHeapObjects: 1n, liveGuestBytes: 0n }, true);
  assert.throws(() => heap.reserve(1n, 0n, 2n), { code: "allocation_limit" });

  const objects = new GuestMemory({ maxObjects: 2 });
  objects.publish({ liveHeapBytes: 0n, liveHeapObjects: 2n, liveGuestBytes: 0n }, true);
  assert.throws(() => objects.reserve(1n, 0n, 0n, 1n), { code: "allocation_limit" });
});

test("failed start setup leaves no scope, execution, or task", async () => {
  const vm = await VirtualMachine.create(runtimeVector("recursive_call").bytes, {
    limits: { maxFrames: 0 },
  });
  try {
    initialize(vm);
    const before = vm.stats();
    assert.throws(() => vm.start("default", []), /frame limit/);
    const after = vm.stats();
    assert.equal(after.activeScopes, before.activeScopes);
    assert.equal(after.tasks, before.tasks);
    assert.equal(after.liveGuestBytes, before.liveGuestBytes);
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("string to slice conversion accounts for new backing heap storage", async () => {
  await assert.rejects(
    runVector("string_conversion", { maxHeapBytes: 1n }),
    /heap byte limit|allocation limit/,
  );
});

test("real byte append charges its backing before publishing the result", async () => {
  await assert.rejects(
    runVector("slice_copy", { maxHeapBytes: 1n }),
    /heap byte limit|allocation limit/,
  );
});

test("real slice graph honors the depth limit during append", async () => {
  await assert.rejects(
    runVector("slice_copy", { maxValueDepth: 1 }),
    /value depth|allocation limit|sequence element/,
  );
});

test("frame cache policy does not change guest allocation accounting", async () => {
  const runs = [];
  for (const maxFrameCacheBytes of [0, 8 * 1024 * 1024]) {
    const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {
      limits: { maxFrameCacheBytes },
    });
    try {
      initialize(vm);
      const execution = vm.start("default", []);
      const result = execute(vm, execution);
      runs.push({ result, stats: vm.stats() });
      vm.release(execution);
    } finally {
      vm.close();
      vm.pump(1);
    }
  }
  assert.deepEqual(runs[0].result, runs[1].result);
  assert.equal(runs[0].stats.totalGuestAllocatedBytes, runs[1].stats.totalGuestAllocatedBytes);
  assert.equal(runs[0].stats.peakGuestBytes, runs[1].stats.peakGuestBytes);
});

test("reusing released slots does not accumulate frame root reservations", async () => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {
    limits: { maxAllocatedBytes: 4096n },
  });
  try {
    initialize(vm);
    for (let count = 0; count < 32; count++) {
      const execution = vm.start("default", []);
      execute(vm, execution);
      vm.release(execution);
    }
    assert.equal(vm.stats().liveGuestBytes, 0n);
    assert.ok(vm.stats().totalGuestAllocatedBytes > 0n);
  } finally {
    vm.close();
    vm.pump(1);
  }
});
