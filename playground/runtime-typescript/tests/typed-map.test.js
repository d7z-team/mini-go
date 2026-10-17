import assert from "node:assert/strict";
import { test } from "vitest";
import { Kind, PrimitiveKind } from "../src/vm/contract.generated";
import { prepareStorageLayout } from "../src/vm/aggregate-storage";
import { TypedMap } from "../src/vm/typed-map";
import { cell, primitive, storageAddress } from "../src/vm/value";

import { storageFixture } from "./support/storage-owner";

function mapFixture(keyKind = PrimitiveKind.PrimitiveInt, valueKind = PrimitiveKind.PrimitiveInt) {
  const { registry, heap, owner, failAllocation } = storageFixture();
  const keyType = primitive(registry, keyKind);
  const valueType = primitive(registry, valueKind);
  const map = new TypedMap(
    owner,
    keyType,
    valueType,
    prepareStorageLayout(keyType),
    prepareStorageLayout(valueType),
  );
  heap.addRoot(map.handle);
  return {
    heap,
    map,
    failAllocation,
    key(value) {
      return {
        type: keyType,
        kind: keyKind === PrimitiveKind.PrimitiveFloat64 ? "float" : "int",
        value,
      };
    },
    value(value) {
      return {
        type: valueType,
        kind: valueKind === PrimitiveKind.PrimitiveString ? "string" : "int",
        value,
      };
    },
  };
}

test("typed map retains payload after input mutation and segmented growth", () => {
  const { heap, map, key, value } = mapFixture();
  const inputKey = key(4n);
  const inputValue = value(40n);
  const first = map.set(inputKey, inputValue);
  inputKey.value = 99n;
  inputValue.value = 99n;
  assert.equal(map.get(key(4n)).value.value, 40n);
  for (let index = 0; index < 300; index++) map.set(key(BigInt(index)), value(BigInt(index * 10)));
  heap.collectMajor();
  assert.equal(map.size, 300);
  assert.equal(map.entry(first.index, first.generation).value.value, 40n);
  for (let index = 0; index < 300; index++)
    assert.equal(map.get(key(BigInt(index))).value.value, BigInt(index * 10));
});

test("numeric iterator identities skip reinserted entries and observe updates", () => {
  const { map, key, value } = mapFixture();
  const first = map.set(key(1n), value(10n));
  const second = map.set(key(2n), value(20n));
  const snapshot = map.snapshot();
  assert.deepEqual([...snapshot], [first.index, first.generation, second.index, second.generation]);
  map.delete(key(1n));
  const replacement = map.set(key(3n), value(30n));
  assert.equal(replacement.index, first.index);
  assert.notEqual(replacement.generation, first.generation);
  map.set(key(2n), value(42n));
  assert.equal(map.entry(snapshot[0], snapshot[1]), undefined);
  assert.equal(map.entry(snapshot[2], snapshot[3]).value.value, 42n);
  map.clear();
  map.set(key(2n), value(99n));
  assert.equal(map.entry(second.index, second.generation), undefined);
});

test("pointer keys compare by managed slot and offset after page decoding", () => {
  const { registry, heap, owner } = storageFixture();
  const int = primitive(registry, PrimitiveKind.PrimitiveInt);
  const pointer = registry.runtime({
    kind: Kind.Pointer,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "pointer-key",
  });
  pointer.dynamic = { kind: Kind.Pointer, elem: int };
  const map = new TypedMap(
    owner,
    pointer,
    int,
    prepareStorageLayout(pointer),
    prepareStorageLayout(int),
  );
  const scalarLayout = heap.registerLayout({ stride: prepareStorageLayout(int).size });
  const handle = heap.reserve(scalarLayout, 1, 24n).commit();
  const root = cell({ type: int, kind: "int", value: 4n });
  root.managedHandle = handle;
  root.managedLocation = {
    handle,
    offset: 0,
    type: int,
    layout: prepareStorageLayout(int),
  };
  const equivalentRoot = cell({ type: int, kind: "int", value: 99n });
  equivalentRoot.managedHandle = handle;
  equivalentRoot.managedLocation = root.managedLocation;
  const first = {
    type: pointer,
    kind: "pointer",
    value: { root, path: [] },
  };
  const equivalent = {
    type: pointer,
    kind: "pointer",
    value: { root: equivalentRoot, path: [] },
  };
  assert.deepEqual(storageAddress(first.value), storageAddress(equivalent.value));
  map.set(first, { type: int, kind: "int", value: 41n });
  assert.equal(map.size, 1);
  assert.equal(map.get(equivalent).value.value, 41n);
  map.close();
});

test("float map keys preserve signed zero and independent NaN entries", () => {
  const { map, key, value } = mapFixture(PrimitiveKind.PrimitiveFloat64);
  map.set(key(-0), value(1n));
  map.set(key(0), value(2n));
  assert.equal(map.size, 1);
  assert.equal(map.get(key(-0)).value.value, 2n);
  map.set(key(Number.NaN), value(3n));
  map.set(key(Number.NaN), value(4n));
  assert.equal(map.size, 3);
  assert.equal(map.get(key(Number.NaN)), undefined);
  assert.deepEqual([...map.entries()].map((entry) => entry.value.value).sort(), [2n, 3n, 4n]);
});

test("typed map cards retain young string backing and clear releases it", () => {
  const { heap, map, key, value } = mapFixture(
    PrimitiveKind.PrimitiveInt,
    PrimitiveKind.PrimitiveString,
  );
  map.set(key(1n), value(new TextEncoder().encode("first")));
  heap.collectMajor();
  heap.collectMajor();
  map.set(key(1n), value(new TextEncoder().encode("updated")));
  heap.collectMinor();
  assert.equal(new TextDecoder().decode(map.get(key(1n)).value.value), "updated");
  const occupied = heap.stats().occupiedBytes;
  map.clear();
  heap.collectMajor();
  assert.equal(map.size, 0);
  assert.ok(heap.stats().occupiedBytes < occupied);
});

test("map allocation failure preserves the existing table before rehash publication", () => {
  const { heap, map, key, value, failAllocation } = mapFixture();
  for (let index = 0; index < 64; index++) map.set(key(BigInt(index)), value(BigInt(index)));
  const snapshot = map.snapshot();
  const before = heap.stats();
  failAllocation(2);
  assert.throws(() => map.set(key(100n), value(100n)), /injected allocation failure/);
  assert.equal(map.size, 64);
  assert.deepEqual(map.snapshot(), snapshot);
  assert.equal(heap.stats().occupiedBytes, before.occupiedBytes);
  assert.equal(heap.stats().reservedBytes, before.reservedBytes);
  heap.collectMajor();
  for (let index = 0; index < 64; index++)
    assert.equal(map.get(key(BigInt(index))).value.value, BigInt(index));
  map.set(key(100n), value(100n));
  assert.equal(map.get(key(100n)).value.value, 100n);
});
