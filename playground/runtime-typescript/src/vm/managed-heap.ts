import { VMError } from "./json";

export interface GuestHandle {
  readonly slot: number;
  readonly generation: number;
}

export interface ManagedLayout {
  readonly stride: number;
  readonly references?: readonly number[];
  readonly dynamics?: readonly number[];
}

export type ManagedScalar =
  | "i8"
  | "u8"
  | "i16"
  | "u16"
  | "i32"
  | "u32"
  | "i64"
  | "u64"
  | "f32"
  | "f64";
const SCALAR_BYTES: Record<ManagedScalar, number> = {
  i8: 1,
  u8: 1,
  i16: 2,
  u16: 2,
  i32: 4,
  u32: 4,
  i64: 8,
  u64: 8,
  f32: 4,
  f64: 8,
};

export interface ManagedHeapStats {
  readonly occupiedBytes: bigint;
  readonly reservedBytes: bigint;
  readonly lastMajorReachableBytes: bigint;
  readonly residentPageBytes: bigint;
  readonly totalAllocatedBytes: bigint;
  readonly totalAllocations: bigint;
  readonly minorCollections: bigint;
  readonly majorCollections: bigint;
  readonly copiedBytes: bigint;
  readonly promotedBytes: bigint;
  readonly reclaimedBytes: bigint;
  readonly scannedRoots: bigint;
  readonly scannedCards: bigint;
  readonly scannedReferences: bigint;
}

export interface ManagedRecord {
  readonly layout: number;
  readonly bytes: bigint;
  readonly page: number;
  readonly offset: number;
  readonly length: number;
  readonly generation: number;
  readonly old: boolean;
}

export type PublishedRecordVisitor = (
  handle: GuestHandle,
  stride: number,
  dynamics: Uint32Array,
  count: number,
) => void;

const PAGE_BYTES = 64 * 1024;
const CARD_BYTES = 256;
const SEGMENT_SIZE = 1024;
const MAX_BYTES = 0x7fff_ffff_ffff_ffffn;
const FREE = 0;
const RESERVED = 1;
const PUBLISHED = 2;
const YOUNG_PAGE = 1;
const SPARE_PAGE = 2;
const OLD_PAGE = 3;
const LARGE_PAGE = 4;

/** Numeric directory segments never contain guest objects or host values. */
class DirectorySegment {
  readonly generation = new Uint32Array(SEGMENT_SIZE);
  readonly state = new Uint8Array(SEGMENT_SIZE);
  readonly layout = new Uint32Array(SEGMENT_SIZE);
  readonly count = new Uint32Array(SEGMENT_SIZE);
  readonly page = new Uint32Array(SEGMENT_SIZE);
  readonly offset = new Uint32Array(SEGMENT_SIZE);
  readonly size = new Uint32Array(SEGMENT_SIZE);
  readonly logical = new BigUint64Array(SEGMENT_SIZE);
  readonly marked = new Uint32Array(SEGMENT_SIZE);
  readonly age = new Uint8Array(SEGMENT_SIZE);
  readonly young = new Uint8Array(SEGMENT_SIZE);
  readonly occupiedIndex = new Uint32Array(SEGMENT_SIZE);
  readonly youngIndex = new Uint32Array(SEGMENT_SIZE);
}

interface PayloadPage {
  bytes: Uint8Array;
  view: DataView;
  kind: number;
  used: number;
  partner: number;
  sizeClass: number;
  entries: Uint32Array;
  free: Uint32Array;
  freeCount: number;
  cards: Uint8Array;
  cardCapacity: number;
  resident: bigint;
}

interface HeapLayout {
  stride: number;
  references: Uint32Array;
  dynamics: Uint32Array;
  key: string;
  users: number;
  pins: number;
  active: boolean;
  charged: boolean;
  metadataBytes: bigint;
}

function firstOffset(offsets: Uint32Array, minimum: number): number {
  let first = 0;
  let last = offsets.length;
  while (first < last) {
    const middle = (first + last) >>> 1;
    if (offsets[middle]! < minimum) first = middle + 1;
    else last = middle;
  }
  return first;
}

export class ManagedReservation {
  finished = false;

  constructor(
    private readonly heap: ManagedHeap,
    readonly handle: GuestHandle,
    private readonly layoutID: number,
  ) {}

  /** An unpublished borrow: callers finish initialization before commit/yield. */
  bytes(): Uint8Array {
    if (this.finished) throw new VMError("reservation is finished", "internal", "memory");
    return this.heap.reservationBytes(this.handle);
  }

  commit(): GuestHandle {
    if (this.finished) throw new VMError("reservation is finished", "internal", "memory");
    this.heap.finishReservation(this.handle, true);
    this.heap.releaseReservationPin(this.layoutID);
    this.finished = true;
    return this.handle;
  }

  abort(): void {
    if (this.finished) return;
    this.heap.finishReservation(this.handle, false);
    this.heap.releaseReservationPin(this.layoutID);
    this.finished = true;
  }
}

/** Typed pages, stable tickets, copying nursery and synchronous major collection. */
export class ManagedHeap {
  onReclaim?: (slot: number, generation: number) => void;
  private readonly segments: DirectorySegment[] = [];
  private readonly layouts: HeapLayout[] = [
    {
      stride: 0,
      references: new Uint32Array(),
      dynamics: new Uint32Array(),
      key: "",
      users: 0,
      pins: 0,
      active: false,
      charged: false,
      metadataBytes: 0n,
    },
  ];
  private readonly layoutKeys = new Map<string, number>();
  private readonly freeLayouts: number[] = [];
  private readonly pages: PayloadPage[] = [];
  private readonly nurseryPages: number[] = [];
  private readonly freePages: number[] = [];
  private readonly oldAvailable: number[][] = Array.from({ length: 14 }, () => []);
  private readonly roots = new Map<number, number>();
  private nextSlot = 1;
  private freeSlots = new Uint32Array();
  private freeCount = 0;
  private occupied = new Uint32Array();
  private occupiedCount = 0;
  private youngSlots = new Uint32Array();
  private youngCount = 0;
  private queue = new Uint32Array();
  private queueLength = 0;
  private cardPages = new Uint32Array();
  private cardIndices = new Uint32Array();
  private cardCount = 0;
  private epoch = 0;
  private unpublished = 0;
  private nurseryCapacity = 0;
  private closed = false;
  private poisonCollected = false;
  private occupiedBytes = 0n;
  private youngBytes = 0n;
  private reservedBytes = 0n;
  private lastMajorReachableBytes = 0n;
  private residentPageBytes = 0n;
  private totalAllocatedBytes = 0n;
  private totalAllocations = 0n;
  private minorCollections = 0n;
  private majorCollections = 0n;
  private copiedBytes = 0n;
  private promotedBytes = 0n;
  private reclaimedBytes = 0n;
  private scannedRoots = 0n;
  private scannedCards = 0n;
  private scannedReferences = 0n;

  constructor(
    private readonly limits: {
      maxGuestBytes?: bigint;
      maxResidentBytes?: bigint;
      nurseryBytes?: number;
    } = {},
  ) {}

  get occupiedGuestBytes(): bigint {
    return this.occupiedBytes;
  }

  get oldGuestBytes(): bigint {
    return this.occupiedBytes - this.youngBytes;
  }

  reserveResident(bytes: bigint): { release(): void } {
    if (this.closed || bytes < 0n)
      throw new VMError("invalid resident allocation", "allocation_limit", "memory");
    this.admitResident(bytes);
    let released = false;
    return {
      release: () => {
        if (released) return;
        released = true;
        if (!this.closed) this.residentPageBytes -= bytes;
      },
    };
  }

  registerLayout(layout: ManagedLayout): number {
    if (this.closed) throw new VMError("managed heap is closed", "internal", "memory.layout");
    if (!Number.isSafeInteger(layout.stride) || layout.stride <= 0 || layout.stride > 0xffff_ffff)
      throw new VMError("invalid managed layout stride", "internal", "memory.layout");
    const references = [...(layout.references ?? [])].sort((a, b) => a - b);
    const dynamics = [...(layout.dynamics ?? [])].sort((a, b) => a - b);
    for (const [offsets, width] of [
      [references, 8],
      [dynamics, 24],
    ] as const)
      for (let index = 0; index < offsets.length; index++) {
        const offset = offsets[index]!;
        if (
          !Number.isSafeInteger(offset) ||
          offset < 0 ||
          offset + width > layout.stride ||
          (index > 0 && offsets[index - 1]! + width > offset)
        )
          throw new VMError("invalid managed reference offset", "internal", "memory.layout");
      }
    for (const dynamic of dynamics)
      for (const reference of references)
        if (reference < dynamic + 24 && reference + 8 > dynamic)
          throw new VMError("overlapping managed reference layout", "internal", "memory.layout");
    const key = `${layout.stride}:${references.join(",")}:${dynamics.join(",")}`;
    const existing = this.layoutKeys.get(key);
    if (existing !== undefined && this.layouts[existing]!.active) {
      return existing;
    }
    const metadataBytes = BigInt(4 + (references.length + dynamics.length) * 4);
    this.admitResident(metadataBytes);
    const id = this.freeLayouts.pop() ?? this.layouts.length;
    const entry = this.layouts[id] ?? {
      stride: 0,
      references: new Uint32Array(),
      dynamics: new Uint32Array(),
      key: "",
      users: 0,
      pins: 0,
      active: false,
      charged: false,
      metadataBytes: 0n,
    };
    entry.stride = layout.stride;
    entry.references = Uint32Array.from(references);
    entry.dynamics = Uint32Array.from(dynamics);
    entry.key = key;
    entry.users = 0;
    entry.pins = 0;
    entry.active = true;
    entry.charged = true;
    entry.metadataBytes = metadataBytes;
    if (id === this.layouts.length) this.layouts.push(entry);
    else this.layouts[id] = entry;
    this.layoutKeys.set(key, id);
    return id;
  }

  /** Drop physical layout metadata that no live payload references anymore. */
  reclaimUnusedLayouts(): void {
    for (let id = 1; id < this.layouts.length; id++) {
      this.reclaimLayout(id);
    }
  }

  private reclaimLayout(layoutID: number): void {
    const layout = this.layouts[layoutID];
    if (!layout || !layout.active || layout.users !== 0 || layout.pins !== 0) return;
    const metadataBytes = layout.metadataBytes;
    layout.active = false;
    this.layoutKeys.delete(layout.key);
    this.residentPageBytes -= metadataBytes;
    layout.charged = false;
    layout.references = new Uint32Array();
    layout.dynamics = new Uint32Array();
    // A reclaimed id is immediately reusable. Clear every descriptor field
    // whose bytes were charged so the inactive entry is only an id tombstone.
    layout.key = "";
    layout.stride = 0;
    layout.metadataBytes = 0n;
    this.freeLayouts.push(layoutID);
  }

  pinLayout(layoutID: number): void {
    const layout = this.layouts[layoutID];
    if (this.closed || !layout || !layout.active || layoutID === 0)
      throw new VMError(`invalid managed layout ${layoutID}`, "internal", "memory.layout");
    if (!layout.charged) {
      this.admitResident(layout.metadataBytes);
      layout.charged = true;
    }
    layout.pins++;
  }

  unpinLayout(layoutID: number): void {
    const layout = this.layouts[layoutID];
    if (!layout || layout.pins === 0) return;
    layout.pins--;
    if (layout.pins === 0 && layout.users === 0) this.reclaimLayout(layoutID);
  }

  releaseReservationPin(layoutID: number): void {
    const layout = this.layouts[layoutID];
    if (!layout || layout.pins === 0) return;
    layout.pins--;
    if (layout.pins === 0 && layout.users === 0) this.reclaimLayout(layoutID);
  }

  reserveControl(bytes: bigint): {
    readonly bytes: bigint;
    commit(): { readonly bytes: bigint; release(): void };
    abort(): void;
  } {
    if (this.closed || bytes < 0n || bytes > MAX_BYTES)
      throw new VMError("invalid control allocation", "allocation_limit", "memory");
    const total = this.occupiedBytes + this.reservedBytes + bytes;
    if (
      total > MAX_BYTES ||
      (this.limits.maxGuestBytes !== undefined && total > this.limits.maxGuestBytes)
    )
      throw new VMError("control allocation requires collection", "collection_required", "memory");
    this.reservedBytes += bytes;
    let state: "reserved" | "committed" | "released" = "reserved";
    return {
      bytes,
      commit: () => {
        if (state !== "reserved" || this.closed)
          throw new VMError("control reservation is unavailable", "internal", "memory");
        state = "committed";
        this.reservedBytes -= bytes;
        this.occupiedBytes += bytes;
        this.totalAllocatedBytes += bytes;
        this.totalAllocations++;
        return {
          bytes,
          release: () => {
            if (state !== "committed") return;
            state = "released";
            if (!this.closed) this.occupiedBytes -= bytes;
          },
        };
      },
      abort: () => {
        if (state !== "reserved") return;
        state = "released";
        if (!this.closed) this.reservedBytes -= bytes;
      },
    };
  }

  reserve(layoutID: number, count: number, logicalBytes: bigint): ManagedReservation {
    const layout = this.layouts[layoutID];
    if (
      this.closed ||
      !layout ||
      !layout.active ||
      layoutID === 0 ||
      !Number.isSafeInteger(count) ||
      count < 0
    )
      throw new VMError("invalid managed allocation", "internal", "memory");
    const physical = layout.stride * count;
    if (
      !Number.isSafeInteger(physical) ||
      physical > 0xffff_fff8 ||
      logicalBytes < 0n ||
      logicalBytes > MAX_BYTES
    )
      throw new VMError("managed allocation size overflow", "allocation_limit", "memory");
    const total = this.occupiedBytes + this.reservedBytes + logicalBytes;
    if (
      total > MAX_BYTES ||
      (this.limits.maxGuestBytes !== undefined && total > this.limits.maxGuestBytes)
    )
      throw new VMError("managed allocation requires collection", "collection_required", "memory");
    this.pinLayout(layoutID);
    const size = Math.max(8, Math.ceil(physical / 8) * 8);
    while (this.freeCount > 0) {
      const slot = this.freeSlots[this.freeCount - 1]!;
      if (this.segments[slot >>> 10]!.generation[slot & 1023] !== 0xffff_ffff) break;
      this.freeCount--;
    }
    let pageID: number;
    const young = size <= PAGE_BYTES / 4;
    try {
      if (this.freeCount === 0 && this.nextSlot >= this.segments.length * SEGMENT_SIZE)
        this.growDirectory();
      if (young) {
        pageID = this.nurseryPage(size);
        const sizeClass = 2 ** Math.max(3, Math.ceil(Math.log2(size)));
        const index = Math.log2(sizeClass) - 3;
        if (this.oldAvailable[index]!.length === 0) {
          // Promotion is optional. A complete paired nursery is already admitted.
          try {
            this.newPage(PAGE_BYTES, OLD_PAGE, sizeClass);
          } catch (error) {
            if (!(error instanceof VMError) || error.code !== "allocation_limit") throw error;
          }
        }
      } else pageID = this.newPage(size, LARGE_PAGE, 0);
    } catch (error) {
      this.releaseReservationPin(layoutID);
      throw error;
    }
    const slot = this.freeCount > 0 ? this.freeSlots[--this.freeCount]! : this.nextSlot++;
    const segment = this.segments[slot >>> 10]!;
    const index = slot & 1023;
    const page = this.pages[pageID]!;
    segment.generation[index]!++;
    segment.state[index] = RESERVED;
    segment.layout[index] = layoutID;
    segment.count[index] = count;
    segment.page[index] = pageID;
    segment.offset[index] = page.used;
    segment.size[index] = size;
    segment.logical[index] = logicalBytes;
    segment.marked[index] = 0;
    segment.age[index] = 0;
    segment.young[index] = young ? 1 : 0;
    page.bytes.fill(0, page.used, page.used + size);
    page.used += size;
    if (!young) page.entries[0] = slot;
    layout.users++;
    this.reservedBytes += logicalBytes;
    this.unpublished++;
    return new ManagedReservation(this, { slot, generation: segment.generation[index]! }, layoutID);
  }

  reservationBytes(handle: GuestHandle): Uint8Array {
    const segment = this.validate(handle, RESERVED);
    const index = handle.slot & 1023;
    return this.pages[segment.page[index]!]!.bytes.subarray(
      segment.offset[index]!,
      segment.offset[index]! + segment.size[index]!,
    );
  }

  finishReservation(handle: GuestHandle, commit: boolean): void {
    const segment = this.validate(handle, RESERVED);
    const index = handle.slot & 1023;
    this.reservedBytes -= segment.logical[index]!;
    this.unpublished--;
    if (!commit) {
      this.freeObject(handle.slot, false);
      return;
    }
    segment.occupiedIndex[index] = this.occupiedCount;
    this.occupied[this.occupiedCount++] = handle.slot;
    if (segment.young[index]) {
      segment.youngIndex[index] = this.youngCount;
      this.youngSlots[this.youngCount++] = handle.slot;
      this.youngBytes += segment.logical[index]!;
    }
    this.occupiedBytes += segment.logical[index]!;
    this.totalAllocatedBytes += segment.logical[index]!;
    this.totalAllocations++;
    segment.state[index] = PUBLISHED;
    this.dirtyRegion(segment, index, 0, segment.size[index]!);
  }

  get(handle: GuestHandle): ManagedRecord {
    const segment = this.validate(handle);
    const index = handle.slot & 1023;
    return {
      layout: segment.layout[index]!,
      bytes: segment.logical[index]!,
      page: segment.page[index]!,
      offset: segment.offset[index]!,
      length: segment.size[index]!,
      generation: handle.generation,
      old: segment.young[index] === 0,
    };
  }

  read(handle: GuestHandle, offset = 0, length?: number): Uint8Array {
    const segment = this.validate(handle);
    const index = handle.slot & 1023;
    const count = length ?? segment.size[index]! - offset;
    this.checkRange(segment, index, offset, count);
    const start = segment.offset[index]! + offset;
    return this.pages[segment.page[index]!]!.bytes.slice(start, start + count);
  }

  write(handle: GuestHandle, payload: Uint8Array, offset = 0): void {
    const segment = this.validate(handle);
    const index = handle.slot & 1023;
    this.checkRange(segment, index, offset, payload.byteLength);
    this.pages[segment.page[index]!]!.bytes.set(payload, segment.offset[index]! + offset);
    this.dirtyRegion(segment, index, offset, payload.byteLength);
  }

  readReference(handle: GuestHandle, offset: number): GuestHandle {
    const segment = this.validate(handle);
    const index = handle.slot & 1023;
    this.checkRange(segment, index, offset, 8);
    const page = this.pages[segment.page[index]!]!;
    const start = segment.offset[index]! + offset;
    return {
      slot: page.view.getUint32(start, true),
      generation: page.view.getUint32(start + 4, true),
    };
  }

  readU32(handle: GuestHandle, offset: number): number {
    const segment = this.validate(handle);
    const index = handle.slot & 1023;
    this.checkRange(segment, index, offset, 4);
    const page = this.pages[segment.page[index]!]!;
    return page.view.getUint32(segment.offset[index]! + offset, true);
  }

  writeReference(handle: GuestHandle, offset: number, child: GuestHandle): void {
    const segment = this.validate(handle);
    const index = handle.slot & 1023;
    this.checkRange(segment, index, offset, 8);
    const page = this.pages[segment.page[index]!]!;
    const start = segment.offset[index]! + offset;
    page.view.setUint32(start, child.slot, true);
    page.view.setUint32(start + 4, child.generation, true);
    this.dirtyRegion(segment, index, offset, 8);
  }

  readScalar(handle: GuestHandle, offset: number, kind: ManagedScalar): number | bigint {
    const segment = this.validate(handle);
    const index = handle.slot & 1023;
    this.checkRange(segment, index, offset, SCALAR_BYTES[kind]);
    const view = this.pages[segment.page[index]!]!.view;
    const start = segment.offset[index]! + offset;
    switch (kind) {
      case "i8":
        return view.getInt8(start);
      case "u8":
        return view.getUint8(start);
      case "i16":
        return view.getInt16(start, true);
      case "u16":
        return view.getUint16(start, true);
      case "i32":
        return view.getInt32(start, true);
      case "u32":
        return view.getUint32(start, true);
      case "i64":
        return view.getBigInt64(start, true);
      case "u64":
        return view.getBigUint64(start, true);
      case "f32":
        return view.getFloat32(start, true);
      case "f64":
        return view.getFloat64(start, true);
    }
  }

  writeScalar(
    handle: GuestHandle,
    offset: number,
    kind: ManagedScalar,
    value: number | bigint,
  ): void {
    const segment = this.validate(handle);
    const index = handle.slot & 1023;
    this.checkRange(segment, index, offset, SCALAR_BYTES[kind]);
    const view = this.pages[segment.page[index]!]!.view;
    const start = segment.offset[index]! + offset;
    switch (kind) {
      case "i8":
        view.setInt8(start, Number(value));
        break;
      case "u8":
        view.setUint8(start, Number(value));
        break;
      case "i16":
        view.setInt16(start, Number(value), true);
        break;
      case "u16":
        view.setUint16(start, Number(value), true);
        break;
      case "i32":
        view.setInt32(start, Number(value), true);
        break;
      case "u32":
        view.setUint32(start, Number(value), true);
        break;
      case "i64":
        view.setBigInt64(start, BigInt(value), true);
        break;
      case "u64":
        view.setBigUint64(start, BigInt(value), true);
        break;
      case "f32":
        view.setFloat32(start, Number(value), true);
        break;
      case "f64":
        view.setFloat64(start, Number(value), true);
        break;
    }
    this.dirtyRegion(segment, index, offset, SCALAR_BYTES[kind]);
  }

  copy(
    destination: GuestHandle,
    destinationOffset: number,
    source: GuestHandle,
    sourceOffset: number,
    length: number,
  ): void {
    const target = this.validate(destination);
    const origin = this.validate(source);
    const targetIndex = destination.slot & 1023;
    const sourceIndex = source.slot & 1023;
    this.checkRange(target, targetIndex, destinationOffset, length);
    this.checkRange(origin, sourceIndex, sourceOffset, length);
    const sourceStart = origin.offset[sourceIndex]! + sourceOffset;
    const bytes = this.pages[origin.page[sourceIndex]!]!.bytes;
    this.pages[target.page[targetIndex]!]!.bytes.set(
      bytes.subarray(sourceStart, sourceStart + length),
      target.offset[targetIndex]! + destinationOffset,
    );
    this.dirtyRegion(target, targetIndex, destinationOffset, length);
  }

  clear(handle: GuestHandle, offset: number, length: number): void {
    const segment = this.validate(handle);
    const index = handle.slot & 1023;
    this.checkRange(segment, index, offset, length);
    const start = segment.offset[index]! + offset;
    this.pages[segment.page[index]!]!.bytes.fill(0, start, start + length);
    this.dirtyRegion(segment, index, offset, length);
  }

  addRoot(handle: GuestHandle): void {
    this.validate(handle);
    this.roots.set(handle.slot, handle.generation);
  }

  removeRoot(handle: GuestHandle): void {
    if (this.roots.get(handle.slot) === handle.generation) this.roots.delete(handle.slot);
  }

  has(handle: GuestHandle): boolean {
    const segment = this.segments[handle.slot >>> 10];
    const index = handle.slot & 1023;
    return Boolean(
      segment &&
        segment.state[index] === PUBLISHED &&
        segment.generation[index] === handle.generation,
    );
  }

  /**
   * Visit the published payload directory after a collection. The callback
   * receives only numeric layout facts; payload bytes remain in the pages and
   * are read through the normal handle accessors. This is used by major-only
   * metadata reclamation and never participates in minor tracing.
   */
  forEachPublishedRecord(visitor: PublishedRecordVisitor): void {
    for (let index = 0; index < this.occupiedCount; index++) {
      const slot = this.occupied[index]!;
      const segment = this.segments[slot >>> 10]!;
      const local = slot & 1023;
      const layout = this.layouts[segment.layout[local]!]!;
      visitor(
        { slot, generation: segment.generation[local]! },
        layout.stride,
        layout.dynamics,
        segment.count[local]!,
      );
    }
  }

  collectMinor(roots: Iterable<GuestHandle> = []): void {
    this.collect(roots, false);
  }
  collectMajor(roots: Iterable<GuestHandle> = []): void {
    this.collect(roots, true);
  }

  private collect(roots: Iterable<GuestHandle>, major: boolean): void {
    if (this.closed || this.unpublished !== 0)
      throw new VMError("collection requires committed allocations", "internal", "memory");
    this.epoch = (this.epoch + 1) >>> 0;
    if (this.epoch === 0) {
      for (const segment of this.segments) segment.marked.fill(0);
      this.epoch = 1;
    }
    this.queueLength = 0;
    for (const [slot, generation] of this.roots) {
      this.scannedRoots++;
      this.mark(slot, generation, major);
    }
    for (const handle of roots) {
      this.scannedRoots++;
      this.mark(handle.slot, handle.generation, major);
    }
    if (!major)
      for (let index = 0; index < this.cardCount; index++) {
        this.scannedCards++;
        this.scanCard(this.cardPages[index]!, this.cardIndices[index]!, 0);
      }
    for (let cursor = 0; cursor < this.queueLength; cursor++)
      this.scanObject(this.queue[cursor]!, 0, 0x1_0000_0000, major ? 1 : 0);
    if (major) {
      for (let index = 0; index < this.occupiedCount; ) {
        const slot = this.occupied[index]!;
        const segment = this.segments[slot >>> 10]!;
        const local = slot & 1023;
        if (!segment.young[local] && segment.marked[local] !== this.epoch)
          this.freeObject(slot, true);
        else index++;
      }
    }
    for (const pageID of this.nurseryPages) {
      const from = this.pages[pageID]!;
      const to = this.pages[from.partner]!;
      to.kind = YOUNG_PAGE;
      to.used = 0;
    }
    for (let index = 0; index < this.youngCount; ) {
      const slot = this.youngSlots[index]!;
      const segment = this.segments[slot >>> 10]!;
      const local = slot & 1023;
      if (segment.marked[local] !== this.epoch) {
        this.freeObject(slot, true);
        continue;
      }
      const from = this.pages[segment.page[local]!]!;
      const size = segment.size[local]!;
      let targetID = from.partner;
      let target = this.pages[targetID]!;
      let offset = target.used;
      let promoted = false;
      const classIndex = Math.max(3, Math.ceil(Math.log2(size))) - 3;
      const available = this.oldAvailable[classIndex]!;
      if (segment.age[local]! >= 1 && available.length > 0) {
        targetID = available[available.length - 1]!;
        target = this.pages[targetID]!;
        const entry = target.free[--target.freeCount]!;
        offset = entry * target.sizeClass;
        target.entries[entry] = slot;
        target.used += target.sizeClass;
        if (target.freeCount === 0) available.pop();
        segment.young[local] = 0;
        this.youngBytes -= segment.logical[local]!;
        this.removeYoung(slot);
        promoted = true;
      } else target.used += size;
      target.bytes.set(
        from.bytes.subarray(segment.offset[local]!, segment.offset[local]! + size),
        offset,
      );
      segment.page[local] = targetID;
      segment.offset[local] = offset;
      segment.age[local] = Math.min(255, segment.age[local]! + 1);
      this.copiedBytes += BigInt(size);
      if (promoted) {
        this.promotedBytes += BigInt(size);
        this.dirtyRegion(segment, local, 0, size);
      } else index++;
    }
    for (let index = 0; index < this.nurseryPages.length; index++) {
      const fromID = this.nurseryPages[index]!;
      const from = this.pages[fromID]!;
      this.nurseryPages[index] = from.partner;
      from.kind = SPARE_PAGE;
      from.used = 0;
      if (this.poisonCollected) from.bytes.fill(0xdd);
    }
    let cardCount = 0;
    for (let index = 0; index < this.cardCount; index++) {
      const pageID = this.cardPages[index]!;
      const card = this.cardIndices[index]!;
      if (this.scanCard(pageID, card, 2)) {
        this.cardPages[cardCount] = pageID;
        this.cardIndices[cardCount++] = card;
      } else this.pages[pageID]!.cards[card] = 0;
    }
    this.cardCount = cardCount;
    if (major) {
      this.lastMajorReachableBytes = this.occupiedBytes;
      this.majorCollections++;
      this.releaseEmptyPages();
    } else this.minorCollections++;
    // Minor collection must stay bounded by nursery/cards. The layout
    // directory is reclaimed only during a major collection; explicit owner
    // release still reclaims one layout in O(1).
    if (major) this.reclaimUnusedLayouts();
  }

  private mark(slot: number, generation: number, major: boolean): void {
    if (slot === 0 && generation === 0) return;
    const segment = this.segments[slot >>> 10];
    const local = slot & 1023;
    if (
      !Number.isInteger(slot) ||
      slot <= 0 ||
      slot > 0xffff_ffff ||
      !segment ||
      segment.state[local] !== PUBLISHED ||
      segment.generation[local] !== generation
    )
      throw new VMError("stale managed handle", "invalid_operation", "memory.handle");
    if ((!major && !segment.young[local]) || segment.marked[local] === this.epoch) return;
    segment.marked[local] = this.epoch;
    this.queue[this.queueLength++] = slot;
  }

  // mode 0 marks young, 1 marks all, 2 checks cards after relocation. Region
  // clipping skips all elements and reference offsets outside the dirty card.
  private scanObject(slot: number, start: number, end: number, mode: number): boolean {
    const segment = this.segments[slot >>> 10]!;
    const index = slot & 1023;
    const base = segment.offset[index]!;
    const layout = this.layouts[segment.layout[index]!]!;
    const count = segment.count[index]!;
    const first = Math.max(0, Math.floor((start - base) / layout.stride));
    const last = Math.min(count, Math.ceil((end - base) / layout.stride));
    const page = this.pages[segment.page[index]!]!;
    let young = false;
    for (let element = first; element < last; element++) {
      const origin = base + element * layout.stride;
      for (
        let ref = firstOffset(layout.references, start - origin - 7);
        ref < layout.references.length;
        ref++
      ) {
        const offset = layout.references[ref]!;
        const position = origin + offset;
        if (position >= end) break;
        young = this.visitReference(page, position, mode) || young;
      }
      for (
        let dynamic = firstOffset(layout.dynamics, start - origin - 15);
        dynamic < layout.dynamics.length;
        dynamic++
      ) {
        const offset = layout.dynamics[dynamic]!;
        const position = origin + offset;
        // A representation-tag write can activate an existing reference in
        // the following card, so both tag and ticket participate in clipping.
        if (position + 4 >= end) break;
        const representation = page.view.getUint32(position + 4, true);
        if (representation === 4 || representation === 5)
          young = this.visitReference(page, position + 8, mode) || young;
      }
    }
    return young;
  }

  private visitReference(page: PayloadPage, offset: number, mode: number): boolean {
    const slot = page.view.getUint32(offset, true);
    const generation = page.view.getUint32(offset + 4, true);
    if (slot === 0 && generation === 0) return false;
    const segment = this.segments[slot >>> 10];
    const local = slot & 1023;
    if (!segment || segment.state[local] !== PUBLISHED || segment.generation[local] !== generation)
      throw new VMError("stale managed handle", "invalid_operation", "memory.handle");
    if (mode !== 2) {
      this.scannedReferences++;
      this.mark(slot, generation, mode === 1);
    }
    return segment.young[local] !== 0;
  }

  private scanCard(pageID: number, card: number, mode: number): boolean {
    const page = this.pages[pageID]!;
    const start = card * CARD_BYTES;
    const end = Math.min(start + CARD_BYTES, page.bytes.length);
    const first = page.sizeClass ? Math.floor(start / page.sizeClass) : 0;
    const last = page.sizeClass
      ? Math.min(page.entries.length, Math.ceil(end / page.sizeClass))
      : 1;
    let young = false;
    for (let index = first; index < last; index++) {
      const slot = page.entries[index]!;
      if (slot) young = this.scanObject(slot, start, end, mode) || young;
    }
    return young;
  }

  private dirtyRegion(
    segment: DirectorySegment,
    index: number,
    offset: number,
    length: number,
  ): void {
    if (segment.young[index] || length === 0) return;
    const pageID = segment.page[index]!;
    const page = this.pages[pageID]!;
    const first = Math.floor((segment.offset[index]! + offset) / CARD_BYTES);
    const last = Math.floor((segment.offset[index]! + offset + length - 1) / CARD_BYTES);
    for (let card = first; card <= last; card++) {
      if (page.cards[card]) continue;
      page.cards[card] = 1;
      this.cardPages[this.cardCount] = pageID;
      this.cardIndices[this.cardCount++] = card;
    }
  }

  release(handle: GuestHandle): void {
    this.validate(handle);
    this.freeObject(handle.slot, true);
  }

  private freeObject(slot: number, published: boolean): void {
    const segment = this.segments[slot >>> 10]!;
    const index = slot & 1023;
    const layout = this.layouts[segment.layout[index]!]!;
    const page = this.pages[segment.page[index]!]!;
    if (!segment.young[index]) {
      if (page.sizeClass) {
        const entry = segment.offset[index]! / page.sizeClass;
        page.entries[entry] = 0;
        if (page.freeCount === 0)
          this.oldAvailable[Math.log2(page.sizeClass) - 3]!.push(segment.page[index]!);
        page.free[page.freeCount++] = entry;
        page.used -= page.sizeClass;
      } else {
        page.entries[0] = 0;
        page.used = 0;
      }
    }
    if (published) {
      this.onReclaim?.(slot, segment.generation[index]!);
      this.occupiedBytes -= segment.logical[index]!;
      this.reclaimedBytes += segment.logical[index]!;
      const last = this.occupied[--this.occupiedCount]!;
      this.occupied[segment.occupiedIndex[index]!] = last;
      this.segments[last >>> 10]!.occupiedIndex[last & 1023] = segment.occupiedIndex[index]!;
      if (segment.young[index]) {
        this.youngBytes -= segment.logical[index]!;
        this.removeYoung(slot);
      }
      this.roots.delete(slot);
    }
    segment.state[index] = FREE;
    if (layout.users > 0) {
      layout.users--;
    }
    segment.logical[index] = 0n;
    if (segment.generation[index] !== 0xffff_ffff) this.freeSlots[this.freeCount++] = slot;
  }

  private removeYoung(slot: number): void {
    const segment = this.segments[slot >>> 10]!;
    const index = slot & 1023;
    const last = this.youngSlots[--this.youngCount]!;
    this.youngSlots[segment.youngIndex[index]!] = last;
    this.segments[last >>> 10]!.youngIndex[last & 1023] = segment.youngIndex[index]!;
  }

  private validate(handle: GuestHandle, state = PUBLISHED): DirectorySegment {
    const segment = this.segments[handle.slot >>> 10];
    if (
      !Number.isInteger(handle.slot) ||
      handle.slot <= 0 ||
      handle.slot > 0xffff_ffff ||
      !segment ||
      segment.state[handle.slot & 1023] !== state ||
      segment.generation[handle.slot & 1023] !== handle.generation
    )
      throw new VMError("stale managed handle", "invalid_operation", "memory.handle");
    return segment;
  }

  private checkRange(
    segment: DirectorySegment,
    index: number,
    offset: number,
    length: number,
  ): void {
    if (
      !Number.isSafeInteger(offset) ||
      !Number.isSafeInteger(length) ||
      offset < 0 ||
      length < 0 ||
      offset + length > segment.size[index]!
    )
      throw new VMError("access outside managed payload", "internal", "memory");
  }

  private admitResident(bytes: bigint): void {
    const total = this.residentPageBytes + bytes;
    if (
      total > MAX_BYTES ||
      (this.limits.maxResidentBytes !== undefined && total > this.limits.maxResidentBytes)
    )
      throw new VMError("managed resident limit exceeded", "allocation_limit", "memory");
    this.residentPageBytes = total;
  }

  private growDirectory(): void {
    if (this.nextSlot >= 0xffff_ffff)
      throw new VMError("managed handle space exhausted", "allocation_limit", "memory");
    // 47 bytes of numeric metadata and four four-byte work vectors per slot.
    this.admitResident(BigInt(SEGMENT_SIZE * 63));
    this.segments.push(new DirectorySegment());
    const capacity = this.segments.length * SEGMENT_SIZE;
    const free = new Uint32Array(capacity);
    free.set(this.freeSlots);
    this.freeSlots = free;
    const occupied = new Uint32Array(capacity);
    occupied.set(this.occupied);
    this.occupied = occupied;
    const young = new Uint32Array(capacity);
    young.set(this.youngSlots);
    this.youngSlots = young;
    const queue = new Uint32Array(capacity);
    queue.set(this.queue);
    this.queue = queue;
  }

  private nurseryPage(size: number): number {
    for (const pageID of this.nurseryPages) {
      const page = this.pages[pageID]!;
      if (size <= page.bytes.length - page.used) return pageID;
    }
    if (this.nurseryCapacity + PAGE_BYTES > (this.limits.nurseryBytes ?? 4 * 1024 * 1024))
      throw new VMError("nursery requires collection", "collection_required", "memory");
    this.admitResident(BigInt(PAGE_BYTES * 2));
    const from = this.newPage(PAGE_BYTES, YOUNG_PAGE, 0, true);
    const to = this.newPage(PAGE_BYTES, SPARE_PAGE, 0, true);
    this.pages[from]!.partner = to;
    this.pages[to]!.partner = from;
    this.nurseryPages.push(from);
    this.nurseryCapacity += PAGE_BYTES;
    return from;
  }

  private newPage(size: number, kind: number, sizeClass: number, prepaid = false): number {
    const reusable = this.freePages[this.freePages.length - 1];
    const previousCards = reusable === undefined ? 0 : this.pages[reusable]!.cardCapacity;
    const cardCount = kind === OLD_PAGE || kind === LARGE_PAGE ? Math.ceil(size / CARD_BYTES) : 0;
    const entries = kind === OLD_PAGE ? size / sizeClass : kind === LARGE_PAGE ? 1 : 0;
    const cardGrowth = Math.max(0, cardCount - previousCards);
    const resident = BigInt(size + entries * (kind === OLD_PAGE ? 8 : 4) + cardCount);
    if (!prepaid) this.admitResident(resident + BigInt(cardGrowth * 8));
    const bytes = new Uint8Array(size);
    const page: PayloadPage = {
      bytes,
      view: new DataView(bytes.buffer),
      kind,
      used: 0,
      partner: 0,
      sizeClass,
      entries: new Uint32Array(entries),
      free: new Uint32Array(kind === OLD_PAGE ? entries : 0),
      freeCount: kind === OLD_PAGE ? entries : 0,
      cards: new Uint8Array(cardCount),
      cardCapacity: Math.max(cardCount, previousCards),
      resident,
    };
    for (let index = 0; index < page.freeCount; index++)
      page.free[index] = page.freeCount - index - 1;
    const id = this.freePages.pop() ?? this.pages.length;
    this.pages[id] = page;
    if (kind === OLD_PAGE) this.oldAvailable[Math.log2(sizeClass) - 3]!.push(id);
    if (cardGrowth > 0) {
      const pages = new Uint32Array(this.cardPages.length + cardGrowth);
      pages.set(this.cardPages);
      this.cardPages = pages;
      const indices = new Uint32Array(this.cardIndices.length + cardGrowth);
      indices.set(this.cardIndices);
      this.cardIndices = indices;
    }
    return id;
  }

  private releaseEmptyPages(): void {
    for (let id = 0; id < this.pages.length; id++) {
      const page = this.pages[id]!;
      if (page.used !== 0 || (page.kind !== OLD_PAGE && page.kind !== LARGE_PAGE)) continue;
      if (page.kind === OLD_PAGE) {
        const available = this.oldAvailable[Math.log2(page.sizeClass) - 3]!;
        // One empty promotion page per class is a bounded cache.
        if (available[0] === id) continue;
        const index = available.indexOf(id);
        if (index >= 0) available.splice(index, 1);
      }
      this.residentPageBytes -= page.resident;
      page.bytes = new Uint8Array();
      page.view = new DataView(page.bytes.buffer);
      page.entries = new Uint32Array();
      page.free = new Uint32Array();
      page.cards = new Uint8Array();
      page.kind = 0;
      page.resident = 0n;
      this.freePages.push(id);
    }
  }

  stats(): ManagedHeapStats {
    return {
      occupiedBytes: this.occupiedBytes,
      reservedBytes: this.reservedBytes,
      lastMajorReachableBytes: this.lastMajorReachableBytes,
      residentPageBytes: this.residentPageBytes,
      totalAllocatedBytes: this.totalAllocatedBytes,
      totalAllocations: this.totalAllocations,
      minorCollections: this.minorCollections,
      majorCollections: this.majorCollections,
      copiedBytes: this.copiedBytes,
      promotedBytes: this.promotedBytes,
      reclaimedBytes: this.reclaimedBytes,
      scannedRoots: this.scannedRoots,
      scannedCards: this.scannedCards,
      scannedReferences: this.scannedReferences,
    };
  }

  close(): void {
    this.closed = true;
    this.segments.length = 0;
    this.pages.length = 0;
    this.layouts.length = 0;
    this.layoutKeys.clear();
    this.freeLayouts.length = 0;
    this.roots.clear();
    this.nurseryPages.length = 0;
    this.freePages.length = 0;
    for (const pages of this.oldAvailable) pages.length = 0;
    this.freeSlots = new Uint32Array();
    this.occupied = new Uint32Array();
    this.youngSlots = new Uint32Array();
    this.queue = new Uint32Array();
    this.cardPages = new Uint32Array();
    this.cardIndices = new Uint32Array();
    this.occupiedBytes = 0n;
    this.youngBytes = 0n;
    this.reservedBytes = 0n;
    this.residentPageBytes = 0n;
  }
}
