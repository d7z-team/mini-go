import assert from "node:assert/strict";
import test from "node:test";
import { GuestMemory } from "../dist/vm/guest-memory.js";
import { VirtualMachine } from "../dist/vm/vm.js";
import { allRuntimeVectors, runtimeVector, sealImage } from "./support/runtime-vectors.js";

function execute(vm, id) {
  for (let count = 0; count < 100_000; count++) {
    const state = vm.pump(4096);
    if (state.executions.find((item) => item.id === id)?.settled) return vm.result(id);
  }
  assert.fail("execution did not settle within its deterministic pump budget");
}

function initialize(vm, label) {
  for (let count = 0; count < 100_000; count++) {
    const state = vm.pump(4096);
    if (state.error) assert.fail(`${label}: initialization failed: ${state.error}`);
    if (state.ready) return;
    assert.equal(vm.actions().length, 0, `${label}: initialization host action`);
  }
  assert.fail(`${label}: initialization did not settle within its deterministic pump budget`);
}

test("direct VM executes every shared optimization corpus", async () => {
  for (const vector of allRuntimeVectors) {
    const vm = await VirtualMachine.create(vector.bytes, {});
    initialize(vm, `${vector.name} O${vector.optimization}`);
    const execution = vm.start("default", []);
    let state;
    for (let count = 0; count < 100_000; count++) {
      state = vm.pump(4096);
      if (state.executions.find((item) => item.id === execution)?.settled) break;
      assert.equal(vm.actions().length, 0, `${vector.name} O${vector.optimization}: host action`);
    }
    assert.equal(
      vm.result(execution).roots[0].data.Integer,
      BigInt(vector.result_integer),
      `${vector.name} O${vector.optimization}`,
    );
    vm.release(execution);
    vm.close();
    vm.pump(1);
    assert.equal(vm.stats().liveGuestBytes, 0n, `${vector.name} O${vector.optimization}: release`);
  }
});

test("execution image decoder enforces the current object schema", async () => {
  const source = new TextDecoder().decode(runtimeVector("arithmetic").bytes);
  for (const [name, input, pattern] of [
    ["duplicate", source.replace("{", '{"format":"duplicate",'), /duplicate object key format/],
    ["case", source.replace('"format"', '"Format"'), /unknown field Format/],
    ["unknown", source.replace("{", '{"unknown":true,'), /unknown field unknown/],
    ["null", source.replace(/"version":[0-9]+/, '"version":null'), /expected integer/],
  ]) {
    await assert.rejects(VirtualMachine.create(new TextEncoder().encode(input), {}), pattern, name);
  }
});

test("hot patch keeps active frames on their revision and routes new calls to the new revision", async () => {
  const vector = runtimeVector("closure");
  const replacement = JSON.parse(new TextDecoder().decode(vector.bytes));
  const artifact = Object.values(replacement.packages)[0].artifact;
  const constant = artifact.constants.find((item) => typeof item.value === "number");
  assert.ok(constant);
  constant.value += 2;
  const replacementBytes = sealImage(replacement);

  const probe = await VirtualMachine.create(replacementBytes, {});
  initialize(probe, "replacement probe");
  const replacementResult = execute(probe, probe.start("default", [])).roots[0].data.Integer;
  probe.close();
  assert.notEqual(replacementResult, BigInt(vector.result_integer));

  const vm = await VirtualMachine.create(vector.bytes, { profile: true });
  initialize(vm, "original program");
  const oldExecution = vm.start("default", []);
  for (let count = 0; count < 10_000; count++) {
    vm.pump(1);
    if ((vm.stats().profile?.opcodes.make_closure ?? 0n) > 0n) break;
  }
  assert.ok((vm.stats().profile?.opcodes.make_closure ?? 0n) > 0n, "old closure was created");
  await vm.patch(replacementBytes);
  const newExecution = vm.start("default", []);
  assert.equal(execute(vm, oldExecution).roots[0].data.Integer, BigInt(vector.result_integer));
  assert.equal(execute(vm, newExecution).roots[0].data.Integer, replacementResult);
  vm.release(oldExecution);
  vm.release(newExecution);

  const rejected = JSON.parse(new TextDecoder().decode(replacementBytes));
  const rejectedArtifact = Object.values(rejected.packages)[0].artifact;
  rejectedArtifact.globals ??= [];
  rejectedArtifact.globals.push({
    id: "global.breaking",
    type: { kind: 3, primitive: 3 },
  });
  await assert.rejects(vm.patch(sealImage(rejected)), { code: "global_shape_changed" });
  assert.equal(vm.stats().generation, 2n);
  vm.close();
});

test("guest memory publishes deterministic lifecycle counters", () => {
  const memory = new GuestMemory({ maxAllocatedBytes: 550n, maxHeapBytes: 300n });
  assert.equal(
    memory.publish({ liveHeapBytes: 128n, liveHeapObjects: 1n, liveGuestBytes: 256n }, true),
    0n,
  );
  assert.deepEqual(memory.stats(), {
    liveHeapBytes: 128n,
    liveHeapObjects: 1n,
    peakHeapBytes: 128n,
    totalHeapAllocatedBytes: 128n,
    liveGuestBytes: 256n,
    peakGuestBytes: 256n,
    totalGuestAllocatedBytes: 256n,
    guestAllocatedSinceCensusBytes: 256n,
    guestAllocations: 1n,
  });
  memory.reserve(32n);
  assert.throws(() => memory.reserve(300n), { code: "allocation_limit" });
  assert.equal(
    memory.publish({ liveHeapBytes: 64n, liveHeapObjects: 1n, liveGuestBytes: 200n }, true),
    64n,
  );
  assert.equal(memory.stats().totalGuestAllocatedBytes, 288n);
  memory.release();
  assert.equal(memory.stats().liveGuestBytes, 0n);
  assert.equal(memory.stats().peakGuestBytes, 256n);
});
