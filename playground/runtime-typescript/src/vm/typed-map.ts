import { STORAGE_COST } from "./contract.generated";
import {
  packedRecordValue,
  type PackedStorage,
  type StorageLayout,
  type StorageOwner,
} from "./aggregate-storage";
import type { GuestHandle, ManagedLayout } from "./managed-heap";
import { VMError } from "./json";
import { assertComparable, guestEqual, hashValue, type RuntimeType, type Value } from "./value";

const ENTRIES_PER_SLAB = 64;
const ENTRY_METADATA = 16;
const IDENTITY_BYTES = 32;
const OCCUPIED = 0;
const GENERATION = 4;
const HASH = 8;
const NEXT = 12;

export interface MapEntryValue {
  readonly index: number;
  readonly generation: number;
  readonly key: Value;
  readonly value: Value;
}

/** A map owns typed entry slabs and numeric collision chains. Only temporary
 * reads contain Values; the persistent payload and all guest edges are pages. */
export class TypedMap {
  readonly handle: GuestHandle;
  private readonly entryStride: number;
  private readonly valueOffset: number;
  private readonly entryLayout: number;
  private readonly directoryLayout: number;
  private readonly bucketLayout: number;
  private readonly identityLayout: number;
  private readonly needsAllocationScope: boolean;
  private closed = false;

  constructor(
    private readonly owner: StorageOwner,
    private readonly keyType: RuntimeType,
    private readonly valueType: RuntimeType,
    private readonly keyLayout: StorageLayout,
    private readonly valueLayout: StorageLayout,
  ) {
    this.valueOffset = Math.ceil((ENTRY_METADATA + keyLayout.size) / 8) * 8;
    this.entryStride = Math.ceil((this.valueOffset + valueLayout.size) / 8) * 8;
    // A map containing only scalar lanes cannot create child payloads while
    // encoding an entry. Its own slab/bucket preparation already rolls back
    // pending handles, so avoid allocating a transaction scope on that hot
    // path. Reference and dynamic lanes retain the full mutation scope.
    this.needsAllocationScope =
      keyLayout.references.length !== 0 ||
      keyLayout.dynamics.length !== 0 ||
      valueLayout.references.length !== 0 ||
      valueLayout.dynamics.length !== 0;
    if (
      !Number.isSafeInteger(this.entryStride) ||
      this.entryStride * ENTRIES_PER_SLAB > 0xffff_fff8
    )
      throw new VMError("map entry storage is too large", "allocation_limit", "map");
    const registerLayout =
      owner.registerLayout ?? ((layout: ManagedLayout) => owner.heap.registerLayout(layout));
    const retainLayout = owner.retainLayout ?? ((layout: number) => owner.heap.pinLayout(layout));
    let entryLayout = 0;
    let directoryLayout = 0;
    let bucketLayout = 0;
    let identityLayout = 0;
    try {
      entryLayout = registerLayout({
        stride: this.entryStride,
        references: [
          ...keyLayout.references.map((offset) => ENTRY_METADATA + offset),
          ...valueLayout.references.map((offset) => this.valueOffset + offset),
        ],
        dynamics: [
          ...keyLayout.dynamics.map((offset) => ENTRY_METADATA + offset),
          ...valueLayout.dynamics.map((offset) => this.valueOffset + offset),
        ],
      });
      retainLayout(entryLayout);
      directoryLayout = registerLayout({ stride: 8, references: [0] });
      retainLayout(directoryLayout);
      bucketLayout = registerLayout({ stride: 4 });
      retainLayout(bucketLayout);
      identityLayout = registerLayout({ stride: IDENTITY_BYTES, references: [0, 8] });
      retainLayout(identityLayout);
      this.handle = owner.allocate(
        identityLayout,
        1,
        BigInt(STORAGE_COST.record_header + STORAGE_COST.map_header),
      );
    } catch (error) {
      if (identityLayout) owner.releaseLayout?.(identityLayout);
      if (bucketLayout) owner.releaseLayout?.(bucketLayout);
      if (directoryLayout) owner.releaseLayout?.(directoryLayout);
      if (entryLayout) owner.releaseLayout?.(entryLayout);
      throw error;
    }
    this.entryLayout = entryLayout;
    this.directoryLayout = directoryLayout;
    this.bucketLayout = bucketLayout;
    this.identityLayout = identityLayout;
  }

  get size(): number {
    return this.word(this.handle, 16);
  }

  collectRuntimeTypes(target: Set<RuntimeType>): void {
    target.add(this.keyType);
    target.add(this.valueType);
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.owner.releaseLayout?.(this.entryLayout);
    this.owner.releaseLayout?.(this.directoryLayout);
    this.owner.releaseLayout?.(this.bucketLayout);
    this.owner.releaseLayout?.(this.identityLayout);
  }

  get(key: Value): MapEntryValue | undefined {
    assertComparable(key);
    const index = this.find(key, this.hash(key));
    return index === 0 ? undefined : this.entry(index);
  }

  set(key: Value, value: Value): MapEntryValue {
    assertComparable(key);
    const allocation = this.needsAllocationScope ? this.owner.beginAllocation?.() : undefined;
    try {
      const hash = this.hash(key);
      const existing = this.find(key, hash);
      // Materialize all fallible value storage before changing the table.
      const encodedValue = this.owner.encode(value, this.valueLayout, this.valueType);
      if (existing !== 0) {
        const { slab, offset } = this.location(existing);
        this.owner.heap.write(slab, encodedValue, offset + this.valueOffset);
        allocation?.commit();
        return this.entry(existing)!;
      }
      const encodedKey = this.owner.encode(key, this.keyLayout, this.keyType);
      const created = this.word(this.handle, 20);
      const free = this.word(this.handle, 28);
      const index = free || created + 1;
      if (index > 0xffff_ffff)
        throw new VMError("map entry index overflow", "allocation_limit", "map");
      this.ensureCapacity(index);
      const { slab, offset } = this.location(index);
      const generation = this.word(slab, offset + GENERATION) + 1;
      const buckets = this.owner.heap.readReference(this.handle, 8);
      const bucket = hash % (this.owner.heap.get(buckets).length / 4);
      const head = this.word(buckets, bucket * 4);
      const nextFree = free ? this.word(slab, offset + NEXT) : 0;
      this.owner.heap.write(slab, encodedKey, offset + ENTRY_METADATA);
      this.owner.heap.write(slab, encodedValue, offset + this.valueOffset);
      this.owner.heap.writeScalar(slab, offset + GENERATION, "u32", generation);
      this.owner.heap.writeScalar(slab, offset + HASH, "u32", hash);
      this.owner.heap.writeScalar(slab, offset + NEXT, "u32", head);
      this.owner.heap.writeScalar(slab, offset + OCCUPIED, "u32", 1);
      this.owner.heap.writeScalar(buckets, bucket * 4, "u32", index);
      this.owner.heap.writeScalar(this.handle, 16, "u32", this.size + 1);
      if (free) this.owner.heap.writeScalar(this.handle, 28, "u32", nextFree);
      else this.owner.heap.writeScalar(this.handle, 20, "u32", index);
      allocation?.commit();
      return { index, generation, key, value };
    } catch (error) {
      allocation?.abort();
      throw error;
    }
  }

  delete(key: Value): boolean {
    assertComparable(key);
    const buckets = this.owner.heap.readReference(this.handle, 8);
    if (!buckets.slot) return false;
    const hash = this.hash(key);
    const bucket = hash % (this.owner.heap.get(buckets).length / 4);
    let index = this.word(buckets, bucket * 4);
    let previous = 0;
    while (index !== 0) {
      const { slab, offset } = this.location(index);
      const next = this.word(slab, offset + NEXT);
      if (this.word(slab, offset + HASH) === hash && guestEqual(this.readKey(slab, offset), key)) {
        if (previous === 0) this.owner.heap.writeScalar(buckets, bucket * 4, "u32", next);
        else {
          const before = this.location(previous);
          this.owner.heap.writeScalar(before.slab, before.offset + NEXT, "u32", next);
        }
        const generation = this.word(slab, offset + GENERATION);
        this.owner.heap.clear(slab, offset, this.entryStride);
        this.owner.heap.writeScalar(slab, offset + GENERATION, "u32", generation);
        if (generation !== 0xffff_ffff) {
          this.owner.heap.writeScalar(slab, offset + NEXT, "u32", this.word(this.handle, 28));
          this.owner.heap.writeScalar(this.handle, 28, "u32", index);
        }
        this.owner.heap.writeScalar(this.handle, 16, "u32", this.size - 1);
        return true;
      }
      previous = index;
      index = next;
    }
    return false;
  }

  clear(): void {
    const buckets = this.owner.heap.readReference(this.handle, 8);
    if (!buckets.slot) return;
    this.owner.heap.clear(buckets, 0, this.owner.heap.get(buckets).length);
    let free = 0;
    for (let index = this.word(this.handle, 20); index > 0; index--) {
      const { slab, offset } = this.location(index);
      const generation = this.word(slab, offset + GENERATION);
      this.owner.heap.clear(slab, offset, this.entryStride);
      this.owner.heap.writeScalar(slab, offset + GENERATION, "u32", generation);
      if (generation !== 0xffff_ffff) {
        this.owner.heap.writeScalar(slab, offset + NEXT, "u32", free);
        free = index;
      }
    }
    this.owner.heap.writeScalar(this.handle, 16, "u32", 0);
    this.owner.heap.writeScalar(this.handle, 28, "u32", free);
  }

  /** Entry identities, not key/value snapshots. Mutated values are read at next. */
  snapshot(): Uint32Array {
    const entries = new Uint32Array(this.size * 2);
    let cursor = 0;
    for (let index = 1, count = this.word(this.handle, 20); index <= count; index++) {
      const { slab, offset } = this.location(index);
      if (!this.word(slab, offset + OCCUPIED)) continue;
      entries[cursor++] = index;
      entries[cursor++] = this.word(slab, offset + GENERATION);
    }
    return entries;
  }

  entry(index: number, generation?: number): MapEntryValue | undefined {
    if (!Number.isInteger(index) || index <= 0 || index > this.word(this.handle, 20))
      return undefined;
    const { slab, offset } = this.location(index);
    if (!this.word(slab, offset + OCCUPIED)) return undefined;
    const current = this.word(slab, offset + GENERATION);
    if (generation !== undefined && generation !== current) return undefined;
    return {
      index,
      generation: current,
      key: this.readKey(slab, offset),
      value: packedRecordValue(
        this.valueType,
        this.valueLayout,
        this.owner,
        this.storage(slab),
        offset + this.valueOffset,
      ),
    };
  }

  *entries(): IterableIterator<MapEntryValue> {
    for (let index = 1, count = this.word(this.handle, 20); index <= count; index++) {
      const entry = this.entry(index);
      if (entry) yield entry;
    }
  }

  private hash(key: Value): number {
    if (key.kind === "int" || key.kind === "uint") {
      const bits = BigInt.asUintN(64, key.value as bigint);
      const low = Number(bits & 0xffff_ffffn);
      const high = Number(bits >> 32n);
      return (Math.imul(low ^ 2166136261, 16777619) ^ high) >>> 0;
    }
    if (key.kind === "string") {
      let hash = 2166136261;
      for (const byte of key.value as Uint8Array) hash = Math.imul(hash ^ byte, 16777619);
      return hash >>> 0;
    }
    if (key.kind === "bool") return key.value ? 1 : 0;
    const text = hashValue(key);
    let hash = 2166136261;
    for (let index = 0; index < text.length; index++)
      hash = Math.imul(hash ^ text.charCodeAt(index), 16777619);
    return hash >>> 0;
  }

  private word(handle: GuestHandle, offset: number): number {
    return this.owner.heap.readScalar(handle, offset, "u32") as number;
  }

  private location(index: number): { slab: GuestHandle; offset: number } {
    const directory = this.owner.heap.readReference(this.handle, 0);
    return {
      slab: this.owner.heap.readReference(
        directory,
        Math.floor((index - 1) / ENTRIES_PER_SLAB) * 8,
      ),
      offset: ((index - 1) % ENTRIES_PER_SLAB) * this.entryStride,
    };
  }

  private readKey(slab: GuestHandle, offset: number): Value {
    return packedRecordValue(
      this.keyType,
      this.keyLayout,
      this.owner,
      this.storage(slab),
      offset + ENTRY_METADATA,
    );
  }

  private storage(handle: GuestHandle): PackedStorage {
    return {
      handle,
      read: (offset, length) => this.owner.heap.read(handle, offset, length),
      write: (offset, bytes) => this.owner.heap.write(handle, bytes, offset),
    };
  }

  private find(key: Value, hash: number): number {
    const buckets = this.owner.heap.readReference(this.handle, 8);
    if (!buckets.slot) return 0;
    const bucket = hash % (this.owner.heap.get(buckets).length / 4);
    let index = this.word(buckets, bucket * 4);
    while (index !== 0) {
      const { slab, offset } = this.location(index);
      if (this.word(slab, offset + HASH) === hash && guestEqual(this.readKey(slab, offset), key))
        return index;
      index = this.word(slab, offset + NEXT);
    }
    return 0;
  }

  private ensureCapacity(index: number): void {
    const capacity = this.word(this.handle, 24);
    if (index <= capacity) return;
    const slabCount = capacity / ENTRIES_PER_SLAB;
    const newCapacity = capacity + ENTRIES_PER_SLAB;
    if (newCapacity > 0xffff_ffff)
      throw new VMError("map storage capacity overflow", "allocation_limit", "map");
    const heap = this.owner.heap;
    const oldDirectory = heap.readReference(this.handle, 0);
    let directory = oldDirectory;
    let slab: GuestHandle;
    const oldBuckets = heap.readReference(this.handle, 8);
    let buckets = oldBuckets;
    let bucketCount = buckets.slot ? heap.get(buckets).length / 4 : 0;
    const pending: GuestHandle[] = [];
    try {
      if (!directory.slot || slabCount * 8 === heap.get(directory).length) {
        const count = Math.max(8, slabCount * 2);
        directory = this.owner.allocate(
          this.directoryLayout,
          count,
          BigInt(STORAGE_COST.record_header + count * STORAGE_COST.reference),
        );
        pending.push(directory);
        if (oldDirectory.slot)
          heap.copy(directory, 0, oldDirectory, 0, slabCount * STORAGE_COST.reference);
      }
      slab = this.owner.allocate(
        this.entryLayout,
        ENTRIES_PER_SLAB,
        BigInt(STORAGE_COST.record_header) +
          BigInt(ENTRIES_PER_SLAB) *
            (BigInt(STORAGE_COST.map_entry) + this.keyLayout.logical + this.valueLayout.logical),
      );
      pending.push(slab);
      if (bucketCount < newCapacity * 2) {
        bucketCount = 2 ** Math.ceil(Math.log2(newCapacity * 2));
        buckets = this.owner.allocate(
          this.bucketLayout,
          bucketCount,
          BigInt(STORAGE_COST.record_header + bucketCount * STORAGE_COST.map_bucket),
        );
        pending.push(buckets);
      }
    } catch (error) {
      for (const handle of pending) this.owner.release(handle);
      throw error;
    }
    // All allocation has succeeded. Rehash updates numeric chains only and
    // preserves every existing entry's index and generation.
    if (buckets !== oldBuckets)
      for (let entry = 1, count = this.word(this.handle, 20); entry <= count; entry++) {
        const location = this.location(entry);
        if (!this.word(location.slab, location.offset + OCCUPIED)) continue;
        const bucket = this.word(location.slab, location.offset + HASH) % bucketCount;
        heap.writeScalar(
          location.slab,
          location.offset + NEXT,
          "u32",
          this.word(buckets, bucket * 4),
        );
        heap.writeScalar(buckets, bucket * 4, "u32", entry);
      }
    heap.writeReference(directory, slabCount * 8, slab);
    heap.writeReference(this.handle, 0, directory);
    heap.writeReference(this.handle, 8, buckets);
    heap.writeScalar(this.handle, 24, "u32", newCapacity);
    // Superseded index pages have no language-visible aliases. The VM may
    // reclaim them at its next collection; iterator roots hold this identity.
  }
}
