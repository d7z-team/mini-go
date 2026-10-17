import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "vitest";
import {
  STORAGE_COST,
  STORAGE_PRIMITIVE_BYTES,
  checkedStorageBytes,
  PrimitiveKind,
  Kind,
} from "../src/vm/contract.generated";
import { VirtualMachine } from "../src/vm/vm";
import { intValue, primitive } from "../src/vm/value";
import { prepareArrayStorageLayout } from "../src/vm/aggregate-storage";
import { runtimeVector } from "./support/runtime-vectors.js";

const vectors = JSON.parse(
  readFileSync(new URL("../../../testdata/runtime/storage_cost.json", import.meta.url), "utf8"),
);

test.each(vectors)("managed allocation cost: $name", async (vector) => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {});
  try {
    const registry = [...vm.revision.program.modules.values()][0].registry;
    const integer = intValue(registry, 42n);
    const capture = vm.managedCell(integer);
    const arrayType = vm.dynamicType(registry, { kind: Kind.Array, elem: integer.type, length: 0 });
    vm.dynamicType(registry, { kind: Kind.Pointer, elem: integer.type });
    const channelType = vm.dynamicType(registry, { kind: Kind.Waitable, elem: integer.type });
    const mapType = vm.dynamicType(registry, {
      kind: Kind.Map,
      key: integer.type,
      elem: integer.type,
    });
    const sliceElement = primitive(
      registry,
      vector.name === "byte_slice_backing"
        ? PrimitiveKind.PrimitiveUint8
        : PrimitiveKind.PrimitiveInt,
    );
    const sliceType = vm.dynamicType(registry, { kind: Kind.Slice, elem: sliceElement });
    const before = vm.stats();
    switch (vector.name) {
      case "bool_cell":
        vm.managedCell({
          type: primitive(registry, PrimitiveKind.PrimitiveBool),
          kind: "bool",
          value: true,
        });
        break;
      case "int_cell":
        vm.managedCell(integer);
        break;
      case "empty_array_cell": {
        vm.managedCell({ type: arrayType, kind: "array", value: [] });
        break;
      }
      case "string_cell":
        vm.managedCell({
          type: primitive(registry, PrimitiveKind.PrimitiveString),
          kind: "string",
          value: new TextEncoder().encode("abc"),
        });
        break;
      case "byte_slice_backing":
      case "int_slice_backing": {
        const bytes = vector.name === "byte_slice_backing";
        vm.managedCell(
          bytes
            ? { type: sliceType, kind: "bytes", value: new Uint8Array([1, 2, 3]) }
            : { type: sliceType, kind: "array", value: [integer, integer, integer] },
          prepareArrayStorageLayout(sliceElement, 3),
        );
        break;
      }
      case "function":
      case "closure":
      case "bound_method":
        vm.storeFunction({
          type: primitive(registry, PrimitiveKind.PrimitiveFunction),
          kind: "function",
          value: {
            module: "main",
            function: "fn.entry",
            captures: vector.name === "closure" ? [{ root: capture, path: [] }] : [],
            ...(vector.name === "bound_method" ? { receiver: integer } : {}),
          },
        });
        break;
      case "rendezvous_channel":
      case "buffered_channel":
        vm.createChannel(channelType, vector.name === "buffered_channel" ? 2 : 0);
        break;
      case "empty_map":
      case "populated_map": {
        const map = vm.createMap(mapType);
        if (vector.name === "populated_map") map.set(integer, integer);
        break;
      }
      default:
        assert.fail(`unknown allocation vector ${vector.name}`);
    }
    const after = vm.stats();
    assert.equal(after.totalAllocatedBytes - before.totalAllocatedBytes, BigInt(vector.bytes));
    assert.equal(after.totalAllocations - before.totalAllocations, BigInt(vector.allocations));
  } finally {
    vm.close();
    vm.pump(1);
  }
});

test("storage cost contract uses the shared logical widths", () => {
  assert.deepEqual(STORAGE_COST, {
    version: 1,
    byte: 1,
    scalar: 8,
    reference: 8,
    dynamic: 24,
    string: 24,
    slice: 32,
    pointer: 24,
    record_header: 16,
    frame_header: 32,
    control_record: 64,
    map_bucket: 4,
    map_entry: 16,
    map_header: 32,
    channel_header: 24,
  });
  assert.equal(STORAGE_PRIMITIVE_BYTES[PrimitiveKind.PrimitiveBool], 1);
  assert.equal(STORAGE_PRIMITIVE_BYTES[PrimitiveKind.PrimitiveString], 24);
  assert.equal(STORAGE_PRIMITIVE_BYTES[PrimitiveKind.PrimitiveComplex128], 16);
  assert.equal(STORAGE_PRIMITIVE_BYTES[PrimitiveKind.PrimitiveFunction], 8);
});

test("storage cost contract rejects overflow before admission", () => {
  assert.equal(checkedStorageBytes(16n, 7n, 8n), 72n);
  assert.equal(checkedStorageBytes(16n, 2n ** 64n - 1n, 0n), 16n);
  assert.equal(checkedStorageBytes(0n, 2n ** 64n - 1n, 8n), undefined);
  assert.equal(checkedStorageBytes(-1n, 0n, 0n), undefined);
});
