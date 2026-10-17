import assert from "node:assert/strict";
import { test } from "vitest";
import { VirtualMachine } from "../src/vm/vm";
import { runtimeVector, sealImage } from "./support/runtime-vectors.js";

const integer = { kind: 3, primitive: 3 };
const string = { kind: 3, primitive: 2 };
const bytes = { kind: 5, node: "bytes" };

function ffiImage() {
  const image = JSON.parse(new TextDecoder().decode(runtimeVector("arithmetic").bytes));
  const artifact = Object.values(image.packages)[0].artifact;
  const fn = artifact.functions.find((candidate) => candidate.id === "fn.Main");
  artifact.type_table = { nodes: [{ id: "bytes", kind: 5, elem: { kind: 3, primitive: 9 } }] };
  artifact.constants = [
    { id: "route", type: string, value: "echo" },
    { id: "payload", type: string, value: "hello" },
  ];
  fn.signature = { results: [integer] };
  fn.code = {
    types: [string, string, bytes, string, integer],
    descriptors: {
      const: [{ constant: "route" }, { constant: "payload" }],
      callffi: [{ arg_count: 2, result_count: 3 }],
      return: [{ result_count: 1 }],
    },
    instructions: [
      [3, 0, 0],
      [3, 1, 1],
      [66, 0, 2],
      [17, 0, 3],
    ],
    operands: [
      { outputs: [0] },
      { outputs: [1] },
      {
        inputs: [
          [0, 0],
          [0, 1],
        ],
        outputs: [2, 3, 4],
      },
      { inputs: [[0, 4]], release: [4] },
    ],
  };
  return sealImage(image);
}

function initialize(vm) {
  for (let count = 0; count < 1_000; count++) {
    vm.collectHeap(count % 2 === 0);
    const state = vm.pump(64);
    if (state.error) throw new Error(state.error);
    if (state.ready) return;
    assert.deepEqual(vm.actions(), []);
  }
  assert.fail("VM initialization did not settle");
}

function waitForHostCall(vm, execution) {
  for (let count = 0; count < 1_000; count++) {
    vm.collectHeap(count % 2 === 0);
    vm.pump(1);
    const call = vm.actions().find((action) => action.kind === "call");
    if (call) return call;
    assert.equal(
      vm.stats().activeScopes > 0n,
      true,
      `execution ${execution} disappeared before the host call`,
    );
  }
  assert.fail("VM did not emit the FFI call");
}

function settle(vm, execution) {
  for (let count = 0; count < 1_000; count++) {
    vm.collectHeap(count % 2 === 0);
    const state = vm.pump(1);
    if (state.executions.find((item) => item.id === execution)?.settled) return;
  }
  assert.fail("execution did not settle");
}

test("FFI result is consumed once and survives forced collection", async () => {
  const vm = await VirtualMachine.create(ffiImage(), {});
  vm.guestMemory.managedHeap.poisonCollected = true;
  try {
    initialize(vm);
    const execution = vm.start("default", []);
    const call = waitForHostCall(vm, execution);
    assert.deepEqual(call, {
      kind: "call",
      id: call.id,
      route: "echo",
      payload: [104, 101, 108, 108, 111],
    });

    vm.complete(call.id, Uint8Array.of(1, 2, 3));
    vm.collectHeap(true);
    settle(vm, execution);
    assert.deepEqual(vm.actions(), [{ kind: "decision", id: call.id, accepted: true }]);
    assert.equal(vm.result(execution).roots[0].data.Integer, 0n);

    vm.complete(call.id, Uint8Array.of(9));
    assert.deepEqual(vm.actions(), [], "a late host result must not produce another decision");
    vm.release(execution);
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("cancel and close emit one host cancellation and one close action", async () => {
  const vm = await VirtualMachine.create(ffiImage(), {});
  try {
    initialize(vm);
    const execution = vm.start("default", []);
    const call = waitForHostCall(vm, execution);
    vm.cancel(execution);
    vm.close();
    assert.deepEqual(vm.actions(), [{ kind: "cancel", id: call.id }]);

    vm.complete(call.id, new Uint8Array(), "host canceled");
    vm.pump(1);
    assert.deepEqual(vm.actions(), [
      { kind: "decision", id: call.id, accepted: false },
      { kind: "close" },
    ]);
    vm.close();
    assert.deepEqual(vm.actions(), [], "repeated close must not duplicate the close action");
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("execution cancellation cancels its host call and discards a late reply once", async () => {
  const vm = await VirtualMachine.create(ffiImage(), {});
  try {
    initialize(vm);
    const execution = vm.start("default", []);
    const call = waitForHostCall(vm, execution);
    vm.cancel(execution);
    assert.deepEqual(vm.actions(), [{ kind: "cancel", id: call.id }]);
    assert.throws(() => vm.result(execution), { code: "canceled" });
    vm.cancel(execution);
    assert.deepEqual(vm.actions(), []);

    vm.complete(call.id, Uint8Array.of(1, 2, 3));
    assert.deepEqual(vm.actions(), [{ kind: "decision", id: call.id, accepted: false }]);
    assert.equal(vm.pendingHost.size, 0);
    vm.complete(call.id, Uint8Array.of(4));
    vm.pump(1);
    assert.deepEqual(vm.actions(), []);
    vm.release(execution);
  } finally {
    vm.close();
    vm.pump(1);
  }
  assert.equal(vm.guestMemory.stats().occupiedBytes, 0n);
});

test("failed patch leaves a pending FFI operation on the current revision", async () => {
  const vm = await VirtualMachine.create(ffiImage(), {});
  try {
    initialize(vm);
    const execution = vm.start("default", []);
    const call = waitForHostCall(vm, execution);
    await assert.rejects(vm.patch(Uint8Array.of(0x7b)), { code: "invalid_image" });
    assert.equal(vm.stats().generation, 1n);
    vm.complete(call.id, Uint8Array.of(7));
    settle(vm, execution);
    assert.equal(vm.result(execution).roots[0].data.Integer, 0n);
    vm.release(execution);
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("FFI control reservation failure publishes no pending host operation", async () => {
  const vm = await VirtualMachine.create(ffiImage(), {});
  try {
    initialize(vm);
    const execution = vm.start("default", []);
    vm.testAllocationFault = { phase: "reserve", remaining: 1 };
    const state = vm.pump(64);
    assert.equal(state.executions.find((item) => item.id === execution)?.state, "Failed");
    assert.deepEqual(vm.actions(), []);
    assert.throws(() => vm.result(execution), { code: "injected_fault" });
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("FFI reply quota failures settle the execution and reject the reply once", async () => {
  for (const roomForReply of [0n, 64n]) {
    const vm = await VirtualMachine.create(ffiImage(), {});
    try {
      initialize(vm);
      const execution = vm.start("default", []);
      const call = waitForHostCall(vm, execution);
      vm.collectHeap(true);
      const heap = vm.guestMemory.managedHeap;
      // The first case rejects reply control admission; the second admits
      // those bytes but leaves no room to materialize the guest backing.
      heap.limits.maxGuestBytes = heap.stats().occupiedBytes + roomForReply;
      assert.doesNotThrow(() => vm.complete(call.id, new Uint8Array(64)));
      const state = vm.pump(1);
      const result = state.executions.find((item) => item.id === execution);
      assert.equal(result.state, "Failed");
      assert.equal(result.settled, true);
      assert.throws(() => vm.result(execution), { code: "allocation_limit" });
      assert.deepEqual(vm.actions(), [{ kind: "decision", id: call.id, accepted: false }]);
      assert.equal(vm.pendingHost.size, 0);
      assert.equal(heap.stats().reservedBytes, 0n);

      vm.complete(call.id, Uint8Array.of(1));
      vm.pump(1);
      assert.deepEqual(vm.actions(), []);
      vm.release(execution);
    } finally {
      vm.close();
      vm.pump(1);
    }
    assert.equal(vm.guestMemory.stats().occupiedBytes, 0n);
  }
});
