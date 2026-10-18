import type {
  HostAddress,
  HostData,
  HostValue,
  PathElement,
  Snapshot,
  TypeIdentity,
} from "./types";
import { Kind, PrimitiveKind, WIRE_FIELDS } from "./contract.generated";
import {
  JsonNumber,
  VMError,
  type JsonValue,
  boolean,
  fail,
  integer,
  schemaObject,
  string,
} from "./json";
import type { GuestHandle } from "./managed-heap";
import type { TypedChannel as Channel } from "./typed-channel";
import {
  fieldLocation,
  pathForStorageOffset,
  prepareArrayStorageLayout,
  prepareStorageLayout,
  packedAggregate,
  packedField,
  packedFields,
  packedIndex,
  packedItems,
  packedLength,
  writePackedField,
  writePackedIndex,
} from "./aggregate-storage";
import type { StorageLayout } from "./aggregate-storage";
export type { TypedChannel as Channel } from "./typed-channel";

const textEncoder = new TextEncoder();

export interface TypeKey {
  module_path: string;
  decl_id: string;
}

export interface TypeRef {
  kind: number;
  primitive: number;
  named: TypeKey;
  node: string;
}

export interface TypeNode {
  id: string;
  kind: number;
  name: string;
  primitive: number;
  identity: TypeKey;
  alias: boolean;
  alias_target?: TypeRef;
  underlying?: TypeRef;
  elem?: TypeRef;
  key?: TypeRef;
  length: number;
  direction: number;
  signature?: FunctionSignature;
  tuple: TypeRef[];
  fields: { name: string; type: TypeRef; tag: string; embedded: boolean }[];
  methods: {
    name: string;
    receiver: TypeRef;
    signature: FunctionSignature;
    function_id: string;
    module_path: string;
  }[];
}

export interface FunctionSignature {
  params: { type: TypeRef }[];
  results: TypeRef[];
  variadic: boolean;
}

export interface RuntimeType {
  module: string;
  ref: TypeRef;
  registry: TypeRegistry;
  dynamic?: DynamicType;
}

export interface DynamicType {
  kind: number;
  elem?: RuntimeType;
  key?: RuntimeType;
  length?: number;
  direction?: number;
  params?: RuntimeType[];
  results?: RuntimeType[];
  variadic?: boolean;
  fields?: { name: string; type: RuntimeType; tag: string; embedded: boolean }[];
}

/** Static facts used by prepared storage/accounting paths. */
export interface TypeFacts {
  readonly kind: number;
  readonly primitive: number;
  readonly containsReferences: boolean;
  readonly flat: boolean;
  readonly fixedSize: boolean;
  readonly depth: number;
  readonly fieldCount: number;
}

export class TypeRegistry {
  private readonly nodes = new Map<string, TypeNode>();
  private readonly named = new Map<string, TypeNode>();
  private readonly staticTypes = new Map<string, RuntimeType>();
  private readonly typeKeys = new WeakMap<object, string>();
  private readonly underlyingTypes = new WeakMap<RuntimeType, RuntimeType>();
  private readonly primitiveTypes: (RuntimeType | undefined)[] = [];
  private readonly typeFacts = new WeakMap<RuntimeType, TypeFacts>();

  constructor(
    readonly module: string,
    table: JsonValue | undefined,
  ) {
    const source =
      table === undefined
        ? Object.create(null)
        : schemaObject(table, `${module}.type_table`, WIRE_FIELDS.TypeTable);
    const nodes = source.nodes;
    if (nodes === undefined) return;
    if (!Array.isArray(nodes)) fail("expected type node array", `${module}.type_table.nodes`);
    for (let index = 0; index < nodes.length; index++) {
      const path = `${module}.type_table.nodes[${index}]`;
      const raw = schemaObject(nodes[index]!, path, WIRE_FIELDS.TypeNode);
      const identity = parseTypeKey(raw.identity, `${path}.identity`);
      const node: TypeNode = {
        id: string(raw.id, `${path}.id`),
        kind: integer(raw.kind, `${path}.kind`),
        name: string(raw.name, `${path}.name`, true),
        primitive: integer(raw.primitive, `${path}.primitive`, true),
        identity,
        alias: boolean(raw.alias, `${path}.alias`),
        length: integer(raw.length, `${path}.length`, true),
        direction: integer(raw.direction, `${path}.direction`, true),
        tuple: parseTypeRefs(raw.tuple, `${path}.tuple`),
        fields: parseFields(raw.fields, `${path}.fields`),
        methods: parseMethods(raw.methods, `${path}.methods`),
      };
      const aliasTarget = optionalTypeRef(raw.alias_target, `${path}.alias_target`);
      const underlying = optionalTypeRef(raw.underlying, `${path}.underlying`);
      const element = optionalTypeRef(raw.elem, `${path}.elem`);
      const key = optionalTypeRef(raw.key, `${path}.key`);
      if (aliasTarget) node.alias_target = aliasTarget;
      if (underlying) node.underlying = underlying;
      if (element) node.elem = element;
      if (key) node.key = key;
      if (raw.signature) node.signature = parseSignature(raw.signature, `${path}.signature`);
      if (this.nodes.has(node.id)) fail("duplicate type node", path);
      this.nodes.set(node.id, node);
      if (identity.module_path || identity.decl_id)
        this.named.set(`${identity.module_path}\0${identity.decl_id}`, node);
    }
  }

  /** Build a registry containing only the selected reachable nodes. */
  static detached(module: string, nodes: readonly TypeNode[]): TypeRegistry {
    const registry = new TypeRegistry(module, undefined);
    for (const node of nodes) {
      registry.nodes.set(node.id, node);
      if (node.identity.module_path || node.identity.decl_id)
        registry.named.set(`${node.identity.module_path}\0${node.identity.decl_id}`, node);
    }
    return registry;
  }

  nodeByID(id: string): TypeNode | undefined {
    return this.nodes.get(id);
  }

  runtime(ref: TypeRef): RuntimeType {
    if (ref.kind === Kind.Primitive && Number.isSafeInteger(ref.primitive) && ref.primitive >= 0) {
      const primitive = this.primitiveTypes[ref.primitive];
      if (primitive) return primitive;
    }
    const key = `${ref.kind}\0${ref.primitive}\0${ref.named.module_path}\0${ref.named.decl_id}\0${ref.node}`;
    const cached = this.staticTypes.get(key);
    if (cached) return cached;
    const value = { module: this.module, ref, registry: this };
    this.staticTypes.set(key, value);
    if (ref.kind === Kind.Primitive && ref.primitive >= 0)
      this.primitiveTypes[ref.primitive] = value;
    return value;
  }

  facts(type: RuntimeType): TypeFacts {
    const cached = this.typeFacts.get(type);
    if (cached) return cached;
    const facts = this.computeFacts(type, new Set<string>());
    this.typeFacts.set(type, facts);
    return facts;
  }

  private computeFacts(type: RuntimeType, active: Set<string>): TypeFacts {
    const cached = this.typeFacts.get(type);
    if (cached) return cached;
    const underlying = this.underlying(type);
    const node = underlying.registry.node(underlying);
    const kind = underlying.dynamic?.kind ?? node?.kind ?? underlying.ref.kind;
    const primitiveKind = underlying.ref.primitive;
    if (kind === Kind.Primitive || kind === Kind.Void || kind === Kind.Any) {
      const facts = {
        kind,
        primitive: primitiveKind,
        containsReferences: kind === Kind.Any,
        flat: kind === Kind.Primitive,
        fixedSize:
          kind === Kind.Primitive &&
          primitiveKind !== PrimitiveKind.PrimitiveString &&
          primitiveKind !== PrimitiveKind.PrimitiveFunction,
        depth: 0,
        fieldCount: 0,
      } satisfies TypeFacts;
      this.typeFacts.set(type, facts);
      return facts;
    }
    const identity = `${underlying.module}\0${underlying.ref.node || this.key(underlying)}`;
    if (active.has(identity))
      return {
        kind,
        primitive: primitiveKind,
        containsReferences: true,
        flat: false,
        fixedSize: false,
        depth: 129,
        fieldCount: 0,
      };
    active.add(identity);
    const element =
      underlying.dynamic?.elem ?? (node?.elem ? underlying.registry.runtime(node.elem) : undefined);
    const fields =
      underlying.dynamic?.fields ??
      (node?.fields ?? []).map((field) => ({
        name: field.name,
        type: underlying.registry.runtime(field.type),
        tag: field.tag,
        embedded: field.embedded,
      }));
    let facts: TypeFacts;
    if (kind === Kind.Struct) {
      let containsReferences = false;
      let fixedSize = true;
      let depth = 0;
      for (const field of fields) {
        const nested = this.computeFacts(field.type, active);
        containsReferences ||= nested.containsReferences;
        fixedSize &&= nested.fixedSize;
        depth = Math.max(depth, nested.depth + 1);
      }
      facts = {
        kind,
        primitive: primitiveKind,
        containsReferences,
        flat: !containsReferences,
        fixedSize,
        depth,
        fieldCount: fields.length,
      };
    } else if (kind === Kind.Array && element) {
      const nested = this.computeFacts(element, active);
      facts = {
        kind,
        primitive: primitiveKind,
        containsReferences: nested.containsReferences,
        flat: false,
        fixedSize: nested.fixedSize,
        depth: nested.depth + 1,
        fieldCount: 0,
      };
    } else {
      facts = {
        kind,
        primitive: primitiveKind,
        containsReferences: true,
        flat: false,
        fixedSize: false,
        depth: 1,
        fieldCount: 0,
      };
    }
    active.delete(identity);
    this.typeFacts.set(type, facts);
    return facts;
  }

  node(type: RuntimeType): TypeNode | undefined {
    if (type.ref.node) return type.registry.nodes.get(type.ref.node);
    if (type.ref.kind === Kind.Named)
      return type.registry.named.get(`${type.ref.named.module_path}\0${type.ref.named.decl_id}`);
    return undefined;
  }

  hasNamed(modulePath: string, declaration: string): boolean {
    return this.named.has(`${modulePath}\0${declaration}`);
  }

  underlying(type: RuntimeType): RuntimeType {
    if (type.dynamic || type.ref.kind <= Kind.Primitive) return type;
    const cached = this.underlyingTypes.get(type);
    if (cached) return cached;
    let current = type;
    const seen = new Set<string>();
    for (;;) {
      const node = current.registry.node(current);
      if (!node) break;
      const key = `${current.module}\0${node.id}`;
      if (seen.has(key)) fail("recursive underlying type", `${current.module}.type_table`);
      seen.add(key);
      if (node.alias && node.alias_target) current = current.registry.runtime(node.alias_target);
      else if (node.underlying) current = current.registry.runtime(node.underlying);
      else {
        if (node.kind !== Kind.Named)
          current = { ...current, ref: { ...current.ref, kind: node.kind } };
        break;
      }
    }
    if (current.ref.kind !== Kind.Named) this.underlyingTypes.set(type, current);
    return current;
  }

  element(type: RuntimeType): RuntimeType | undefined {
    const underlying = this.underlying(type);
    if (underlying.dynamic) return underlying.dynamic.elem;
    const node = underlying.registry.node(underlying);
    return node?.elem ? underlying.registry.runtime(node.elem) : undefined;
  }

  identity(type: RuntimeType): TypeIdentity {
    if (type.dynamic) return { Structural: { module: "dynamic", node: this.key(type) } };
    if (type.ref.kind === Kind.Void) return "Void";
    if (type.ref.kind === Kind.Any) return "Any";
    if (type.ref.kind === Kind.Primitive) return { Primitive: type.ref.primitive };
    if (type.ref.kind === Kind.Named) return { Named: type.ref.named };
    const node = this.node(type);
    if (node?.identity.module_path && node.identity.decl_id) return { Named: node.identity };
    if (node?.kind === Kind.Pointer && node.elem)
      return { Pointer: this.identity(type.registry.runtime(node.elem)) };
    if (node?.kind === Kind.Slice && node.elem)
      return { Slice: this.identity(type.registry.runtime(node.elem)) };
    return { Structural: { module: type.module, node: type.ref.node } };
  }

  key(type: RuntimeType): string {
    if (type.ref.kind === Kind.Void || type.ref.kind === Kind.Any) return `kind:${type.ref.kind}`;
    if (type.ref.kind === Kind.Primitive) return `primitive:${type.ref.primitive}`;
    if (type.ref.kind === Kind.Named)
      return `named:${type.ref.named.module_path}\0${type.ref.named.decl_id}`;
    const cached = this.typeKeys.get(type);
    if (cached !== undefined) return cached;
    const key = JSON.stringify(this.canonical(type, new Set()));
    this.typeKeys.set(type, key);
    return key;
  }

  private canonical(type: RuntimeType, seen: Set<string>): unknown {
    if (type.ref.kind === Kind.Named)
      return [Kind.Named, type.ref.named.module_path, type.ref.named.decl_id];
    if (type.ref.kind === Kind.Primitive) return [Kind.Primitive, type.ref.primitive];
    if (type.ref.kind === Kind.Any || type.ref.kind === Kind.Void) return [type.ref.kind];
    const cycle = `${type.module}\0${type.ref.node}`;
    if (type.ref.node && seen.has(cycle)) return ["cycle", cycle];
    if (type.ref.node) seen.add(cycle);
    const underlying = this.underlying(type);
    const node = underlying.registry.node(underlying);
    const shape = underlying.dynamic;
    const kind = shape?.kind ?? node?.kind ?? underlying.ref.kind;
    const nested = (value: RuntimeType | undefined): unknown =>
      value && value.registry.canonical(value, new Set(seen));
    const element =
      shape?.elem ?? (node?.elem ? underlying.registry.runtime(node.elem) : undefined);
    const key = shape?.key ?? (node?.key ? underlying.registry.runtime(node.key) : undefined);
    if (
      kind === Kind.Pointer ||
      kind === Kind.Slice ||
      kind === Kind.Array ||
      kind === Kind.Waitable
    )
      return [
        kind,
        nested(element),
        shape?.length ?? node?.length ?? 0,
        shape?.direction ?? node?.direction ?? 0,
      ];
    if (kind === Kind.Map) return [kind, nested(key), nested(element)];
    if (kind === Kind.Struct) {
      const fields =
        shape?.fields ??
        (node?.fields ?? []).map((field) => ({
          ...field,
          type: underlying.registry.runtime(field.type),
        }));
      return [
        kind,
        fields.map((field) => [field.name, nested(field.type), field.tag, field.embedded]),
      ];
    }
    if (kind === Kind.Function) {
      const params =
        shape?.params ??
        node?.signature?.params.map((item) => underlying.registry.runtime(item.type)) ??
        [];
      const results =
        shape?.results ??
        node?.signature?.results.map((item) => underlying.registry.runtime(item)) ??
        [];
      return [
        kind,
        params.map(nested),
        results.map(nested),
        shape?.variadic ?? node?.signature?.variadic ?? false,
      ];
    }
    if (kind === Kind.Interface) {
      return [
        kind,
        (node?.methods ?? []).map((method) => [
          method.name,
          method.signature.params.length,
          method.signature.results.length,
        ]),
      ];
    }
    return [kind, type.module, type.ref.node];
  }

  identical(left: RuntimeType, right: RuntimeType): boolean {
    if (left === right) return true;
    if (left.ref.kind !== right.ref.kind) return false;
    if (left.ref.kind === Kind.Primitive) return left.ref.primitive === right.ref.primitive;
    if (left.ref.kind === Kind.Named)
      return (
        left.ref.named.module_path === right.ref.named.module_path &&
        left.ref.named.decl_id === right.ref.named.decl_id
      );
    return this.key(left) === right.registry.key(right);
  }
}

export interface DetachedTypeClosure {
  readonly root: RuntimeType;
  readonly entries: readonly (readonly [RuntimeType, RuntimeType])[];
  readonly remap: (type: RuntimeType) => RuntimeType;
}

/**
 * Copy only the type nodes reachable from a storage/data root. The detached
 * registries deliberately contain no reference to the source Program table;
 * recursive and cross-module refs are remapped through the copied registries.
 */
export function detachTypeClosure(root: RuntimeType): DetachedTypeClosure {
  const selected = new Map<TypeRegistry, Set<string>>();
  // Every closure, including primitive/Any/Void roots, receives a detached
  // registry so a payload never keeps the source program table alive merely
  // through a scalar descriptor.
  selected.set(root.registry, new Set());
  const registryIDs = new WeakMap<TypeRegistry, number>();
  let nextRegistryID = 1;
  const registryID = (registry: TypeRegistry): number => {
    const existing = registryIDs.get(registry);
    if (existing !== undefined) return existing;
    const id = nextRegistryID++;
    registryIDs.set(registry, id);
    return id;
  };
  const visited = new Set<string>();
  const visit = (type: RuntimeType): void => {
    if (type.dynamic) {
      if (!selected.has(type.registry)) selected.set(type.registry, new Set());
      const dynamicKey = `dynamic:${registryID(type.registry)}\0${type.registry.key(type)}`;
      if (visited.has(dynamicKey)) return;
      visited.add(dynamicKey);
      if (type.dynamic.elem) visit(type.dynamic.elem);
      if (type.dynamic.key) visit(type.dynamic.key);
      for (const parameter of type.dynamic.params ?? []) visit(parameter);
      for (const result of type.dynamic.results ?? []) visit(result);
      for (const field of type.dynamic.fields ?? []) visit(field.type);
      return;
    }
    if (
      type.ref.kind === Kind.Primitive ||
      type.ref.kind === Kind.Any ||
      type.ref.kind === Kind.Void
    )
      return;
    const node = type.registry.node(type);
    if (!node) return;
    const key = `${registryID(type.registry)}\0${node.id}`;
    if (visited.has(key)) return;
    visited.add(key);
    let nodes = selected.get(type.registry);
    if (!nodes) selected.set(type.registry, (nodes = new Set()));
    nodes.add(node.id);
    const refs: TypeRef[] = [
      ...node.tuple,
      ...node.fields.map((field) => field.type),
      ...(node.signature?.params.map((parameter) => parameter.type) ?? []),
      ...(node.signature?.results ?? []),
      ...node.methods.flatMap((method) => [
        method.receiver,
        ...method.signature.params.map((parameter) => parameter.type),
        ...method.signature.results,
      ]),
    ];
    if (node.alias_target) refs.push(node.alias_target);
    if (node.underlying) refs.push(node.underlying);
    if (node.elem) refs.push(node.elem);
    if (node.key) refs.push(node.key);
    for (const ref of refs) visit(type.registry.runtime(ref));
  };
  visit(root);

  const registries = new Map<TypeRegistry, TypeRegistry>();
  for (const [source, ids] of selected) {
    const nodes = [...ids]
      .map((id) => source.nodeByID(id))
      .filter((node): node is TypeNode => node !== undefined);
    registries.set(source, TypeRegistry.detached(source.module, nodes));
  }
  const cache = new Map<string, RuntimeType>();
  const originals = new Map<string, RuntimeType>();
  const sourceIDs = new Map<TypeRegistry, number>();
  let nextSourceID = 1;
  const remap = (type: RuntimeType): RuntimeType => {
    let sourceID = sourceIDs.get(type.registry);
    if (sourceID === undefined) {
      sourceID = nextSourceID++;
      sourceIDs.set(type.registry, sourceID);
    }
    const key = `${sourceID}:${type.registry.key(type)}`;
    originals.set(key, type);
    const cached = cache.get(key);
    if (cached) return cached;
    const registry = registries.get(type.registry) ?? type.registry;
    if (!type.dynamic) {
      const result = registry.runtime({ ...type.ref, named: { ...type.ref.named } });
      cache.set(key, result);
      return result;
    }
    const result: RuntimeType = {
      module: type.module,
      ref: { ...type.ref, named: { ...type.ref.named } },
      registry,
    };
    cache.set(key, result);
    result.dynamic = {
      ...type.dynamic,
      ...(type.dynamic.elem ? { elem: remap(type.dynamic.elem) } : {}),
      ...(type.dynamic.key ? { key: remap(type.dynamic.key) } : {}),
      ...(type.dynamic.params ? { params: type.dynamic.params.map(remap) } : {}),
      ...(type.dynamic.results ? { results: type.dynamic.results.map(remap) } : {}),
      ...(type.dynamic.fields
        ? { fields: type.dynamic.fields.map((field) => ({ ...field, type: remap(field.type) })) }
        : {}),
    };
    return result;
  };
  const detachedRoot = remap(root);
  return {
    root: detachedRoot,
    entries: [...cache].map(([key, type]) => [originals.get(key)!, type] as const),
    remap,
  };
}

export type ValueKind =
  | "uninitialized"
  | "nil"
  | "bool"
  | "int"
  | "uint"
  | "float"
  | "complex"
  | "string"
  | "struct"
  | "array"
  | "bytes"
  | "slice"
  | "map"
  | "interface"
  | "pointer"
  | "function"
  | "dynamicFunction"
  | "method"
  | "channel"
  | "mutex"
  | "opaque";

export interface Value {
  type: RuntimeType;
  kind: ValueKind;
  value?: unknown;
}

export interface Cell {
  readonly id: number;
  readonly read: () => Value;
  readonly write: (value: Value) => void;
  /** Typed inline paths can access only the addressed field or element. */
  readPath?: (path: readonly AddressPath[]) => Value | undefined;
  writePath?: (path: readonly AddressPath[], value: Value) => boolean;
  readLocation?: (location: AddressLocation) => Value | undefined;
  writeLocation?: (location: AddressLocation, value: Value) => boolean;
  /** Stable managed identity for payloads owned by a VM heap. */
  managedHandle?: GuestHandle;
  managedLocation?: AddressLocation;
}

export type AddressPath =
  | { kind: "field"; name: string }
  | { kind: "index"; index: number }
  | { kind: "arrayView"; start: number; length: number; type: RuntimeType }
  | { kind: "typeView"; type: RuntimeType };

export interface Address {
  readonly root: Cell;
  readonly path: AddressPath[];
  readonly location?: AddressLocation;
}

export interface AddressLocation {
  readonly handle: GuestHandle;
  readonly offset: number;
  readonly type: RuntimeType;
  readonly layout: StorageLayout;
}

export interface SliceValue {
  storage: Address;
  start: number;
  length: number;
  capacity: number;
}

export interface FunctionValue {
  module: string;
  function: string;
  captures: Address[];
  revision: number;
  receiver?: Value;
}

let nextCell = 1;

export function cell(value: Value): Cell {
  return cellAccess(
    () => value,
    (next) => {
      value = next;
    },
  );
}

export function cellAccess(read: () => Value, write: (value: Value) => void): Cell {
  return {
    id: nextCell++,
    read,
    write,
  };
}

export function primitive(registry: TypeRegistry, kind: number): RuntimeType {
  return registry.runtime({
    kind: Kind.Primitive,
    primitive: kind,
    named: { module_path: "", decl_id: "" },
    node: "",
  });
}

export function boolValue(registry: TypeRegistry, value: boolean): Value {
  return { type: primitive(registry, PrimitiveKind.PrimitiveBool), kind: "bool", value };
}

export function intValue(registry: TypeRegistry, value: bigint): Value {
  return {
    type: primitive(registry, PrimitiveKind.PrimitiveInt),
    kind: "int",
    value: BigInt.asIntN(64, value),
  };
}

export function stringValue(registry: TypeRegistry, value: Uint8Array): Value {
  return {
    type: primitive(registry, PrimitiveKind.PrimitiveString),
    kind: "string",
    value: new Uint8Array(value),
  };
}

export function nilValue(type: RuntimeType): Value {
  return { type, kind: "nil" };
}

export function zero(type: RuntimeType): Value {
  const underlying = type.registry.underlying(type);
  const node = underlying.registry.node(underlying);
  const dynamic = underlying.dynamic;
  let value: Value;
  switch (underlying.ref.kind) {
    case Kind.Void:
    case Kind.Any:
    case Kind.Interface:
    case Kind.Function:
    case Kind.Map:
    case Kind.Pointer:
    case Kind.Slice:
    case Kind.Waitable:
      value = nilValue(type);
      break;
    case Kind.Primitive:
      if (underlying.ref.primitive === PrimitiveKind.PrimitiveBool)
        value = { type, kind: "bool", value: false };
      else if (underlying.ref.primitive === PrimitiveKind.PrimitiveString)
        value = { type, kind: "string", value: new Uint8Array() };
      else if (
        underlying.ref.primitive >= PrimitiveKind.PrimitiveInt &&
        underlying.ref.primitive <= PrimitiveKind.PrimitiveInt64
      )
        value = { type, kind: "int", value: 0n };
      else if (
        underlying.ref.primitive >= PrimitiveKind.PrimitiveUint &&
        underlying.ref.primitive <= PrimitiveKind.PrimitiveUintptr
      )
        value = { type, kind: "uint", value: 0n };
      else if (
        underlying.ref.primitive === PrimitiveKind.PrimitiveFloat32 ||
        underlying.ref.primitive === PrimitiveKind.PrimitiveFloat64
      )
        value = { type, kind: "float", value: 0 };
      else if (
        underlying.ref.primitive === PrimitiveKind.PrimitiveComplex64 ||
        underlying.ref.primitive === PrimitiveKind.PrimitiveComplex128
      )
        value = { type, kind: "complex", value: { real: 0, imag: 0 } };
      else value = nilValue(type);
      break;
    case Kind.Array: {
      const element =
        dynamic?.elem ?? (node?.elem ? underlying.registry.runtime(node.elem) : undefined);
      if (!element) fail("array type is missing element", `${type.module}.type_table`);
      const length = dynamic?.length ?? node!.length;
      value = { type, kind: "array", value: Array.from({ length }, () => zero(element)) };
      break;
    }
    case Kind.Struct: {
      const fields = new Map<string, Value>();
      if (dynamic?.fields) {
        for (const field of dynamic.fields) fields.set(field.name, zero(field.type));
      } else {
        for (const field of node?.fields ?? [])
          fields.set(field.name, zero(underlying.registry.runtime(field.type)));
      }
      value = { type, kind: "struct", value: fields };
      break;
    }
    case Kind.Tuple: {
      value = {
        type,
        kind: "array",
        value: (node?.tuple ?? []).map((item) => zero(underlying.registry.runtime(item))),
      };
      break;
    }
    default:
      value = nilValue(type);
  }
  return value;
}

export function cloneValue(value: Value): Value {
  const packed = packedAggregate(value);
  if (packed) return packed.owner.copyAggregate(value);
  switch (value.kind) {
    case "array":
      return { ...value, value: (value.value as Value[]).map(cloneValue) };
    case "struct": {
      const fields = new Map<string, Value>();
      for (const [name, item] of value.value as Map<string, Value>)
        fields.set(name, cloneValue(item));
      return { ...value, value: fields };
    }
    case "interface":
      return { ...value, value: cloneValue(value.value as Value) };
    case "string":
      // Guest strings are immutable. Host boundaries and mutable byte slices
      // copy their backing storage where ownership can change.
      return { ...value };
    case "complex":
      return { ...value, value: { ...(value.value as { real: number; imag: number }) } };
    default:
      return { ...value };
  }
}

/** Decode one Go rune; each invalid UTF-8 byte consumes exactly one byte. */
export function decodeRune(bytes: Uint8Array, offset: number): [number, number] {
  if (offset < 0 || offset >= bytes.length) fail("string index out of range", "string", "panic");
  const first = bytes[offset]!;
  if (first < 0x80) return [first, 1];
  if (first < 0xc2 || first > 0xf4) return [0xfffd, 1];
  const width = first >= 0xf0 ? 4 : first >= 0xe0 ? 3 : 2;
  if (offset + width > bytes.length) return [0xfffd, 1];
  let rune = first & (0x7f >> width);
  for (let index = 1; index < width; index++) {
    const next = bytes[offset + index]!;
    if ((next & 0xc0) !== 0x80) return [0xfffd, 1];
    rune = (rune << 6) | (next & 0x3f);
  }
  if (
    (width === 2 && rune < 0x80) ||
    (width === 3 && rune < 0x800) ||
    (width === 4 && rune < 0x10000) ||
    rune > 0x10ffff ||
    (rune >= 0xd800 && rune <= 0xdfff)
  )
    return [0xfffd, 1];
  return [rune, width];
}

export function parseTypeRef(value: JsonValue, path: string): TypeRef {
  const raw = schemaObject(value, path, WIRE_FIELDS.TypeRef);
  return {
    kind: integer(raw.kind, `${path}.kind`),
    primitive: integer(raw.primitive, `${path}.primitive`, true),
    named: parseTypeKey(raw.named, `${path}.named`),
    node: string(raw.node, `${path}.node`, true),
  };
}

function optionalTypeRef(value: JsonValue | undefined, path: string): TypeRef | undefined {
  if (value === undefined) return undefined;
  const parsed = parseTypeRef(value, path);
  return parsed.kind === Kind.Invalid ? undefined : parsed;
}

function parseTypeKey(value: JsonValue | undefined, path: string): TypeKey {
  if (value === undefined) return { module_path: "", decl_id: "" };
  const raw = schemaObject(value, path, WIRE_FIELDS.TypeKey);
  return {
    module_path: string(raw.module_path, `${path}.module_path`, true),
    decl_id: string(raw.decl_id, `${path}.decl_id`, true),
  };
}

function parseTypeRefs(value: JsonValue | undefined, path: string): TypeRef[] {
  if (value === undefined) return [];
  if (!Array.isArray(value)) fail("expected type array", path);
  return value.map((item, index) => parseTypeRef(item, `${path}[${index}]`));
}

export function parseSignature(value: JsonValue, path: string): FunctionSignature {
  const raw = schemaObject(value, path, WIRE_FIELDS.FunctionSignature);
  const params = raw.params;
  return {
    params:
      params === undefined
        ? []
        : (() => {
            if (!Array.isArray(params)) fail("expected parameter array", `${path}.params`);
            return params.map((item, index) => {
              const parameter = schemaObject(
                item,
                `${path}.params[${index}]`,
                WIRE_FIELDS.TypeParam,
              );
              if (!parameter.type) fail("missing parameter type", `${path}.params[${index}].type`);
              return { type: parseTypeRef(parameter.type, `${path}.params[${index}].type`) };
            });
          })(),
    results: parseTypeRefs(raw.results, `${path}.results`),
    variadic: boolean(raw.variadic, `${path}.variadic`),
  };
}

function parseFields(value: JsonValue | undefined, path: string): TypeNode["fields"] {
  if (value === undefined) return [];
  if (!Array.isArray(value)) fail("expected field array", path);
  return value.map((item, index) => {
    const fieldPath = `${path}[${index}]`;
    const field = schemaObject(item, fieldPath, WIRE_FIELDS.Field);
    if (!field.type) fail("missing field type", `${fieldPath}.type`);
    return {
      name: string(field.name, `${fieldPath}.name`),
      type: parseTypeRef(field.type, `${fieldPath}.type`),
      tag: string(field.tag, `${fieldPath}.tag`, true),
      embedded: boolean(field.embedded, `${fieldPath}.embedded`),
    };
  });
}

function parseMethods(value: JsonValue | undefined, path: string): TypeNode["methods"] {
  if (value === undefined) return [];
  if (!Array.isArray(value)) fail("expected method array", path);
  return value.map((item, index) => {
    const methodPath = `${path}[${index}]`;
    const method = schemaObject(item, methodPath, WIRE_FIELDS.Method);
    if (!method.receiver || !method.signature) fail("invalid method", methodPath);
    return {
      name: string(method.name, `${methodPath}.name`),
      receiver: parseTypeRef(method.receiver, `${methodPath}.receiver`),
      signature: parseSignature(method.signature, `${methodPath}.signature`),
      function_id: string(method.function_id, `${methodPath}.function_id`, true),
      module_path: string(method.module_path, `${methodPath}.module_path`, true),
    };
  });
}

export function decodeConstant(type: RuntimeType, source: JsonValue): Value {
  const underlying = type.registry.underlying(type);
  const primitiveKind = underlying.ref.primitive;
  if (source === null) return zero(type);
  if (underlying.ref.kind === Kind.Primitive) {
    if (primitiveKind === PrimitiveKind.PrimitiveBool) {
      if (typeof source !== "boolean") fail("invalid boolean constant", "constant");
      return { type, kind: "bool", value: source };
    }
    if (primitiveKind === PrimitiveKind.PrimitiveString) {
      if (typeof source === "string")
        return { type, kind: "string", value: textEncoder.encode(source) };
      const raw = schemaObject(source, "constant", ["bytes"]);
      const encoded = string(raw.bytes, "constant.bytes");
      return { type, kind: "string", value: decodeBase64(encoded) };
    }
    if (
      primitiveKind >= PrimitiveKind.PrimitiveInt &&
      primitiveKind <= PrimitiveKind.PrimitiveInt64
    ) {
      const exact = exactInteger(source);
      return normalize({ type, kind: "int", value: BigInt.asIntN(64, exact) });
    }
    if (
      primitiveKind >= PrimitiveKind.PrimitiveUint &&
      primitiveKind <= PrimitiveKind.PrimitiveUintptr
    ) {
      const exact = exactInteger(source);
      return normalize({ type, kind: "uint", value: BigInt.asUintN(64, exact) });
    }
    if (
      primitiveKind === PrimitiveKind.PrimitiveFloat32 ||
      primitiveKind === PrimitiveKind.PrimitiveFloat64
    ) {
      return normalize({ type, kind: "float", value: exactFloat(source) });
    }
    if (
      primitiveKind === PrimitiveKind.PrimitiveComplex64 ||
      primitiveKind === PrimitiveKind.PrimitiveComplex128
    ) {
      const raw = schemaObject(source, "constant", ["real", "imag"]);
      return normalize({
        type,
        kind: "complex",
        value: { real: exactFloat(raw.real!), imag: exactFloat(raw.imag!) },
      });
    }
  }
  if (underlying.ref.kind === Kind.Slice && typeof source === "string") {
    const bytes = decodeBase64(source);
    const storage = cell({ type, kind: "bytes", value: bytes });
    return {
      type,
      kind: "slice",
      value: {
        storage: { root: storage, path: [] },
        start: 0,
        length: bytes.length,
        capacity: bytes.length,
      },
    };
  }
  return zero(type);
}

function exactInteger(value: JsonValue): bigint {
  if (value instanceof JsonNumber) return value.integer("constant");
  if (typeof value === "string" && /^-?(?:0|[1-9][0-9]*)$/.test(value)) return BigInt(value);
  fail("invalid integer constant", "constant");
}

function exactFloat(value: JsonValue): number {
  if (value instanceof JsonNumber) return value.number("constant");
  if (typeof value === "string") {
    const parts = value.split("/");
    if (parts.length === 1) {
      const number = Number(value);
      if (Number.isFinite(number)) return number;
    } else if (parts.length === 2 && /^-?[0-9]+$/.test(parts[0]!) && /^[0-9]+$/.test(parts[1]!)) {
      return Number(BigInt(parts[0]!)) / Number(BigInt(parts[1]!));
    }
  }
  fail("invalid floating-point constant", "constant");
}

function decodeBase64(value: string): Uint8Array {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index++) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

export function normalize(value: Value): Value {
  const underlying = value.type.registry.underlying(value.type);
  if (underlying.ref.kind !== Kind.Primitive) return value;
  const primitiveKind = underlying.ref.primitive;
  if (value.kind === "int") {
    const width = INTEGER_WIDTHS[primitiveKind];
    if (width) value.value = BigInt.asIntN(width, value.value as bigint);
  } else if (value.kind === "uint") {
    const width = UNSIGNED_INTEGER_WIDTHS[primitiveKind];
    if (width) value.value = BigInt.asUintN(width, value.value as bigint);
  } else if (value.kind === "float" && primitiveKind === PrimitiveKind.PrimitiveFloat32) {
    value.value = Math.fround(value.value as number);
  } else if (value.kind === "complex" && primitiveKind === PrimitiveKind.PrimitiveComplex64) {
    const complex = value.value as { real: number; imag: number };
    complex.real = Math.fround(complex.real);
    complex.imag = Math.fround(complex.imag);
  }
  return value;
}

const INTEGER_WIDTHS: Readonly<Record<number, number>> = Object.freeze({
  [PrimitiveKind.PrimitiveInt8]: 8,
  [PrimitiveKind.PrimitiveInt16]: 16,
  [PrimitiveKind.PrimitiveInt32]: 32,
  [PrimitiveKind.PrimitiveInt64]: 64,
  [PrimitiveKind.PrimitiveInt]: 64,
});

const UNSIGNED_INTEGER_WIDTHS: Readonly<Record<number, number>> = Object.freeze({
  [PrimitiveKind.PrimitiveUint8]: 8,
  [PrimitiveKind.PrimitiveUint16]: 16,
  [PrimitiveKind.PrimitiveUint32]: 32,
  [PrimitiveKind.PrimitiveUint64]: 64,
  [PrimitiveKind.PrimitiveUintptr]: 64,
  [PrimitiveKind.PrimitiveUint]: 64,
});

export function advanceAddress(address: Address, segment: AddressPath): Address {
  const location = storageAddress(address);
  if (!location) return { root: address.root, path: [...address.path, segment] };
  let next: AddressLocation = location;
  if (segment.kind === "typeView") {
    next = { ...next, type: segment.type };
  } else if (segment.kind === "field") {
    const field = fieldLocation(next, next.layout, segment.name);
    next = { ...field, layout: prepareStorageLayout(field.type) };
  } else if (segment.kind === "index") {
    if (!next.layout.element || !next.layout.elementType)
      fail(
        `value is not indexable (${next.type.registry.key(next.type)}; ${next.layout.kind})`,
        "index",
        "type_error",
      );
    if (segment.index < 0 || segment.index >= (next.layout.length ?? 0))
      fail("index out of range", "index", "panic");
    next = {
      handle: next.handle,
      offset: next.offset + segment.index * next.layout.element.size,
      type: next.layout.elementType,
      layout: next.layout.element,
    };
  } else {
    if (!next.layout.element || !next.layout.elementType)
      fail("array view targets non-array", "address", "type_error");
    if (
      segment.start < 0 ||
      segment.length < 0 ||
      segment.start + segment.length > (next.layout.length ?? 0)
    )
      fail("index out of range", "index", "panic");
    next = {
      handle: next.handle,
      offset: next.offset + segment.start * next.layout.element.size,
      type: segment.type,
      layout: prepareArrayStorageLayout(next.layout.elementType, segment.length),
    };
  }
  return { root: address.root, path: [...address.path, segment], location: next };
}

export function storageAddress(address: Address): AddressLocation | undefined {
  if (address.location) return address.location;
  const base = address.root.managedLocation;
  if (!base) return undefined;
  return advanceAddressPath({ root: address.root, path: [], location: base }, address.path)
    .location;
}

function advanceAddressPath(address: Address, path: readonly AddressPath[]): Address {
  let result = address;
  for (const segment of path) result = advanceAddress(result, segment);
  return result;
}

export function dereference(address: Address): Value {
  const location = storageAddress(address);
  if (location) {
    const direct = address.root.readLocation?.(location);
    if (direct) return direct;
  }
  const direct = address.root.readPath?.(address.path);
  if (direct) return direct;
  let value = address.root.read();
  for (const segment of address.path) {
    if (segment.kind === "typeView") continue;
    if (segment.kind === "arrayView") {
      const values = value.value as Value[];
      value = {
        type: segment.type,
        kind: "array",
        value: values.slice(segment.start, segment.start + segment.length),
      };
    } else if (segment.kind === "field") {
      if (value.kind === "interface") value = value.value as Value;
      if (value.kind === "pointer") value = dereference(value.value as Address);
      if (
        value.kind === "nil" &&
        value.type.registry.underlying(value.type).ref.kind === Kind.Pointer
      )
        fail("nil pointer dereference", "address", "panic");
      if (value.kind !== "struct")
        fail("field address targets non-struct", "address", "type_error");
      const field = fieldValue(value, segment.name);
      if (!field) fail("field address is invalid", "address", "type_error");
      value = field;
    } else {
      value = indexValue(value, segment.index);
    }
  }
  return value;
}

export function assign(address: Address, assigned: Value): void {
  const location = storageAddress(address);
  if (location && address.root.writeLocation?.(location, assigned)) return;
  if (address.root.writePath?.(address.path, assigned)) return;
  if (address.path.length === 0) {
    address.root.write(cloneValue(assigned));
    return;
  }
  const result = updateAtAddress(address.root.read(), address.path, assigned);
  if (result.writeRoot) address.root.write(result.value);
}

interface AddressUpdate {
  value: Value;
  /** False means the update was handed to a nested address owner. */
  writeRoot: boolean;
}

/**
 * Update a materialized value while preserving the owner of aliased storage.
 * A pointer or slice is an address into another cell, so descending through
 * one must call assign on that cell instead of modifying the temporary value
 * returned by read().
 */
function updateAtAddress(
  value: Value,
  path: readonly AddressPath[],
  assigned: Value,
): AddressUpdate {
  if (path.length === 0) return { value: cloneValue(assigned), writeRoot: true };
  const segment: AddressPath = path[0]!;
  const rest = path.slice(1);
  if (segment.kind === "typeView") return updateAtAddress(value, rest, assigned);

  if (value.kind === "interface") {
    const inner = updateAtAddress(value.value as Value, path, assigned);
    if (!inner.writeRoot) return { value, writeRoot: false };
    return { value: { ...value, value: inner.value }, writeRoot: true };
  }

  if (value.kind === "pointer") {
    const target = value.value as Address;
    assign(advanceAddressPath(target, path), assigned);
    return { value, writeRoot: false };
  }

  if (value.kind === "slice") {
    const slice = value.value as SliceValue;
    if (segment.kind === "index") {
      if (segment.index < 0 || segment.index >= slice.length)
        fail("index out of range", "index", "panic");
      assign(
        advanceAddressPath(slice.storage, [
          { kind: "index", index: slice.start + segment.index },
          ...rest,
        ]),
        assigned,
      );
      return { value, writeRoot: false };
    }
    if (segment.kind === "arrayView") {
      const start = slice.start + segment.start;
      if (start < slice.start || segment.start + segment.length > slice.length)
        fail("index out of range", "index", "panic");
      assign(advanceAddressPath(slice.storage, [{ ...segment, start }, ...rest]), assigned);
      return { value, writeRoot: false };
    }
    fail("slice path is invalid", "address", "type_error");
  }

  if (segment.kind === "arrayView") {
    if (value.kind !== "array") fail("array view targets non-array", "address", "type_error");
    const values = value.value as Value[];
    if (segment.start < 0 || segment.start + segment.length > values.length)
      fail("index out of range", "index", "panic");
    if (rest.length > 0) {
      const next = rest[0]!;
      if (next.kind !== "index") fail("array view path is invalid", "address", "type_error");
      if (next.index < 0 || next.index >= segment.length)
        fail("index out of range", "index", "panic");
      const updated = updateAtAddress(values[segment.start + next.index]!, rest.slice(1), assigned);
      if (updated.writeRoot) values[segment.start + next.index] = updated.value;
      return { value, writeRoot: updated.writeRoot };
    }
    if (assigned.kind !== "array")
      fail("array view assignment requires an array", "address", "type_error");
    const replacements = assigned.value as Value[];
    if (replacements.length !== segment.length)
      fail("array view assignment length mismatch", "address", "type_error");
    for (let index = 0; index < replacements.length; index++)
      values[segment.start + index] = cloneValue(replacements[index]!);
    return { value, writeRoot: true };
  }

  if (segment.kind === "field") {
    if (value.kind !== "struct") fail("field address targets non-struct", "address", "type_error");
    if (writePackedField(value, segment.name, assigned)) return { value, writeRoot: true };
    const fields = value.value as Map<string, Value>;
    const current = fields.get(segment.name);
    if (!current) fail("field address is invalid", "address", "type_error");
    const updated = updateAtAddress(current, rest, assigned);
    if (updated.writeRoot) fields.set(segment.name, updated.value);
    return { value, writeRoot: updated.writeRoot };
  }

  if (value.kind === "bytes") {
    const bytes = value.value as Uint8Array;
    if (segment.index < 0 || segment.index >= bytes.length)
      fail("index out of range", "index", "panic");
    if (rest.length > 0) fail("byte index path is invalid", "address", "type_error");
    bytes[segment.index] = Number(assigned.value as bigint);
    return { value, writeRoot: true };
  }

  if (value.kind !== "array") fail("value is not indexable", "index", "type_error");
  const values = value.value as Value[];
  if (segment.index < 0 || segment.index >= values.length)
    fail("index out of range", "index", "panic");
  const updated = updateAtAddress(values[segment.index]!, rest, assigned);
  if (updated.writeRoot) values[segment.index] = updated.value;
  return { value, writeRoot: updated.writeRoot };
}

export function fieldValue(value: Value, name: string): Value {
  if (value.kind === "interface") value = value.value as Value;
  if (value.kind === "pointer") value = dereference(value.value as Address);
  if (value.kind === "nil" && value.type.registry.underlying(value.type).ref.kind === Kind.Pointer)
    fail("nil pointer dereference", "field", "panic");
  if (value.kind !== "struct") fail(`field access targets ${value.kind}`, "field", "type_error");
  const packed = packedField(value, name);
  if (packed) return packed;
  const packedRoot = packedAggregate(value);
  if (packedRoot) {
    const decoded = packedRoot.owner.decode(
      packedRoot.type,
      packedRoot.layout,
      packedRoot.storage.read(packedRoot.offset, packedRoot.layout.size),
    );
    if (decoded.kind === "struct" && decoded.value instanceof Map) return fieldValue(decoded, name);
    fail(
      `unknown field ${name} (${packedRoot.layout.fields.map((field) => field.name).join(",")})`,
      name,
      "type_error",
    );
  }
  const field = (value.value as Map<string, Value>).get(name);
  if (!field) {
    const underlying = value.type.registry.underlying(value.type);
    const node = underlying.registry.node(underlying);
    const embedded =
      underlying.dynamic?.fields?.filter((item) => item.embedded) ??
      (node?.fields ?? [])
        .filter((item) => item.embedded)
        .map((item) => ({
          name: item.name,
          type: underlying.registry.runtime(item.type),
        }));
    for (const item of embedded) {
      const nested = (value.value as Map<string, Value>).get(item.name);
      if (nested) {
        try {
          return fieldValue(nested, name);
        } catch (error) {
          if (!(error instanceof VMError) || error.code !== "type_error") throw error;
        }
      }
    }
    fail(
      `unknown field ${name} (${[...(value.value as Map<string, Value>).keys()].join(",")}; ${JSON.stringify(value.type.registry.identity(value.type))})`,
      name,
      "type_error",
    );
  }
  return field;
}

export function indexValue(value: Value, index: number): Value {
  if (!Number.isSafeInteger(index) || index < 0) fail("index out of range", "index", "panic");
  if (value.kind === "pointer") {
    const address = value.value as Address;
    return dereference(advanceAddress(address, { kind: "index", index }));
  }
  if (value.kind === "slice") {
    const slice = value.value as SliceValue;
    if (index >= slice.length) fail("index out of range", "index", "panic");
    return dereference({
      ...advanceAddress(slice.storage, { kind: "index", index: slice.start + index }),
    });
  }
  if (value.kind === "array") {
    const packed = packedIndex(value, index);
    if (packed) return packed;
    const item = (value.value as Value[])[index];
    if (!item) fail("index out of range", "index", "panic");
    return item;
  }
  if (value.kind === "bytes") {
    const bytes = value.value as Uint8Array;
    if (index >= bytes.length) fail("index out of range", "index", "panic");
    return {
      type: primitive(value.type.registry, PrimitiveKind.PrimitiveUint8),
      kind: "uint",
      value: BigInt(bytes[index]!),
    };
  }
  if (value.kind === "string") {
    const bytes = value.value as Uint8Array;
    if (index >= bytes.length) fail("index out of range", "index", "panic");
    return {
      type: primitive(value.type.registry, PrimitiveKind.PrimitiveUint8),
      kind: "uint",
      value: BigInt(bytes[index]!),
    };
  }
  fail("value is not indexable", "index", "type_error");
}

export function setIndex(value: Value, index: number, assigned: Value): void {
  if (!Number.isSafeInteger(index) || index < 0) fail("index out of range", "index", "panic");
  if (value.kind === "pointer") {
    const address = value.value as Address;
    assign(advanceAddress(address, { kind: "index", index }), assigned);
    return;
  }
  if (value.kind === "slice") {
    const slice = value.value as SliceValue;
    if (index >= slice.length) fail("index out of range", "index", "panic");
    assign(advanceAddress(slice.storage, { kind: "index", index: slice.start + index }), assigned);
    return;
  }
  if (value.kind === "array") {
    if (writePackedIndex(value, index, assigned)) return;
    const values = value.value as Value[];
    if (index >= values.length) fail("index out of range", "index", "panic");
    values[index] = cloneValue(assigned);
    return;
  }
  if (value.kind === "bytes") {
    const bytes = value.value as Uint8Array;
    if (index >= bytes.length) fail("index out of range", "index", "panic");
    bytes[index] = Number(assigned.value as bigint);
    return;
  }
  fail("value is not assignable by index", "index", "type_error");
}

export function guestEqual(left: Value, right: Value): boolean {
  if (left.kind === "interface" || right.kind === "interface") {
    if (left.kind !== "interface" || right.kind !== "interface") {
      const boxed = left.kind === "interface" ? left : right;
      const other = left.kind === "interface" ? right : left;
      if (other.kind === "nil") {
        const kind = other.type.registry.underlying(other.type).ref.kind;
        if (kind === Kind.Any || kind === Kind.Interface) return false;
      }
      return guestEqual(boxed.value as Value, other);
    }
    const leftDynamic = left.value as Value;
    const rightDynamic = right.value as Value;
    if (!leftDynamic.type.registry.identical(leftDynamic.type, rightDynamic.type)) return false;
    assertComparable(leftDynamic);
    return guestEqual(leftDynamic, rightDynamic);
  }
  if (left.kind === "nil" || right.kind === "nil")
    return left.kind === right.kind && left.type.registry.identical(left.type, right.type);
  if (left.kind !== right.kind || !left.type.registry.identical(left.type, right.type))
    return false;
  switch (left.kind) {
    case "bool":
    case "int":
    case "uint":
    case "float":
      return left.value === right.value;
    case "string":
    case "bytes":
      return bytesEqual(left.value as Uint8Array, right.value as Uint8Array);
    case "complex": {
      const a = left.value as { real: number; imag: number };
      const b = right.value as { real: number; imag: number };
      return a.real === b.real && a.imag === b.imag;
    }
    case "pointer":
      return addressEqual(left.value as Address, right.value as Address);
    case "channel":
    case "mutex":
      return left.value === right.value;
    case "array": {
      const leftPacked = packedAggregate(left);
      const rightPacked = packedAggregate(right);
      if (leftPacked || rightPacked) {
        const leftLength = leftPacked ? packedLength(left)! : (left.value as Value[]).length;
        const rightLength = rightPacked ? packedLength(right)! : (right.value as Value[]).length;
        if (leftLength !== rightLength) return false;
        for (let index = 0; index < leftLength; index++)
          if (!guestEqual(indexValue(left, index), indexValue(right, index))) return false;
        return true;
      }
      const a = left.value as Value[];
      const b = right.value as Value[];
      return a.length === b.length && a.every((item, index) => guestEqual(item, b[index]!));
    }
    case "struct": {
      const leftPacked = packedAggregate(left);
      const rightPacked = packedAggregate(right);
      if (leftPacked || rightPacked) {
        const leftFields = leftPacked ? packedFields(left)! : (left.value as Map<string, Value>);
        for (const [name, item] of leftFields)
          if (name !== "_" && !guestEqual(item, fieldValue(right, name))) return false;
        return true;
      }
      const a = left.value as Map<string, Value>;
      const b = right.value as Map<string, Value>;
      for (const [name, item] of a)
        if (name !== "_" && !guestEqual(item, b.get(name)!)) return false;
      return true;
    }
    default:
      fail("uncomparable dynamic value", "compare", "panic");
  }
}

const objectHashIds = new WeakMap<object, number>();
let nextObjectHashId = 1;

function objectHashId(value: object): number {
  let id = objectHashIds.get(value);
  if (id === undefined) {
    id = nextObjectHashId++;
    objectHashIds.set(value, id);
  }
  return id;
}

function hashText(value: string): string {
  let hash = 2166136261;
  for (let index = 0; index < value.length; index++) {
    hash ^= value.charCodeAt(index);
    hash = Math.imul(hash, 16777619);
  }
  return (hash >>> 0).toString(16);
}

function hashAddress(address: Address): string {
  const location = storageAddress(address);
  if (location)
    return hashText(`${location.handle.slot}:${location.handle.generation}:${location.offset}`);
  const path = address.path.filter((segment) => segment.kind !== "typeView");
  let text = `root:${address.root.id}/path:${path.length};`;
  for (const segment of path) {
    if (segment.kind === "field") text += `f${segment.name.length}:${segment.name};`;
    else if (segment.kind === "index") text += `i${segment.index};`;
    else if (segment.kind === "arrayView") text += `a${segment.start};`;
  }
  return hashText(text);
}

export function hashValue(value: Value, seen = new Set<object>()): string {
  if (value.kind === "interface") return hashValue(value.value as Value, seen);
  let text = `${value.type.registry.key(value.type)}|${value.kind}|`;
  switch (value.kind) {
    case "nil":
    case "uninitialized":
      break;
    case "bool":
      text += value.value ? "1" : "0";
      break;
    case "int":
    case "uint":
      text += String(value.value);
      break;
    case "float": {
      const number = value.value as number;
      text += number === 0 ? "0" : String(number);
      break;
    }
    case "string":
    case "bytes": {
      const bytes = value.value as Uint8Array;
      text += String(bytes.length) + ":";
      for (const byte of bytes) text += String.fromCharCode(byte);
      break;
    }
    case "complex": {
      const complex = value.value as { real: number; imag: number };
      text += `${complex.real === 0 ? 0 : complex.real},${complex.imag === 0 ? 0 : complex.imag}`;
      break;
    }
    case "pointer":
      return `pointer:${hashAddress(value.value as Address)}`;
    case "channel":
    case "mutex":
      return `${value.kind}:${objectHashId(value.value as object)}`;
    case "array": {
      const packed = packedAggregate(value);
      if (packed) {
        if (seen.has(packed)) return `${text}cycle`;
        seen.add(packed);
        let separator = "";
        for (const item of packedItems(value)!) {
          text += separator + hashValue(item, seen);
          separator = ",";
        }
        seen.delete(packed);
        break;
      }
      const array = value.value as Value[];
      if (seen.has(array)) return `${text}cycle`;
      seen.add(array);
      text += array.map((item) => hashValue(item, seen)).join(",");
      seen.delete(array);
      break;
    }
    case "struct": {
      const packed = packedAggregate(value);
      if (packed) {
        if (seen.has(packed)) return `${text}cycle`;
        seen.add(packed);
        for (const [name, item] of [...packedFields(value)!].sort(([left], [right]) =>
          left.localeCompare(right),
        ))
          if (name !== "_") text += `${name}:${hashValue(item, seen)};`;
        seen.delete(packed);
        break;
      }
      const fields = value.value as Map<string, Value>;
      if (seen.has(fields)) return `${text}cycle`;
      seen.add(fields);
      for (const [name, item] of [...fields].sort(([left], [right]) => left.localeCompare(right)))
        if (name !== "_") text += `${name}:${hashValue(item, seen)};`;
      seen.delete(fields);
      break;
    }
    default:
      return `${value.kind}:${objectHashId(value.value as object)}`;
  }
  return hashText(text);
}

export function assertComparable(value: Value): void {
  if (value.kind === "interface") {
    assertComparable(value.value as Value);
    return;
  }
  const kind = value.type.registry.underlying(value.type).ref.kind;
  if (kind === Kind.Slice || kind === Kind.Map || kind === Kind.Function)
    fail("uncomparable dynamic value", "compare", "panic");
  if (value.kind === "array") {
    if (packedAggregate(value)) {
      for (const item of packedItems(value)!) assertComparable(item);
      return;
    }
    for (const item of value.value as Value[]) assertComparable(item);
  } else if (value.kind === "struct") {
    if (packedAggregate(value)) {
      for (const [name, item] of packedFields(value)!) if (name !== "_") assertComparable(item);
      return;
    }
    for (const [name, item] of value.value as Map<string, Value>)
      if (name !== "_") assertComparable(item);
  }
}

function addressEqual(left: Address, right: Address): boolean {
  const leftLocation = storageAddress(left);
  const rightLocation = storageAddress(right);
  if (leftLocation || rightLocation) {
    if (!leftLocation || !rightLocation) return false;
    return (
      leftLocation.handle.slot === rightLocation.handle.slot &&
      leftLocation.handle.generation === rightLocation.handle.generation &&
      leftLocation.offset === rightLocation.offset
    );
  }
  if (left.root !== right.root) return false;
  const leftPath = left.path.filter((segment) => segment.kind !== "typeView");
  const rightPath = right.path.filter((segment) => segment.kind !== "typeView");
  if (leftPath.length !== rightPath.length) return false;
  for (let index = 0; index < leftPath.length; index++) {
    const a = leftPath[index]!;
    const b = rightPath[index]!;
    if (a.kind !== b.kind) return false;
    if (a.kind === "field" && b.kind === "field" && a.name !== b.name) return false;
    if (a.kind === "index" && b.kind === "index" && a.index !== b.index) return false;
    if (a.kind === "arrayView" && b.kind === "arrayView" && a.start !== b.start) return false;
  }
  return true;
}

function bytesEqual(left: Uint8Array, right: Uint8Array): boolean {
  if (left.length !== right.length) return false;
  for (let index = 0; index < left.length; index++) if (left[index] !== right[index]) return false;
  return true;
}

export function asIndex(value: Value): number {
  if (value.kind !== "int" && value.kind !== "uint")
    fail("expected integer index", "index", "type_error");
  const integer = value.value as bigint;
  if (integer < 0n || integer > BigInt(Number.MAX_SAFE_INTEGER))
    fail("index out of range", "index", "panic");
  return Number(integer);
}

export function hostValue(value: Value, objects: Cell[], indexes: Map<Cell, number>): HostValue {
  const typ = value.type.registry.identity(value.type);
  let data: HostData;
  switch (value.kind) {
    case "uninitialized":
    case "nil":
      data = "Nil";
      break;
    case "bool":
      data = { Bool: value.value as boolean };
      break;
    case "int":
      data = { Integer: value.value as bigint };
      break;
    case "uint":
      data = { Unsigned: value.value as bigint };
      break;
    case "float":
      data = { FloatBits: floatBits(value.value as number) };
      break;
    case "complex": {
      const complex = value.value as { real: number; imag: number };
      data = { ComplexBits: { real: floatBits(complex.real), imag: floatBits(complex.imag) } };
      break;
    }
    case "string":
    case "bytes":
    case "opaque":
      data = { String: Uint8Array.from(value.value as Uint8Array) };
      break;
    case "array":
      data = {
        Array: packedAggregate(value)
          ? [...packedItems(value)!].map((item) => hostValue(item, objects, indexes))
          : (value.value as Value[]).map((item) => hostValue(item, objects, indexes)),
      };
      break;
    case "struct": {
      const fields: Record<string, HostValue> = {};
      const entries = packedAggregate(value)
        ? packedFields(value)!
        : (value.value as Map<string, Value>);
      for (const [name, item] of entries) fields[name] = hostValue(item, objects, indexes);
      data = { Struct: fields };
      break;
    }
    case "interface":
      data = { Interface: hostValue(value.value as Value, objects, indexes) };
      break;
    case "pointer":
      data = { Pointer: hostAddress(value.value as Address, objects, indexes) };
      break;
    case "slice": {
      const slice = value.value as SliceValue;
      data = {
        Slice: {
          storage: hostAddress(slice.storage, objects, indexes),
          start: BigInt(slice.start),
          length: BigInt(slice.length),
          capacity: BigInt(slice.capacity),
        },
      };
      break;
    }
    case "map": {
      const storage = ensureObject(cell({ ...value, kind: "opaque" }), objects, indexes);
      data = { Map: BigInt(storage) };
      break;
    }
    case "function": {
      const fn = value.value as FunctionValue;
      data = {
        Function: {
          module: fn.module,
          function: fn.function,
          captures: fn.captures.map((address) => hostAddress(address, objects, indexes)),
        },
      };
      break;
    }
    case "dynamicFunction":
      data = { DynamicFunction: hostValue(value.value as Value, objects, indexes) };
      break;
    case "method": {
      const fn = value.value as FunctionValue;
      const functionValue: Value = { ...value, kind: "function", value: fn };
      const method: { function: HostValue; receiver?: HostValue } = {
        function: hostValue(functionValue, objects, indexes),
      };
      if (fn.receiver) method.receiver = hostValue(fn.receiver, objects, indexes);
      data = {
        Method: method,
      };
      break;
    }
    case "channel": {
      const channel = value.value as Channel;
      data = {
        Channel: {
          capacity: BigInt(channel.capacity),
          closed: channel.closed,
          queued: Array.from(channel.queue, (item) => hostValue(item, objects, indexes)),
        },
      };
      break;
    }
    case "mutex": {
      const mutex = value.value as { locked: boolean; waiting: number; granted: boolean };
      data = {
        Mutex: {
          locked: mutex.locked,
          waiting: BigInt(mutex.waiting),
          granted: mutex.granted,
        },
      };
      break;
    }
  }
  return { typ, data };
}

function hostAddress(address: Address, objects: Cell[], indexes: Map<Cell, number>): HostAddress {
  let path = address.path;
  const location = storageAddress(address);
  if (location) {
    const base = address.root.managedLocation;
    const resolved =
      base && pathForStorageOffset(base.layout, base.type, location.type, location.offset);
    if (!resolved)
      fail("address cannot be represented in a host snapshot", "snapshot.address", "internal");
    path = resolved;
  }
  return {
    object: BigInt(ensureObject(address.root, objects, indexes)),
    path: path.map((segment): PathElement => {
      switch (segment.kind) {
        case "field":
          return { Field: segment.name };
        case "index":
          return { Index: BigInt(segment.index) };
        case "arrayView":
          return {
            ArrayView: {
              start: BigInt(segment.start),
              length: BigInt(segment.length),
              typ: segment.type.registry.identity(segment.type),
            },
          };
        case "typeView":
          return { TypeView: segment.type.registry.identity(segment.type) };
      }
    }),
  };
}

function ensureObject(root: Cell, objects: Cell[], indexes: Map<Cell, number>): number {
  let index = indexes.get(root);
  if (index === undefined) {
    index = objects.length;
    indexes.set(root, index);
    objects.push(root);
  }
  return index;
}

export function snapshot(roots: Value[]): Snapshot {
  const objects: Cell[] = [];
  const indexes = new Map<Cell, number>();
  const outputRoots = roots.map((value) => hostValue(value, objects, indexes));
  const outputObjects: HostValue[] = [];
  for (let index = 0; index < objects.length; index++)
    outputObjects.push(hostValue(objects[index]!.read(), objects, indexes));
  return { roots: outputRoots, objects: outputObjects };
}

function floatBits(value: number): bigint {
  const bytes = new ArrayBuffer(8);
  const view = new DataView(bytes);
  view.setFloat64(0, value, true);
  return view.getBigUint64(0, true);
}

export function importHostValue(value: HostValue, registry: TypeRegistry): Value {
  const type = importType(value.typ, registry);
  const data = value.data;
  if (data === "Nil") return nilValue(type);
  if ("Bool" in data) return { type, kind: "bool", value: data.Bool };
  if ("Integer" in data) return normalize({ type, kind: "int", value: data.Integer });
  if ("Unsigned" in data) return normalize({ type, kind: "uint", value: data.Unsigned });
  if ("FloatBits" in data) return { type, kind: "float", value: bitsFloat(data.FloatBits) };
  if ("ComplexBits" in data)
    return {
      type,
      kind: "complex",
      value: { real: bitsFloat(data.ComplexBits.real), imag: bitsFloat(data.ComplexBits.imag) },
    };
  if ("String" in data) {
    const underlying = registry.underlying(type);
    if (underlying.ref.kind === Kind.Slice)
      return {
        type: primitive(registry, PrimitiveKind.PrimitiveString),
        kind: "string",
        value: Uint8Array.from(data.String),
      };
    return { type, kind: "string", value: Uint8Array.from(data.String) };
  }
  if ("Array" in data)
    return {
      type,
      kind: "array",
      value: data.Array.map((item) => importHostValue(item, registry)),
    };
  if ("Struct" in data) {
    const fields = new Map<string, Value>();
    for (const [name, item] of Object.entries(data.Struct))
      fields.set(name, importHostValue(item, registry));
    return { type, kind: "struct", value: fields };
  }
  if ("Interface" in data)
    return { type, kind: "interface", value: importHostValue(data.Interface, registry) };
  fail("host value requires an owned graph snapshot", "arguments", "invalid_argument");
}

function importType(identity: TypeIdentity, registry: TypeRegistry): RuntimeType {
  if (identity === "Void")
    return registry.runtime({ kind: Kind.Void, primitive: 0, named: emptyKey(), node: "" });
  if (identity === "Any")
    return registry.runtime({ kind: Kind.Any, primitive: 0, named: emptyKey(), node: "" });
  if ("Primitive" in identity)
    return registry.runtime({
      kind: Kind.Primitive,
      primitive: identity.Primitive,
      named: emptyKey(),
      node: "",
    });
  if ("Pointer" in identity) {
    const element = importType(identity.Pointer, registry);
    return {
      module: registry.module,
      registry,
      ref: { kind: Kind.Pointer, primitive: 0, named: emptyKey(), node: "" },
      dynamic: { kind: Kind.Pointer, elem: element },
    };
  }
  if ("Slice" in identity) {
    const element = importType(identity.Slice, registry);
    return {
      module: registry.module,
      registry,
      ref: { kind: Kind.Slice, primitive: 0, named: emptyKey(), node: "" },
      dynamic: { kind: Kind.Slice, elem: element },
    };
  }
  if ("Named" in identity)
    return registry.runtime({ kind: Kind.Named, primitive: 0, named: identity.Named, node: "" });
  fail("structural host type is not accepted as an argument", "arguments", "invalid_argument");
}

function emptyKey(): TypeKey {
  return { module_path: "", decl_id: "" };
}

function bitsFloat(bits: bigint): number {
  const bytes = new ArrayBuffer(8);
  const view = new DataView(bytes);
  view.setBigUint64(0, bits, true);
  return view.getFloat64(0, true);
}
