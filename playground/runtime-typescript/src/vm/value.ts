import type {
  HostAddress,
  HostData,
  HostValue,
  PathElement,
  Snapshot,
  TypeIdentity,
} from "./types";
import { Kind, PrimitiveKind, WIRE_FIELDS } from "./contract.generated";
import { JsonNumber, type JsonValue, boolean, fail, integer, schemaObject, string } from "./json";

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
  readonly depth: number;
  readonly fieldCount: number;
}

export class TypeRegistry {
  private readonly nodes = new Map<string, TypeNode>();
  private readonly named = new Map<string, TypeNode>();
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

  runtime(ref: TypeRef): RuntimeType {
    if (ref.kind === Kind.Primitive && Number.isSafeInteger(ref.primitive) && ref.primitive >= 0) {
      const cached = this.primitiveTypes[ref.primitive];
      if (cached) return cached;
      const value = { module: this.module, ref, registry: this };
      this.primitiveTypes[ref.primitive] = value;
      return value;
    }
    return { module: this.module, ref, registry: this };
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
      let depth = 0;
      for (const field of fields) {
        const nested = this.computeFacts(field.type, active);
        containsReferences ||= nested.containsReferences;
        depth = Math.max(depth, nested.depth + 1);
      }
      facts = {
        kind,
        primitive: primitiveKind,
        containsReferences,
        flat: !containsReferences,
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
        depth: nested.depth + 1,
        fieldCount: 0,
      };
    } else {
      facts = {
        kind,
        primitive: primitiveKind,
        containsReferences: true,
        flat: false,
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
  metadata?: unknown;
}

export interface Cell {
  readonly id: number;
  value: Value;
}

export type AddressPath =
  | { kind: "field"; name: string }
  | { kind: "index"; index: number }
  | { kind: "arrayView"; start: number; length: number; type: RuntimeType }
  | { kind: "typeView"; type: RuntimeType };

export interface Address {
  readonly root: Cell;
  readonly path: AddressPath[];
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

export interface GuestMapEntry {
  key: Value;
  value: Value;
}

export interface GuestMap {
  entries: GuestMapEntry[];
  buckets: Map<string, Set<GuestMapEntry>>;
}

export interface Channel {
  capacity: number;
  closed: boolean;
  queue: Value[];
  senders: { task: number; value: Value }[];
  receivers: number[];
}

let nextCell = 1;

export function cell(value: Value): Cell {
  return { id: nextCell++, value };
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

export function dereference(address: Address): Value {
  let value = address.root.value;
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
      const field = (value.value as Map<string, Value>).get(segment.name);
      if (!field) fail("field address is invalid", "address", "type_error");
      value = field;
    } else {
      value = indexValue(value, segment.index);
    }
  }
  return value;
}

export function assign(address: Address, assigned: Value): void {
  if (address.path.length === 0) {
    address.root.value = cloneValue(assigned);
    return;
  }
  let value = address.root.value;
  let arrayOffset = 0;
  for (let index = 0; index < address.path.length - 1; index++) {
    const segment = address.path[index]!;
    if (segment.kind === "typeView") continue;
    if (segment.kind === "arrayView") {
      arrayOffset += segment.start;
      continue;
    }
    value =
      segment.kind === "field"
        ? fieldValue(value, segment.name)
        : indexValue(value, segment.index + arrayOffset);
    arrayOffset = 0;
  }
  const leaf = address.path[address.path.length - 1]!;
  if (leaf.kind === "field") {
    if (value.kind === "pointer") value = dereference(value.value as Address);
    const fields = value.value as Map<string, Value>;
    if (!fields.has(leaf.name)) fail("field address is invalid", "address", "type_error");
    fields.set(leaf.name, cloneValue(assigned));
  } else if (leaf.kind === "index") {
    setIndex(value, leaf.index + arrayOffset, assigned);
  } else if (leaf.kind === "arrayView") {
    if (assigned.kind !== "array")
      fail("array view assignment requires an array", "address", "type_error");
    const values = assigned.value as Value[];
    if (values.length !== leaf.length)
      fail("array view assignment length mismatch", "address", "type_error");
    for (let index = 0; index < values.length; index++)
      setIndex(value, leaf.start + arrayOffset + index, values[index]!);
  } else {
    assign(
      { root: address.root, path: address.path.slice(0, -1) },
      { ...cloneValue(assigned), type: leaf.type },
    );
  }
}

export function fieldValue(value: Value, name: string): Value {
  if (value.kind === "interface") value = value.value as Value;
  if (value.kind === "pointer") value = dereference(value.value as Address);
  if (value.kind === "nil" && value.type.registry.underlying(value.type).ref.kind === Kind.Pointer)
    fail("nil pointer dereference", "field", "panic");
  if (value.kind !== "struct") fail(`field access targets ${value.kind}`, "field", "type_error");
  const field = (value.value as Map<string, Value>).get(name);
  if (!field) fail("unknown field", name, "type_error");
  return field;
}

export function indexValue(value: Value, index: number): Value {
  if (!Number.isSafeInteger(index) || index < 0) fail("index out of range", "index", "panic");
  if (value.kind === "pointer") return indexValue(dereference(value.value as Address), index);
  if (value.kind === "slice") {
    const slice = value.value as SliceValue;
    if (index >= slice.length) fail("index out of range", "index", "panic");
    return dereference({
      root: slice.storage.root,
      path: [...slice.storage.path, { kind: "index", index: slice.start + index }],
    });
  }
  if (value.kind === "array") {
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
    assign({ root: address.root, path: [...address.path, { kind: "index", index }] }, assigned);
    return;
  }
  if (value.kind === "slice") {
    const slice = value.value as SliceValue;
    if (index >= slice.length) fail("index out of range", "index", "panic");
    assign(
      {
        root: slice.storage.root,
        path: [...slice.storage.path, { kind: "index", index: slice.start + index }],
      },
      assigned,
    );
    return;
  }
  if (value.kind === "array") {
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
      const a = left.value as Value[];
      const b = right.value as Value[];
      return a.length === b.length && a.every((item, index) => guestEqual(item, b[index]!));
    }
    case "struct": {
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
  const path = address.path.filter((segment) => segment.kind !== "typeView");
  let text = `root:${address.root.id}/path:${path.length};`;
  for (const segment of path) {
    if (segment.kind === "field") text += `f${segment.name.length}:${segment.name};`;
    else if (segment.kind === "index") text += `i${segment.index};`;
    else if (segment.kind === "arrayView") text += `a${segment.start};`;
  }
  return hashText(text);
}

function hashValue(value: Value, seen = new Set<object>()): string {
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
      const array = value.value as Value[];
      if (seen.has(array)) return `${text}cycle`;
      seen.add(array);
      text += array.map((item) => hashValue(item, seen)).join(",");
      seen.delete(array);
      break;
    }
    case "struct": {
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

export function createGuestMap(): GuestMap {
  return { entries: [], buckets: new Map() };
}

export function guestMapGet(map: GuestMap, key: Value): GuestMapEntry | undefined {
  const bucket = map.buckets.get(hashValue(key));
  if (!bucket) return undefined;
  for (const entry of bucket) if (guestEqual(entry.key, key)) return entry;
  return undefined;
}

export function guestMapSet(map: GuestMap, key: Value, value: Value): GuestMapEntry {
  const hash = hashValue(key);
  let bucket = map.buckets.get(hash);
  if (!bucket) {
    bucket = new Set();
    map.buckets.set(hash, bucket);
  }
  for (const entry of bucket) {
    if (guestEqual(entry.key, key)) {
      entry.value = value;
      return entry;
    }
  }
  const entry = { key, value };
  map.entries.push(entry);
  bucket.add(entry);
  return entry;
}

export function guestMapDelete(map: GuestMap, key: Value): boolean {
  const hash = hashValue(key);
  const bucket = map.buckets.get(hash);
  if (!bucket) return false;
  for (const entry of bucket) {
    if (!guestEqual(entry.key, key)) continue;
    bucket.delete(entry);
    if (!bucket.size) map.buckets.delete(hash);
    const index = map.entries.indexOf(entry);
    if (index >= 0) map.entries.splice(index, 1);
    return true;
  }
  return false;
}

export function guestMapClear(map: GuestMap): void {
  map.entries.length = 0;
  map.buckets.clear();
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
    for (const item of value.value as Value[]) assertComparable(item);
  } else if (value.kind === "struct") {
    for (const [name, item] of value.value as Map<string, Value>)
      if (name !== "_") assertComparable(item);
  }
}

function addressEqual(left: Address, right: Address): boolean {
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
      data = { Array: (value.value as Value[]).map((item) => hostValue(item, objects, indexes)) };
      break;
    case "struct": {
      const fields: Record<string, HostValue> = {};
      for (const [name, item] of value.value as Map<string, Value>)
        fields[name] = hostValue(item, objects, indexes);
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
          queued: channel.queue.map((item) => hostValue(item, objects, indexes)),
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
  return {
    object: BigInt(ensureObject(address.root, objects, indexes)),
    path: address.path.map((segment): PathElement => {
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
    outputObjects.push(hostValue(objects[index]!.value, objects, indexes));
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
  if ("String" in data) return { type, kind: "string", value: Uint8Array.from(data.String) };
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
