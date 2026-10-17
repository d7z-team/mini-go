import assert from "node:assert/strict";
import { test } from "vitest";
import { Kind, PrimitiveKind } from "../src/vm/contract.generated";
import { prepareStorageLayout } from "../src/vm/aggregate-storage";
import {
  advanceAddress,
  TypeRegistry,
  assign,
  boolValue,
  cell,
  cloneValue,
  decodeRune,
  dereference,
  guestEqual,
  hashValue,
  importHostValue,
  nilValue,
  normalize,
  primitive,
  snapshot,
  storageAddress,
  stringValue,
} from "../src/vm/value";
import { convert } from "../src/vm/operators";

function values() {
  const registry = new TypeRegistry("value-test", undefined);
  return {
    registry,
    int: primitive(registry, PrimitiveKind.PrimitiveInt),
    int32: primitive(registry, PrimitiveKind.PrimitiveInt32),
    float: primitive(registry, PrimitiveKind.PrimitiveFloat64),
    string: primitive(registry, PrimitiveKind.PrimitiveString),
  };
}

test("managed addresses use handle and byte offset for equality, hashing, and snapshots", () => {
  const { registry, int } = values();
  const empty = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "Empty" },
    node: "empty",
  });
  empty.dynamic = { kind: Kind.Struct, fields: [] };
  const pair = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "Pair" },
    node: "pair",
  });
  pair.dynamic = {
    kind: Kind.Struct,
    fields: [
      { name: "Count", type: int, tag: "", embedded: false },
      { name: "Tail", type: empty, tag: "", embedded: false },
    ],
  };
  const layout = prepareStorageLayout(pair);
  const handle = { slot: 31, generation: 7 };
  const root = cell({
    type: pair,
    kind: "struct",
    value: new Map([
      ["Count", { type: int, kind: "int", value: 3n }],
      ["Tail", { type: empty, kind: "struct", value: new Map() }],
    ]),
  });
  root.managedHandle = handle;
  root.managedLocation = { handle, offset: 0, type: pair, layout };
  let directWrite;
  root.readLocation = (location) =>
    location.offset === layout.fields[0].offset
      ? { type: int, kind: "int", value: 17n }
      : undefined;
  root.writeLocation = (location, value) => {
    if (location.offset !== layout.fields[0].offset) return false;
    directWrite = value;
    return true;
  };
  const base = { root, path: [] };
  const count = advanceAddress(base, { kind: "field", name: "Count" });
  const sameCount = {
    root,
    path: [],
    location: {
      handle,
      offset: layout.fields[0].offset,
      type: int,
      layout: prepareStorageLayout(int),
    },
  };
  assert.equal(dereference({ root, path: [{ kind: "field", name: "Count" }] }).value, 17n);
  assign(
    { root, path: [{ kind: "field", name: "Count" }] },
    { type: int, kind: "int", value: 23n },
  );
  assert.equal(directWrite.value, 23n);
  const tail = advanceAddress(base, { kind: "field", name: "Tail" });
  assert.deepEqual(storageAddress(count), storageAddress(sameCount));
  assert.equal(
    guestEqual(
      { type: int, kind: "pointer", value: count },
      { type: int, kind: "pointer", value: sameCount },
    ),
    true,
  );
  assert.equal(
    hashValue({ type: int, kind: "pointer", value: count }),
    hashValue({ type: int, kind: "pointer", value: sameCount }),
  );
  assert.equal(storageAddress(tail).offset, layout.fields[1].offset);

  const encoded = snapshot([{ type: int, kind: "pointer", value: count }]);
  assert.deepEqual(encoded.roots[0].data, {
    Pointer: { object: 0n, path: [{ Field: "Count" }] },
  });
  assert.equal(encoded.objects.length, 1);
});

test("normalization reuses static integer widths", () => {
  const { int32 } = values();
  const value = normalize({ type: int32, kind: "int", value: 0x1_0000_0001n });
  assert.equal(value.value, 1n);
});

test("nested pointer and slice writes reach their owning cells", () => {
  const { registry, int } = values();
  const structType = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "Pair" },
    node: "pair",
  });
  structType.dynamic = {
    kind: Kind.Struct,
    fields: [
      { name: "value", type: int, tag: "", embedded: false },
      { name: "next", type: int, tag: "", embedded: false },
    ],
  };
  const pointerType = registry.runtime({
    kind: Kind.Pointer,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "",
  });
  const pair = (value, next) => ({
    type: structType,
    kind: "struct",
    value: new Map([
      ["value", { type: int, kind: "int", value: BigInt(value) }],
      ["next", { type: int, kind: "int", value: BigInt(next) }],
    ]),
  });
  const target = cell(pair(1, 2));
  const outer = cell({
    type: structType,
    kind: "struct",
    value: new Map([
      ["value", { type: int, kind: "int", value: 0n }],
      [
        "next",
        {
          type: pointerType,
          kind: "pointer",
          value: { root: target, path: [] },
        },
      ],
    ]),
  });
  assign(
    {
      root: outer,
      path: [
        { kind: "field", name: "next" },
        { kind: "field", name: "value" },
      ],
    },
    { type: int, kind: "int", value: 9n },
  );
  assert.equal(dereference({ root: target, path: [{ kind: "field", name: "value" }] }).value, 9n);
  assert.equal(dereference({ root: outer, path: [{ kind: "field", name: "value" }] }).value, 0n);

  const arrayType = registry.runtime({
    kind: Kind.Array,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "pair-array",
  });
  arrayType.dynamic = { kind: Kind.Array, elem: structType, length: 2 };
  const backing = cell({
    type: arrayType,
    kind: "array",
    value: [pair(3, 4), pair(5, 6)],
  });
  const sliceType = registry.runtime({
    kind: Kind.Slice,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "pair-slice",
  });
  sliceType.dynamic = { kind: Kind.Slice, elem: structType };
  const slice = cell({
    type: sliceType,
    kind: "slice",
    value: { storage: { root: backing, path: [] }, start: 0, length: 2, capacity: 2 },
  });
  assign(
    {
      root: slice,
      path: [
        { kind: "index", index: 1 },
        { kind: "field", name: "next" },
      ],
    },
    { type: int, kind: "int", value: 11n },
  );
  assert.equal(
    dereference({
      root: backing,
      path: [
        { kind: "index", index: 1 },
        { kind: "field", name: "next" },
      ],
    }).value,
    11n,
  );
});

test("decodeRune consumes one byte for invalid and truncated UTF-8", () => {
  const cases = [
    { name: "leading continuation", bytes: [0x80], runes: [0xfffd], widths: [1] },
    { name: "truncated sequence", bytes: [0xe2, 0x82], runes: [0xfffd, 0xfffd], widths: [1, 1] },
    {
      name: "invalid continuation",
      bytes: [0xe2, 0x28, 0xa1],
      runes: [0xfffd, 0x28, 0xfffd],
      widths: [1, 1, 1],
    },
    { name: "overlong encoding", bytes: [0xc0, 0x80], runes: [0xfffd, 0xfffd], widths: [1, 1] },
    {
      name: "valid supplementary rune",
      bytes: [0xf0, 0x9f, 0x98, 0x80],
      runes: [0x1f600],
      widths: [4],
    },
  ];
  for (const item of cases) {
    const bytes = Uint8Array.from(item.bytes);
    const runes = [];
    const widths = [];
    for (let offset = 0; offset < bytes.length; ) {
      const [rune, width] = decodeRune(bytes, offset);
      runes.push(rune);
      widths.push(width);
      offset += width;
    }
    assert.deepEqual(runes, item.runes, item.name);
    assert.deepEqual(widths, item.widths, `${item.name} widths`);
  }
});

test("Int32 to String applies Go rune replacement semantics", () => {
  const { int32, string } = values();
  const cases = [
    { input: -1n, expected: "\ufffd" },
    { input: 0xd800n, expected: "\ufffd" },
    { input: 0x110000n, expected: "\ufffd" },
    { input: 0x1f600n, expected: "😀" },
  ];
  for (const item of cases) {
    const converted = convert({ type: int32, kind: "int", value: item.input }, string);
    assert.deepEqual([...converted.value], [...new TextEncoder().encode(item.expected)]);
  }
});

test("String to rune slice follows the unicode_rune_range cases", () => {
  const { registry, string, int32 } = values();
  const runeSlice = registry.runtime({
    kind: Kind.Slice,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "rune-slice",
  });
  runeSlice.dynamic = { kind: Kind.Slice, elem: int32 };
  const cases = [
    { bytes: new TextEncoder().encode("A界😀B"), runes: [65n, 30028n, 128512n, 66n] },
    { bytes: Uint8Array.from([0xff, 0x41, 0x80]), runes: [0xfffdn, 65n, 0xfffdn] },
  ];
  for (const item of cases) {
    const converted = convert({ type: string, kind: "string", value: item.bytes }, runeSlice);
    assert.equal(converted.kind, "slice");
    const slice = converted.value;
    const backing = slice.storage.root.read().value;
    assert.deepEqual(
      backing.map((value) => value.value),
      item.runes,
    );
    assert.equal(slice.length, item.runes.length);
  }
});

test("nil byte and rune slices convert to an empty string", () => {
  const { registry, string } = values();
  for (const kind of [PrimitiveKind.PrimitiveUint8, PrimitiveKind.PrimitiveInt32]) {
    const type = registry.runtime({
      kind: Kind.Slice,
      primitive: 0,
      named: { module_path: "", decl_id: "" },
      node: `slice-${kind}`,
    });
    type.dynamic = { kind: Kind.Slice, elem: primitive(registry, kind) };
    const result = convert(nilValue(type), string);
    assert.equal(result.kind, "string");
    assert.deepEqual([...result.value], []);
  }
});

test("identical uses primitive and named fast paths", () => {
  const { registry } = values();
  assert.equal(
    registry.identical(
      primitive(registry, PrimitiveKind.PrimitiveInt),
      primitive(registry, PrimitiveKind.PrimitiveInt),
    ),
    true,
  );
  assert.equal(
    registry.identical(
      registry.runtime({
        kind: Kind.Named,
        primitive: 0,
        named: { module_path: "m", decl_id: "T" },
        node: "",
      }),
      registry.runtime({
        kind: Kind.Named,
        primitive: 0,
        named: { module_path: "m", decl_id: "T" },
        node: "",
      }),
    ),
    true,
  );
});

test("comparable value equality agrees with hashing", () => {
  const { registry, int, string } = values();
  const first = { type: int, kind: "int", value: 7n };
  const equalKeys = (left, right) => {
    assert.equal(guestEqual(left, right), true);
    assert.equal(hashValue(left), hashValue(right));
  };
  const arrayType = registry.runtime({
    kind: Kind.Array,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "array-key",
  });
  const arrayKey = { type: arrayType, kind: "array", value: [first] };
  const equalArrayKey = {
    type: arrayType,
    kind: "array",
    value: [{ type: int, kind: "int", value: 7n }],
  };
  equalKeys(arrayKey, equalArrayKey);

  const structType = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "struct-key",
  });
  const structKey = {
    type: structType,
    kind: "struct",
    value: new Map([
      ["_", { type: int, kind: "int", value: 1n }],
      ["value", first],
    ]),
  };
  const equalStructKey = {
    type: structType,
    kind: "struct",
    value: new Map([
      ["_", { type: int, kind: "int", value: 2n }],
      ["value", { type: int, kind: "int", value: 7n }],
    ]),
  };
  equalKeys(structKey, equalStructKey);

  const channelType = registry.runtime({
    kind: Kind.Waitable,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "channel-key",
  });
  const channel = { capacity: 0, closed: false, queue: [], senders: [], receivers: [] };
  const channelKey = { type: channelType, kind: "channel", value: channel };
  const equalChannelKey = { type: channelType, kind: "channel", value: channel };
  equalKeys(channelKey, equalChannelKey);

  const namedType = registry.runtime({
    kind: Kind.Named,
    primitive: 0,
    named: { module_path: "sample", decl_id: "Key" },
    node: "",
  });
  const namedKey = { type: namedType, kind: "int", value: 9n };
  const equalNamedKey = {
    type: registry.runtime({
      kind: Kind.Named,
      primitive: 0,
      named: { module_path: "sample", decl_id: "Key" },
      node: "",
    }),
    kind: "int",
    value: 9n,
  };
  equalKeys(namedKey, equalNamedKey);

  const anyType = registry.runtime({
    kind: Kind.Any,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "",
  });
  const boxedKey = { type: anyType, kind: "interface", value: namedKey };
  const equalBoxedKey = {
    type: anyType,
    kind: "interface",
    value: { type: equalNamedKey.type, kind: "int", value: 9n },
  };
  assert.equal(guestEqual(boxedKey, namedKey), true);
  equalKeys(boxedKey, equalBoxedKey);

  const nilType = registry.runtime({
    kind: Kind.Pointer,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "nil-key",
  });
  const nilKey = nilValue(nilType);
  const equalNilKey = nilValue(
    registry.runtime({
      kind: Kind.Pointer,
      primitive: 0,
      named: { module_path: "", decl_id: "" },
      node: "nil-key",
    }),
  );
  equalKeys(nilKey, equalNilKey);

  const pointerType = registry.runtime({
    kind: Kind.Pointer,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "",
  });
  const root = cell(first);
  const pointerWithView = {
    type: pointerType,
    kind: "pointer",
    value: { root, path: [{ kind: "typeView", type: int }] },
  };
  const pointerWithIndex = {
    type: pointerType,
    kind: "pointer",
    value: { root, path: [{ kind: "index", index: 3 }] },
  };
  const pointerWithNoPath = { type: pointerType, kind: "pointer", value: { root, path: [] } };
  const pointerWithViewAndIndex = {
    type: pointerType,
    kind: "pointer",
    value: {
      root,
      path: [
        { kind: "typeView", type: int },
        { kind: "index", index: 3 },
      ],
    },
  };
  const pointerWithOtherIndex = {
    type: pointerType,
    kind: "pointer",
    value: { root, path: [{ kind: "index", index: 4 }] },
  };
  assert.equal(guestEqual(pointerWithView, pointerWithNoPath), true);
  assert.equal(guestEqual(pointerWithView, pointerWithIndex), false);
  assert.equal(guestEqual(pointerWithViewAndIndex, pointerWithIndex), true);
  assert.equal(guestEqual(pointerWithIndex, pointerWithOtherIndex), false);
  equalKeys(pointerWithView, pointerWithNoPath);
  equalKeys(pointerWithViewAndIndex, pointerWithIndex);
  const text = new TextEncoder().encode("shared");
  const original = { type: string, kind: "string", value: text };
  equalKeys(original, cloneValue(original));
});

test("host and byte conversions own mutable byte input while guest clones share", () => {
  const { registry, string } = values();
  const source = new Uint8Array([97]);
  const owned = stringValue(registry, source);
  source[0] = 98;
  assert.deepEqual([...owned.value], [97]);

  const hostBytes = new Uint8Array([97]);
  const imported = importHostValue(
    { typ: { Primitive: PrimitiveKind.PrimitiveString }, data: { String: hostBytes } },
    registry,
  );
  hostBytes[0] = 98;
  assert.deepEqual([...imported.value], [97]);

  const raw = { type: string, kind: "string", value: new Uint8Array([97]) };
  const cloned = cloneValue(raw);
  assert.equal(cloned.value, raw.value);

  const sliceType = registry.runtime({
    kind: Kind.Slice,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "mutable-byte-slice",
  });
  sliceType.dynamic = {
    kind: Kind.Slice,
    elem: primitive(registry, PrimitiveKind.PrimitiveUint8),
  };
  const mutableBacking = new Uint8Array([97]);
  const byteSlice = {
    type: sliceType,
    kind: "slice",
    value: {
      storage: { root: cell({ type: sliceType, kind: "bytes", value: mutableBacking }), path: [] },
      start: 0,
      length: 1,
      capacity: 1,
    },
  };
  const converted = convert(byteSlice, string);
  mutableBacking[0] = 98;
  assert.deepEqual([...converted.value], [97]);

  assert.equal(
    guestEqual(cloned, { type: string, kind: "string", value: new Uint8Array([97]) }),
    true,
  );
});

test("host type identities preserve pointer and slice element descriptors", () => {
  const { registry, int32 } = values();
  const slice = importHostValue(
    { typ: { Slice: { Primitive: PrimitiveKind.PrimitiveInt32 } }, data: "Nil" },
    registry,
  );
  const pointer = importHostValue(
    { typ: { Pointer: { Primitive: PrimitiveKind.PrimitiveInt32 } }, data: "Nil" },
    registry,
  );
  assert.equal(registry.underlying(slice.type).ref.kind, Kind.Slice);
  assert.equal(registry.element(slice.type), int32);
  assert.equal(registry.underlying(pointer.type).ref.kind, Kind.Pointer);
  assert.equal(registry.element(pointer.type), int32);
});
