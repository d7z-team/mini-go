import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "vitest";
import { Kind, PrimitiveKind } from "../src/vm/contract.generated";
import { FrameStorage } from "../src/vm/frame-storage";
import { dereference, intValue, nilValue, primitive } from "../src/vm/value";
import { VirtualMachine } from "../src/vm/vm";
import { runtimeVector } from "./support/runtime-vectors.js";

test.each(["reserve", "build", "publish"])(
  "allocation failure during %s leaves no partially published payload",
  async (phase) => {
    const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
    try {
      const module = [...vm.revision.program.moduleValues()][0];
      const value = intValue(module.registry, 7n);
      const before = vm.stats();
      vm.testAllocationFault = { phase, remaining: 1 };
      assert.throws(() => vm.managedCell(value), { code: "injected_fault" });
      const after = vm.stats();
      assert.equal(after.occupiedBytes, before.occupiedBytes);
      assert.equal(after.reservedBytes, before.reservedBytes);
      delete vm.testAllocationFault;
      assert.equal(vm.managedCell(value).read().value, 7n);
    } finally {
      vm.close();
      vm.pump(1);
    }
  },
);

test("failed map growth releases nested string backing for an immediate retry", async () => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
  try {
    const registry = [...vm.revision.program.moduleValues()][0].registry;
    const int = primitive(registry, PrimitiveKind.PrimitiveInt);
    const string = primitive(registry, PrimitiveKind.PrimitiveString);
    const mapType = registry.runtime({
      kind: Kind.Map,
      primitive: 0,
      named: { module_path: "", decl_id: "" },
      node: "",
    });
    mapType.dynamic = { kind: Kind.Map, key: int, elem: string };
    const map = vm.createMap(mapType);
    const text = (value) => ({
      type: string,
      kind: "string",
      value: new TextEncoder().encode(value),
    });
    for (let index = 0; index < 64; index++)
      map.set(intValue(registry, BigInt(index)), text(`value-${index}`));
    const before = vm.stats();
    // The first reserve creates the new value's string backing. The second
    // reserve is the map growth allocation; both must remain unpublished.
    vm.testAllocationFault = { phase: "reserve", remaining: 2 };
    assert.throws(() => map.set(intValue(registry, 64n), text("retry")), {
      code: "injected_fault",
    });
    delete vm.testAllocationFault;
    const failed = vm.stats();
    assert.equal(map.size, 64);
    assert.equal(failed.occupiedBytes, before.occupiedBytes);
    assert.equal(failed.reservedBytes, before.reservedBytes);
    assert.equal(map.get(intValue(registry, 63n)).value.value instanceof Uint8Array, true);
    map.set(intValue(registry, 64n), text("retry"));
    assert.equal(map.size, 65);
    assert.equal(new TextDecoder().decode(map.get(intValue(registry, 64n)).value.value), "retry");
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("task admission failure does not publish a scope or task", async () => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
  try {
    vm.testAllocationFault = { phase: "reserve", remaining: 1 };
    assert.throws(() => vm.start("default", []), { code: "injected_fault" });
    assert.equal(vm.stats().tasks, 0n);
    assert.equal(vm.stats().activeScopes, 0n);
    delete vm.testAllocationFault;
    assert.doesNotThrow(() => vm.start("default", []));
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("first frame admission failure aborts its task reservation", async () => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
  try {
    assert.equal(vm.pump(4096).ready, true);
    const before = vm.stats();
    vm.testAllocationFault = { phase: "reserve", remaining: 2 };
    assert.throws(() => vm.start("default", []), { code: "injected_fault" });
    const after = vm.stats();
    assert.equal(after.totalAllocatedBytes, before.totalAllocatedBytes);
    assert.equal(after.totalAllocations, before.totalAllocations);
    assert.equal(after.occupiedBytes, before.occupiedBytes);
    assert.equal(after.reservedBytes, 0n);
    assert.equal(after.tasks, 0n);
    assert.equal(after.activeScopes, 0n);
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test.each(
  ["memory_pointer.json", "memory_pointer_view.json"].flatMap((name) =>
    [128, 512].map((limit) => [name, limit]),
  ),
)("%s admits its initial task and frame atomically at a %i byte limit", async (name, limit) => {
  const vectors = JSON.parse(
    readFileSync(new URL("../../../testdata/runtime/state.json", import.meta.url), "utf8"),
  );
  const vector = vectors.find((item) => item.name === name && item.limit === limit);
  assert.ok(vector);
  const vm = await VirtualMachine.create(new TextEncoder().encode(JSON.stringify(vector.image)), {
    limits: { maxGuestBytes: vector.limit },
  });
  try {
    assert.equal(vm.pump(4096).ready, true);
    if (limit === 128) assert.throws(() => vm.start("default", []), { code: "allocation_limit" });
    else vm.start("default", []);
    const stats = vm.stats();
    assert.deepEqual(
      [stats.occupiedBytes, stats.reservedBytes, stats.totalAllocatedBytes],
      vector.actions[0].memory.slice(0, 3).map(BigInt),
    );
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("a live empty map keeps its layouts when another map is reclaimed", async () => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
  try {
    const registry = [...vm.revision.program.moduleValues()][0].registry;
    const int = primitive(registry, PrimitiveKind.PrimitiveInt);
    const type = registry.runtime({
      kind: Kind.Map,
      primitive: 0,
      named: { module_path: "", decl_id: "" },
      node: "",
    });
    type.dynamic = { kind: Kind.Map, key: int, elem: int };
    const old = vm.createMap(type);
    old.set(intValue(registry, 1n), intValue(registry, 7n));
    const live = vm.createMap(type);
    vm.allocationRoots.length = 0;
    vm.allocationRoots.push(live.handle);
    vm.collectHeap(true);
    assert.equal(vm.guestMemory.managedHeap.has(old.handle), false);
    live.set(intValue(registry, 2n), intValue(registry, 42n));
    assert.equal(live.get(intValue(registry, 2n)).value.value, 42n);
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test.each(["reserveControl", "reserveResident"])(
  "iterator replacement preserves the previous owner when %s fails",
  async (reservation) => {
    const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
    const module = [...vm.revision.program.moduleValues()][0];
    const frame = { fn: { module } };
    try {
      const registry = module.registry;
      const int = primitive(registry, PrimitiveKind.PrimitiveInt);
      const mapType = registry.runtime({
        kind: Kind.Map,
        primitive: 0,
        named: { module_path: "", decl_id: "" },
        node: "",
      });
      mapType.dynamic = { kind: Kind.Map, key: int, elem: int };
      const map = vm.createMap(mapType);
      map.set(intValue(registry, 7n), intValue(registry, 42n));
      const value = { type: mapType, kind: "map", value: map };
      vm.mapIterInit(frame, 0, value);
      const previous = frame.mapIterators[0];
      const before = vm.stats();
      const reserve = vm[reservation];
      const failure = new Error("injected iterator admission failure");
      vm[reservation] = () => {
        throw failure;
      };
      assert.throws(() => vm.mapIterInit(frame, 0, value), failure);
      vm[reservation] = reserve;
      assert.equal(frame.mapIterators[0], previous);
      assert.ok(previous.memory);
      assert.ok(previous.resident);
      assert.equal(vm.stats().occupiedBytes, before.occupiedBytes);
      assert.equal(vm.stats().reservedBytes, before.reservedBytes);
      assert.equal(vm.stats().residentPageBytes, before.residentPageBytes);
      assert.equal(vm.mapIterNext(frame, { iteratorLocal: 0 })[1].value, 42n);
    } finally {
      vm.releaseMapIterators(frame);
      vm.close();
      vm.pump(1);
    }
  },
);

test("major collection releases storage type metadata after its payload dies", async () => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
  try {
    const registry = [...vm.revision.program.moduleValues()][0].registry;
    const any = registry.runtime({
      kind: Kind.Any,
      primitive: 0,
      named: { module_path: "", decl_id: "" },
      node: "",
    });
    const value = { type: any, kind: "interface", value: intValue(registry, 9n) };
    const cell = vm.managedCell(value);
    assert.ok(vm.storageTypes.some(Boolean));
    vm.storageOwner.release(cell.managedHandle);
    vm.collectHeap(true);
    assert.equal(vm.storageTypes.filter(Boolean).length, 0);
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("major collection reclaims an unowned descriptor while another task is active", async () => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
  try {
    const registry = [...vm.revision.program.moduleValues()][0].registry;
    const any = registry.runtime({
      kind: Kind.Any,
      primitive: 0,
      named: { module_path: "", decl_id: "" },
      node: "",
    });
    const cell = vm.managedCell({ type: any, kind: "interface", value: intValue(registry, 11n) });
    vm.start("default", []);
    vm.storageOwner.release(cell.managedHandle);
    vm.collectHeap(true);
    assert.equal(vm.storageTypes.filter(Boolean).length, 0);
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("construction pin survives a major collection before payload publication", async () => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
  try {
    const registry = [...vm.revision.program.moduleValues()][0].registry;
    const any = registry.runtime({
      kind: Kind.Any,
      primitive: 0,
      named: { module_path: "", decl_id: "" },
      node: "",
    });
    const allocate = vm.allocatePayload;
    vm.allocatePayload = function (...args) {
      this.collectHeap(true);
      return allocate.apply(this, args);
    };
    const cell = vm.managedCell({ type: any, kind: "interface", value: intValue(registry, 13n) });
    assert.equal(cell.read().value.value, 13n);
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test.each(
  ["map", "channel", "cell", "frame"].flatMap((container) =>
    [false, true].map((typedNil) => [container, typedNil]),
  ),
)(
  "%s interface storage retains a concrete pointer with typedNil=%s",
  async (container, typedNil) => {
    const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
    try {
      const registry = [...vm.revision.program.moduleValues()][0].registry;
      const int = primitive(registry, PrimitiveKind.PrimitiveInt);
      const any = vm.dynamicType(registry, { kind: Kind.Any });
      const pointerType = vm.dynamicType(registry, { kind: Kind.Pointer, elem: int });
      const root = vm.managedCell(intValue(registry, 42n));
      const pointer = typedNil
        ? nilValue(pointerType)
        : { type: pointerType, kind: "pointer", value: { root, path: [] } };
      let read;
      if (container === "map") {
        const type = vm.dynamicType(registry, { kind: Kind.Map, key: int, elem: any });
        const map = vm.createMap(type);
        map.set(intValue(registry, 7n), pointer);
        read = () => map.get(intValue(registry, 7n)).value;
      } else if (container === "channel") {
        const type = vm.dynamicType(registry, { kind: Kind.Waitable, elem: any });
        const channel = vm.createChannel(type, 1);
        channel.push(pointer);
        read = () => channel.shift();
      } else if (container === "cell") {
        const cell = vm.managedCell(nilValue(any));
        cell.write(pointer);
        read = () => cell.read();
      } else {
        const frame = new FrameStorage([any], vm.storageOwner);
        assert.equal(frame.write(0, pointer), true);
        read = () => frame.read(0);
      }
      const value = read();
      assert.equal(value.kind, "interface");
      assert.equal(value.value.kind, typedNil ? "nil" : "pointer");
      assert.equal(registry.identical(value.value.type, pointerType), true);
      if (!typedNil) assert.equal(dereference(value.value.value).value, 42n);
    } finally {
      vm.close();
      vm.pump(1);
    }
  },
);

test.each(["build", "publish"])(
  "map and channel construction aborts before owner publication on %s failure",
  async (phase) => {
    const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
    try {
      const registry = [...vm.revision.program.moduleValues()][0].registry;
      const int = primitive(registry, PrimitiveKind.PrimitiveInt);
      const mapType = registry.runtime({
        kind: Kind.Map,
        primitive: 0,
        named: { module_path: "", decl_id: "" },
        node: "",
      });
      mapType.dynamic = { kind: Kind.Map, key: int, elem: int };
      const channelType = registry.runtime({
        kind: Kind.Waitable,
        primitive: 0,
        named: { module_path: "", decl_id: "" },
        node: "",
      });
      channelType.dynamic = { kind: Kind.Waitable, elem: int };
      const before = vm.stats();
      vm.testAllocationFault = { phase, remaining: 1 };
      assert.throws(() => vm.createMap(mapType), { code: "injected_fault" });
      vm.testAllocationFault = { phase, remaining: 1 };
      assert.throws(() => vm.createChannel(channelType, 2), { code: "injected_fault" });
      const after = vm.stats();
      assert.equal(after.occupiedBytes, before.occupiedBytes);
      assert.equal(after.reservedBytes, before.reservedBytes);
      delete vm.testAllocationFault;
      assert.ok(vm.createMap(mapType).handle.slot > 0);
      assert.ok(vm.createChannel(channelType, 2).handle.slot > 0);
    } finally {
      vm.close();
      vm.pump(1);
    }
  },
);
