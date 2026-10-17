import assert from "node:assert/strict";
import { test } from "vitest";
import { GuestMemory } from "../src/vm/guest-memory";

function layout(memory, stride = 16) {
  return memory.managedHeap.registerLayout({ stride });
}

test("managed payload reservations move through one logical ledger", () => {
  const memory = new GuestMemory({ maxGuestBytes: 64n });
  const record = layout(memory);
  const reservation = memory.managedHeap.reserve(record, 1, 24n);

  assert.equal(memory.stats().occupiedBytes, 0n);
  assert.equal(memory.stats().reservedBytes, 24n);
  assert.equal(memory.stats().totalAllocatedBytes, 0n);

  const handle = reservation.commit();
  assert.deepEqual(memory.managedHeap.get(handle).bytes, 24n);
  assert.equal(memory.stats().occupiedBytes, 24n);
  assert.equal(memory.stats().reservedBytes, 0n);
  assert.equal(memory.stats().totalAllocatedBytes, 24n);
  assert.equal(memory.stats().totalAllocations, 1n);
});

test("maxGuestBytes admits occupied and pending logical storage together", () => {
  const memory = new GuestMemory({ maxGuestBytes: 24n });
  const record = layout(memory);
  const first = memory.managedHeap.reserve(record, 1, 16n);
  first.commit();

  assert.throws(
    () => memory.managedHeap.reserve(record, 1, 9n),
    (error) => error.code === "collection_required",
  );
  const pending = memory.managedHeap.reserve(record, 1, 8n);
  assert.equal(memory.stats().reservedBytes, 8n);
  pending.abort();
  assert.equal(memory.stats().reservedBytes, 0n);
  assert.equal(memory.stats().occupiedBytes, 16n);
});

test("stats reads are observational and do not perform collection", () => {
  const memory = new GuestMemory();
  const record = layout(memory);
  const handle = memory.managedHeap.reserve(record, 1, 16n).commit();

  const before = memory.stats();
  assert.equal(before.occupiedBytes, 16n);
  assert.equal(before.majorCollections, 0n);
  assert.equal(memory.stats().occupiedBytes, 16n);
  assert.equal(memory.stats().majorCollections, 0n);

  memory.managedHeap.collectMajor();
  const after = memory.stats();
  assert.equal(after.majorCollections, 1n);
  assert.equal(after.occupiedBytes, 0n);
  assert.equal(after.reclaimedBytes, 16n);
  assert.throws(() => memory.managedHeap.get(handle), { code: "invalid_operation" });
});

test("managed payload is initialized before commit and remains page authoritative", () => {
  const memory = new GuestMemory();
  const record = memory.managedHeap.registerLayout({
    stride: 16,
    references: [0],
  });
  const reservation = memory.managedHeap.reserve(record, 1, 16n);
  reservation.bytes().set(Uint8Array.from([1, 2, 3]));
  const handle = reservation.commit();

  assert.deepEqual([...memory.managedHeap.read(handle, 0, 3)], [1, 2, 3]);
  memory.managedHeap.writeScalar(handle, 4, "u32", 0xfeed_beef);
  assert.equal(memory.managedHeap.readScalar(handle, 4, "u32"), 0xfeed_beef);
});

test("control reservations share the payload budget and release deterministically", () => {
  const memory = new GuestMemory({ maxGuestBytes: 48n });
  const reservation = memory.reserveControl(32n);
  assert.equal(memory.stats().reservedBytes, 32n);
  const lease = reservation.commit();
  assert.equal(memory.stats().occupiedBytes, 32n);
  assert.equal(memory.stats().reservedBytes, 0n);

  assert.throws(() => memory.managedHeap.reserve(layout(memory), 1, 17n), {
    code: "collection_required",
  });
  lease.release();
  assert.equal(memory.stats().occupiedBytes, 0n);
});

test("resident limits govern pages and metadata independently of guest logical bytes", () => {
  const memory = new GuestMemory({ maxResidentBytes: 1n });
  assert.throws(
    () => layout(memory),
    (error) => error.code === "allocation_limit",
  );
});
