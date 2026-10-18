import assert from "node:assert/strict";
import { test } from "vitest";
import { Kind, PrimitiveKind } from "../src/vm/contract.generated";
import {
  decodeStorage,
  encodeStorage,
  fieldLocation,
  indexLocation,
  pathForStorageOffset,
  packedField,
  packedIndex,
  prepareStorageLayout,
  resolveStorageLocation,
  writePackedField,
} from "../src/vm/aggregate-storage";
import { TypeRegistry, primitive, nilValue, cloneValue } from "../src/vm/value";
import { FrameStorage } from "../src/vm/frame-storage";
import { storageFixture } from "./support/storage-owner.js";

test("zero-sized direct locations retain their target layout", () => {
  const registry = new TypeRegistry("empty-address", undefined);
  const empty = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "",
  });
  empty.dynamic = { kind: Kind.Struct, fields: [] };
  const outer = {
    ...empty,
    dynamic: {
      kind: Kind.Struct,
      fields: [
        { name: "Count", type: primitive(registry, PrimitiveKind.PrimitiveInt) },
        { name: "Tail", type: empty },
      ],
    },
  };
  const layout = prepareStorageLayout(outer);
  const handle = { slot: 1, generation: 1 };
  const tail = resolveStorageLocation({ handle, offset: 0, type: outer, layout }, layout, [
    { kind: "field", name: "Tail" },
  ]);
  assert.equal(tail.offset, layout.size);
  assert.equal(tail.layout, layout.fields[1].layout);
  assert.deepEqual(pathForStorageOffset(layout, outer, empty, layout.size), [
    { kind: "field", name: "Tail" },
  ]);
  const array = {
    ...empty,
    ref: { ...empty.ref, kind: Kind.Array },
    dynamic: { kind: Kind.Array, elem: empty, length: 2 },
  };
  const arrayLayout = prepareStorageLayout(array);
  const first = resolveStorageLocation(
    { handle, offset: 0, type: array, layout: arrayLayout },
    arrayLayout,
    [{ kind: "index", index: 0 }],
  );
  assert.equal(first.layout, arrayLayout.element);
  assert.deepEqual(pathForStorageOffset(arrayLayout, array, empty, 0), [
    { kind: "index", index: 0 },
  ]);
});

test("dynamic payload distinguishes an empty interface, typed nil and exact scalar bits", () => {
  const registry = new TypeRegistry("dynamic-storage", undefined);
  const any = registry.runtime({
    kind: Kind.Any,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "",
  });
  const pointer = registry.runtime({
    kind: Kind.Pointer,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "",
  });
  pointer.dynamic = { kind: Kind.Pointer, elem: primitive(registry, PrimitiveKind.PrimitiveInt) };
  const layout = prepareStorageLayout(any);
  assert.deepEqual(layout.references, []);
  assert.deepEqual(layout.dynamics, [0]);
  const types = [];
  const context = {
    typeID(type) {
      types.push(type);
      return types.length;
    },
    typeByID(id) {
      return types[id - 1];
    },
    writeReference() {
      assert.fail("scalar and typed nil do not have heap edges");
    },
    readReference() {
      assert.fail("scalar and typed nil do not read heap edges");
    },
  };
  const bytes = new Uint8Array(layout.size);
  for (const concrete of [
    nilValue(pointer),
    {
      type: primitive(registry, PrimitiveKind.PrimitiveInt64),
      kind: "int",
      value: -0x8000_0000_0000_0000n,
    },
    {
      type: primitive(registry, PrimitiveKind.PrimitiveUint64),
      kind: "uint",
      value: 0xffff_ffff_ffff_ffffn,
    },
    { type: primitive(registry, PrimitiveKind.PrimitiveFloat64), kind: "float", value: -0 },
    {
      type: primitive(registry, PrimitiveKind.PrimitiveComplex128),
      kind: "complex",
      value: { real: 1.25, imag: -4.5 },
    },
  ]) {
    bytes.fill(0xa5);
    encodeStorage({ type: any, kind: "interface", value: concrete }, layout, bytes, 0, context);
    const result = decodeStorage(any, layout, bytes, 0, context);
    assert.equal(result.kind, "interface");
    assert.deepEqual(result.value, concrete);
  }
  assert.deepEqual(
    decodeStorage(any, layout, new Uint8Array(layout.size), 0, context),
    nilValue(any),
  );
});

test("encoding an empty dynamic value clears a reused payload lane", () => {
  const registry = new TypeRegistry("dynamic-clear", undefined);
  const any = registry.runtime({
    kind: Kind.Any,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "",
  });
  const layout = prepareStorageLayout(any);
  const bytes = new Uint8Array(layout.size).fill(0xff);
  encodeStorage({ type: any, kind: "nil" }, layout, bytes, 0, {
    writeReference() {},
    readReference() {},
  });
  assert.deepEqual([...bytes], new Array(layout.size).fill(0));
});

test("direct codec clears reused slice and omitted struct fields", () => {
  const { registry, owner } = storageFixture();
  let encodeCalls = 0;
  const encode = owner.encode;
  owner.encode = (...args) => {
    encodeCalls++;
    return encode(...args);
  };
  const int = primitive(registry, PrimitiveKind.PrimitiveInt32);
  const slice = registry.runtime({
    kind: Kind.Slice,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "bytes",
  });
  slice.dynamic = { kind: Kind.Slice, elem: int };
  const layout = prepareStorageLayout(slice);
  const location = { handle: { slot: 7, generation: 3 }, offset: 0, type: slice };
  const root = { managedLocation: { ...location, layout } };
  const bytes = new Uint8Array(layout.size).fill(0xa5);
  const nonNil = {
    type: slice,
    kind: "slice",
    value: { storage: { root, path: [] }, start: 0, length: 1, capacity: 1 },
  };
  owner.encodeInto(nonNil, layout, slice, bytes);
  assert.equal(new DataView(bytes.buffer).getUint32(16, true), 1);
  owner.encodeInto(nilValue(slice), layout, slice, bytes);
  assert.deepEqual([...bytes], new Array(layout.size).fill(0));

  const row = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "Row" },
    node: "row",
  });
  row.dynamic = {
    kind: Kind.Struct,
    fields: [{ name: "data", type: slice, tag: "", embedded: false }],
  };
  const rowLayout = prepareStorageLayout(row);
  const rowBytes = new Uint8Array(rowLayout.size).fill(0xa5);
  owner.encodeInto(
    {
      type: row,
      kind: "struct",
      value: new Map([["data", nonNil]]),
    },
    rowLayout,
    row,
    rowBytes,
  );
  owner.encodeInto({ type: row, kind: "struct", value: new Map() }, rowLayout, row, rowBytes);
  assert.deepEqual([...rowBytes], new Array(rowLayout.size).fill(0));
  assert.equal(encodeCalls, 0, "direct codec must not allocate a second encoded buffer");
});

test("aggregate interface lanes box a pointer assigned through the interface type", () => {
  const registry = new TypeRegistry("dynamic-pointer", undefined);
  const int = primitive(registry, PrimitiveKind.PrimitiveInt32);
  const pointer = registry.runtime({
    kind: Kind.Pointer,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "pointer",
  });
  pointer.dynamic = { kind: Kind.Pointer, elem: int };
  const any = registry.runtime({
    kind: Kind.Any,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "any",
  });
  const array = registry.runtime({
    kind: Kind.Array,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "array",
  });
  array.dynamic = { kind: Kind.Array, elem: any, length: 1 };
  const handle = { slot: 7, generation: 3 };
  const pointerValue = {
    type: pointer,
    kind: "pointer",
    value: { root: {}, path: [] },
  };
  const types = [pointer];
  const bytes = new Uint8Array(prepareStorageLayout(array).size);
  encodeStorage(
    { type: array, kind: "array", value: [pointerValue] },
    prepareStorageLayout(array),
    bytes,
    0,
    {
      typeID: (type) => {
        assert.equal(type, pointer);
        return 1;
      },
      encodeBox: (value) => {
        assert.equal(value, pointerValue);
        return handle;
      },
      writeReference(offset, value) {
        if (value) new DataView(bytes.buffer).setUint32(offset, value.slot, true);
      },
      readReference: () => handle,
    },
  );
  assert.equal(new DataView(bytes.buffer).getUint32(0, true), 1);
  assert.equal(new DataView(bytes.buffer).getUint32(4, true), 5);
  assert.equal(new DataView(bytes.buffer).getUint32(8, true), handle.slot);
  const decoded = decodeStorage(array, prepareStorageLayout(array), bytes, 0, {
    typeByID: (id) => types[id - 1],
    readReference: () => handle,
    writeReference() {},
    decodeBox: () => pointerValue,
  });
  assert.equal(decoded.value[0].kind, "interface");
  assert.equal(decoded.value[0].value, pointerValue);
});

test("typed aggregate layout encodes nested arrays without a Value graph", () => {
  const registry = new TypeRegistry("storage-test", undefined);
  const int = primitive(registry, PrimitiveKind.PrimitiveInt32);
  const pair = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "Pair" },
    node: "pair",
  });
  pair.dynamic = {
    kind: Kind.Struct,
    fields: [
      { name: "left", type: int, tag: "", embedded: false },
      { name: "right", type: int, tag: "", embedded: false },
    ],
  };
  const array = registry.runtime({
    kind: Kind.Array,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "pair-array",
  });
  array.dynamic = { kind: Kind.Array, elem: pair, length: 2 };
  const layout = prepareStorageLayout(array);
  assert.equal(layout.size, 16);
  assert.deepEqual(layout.references, []);
  const rootLocation = { handle: { slot: 4, generation: 2 }, offset: 10, type: array };
  const second = indexLocation(rootLocation, layout, 1);
  assert.equal(second.offset, 18);
  assert.equal(fieldLocation(second, layout.element, "right").offset, 22);
  const values = [1, 2].map((base) => ({
    type: pair,
    kind: "struct",
    value: new Map([
      ["left", { type: int, kind: "int", value: BigInt(base) }],
      ["right", { type: int, kind: "int", value: BigInt(base + 10) }],
    ]),
  }));
  const input = { type: array, kind: "array", value: values };
  const bytes = new Uint8Array(layout.size);
  const refs = new Map();
  const context = {
    writeReference(offset, handle) {
      if (handle) refs.set(offset, handle);
    },
    readReference(offset) {
      return refs.get(offset);
    },
  };
  encodeStorage(input, layout, bytes, 0, context);
  const decoded = decodeStorage(array, layout, bytes, 0, context);
  assert.deepEqual(
    decoded.value.map((item) => [...item.value].map(([, value]) => value.value)),
    [
      [1n, 11n],
      [2n, 12n],
    ],
  );
});

test("dynamic typed zero keeps an aggregate inline without a boxed heap edge", () => {
  const registry = new TypeRegistry("dynamic-zero", undefined);
  const int = primitive(registry, PrimitiveKind.PrimitiveInt32);
  const pair = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "Pair" },
    node: "pair",
  });
  pair.dynamic = {
    kind: Kind.Struct,
    fields: [
      { name: "left", type: int, tag: "", embedded: false },
      { name: "right", type: int, tag: "", embedded: false },
    ],
  };
  const any = registry.runtime({
    kind: Kind.Any,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "",
  });
  const layout = prepareStorageLayout(any);
  const values = new Map([
    ["left", { type: int, kind: "int", value: 0n }],
    ["right", { type: int, kind: "int", value: 0n }],
  ]);
  const types = [pair];
  const bytes = new Uint8Array(layout.size);
  const context = {
    typeID: () => 1,
    typeByID: (id) => types[id - 1],
    writeReference: () => assert.fail("typed zero must not write a reference"),
    readReference: () => assert.fail("typed zero must not read a reference"),
  };
  encodeStorage(
    { type: any, kind: "interface", value: { type: pair, kind: "struct", value: values } },
    layout,
    bytes,
    0,
    context,
  );
  assert.equal(new DataView(bytes.buffer).getUint32(4, true), 3);
  const decoded = decodeStorage(any, layout, bytes, 0, context);
  assert.equal(decoded.kind, "interface");
  assert.equal(decoded.value.kind, "struct");
  assert.deepEqual(
    [...decoded.value.value].map(([name, value]) => [name, value.value]),
    [
      ["left", 0n],
      ["right", 0n],
    ],
  );
});

test("physical aggregate locations round-trip through an address path", () => {
  const registry = new TypeRegistry("storage-location-test", undefined);
  const int = primitive(registry, PrimitiveKind.PrimitiveInt32);
  const pair = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "Pair" },
    node: "pair",
  });
  pair.dynamic = {
    kind: Kind.Struct,
    fields: [
      { name: "left", type: int, tag: "", embedded: false },
      { name: "right", type: int, tag: "", embedded: false },
    ],
  };
  const array = registry.runtime({
    kind: Kind.Array,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "pair-array",
  });
  array.dynamic = { kind: Kind.Array, elem: pair, length: 2 };
  const layout = prepareStorageLayout(array);
  const root = { handle: { slot: 9, generation: 3 }, offset: 0, type: array };
  const location = resolveStorageLocation(root, layout, [
    { kind: "index", index: 1 },
    { kind: "field", name: "right" },
  ]);
  assert.equal(location.offset, 12);
  assert.equal(registry.identical(location.type, int), true);
  assert.deepEqual(pathForStorageOffset(layout, array, int, location.offset), [
    { kind: "index", index: 1 },
    { kind: "field", name: "right" },
  ]);
});

test("frame aggregates expose direct packed field and index views", () => {
  const registry = new TypeRegistry("packed-frame", undefined);
  const int = primitive(registry, PrimitiveKind.PrimitiveInt32);
  const pair = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "Pair" },
    node: "pair",
  });
  pair.dynamic = {
    kind: Kind.Struct,
    fields: [
      { name: "left", type: int, tag: "", embedded: false },
      { name: "right", type: int, tag: "", embedded: false },
    ],
  };
  const array = registry.runtime({
    kind: Kind.Array,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "array",
  });
  array.dynamic = { kind: Kind.Array, elem: pair, length: 2 };
  const fixture = storageFixture();
  const storage = new FrameStorage([array], fixture.owner);
  const value = {
    type: array,
    kind: "array",
    value: [
      {
        type: pair,
        kind: "struct",
        value: new Map([
          ["left", { type: int, kind: "int", value: 7n }],
          ["right", { type: int, kind: "int", value: 8n }],
        ]),
      },
      {
        type: pair,
        kind: "struct",
        value: new Map([
          ["left", { type: int, kind: "int", value: 9n }],
          ["right", { type: int, kind: "int", value: 10n }],
        ]),
      },
    ],
  };
  storage.write(0, value);
  const packed = storage.read(0);
  assert.equal(packed.value instanceof Array, false);
  const second = packedIndex(packed, 1);
  assert.equal(packedField(second, "right").value, 10n);
  storage.write(0, storage.read(0));
  assert.equal(packedField(packedIndex(storage.read(0), 1), "right").value, 10n);
  const copied = cloneValue(second);
  assert.equal(writePackedField(second, "right", { type: int, kind: "int", value: 42n }), true);
  assert.equal(packedField(second, "right").value, 42n);
  assert.equal(packedField(copied, "right").value, 10n);
  storage.reset();
  assert.equal(packedField(copied, "right").value, 10n);
});

test("frame classification uses logical width while preserving padded physical lanes", () => {
  const registry = new TypeRegistry("logical-inline", undefined);
  const bool = primitive(registry, PrimitiveKind.PrimitiveBool);
  const string = primitive(registry, PrimitiveKind.PrimitiveString);
  const row = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "Row" },
    node: "row",
  });
  row.dynamic = {
    kind: Kind.Struct,
    fields: [
      { name: "flag", type: bool, tag: "", embedded: false },
      { name: "text", type: string, tag: "", embedded: false },
    ],
  };
  const array = registry.runtime({
    kind: Kind.Array,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "rows",
  });
  array.dynamic = { kind: Kind.Array, elem: row, length: 5 };
  const layout = prepareStorageLayout(array);
  // Each row is physically 32 bytes because the string is word aligned, but
  // its logical width is one bool plus one string descriptor: 25 bytes.
  assert.equal(layout.size, 160);
  assert.equal(layout.logical, 125n);
  assert.equal(FrameStorage.logicalByteLength([layout]), 125n);
  assert.equal(FrameStorage.byteLength([array], [layout]), 166);

  const fixture = storageFixture();
  let copyAggregateCalls = 0;
  const copyAggregate = fixture.owner.copyAggregate;
  fixture.owner.copyAggregate = (value) => {
    copyAggregateCalls++;
    return copyAggregate(value);
  };
  const storage = new FrameStorage([array], fixture.owner, [layout]);
  const text = (value) => ({
    type: string,
    kind: "string",
    value: new TextEncoder().encode(value),
  });
  const rowValue = (value) => ({
    type: row,
    kind: "struct",
    value: new Map([
      ["flag", { type: bool, kind: "bool", value: false }],
      ["text", text(value)],
    ]),
  });
  storage.write(0, {
    type: array,
    kind: "array",
    value: ["a", "b", "c", "d", "e"].map(rowValue),
  });
  assert.equal(copyAggregateCalls, 0, "logical-inline aggregate must stay in the frame bank");
  assert.equal([...storage.allReferences()].length, 5);

  const first = packedIndex(storage.read(0), 0);
  const copied = cloneValue(first);
  assert.equal(copyAggregateCalls, 1, "only explicit value copy should create a managed aggregate");
  const copiedHandle = copied.value.storage.handle;
  assert.deepEqual([...packedField(first, "text").value], [97]);
  assert.equal(writePackedField(first, "flag", { type: bool, kind: "bool", value: true }), true);
  assert.equal(packedField(first, "flag").value, true);
  assert.equal(packedField(copied, "flag").value, false);

  fixture.heap.collectMajor([...storage.allReferences(), copiedHandle]);
  assert.deepEqual([...packedField(first, "text").value], [97]);
  assert.deepEqual([...packedField(copied, "text").value], [97]);
});

test("frame writes use the prepared payload as the encode destination", () => {
  const { registry, owner } = storageFixture();
  const int = primitive(registry, PrimitiveKind.PrimitiveInt32);
  let encodeIntoCalls = 0;
  const encode = owner.encode;
  owner.encodeInto = (value, layout, type, target) => {
    encodeIntoCalls++;
    const encoded = encode(value, layout, type);
    target.set(encoded);
    return target;
  };
  const storage = new FrameStorage([int], owner, [prepareStorageLayout(int)]);
  storage.write(0, { type: int, kind: "int", value: 42n });
  assert.equal(encodeIntoCalls, 0, "scalar lanes bypass aggregate encoding");

  const text = primitive(registry, PrimitiveKind.PrimitiveString);
  const row = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "Row" },
    node: "row",
  });
  row.dynamic = {
    kind: Kind.Struct,
    fields: [{ name: "text", type: text, tag: "", embedded: false }],
  };
  const aggregate = new FrameStorage([row], owner, [prepareStorageLayout(row)]);
  aggregate.write(0, {
    type: row,
    kind: "struct",
    value: new Map([
      ["text", { type: text, kind: "string", value: new TextEncoder().encode("ok") }],
    ]),
  });
  assert.equal(encodeIntoCalls, 1);
});

test("large frame arrays own independent managed storage across frame reuse and collection", () => {
  const { registry, owner, heap } = storageFixture();
  const int = primitive(registry, PrimitiveKind.PrimitiveInt64);
  const type = registry.runtime({
    kind: Kind.Array,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "large-array",
  });
  type.dynamic = { kind: Kind.Array, elem: int, length: 64 };
  const layout = prepareStorageLayout(type);
  assert.equal(FrameStorage.logicalByteLength([layout]), 8n);
  const source = new FrameStorage([type], owner);
  source.initializeZero(0);
  assert.equal(packedIndex(source.read(0), 63).value, 0n);
  source.write(0, {
    type,
    kind: "array",
    value: Array.from({ length: 64 }, (_, index) => ({
      type: int,
      kind: "int",
      value: BigInt(index),
    })),
  });
  const target = new FrameStorage([type], owner);
  target.write(0, source.read(0));
  source.reset();
  for (const major of [false, true]) {
    if (major) heap.collectMajor(target.allReferences());
    else heap.collectMinor(target.allReferences());
    assert.equal(packedIndex(target.read(0), 63).value, 63n);
  }
});
