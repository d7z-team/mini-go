import { Kind, PrimitiveKind, STORAGE_PRIMITIVE_BYTES } from "./contract.generated";
import type { GuestHandle, ManagedHeap, ManagedLayout } from "./managed-heap";
import { VMError } from "./json";
import {
  type Address,
  type AddressPath,
  type RuntimeType,
  type SliceValue,
  type TypeNode,
  type Value,
  nilValue,
  zero,
} from "./value";

/** A physical layout used by the managed payload, independent of reflect ABI. */
export interface StorageLayout {
  readonly kind: number;
  readonly size: number;
  readonly logical: bigint;
  readonly alignment: number;
  readonly fields: readonly StorageField[];
  readonly element?: StorageLayout;
  readonly elementType?: RuntimeType;
  readonly length?: number;
  readonly references: readonly number[];
  readonly dynamics: readonly number[];
}

export interface StorageField {
  readonly name: string;
  readonly type: RuntimeType;
  readonly embedded?: boolean;
  readonly offset: number;
  readonly layout: StorageLayout;
}

export interface StorageOwner {
  readonly heap: ManagedHeap;
  registerLayout?(layout: ManagedLayout): number;
  retainLayout?(layout: number): void;
  releaseLayout?(layout: number): void;
  allocate(layout: number, count: number, logicalBytes: bigint): GuestHandle;
  release(handle: GuestHandle): void;
  encode(value: Value, layout: StorageLayout, type: RuntimeType): Uint8Array;
  /** Encode directly into caller-owned storage when the layout is already known. */
  encodeInto(
    value: Value,
    layout: StorageLayout,
    type: RuntimeType,
    target: Uint8Array,
  ): Uint8Array;
  decode(type: RuntimeType, layout: StorageLayout, bytes: Uint8Array): Value;
  copyAggregate(value: Value): Value;
  retain?(layout: StorageLayout, bytes: Uint8Array): void;
  beginConstruction?(): () => void;
  /**
   * Groups newly allocated payloads until their owning container publishes
   * the references. A failed container mutation aborts the group so an
   * unreachable child does not consume the next allocation attempt.
   */
  beginAllocation?(): { commit(): void; abort(): void };
  retainDynamic?(layout: StorageLayout, bytes: Uint8Array): () => void;
}

/**
 * A view over one typed record.  The view carries only a storage location and
 * its prepared layout; it never owns a guest object graph.  Frame storage and
 * managed pages implement the same tiny byte-range interface so aggregate
 * consumers can address a field or element without decoding the record.
 */
export interface PackedStorage {
  readonly handle?: GuestHandle;
  read(offset: number, length: number): Uint8Array;
  write?(offset: number, bytes: Uint8Array): void;
}

export interface PackedAggregate {
  readonly storage: PackedStorage;
  readonly offset: number;
  readonly layout: StorageLayout;
  readonly type: RuntimeType;
  readonly owner: StorageOwner;
  readonly commit?: () => void;
}

export function packedAggregate(value: Value): PackedAggregate | undefined {
  if (value.kind !== "array" && value.kind !== "struct") return undefined;
  const candidate = value.value;
  if (!candidate || typeof candidate !== "object") return undefined;
  const view = candidate as Partial<PackedAggregate>;
  return view.storage && view.layout && view.owner && typeof view.offset === "number"
    ? (candidate as PackedAggregate)
    : undefined;
}

export function packedRecordValue(
  type: RuntimeType,
  layout: StorageLayout,
  owner: StorageOwner,
  storage: PackedStorage,
  offset: number,
): Value {
  const kind = aggregateKind(type);
  if (!kind) return owner.decode(type, layout, storage.read(offset, layout.size));
  return {
    type,
    kind,
    value: { storage, offset, layout, type, owner } satisfies PackedAggregate,
  };
}

export function aggregateKind(type: RuntimeType): "array" | "struct" | undefined {
  const kind = type.registry.underlying(type).ref.kind;
  if (kind === Kind.Array || kind === Kind.Tuple) return "array";
  if (kind === Kind.Struct) return "struct";
  return undefined;
}

function packedChild(
  parent: PackedAggregate,
  type: RuntimeType,
  layout: StorageLayout,
  offset: number,
): Value {
  const kind = aggregateKind(type);
  if (kind)
    return {
      type,
      kind,
      value: {
        storage: parent.storage,
        offset: parent.offset + offset,
        layout,
        type,
        owner: parent.owner,
        ...(parent.commit ? { commit: parent.commit } : {}),
      } satisfies PackedAggregate,
    };
  return parent.owner.decode(
    type,
    layout,
    parent.storage.read(parent.offset + offset, layout.size),
  );
}

export function packedField(value: Value, name: string): Value | undefined {
  const parent = packedAggregate(value);
  if (!parent) return undefined;
  const field = parent.layout.fields.find((candidate) => candidate.name === name);
  if (field) return packedChild(parent, field.type, field.layout, field.offset);
  for (const embedded of parent.layout.fields) {
    if (!embedded.embedded) continue;
    const nested = packedChild(parent, embedded.type, embedded.layout, embedded.offset);
    const result = packedField(nested, name);
    if (result) return result;
  }
  return undefined;
}

export function packedIndex(value: Value, index: number): Value | undefined {
  const parent = packedAggregate(value);
  if (!parent || !parent.layout.element || !parent.layout.elementType) return undefined;
  if (!Number.isSafeInteger(index) || index < 0 || index >= (parent.layout.length ?? 0))
    return undefined;
  return packedChild(
    parent,
    parent.layout.elementType,
    parent.layout.element,
    index * parent.layout.element.size,
  );
}

export function writePackedField(value: Value, name: string, assigned: Value): boolean {
  const parent = packedAggregate(value);
  if (!parent || !parent.storage.write) return false;
  const field = parent.layout.fields.find((candidate) => candidate.name === name);
  if (!field) return false;
  parent.storage.write(
    parent.offset + field.offset,
    parent.owner.encode(assigned, field.layout, field.type),
  );
  parent.commit?.();
  return true;
}

export function writePackedIndex(value: Value, index: number, assigned: Value): boolean {
  const parent = packedAggregate(value);
  if (!parent || !parent.layout.element || !parent.layout.elementType || !parent.storage.write)
    return false;
  if (!Number.isSafeInteger(index) || index < 0 || index >= (parent.layout.length ?? 0))
    return false;
  parent.storage.write(
    parent.offset + index * parent.layout.element.size,
    parent.owner.encode(assigned, parent.layout.element, parent.layout.elementType),
  );
  parent.commit?.();
  return true;
}

export function packedLength(value: Value): number | undefined {
  const layout = packedAggregate(value)?.layout;
  return layout ? (layout.length ?? layout.fields.length) : undefined;
}

export function packedFields(value: Value): IterableIterator<[string, Value]> | undefined {
  const parent = packedAggregate(value);
  if (!parent) return undefined;
  return (function* (): IterableIterator<[string, Value]> {
    for (const field of parent.layout.fields)
      yield [field.name, packedChild(parent, field.type, field.layout, field.offset)];
  })();
}

export function packedItems(value: Value): IterableIterator<Value> | undefined {
  const parent = packedAggregate(value);
  if (!parent || !parent.layout.element || !parent.layout.elementType) return undefined;
  return (function* (): IterableIterator<Value> {
    for (let index = 0; index < (parent.layout.length ?? 0); index++) {
      yield packedChild(
        parent,
        parent.layout.elementType!,
        parent.layout.element!,
        index * parent.layout.element!.size,
      );
    }
  })();
}

export function* storageReferences(
  layout: StorageLayout,
  view: DataView,
  origin = 0,
): IterableIterator<GuestHandle> {
  for (const offset of layout.references) {
    const slot = view.getUint32(origin + offset, true);
    const generation = view.getUint32(origin + offset + 4, true);
    if (slot || generation) yield { slot, generation };
  }
  for (const offset of layout.dynamics) {
    const tag = view.getUint32(origin + offset + 4, true);
    if (tag !== 4 && tag !== 5) continue;
    const slot = view.getUint32(origin + offset + 8, true);
    const generation = view.getUint32(origin + offset + 12, true);
    if (slot || generation) yield { slot, generation };
  }
}

export interface StorageLocation {
  readonly handle: GuestHandle;
  readonly offset: number;
  readonly type: RuntimeType;
  readonly layout?: StorageLayout;
}

export function remapStorageLayout(
  layout: StorageLayout,
  remap: (type: RuntimeType) => RuntimeType,
): StorageLayout {
  const result: StorageLayout = {
    ...layout,
    fields: layout.fields.map((field) => ({
      ...field,
      type: remap(field.type),
      layout: remapStorageLayout(field.layout, remap),
    })),
    ...(layout.elementType ? { elementType: remap(layout.elementType) } : {}),
    ...(layout.element ? { element: remapStorageLayout(layout.element, remap) } : {}),
  };
  return result;
}

/** Resolve a nested field/index without allocating a Value or Address path. */
export function fieldLocation(
  location: StorageLocation,
  layout: StorageLayout,
  name: string,
): StorageLocation {
  const field = layout.fields.find((candidate) => candidate.name === name);
  if (!field) {
    for (const embedded of layout.fields) {
      if (!embedded.embedded) continue;
      try {
        return fieldLocation(
          {
            handle: location.handle,
            offset: location.offset + embedded.offset,
            type: embedded.type,
          },
          embedded.layout,
          name,
        );
      } catch (error) {
        if (!(error instanceof Error) || !error.message.startsWith("unknown storage field"))
          throw error;
      }
    }
    throw new Error(`unknown storage field ${name}`);
  }
  return { handle: location.handle, offset: location.offset + field.offset, type: field.type };
}

function fieldLayout(layout: StorageLayout, name: string): StorageLayout | undefined {
  const field = layout.fields.find((candidate) => candidate.name === name);
  if (field) return field.layout;
  for (const embedded of layout.fields) {
    if (!embedded.embedded) continue;
    const nested = fieldLayout(embedded.layout, name);
    if (nested) return nested;
  }
  return undefined;
}

export function indexLocation(
  location: StorageLocation,
  layout: StorageLayout,
  index: number,
): StorageLocation {
  if (!layout.element || !Number.isSafeInteger(index) || index < 0 || index >= (layout.length ?? 0))
    throw new VMError("index out of range", "panic", "index");
  if (!layout.elementType) throw new Error("storage element type is missing");
  return {
    handle: location.handle,
    offset: location.offset + index * layout.element.size,
    type: layout.elementType,
  };
}

/** Resolve an address path to the same physical location used by the managed
 * record. The path is retained at the language boundary, while the
 * heap edge stores only the owner ticket and byte offset. */
export function resolveStorageLocation(
  location: StorageLocation,
  layout: StorageLayout,
  path: readonly AddressPath[],
): StorageLocation {
  let current = location;
  let currentLayout = layout;
  for (const segment of path) {
    if (segment.kind === "typeView") {
      current = { ...current, type: segment.type };
      continue;
    }
    if (segment.kind === "field") {
      current = fieldLocation(current, currentLayout, segment.name);
      currentLayout = fieldLayout(currentLayout, segment.name)!;
      continue;
    }
    if (segment.kind === "index") {
      current = indexLocation(current, currentLayout, segment.index);
      currentLayout = currentLayout.element!;
      continue;
    }
    if (segment.kind === "arrayView") {
      if (!currentLayout.element || !currentLayout.elementType)
        throw new Error("storage array view is missing an element layout");
      if (
        !Number.isSafeInteger(segment.start) ||
        segment.start < 0 ||
        segment.start + segment.length > (currentLayout.length ?? 0)
      )
        throw new Error("storage array view is out of range");
      current = {
        handle: current.handle,
        offset: current.offset + segment.start * currentLayout.element.size,
        type: segment.type,
      };
      currentLayout = prepareArrayStorageLayout(currentLayout.elementType, segment.length);
      continue;
    }
  }
  return { ...current, layout: currentLayout };
}

/** Reconstruct a language address path for a decoded physical location. */
export function pathForStorageOffset(
  layout: StorageLayout,
  rootType: RuntimeType,
  targetType: RuntimeType,
  offset: number,
): AddressPath[] | undefined {
  if (offset === 0 && rootType.registry.identical(rootType, targetType)) return [];
  if (offset === 0 && sameStorageShape(layout, prepareStorageLayout(targetType)))
    return [{ kind: "typeView", type: targetType }];
  const target = targetType.registry.underlying(targetType);
  if (target.ref.kind === Kind.Slice && layout.elementType && layout.element) {
    const element = target.registry.element(target);
    const start = offset / layout.element.size;
    if (
      element &&
      element.registry.identical(element, layout.elementType) &&
      Number.isInteger(start) &&
      start >= 0 &&
      start <= (layout.length ?? 0)
    )
      return start === 0
        ? []
        : [{ kind: "arrayView", start, length: (layout.length ?? 0) - start, type: targetType }];
  }
  if (layout.element && layout.elementType && target.ref.kind === Kind.Array) {
    const node = target.registry.node(target);
    const length = target.dynamic?.length ?? node?.length;
    const element =
      target.dynamic?.elem ?? (node?.elem ? target.registry.runtime(node.elem) : undefined);
    const start = offset / layout.element.size;
    if (
      length !== undefined &&
      element &&
      Number.isInteger(start) &&
      start >= 0 &&
      start + length <= (layout.length ?? 0) &&
      element.registry.identical(element, layout.elementType)
    )
      return [{ kind: "arrayView", start, length, type: targetType }];
  }
  if (offset < 0 || offset > layout.size) return undefined;
  for (const field of layout.fields) {
    if (
      offset < field.offset ||
      offset > field.offset + field.layout.size ||
      (offset === field.offset + field.layout.size && field.layout.size !== 0)
    )
      continue;
    const nested = pathForStorageOffset(
      field.layout,
      field.type,
      targetType,
      offset - field.offset,
    );
    if (nested) return [{ kind: "field", name: field.name }, ...nested];
  }
  if (layout.element && layout.elementType && layout.length !== undefined) {
    const index = layout.element.size === 0 ? 0 : Math.floor(offset / layout.element.size);
    if (index < layout.length) {
      const nested = pathForStorageOffset(
        layout.element,
        layout.elementType,
        targetType,
        offset - index * layout.element.size,
      );
      if (nested) return [{ kind: "index", index }, ...nested];
    }
  }
  return undefined;
}

function sameStorageShape(left: StorageLayout, right: StorageLayout): boolean {
  if (
    left.kind !== right.kind ||
    left.size !== right.size ||
    left.fields.length !== right.fields.length ||
    left.length !== right.length
  )
    return false;
  if (left.element || right.element)
    return Boolean(left.element && right.element && sameStorageShape(left.element, right.element));
  return left.fields.every((field, index) => {
    const other = right.fields[index]!;
    return (
      field.name === other.name &&
      field.offset === other.offset &&
      sameStorageShape(field.layout, other.layout)
    );
  });
}

export interface StorageCodecContext {
  readonly writeReference: (offset: number, handle: GuestHandle | undefined) => void;
  readonly readReference: (offset: number) => GuestHandle | undefined;
  readonly encodeBytes?: (bytes: Uint8Array) => StorageLocation | undefined;
  readonly decodeBytes?: (location: StorageLocation, length: number) => Uint8Array;
  readonly encodeReference?: (value: Value) => GuestHandle | undefined;
  readonly encodeAddress?: (address: Address, type: RuntimeType) => StorageLocation | undefined;
  readonly decodeAddress?: (location: StorageLocation) => Address;
  readonly resolveValue?: (handle: GuestHandle, type: RuntimeType) => Value;
  readonly typeID?: (type: RuntimeType) => number;
  readonly typeByID?: (id: number) => RuntimeType;
  readonly encodeBox?: (value: Value) => GuestHandle;
  readonly decodeBox?: (handle: GuestHandle, type: RuntimeType) => Value;
}

function dynamicType(type: RuntimeType): boolean {
  const underlying = type.registry.underlying(type);
  return (
    underlying.ref.kind === Kind.Any ||
    underlying.ref.kind === Kind.Interface ||
    (underlying.ref.kind === Kind.Primitive &&
      underlying.ref.primitive === PrimitiveKind.PrimitiveError)
  );
}

const WORD_BYTES = 8;

function align(value: number, alignment: number): number {
  const size = Math.ceil(value / alignment) * alignment;
  if (!Number.isSafeInteger(size) || size > 0xffff_fff8)
    throw new Error("storage layout size overflow");
  return size;
}

function primitiveSize(kind: number): number {
  switch (kind) {
    case PrimitiveKind.PrimitiveBool:
    case PrimitiveKind.PrimitiveUint8:
    case PrimitiveKind.PrimitiveInt8:
      return 1;
    case PrimitiveKind.PrimitiveUint16:
    case PrimitiveKind.PrimitiveInt16:
      return 2;
    case PrimitiveKind.PrimitiveUint32:
    case PrimitiveKind.PrimitiveInt32:
    case PrimitiveKind.PrimitiveFloat32:
      return 4;
    case PrimitiveKind.PrimitiveComplex64:
      return 8;
    case PrimitiveKind.PrimitiveComplex128:
      return 16;
    case PrimitiveKind.PrimitiveString:
      return 24;
    default:
      return 8;
  }
}

interface RuntimeField {
  readonly name: string;
  readonly type: RuntimeType;
  readonly embedded?: boolean;
}

function fieldsOf(type: RuntimeType, node: TypeNode | undefined): readonly RuntimeField[] {
  return (
    type.dynamic?.fields ??
    (node?.fields ?? []).map((field) => ({
      name: field.name,
      type: type.registry.runtime(field.type),
      tag: field.tag,
      embedded: field.embedded,
    }))
  );
}

/** Build one stable physical layout. Recursive reference types stop at a descriptor. */
const preparedStorageLayouts = new WeakMap<RuntimeType, StorageLayout>();

export function prepareStorageLayout(type: RuntimeType, active = new Set<string>()): StorageLayout {
  if (active.size === 0) {
    const cached = preparedStorageLayouts.get(type);
    if (cached) return cached;
  }
  const layout = buildStorageLayout(type, active);
  if (active.size === 0) preparedStorageLayouts.set(type, layout);
  return layout;
}

function buildStorageLayout(type: RuntimeType, active: Set<string>): StorageLayout {
  const underlying = type.registry.underlying(type);
  const node = underlying.registry.node(underlying);
  const kind = underlying.dynamic?.kind ?? node?.kind ?? underlying.ref.kind;
  const identity = `${underlying.module}\0${underlying.ref.node || underlying.registry.key(underlying)}`;
  if (active.has(identity)) return descriptorLayout(kind);
  const nextActive = new Set(active).add(identity);

  if (dynamicType(type))
    return {
      kind,
      size: 24,
      logical: 24n,
      alignment: 8,
      fields: [],
      references: [],
      dynamics: [0],
    };

  if (kind === Kind.Primitive)
    return primitiveLayout(underlying.ref.primitive, primitiveSize(underlying.ref.primitive));
  if (kind === Kind.Array) {
    const element =
      underlying.dynamic?.elem ?? (node?.elem ? underlying.registry.runtime(node.elem) : undefined);
    const length = underlying.dynamic?.length ?? node?.length ?? 0;
    if (!element) return descriptorLayout(kind);
    return aggregateLayout(kind, element, prepareStorageLayout(element, nextActive), length);
  }
  if (kind === Kind.Struct) {
    let offset = 0;
    let maxAlignment = 1;
    const fields: StorageField[] = [];
    for (const field of fieldsOf(underlying, node)) {
      const fieldLayout = prepareStorageLayout(field.type, nextActive);
      offset = align(offset, fieldLayout.alignment);
      fields.push({
        name: field.name,
        type: field.type,
        ...(field.embedded !== undefined ? { embedded: field.embedded } : {}),
        offset,
        layout: fieldLayout,
      });
      offset += fieldLayout.size;
      maxAlignment = Math.max(maxAlignment, fieldLayout.alignment);
    }
    return finalizeLayout(kind, offset, maxAlignment, fields);
  }
  if (kind === Kind.Tuple) {
    const fields = (node?.tuple ?? []).map((item, index) => ({
      name: String(index),
      type: underlying.registry.runtime(item),
      tag: "",
      embedded: false,
    }));
    let offset = 0;
    let maxAlignment = 1;
    const prepared: StorageField[] = [];
    for (const field of fields) {
      const fieldLayout = prepareStorageLayout(field.type, nextActive);
      offset = align(offset, fieldLayout.alignment);
      prepared.push({ name: field.name, type: field.type, offset, layout: fieldLayout });
      offset += fieldLayout.size;
      maxAlignment = Math.max(maxAlignment, fieldLayout.alignment);
    }
    return finalizeLayout(kind, offset, maxAlignment, prepared);
  }
  if (kind === Kind.Slice) return descriptorLayout(kind, 32, [0]);
  if (kind === Kind.Pointer) return descriptorLayout(kind, 24, [0]);
  if (kind === Kind.Map || kind === Kind.Function || kind === Kind.Waitable)
    return descriptorLayout(kind, 8, [0]);
  return descriptorLayout(kind);
}

export function prepareArrayStorageLayout(element: RuntimeType, length: number): StorageLayout {
  return aggregateLayout(Kind.Array, element, prepareStorageLayout(element), length);
}

function primitiveLayout(kind: number, size: number): StorageLayout {
  return {
    kind,
    size,
    logical: BigInt(STORAGE_PRIMITIVE_BYTES[kind] ?? 0),
    alignment: Math.max(1, Math.min(size, WORD_BYTES)),
    fields: [],
    references:
      kind === PrimitiveKind.PrimitiveString || kind === PrimitiveKind.PrimitiveFunction ? [0] : [],
    dynamics: [],
  };
}

function descriptorLayout(
  kind: number,
  size = WORD_BYTES,
  references: number[] = [],
): StorageLayout {
  return {
    kind,
    size,
    logical: BigInt(size),
    alignment: Math.min(size, WORD_BYTES),
    fields: [],
    references,
    dynamics: [],
  };
}

function aggregateLayout(
  kind: number,
  elementType: RuntimeType,
  element: StorageLayout,
  length: number,
): StorageLayout {
  const size = element.size * length;
  if (
    !Number.isSafeInteger(size) ||
    size > 0xffff_fff8 ||
    !Number.isSafeInteger(length) ||
    length < 0
  )
    throw new Error("storage array size overflow");
  const references: number[] = [];
  const dynamics: number[] = [];
  if (element.references.length || element.dynamics.length)
    for (let index = 0; index < length; index++) {
      for (const offset of element.references) references.push(index * element.size + offset);
      for (const offset of element.dynamics) dynamics.push(index * element.size + offset);
    }
  return {
    kind,
    size,
    logical: element.logical * BigInt(length),
    alignment: element.alignment,
    fields: [],
    element,
    elementType,
    length,
    references,
    dynamics,
  };
}

function finalizeLayout(
  kind: number,
  size: number,
  alignment: number,
  fields: readonly StorageField[],
): StorageLayout {
  const references: number[] = [];
  const dynamics: number[] = [];
  for (const field of fields) {
    for (const offset of field.layout.references) references.push(field.offset + offset);
    for (const offset of field.layout.dynamics) dynamics.push(field.offset + offset);
  }
  return {
    kind,
    size: align(size, alignment),
    logical: fields.reduce((sum, field) => sum + field.layout.logical, 0n),
    alignment,
    fields,
    references,
    dynamics,
  };
}

function numberAt(value: Value): number {
  return value.kind === "float" ? (value.value as number) : Number(value.value as bigint);
}

function typedZeroAggregate(value: Value): boolean {
  const kind = value.type.registry.underlying(value.type).ref.kind;
  if (kind !== Kind.Array && kind !== Kind.Struct && kind !== Kind.Tuple) return false;
  const packed = packedAggregate(value);
  if (packed) {
    const bytes = packed.storage.read(packed.offset, packed.layout.size);
    for (const byte of bytes) if (byte !== 0) return false;
    return true;
  }
  switch (value.kind) {
    case "array":
      return (value.value as Value[]).every(typedZeroValue);
    case "struct":
      return [...(value.value as Map<string, Value>).values()].every(typedZeroValue);
    default:
      return false;
  }
}

function typedZeroValue(value: Value): boolean {
  switch (value.kind) {
    case "nil":
      return true;
    case "bool":
      return !value.value;
    case "int":
    case "uint":
      return value.value === 0n;
    case "float":
      return value.value === 0;
    case "complex":
      return (
        (value.value as { real: number; imag: number }).real === 0 &&
        (value.value as { real: number; imag: number }).imag === 0
      );
    case "array":
      return (value.value as Value[]).every(typedZeroValue);
    case "struct":
      return [...(value.value as Map<string, Value>).values()].every(typedZeroValue);
    case "interface":
      return typedZeroValue(value.value as Value);
    default:
      return false;
  }
}

function writeScalar(
  view: DataView,
  offset: number,
  type: RuntimeType,
  size: number,
  value: Value,
): void {
  const primitive = type.registry.underlying(type).ref.primitive;
  switch (primitive) {
    case PrimitiveKind.PrimitiveBool:
      view.setUint8(offset, value.value ? 1 : 0);
      break;
    case PrimitiveKind.PrimitiveFloat32:
      view.setFloat32(offset, numberAt(value), true);
      break;
    case PrimitiveKind.PrimitiveFloat64:
      view.setFloat64(offset, numberAt(value), true);
      break;
    case PrimitiveKind.PrimitiveComplex64: {
      const complex = value.value as { real: number; imag: number };
      view.setFloat32(offset, complex.real, true);
      view.setFloat32(offset + 4, complex.imag, true);
      break;
    }
    case PrimitiveKind.PrimitiveComplex128: {
      const complex = value.value as { real: number; imag: number };
      view.setFloat64(offset, complex.real, true);
      view.setFloat64(offset + 8, complex.imag, true);
      break;
    }
    default:
      if (size === 1) {
        if (value.kind === "uint") view.setUint8(offset, Number(value.value as bigint));
        else view.setInt8(offset, Number(value.value as bigint));
      } else if (size === 2) {
        if (value.kind === "uint") view.setUint16(offset, Number(value.value as bigint), true);
        else view.setInt16(offset, Number(value.value as bigint), true);
      } else if (size === 4) {
        if (value.kind === "uint") view.setUint32(offset, Number(value.value as bigint), true);
        else view.setInt32(offset, Number(value.value as bigint), true);
      } else if (value.kind === "uint") view.setBigUint64(offset, value.value as bigint, true);
      else view.setBigInt64(offset, value.value as bigint, true);
  }
}

function readScalar(view: DataView, offset: number, type: RuntimeType, size: number): Value {
  const primitive = type.registry.underlying(type).ref.primitive;
  if (primitive === PrimitiveKind.PrimitiveBool)
    return { type, kind: "bool", value: view.getUint8(offset) !== 0 };
  if (primitive === PrimitiveKind.PrimitiveFloat32)
    return { type, kind: "float", value: view.getFloat32(offset, true) };
  if (primitive === PrimitiveKind.PrimitiveFloat64)
    return { type, kind: "float", value: view.getFloat64(offset, true) };
  if (primitive === PrimitiveKind.PrimitiveComplex64)
    return {
      type,
      kind: "complex",
      value: { real: view.getFloat32(offset, true), imag: view.getFloat32(offset + 4, true) },
    };
  if (primitive === PrimitiveKind.PrimitiveComplex128)
    return {
      type,
      kind: "complex",
      value: { real: view.getFloat64(offset, true), imag: view.getFloat64(offset + 8, true) },
    };
  const unsigned =
    primitive >= PrimitiveKind.PrimitiveUint && primitive <= PrimitiveKind.PrimitiveUintptr;
  const value = unsigned
    ? size === 1
      ? BigInt(view.getUint8(offset))
      : size === 2
        ? BigInt(view.getUint16(offset, true))
        : size === 4
          ? BigInt(view.getUint32(offset, true))
          : view.getBigUint64(offset, true)
    : size === 1
      ? BigInt(view.getInt8(offset))
      : size === 2
        ? BigInt(view.getInt16(offset, true))
        : size === 4
          ? BigInt(view.getInt32(offset, true))
          : view.getBigInt64(offset, true);
  return {
    type,
    kind: unsigned ? "uint" : "int",
    value,
  };
}

/** Encode an aggregate into an already reserved payload. No allocation occurs here. */
export function encodeStorage(
  value: Value,
  layout: StorageLayout,
  target: Uint8Array,
  offset: number,
  context: StorageCodecContext,
  expectedType: RuntimeType = value.type,
): void {
  const view = new DataView(target.buffer, target.byteOffset, target.byteLength);
  const packed = packedAggregate(value);
  if (packed && !dynamicType(expectedType)) {
    if (packed.layout.size !== layout.size)
      throw new Error("packed aggregate layout size mismatch");
    target.set(packed.storage.read(packed.offset, layout.size), offset);
    return;
  }
  // The destination may be a reused frame lane. Nil descriptors, omitted
  // fields and shorter dynamic encodings must not retain the previous bytes.
  // Packed copies above cover the entire lane and preserve memmove semantics.
  target.fill(0, offset, offset + layout.size);
  if (dynamicType(expectedType)) {
    if (value.kind === "nil" && dynamicType(value.type)) {
      return;
    }
    const concrete = value.kind === "interface" ? (value.value as Value) : value;
    if (!context.typeID) throw new Error("dynamic storage requires a type registry");
    view.setUint32(offset, context.typeID(concrete.type), true);
    if (concrete.kind === "nil") {
      view.setUint32(offset + 4, 2, true);
    } else if (["bool", "int", "uint", "float", "complex"].includes(concrete.kind)) {
      view.setUint32(offset + 4, 1, true);
      const primitive = concrete.type.registry.underlying(concrete.type).ref.primitive;
      writeScalar(view, offset + 8, concrete.type, primitiveSize(primitive), concrete);
    } else if (typedZeroAggregate(concrete)) {
      // A non-nil aggregate with no payload edges has a stable typed zero
      // representation. Keeping it inline avoids allocating a boxed record.
      view.setUint32(offset + 4, 3, true);
    } else {
      const direct =
        concrete.kind === "map" ||
        concrete.kind === "channel" ||
        concrete.kind === "function" ||
        concrete.kind === "method" ||
        concrete.kind === "dynamicFunction";
      const handle = direct ? context.encodeReference?.(concrete) : context.encodeBox?.(concrete);
      if (!handle) throw new Error(`dynamic ${concrete.kind} storage has no managed payload`);
      view.setUint32(offset + 4, direct ? 4 : 5, true);
      context.writeReference(offset + 8, handle);
    }
    return;
  }
  if (value.kind === "nil") return;
  if (value.kind === "array") {
    const values = value.value as Value[];
    for (let index = 0; index < values.length; index++)
      if (layout.element)
        encodeStorage(
          values[index]!,
          layout.element,
          target,
          offset + index * layout.element.size,
          context,
          layout.elementType,
        );
    return;
  }
  if (value.kind === "struct") {
    const fields = value.value as Map<string, Value>;
    for (const field of layout.fields) {
      const nested = fields.get(field.name);
      if (nested)
        encodeStorage(nested, field.layout, target, offset + field.offset, context, field.type);
    }
    return;
  }
  const kind = value.type.registry.underlying(value.type).ref.kind;
  if (
    kind === Kind.Primitive &&
    value.kind !== "string" &&
    value.type.registry.underlying(value.type).ref.primitive !== PrimitiveKind.PrimitiveFunction
  ) {
    writeScalar(view, offset, value.type, layout.size, value);
    return;
  }
  if (value.kind === "pointer" || value.kind === "slice") {
    const address =
      value.kind === "pointer" ? (value.value as Address) : (value.value as SliceValue).storage;
    const location = context.encodeAddress?.(address, value.type);
    context.writeReference(offset, location?.handle);
    if (location) view.setUint32(offset + 8, location.offset, true);
    if (value.kind === "slice") {
      const slice = value.value as SliceValue;
      view.setUint32(offset + 12, slice.start, true);
      view.setUint32(offset + 16, slice.length, true);
      view.setUint32(offset + 20, slice.capacity, true);
    }
    return;
  }
  if (value.kind === "string") {
    const location = context.encodeBytes?.(value.value as Uint8Array);
    context.writeReference(offset, location?.handle);
    if (location) {
      view.setUint32(offset + 8, location.offset, true);
      view.setUint32(offset + 12, (value.value as Uint8Array).byteLength, true);
    }
    return;
  }
  if (
    value.kind === "map" ||
    value.kind === "channel" ||
    value.kind === "function" ||
    value.kind === "method" ||
    value.kind === "dynamicFunction"
  ) {
    context.writeReference(offset, context.encodeReference?.(value));
    return;
  }
  if (value.kind === "bytes") target.set(value.value as Uint8Array, offset);
}

/** Decode a typed record for a host/debug boundary or a non-hot-path value read. */
export function decodeStorage(
  type: RuntimeType,
  layout: StorageLayout,
  source: Uint8Array,
  offset: number,
  context: StorageCodecContext,
): Value {
  const kind = type.registry.underlying(type).ref.kind;
  if (dynamicType(type)) {
    const view = new DataView(source.buffer, source.byteOffset, source.byteLength);
    const id = view.getUint32(offset, true);
    if (id === 0) return nilValue(type);
    if (!context.typeByID) throw new Error("dynamic storage requires a type registry");
    const concrete = context.typeByID(id);
    const representation = view.getUint32(offset + 4, true);
    let value: Value;
    if (representation === 2) value = nilValue(concrete);
    else if (representation === 3) value = zero(concrete);
    else if (representation === 1) {
      const primitive = concrete.registry.underlying(concrete).ref.primitive;
      value = readScalar(view, offset + 8, concrete, primitiveSize(primitive));
    } else {
      const handle = context.readReference(offset + 8);
      if (!handle) throw new Error("dynamic storage has no reference");
      const decoded =
        representation === 4
          ? context.resolveValue?.(handle, concrete)
          : representation === 5
            ? context.decodeBox?.(handle, concrete)
            : undefined;
      if (!decoded) throw new Error("invalid dynamic storage representation");
      value = decoded;
    }
    return { type, kind: "interface", value };
  }
  if (layout.element && layout.length !== undefined && layout.elementType) {
    const values: Value[] = [];
    for (let index = 0; index < (layout.length ?? 0); index++)
      values.push(
        decodeStorage(
          layout.elementType,
          layout.element,
          source,
          offset + index * layout.element.size,
          context,
        ),
      );
    return { type, kind: "array", value: values };
  }
  if (kind === Kind.Struct) {
    const fields = new Map<string, Value>();
    for (const field of layout.fields)
      fields.set(
        field.name,
        decodeStorage(field.type, field.layout, source, offset + field.offset, context),
      );
    return { type, kind: "struct", value: fields };
  }
  if (
    kind === Kind.Primitive &&
    type.registry.underlying(type).ref.primitive !== PrimitiveKind.PrimitiveString &&
    type.registry.underlying(type).ref.primitive !== PrimitiveKind.PrimitiveFunction
  )
    return readScalar(
      new DataView(source.buffer, source.byteOffset, source.byteLength),
      offset,
      type,
      layout.size,
    );
  const reference = context.readReference(offset);
  const liveReference =
    reference && (reference.slot !== 0 || reference.generation !== 0) ? reference : undefined;
  if (kind === Kind.Pointer) {
    if (!liveReference || !context.decodeAddress) return { type, kind: "nil" };
    const targetType = type.registry.element(type) ?? type;
    return {
      type,
      kind: "pointer",
      value: context.decodeAddress({
        handle: liveReference,
        offset: new DataView(source.buffer, source.byteOffset, source.byteLength).getUint32(
          offset + 8,
          true,
        ),
        type: targetType,
        layout: prepareStorageLayout(targetType),
      }),
    };
  }
  if (kind === Kind.Slice) {
    const view = new DataView(source.buffer, source.byteOffset, source.byteLength);
    if (!liveReference || !context.decodeAddress) return nilValue(type);
    const start = view.getUint32(offset + 12, true);
    const length = view.getUint32(offset + 16, true);
    const capacity = view.getUint32(offset + 20, true);
    const element = type.registry.element(type);
    if (!element) throw new Error("slice storage has no element type");
    return {
      type,
      kind: "slice",
      value: {
        storage: context.decodeAddress({
          handle: liveReference,
          offset: view.getUint32(offset + 8, true),
          // A slice points at the backing record as a whole.  Its element
          // index is carried by start/length below and must not be encoded as
          // an address path, or every access would apply the index twice.
          type,
          layout: prepareArrayStorageLayout(element, start + capacity),
        }),
        start,
        length,
        capacity,
      },
    };
  }
  if (
    kind === Kind.Primitive &&
    type.registry.underlying(type).ref.primitive === PrimitiveKind.PrimitiveString
  ) {
    const view = new DataView(source.buffer, source.byteOffset, source.byteLength);
    const length = view.getUint32(offset + 12, true);
    if (liveReference && context.decodeBytes)
      return {
        type,
        kind: "string",
        value: context.decodeBytes(
          { handle: liveReference, offset: view.getUint32(offset + 8, true), type },
          length,
        ),
      };
    return { type, kind: "string", value: new Uint8Array(0) };
  }
  if (liveReference && context.resolveValue) return context.resolveValue(liveReference, type);
  return { type, kind: "nil" };
}
