import { Kind, PrimitiveKind } from "./contract.generated";
import {
  aggregateKind,
  packedAggregate,
  type PackedAggregate,
  type PackedStorage,
  prepareStorageLayout,
  storageReferences,
  type StorageLayout,
  type StorageOwner,
} from "./aggregate-storage";
import type { GuestHandle } from "./managed-heap";
import type { RuntimeType, Value } from "./value";

const KIND_NONE = 0;
const KIND_BOOL = 1;
const KIND_INT = 2;
const KIND_UINT = 3;
const KIND_FLOAT = 4;
const KIND_COMPLEX = 5;
const MAX_INLINE_BYTES = 128;
const MAX_INLINE_LOGICAL_BYTES = BigInt(MAX_INLINE_BYTES);

function isInlineLayout(layout: StorageLayout): boolean {
  // Descriptors (pointer, slice, map, interface, function, and resource
  // values) have their own fixed lane representation. The aggregate threshold
  // must never route one of them through copyAggregate.
  return (
    (layout.kind !== Kind.Array && layout.kind !== Kind.Struct && layout.kind !== Kind.Tuple) ||
    layout.logical <= MAX_INLINE_LOGICAL_BYTES
  );
}

class FramePayloadStorage implements PackedStorage {
  constructor(private readonly payload: Uint8Array) {}

  read(offset: number, length: number): Uint8Array {
    return this.payload.subarray(offset, offset + length);
  }

  write(offset: number, bytes: Uint8Array): void {
    this.payload.set(bytes, offset);
  }
}

function valueKind(value: Value): number {
  switch (value.kind) {
    case "bool":
      return KIND_BOOL;
    case "int":
      return KIND_INT;
    case "uint":
      return KIND_UINT;
    case "float":
      return KIND_FLOAT;
    case "complex":
      return KIND_COMPLEX;
    default:
      return KIND_NONE;
  }
}

function typeKind(type: RuntimeType): number {
  const underlying = type.registry.underlying(type);
  if (underlying.ref.kind !== Kind.Primitive) return KIND_NONE;
  switch (underlying.ref.primitive) {
    case PrimitiveKind.PrimitiveBool:
      return KIND_BOOL;
    case PrimitiveKind.PrimitiveInt:
    case PrimitiveKind.PrimitiveInt8:
    case PrimitiveKind.PrimitiveInt16:
    case PrimitiveKind.PrimitiveInt32:
    case PrimitiveKind.PrimitiveInt64:
      return KIND_INT;
    case PrimitiveKind.PrimitiveUint:
    case PrimitiveKind.PrimitiveUint8:
    case PrimitiveKind.PrimitiveUint16:
    case PrimitiveKind.PrimitiveUint32:
    case PrimitiveKind.PrimitiveUint64:
    case PrimitiveKind.PrimitiveUintptr:
      return KIND_UINT;
    case PrimitiveKind.PrimitiveFloat32:
    case PrimitiveKind.PrimitiveFloat64:
      return KIND_FLOAT;
    case PrimitiveKind.PrimitiveComplex64:
    case PrimitiveKind.PrimitiveComplex128:
      return KIND_COMPLEX;
    default:
      return KIND_NONE;
  }
}

/**
 * Typed storage for frame locals and temporary slots.
 *
 * Each slot has one storage class and one offset into the corresponding bank;
 * the bank lengths are the number of slots of that class, rather than the
 * total slot count. Aggregate and descriptor slots use inline byte ranges;
 * their root lanes are enumerated without materializing guest Values.
 */
export class FrameStorage {
  private readonly classes: Uint8Array;
  private readonly offsets: Uint32Array;
  private readonly initialized: Uint8Array;
  private readonly bools: Uint8Array;
  private readonly ints: BigInt64Array;
  private readonly uints: BigUint64Array;
  private readonly numbers: Float64Array;
  private readonly complexReal: Float64Array;
  private readonly complexImag: Float64Array;
  private readonly layouts: (StorageLayout | undefined)[];
  private readonly dynamicReleases: Array<(() => void) | undefined>;
  private readonly payload: Uint8Array;
  private readonly view: DataView;
  private readonly packedStorage: PackedStorage;

  /** Numeric bank capacity, including slot offsets and initialization bits. */
  static byteLength(
    types: readonly RuntimeType[],
    preparedLayouts?: readonly StorageLayout[],
  ): number {
    let bytes = types.length * 6;
    let payload = 0;
    for (let index = 0; index < types.length; index++) {
      const type = types[index]!;
      switch (typeKind(type)) {
        case KIND_NONE: {
          const layout = preparedLayouts?.[index] ?? prepareStorageLayout(type);
          payload =
            Math.ceil(payload / layout.alignment) * layout.alignment +
            (isInlineLayout(layout) ? layout.size : 8);
          break;
        }
        case KIND_BOOL:
          bytes++;
          break;
        case KIND_COMPLEX:
          bytes += 16;
          break;
        default:
          bytes += 8;
      }
    }
    return bytes + payload;
  }

  static logicalByteLength(layouts: readonly StorageLayout[]): bigint {
    return layouts.reduce(
      (bytes, layout) => bytes + (isInlineLayout(layout) ? layout.logical : 8n),
      0n,
    );
  }

  reset(): void {
    for (const release of this.dynamicReleases) release?.();
    this.dynamicReleases.fill(undefined);
    this.initialized.fill(0);
    this.payload.fill(0);
  }

  values(count = this.types.length): Value[] {
    return Array.from({ length: count }, (_, index) => this.read(index)!);
  }

  *allReferences(): IterableIterator<GuestHandle> {
    for (let index = 0; index < this.types.length; index++) yield* this.references(index);
  }

  constructor(
    private readonly types: readonly RuntimeType[],
    private readonly owner: StorageOwner,
    preparedLayouts?: readonly StorageLayout[],
  ) {
    this.classes = Uint8Array.from(types, typeKind);
    this.offsets = new Uint32Array(types.length);
    const counts = [0, 0, 0, 0, 0, 0];
    for (const kind of this.classes) counts[kind] = counts[kind]! + 1;
    const next = [0, 0, 0, 0, 0, 0];
    for (let index = 0; index < this.classes.length; index++)
      this.offsets[index] = next[this.classes[index]!]!++;
    this.initialized = new Uint8Array(types.length);
    this.bools = new Uint8Array(counts[KIND_BOOL]!);
    this.ints = new BigInt64Array(counts[KIND_INT]!);
    this.uints = new BigUint64Array(counts[KIND_UINT]!);
    this.numbers = new Float64Array(counts[KIND_FLOAT]!);
    this.complexReal = new Float64Array(counts[KIND_COMPLEX]!);
    this.complexImag = new Float64Array(counts[KIND_COMPLEX]!);
    let size = 0;
    this.layouts = types.map((type, index) => {
      if (this.classes[index] !== KIND_NONE) return undefined;
      const layout = preparedLayouts?.[index] ?? prepareStorageLayout(type);
      size = Math.ceil(size / layout.alignment) * layout.alignment;
      this.offsets[index] = size;
      size += isInlineLayout(layout) ? layout.size : 8;
      return layout;
    });
    this.payload = new Uint8Array(size);
    this.view = new DataView(this.payload.buffer);
    this.packedStorage = new FramePayloadStorage(this.payload);
    this.dynamicReleases = new Array(types.length);
  }

  read(index: number): Value | undefined {
    if (!this.initialized[index]) return undefined;
    const type = this.types[index];
    const kind = this.classes[index];
    const offset = this.offsets[index]!;
    if (!type) return undefined;
    switch (kind) {
      case KIND_BOOL:
        return { type, kind: "bool", value: this.bools[offset] !== 0 };
      case KIND_INT:
        return { type, kind: "int", value: this.ints[offset]! };
      case KIND_UINT:
        return { type, kind: "uint", value: this.uints[offset]! };
      case KIND_FLOAT:
        return { type, kind: "float", value: this.numbers[offset]! };
      case KIND_COMPLEX:
        return {
          type,
          kind: "complex",
          value: { real: this.complexReal[offset]!, imag: this.complexImag[offset]! },
        };
      default:
        const layout = this.layouts[index]!;
        const kind = aggregateKind(type);
        if (kind) {
          let storage = this.packedStorage;
          let origin = offset;
          if (!isInlineLayout(layout)) {
            const handle = {
              slot: this.view.getUint32(offset, true),
              generation: this.view.getUint32(offset + 4, true),
            };
            storage = {
              handle,
              read: (offset, length) => this.owner.heap.read(handle, offset, length),
              write: (offset, bytes) => this.owner.heap.write(handle, bytes, offset),
            };
            origin = 0;
          }
          return {
            type,
            kind,
            value: {
              storage,
              offset: origin,
              layout,
              type,
              owner: this.owner,
              ...(isInlineLayout(layout) ? { commit: () => this.refreshDynamic(index) } : {}),
            } satisfies PackedAggregate,
          };
        }
        return this.owner.decode(type, layout, this.payload.subarray(offset, offset + layout.size));
    }
  }

  private refreshDynamic(index: number): void {
    const layout = this.layouts[index];
    if (!layout) return;
    this.dynamicReleases[index]?.();
    this.dynamicReleases[index] = this.owner.retainDynamic?.(
      layout,
      this.payload.subarray(this.offsets[index]!, this.offsets[index]! + layout.size),
    );
  }

  write(index: number, value: Value): boolean {
    const kind = valueKind(value);
    if (this.classes[index] === KIND_NONE) {
      const layout = this.layouts[index]!;
      if (!isInlineLayout(layout)) {
        const copied = this.owner.copyAggregate(value).value as PackedAggregate;
        const handle = copied.storage.handle!;
        const offset = this.offsets[index]!;
        this.view.setUint32(offset, handle.slot, true);
        this.view.setUint32(offset + 4, handle.generation, true);
        this.initialized[index] = 1;
        return true;
      }
      const releaseConstruction = this.owner.beginConstruction?.();
      try {
        const offset = this.offsets[index]!;
        const target = this.payload.subarray(offset, offset + layout.size);
        let source = value;
        const packed = packedAggregate(value);
        if (
          packed &&
          packed.storage === this.packedStorage &&
          packed.offset < offset + layout.size &&
          packed.offset + packed.layout.size > offset
        ) {
          // A direct encode may otherwise overwrite a field before it has
          // been read (parallel assignment and self-assignment are valid).
          const bytes = Uint8Array.from(packed.storage.read(packed.offset, packed.layout.size));
          source = {
            ...value,
            value: {
              ...packed,
              storage: new FramePayloadStorage(bytes),
              offset: 0,
            } satisfies PackedAggregate,
          };
        }
        const encoded = this.owner.encodeInto(source, layout, this.types[index]!, target);
        const retainDynamic = this.owner.retainDynamic?.(layout, encoded);
        if (encoded !== target) target.set(encoded);
        this.dynamicReleases[index]?.();
        this.dynamicReleases[index] = retainDynamic;
        this.initialized[index] = 1;
        return true;
      } finally {
        releaseConstruction?.();
      }
    }
    if (kind === KIND_NONE || this.classes[index] !== kind) return false;
    const offset = this.offsets[index]!;
    this.initialized[index] = 1;
    switch (kind) {
      case KIND_BOOL:
        this.bools[offset] = value.value ? 1 : 0;
        break;
      case KIND_INT:
        this.ints[offset] = value.value as bigint;
        break;
      case KIND_UINT:
        this.uints[offset] = value.value as bigint;
        break;
      case KIND_FLOAT:
        this.numbers[offset] = value.value as number;
        break;
      case KIND_COMPLEX: {
        const complex = value.value as { real: number; imag: number };
        this.complexReal[offset] = complex.real;
        this.complexImag[offset] = complex.imag;
        break;
      }
    }
    return true;
  }

  clear(index: number): void {
    this.dynamicReleases[index]?.();
    this.dynamicReleases[index] = undefined;
    this.initialized[index] = 0;
    const layout = this.layouts[index];
    if (layout)
      this.payload.fill(
        0,
        this.offsets[index]!,
        this.offsets[index]! + (isInlineLayout(layout) ? layout.size : 8),
      );
  }

  initializeZero(index: number): void {
    this.clear(index);
    const type = this.types[index]!;
    const layout = this.layouts[index];
    if (layout && !isInlineLayout(layout)) {
      this.write(index, {
        type,
        kind: aggregateKind(type)!,
        value: {
          storage: new FramePayloadStorage(new Uint8Array(layout.size)),
          offset: 0,
          layout,
          type,
          owner: this.owner,
        } satisfies PackedAggregate,
      });
      return;
    }
    const offset = this.offsets[index]!;
    switch (this.classes[index]) {
      case KIND_BOOL:
        this.bools[offset] = 0;
        break;
      case KIND_INT:
        this.ints[offset] = 0n;
        break;
      case KIND_UINT:
        this.uints[offset] = 0n;
        break;
      case KIND_FLOAT:
        this.numbers[offset] = 0;
        break;
      case KIND_COMPLEX:
        this.complexReal[offset] = this.complexImag[offset] = 0;
        break;
    }
    this.initialized[index] = 1;
  }

  *references(index: number): IterableIterator<GuestHandle> {
    const layout = this.layouts[index];
    if (!this.initialized[index] || !layout) return;
    if (!isInlineLayout(layout)) {
      const offset = this.offsets[index]!;
      yield {
        slot: this.view.getUint32(offset, true),
        generation: this.view.getUint32(offset + 4, true),
      };
      return;
    }
    yield* storageReferences(layout, this.view, this.offsets[index]!);
  }

  /** Collect dynamic type IDs stored in initialized aggregate payload lanes. */
  collectDynamicTypeIDs(target: Set<number>): void {
    for (let index = 0; index < this.types.length; index++) {
      const layout = this.layouts[index];
      // Scan the raw lane even for a currently uninitialized slot. A frame
      // can be collected while a slot is being replaced; retaining the
      // descriptor for that transient byte pattern keeps a following decode
      // valid until reset clears the bank.
      if (!layout || !isInlineLayout(layout) || layout.dynamics.length === 0) continue;
      const base = this.offsets[index]!;
      for (const offset of layout.dynamics) {
        const id = this.view.getUint32(base + offset, true);
        target.add(id);
      }
    }
  }

  collectRuntimeTypes(target: Set<RuntimeType>): void {
    for (const type of this.types) target.add(type);
  }
}
