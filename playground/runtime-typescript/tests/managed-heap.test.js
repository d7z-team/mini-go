import assert from "node:assert/strict";
import { test } from "vitest";
import { ManagedHeap } from "../src/vm/managed-heap";
import { VirtualMachine } from "../src/vm/vm";
import { runtimeVector } from "./support/runtime-vectors.js";

function execute(vm, id) {
  for (let count = 0; count < 100_000; count++) {
    const state = vm.pump(4096);
    if (state.error) throw new Error(state.error);
    if (state.executions.find((execution) => execution.id === id)?.settled) return vm.result(id);
  }
  throw new Error("execution did not settle");
}

function allocate(heap, stride, logical = BigInt(stride), references = []) {
  const layout = heap.registerLayout({ stride, references });
  const reservation = heap.reserve(layout, 1, logical);
  const bytes = reservation.bytes();
  for (let index = 0; index < bytes.length; index++) bytes[index] = index + 1;
  for (const offset of references) bytes.fill(0, offset, offset + 8);
  return reservation.commit();
}

test("slice backing allocation in the real VM publishes managed payload storage", async () => {
  const vm = await VirtualMachine.create(runtimeVector("slice_copy").bytes, {});
  try {
    for (;;) {
      const state = vm.pump(4096);
      if (state.error) throw new Error(state.error);
      if (state.ready) break;
    }
    const result = execute(vm, vm.start("default", []));
    assert.equal(result.roots.length, 1);
    assert.ok(vm.guestMemory.managedHeap.stats().totalAllocations > 0n);
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("managed heap keeps generation checked handles while collecting young payloads", () => {
  const heap = new ManagedHeap();
  const retained = allocate(heap, 24, 24n, [0]);
  const child = allocate(heap, 48, 48n);
  heap.writeReference(retained, 0, child);
  heap.addRoot(retained);
  const temporary = allocate(heap, 32);

  const before = heap.get(retained);
  heap.collectMinor();
  const after = heap.get(retained);
  assert.notDeepEqual(
    { page: after.page, offset: after.offset },
    { page: before.page, offset: before.offset },
  );
  assert.deepEqual(heap.readReference(retained, 0), child);
  assert.deepEqual([...heap.read(child).slice(0, 3)], [1, 2, 3]);
  assert.throws(() => heap.get(temporary), /stale managed handle/);

  heap.collectMinor();
  const stats = heap.stats();
  assert.equal(stats.minorCollections, 2n);
  assert.equal(stats.promotedBytes, 72n);
  assert.equal(stats.reclaimedBytes, 32n);
  assert.ok(stats.copiedBytes >= 72n);
});

test("major collection follows numeric references and rejects stale generations", () => {
  const heap = new ManagedHeap();
  const root = allocate(heap, 16, 16n, [0]);
  const child = allocate(heap, 16);
  const dead = allocate(heap, 16);
  heap.writeReference(root, 0, child);
  heap.addRoot(root);
  heap.collectMajor();
  assert.equal(heap.get(child).bytes, 16n);
  assert.throws(() => heap.get(dead), /stale managed handle/);
  heap.removeRoot(root);
  heap.collectMajor();
  assert.throws(() => heap.get(root), /stale managed handle/);
  const reused = [allocate(heap, 16), allocate(heap, 16)];
  const rootGeneration = reused.find((handle) => handle.slot === root.slot);
  assert.ok(rootGeneration);
  assert.notEqual(rootGeneration.generation, root.generation);
  assert.throws(() => heap.get(root), /stale managed handle/);
});

test("old to young references are remembered for a minor collection", () => {
  const heap = new ManagedHeap();
  const old = allocate(heap, 16, 16n, [0]);
  heap.addRoot(old);
  heap.collectMinor();
  heap.collectMinor();
  const young = allocate(heap, 16);
  heap.writeReference(old, 0, young);
  heap.collectMinor();
  assert.equal(heap.get(young).bytes, 16n);
});
