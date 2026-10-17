import assert from "node:assert/strict";
import { test } from "vitest";
import { ManagedHeap } from "../src/vm/managed-heap";

function allocate(heap, layout, logical = BigInt(layout.stride), initialize = false) {
  const reservation = heap.reserve(layout.id, layout.count ?? 1, logical);
  if (initialize) reservation.bytes().fill(0xab);
  return reservation.commit();
}

function layout(heap, stride, references = [], dynamics = [], count = 1) {
  return { id: heap.registerLayout({ stride, references, dynamics }), stride, count };
}

function reference(heap, parent, offset, child) {
  heap.writeReference(parent, offset, child);
}

test("old allocation pressure follows promotion, control owners and reclamation", () => {
  const heap = new ManagedHeap();
  const record = layout(heap, 8);
  const parent = allocate(heap, record);
  heap.addRoot(parent);
  const abandoned = heap.reserve(record.id, 1, 8n);
  abandoned.abort();
  assert.equal(heap.oldGuestBytes, 0n);
  heap.collectMinor();
  assert.equal(heap.oldGuestBytes, 0n);
  heap.collectMinor();
  assert.equal(heap.oldGuestBytes, 8n);
  const control = heap.reserveControl(64n).commit();
  assert.equal(heap.oldGuestBytes, 72n);
  control.release();
  assert.equal(heap.oldGuestBytes, 8n);
  const temporary = allocate(heap, record);
  assert.equal(heap.oldGuestBytes, 8n);
  heap.release(temporary);
  heap.removeRoot(parent);
  heap.collectMajor();
  assert.equal(heap.oldGuestBytes, 0n);
  assert.equal(heap.occupiedGuestBytes, 0n);
  heap.close();
  assert.equal(heap.oldGuestBytes, 0n);
});

test("old card tracing preserves an old to young to young chain", () => {
  const heap = new ManagedHeap();
  const record = layout(heap, 8, [0]);
  const parent = allocate(heap, record);
  heap.addRoot(parent);
  heap.collectMinor();
  heap.collectMinor();
  assert.equal(heap.get(parent).old, true);

  const child = allocate(heap, record);
  const grandchild = allocate(heap, record);
  reference(heap, parent, 0, child);
  reference(heap, child, 0, grandchild);
  heap.collectMinor();
  assert.equal(heap.get(child).bytes, 8n);
  assert.equal(heap.get(grandchild).bytes, 8n);
});

test("cycles survive copying collection and handles reject reused generations", () => {
  const heap = new ManagedHeap();
  const record = layout(heap, 16, [0, 8]);
  const first = allocate(heap, record);
  const second = allocate(heap, record);
  reference(heap, first, 0, second);
  reference(heap, second, 0, first);
  heap.addRoot(first);
  heap.collectMinor();
  assert.deepEqual(heap.readReference(first, 0), second);
  heap.release(second);
  assert.throws(() => heap.get(second), /stale managed handle/);
  const reused = allocate(heap, record);
  assert.equal(reused.slot, second.slot);
  assert.notEqual(reused.generation, second.generation);
  assert.throws(() => heap.get(second), /stale managed handle/);
});

test("random graph collection agrees with an independent reachability oracle", () => {
  const heap = new ManagedHeap();
  const record = layout(heap, 16, [0, 8]);
  const count = 192;
  const objects = Array.from({ length: count }, () => allocate(heap, record));
  let seed = 0x9e3779b9;
  const next = () => {
    seed ^= seed << 13;
    seed ^= seed >>> 17;
    seed ^= seed << 5;
    return seed >>> 0;
  };
  const graph = objects.map(() => []);
  for (let index = 0; index < count; index++) {
    for (let edge = 0; edge < 2; edge++) {
      const target = next() % count;
      graph[index].push(target);
      reference(heap, objects[index], edge * 8, objects[target]);
    }
  }
  const roots = [0, 17, 17, 91, 150, 191];
  const reachable = new Uint8Array(count);
  const visit = (index) => {
    if (reachable[index]) return;
    reachable[index] = 1;
    for (const child of graph[index]) visit(child);
  };
  for (const root of roots) visit(root);
  heap.collectMinor(roots.map((index) => objects[index]));
  for (let index = 0; index < count; index++) {
    if (reachable[index]) assert.doesNotThrow(() => heap.get(objects[index]));
    else assert.throws(() => heap.get(objects[index]), /stale managed handle/);
  }
});

test("card scanning clips a large backing record to the dirty card", () => {
  const heap = new ManagedHeap();
  const record = layout(heap, 16, [0], [], 1024);
  const backing = allocate(heap, record, 16n * 1024n);
  heap.addRoot(backing);
  heap.collectMinor();
  heap.collectMinor();
  assert.equal(heap.get(backing).old, true);
  const child = allocate(heap, layout(heap, 8));
  const elementOffset = 80 * 16;
  reference(heap, backing, elementOffset, child);
  const before = heap.stats();
  heap.collectMinor();
  const after = heap.stats();
  assert.doesNotThrow(() => heap.get(child));
  assert.equal(after.scannedCards, before.scannedCards + 1n);
});

test("dynamic Ref and Box representations trace while scalar values do not", () => {
  const heap = new ManagedHeap();
  const record = layout(heap, 32, [], [0]);
  const parent = allocate(heap, record);
  const child = allocate(heap, record);
  const bytes = heap.read(parent);
  const view = new DataView(bytes.buffer);
  view.setUint32(4, 4, true);
  view.setUint32(8, child.slot, true);
  view.setUint32(12, child.generation, true);
  heap.write(parent, bytes);
  heap.addRoot(parent);
  heap.collectMinor();
  assert.doesNotThrow(() => heap.get(child));
});

test("a dynamic tag write remembers a ticket in the adjacent card", () => {
  const heap = new ManagedHeap();
  const record = layout(heap, 512, [], [248]);
  const parent = allocate(heap, record);
  heap.collectMinor([parent]);
  heap.collectMinor([parent]);
  assert.equal(heap.get(parent).old, true);
  const child = allocate(heap, record);
  reference(heap, parent, 256, child);
  // Clear the ticket's card while its representation is not a reference.
  heap.collectMinor([parent, child]);
  heap.writeScalar(parent, 252, "u32", 4);
  heap.collectMinor([parent]);
  assert.doesNotThrow(() => heap.get(child));
});

test("copying changes the physical location and poisons retired space", () => {
  const heap = new ManagedHeap();
  heap.poisonCollected = true;
  const record = layout(heap, 16);
  const root = allocate(heap, record);
  heap.writeScalar(root, 0, "u64", 0xffff_ffff_ffff_fffen);
  const before = heap.get(root);
  const previousPage = heap.pages[before.page].bytes;
  heap.collectMinor([root]);
  assert.notEqual(heap.get(root).page, before.page);
  assert.equal(heap.readScalar(root, 0, "u64"), 0xffff_ffff_ffff_fffen);
  assert.deepEqual(
    previousPage.slice(before.offset, before.offset + 16),
    new Uint8Array(16).fill(0xdd),
  );
  const next = allocate(heap, record);
  assert.deepEqual(heap.read(next), new Uint8Array(16));
});

test("unfinished zero-cost reservations block collection until abort", () => {
  const heap = new ManagedHeap();
  const record = layout(heap, 8);
  const reservation = heap.reserve(record.id, 1, 0n);
  assert.throws(() => heap.collectMinor(), /collection requires committed allocations/);
  reservation.abort();
  assert.doesNotThrow(() => heap.collectMinor());
});

test("major collection releases unreachable large pages", () => {
  const heap = new ManagedHeap();
  const record = layout(heap, 32_768);
  const large = allocate(heap, record, 32_768n);
  const before = heap.stats().residentPageBytes;
  heap.collectMajor();
  assert.throws(() => heap.get(large), /stale managed handle/);
  assert.ok(heap.stats().residentPageBytes < before);
});

test("layout metadata is resident charged and reusable after its last payload dies", () => {
  const heap = new ManagedHeap();
  const first = layout(heap, 8);
  const before = heap.stats().residentPageBytes;
  const object = allocate(heap, first);
  const withPayload = heap.stats().residentPageBytes;
  assert.ok(withPayload > before);
  heap.collectMajor();
  assert.throws(() => heap.get(object), /stale managed handle/);
  const afterReclaim = heap.stats().residentPageBytes;
  const second = layout(heap, 16);
  assert.equal(second.id, first.id);
  allocate(heap, second);
  assert.ok(heap.stats().residentPageBytes > afterReclaim);
});

test("minor collection does not sweep the layout directory", () => {
  const heap = new ManagedHeap();
  const first = layout(heap, 8);
  allocate(heap, first);

  heap.collectMinor();
  const second = layout(heap, 16);
  assert.notEqual(second.id, first.id);

  heap.collectMajor();
  const third = layout(heap, 32);
  assert.ok([first.id, second.id].includes(third.id));
});

test("re-registering a shape does not acquire an implicit owner pin", () => {
  const heap = new ManagedHeap();
  const first = heap.registerLayout({ stride: 8 });
  assert.equal(heap.registerLayout({ stride: 8 }), first);
  const object = allocate(heap, { id: first, stride: 8 });
  heap.collectMajor();
  assert.throws(() => heap.get(object), /stale managed handle/);
  assert.equal(heap.registerLayout({ stride: 16 }), first);
});

test("promotion falls back to the prepared to-space when old admission is unavailable", () => {
  const heap = new ManagedHeap({ maxResidentBytes: 220_000n });
  const record = layout(heap, 16);
  const root = allocate(heap, record);
  heap.addRoot(root);
  heap.collectMinor();
  heap.collectMinor();
  assert.equal(heap.get(root).old, false);
});

test("nursery pair admission and collection do not grow resident storage", () => {
  const limited = new ManagedHeap({ maxResidentBytes: 100_000n });
  const record = layout(limited, 8);
  assert.throws(() => allocate(limited, record), /managed resident limit exceeded/);

  const heap = new ManagedHeap();
  const small = layout(heap, 8);
  const root = allocate(heap, small);
  const before = heap.stats();
  heap.addRoot(root);
  heap.collectMinor();
  const after = heap.stats();
  assert.equal(after.residentPageBytes, before.residentPageBytes);
  assert.equal(after.totalAllocations, before.totalAllocations);
});

test("directory admission failure releases the unpublished layout pin", () => {
  const heap = new ManagedHeap({ maxResidentBytes: 32n });
  const before = heap.stats();
  const record = layout(heap, 8);
  assert.throws(() => allocate(heap, record), /managed resident limit exceeded/);
  heap.reclaimUnusedLayouts();
  const after = heap.stats();
  assert.equal(after.residentPageBytes, before.residentPageBytes);
  assert.equal(after.reservedBytes, before.reservedBytes);
  assert.equal(after.occupiedBytes, before.occupiedBytes);
});
