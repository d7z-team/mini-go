import assert from "node:assert/strict";
import { test } from "vitest";
import { Kind, PrimitiveKind } from "../src/vm/contract.generated";
import { GUEST_COST, GuestMemory } from "../src/vm/guest-memory";
import { VirtualMachine } from "../src/vm/vm";
import { TypeRegistry, cell, primitive } from "../src/vm/value";

function byteVM() {
  const vm = Object.create(VirtualMachine.prototype);
  vm.options = { limits: { maxSequenceElements: 1_000_000 } };
  vm.guestMemory = new GuestMemory(undefined);
  vm.censusStorage = new WeakSet();
  vm.updateMemoryCensus = () => {};
  return vm;
}

function byteTypes() {
  const registry = new TypeRegistry("append-test", undefined);
  const byte = primitive(registry, PrimitiveKind.PrimitiveUint8);
  const slice = registry.runtime({
    kind: Kind.Slice,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "bytes",
  });
  slice.dynamic = { kind: Kind.Slice, elem: byte };
  return { registry, byte, slice };
}

function uint(byte, type) {
  return { type, kind: "uint", value: BigInt(byte) };
}

function reflectedCall(vm, registry, id, inputs) {
  const result = {};
  vm.outputs = (_frame, _outputs, values) => (result.values = values);
  vm.outputType = () => primitive(registry, PrimitiveKind.PrimitiveInt);
  vm.reflectValue = (_frame, _type, view) => view.current;
  vm.reflectFailure = (_frame, _instruction, message) => (result.error = message);
  vm.reflectIntrinsic(
    {},
    { fn: { module: { registry } } },
    { operands: { outputs: [0] } },
    id,
    inputs,
  );
  return result;
}

test("reflect MakeSlice enforces capacity limits before constructing storage", () => {
  const vm = byteVM();
  const { registry, slice } = byteTypes();
  vm.options.limits.maxSequenceElements = 1;
  const integer = primitive(registry, PrimitiveKind.PrimitiveInt);
  assert.throws(
    () =>
      reflectedCall(vm, registry, "reflect.make_slice", [
        { type: slice, kind: "nil", metadata: slice },
        { type: integer, kind: "int", value: 0n },
        { type: integer, kind: "int", value: 2n },
      ]),
    (error) => error.code === "allocation_limit",
  );
  assert.equal(vm.guestMemory.stats().totalGuestAllocatedBytes, 0n);
  const result = reflectedCall(vm, registry, "reflect.make_slice", [
    { type: slice, kind: "nil", metadata: slice },
    { type: integer, kind: "int", value: 0n },
    { type: integer, kind: "int", value: 1n },
  ]);
  assert.equal(result.values[0].value.capacity, 1);
  assert.equal(result.values[0].value.storage.root.value.kind, "bytes");
});

test("reflect byte append validates every element before changing shared storage", () => {
  const vm = byteVM();
  const { registry, slice, byte } = byteTypes();
  const original = vm.sliceFromValues(slice, [uint(7, byte), uint(0, byte)], 1);
  const integer = primitive(registry, PrimitiveKind.PrimitiveInt);
  const values = [uint(9, byte), { type: integer, kind: "int", value: 300n }];
  const result = reflectedCall(vm, registry, "reflect.value_append", [
    { type: slice, kind: "nil", metadata: { current: original } },
    {
      type: slice,
      kind: "array",
      value: values.map((current) => ({ type: byte, kind: "nil", metadata: { current } })),
    },
  ]);
  assert.match(result.error, /not assignable/);
  assert.deepEqual([...original.value.storage.root.value.value], [7, 0]);
});

test("reflect bulk byte operations require identical element types even for empty slices", () => {
  const vm = byteVM();
  const { registry, byte } = byteTypes();
  const slices = ["A", "B"].map((name) => {
    const identity = { module_path: registry.module, decl_id: name };
    registry.nodes.set(name, { id: name, kind: Kind.Named, underlying: byte.ref });
    const element = registry.runtime({
      kind: Kind.Named,
      primitive: 0,
      named: identity,
      node: name,
    });
    const slice = registry.runtime({
      kind: Kind.Slice,
      primitive: 0,
      named: identity,
      node: `slice${name}`,
    });
    slice.dynamic = { kind: Kind.Slice, elem: element };
    return { type: slice, kind: "nil" };
  });
  for (const id of ["reflect.value_copy", "reflect.value_append_slice"]) {
    const result = reflectedCall(
      vm,
      registry,
      id,
      slices.map((current) => ({
        type: current.type,
        kind: "nil",
        metadata: { current },
      })),
    );
    assert.match(result.error, /matching.*element types/);
  }
});

test("append of no values to a nil byte slice preserves nil", () => {
  const vm = byteVM();
  const { slice } = byteTypes();
  const nil = { type: slice, kind: "nil" };
  assert.equal(vm.append({}, [nil]), nil);
});

test("byte append grows one Uint8Array backing with exact storage cost", () => {
  const vm = byteVM();
  const { slice, byte } = byteTypes();
  const nil = { type: slice, kind: "nil" };
  const result = vm.append({}, [nil, uint(1, byte), uint(2, byte), uint(3, byte)]);
  assert.equal(result.kind, "slice");
  const backing = result.value.storage.root.value;
  assert.equal(backing.kind, "bytes");
  assert.deepEqual([...backing.value], [1, 2, 3]);
  assert.equal(result.value.capacity, 3);
  assert.equal(
    vm.guestMemory.stats().totalGuestAllocatedBytes,
    2n * (GUEST_COST.node + 3n * GUEST_COST.byte),
  );
});

test("uint8 slices use the byte backing bulk path", () => {
  const vm = byteVM();
  const { slice, byte } = byteTypes();
  const values = [uint(9, byte), uint(10, byte)];
  const result = vm.sliceFromValues(slice, values);
  assert.equal(result.value.storage.root.value.kind, "bytes");
  assert.deepEqual([...result.value.storage.root.value.value], [9, 10]);
});

test("expanded self append copies overlapping byte ranges correctly", () => {
  const vm = byteVM();
  const { slice } = byteTypes();
  const root = cell({ type: slice, kind: "bytes", value: Uint8Array.from([1, 2, 0, 0]) });
  const source = {
    type: slice,
    kind: "slice",
    value: { storage: { root, path: [] }, start: 0, length: 2, capacity: 4 },
  };
  const result = vm.append({ expand: true }, [source, source]);
  assert.deepEqual([...result.value.storage.root.value.value], [1, 2, 1, 2]);
  assert.equal(result.value.length, 4);
});

test("byte append preserves array backing aliases when a byte view is unavailable", () => {
  const vm = byteVM();
  const { slice, byte } = byteTypes();
  const root = cell({
    type: slice,
    kind: "array",
    value: [uint(1, byte), uint(0, byte), uint(0, byte)],
  });
  const source = {
    type: slice,
    kind: "slice",
    value: { storage: { root, path: [] }, start: 0, length: 1, capacity: 3 },
  };
  const result = vm.append({}, [source, uint(2, byte)]);
  assert.equal(result.value.storage.root, root);
  assert.deepEqual(
    result.value.storage.root.value.value.map((value) => value.value),
    [1n, 2n, 0n],
  );
});

test("byte backing budget failure happens before append mutates the input", () => {
  const vm = byteVM();
  vm.guestMemory = new GuestMemory({ maxHeapBytes: 1n });
  const { slice, byte } = byteTypes();
  const original = {
    type: slice,
    kind: "slice",
    value: {
      storage: {
        root: cell({ type: slice, kind: "bytes", value: Uint8Array.from([7, 0]) }),
        path: [],
      },
      start: 0,
      length: 1,
      capacity: 1,
    },
  };
  assert.throws(() => vm.append({}, [original, uint(8, byte)]), /heap byte limit/);
  assert.deepEqual([...original.value.storage.root.value.value], [7, 0]);
  assert.equal(original.value.length, 1);
});

test("capacity growth cannot bypass the sequence limit", () => {
  const vm = byteVM();
  vm.options.limits.maxSequenceElements = 3;
  const { slice, byte } = byteTypes();
  const root = cell({ type: slice, kind: "bytes", value: Uint8Array.from([7, 0]) });
  const original = {
    type: slice,
    kind: "slice",
    value: { storage: { root, path: [] }, start: 0, length: 2, capacity: 2 },
  };
  assert.throws(() => vm.append({}, [original, uint(8, byte)]), /sequence element limit/);
  assert.deepEqual([...root.value.value], [7, 0]);
  assert.equal(original.value.length, 2);
});

test("append reserves the output root before mutating shared capacity", () => {
  const vm = byteVM();
  vm.guestMemory = new GuestMemory({ maxAllocatedBytes: GUEST_COST.node - 1n });
  const { slice, byte } = byteTypes();
  const root = cell({ type: slice, kind: "bytes", value: Uint8Array.from([7, 0]) });
  const source = {
    type: slice,
    kind: "slice",
    value: { storage: { root, path: [] }, start: 0, length: 1, capacity: 2 },
  };
  const frame = {
    slots: [source, uint(8, byte), undefined],
    inputScratch: [],
    guestFrameBytes: 0n,
    guestSlotRoots: new Uint8Array(3),
  };
  const instruction = {
    opcode: 38,
    descriptor: {},
    arity: [2, 1],
    input0: [0, 0],
    input1: [0, 1],
    output0: 2,
    operands: {
      inputs: [
        [0, 0],
        [0, 1],
      ],
      outputs: [2],
    },
  };
  assert.throws(() => vm.execute({}, frame, instruction), /logical allocation limit/);
  assert.deepEqual([...root.value.value], [7, 0]);
  assert.equal(frame.slots[2], undefined);
});

test("shallow depth limits keep a new byte root on the census path", () => {
  const vm = byteVM();
  vm.options.limits.maxValueDepth = 1;
  const { slice, byte } = byteTypes();
  vm.append({}, [{ type: slice, kind: "nil" }, uint(1, byte)]);
  assert.equal(vm.guestMemory.needsCensus(), true);
});

test("byte copy uses the bulk path without marking the graph dirty", () => {
  const vm = byteVM();
  const { registry, slice } = byteTypes();
  const root = cell({ type: slice, kind: "bytes", value: Uint8Array.from([0, 0, 0]) });
  const destination = {
    type: slice,
    kind: "slice",
    value: { storage: { root, path: [] }, start: 0, length: 3, capacity: 3 },
  };
  const stringType = primitive(registry, PrimitiveKind.PrimitiveString);
  vm.guestMemory.publish({ liveHeapBytes: 0n, liveHeapObjects: 0n, liveGuestBytes: 0n }, true);
  assert.equal(
    vm.copy(destination, { type: stringType, kind: "string", value: Uint8Array.from([4, 5]) }),
    2,
  );
  assert.deepEqual([...root.value.value], [4, 5, 0]);
  assert.equal(vm.guestMemory.needsCensus(), false);
});
