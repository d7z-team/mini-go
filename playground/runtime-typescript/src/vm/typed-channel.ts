import {
  packedAggregate,
  packedRecordValue,
  prepareStorageLayout,
  type PackedStorage,
  type StorageLayout,
  type StorageOwner,
} from "./aggregate-storage";
import { STORAGE_COST } from "./contract.generated";
import type { GuestHandle, ManagedLayout } from "./managed-heap";
import type { Cell, RuntimeType, Value } from "./value";

/** Scheduler queues contain task identities; buffered guest values live in a
 * typed ring. Consuming an entry clears its guest edges before reusing it. */
export class TypedChannel {
  readonly handle: GuestHandle;
  readonly queue = this;
  readonly senders: { task: number; value: Cell }[] = [];
  readonly receivers: number[] = [];
  private readonly layout: StorageLayout;
  private readonly stride: number;
  private readonly bufferLayout: number;
  private readonly identityLayout: number;
  private closedLayout = false;

  constructor(
    private readonly owner: StorageOwner,
    private readonly element: RuntimeType,
    capacity: number,
  ) {
    if (!Number.isSafeInteger(capacity) || capacity < 0 || capacity > 0xffff_ffff)
      throw new Error("invalid channel capacity");
    this.layout = prepareStorageLayout(element);
    this.stride = Math.max(1, this.layout.size);
    const registerLayout =
      owner.registerLayout ?? ((layout: ManagedLayout) => owner.heap.registerLayout(layout));
    const retainLayout = owner.retainLayout ?? ((layout: number) => owner.heap.pinLayout(layout));
    let bufferLayout = 0;
    let identityLayout = 0;
    let buffer: GuestHandle | undefined;
    let handle: GuestHandle | undefined;
    try {
      bufferLayout = registerLayout({
        stride: this.stride,
        references: this.layout.references,
        dynamics: this.layout.dynamics,
      });
      retainLayout(bufferLayout);
      identityLayout = registerLayout({ stride: 24, references: [16] });
      retainLayout(identityLayout);
      if (capacity > 0)
        buffer = owner.allocate(
          bufferLayout,
          capacity,
          BigInt(STORAGE_COST.record_header) + BigInt(capacity) * this.layout.logical,
        );
      handle = owner.allocate(
        identityLayout,
        1,
        BigInt(STORAGE_COST.record_header + STORAGE_COST.channel_header),
      );
      if (buffer) owner.heap.writeReference(handle, 16, buffer);
      owner.heap.writeScalar(handle, 12, "u32", capacity);
    } catch (error) {
      if (handle) owner.release(handle);
      if (buffer) owner.release(buffer);
      if (identityLayout) owner.releaseLayout?.(identityLayout);
      if (bufferLayout) owner.releaseLayout?.(bufferLayout);
      throw error;
    }
    this.bufferLayout = bufferLayout;
    this.identityLayout = identityLayout;
    this.handle = handle!;
  }

  close(): void {
    if (this.closedLayout) return;
    this.closedLayout = true;
    this.owner.releaseLayout?.(this.bufferLayout);
    this.owner.releaseLayout?.(this.identityLayout);
  }

  get capacity(): number {
    return Number(this.owner.heap.readScalar(this.handle, 12, "u32"));
  }

  collectRuntimeTypes(target: Set<RuntimeType>): void {
    target.add(this.element);
  }

  get closed(): boolean {
    return this.owner.heap.readScalar(this.handle, 0, "u32") !== 0;
  }
  set closed(value: boolean) {
    this.owner.heap.writeScalar(this.handle, 0, "u32", value ? 1 : 0);
  }

  private bufferStorage(buffer: GuestHandle): PackedStorage {
    const heap = this.owner.heap;
    return {
      handle: buffer,
      read: (position, length) => heap.read(buffer, position, length),
      write: (position, value) => heap.write(buffer, value, position),
    };
  }

  get length(): number {
    return Number(this.owner.heap.readScalar(this.handle, 4, "u32"));
  }

  push(value: Value): void {
    const length = this.length;
    if (length >= this.capacity) throw new Error("channel buffer is full");
    const encoded = this.owner.encode(value, this.layout, this.element);
    const head = Number(this.owner.heap.readScalar(this.handle, 8, "u32"));
    const buffer = this.owner.heap.readReference(this.handle, 16);
    this.owner.heap.write(buffer, encoded, ((head + length) % this.capacity) * this.stride);
    this.owner.heap.writeScalar(this.handle, 4, "u32", length + 1);
  }

  shift(replacement?: Value): Value | undefined {
    const length = this.length;
    if (!length) return undefined;
    const encoded = replacement && this.owner.encode(replacement, this.layout, this.element);
    const heap = this.owner.heap;
    const head = Number(heap.readScalar(this.handle, 8, "u32"));
    const buffer = heap.readReference(this.handle, 16);
    const offset = head * this.stride;
    const storage = this.bufferStorage(buffer);
    this.owner.retain?.(this.layout, heap.read(buffer, offset, this.layout.size));
    const value = packedRecordValue(this.element, this.layout, this.owner, storage, offset);
    const stable = packedAggregate(value) ? this.owner.copyAggregate(value) : value;
    heap.clear(buffer, offset, this.stride);
    if (encoded) heap.write(buffer, encoded, ((head + length) % this.capacity) * this.stride);
    heap.writeScalar(this.handle, 8, "u32", (head + 1) % this.capacity);
    heap.writeScalar(this.handle, 4, "u32", encoded ? length : length - 1);
    return stable;
  }

  *[Symbol.iterator](): IterableIterator<Value> {
    const heap = this.owner.heap;
    const head = Number(heap.readScalar(this.handle, 8, "u32"));
    const buffer = heap.readReference(this.handle, 16);
    for (let index = 0; index < this.length; index++) {
      const offset = ((head + index) % this.capacity) * this.stride;
      const storage = this.bufferStorage(buffer);
      this.owner.retain?.(this.layout, heap.read(buffer, offset, this.layout.size));
      const value = packedRecordValue(this.element, this.layout, this.owner, storage, offset);
      yield packedAggregate(value) ? this.owner.copyAggregate(value) : value;
    }
  }
}
