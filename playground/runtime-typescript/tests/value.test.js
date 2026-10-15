import assert from "node:assert/strict";
import { test } from "vitest";
import { Kind, PrimitiveKind } from "../src/vm/contract.generated";
import {
  TypeRegistry,
  boolValue,
  cell,
  cloneValue,
  createGuestMap,
  decodeRune,
  guestMapDelete,
  guestMapGet,
  guestMapSet,
  guestEqual,
  importHostValue,
  nilValue,
  normalize,
  primitive,
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

test("normalization reuses static integer widths", () => {
  const { int32 } = values();
  const value = normalize({ type: int32, kind: "int", value: 0x1_0000_0001n });
  assert.equal(value.value, 1n);
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
    const backing = slice.storage.root.value.value;
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

test("guest map indexing preserves update, deletion, and iterator identity", () => {
  const { registry, int, float, string } = values();
  const map = createGuestMap();
  const first = { type: int, kind: "int", value: 7n };
  const second = { type: int, kind: "int", value: 7n };
  const firstEntry = guestMapSet(map, first, boolValue(registry, true));
  assert.equal(guestMapGet(map, second)?.value.value, true);
  assert.equal(map.entries.length, 1);
  const updatedEntry = guestMapSet(map, second, boolValue(registry, false));
  assert.equal(updatedEntry, firstEntry);
  assert.equal(guestMapGet(map, first)?.value.value, false);

  const zero = { type: float, kind: "float", value: -0 };
  const positiveZero = { type: float, kind: "float", value: 0 };
  guestMapSet(map, zero, boolValue(registry, false));
  assert.equal(guestMapGet(map, positiveZero)?.value.value, false);

  assert.equal(guestMapDelete(map, second), true);
  assert.equal(guestMapGet(map, first), undefined);
  const deletedEntry = guestMapSet(map, first, boolValue(registry, true));
  assert.notEqual(deletedEntry, firstEntry);
  assert.equal(map.entries.includes(firstEntry), false);

  const snapshot = [...map.entries];
  assert.equal(guestMapDelete(map, first), true);
  assert.equal(snapshot.includes(deletedEntry), true);
  assert.equal(map.entries.includes(deletedEntry), false);

  const text = new TextEncoder().encode("shared");
  const original = { type: string, kind: "string", value: text };
  const cloned = cloneValue(original);
  assert.notEqual(cloned, original);
  assert.equal(cloned.value, text);
  assert.equal(guestEqual(original, cloned), true);

  const nan = { type: float, kind: "float", value: Number.NaN };
  const otherNan = { type: float, kind: "float", value: Number.NaN };
  const entriesBeforeNaN = map.entries.length;
  guestMapSet(map, nan, boolValue(registry, true));
  guestMapSet(map, otherNan, boolValue(registry, false));
  assert.equal(map.entries.length, entriesBeforeNaN + 2);
  assert.equal(
    [...map.buckets.values()].some((bucket) => bucket.size === 2),
    true,
  );
  assert.equal(guestMapGet(map, nan), undefined);

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
  guestMapSet(map, arrayKey, boolValue(registry, true));
  assert.equal(guestMapGet(map, equalArrayKey)?.value.value, true);

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
  guestMapSet(map, structKey, boolValue(registry, true));
  assert.equal(guestMapGet(map, equalStructKey)?.value.value, true);

  const bytesType = registry.runtime({
    kind: Kind.Slice,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "bytes-key",
  });
  const bytes = { type: bytesType, kind: "bytes", value: new Uint8Array([1, 2]) };
  const equalBytes = { type: bytesType, kind: "bytes", value: new Uint8Array([1, 2]) };
  guestMapSet(map, bytes, boolValue(registry, true));
  assert.equal(guestMapGet(map, equalBytes)?.value.value, true);

  const channelType = registry.runtime({
    kind: Kind.Waitable,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "channel-key",
  });
  const channel = { capacity: 0, closed: false, queue: [], senders: [], receivers: [] };
  const channelKey = { type: channelType, kind: "channel", value: channel };
  const equalChannelKey = { type: channelType, kind: "channel", value: channel };
  guestMapSet(map, channelKey, boolValue(registry, true));
  assert.equal(guestMapGet(map, equalChannelKey)?.value.value, true);

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
  guestMapSet(map, namedKey, boolValue(registry, true));
  assert.equal(guestMapGet(map, equalNamedKey)?.value.value, true);

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
  guestMapSet(map, boxedKey, boolValue(registry, true));
  assert.equal(guestMapGet(map, equalBoxedKey)?.value.value, true);

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
  guestMapSet(map, nilKey, boolValue(registry, true));
  assert.equal(guestMapGet(map, equalNilKey)?.value.value, true);

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
  guestMapSet(map, pointerWithView, boolValue(registry, true));
  assert.equal(guestMapGet(map, pointerWithNoPath)?.value.value, true);
  assert.equal(guestMapGet(map, pointerWithIndex), undefined);
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

  const map = createGuestMap();
  guestMapSet(map, cloned, boolValue(registry, true));
  assert.equal(
    guestMapGet(map, { type: string, kind: "string", value: new Uint8Array([97]) })?.value.value,
    true,
  );
});
