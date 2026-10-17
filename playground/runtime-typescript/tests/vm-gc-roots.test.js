import assert from "node:assert/strict";
import { test } from "vitest";
import { VirtualMachine } from "../src/vm/vm";
import { runtimeVector, sealImage } from "./support/runtime-vectors.js";

function forceCollection(vm, major) {
  vm.collectHeap(major);
}

test("young allocation growth does not request a full old graph scan", async () => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
  try {
    initializeWithCollection(vm, "young allocation growth");
    const heap = vm.guestMemory.managedHeap;
    vm.nextMajorBytes = heap.oldGuestBytes + 32n;
    const before = vm.stats();
    const layout = heap.registerLayout({ stride: 8, references: [], dynamics: [] });
    vm.allocatePayload(layout, 1, 64n);
    assert.equal(vm.stats().majorCollections, before.majorCollections);
    assert.ok(heap.occupiedGuestBytes > vm.nextMajorBytes);
  } finally {
    vm.close();
  }
});

function initializeWithCollection(vm, label) {
  for (let count = 0; count < 100_000; count++) {
    forceCollection(vm, count % 2 === 0);
    const state = vm.pump(1);
    if (state.error) throw new Error(`${label}: ${state.error}`);
    if (state.ready) return;
  }
  throw new Error(`${label}: initialization did not settle`);
}

function executeWithCollection(vm, execution, label) {
  for (let count = 0; count < 100_000; count++) {
    forceCollection(vm, count % 2 === 0);
    const state = vm.pump(1);
    if (state.error) throw new Error(`${label}: ${state.error}`);
    if (state.executions.find((item) => item.id === execution)?.settled)
      return vm.result(execution);
  }
  throw new Error(`${label}: execution did not settle`);
}

async function runWithForcedCollection(name) {
  const vm = await VirtualMachine.create(runtimeVector(name).bytes, {});
  vm.guestMemory.managedHeap.poisonCollected = true;
  const allocate = vm.allocatePayload;
  let collections = 0;
  vm.allocatePayload = function (...args) {
    this.collectHeap(collections++ % 2 === 0);
    return allocate.apply(this, args);
  };
  try {
    initializeWithCollection(vm, name);
    const result = executeWithCollection(vm, vm.start("default", []), name);
    return { result, stats: vm.stats() };
  } finally {
    vm.close();
    vm.pump(1);
  }
}

test.each([
  ["nested_value_storage", 42n],
  ["managed_graph_churn", 42n],
  ["frame_cell_rebind", 42n],
  ["closure", 8n],
  ["stdlib_reflect_fields", 42n],
  ["stdlib_reflect_slice", 42n],
  ["channel_buffer", 42n],
  ["defer_result", 42n],
  ["recover_panic", 42n],
  ["map_range", 35n],
  ["select_transaction", 42n],
  ["select_wait", 42n],
])("forced minor/major collection preserves %s owner roots", async (name, expected) => {
  const { result, stats } = await runWithForcedCollection(name);
  assert.equal(result.roots[0].data.Integer, expected);
  assert.ok(stats.minorCollections > 0n);
  assert.ok(stats.majorCollections > 0n);
  assert.ok(stats.copiedBytes > 0n);
});

test("forced collection preserves an active closure across a hot patch", async () => {
  const original = runtimeVector("closure").bytes;
  const replacementImage = JSON.parse(new TextDecoder().decode(original));
  const constant = Object.values(replacementImage.packages)[0].artifact.constants.find(
    (item) => typeof item.value === "number",
  );
  assert.ok(constant);
  constant.value += 2;
  const replacement = sealImage(replacementImage);

  const vm = await VirtualMachine.create(original, { profile: true });
  vm.guestMemory.managedHeap.poisonCollected = true;
  try {
    initializeWithCollection(vm, "hot patch original");
    const oldExecution = vm.start("default", []);
    for (let count = 0; count < 256; count++) {
      forceCollection(vm, count % 2 === 0);
      vm.pump(1);
      if ((vm.stats().profile?.opcodes.make_closure ?? 0n) > 0n) break;
    }
    const probe = await VirtualMachine.create(replacement, {});
    initializeWithCollection(probe, "hot patch probe");
    const expected = executeWithCollection(probe, probe.start("default", []), "hot patch probe");
    probe.close();
    probe.pump(1);
    await vm.patch(replacement);
    const newExecution = vm.start("default", []);
    const oldResult = executeWithCollection(vm, oldExecution, "hot patch old closure");
    const newResult = executeWithCollection(vm, newExecution, "hot patch new closure");
    assert.equal(oldResult.roots[0].data.Integer, 8n);
    assert.equal(newResult.roots[0].data.Integer, expected.roots[0].data.Integer);
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test.each([0, 8 * 1024 * 1024])(
  "repeated deferred returns survive collection with a %i byte frame cache",
  async (maxFrameCacheBytes) => {
    for (const name of ["defer_result", "recover_panic", "nested_value_storage"]) {
      const vm = await VirtualMachine.create(runtimeVector(name).bytes, {
        limits: { maxFrameCacheBytes },
      });
      vm.guestMemory.managedHeap.poisonCollected = true;
      try {
        initializeWithCollection(vm, name);
        for (let call = 0; call < 3; call++) {
          const execution = vm.start("default", []);
          const result = executeWithCollection(vm, execution, `${name} call ${call}`);
          assert.equal(result.roots[0].data.Integer, 42n);
          assert.equal(vm.stats().reservedBytes, 0n);
        }
      } finally {
        vm.close();
        vm.pump(1);
      }
    }
  },
);
