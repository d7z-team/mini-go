import {
  CONTRACT,
  INTRINSICS,
  Kind,
  PrimitiveKind,
  OPCODE_ARITIES,
  OPCODE_DESCRIPTOR_TABLES,
  OPCODE_NAMES,
  DESCRIPTOR_FIELDS,
  WIRE_FIELDS,
} from "./contract.generated";
import {
  JsonNumber,
  VMError,
  array,
  boolean,
  decodeJson,
  fail,
  integer,
  object,
  schemaObject,
  sha256Hex,
  string,
  type JsonValue,
} from "./json";
import {
  TypeRegistry,
  cell,
  decodeConstant,
  parseSignature,
  parseTypeRef,
  primitive,
  zero,
  type Cell,
  type FunctionSignature,
  type RuntimeType,
  type TypeRef,
  type Value,
} from "./value";
import { OperatorCode, operatorCode, unaryOperatorCode, type UnaryOperatorCode } from "./operators";
import type { FrameReferences } from "./reference-plan";
import { prepareFrameReferences } from "./reference-plan";

export interface LoadOptions {
  maxImageBytes: number;
  maxArtifactBytes: number;
  maxPackages: number;
  maxTypeNodes: number;
}

export interface PreparedOperands {
  inputs: readonly (readonly [number, number])[];
  outputs: readonly number[];
  release: readonly number[];
  releaseBefore: readonly number[];
}

export interface PreparedSelectCase {
  channel: number;
  send?: number;
  value?: number;
  ok?: number;
}

export type PreparedAddressPath =
  | { readonly kind: "field"; readonly field: string }
  | { readonly kind: "index"; readonly local: number }
  | { readonly kind: "indirect" };

export interface PreparedAddressPlan {
  readonly kind: "local" | "upvalue" | "global" | "export";
  readonly index: number;
  readonly module?: PreparedModule;
  readonly path: readonly PreparedAddressPath[];
  readonly targetType: RuntimeType;
}

export interface PreparedTypeCase {
  readonly type?: RuntimeType;
  readonly binding?: number;
  readonly target: number;
  readonly original: boolean;
}

export interface PreparedTypeDispatch {
  readonly subject: number;
  readonly defaultBinding?: number;
  readonly defaultTarget: number;
  readonly cases: readonly PreparedTypeCase[];
}

export interface PreparedExport {
  readonly kind: string;
  readonly id: string;
  readonly type?: RuntimeType;
  readonly untyped: boolean;
}

export interface PreparedInstruction {
  opcode: number;
  /** Compiler-selected primitive lanes: input in low byte, output in high byte. */
  mode: number;
  /** Source-level fuel represented by this dispatch. */
  logicalCost: number;
  name: string;
  /** Index in the wire instruction array, retained for source/debug mapping. */
  wirePC: number;
  descriptor: Record<string, unknown> | undefined;
  operands: PreparedOperands;
  arity: readonly [number, number];
  inputScratch: boolean;
  /** Effects not already accounted for at a value storage boundary. */
  changesReachability: boolean;
  /** Loader-proven private scalar operation with no allocation or fault exit. */
  fastScalar?: boolean;
  target?: number;
  local?: number;
  upvalue?: number;
  global?: number;
  constant?: number;
  type?: RuntimeType;
  /** First two operands and first two outputs are read without inputScratch. */
  input0?: readonly [number, number];
  input1?: readonly [number, number];
  input2?: readonly [number, number];
  output0?: number;
  output1?: number;
  operatorCode?: OperatorCode | UnaryOperatorCode;
  updateOperatorCode?: OperatorCode;
  /** Source primitive for the allocation-free scalar lane path. */
  scalarPrimitive?: number;
  rebind?: boolean;
  negate?: boolean;
  when?: boolean;
  ownerDepth?: number;
  expand?: boolean;
  hasCapacity?: boolean;
  elementCount?: number;
  entryCount?: number;
  minMax?: "min" | "max";
  modulePath?: string;
  functionID?: string;
  exportName?: string;
  structFields?: readonly string[];
  selectDefault?: boolean;
  /** Valid only in the caller's source Program; old logical calls resolve current by identity. */
  callTarget?: PreparedFunction;
  method?: string;
  intrinsic?: string;
  field?: string;
  fieldPath?: readonly number[];
  /** Prepared frame-local index for map iterator state. */
  iteratorLocal?: number;
  address?: PreparedAddressPlan;
  captures?: readonly PreparedAddressPlan[];
  typeDispatch?: PreparedTypeDispatch;
  exportModule?: PreparedModule;
  exported?: PreparedExport;
  selectIndex?: number;
  selectCases?: readonly PreparedSelectCase[];
}

export interface PreparedBlock {
  readonly first: number;
  readonly count: number;
  readonly fallthrough?: number;
  readonly logicalCost: number;
}

export interface PreparedFunction {
  readonly id: string;
  readonly module: PreparedModule;
  readonly signature: FunctionSignature;
  readonly localTypes: RuntimeType[];
  readonly localIndexes: ReadonlyMap<string, number>;
  readonly cellLocals: ReadonlySet<number>;
  readonly resultLocals: number[];
  readonly upvalueTypes: RuntimeType[];
  readonly upvalueIndexes: ReadonlyMap<string, number>;
  readonly slotTypes: RuntimeType[];
  readonly maxInputArity: number;
  readonly instructions: PreparedInstruction[];
  readonly blocks: readonly PreparedBlock[];
  readonly blockAtPC: readonly number[];
  readonly fastRunEnd: readonly number[];
  readonly labels: ReadonlyMap<string, number>;
  /** Wire-to-executable mapping after label-only instructions are stripped. */
  readonly wireToExecutablePC: readonly number[];
  /** Executable instruction to original wire PC mapping. */
  readonly executableToWirePC: readonly number[];
  references?: FrameReferences;
}

export interface PreparedModule {
  readonly path: string;
  readonly packageName: string;
  readonly registry: TypeRegistry;
  readonly constants: Value[];
  /** Pair-u32 cache for integer constants; two words per constant. */
  readonly constantIntegerBits: Uint32Array;
  readonly constantUntyped: boolean[];
  readonly constantIndexes: ReadonlyMap<string, number>;
  readonly globals: Cell[];
  readonly globalIndexes: ReadonlyMap<string, number>;
  readonly functions: ReadonlyMap<string, PreparedFunction>;
  readonly exports: ReadonlyMap<string, PreparedExport>;
  readonly requirements: ReadonlySet<string>;
  readonly artifactHash: string;
}

function hasManagedReference(type: RuntimeType, active = new Set<string>()): boolean {
  const underlying = type.registry.underlying(type);
  const key = `${underlying.module}\0${underlying.registry.key(underlying)}`;
  if (active.has(key)) return true;
  const node = underlying.registry.node(underlying);
  const kind = underlying.dynamic?.kind ?? node?.kind ?? underlying.ref.kind;
  if (kind === Kind.Primitive) return underlying.ref.primitive === PrimitiveKind.PrimitiveString;
  if (
    kind === Kind.Pointer ||
    kind === Kind.Slice ||
    kind === Kind.Map ||
    kind === Kind.Interface ||
    kind === Kind.Any ||
    kind === Kind.Function ||
    kind === Kind.Waitable
  )
    return true;
  const next = new Set(active).add(key);
  if (kind === Kind.Array) {
    const element = underlying.registry.element(underlying);
    return element ? hasManagedReference(element, next) : false;
  }
  if (kind === Kind.Struct) {
    const fields =
      underlying.dynamic?.fields ??
      (node?.fields ?? []).map((field) => ({
        name: field.name,
        type: underlying.registry.runtime(field.type),
        tag: field.tag,
        embedded: field.embedded,
      }));
    return fields.some((field) => hasManagedReference(field.type, next));
  }
  return false;
}

export interface ProgramEntry {
  name: string;
  module: string;
  function: string;
}

class ReadonlyMapSnapshot<K, V> implements ReadonlyMap<K, V> {
  readonly #values: Map<K, V>;

  constructor(source: ReadonlyMap<K, V>) {
    this.#values = new Map(source);
  }

  get size(): number {
    return this.#values.size;
  }
  get(key: K): V | undefined {
    return this.#values.get(key);
  }
  has(key: K): boolean {
    return this.#values.has(key);
  }
  entries(): MapIterator<[K, V]> {
    return this.#values.entries();
  }
  keys(): MapIterator<K> {
    return this.#values.keys();
  }
  values(): MapIterator<V> {
    return this.#values.values();
  }
  forEach(callback: (value: V, key: K, map: ReadonlyMap<K, V>) => void, thisArg?: unknown): void {
    for (const [key, value] of this.#values) callback.call(thisArg, value, key, this);
  }
  [Symbol.iterator](): MapIterator<[K, V]> {
    return this.#values[Symbol.iterator]();
  }
}

class ReadonlySetSnapshot<T> implements ReadonlySet<T> {
  readonly #values: Set<T>;

  constructor(source: ReadonlySet<T>) {
    this.#values = new Set(source);
  }

  get size(): number {
    return this.#values.size;
  }
  has(value: T): boolean {
    return this.#values.has(value);
  }
  entries(): SetIterator<[T, T]> {
    return this.#values.entries();
  }
  keys(): SetIterator<T> {
    return this.#values.keys();
  }
  values(): SetIterator<T> {
    return this.#values.values();
  }
  forEach(callback: (value: T, key: T, set: ReadonlySet<T>) => void, thisArg?: unknown): void {
    for (const value of this.#values) callback.call(thisArg, value, value, this);
  }
  [Symbol.iterator](): SetIterator<T> {
    return this.#values[Symbol.iterator]();
  }
}

export class Program {
  readonly #entries: Map<string, ProgramEntry>;
  readonly #modules: Map<string, PreparedModule>;

  private constructor(
    readonly hash: string,
    readonly root: string,
    readonly compilerID: string,
    readonly targetTags: readonly string[],
    readonly capabilities: readonly string[],
    entries: Map<string, ProgramEntry>,
    modules: Map<string, PreparedModule>,
  ) {
    this.#entries = new Map(entries);
    this.#modules = new Map(modules);
  }

  static async load(bytes: Uint8Array, limits: LoadOptions): Promise<Program> {
    const document = await decodeJson(bytes, limits.maxImageBytes);
    const image = schemaObject(document.value, "image", WIRE_FIELDS.ExecutionImage);
    if (string(image.format, "image.format") !== CONTRACT.execution_format)
      fail("unsupported execution image format", "image.format");
    if (integer(image.version, "image.version") !== CONTRACT.execution_version)
      fail("unsupported execution image version", "image.version");
    if (string(image.contract_id, "image.contract_id") !== CONTRACT.execution_contract)
      fail("unsupported execution image contract", "image.contract_id");
    const compilerID = string(image.compiler_id, "image.compiler_id");
    if (compilerID !== CONTRACT.compiler_id)
      fail("compiler identity mismatch", "image.compiler_id");
    const root = string(image.root, "image.root");
    const target = schemaObject(image.target!, "image.target", WIRE_FIELDS.Target);
    const targetTags = array(target.tags, "image.target.tags").map((tag, index) =>
      string(tag, `image.target.tags[${index}]`),
    );
    if (
      !targetTags.includes("minigo") ||
      targetTags.some((tag) => !/^[A-Za-z0-9_.]+$/.test(tag)) ||
      targetTags.some((tag, index) => index > 0 && targetTags[index - 1]! >= tag)
    )
      fail(
        "target tags must be sorted unique canonical names containing minigo",
        "image.target.tags",
      );
    const capabilities = array(image.capabilities, "image.capabilities").map((capability, index) =>
      string(capability, `image.capabilities[${index}]`),
    );
    if (
      capabilities.some((name) => !name.trim()) ||
      capabilities.some((name, index) => index > 0 && capabilities[index - 1]! >= name)
    )
      fail("capabilities must be sorted unique non-empty names", "image.capabilities");
    const packages = object(image.packages!, "image.packages");
    const packageNames = Object.keys(packages);
    if (packageNames.length === 0) fail("execution image contains no packages", "image.packages");
    if (packageNames.length > limits.maxPackages)
      fail("package count exceeds limit", "image.packages", "load_limit");

    const modules = new Map<string, PreparedModule>();
    const artifacts = new Map<string, { artifact: Record<string, JsonValue>; hash: string }>();
    let artifactBytes = 0;
    for (const modulePath of packageNames) {
      const archivePath = `image.packages.${modulePath}`;
      const archive = schemaObject(packages[modulePath]!, archivePath, WIRE_FIELDS.PackageArchive);
      const artifact = schemaObject(
        archive.artifact!,
        `${archivePath}.artifact`,
        WIRE_FIELDS.Artifact,
      );
      const artifactHash = string(archive.artifact_hash, `${archivePath}.artifact_hash`);
      const encodedArtifact = document.bytes(artifact);
      artifactBytes += encodedArtifact.byteLength;
      if (artifactBytes > limits.maxArtifactBytes)
        fail("artifact bytes exceed limit", archivePath, "load_limit");
      if ((await sha256Hex(encodedArtifact)) !== artifactHash)
        fail("artifact hash mismatch", `${archivePath}.artifact_hash`);
      if (string(artifact.format, `${archivePath}.artifact.format`) !== CONTRACT.spec.format)
        fail("unsupported artifact format", `${archivePath}.artifact.format`);
      if (integer(artifact.version, `${archivePath}.artifact.version`) !== CONTRACT.spec.version)
        fail("unsupported artifact version", `${archivePath}.artifact.version`);
      if (
        string(artifact.opcode_set, `${archivePath}.artifact.opcode_set`) !==
        CONTRACT.spec.opcode_set
      )
        fail("unsupported opcode set", `${archivePath}.artifact.opcode_set`);
      const module = schemaObject(
        artifact.module!,
        `${archivePath}.artifact.module`,
        WIRE_FIELDS.Module,
      );
      if (string(module.path, `${archivePath}.artifact.module.path`) !== modulePath)
        fail("package key does not match module path", `${archivePath}.artifact.module.path`);
      const packageName = string(module.package, `${archivePath}.artifact.module.package`);
      if (!modulePath.trim() || !packageName.trim())
        fail("module path and package name must be non-empty", `${archivePath}.artifact.module`);
      const table = artifact.type_table;
      const tableObject =
        table === undefined
          ? undefined
          : schemaObject(table, `${archivePath}.artifact.type_table`, WIRE_FIELDS.TypeTable);
      if (
        tableObject &&
        array(tableObject.nodes, `${archivePath}.artifact.type_table.nodes`).length >
          limits.maxTypeNodes
      )
        fail(
          "type node count exceeds limit",
          `${archivePath}.artifact.type_table.nodes`,
          "load_limit",
        );
      artifacts.set(modulePath, {
        artifact,
        hash: artifactHash,
      });
      const registry = new TypeRegistry(modulePath, table);
      modules.set(modulePath, {
        path: modulePath,
        packageName,
        registry,
        constants: [],
        constantIntegerBits: new Uint32Array(
          array(artifact.constants, `${archivePath}.artifact.constants`).length * 2,
        ),
        constantUntyped: [],
        constantIndexes: new Map(),
        globals: [],
        globalIndexes: new Map(),
        functions: new Map(),
        exports: new Map(),
        requirements: new Set(),
        artifactHash,
      });
    }

    for (const [modulePath, source] of artifacts)
      prepareModule(modules.get(modulePath)!, source.artifact);
    if (!modules.has(root)) fail("root module does not exist", "image.root", "link_error");
    validateRequirements(modules, artifacts);
    validatePreparedProgram(modules);
    for (const module of modules.values())
      for (const fn of module.functions.values())
        fn.references = prepareFrameReferences(
          fn,
          fn.localTypes.map((type) => hasManagedReference(type)),
          fn.slotTypes.map((type) => hasManagedReference(type)),
        );
    sealPreparedProgram(modules);

    const entries = new Map<string, ProgramEntry>();
    for (const [index, item] of array(image.entries, "image.entries").entries()) {
      const path = `image.entries[${index}]`;
      const entry = schemaObject(item, path, WIRE_FIELDS.Entry);
      const prepared: ProgramEntry = {
        name: string(entry.name, `${path}.name`),
        module: string(entry.module_path, `${path}.module_path`),
        function: string(entry.function_id, `${path}.function_id`),
      };
      if (entries.has(prepared.name)) fail("duplicate entry name", `${path}.name`);
      if (!prepared.name) fail("entry name is empty", `${path}.name`);
      if (prepared.module !== root)
        fail("entry must belong to the root module", `${path}.module_path`, "link_error");
      if (!modules.get(prepared.module)?.functions.has(prepared.function))
        fail("entry function does not exist", path);
      entries.set(prepared.name, Object.freeze(prepared));
    }
    if (!entries.size) fail("execution image contains no entries", "image.entries");
    const imageHash = string(image.hash, "image.hash");
    const calculatedHash = await sha256Hex(document.replacingProperty(image, "hash", '""'));
    if (calculatedHash !== imageHash) fail("execution image hash mismatch", "image.hash");
    return new Program(imageHash, root, compilerID, targetTags, capabilities, entries, modules);
  }

  function(module: string, id: string): PreparedFunction {
    const fn = this.#modules.get(module)?.functions.get(id);
    if (!fn) throw new VMError(`unknown function ${module}.${id}`, "link_error", "function");
    return fn;
  }

  entry(name: string): ProgramEntry | undefined {
    return this.#entries.get(name);
  }

  module(path: string): PreparedModule | undefined {
    return this.#modules.get(path);
  }

  moduleValues(): IterableIterator<PreparedModule> {
    return this.#modules.values();
  }
}

function prepareModule(module: PreparedModule, artifact: Record<string, JsonValue>): void {
  for (const [index, item] of array(artifact.constants, `${module.path}.constants`).entries()) {
    const path = `${module.path}.constants[${index}]`;
    const constant = schemaObject(item, path, WIRE_FIELDS.Constant);
    const id = string(constant.id, `${path}.id`);
    if (module.constantIndexes.has(id)) fail("duplicate constant", `${path}.id`);
    if (!constant.type || constant.value === undefined) fail("invalid constant", path);
    const type = module.registry.runtime(parseTypeRef(constant.type, `${path}.type`));
    (module.constantIndexes as Map<string, number>).set(id, module.constants.length);
    const decoded = decodeConstant(type, constant.value);
    module.constants.push(decoded);
    if (decoded.kind === "int" || decoded.kind === "uint") {
      const bits = BigInt.asUintN(64, decoded.value as bigint);
      module.constantIntegerBits[index * 2] = Number(bits & 0xffff_ffffn);
      module.constantIntegerBits[index * 2 + 1] = Number(bits >> 32n);
    }
    module.constantUntyped.push(boolean(constant.untyped, `${path}.untyped`));
  }
  for (const [index, item] of array(artifact.globals, `${module.path}.globals`).entries()) {
    const path = `${module.path}.globals[${index}]`;
    const global = schemaObject(item, path, WIRE_FIELDS.Global);
    const id = string(global.id, `${path}.id`);
    if (module.globalIndexes.has(id)) fail("duplicate global", `${path}.id`);
    if (!global.type) fail("global is missing type", `${path}.type`);
    const type = module.registry.runtime(parseTypeRef(global.type, `${path}.type`));
    (module.globalIndexes as Map<string, number>).set(id, module.globals.length);
    module.globals.push(cell(zero(type)));
  }
  for (const [index, item] of array(artifact.exports, `${module.path}.exports`).entries()) {
    const path = `${module.path}.exports[${index}]`;
    const exported = schemaObject(item, path, WIRE_FIELDS.Export);
    const name = string(exported.name, `${path}.name`);
    if (module.exports.has(name)) fail("duplicate export", `${path}.name`);
    const declaration: { kind: string; id: string; type?: RuntimeType; untyped: boolean } = {
      kind: string(exported.kind, `${path}.kind`),
      id: string(exported.id, `${path}.id`),
      untyped: boolean(exported.untyped, `${path}.untyped`),
    };
    if (exported.type)
      declaration.type = module.registry.runtime(parseTypeRef(exported.type, `${path}.type`));
    (
      module.exports as Map<
        string,
        { kind: string; id: string; type?: RuntimeType; untyped: boolean }
      >
    ).set(name, declaration);
  }

  const functions = array(artifact.functions, `${module.path}.functions`);
  for (const [index, item] of functions.entries()) {
    const path = `${module.path}.functions[${index}]`;
    const raw = schemaObject(item, path, WIRE_FIELDS.Function);
    const id = string(raw.id, `${path}.id`);
    if (module.functions.has(id)) fail("duplicate function", `${path}.id`);
    if (!raw.signature || !raw.code) fail("function is incomplete", path);
    const code = schemaObject(raw.code, `${path}.code`, WIRE_FIELDS.MicroCode);
    const localTypes: RuntimeType[] = [];
    const localIndexes = new Map<string, number>();
    for (const [localIndex, localValue] of array(raw.locals, `${path}.locals`).entries()) {
      const localPath = `${path}.locals[${localIndex}]`;
      const local = schemaObject(localValue, localPath, WIRE_FIELDS.Local);
      const localID = string(local.id, `${localPath}.id`);
      if (localIndexes.has(localID)) fail("duplicate local", `${localPath}.id`);
      if (!local.type) fail("local is missing type", `${localPath}.type`);
      localIndexes.set(localID, localTypes.length);
      localTypes.push(module.registry.runtime(parseTypeRef(local.type, `${localPath}.type`)));
    }
    const upvalueTypes: RuntimeType[] = [];
    const upvalueIndexes = new Map<string, number>();
    for (const [upvalueIndex, upvalueValue] of array(raw.upvalues, `${path}.upvalues`).entries()) {
      const upvaluePath = `${path}.upvalues[${upvalueIndex}]`;
      const upvalue = schemaObject(upvalueValue, upvaluePath, WIRE_FIELDS.Upvalue);
      const upvalueID = string(upvalue.id, `${upvaluePath}.id`);
      if (upvalueIndexes.has(upvalueID)) fail("duplicate upvalue", `${upvaluePath}.id`);
      if (!upvalue.type) fail("upvalue is missing type", `${upvaluePath}.type`);
      upvalueIndexes.set(upvalueID, upvalueTypes.length);
      upvalueTypes.push(module.registry.runtime(parseTypeRef(upvalue.type, `${upvaluePath}.type`)));
    }
    const slotTypes = array(code.types, `${path}.code.types`).map((type, typeIndex) =>
      module.registry.runtime(parseTypeRef(type, `${path}.code.types[${typeIndex}]`)),
    );
    const operandRecords = array(code.operands, `${path}.code.operands`).map(
      (record, recordIndex) => prepareOperands(record, `${path}.code.operands[${recordIndex}]`),
    );
    const descriptors = schemaObject(
      code.descriptors!,
      `${path}.code.descriptors`,
      WIRE_FIELDS.DescriptorTables,
    );
    const instructions: PreparedInstruction[] = [];
    const labels = new Map<string, number>();
    const wireToExecutablePC: number[] = [];
    const executableToWirePC: number[] = [];
    let executablePC = 0;
    let maxInputArity = 0;
    for (const [pc, item] of array(code.instructions, `${path}.code.instructions`).entries()) {
      const instructionPath = `${path}.code.instructions[${pc}]`;
      if (!Array.isArray(item) || item.length !== 4)
        fail(
          "instruction must contain packed opcode/mode, descriptor, operands and cost",
          instructionPath,
        );
      const opcodeAndMode = integer(item[0], `${instructionPath}[0]`);
      validateU32(opcodeAndMode, `${instructionPath}[0]`);
      const opcode = opcodeAndMode & 0xffff;
      const mode = opcodeAndMode >>> 16;
      const descriptorIndex = integer(item[1], `${instructionPath}[1]`);
      const operandIndex = integer(item[2], `${instructionPath}[2]`);
      const logicalCost = integer(item[3], `${instructionPath}[3]`);
      validateU32(descriptorIndex, `${instructionPath}[1]`);
      validateU32(operandIndex, `${instructionPath}[2]`);
      validateU32(logicalCost, `${instructionPath}[3]`);
      if (logicalCost === 0) fail("instruction cost must be positive", `${instructionPath}[3]`);
      const name = OPCODE_NAMES[opcode];
      if (!name || opcode === 0) fail("unknown opcode", `${instructionPath}[0]`);
      const tableName = OPCODE_DESCRIPTOR_TABLES[opcode]!;
      let descriptor: Record<string, unknown> | undefined;
      if (tableName) {
        const table = array(descriptors[tableName], `${path}.code.descriptors.${tableName}`);
        if (descriptorIndex >= table.length)
          fail("descriptor index out of range", `${instructionPath}[1]`);
        descriptor = plainObject(
          table[descriptorIndex]!,
          `${path}.code.descriptors.${tableName}[${descriptorIndex}]`,
          DESCRIPTOR_FIELDS[tableName as keyof typeof DESCRIPTOR_FIELDS],
        );
      } else if (descriptorIndex !== 0) {
        fail("descriptor-free opcode requires descriptor zero", `${instructionPath}[1]`);
      }
      if (operandIndex >= operandRecords.length)
        fail("operand index out of range", `${instructionPath}[2]`);
      wireToExecutablePC[pc] = executablePC;
      if (name === "label") {
        const label = descriptor?.label;
        if (typeof label !== "string" || !label.trim() || labels.has(label))
          fail("invalid or duplicate label", instructionPath);
        const operands = operandRecords[operandIndex]!;
        const arity = instructionArity({ opcode, name, descriptor }, instructionPath);
        if (operands.inputs.length !== arity[0] || operands.outputs.length !== arity[1])
          fail("operand arity mismatch", instructionPath, "invalid_operand");
        if (operands.release.length || operands.releaseBefore.length)
          fail("label cannot release slots", instructionPath, "invalid_operand");
        labels.set(label, executablePC);
        continue;
      }
      executableToWirePC.push(pc);
      const prepared = {
        opcode,
        mode,
        logicalCost,
        name,
        wirePC: pc,
        descriptor,
        operands: operandRecords[operandIndex]!,
      };
      const arity = instructionArity(prepared, instructionPath);
      instructions.push({
        ...prepared,
        arity,
        changesReachability: [
          "make_closure",
          "make_sequence",
          "make_map",
          "make_slice",
          "make_waitable",
          "delete",
          "clear",
          "waitable_close",
          "init_module",
          "spawn",
          "call_ffi",
          "call_intrinsic",
          "select",
        ].includes(name),
        inputScratch:
          (arity[0] > 2 && name !== "update_compare_branch") ||
          [17, 21, 22, 23, 24, 26, 27, 28, 29, 35, 38, 65, 66, 67, 72].includes(opcode),
      });
      maxInputArity = Math.max(maxInputArity, prepared.operands.inputs.length);
      executablePC++;
    }
    wireToExecutablePC.push(executablePC);
    const blocks: PreparedBlock[] = [];
    const blockAtPC = new Array<number>(instructions.length);
    let expectedFirst = 0;
    for (const [blockIndex, item] of array(code.blocks, `${path}.code.blocks`).entries()) {
      const blockPath = `${path}.code.blocks[${blockIndex}]`;
      const rawBlock = schemaObject(item, blockPath, WIRE_FIELDS.BasicBlock);
      const first = integer(rawBlock.first, `${blockPath}.first`);
      const count = integer(rawBlock.count, `${blockPath}.count`);
      const fallthrough = integer(rawBlock.fallthrough, `${blockPath}.fallthrough`);
      const logicalCost = integer(rawBlock.cost, `${blockPath}.cost`);
      for (const [value, field] of [
        [first, "first"],
        [count, "count"],
        [fallthrough, "fallthrough"],
        [logicalCost, "cost"],
      ] as const)
        validateU32(value, `${blockPath}.${field}`);
      if (first !== expectedFirst || count === 0 || first + count > instructions.length)
        fail("basic blocks must be contiguous non-empty executable ranges", blockPath);
      let calculatedCost = 0;
      for (let pc = first; pc < first + count; pc++) {
        calculatedCost += instructions[pc]!.logicalCost;
        blockAtPC[pc] = blockIndex;
      }
      if (logicalCost !== calculatedCost) fail("basic block cost mismatch", `${blockPath}.cost`);
      const prepared: PreparedBlock = {
        first,
        count,
        logicalCost,
        ...(fallthrough === MAX_U32 ? {} : { fallthrough }),
      };
      if (
        prepared.fallthrough !== undefined &&
        prepared.fallthrough >= array(code.blocks, `${path}.code.blocks`).length
      )
        fail("basic block fallthrough is out of range", `${blockPath}.fallthrough`);
      blocks.push(Object.freeze(prepared));
      expectedFirst += count;
    }
    if (instructions.length !== 0 && (blocks.length === 0 || expectedFirst !== instructions.length))
      fail("basic blocks do not cover executable instructions", `${path}.code.blocks`);
    const seenResults = new Set<string>();
    const resultLocals = array(raw.result_locals, `${path}.result_locals`).map(
      (result, resultIndex) => {
        const resultID = string(result, `${path}.result_locals[${resultIndex}]`);
        if (seenResults.has(resultID))
          fail("duplicate result local", `${path}.result_locals[${resultIndex}]`);
        seenResults.add(resultID);
        const local = localIndexes.get(resultID);
        if (local === undefined)
          fail("unknown result local", `${path}.result_locals[${resultIndex}]`);
        return local;
      },
    );
    const signature = parseSignature(raw.signature, `${path}.signature`);
    if (localTypes.length < signature.params.length)
      fail("function has fewer locals than parameters", `${path}.locals`);
    if (resultLocals.length && resultLocals.length !== signature.results.length)
      fail("result locals do not match function results", `${path}.result_locals`);
    const cellLocals = new Set<number>();
    // Named results remain addressable through defer/recover and therefore
    // need stable cells even without an explicit address/capture instruction.
    for (const result of resultLocals) cellLocals.add(result);
    for (const instruction of instructions) {
      const descriptor = instruction.descriptor ?? {};
      if (instruction.name === "address_of") {
        if (descriptor.kind === "local") {
          const index = localIndexes.get(String(descriptor.local));
          if (index !== undefined) cellLocals.add(index);
        }
      } else if (instruction.name === "make_closure") {
        for (const capture of descriptorList(descriptor, "captures", path)) {
          const address = descriptorRecord(capture, path);
          if (address.kind !== "local") continue;
          const index = localIndexes.get(String(address.local));
          if (index !== undefined) cellLocals.add(index);
        }
      }
    }
    const fn: PreparedFunction = {
      id,
      module,
      signature,
      localTypes,
      localIndexes,
      cellLocals,
      resultLocals,
      upvalueTypes,
      upvalueIndexes,
      slotTypes,
      maxInputArity,
      instructions,
      blocks,
      blockAtPC,
      fastRunEnd: new Array<number>(instructions.length).fill(0),
      labels,
      wireToExecutablePC,
      executableToWirePC,
    };
    (module.functions as Map<string, PreparedFunction>).set(id, fn);
  }
}

function sealPreparedProgram(modules: Map<string, PreparedModule>): void {
  for (const module of modules.values()) {
    for (const fn of module.functions.values()) {
      Object.defineProperties(fn, {
        localIndexes: { value: new ReadonlyMapSnapshot(fn.localIndexes) },
        upvalueIndexes: { value: new ReadonlyMapSnapshot(fn.upvalueIndexes) },
        labels: { value: new ReadonlyMapSnapshot(fn.labels) },
      });
      Object.freeze(fn.localTypes);
      Object.freeze(fn.resultLocals);
      Object.freeze(fn.upvalueTypes);
      Object.freeze(fn.slotTypes);
      Object.freeze(fn.wireToExecutablePC);
      Object.freeze(fn.executableToWirePC);
      Object.freeze(fn.blocks);
      Object.freeze(fn.blockAtPC);
      Object.freeze(fn.fastRunEnd);
      for (const instruction of fn.instructions) {
        Object.freeze(instruction.operands.inputs);
        Object.freeze(instruction.operands.outputs);
        Object.freeze(instruction.operands.release);
        Object.freeze(instruction.operands.releaseBefore);
        Object.freeze(instruction.operands);
        // The verifier has compiled every static field into typed metadata.
        // Drop the wire object so execution cannot retain a second decoding
        // path or observe mutable descriptor state.
        instruction.descriptor = undefined;
        Object.freeze(instruction);
      }
      Object.freeze(fn.instructions);
      Object.freeze(fn);
    }
    Object.defineProperties(module, {
      constantIndexes: { value: new ReadonlyMapSnapshot(module.constantIndexes) },
      globalIndexes: { value: new ReadonlyMapSnapshot(module.globalIndexes) },
      functions: { value: new ReadonlyMapSnapshot(module.functions) },
      exports: { value: new ReadonlyMapSnapshot(module.exports) },
      requirements: { value: new ReadonlySetSnapshot(module.requirements) },
    });
    for (const exported of module.exports.values()) Object.freeze(exported);
    Object.freeze(module.constants);
    Object.freeze(module.constantUntyped);
    Object.freeze(module);
  }
}

type ArtifactSource = { artifact: Record<string, JsonValue>; hash: string };

function validateRequirements(
  modules: Map<string, PreparedModule>,
  artifacts: Map<string, ArtifactSource>,
): void {
  for (const [modulePath, source] of artifacts) {
    const owner = modules.get(modulePath)!;
    const requirements = array(source.artifact.requirements, `${modulePath}.requirements`);
    for (const [index, item] of requirements.entries()) {
      const path = `${modulePath}.requirements[${index}]`;
      const requirement = schemaObject(item, path, WIRE_FIELDS.Requirement);
      if (string(requirement.kind, `${path}.kind`) !== "source")
        fail("unknown requirement kind", `${path}.kind`, "link_error");
      const dependencyPath = string(requirement.module_path, `${path}.module_path`);
      if (!dependencyPath.trim() || owner.requirements.has(dependencyPath))
        fail("empty or duplicate module requirement", `${path}.module_path`, "link_error");
      const dependency = modules.get(dependencyPath);
      const dependencySource = artifacts.get(dependencyPath);
      if (!dependency || !dependencySource)
        fail("required module is missing", `${path}.module_path`, "missing_dependency");
      const requiredHash = string(requirement.hash, `${path}.hash`, true);
      if (requiredHash && !/^[0-9a-f]{64}$/.test(requiredHash))
        fail("requirement hash is invalid", `${path}.hash`, "link_error");
      if (requiredHash && requiredHash !== dependencySource.hash)
        fail("required module hash does not match", `${path}.hash`, "link_error");
      const names = new Set<string>();
      for (const [exportIndex, value] of array(requirement.exports, `${path}.exports`).entries()) {
        const name = string(value, `${path}.exports[${exportIndex}]`);
        if (!name.trim() || name !== name.trim() || names.has(name))
          fail("invalid or duplicate required export", `${path}.exports[${exportIndex}]`);
        names.add(name);
        if (!dependency.exports.has(name) && !dependency.registry.hasNamed(dependencyPath, name))
          fail("required export is missing", `${path}.exports[${exportIndex}]`, "missing_export");
      }
      (owner.requirements as Set<string>).add(dependencyPath);
    }
  }

  const state = new Map<string, number>();
  const visit = (modulePath: string): void => {
    const current = state.get(modulePath) ?? 0;
    if (current === 2) return;
    if (current === 1) fail("module dependency cycle", modulePath, "dependency_cycle");
    state.set(modulePath, 1);
    for (const dependency of modules.get(modulePath)!.requirements) visit(dependency);
    state.set(modulePath, 2);
  };
  for (const modulePath of modules.keys()) visit(modulePath);
}

const LOCAL_OUTPUT = 0x80000000;
const MAX_U32 = 0xffffffff;
const DIRECT_LOCAL_OUTPUTS = new Set([
  "zero",
  "unary",
  "binary",
  "load_index",
  "len",
  "cap",
  "convert",
  "get_path",
  "call_direct",
  "map_iter_next",
  "string_next_rune_index",
  "index_convert",
  "min_max",
]);

function isIntegerScalar(type: RuntimeType): boolean {
  const primitive = type.registry.underlying(type).ref;
  return (
    primitive.kind === Kind.Primitive &&
    ((primitive.primitive >= PrimitiveKind.PrimitiveInt &&
      primitive.primitive <= PrimitiveKind.PrimitiveInt64) ||
      (primitive.primitive >= PrimitiveKind.PrimitiveUint &&
        primitive.primitive <= PrimitiveKind.PrimitiveUintptr))
  );
}

function underlyingKind(type: RuntimeType): number {
  return type.registry.underlying(type).ref.kind;
}

function resolveAlias(type: RuntimeType): RuntimeType {
  const seen = new Set<string>();
  for (;;) {
    const node = type.registry.node(type);
    if (!node?.alias || !node.alias_target) return type;
    const key = `${type.module}\0${node.id}`;
    if (seen.has(key)) fail("recursive type alias", `${type.module}.type_table`);
    seen.add(key);
    type = type.registry.runtime(node.alias_target);
  }
}

function primitiveKind(type: RuntimeType): number | undefined {
  const ref = type.registry.underlying(type).ref;
  return ref.kind === Kind.Primitive ? ref.primitive : undefined;
}

function elementType(type: RuntimeType): RuntimeType | undefined {
  return type.registry.element(type);
}

function fieldsOfType(
  type: RuntimeType,
): readonly { name: string; type: RuntimeType; embedded: boolean }[] {
  const underlying = type.registry.underlying(type);
  const node = underlying.registry.node(underlying);
  return (
    underlying.dynamic?.fields ??
    (node?.fields ?? []).map((field) => ({
      name: field.name,
      type: underlying.registry.runtime(field.type),
      embedded: field.embedded,
    }))
  );
}

function functionTypeSignature(
  type: RuntimeType,
): { params: RuntimeType[]; results: RuntimeType[]; variadic: boolean } | undefined {
  const underlying = type.registry.underlying(type);
  if (underlying.ref.kind !== Kind.Function) return undefined;
  if (underlying.dynamic)
    return {
      params: [...(underlying.dynamic.params ?? [])],
      results: [...(underlying.dynamic.results ?? [])],
      variadic: underlying.dynamic.variadic ?? false,
    };
  const signature = underlying.registry.node(underlying)?.signature;
  return signature
    ? {
        params: signature.params.map((item) => underlying.registry.runtime(item.type)),
        results: signature.results.map((item) => underlying.registry.runtime(item)),
        variadic: signature.variadic,
      }
    : undefined;
}

function isNilAssignable(type: RuntimeType): boolean {
  return new Set<number>([
    Kind.Any,
    Kind.Interface,
    Kind.Pointer,
    Kind.Slice,
    Kind.Map,
    Kind.Function,
    Kind.Waitable,
  ]).has(underlyingKind(type));
}

function validatePreparedProgram(modules: Map<string, PreparedModule>): void {
  for (const module of modules.values()) {
    for (const [name, exported] of module.exports) {
      const path = `${module.path}.exports.${name}`;
      if (!name.trim()) fail("export name is empty", path);
      if (
        (exported.kind === "const" && !module.constantIndexes.has(exported.id)) ||
        (exported.kind === "global" && !module.globalIndexes.has(exported.id)) ||
        (exported.kind === "function" && !module.functions.has(exported.id)) ||
        (exported.kind === "type" &&
          !module.registry.hasNamed(module.path, name) &&
          (!exported.type || !module.registry.node(exported.type))) ||
        !["const", "global", "function", "type"].includes(exported.kind)
      )
        fail("export target is invalid", path, "link_error");
      if (exported.untyped && exported.kind !== "const")
        fail("only constant exports may be untyped", path);
    }
    for (const fn of module.functions.values()) validateFunction(fn, modules);
  }
}

function validateFunction(fn: PreparedFunction, modules: Map<string, PreparedModule>): void {
  for (const [pc, instruction] of fn.instructions.entries()) {
    const path = `${fn.module.path}.${fn.id}.instructions[${pc}]`;
    validateOperands(fn, instruction, path);
    validateDescriptor(fn, instruction, modules, path);
    validateInstructionTypes(fn, instruction, path);
    prepareInstructionMetadata(fn, instruction);
  }
  validateSlotInitialization(fn);
  for (const block of fn.blocks) {
    const end = block.first + block.count;
    for (let start = block.first; start < end; ) {
      if (!fn.instructions[start]!.fastScalar) {
        start++;
        continue;
      }
      let runEnd = start + 1;
      while (runEnd < end && fn.instructions[runEnd]!.fastScalar) runEnd++;
      for (let pc = start; pc < runEnd; pc++) (fn.fastRunEnd as number[])[pc] = runEnd;
      start = runEnd;
    }
  }
}

function prepareAddressPlan(descriptor: Record<string, unknown>): PreparedAddressPlan {
  const path = descriptorList(descriptor, "path", "address").map((raw) => {
    const segment = descriptorRecord(raw, "address path");
    const kind = segment.kind;
    if (kind === "field")
      return Object.freeze({ kind, field: segment.field as string }) satisfies PreparedAddressPath;
    if (kind === "index")
      return Object.freeze({ kind, local: segment.$index as number }) satisfies PreparedAddressPath;
    return Object.freeze({ kind: "indirect" }) satisfies PreparedAddressPath;
  });
  const plan = {
    kind: descriptor.kind as PreparedAddressPlan["kind"],
    index: descriptor.$index as number,
    path: Object.freeze(path),
    targetType: descriptor.$targetType as RuntimeType,
  };
  const module = descriptor.$module as PreparedModule | undefined;
  return Object.freeze(module ? { ...plan, module } : plan);
}

function prepareInstructionMetadata(fn: PreparedFunction, instruction: PreparedInstruction): void {
  const input0 = instruction.operands.inputs[0];
  const input1 = instruction.operands.inputs[1];
  const input2 = instruction.operands.inputs[2];
  const output0 = instruction.operands.outputs[0];
  const output1 = instruction.operands.outputs[1];
  if (input0 !== undefined) instruction.input0 = input0;
  if (input1 !== undefined) instruction.input1 = input1;
  if (input2 !== undefined) instruction.input2 = input2;
  if (output0 !== undefined) instruction.output0 = output0;
  if (output1 !== undefined) instruction.output1 = output1;
  const descriptor = instruction.descriptor;
  if (!descriptor) return;
  if (typeof descriptor.operator === "string" && instruction.name !== "min_max") {
    instruction.operatorCode =
      instruction.name === "unary"
        ? unaryOperatorCode(descriptor.operator)
        : operatorCode(descriptor.operator);
  }
  if (instruction.name === "update_compare_branch") {
    instruction.updateOperatorCode = operatorCode(String(descriptor.update));
    instruction.operatorCode = operatorCode(String(descriptor.comparison));
  }
  if (instruction.name === "min_max") instruction.minMax = descriptor.operator as "min" | "max";
  if (typeof descriptor.$target === "number") instruction.target = descriptor.$target;
  if (typeof descriptor.$local === "number") instruction.local = descriptor.$local;
  if (typeof descriptor.$upvalue === "number") instruction.upvalue = descriptor.$upvalue;
  if (typeof descriptor.$global === "number") instruction.global = descriptor.$global;
  if (typeof descriptor.$constant === "number") instruction.constant = descriptor.$constant;
  if (descriptor.$type && typeof descriptor.$type === "object")
    instruction.type = descriptor.$type as RuntimeType;
  if (typeof descriptor.method === "string") instruction.method = descriptor.method;
  if (typeof descriptor.id === "string") instruction.intrinsic = descriptor.id;
  if (typeof descriptor.field === "string") instruction.field = descriptor.field;
  if (typeof descriptor.module_path === "string")
    instruction.modulePath = descriptor.module_path || fn.module.path;
  if (typeof descriptor.function === "string") instruction.functionID = descriptor.function;
  if (typeof descriptor.export === "string") instruction.exportName = descriptor.export;
  if (descriptor.rebind === true) instruction.rebind = true;
  if (descriptor.negate === true) instruction.negate = true;
  if (descriptor.when === true) instruction.when = true;
  if (typeof descriptor.owner_depth === "number") instruction.ownerDepth = descriptor.owner_depth;
  if (descriptor.expand === true) instruction.expand = true;
  if (descriptor.has_capacity === true) instruction.hasCapacity = true;
  if (typeof descriptor.element_count === "number")
    instruction.elementCount = descriptor.element_count;
  if (typeof descriptor.entry_count === "number") instruction.entryCount = descriptor.entry_count;
  if (instruction.name === "make_struct")
    instruction.structFields = Object.freeze(
      descriptorList(descriptor, "fields", instruction.name).map(String),
    );
  if (
    (instruction.name === "map_iter_init" ||
      instruction.name === "map_iter_next" ||
      instruction.name === "map_iter_close") &&
    typeof descriptor.$local === "number"
  )
    instruction.iteratorLocal = descriptor.$local;
  if (instruction.name === "get_path") {
    const fields = descriptorList(descriptor, "fields", instruction.name);
    instruction.fieldPath = Object.freeze(fields.map((field) => Number(field)));
  }
  if (instruction.name === "make_closure") {
    instruction.captures = Object.freeze(
      descriptorList(descriptor, "captures", instruction.name).map((capture) =>
        prepareAddressPlan(descriptorRecord(capture, instruction.name)),
      ),
    );
  }
  if (instruction.name === "address_of") instruction.address = prepareAddressPlan(descriptor);
  if (instruction.name === "type_dispatch") {
    const defaultBinding = descriptor.$default_local as number | undefined;
    instruction.typeDispatch = Object.freeze({
      subject: descriptor.$subject as number,
      ...(defaultBinding === undefined ? {} : { defaultBinding }),
      defaultTarget: descriptor.$target as number,
      cases: Object.freeze(
        descriptorList(descriptor, "cases", instruction.name).map((raw) => {
          const candidate = descriptorRecord(raw, instruction.name);
          const type = candidate.$type as RuntimeType | undefined;
          const binding = candidate.$binding as number | undefined;
          return Object.freeze({
            ...(type === undefined ? {} : { type }),
            ...(binding === undefined ? {} : { binding }),
            target: candidate.$target as number,
            original: candidate.original === true,
          });
        }),
      ),
    });
  }
  if (instruction.name === "load_export") {
    instruction.exportModule = descriptor.$module as PreparedModule;
    instruction.exported = descriptor.$export as PreparedExport;
  }
  if (instruction.name === "select") {
    instruction.selectDefault = descriptor.default === true;
    instruction.selectIndex = descriptor.$index as number;
    instruction.selectCases = Object.freeze(
      descriptorList(descriptor, "cases", instruction.name).map((value) => {
        const selected = descriptorRecord(value, instruction.name);
        const result: PreparedSelectCase = { channel: selected.$channel as number };
        if (selected.$send !== undefined) result.send = selected.$send as number;
        if (selected.$value !== undefined) result.value = selected.$value as number;
        if (selected.$ok !== undefined) result.ok = selected.$ok as number;
        return result;
      }),
    );
  }
  const inputType = (index: number): RuntimeType | undefined => {
    const operand = instruction.operands.inputs[index];
    if (!operand) return undefined;
    if (operand[0] === 0) return fn.slotTypes[operand[1]];
    if (operand[0] === 1) return fn.module.constants[operand[1]]?.type;
    return fn.localTypes[operand[1]];
  };
  const scalar = inputType(0) ? primitiveKind(inputType(0)!) : undefined;
  const directPrimitive =
    scalar !== undefined &&
    scalar !== PrimitiveKind.PrimitiveString &&
    scalar !== PrimitiveKind.PrimitiveComplex64 &&
    scalar !== PrimitiveKind.PrimitiveComplex128;
  if (
    directPrimitive &&
    instruction.operatorCode !== OperatorCode.Complex &&
    (["unary", "binary", "compare_branch", "update_compare_branch", "jump_if"].includes(
      instruction.name,
    ) ||
      (instruction.name === "load_local" && !fn.cellLocals.has(instruction.local!)) ||
      (instruction.name === "store_local" &&
        !fn.cellLocals.has(instruction.local!) &&
        !instruction.rebind))
  )
    instruction.scalarPrimitive = scalar;
  else if (["const", "zero"].includes(instruction.name)) {
    const output = instruction.operands.outputs[0];
    const type =
      output === undefined
        ? undefined
        : output >= LOCAL_OUTPUT
          ? fn.localTypes[output - LOCAL_OUTPUT]
          : fn.slotTypes[output];
    const outputPrimitive = type ? primitiveKind(type) : undefined;
    if (
      outputPrimitive !== undefined &&
      outputPrimitive !== PrimitiveKind.PrimitiveString &&
      outputPrimitive !== PrimitiveKind.PrimitiveComplex64 &&
      outputPrimitive !== PrimitiveKind.PrimitiveComplex128
    )
      instruction.scalarPrimitive = outputPrimitive;
  }
  const riskyOperator =
    instruction.operatorCode === OperatorCode.Divide ||
    instruction.operatorCode === OperatorCode.Remainder ||
    instruction.operatorCode === OperatorCode.ShiftLeft ||
    instruction.operatorCode === OperatorCode.ShiftRight;
  instruction.fastScalar =
    instruction.logicalCost > 0 &&
    (instruction.name === "jump" ||
      (instruction.scalarPrimitive !== undefined &&
        !riskyOperator &&
        [
          "zero",
          "unary",
          "binary",
          "load_local",
          "store_local",
          "jump_if",
          "compare_branch",
          "update_compare_branch",
        ].includes(instruction.name)));
}

function validateOperands(
  fn: PreparedFunction,
  instruction: PreparedInstruction,
  path: string,
): void {
  const operands = instruction.operands;
  const arity = instructionArity(instruction, path);
  if (operands.inputs.length !== arity[0] || operands.outputs.length !== arity[1])
    fail("operand arity mismatch", path, "invalid_operand");
  for (const [index, input] of operands.inputs.entries()) {
    const inputPath = `${path}.inputs[${index}]`;
    const [kind, slot] = input;
    validateU32(slot, `${inputPath}[1]`);
    if (kind === 0 && slot < fn.slotTypes.length) continue;
    if (kind === 1 && slot < fn.module.constants.length) {
      if (fn.module.constantUntyped[slot])
        fail("untyped constant is not an execution operand", inputPath, "invalid_operand");
      continue;
    }
    if (kind === 2 && slot < fn.localTypes.length) {
      if (fn.cellLocals.has(slot))
        fail("direct local input requires private local storage", inputPath, "invalid_operand");
      continue;
    }
    fail("input kind or index is out of range", inputPath, "invalid_operand");
  }
  validateDistinctSlots(operands.outputs, `${path}.outputs`);
  for (const output of operands.outputs) {
    validateU32(output, `${path}.outputs`);
    if (output >= LOCAL_OUTPUT) {
      if (
        !DIRECT_LOCAL_OUTPUTS.has(instruction.name) ||
        (operands.outputs.length !== 1 &&
          instruction.name !== "call_direct" &&
          instruction.name !== "map_iter_next") ||
        output - LOCAL_OUTPUT >= fn.localTypes.length
      )
        fail("invalid direct local output", `${path}.outputs`, "invalid_operand");
      if (fn.cellLocals.has(output - LOCAL_OUTPUT))
        fail(
          "direct local output requires private local storage",
          `${path}.outputs`,
          "invalid_operand",
        );
    } else if (output >= fn.slotTypes.length) {
      fail("output slot is out of range", `${path}.outputs`, "invalid_operand");
    }
    if (operands.inputs.some(([kind, slot]) => kind === 0 && slot === output))
      fail("output aliases an input slot", `${path}.outputs`, "invalid_operand");
  }
  for (const [name, slots] of [
    ["release", operands.release],
    ["release_before", operands.releaseBefore],
  ] as const) {
    validateDistinctSlots(slots, `${path}.${name}`);
    for (const slot of slots) {
      validateU32(slot, `${path}.${name}`);
      if (slot >= fn.slotTypes.length)
        fail("release slot is out of range", `${path}.${name}`, "invalid_operand");
    }
  }
  if (instruction.name === "label" && (operands.release.length || operands.releaseBefore.length))
    fail("label cannot release slots", path, "invalid_operand");
}

function instructionArity(
  instruction: Pick<PreparedInstruction, "opcode" | "name" | "descriptor">,
  path: string,
): [number, number] {
  const fixed = OPCODE_ARITIES[instruction.opcode] as readonly [number, number] | null;
  if (fixed) return [fixed[0], fixed[1]];
  const descriptor = instruction.descriptor!;
  const count = (name: string, optional = false): number =>
    descriptorInteger(descriptor, name, path, optional);
  let inputs: number;
  let outputs: number;
  switch (instruction.name) {
    case "make_slice":
      inputs = descriptorBoolean(descriptor, "has_capacity", path) ? 2 : 1;
      outputs = 1;
      break;
    case "make_map":
      inputs = checkedCount(count("entry_count"), 2, path);
      if (descriptorBoolean(descriptor, "has_capacity", path)) inputs++;
      outputs = 1;
      break;
    case "append":
      inputs = checkedCount(count("count"), 1, path) + 1;
      outputs = 1;
      break;
    case "make_struct":
      inputs = descriptorList(descriptor, "fields", path).length;
      outputs = 1;
      break;
    case "make_sequence":
      inputs = count("element_count");
      outputs = 1;
      break;
    case "call_interface":
      inputs = count("arg_count") + 1;
      outputs = count("result_count", true);
      break;
    case "call_value":
      inputs = count("arg_count") + 1;
      outputs = count("result_count", true);
      break;
    case "call_direct":
      inputs = count("arg_count");
      outputs = count("result_count", true);
      break;
    case "tail_call_direct":
      inputs = count("arg_count");
      outputs = 0;
      break;
    case "spawn":
      inputs = count("arg_count") + 1;
      outputs = 0;
      break;
    case "return":
      inputs = count("result_count");
      outputs = 0;
      break;
    case "call_ffi":
    case "call_intrinsic":
      inputs = count("arg_count");
      outputs = count("result_count", instruction.name === "call_intrinsic");
      break;
    case "min_max":
      inputs = count("arg_count");
      outputs = 1;
      break;
    default:
      fail("opcode has no operand arity", path, "invalid_operand");
  }
  if (inputs < 0 || outputs < 0 || !Number.isSafeInteger(inputs) || !Number.isSafeInteger(outputs))
    fail("operand arity is invalid", path, "invalid_operand");
  return [inputs, outputs];
}

function validateDescriptor(
  fn: PreparedFunction,
  instruction: PreparedInstruction,
  modules: Map<string, PreparedModule>,
  path: string,
): void {
  const descriptor = instruction.descriptor ?? {};
  const local = (name: string, optional = false): number | undefined => {
    const id = descriptorString(descriptor, name, path, optional);
    if (optional && !id) return undefined;
    const index = fn.localIndexes.get(id);
    if (index === undefined) fail("unknown local", `${path}.${name}`, "link_error");
    return index;
  };
  const label = (value: unknown, labelPath: string): number => {
    if (typeof value !== "string" || !value.trim() || !fn.labels.has(value))
      fail("unknown jump label", labelPath, "invalid_jump");
    return fn.labels.get(value)!;
  };
  const targetModule = (name = "module_path", allowCurrent = true): PreparedModule => {
    const modulePath = descriptorString(descriptor, name, path, allowCurrent);
    const resolvedPath = modulePath || fn.module.path;
    if (resolvedPath !== fn.module.path && !fn.module.requirements.has(resolvedPath))
      fail("module is not a declared dependency", `${path}.${name}`, "link_error");
    const target = modules.get(resolvedPath);
    if (!target) fail("module does not exist", `${path}.${name}`, "link_error");
    return target;
  };
  const operandType = (operand: readonly [number, number]): RuntimeType => {
    const [kind, index] = operand;
    if (kind === 0) return fn.slotTypes[index]!;
    if (kind === 1) return fn.module.constants[index]!.type;
    if (kind === 2) return fn.localTypes[index]!;
    fail("unknown operand kind", path, "invalid_operand");
  };
  const isLengthBearing = (type: RuntimeType): boolean => {
    let underlying = type.registry.underlying(type);
    if (underlying.ref.kind === Kind.Pointer) {
      const element = underlying.registry.element(underlying);
      if (!element) return false;
      underlying = underlying.registry.underlying(element);
    }
    if (
      underlying.ref.kind === Kind.Primitive &&
      underlying.ref.primitive !== PrimitiveKind.PrimitiveString
    )
      return false;
    return new Set<number>([Kind.Primitive, Kind.Slice, Kind.Array, Kind.Map, Kind.Waitable]).has(
      underlying.ref.kind,
    );
  };
  const isByteIndexable = (type: RuntimeType): boolean => {
    let underlying = type.registry.underlying(type);
    if (underlying.ref.kind === Kind.Pointer) {
      const element = underlying.registry.element(underlying);
      if (!element) return false;
      underlying = underlying.registry.underlying(element);
      if (underlying.ref.kind !== Kind.Array) return false;
    }
    if (underlying.ref.kind === Kind.Primitive)
      return underlying.ref.primitive === PrimitiveKind.PrimitiveString;
    if (underlying.ref.kind !== Kind.Slice && underlying.ref.kind !== Kind.Array) return false;
    const element = underlying.registry.element(underlying);
    if (!element) return false;
    const primitive = underlying.registry.underlying(element).ref;
    return (
      primitive.kind === Kind.Primitive && primitive.primitive === PrimitiveKind.PrimitiveUint8
    );
  };
  const outputType = (output: number): RuntimeType =>
    output >= LOCAL_OUTPUT ? fn.localTypes[output - LOCAL_OUTPUT]! : fn.slotTypes[output]!;
  const validateAddress = (
    value: unknown,
    addressPath: string,
    allowExport: boolean,
  ): RuntimeType => {
    const address = descriptorRecord(value, addressPath);
    assertAllowedFields(address, WIRE_FIELDS.AddressPayload, addressPath);
    const kind = descriptorString(address, "kind", addressPath);
    let current: RuntimeType;
    if (kind === "local") {
      const index = localFrom(address, "local", fn.localIndexes, addressPath)!;
      address.$index = index;
      current = fn.localTypes[index]!;
    } else if (kind === "upvalue") {
      const index = localFrom(address, "upvalue", fn.upvalueIndexes, addressPath)!;
      address.$index = index;
      current = fn.upvalueTypes[index]!;
    } else if (kind === "global") {
      const index = localFrom(address, "global", fn.module.globalIndexes, addressPath)!;
      address.$index = index;
      current = fn.module.globals[index]!.read().type;
    } else if (kind === "export" && allowExport) {
      const modulePath = descriptorString(address, "module_path", addressPath);
      if (!fn.module.requirements.has(modulePath))
        fail("export address module is not a dependency", addressPath, "link_error");
      const target = modules.get(modulePath);
      const exported = target?.exports.get(descriptorString(address, "export", addressPath));
      if (exported?.kind !== "global")
        fail("export address does not resolve to a global", addressPath, "link_error");
      const index = target!.globalIndexes.get(exported.id);
      if (index === undefined) fail("export global does not exist", addressPath, "link_error");
      address.$module = target;
      address.$index = index;
      current = target!.globals[index]!.read().type;
    } else fail("invalid address kind", `${addressPath}.kind`, "invalid_operand");
    for (const [index, raw] of descriptorList(address, "path", addressPath).entries()) {
      const segmentPath = `${addressPath}.path[${index}]`;
      const segment = descriptorRecord(raw, segmentPath);
      assertAllowedFields(segment, WIRE_FIELDS.AddressPathSegment, segmentPath);
      const segmentKind = descriptorString(segment, "kind", segmentPath);
      if (segmentKind === "field") {
        const name = descriptorString(segment, "field", segmentPath);
        if (underlyingKind(current) === Kind.Pointer) {
          const pointee = elementType(current);
          if (!pointee) fail("address field pointer has no target", segmentPath, "invalid_operand");
          current = pointee;
        }
        const findField = (type: RuntimeType, active: Set<string>): RuntimeType | undefined => {
          const key = `${type.module}\0${type.registry.key(type)}`;
          if (active.has(key)) return undefined;
          active.add(key);
          const fields = fieldsOfType(type);
          const direct = fields.find((candidate) => candidate.name === name);
          if (direct) return direct.type;
          for (const candidate of fields) {
            if (!candidate.embedded) continue;
            let embedded = candidate.type;
            if (underlyingKind(embedded) === Kind.Pointer)
              embedded = elementType(embedded) ?? embedded;
            const found = findField(embedded, active);
            if (found) return found;
          }
          return undefined;
        };
        const field = findField(current, new Set());
        if (!field) fail("address field does not exist", segmentPath, "invalid_operand");
        current = field;
      } else if (segmentKind === "index") {
        const localIndex = localFrom(segment, "local", fn.localIndexes, segmentPath)!;
        segment.$index = localIndex;
        if (!isIntegerScalar(fn.localTypes[localIndex]!))
          fail("address index local must be an integer", segmentPath, "invalid_operand");
        if (underlyingKind(current) === Kind.Pointer) {
          const pointee = elementType(current);
          if (!pointee || underlyingKind(pointee) !== Kind.Array)
            fail("address index pointer does not target an array", segmentPath, "invalid_operand");
          current = pointee;
        }
        const element = elementType(current);
        const currentKind = underlyingKind(current);
        if (!element || (currentKind !== Kind.Array && currentKind !== Kind.Slice))
          fail("address index targets a non-sequence", segmentPath, "invalid_operand");
        current = element;
      } else if (segmentKind === "indirect") {
        const element = elementType(current);
        if (!element || underlyingKind(current) !== Kind.Pointer)
          fail("address indirect segment targets a non-pointer", segmentPath, "invalid_operand");
        current = element;
      } else fail("invalid address path segment", `${segmentPath}.kind`, "invalid_operand");
    }
    current = resolveAlias(current);
    address.$targetType = current;
    return current;
  };

  switch (instruction.name) {
    case "const": {
      const id = descriptorString(descriptor, "constant", path);
      const index = fn.module.constantIndexes.get(id);
      if (index === undefined || fn.module.constantUntyped[index])
        fail("unknown or untyped constant", `${path}.constant`, "invalid_operand");
      descriptor.$constant = index;
      break;
    }
    case "load_local":
    case "store_local":
    case "map_iter_init":
    case "map_iter_next":
    case "map_iter_close":
      descriptor.$local = local("local");
      if (descriptor.rebind === true && instruction.name !== "store_local")
        fail("rebind is only valid for store_local", `${path}.rebind`, "invalid_operand");
      break;
    case "load_upvalue":
    case "store_upvalue":
      descriptor.$upvalue = localFrom(descriptor, "upvalue", fn.upvalueIndexes, path);
      break;
    case "load_global":
    case "store_global":
      descriptor.$global = localFrom(descriptor, "global", fn.module.globalIndexes, path);
      break;
    case "label": {
      const value = descriptorString(descriptor, "label", path);
      if (!value.trim()) fail("label is empty", `${path}.label`, "invalid_label");
      break;
    }
    case "jump":
    case "jump_if":
      descriptor.$target = label(descriptor.label, `${path}.label`);
      break;
    case "compare_branch":
      if (
        !["==", "!=", "<", "<=", ">", ">="].includes(descriptorString(descriptor, "operator", path))
      )
        fail("invalid comparison operator", `${path}.operator`, "invalid_operand");
      descriptor.$type = validateTypeDescriptor(fn.module, descriptor.type, `${path}.type`);
      descriptor.$target = label(descriptor.label, `${path}.label`);
      break;
    case "min_max": {
      const type = validateTypeDescriptor(fn.module, descriptor.type, `${path}.type`);
      if (!type) fail("min/max type is unavailable", `${path}.type`, "invalid_operand");
      const underlying = fn.module.registry.underlying(type).ref;
      const ordered =
        underlying.kind === Kind.Primitive &&
        (underlying.primitive === PrimitiveKind.PrimitiveString ||
          (underlying.primitive >= PrimitiveKind.PrimitiveInt &&
            underlying.primitive <= PrimitiveKind.PrimitiveFloat64));
      if (!ordered) fail("min/max type is not ordered", `${path}.type`, "invalid_operand");
      const operator = descriptorString(descriptor, "operator", path);
      if (operator !== "min" && operator !== "max")
        fail("invalid min/max operator", `${path}.operator`, "invalid_operand");
      const argCount = descriptorInteger(descriptor, "arg_count", path);
      if (argCount < 1 || argCount !== instruction.operands.inputs.length)
        fail("min/max argument count is invalid", `${path}.arg_count`, "invalid_operand");
      for (const operand of instruction.operands.inputs)
        if (!fn.module.registry.identical(operandType(operand), type))
          fail("min/max operand type differs from descriptor", path, "invalid_operand");
      if (!fn.module.registry.identical(outputType(instruction.operands.outputs[0]!), type))
        fail("min/max output type differs from descriptor", path, "invalid_operand");
      descriptor.$type = type;
      break;
    }
    case "len_compare_branch": {
      const type = validateTypeDescriptor(fn.module, descriptor.type, `${path}.type`);
      if (
        !type ||
        !fn.module.registry.identical(
          type,
          primitive(fn.module.registry, PrimitiveKind.PrimitiveInt),
        )
      )
        fail("length comparison descriptor must use Int", `${path}.type`, "invalid_operand");
      if (!isLengthBearing(operandType(instruction.operands.inputs[0]!)))
        fail("length comparison requires a length-bearing object", path, "invalid_operand");
      const indexType = fn.module.registry.underlying(operandType(instruction.operands.inputs[1]!));
      if (
        indexType.ref.kind !== Kind.Primitive ||
        indexType.ref.primitive < PrimitiveKind.PrimitiveInt ||
        indexType.ref.primitive > PrimitiveKind.PrimitiveUintptr
      )
        fail("length comparison operand must be an integer", path, "invalid_operand");
      descriptor.$type = type;
      if (
        !["==", "!=", "<", "<=", ">", ">="].includes(descriptorString(descriptor, "operator", path))
      )
        fail("invalid comparison operator", `${path}.operator`, "invalid_operand");
      descriptor.$target = label(descriptor.label, `${path}.label`);
      break;
    }
    case "update_compare_branch": {
      const type = validateTypeDescriptor(fn.module, descriptor.type, `${path}.type`);
      if (!type) fail("update comparison type is unavailable", `${path}.type`, "invalid_operand");
      if (descriptorString(descriptor, "update", path) !== "+" && descriptor.update !== "-")
        fail("update comparison requires + or -", `${path}.update`, "invalid_operand");
      if (
        !["==", "!=", "<", "<=", ">", ">="].includes(
          descriptorString(descriptor, "comparison", path),
        )
      )
        fail("invalid update comparison operator", `${path}.comparison`, "invalid_operand");
      descriptor.$type = type;
      descriptor.$target = label(descriptor.label, `${path}.label`);
      break;
    }
    case "type_dispatch":
      descriptor.$subject = local("subject");
      descriptor.$default_local = local("default_local", true);
      descriptor.$target = label(descriptor.default, `${path}.default`);
      for (const [index, value] of descriptorList(descriptor, "cases", path).entries()) {
        const casePath = `${path}.cases[${index}]`;
        const candidate = descriptorRecord(value, casePath);
        assertAllowedFields(candidate, WIRE_FIELDS.TypeCase, casePath);
        candidate.$binding = localFrom(candidate, "binding", fn.localIndexes, casePath, true);
        candidate.$target = label(candidate.label, `${casePath}.label`);
        if (candidate.type !== undefined)
          candidate.$type = validateTypeDescriptor(
            fn.module,
            candidate.type,
            `${casePath}.type`,
            candidate.original === true,
          );
      }
      break;
    case "select":
      descriptor.$index = local("index");
      for (const [index, value] of descriptorList(descriptor, "cases", path).entries()) {
        const casePath = `${path}.cases[${index}]`;
        const selected = descriptorRecord(value, casePath);
        assertAllowedFields(selected, WIRE_FIELDS.SelectCase, casePath);
        selected.$channel = localFrom(selected, "channel", fn.localIndexes, casePath);
        const send = localFrom(selected, "send", fn.localIndexes, casePath, true);
        selected.$send = send;
        if (send === undefined) {
          selected.$value = localFrom(selected, "value", fn.localIndexes, casePath);
          selected.$ok = localFrom(selected, "ok", fn.localIndexes, casePath);
        } else if (selected.value !== undefined || selected.ok !== undefined) {
          fail("send select case cannot contain receive locals", casePath, "invalid_operand");
        }
      }
      break;
    case "return":
      if (descriptorInteger(descriptor, "result_count", path) !== fn.signature.results.length)
        fail("return result count does not match function signature", path, "invalid_return");
      break;
    case "defer_push":
      if (descriptorInteger(descriptor, "owner_depth", path, true) < 0)
        fail("defer owner depth is negative", path, "invalid_operand");
      break;
    case "call_direct":
    case "tail_call_direct": {
      const target = targetModule();
      const callee = target.functions.get(descriptorString(descriptor, "function", path));
      if (!callee) fail("call target does not exist", path, "link_error");
      instruction.callTarget = callee;
      const results = descriptorInteger(descriptor, "result_count", path, true);
      if (
        descriptorInteger(descriptor, "arg_count", path) !== callee.signature.params.length ||
        results !== callee.signature.results.length ||
        (instruction.name === "tail_call_direct" && results !== fn.signature.results.length)
      )
        fail("call contract does not match target signature", path, "invalid_call");
      if (instruction.name === "call_direct") {
        for (const [index, output] of instruction.operands.outputs.entries()) {
          if (output < LOCAL_OUTPUT) continue;
          const localIndex = output - LOCAL_OUTPUT;
          const resultType = target.registry.runtime(callee.signature.results[index]!);
          if (!target.registry.identical(fn.localTypes[localIndex]!, resultType))
            fail("direct local call output type does not match result", path, "invalid_call");
        }
      }
      break;
    }
    case "call_value":
      nonNegativeCounts(descriptor, path);
      break;
    case "spawn": {
      nonNegativeCounts(descriptor, path);
      if (descriptorInteger(descriptor, "result_count", path, true) !== 0)
        fail("spawn cannot produce results", path, "invalid_call");
      if (descriptor.function !== undefined) {
        const target = targetModule();
        const callee = target.functions.get(descriptorString(descriptor, "function", path));
        if (!callee) fail("spawn target does not exist", path, "link_error");
        instruction.callTarget = callee;
        if (descriptorInteger(descriptor, "arg_count", path) !== callee.signature.params.length)
          fail("spawn contract does not match target signature", path, "invalid_call");
      }
      break;
    }
    case "call_interface":
      nonNegativeCounts(descriptor, path);
      descriptorString(descriptor, "method", path);
      descriptor.$type = validateTypeDescriptor(
        fn.module,
        descriptor.interface_type,
        `${path}.interface_type`,
      );
      break;
    case "make_closure": {
      const target = targetModule();
      const callee = target.functions.get(descriptorString(descriptor, "function", path));
      if (!callee) fail("closure target does not exist", path, "link_error");
      instruction.callTarget = callee;
      const captures = descriptorList(descriptor, "captures", path);
      if (captures.length !== callee.upvalueTypes.length)
        fail("closure capture count does not match target", path, "invalid_call");
      captures.forEach((capture, index) => {
        const type = validateAddress(capture, `${path}.captures[${index}]`, false);
        if (!callee.upvalueTypes[index]!.registry.identical(type, callee.upvalueTypes[index]!))
          fail("closure capture type does not match target", path, "invalid_call");
      });
      break;
    }
    case "make_sequence":
      descriptor.$type = validateTypeDescriptor(fn.module, descriptor.type, `${path}.type`);
      nonNegative(descriptorInteger(descriptor, "element_count", path), path);
      break;
    case "make_map":
      descriptor.$type = validateTypeDescriptor(fn.module, descriptor.type, `${path}.type`);
      nonNegative(descriptorInteger(descriptor, "entry_count", path), path);
      break;
    case "make_struct": {
      descriptor.$type = validateTypeDescriptor(fn.module, descriptor.type, `${path}.type`);
      const fields = descriptorList(descriptor, "fields", path).map((field, index) => {
        if (typeof field !== "string" || !field.trim())
          fail("invalid struct field", `${path}.fields[${index}]`);
        return field;
      });
      if (new Set(fields).size !== fields.length) fail("duplicate struct field", `${path}.fields`);
      break;
    }
    case "make_slice":
    case "make_waitable":
    case "zero":
    case "type_assert":
    case "type_assert_ok":
    case "convert":
      descriptor.$type = validateTypeDescriptor(fn.module, descriptor.type, `${path}.type`);
      break;
    case "append": {
      const count = descriptorInteger(descriptor, "count", path);
      nonNegative(count, path);
      if (descriptorBoolean(descriptor, "expand", path) && count !== 1)
        fail("expanded append requires one value", path, "invalid_operand");
      break;
    }
    case "address_of":
      validateAddress(descriptor, path, true);
      break;
    case "init_module":
      targetModule("module_path", false);
      break;
    case "load_export": {
      const target = targetModule("module_path", false);
      const exported = target.exports.get(descriptorString(descriptor, "export", path));
      if (!exported) fail("export does not exist", path, "link_error");
      descriptor.$module = target;
      descriptor.$export = exported;
      break;
    }
    case "call_ffi":
      if (
        descriptorInteger(descriptor, "arg_count", path) !== 2 ||
        descriptorInteger(descriptor, "result_count", path) !== 3
      )
        fail("FFI call contract is invalid", path, "invalid_call");
      break;
    case "call_intrinsic": {
      const id = descriptorString(descriptor, "id", path);
      const intrinsic = (
        INTRINSICS as ReadonlyMap<
          string,
          { readonly args: number; readonly results: number; readonly effect: number }
        >
      ).get(id);
      if (
        !intrinsic ||
        descriptorInteger(descriptor, "arg_count", path) !== intrinsic.args ||
        descriptorInteger(descriptor, "result_count", path, true) !== intrinsic.results
      )
        fail("intrinsic contract is invalid", path, "invalid_call");
      break;
    }
    case "get_path": {
      descriptor.$type = validateTypeDescriptor(fn.module, descriptor.type, `${path}.type`);
      const fields = descriptorList(descriptor, "fields", path);
      if (!fields.length || fields.length > 16)
        fail("field path requires 1 to 16 fields", path, "invalid_operand");
      fields.forEach((field, index) => {
        if (!Number.isSafeInteger(field) || (field as number) < 0)
          fail("field index is invalid", `${path}.fields[${index}]`, "invalid_operand");
      });
      break;
    }
    case "index_convert": {
      const type = validateTypeDescriptor(fn.module, descriptor.type, `${path}.type`);
      if (!type || !isIntegerScalar(type))
        fail("index conversion target must be an integer", `${path}.type`, "invalid_operand");
      if (!isByteIndexable(operandType(instruction.operands.inputs[0]!)))
        fail("index conversion requires string or byte sequence", path, "invalid_operand");
      if (!isIntegerScalar(operandType(instruction.operands.inputs[1]!)))
        fail("index conversion index must be an integer", path, "invalid_operand");
      if (!fn.module.registry.identical(outputType(instruction.operands.outputs[0]!), type))
        fail("index conversion output type differs from descriptor", path, "invalid_operand");
      descriptor.$type = type;
      break;
    }
  }
}

function validateInstructionTypes(
  fn: PreparedFunction,
  instruction: PreparedInstruction,
  path: string,
): void {
  const descriptor = instruction.descriptor ?? {};
  const input = (index: number): RuntimeType => {
    const operand = instruction.operands.inputs[index];
    if (!operand) fail("instruction input is missing", path, "invalid_operand");
    if (operand[0] === 0) return fn.slotTypes[operand[1]]!;
    if (operand[0] === 1) return fn.module.constants[operand[1]]!.type;
    return fn.localTypes[operand[1]]!;
  };
  const output = (index: number): RuntimeType => {
    const target = instruction.operands.outputs[index];
    if (target === undefined) fail("instruction output is missing", path, "invalid_operand");
    return target >= LOCAL_OUTPUT ? fn.localTypes[target - LOCAL_OUTPUT]! : fn.slotTypes[target]!;
  };
  const identical = (left: RuntimeType, right: RuntimeType): boolean =>
    left.registry.identical(left, right);
  const methodsOf = (type: RuntimeType) => {
    const direct = type.registry.node(type);
    if (direct?.methods.length) return direct.methods;
    const underlying = type.registry.underlying(type);
    if (underlying.ref.kind === Kind.Pointer) {
      const element = elementType(underlying);
      const node = element && element.registry.node(element);
      if (node?.methods.length) return node.methods;
    }
    return underlying.registry.node(underlying)?.methods ?? [];
  };
  const implementsInterface = (source: RuntimeType, target: RuntimeType): boolean => {
    const required = methodsOf(target);
    if (!required.length) return true;
    const available = methodsOf(source);
    return required.every((method) => {
      const candidate = available.find((item) => item.name === method.name);
      if (!candidate) return false;
      const left = candidate.signature;
      const right = method.signature;
      if (
        left.variadic !== right.variadic ||
        left.params.length !== right.params.length ||
        left.results.length !== right.results.length
      )
        return false;
      return (
        left.params.every((item, index) =>
          identical(
            source.registry.runtime(item.type),
            target.registry.runtime(right.params[index]!.type),
          ),
        ) &&
        left.results.every((item, index) =>
          identical(source.registry.runtime(item), target.registry.runtime(right.results[index]!)),
        )
      );
    });
  };
  const assignable = (source: RuntimeType, target: RuntimeType, index: number): boolean => {
    if (identical(source, target)) return true;
    if (underlyingKind(source) === Kind.Void) return isNilAssignable(target);
    if (underlyingKind(target) === Kind.Any) return true;
    if (underlyingKind(target) === Kind.Interface) return implementsInterface(source, target);
    const sourceUnderlying = source.registry.underlying(source);
    const targetUnderlying = target.registry.underlying(target);
    if (sourceUnderlying.registry.identical(sourceUnderlying, targetUnderlying)) return true;
    if (
      sourceUnderlying.ref.kind === Kind.Waitable &&
      targetUnderlying.ref.kind === Kind.Waitable
    ) {
      const sourceElement = elementType(sourceUnderlying);
      const targetElement = elementType(targetUnderlying);
      const sourceDirection = channelDirection(sourceUnderlying);
      const targetDirection = channelDirection(targetUnderlying);
      if (
        sourceElement &&
        targetElement &&
        identical(sourceElement, targetElement) &&
        (sourceDirection === targetDirection || sourceDirection === 1)
      )
        return true;
    }
    if (primitiveKind(source) !== undefined && primitiveKind(source) === primitiveKind(target))
      return true;
    const operand = instruction.operands.inputs[index];
    if (operand?.[0] !== 1 || !fn.module.constantUntyped[operand[1]]) return false;
    const sourcePrimitive = primitiveKind(source);
    const targetPrimitive = primitiveKind(target);
    if (sourcePrimitive === undefined || targetPrimitive === undefined) return false;
    if (sourcePrimitive === PrimitiveKind.PrimitiveBool)
      return targetPrimitive === PrimitiveKind.PrimitiveBool;
    if (sourcePrimitive === PrimitiveKind.PrimitiveString)
      return targetPrimitive === PrimitiveKind.PrimitiveString;
    return (
      sourcePrimitive >= PrimitiveKind.PrimitiveInt &&
      sourcePrimitive <= PrimitiveKind.PrimitiveComplex128 &&
      targetPrimitive >= PrimitiveKind.PrimitiveInt &&
      targetPrimitive <= PrimitiveKind.PrimitiveComplex128
    );
  };
  const expectInputs = (types: readonly RuntimeType[]): void => {
    if (types.length !== instruction.operands.inputs.length)
      fail("instruction input signature mismatch", path, "invalid_operand");
    types.forEach((type, index) => {
      if (!assignable(input(index), type, index))
        fail(`instruction input ${index} type mismatch`, path, "invalid_operand");
    });
  };
  const expectOutputs = (types: readonly RuntimeType[]): void => {
    if (types.length !== instruction.operands.outputs.length)
      fail("instruction output signature mismatch", path, "invalid_operand");
    types.forEach((type, index) => {
      const actual = output(index);
      const actualUnderlying = actual.registry.underlying(actual);
      const expectedUnderlying = type.registry.underlying(type);
      const actualPrimitive = primitiveKind(actual);
      const expectedPrimitive = primitiveKind(type);
      if (
        !identical(actual, type) &&
        !actualUnderlying.registry.identical(actualUnderlying, expectedUnderlying) &&
        (actualPrimitive === undefined || actualPrimitive !== expectedPrimitive)
      )
        fail(`instruction output ${index} type mismatch`, path, "invalid_operand");
    });
  };
  const boolType = primitive(fn.module.registry, PrimitiveKind.PrimitiveBool);
  const intType = primitive(fn.module.registry, PrimitiveKind.PrimitiveInt);
  const declaredType = descriptor.$type as RuntimeType | undefined;
  const integerInput = (index: number): void => {
    if (!isIntegerScalar(input(index)))
      fail(`instruction input ${index} must be an integer`, path, "invalid_operand");
  };
  const mapParts = (type: RuntimeType): [RuntimeType, RuntimeType] | undefined => {
    const underlying = type.registry.underlying(type);
    if (underlying.ref.kind !== Kind.Map) return undefined;
    const node = underlying.registry.node(underlying);
    const key =
      underlying.dynamic?.key ?? (node?.key ? underlying.registry.runtime(node.key) : undefined);
    const value =
      underlying.dynamic?.elem ?? (node?.elem ? underlying.registry.runtime(node.elem) : undefined);
    return key && value ? [key, value] : undefined;
  };
  const field = (type: RuntimeType, name: string): RuntimeType | undefined => {
    let current = type.registry.underlying(type);
    if (current.ref.kind === Kind.Pointer) {
      const element = elementType(current);
      if (!element) return undefined;
      current = element.registry.underlying(element);
    }
    return fieldsOfType(current).find((candidate) => candidate.name === name)?.type;
  };
  const sequenceElement = (type: RuntimeType): RuntimeType | undefined => {
    let current = type.registry.underlying(type);
    if (current.ref.kind === Kind.Pointer) {
      const pointee = elementType(current);
      if (!pointee || underlyingKind(pointee) !== Kind.Array) return undefined;
      current = pointee.registry.underlying(pointee);
    }
    return elementType(current);
  };
  const channelDirection = (type: RuntimeType): number => {
    const underlying = type.registry.underlying(type);
    return underlying.dynamic?.direction ?? underlying.registry.node(underlying)?.direction ?? 0;
  };

  switch (instruction.name) {
    case "const":
      expectOutputs([fn.module.constants[descriptor.$constant as number]!.type]);
      return;
    case "zero":
      expectOutputs([declaredType!]);
      return;
    case "load_local":
      expectOutputs([fn.localTypes[descriptor.$local as number]!]);
      return;
    case "store_local":
      expectInputs([fn.localTypes[descriptor.$local as number]!]);
      return;
    case "load_upvalue":
      expectOutputs([fn.upvalueTypes[descriptor.$upvalue as number]!]);
      return;
    case "store_upvalue":
      expectInputs([fn.upvalueTypes[descriptor.$upvalue as number]!]);
      return;
    case "load_global":
      expectOutputs([fn.module.globals[descriptor.$global as number]!.read().type]);
      return;
    case "store_global":
      expectInputs([fn.module.globals[descriptor.$global as number]!.read().type]);
      return;
    case "jump_if":
      expectInputs([boolType]);
      return;
    case "recover":
      if (underlyingKind(output(0)) !== Kind.Any && underlyingKind(output(0)) !== Kind.Interface)
        fail("recover result must be an interface", path, "invalid_operand");
      return;
    case "defer_push":
      if (!functionTypeSignature(input(0)))
        fail("deferred operand is not a function", path, "invalid_operand");
      return;
    case "return":
      expectInputs(fn.signature.results.map((type) => fn.module.registry.runtime(type)));
      return;
    case "unary": {
      const source = input(0);
      const primitiveValue = primitiveKind(source);
      const operator = descriptorString(descriptor, "operator", path);
      let result = source;
      const numeric =
        primitiveValue !== undefined &&
        primitiveValue >= PrimitiveKind.PrimitiveInt &&
        primitiveValue <= PrimitiveKind.PrimitiveComplex128;
      const integer =
        primitiveValue !== undefined &&
        primitiveValue >= PrimitiveKind.PrimitiveInt &&
        primitiveValue <= PrimitiveKind.PrimitiveUintptr;
      const valid =
        ((operator === "+" || operator === "-") && numeric) ||
        (operator === "^" && integer) ||
        (operator === "!" && primitiveValue === PrimitiveKind.PrimitiveBool) ||
        ((operator === "real" || operator === "imag") &&
          (primitiveValue === PrimitiveKind.PrimitiveComplex64 ||
            primitiveValue === PrimitiveKind.PrimitiveComplex128));
      if (!valid) fail("operand type does not support operator", path, "invalid_operand");
      if (operator === "!") result = boolType;
      else if (operator === "real" || operator === "imag")
        result = primitive(
          fn.module.registry,
          primitiveValue === PrimitiveKind.PrimitiveComplex64
            ? PrimitiveKind.PrimitiveFloat32
            : PrimitiveKind.PrimitiveFloat64,
        );
      expectOutputs([result]);
      return;
    }
    case "update_compare_branch": {
      if (instruction.operands.inputs[0]?.[0] !== 2)
        fail("update comparison target must be a direct local", path, "invalid_operand");
      const type = declaredType!;
      if (!identical(input(0), type) || !identical(input(1), type) || !identical(input(2), type))
        fail("update comparison operands differ from descriptor type", path, "invalid_operand");
      const primitiveValue = primitiveKind(type);
      if (
        primitiveValue === undefined ||
        primitiveValue < PrimitiveKind.PrimitiveInt ||
        primitiveValue > PrimitiveKind.PrimitiveFloat64
      )
        fail("update comparison requires an ordered numeric scalar", path, "invalid_operand");
      return;
    }
    case "binary":
    case "compare_branch": {
      const left = input(0);
      const right = input(1);
      const leftPrimitive = primitiveKind(left);
      const rightPrimitive = primitiveKind(right);
      const operator = descriptorString(descriptor, "operator", path);
      const matching = identical(left, right);
      const numeric =
        leftPrimitive !== undefined &&
        leftPrimitive >= PrimitiveKind.PrimitiveInt &&
        leftPrimitive <= PrimitiveKind.PrimitiveComplex128;
      const integer =
        leftPrimitive !== undefined &&
        leftPrimitive >= PrimitiveKind.PrimitiveInt &&
        leftPrimitive <= PrimitiveKind.PrimitiveUintptr;
      let result = left;
      let valid = false;
      if (operator === "==" || operator === "!=") {
        valid =
          matching ||
          (underlyingKind(left) === Kind.Void && isNilAssignable(right)) ||
          (underlyingKind(right) === Kind.Void && isNilAssignable(left));
        result = boolType;
      } else if (["<", "<=", ">", ">="].includes(operator)) {
        valid =
          matching &&
          (leftPrimitive === PrimitiveKind.PrimitiveString ||
            (leftPrimitive !== undefined &&
              leftPrimitive >= PrimitiveKind.PrimitiveInt &&
              leftPrimitive <= PrimitiveKind.PrimitiveFloat64));
        result = boolType;
      } else if (operator === "<<" || operator === ">>") {
        valid = integer && isIntegerScalar(right);
      } else if (operator === "&&" || operator === "||") {
        valid = matching && leftPrimitive === PrimitiveKind.PrimitiveBool;
      } else if (operator === "complex") {
        valid =
          matching &&
          (leftPrimitive === PrimitiveKind.PrimitiveFloat32 ||
            leftPrimitive === PrimitiveKind.PrimitiveFloat64);
        result = primitive(
          fn.module.registry,
          leftPrimitive === PrimitiveKind.PrimitiveFloat32
            ? PrimitiveKind.PrimitiveComplex64
            : PrimitiveKind.PrimitiveComplex128,
        );
      } else if (operator === "+") {
        valid = matching && (numeric || leftPrimitive === PrimitiveKind.PrimitiveString);
      } else if (["-", "*", "/"].includes(operator)) valid = matching && numeric;
      else if (["%", "&", "|", "^", "&^"].includes(operator)) valid = matching && integer;
      if (
        !valid ||
        (operator !== "<<" && operator !== ">>" && matching && leftPrimitive !== rightPrimitive)
      )
        fail("operand types do not support operator", path, "invalid_operand");
      if (instruction.name === "binary") expectOutputs([result]);
      else if (declaredType && !identical(left, declaredType))
        fail("comparison type differs from descriptor", path, "invalid_operand");
      return;
    }
    case "call_direct":
    case "tail_call_direct": {
      const callee = instruction.callTarget!;
      const params = callee.signature.params.map((item) =>
        callee.module.registry.runtime(item.type),
      );
      expectInputs(params);
      if (instruction.name === "call_direct")
        expectOutputs(callee.signature.results.map((item) => callee.module.registry.runtime(item)));
      return;
    }
    case "call_value":
    case "spawn": {
      if (instruction.name === "spawn" && instruction.callTarget) {
        const params = instruction.callTarget.signature.params.map((item) =>
          instruction.callTarget!.module.registry.runtime(item.type),
        );
        expectInputs(params);
        return;
      }
      const signature = functionTypeSignature(input(0));
      if (!signature) fail("callee is not a function", path, "invalid_call");
      expectInputs([input(0), ...signature.params]);
      expectOutputs(instruction.name === "spawn" ? [] : signature.results);
      return;
    }
    case "call_interface": {
      const interfaceType = declaredType!;
      if (
        underlyingKind(interfaceType) !== Kind.Interface &&
        underlyingKind(interfaceType) !== Kind.Any
      )
        fail("interface call descriptor is not an interface", path, "invalid_call");
      if (!assignable(input(0), interfaceType, 0))
        fail("interface call receiver type mismatch", path, "invalid_call");
      const interfaceMethods = methodsOf(interfaceType);
      const method = interfaceMethods.find((candidate) => candidate.name === instruction.method);
      // Compiler intrinsics may deliberately dispatch a method that is only
      // present on the dynamic receiver.  Validate the complete signature
      // whenever the declared interface owns the method; otherwise the
      // runtime method table remains the authority.
      if (!method) return;
      const params = method.signature.params.map((item) =>
        interfaceType.registry.runtime(item.type),
      );
      const results = method.signature.results.map((item) => interfaceType.registry.runtime(item));
      expectInputs([input(0), ...params]);
      expectOutputs(results);
      return;
    }
    case "make_closure": {
      const signature = functionTypeSignature(output(0));
      const target = instruction.callTarget!;
      if (
        !signature ||
        signature.params.length !== target.signature.params.length ||
        signature.results.length !== target.signature.results.length ||
        signature.variadic !== target.signature.variadic
      )
        fail("closure result type does not match target", path, "invalid_call");
      return;
    }
    case "len":
    case "cap": {
      let measured = input(0).registry.underlying(input(0));
      if (measured.ref.kind === Kind.Pointer)
        measured = elementType(measured)!.registry.underlying(elementType(measured)!);
      const lengthBearing =
        measured.ref.kind === Kind.Slice ||
        measured.ref.kind === Kind.Array ||
        measured.ref.kind === Kind.Map ||
        measured.ref.kind === Kind.Waitable ||
        (instruction.name === "len" &&
          measured.ref.kind === Kind.Primitive &&
          measured.ref.primitive === PrimitiveKind.PrimitiveString);
      if (!lengthBearing)
        fail("length operation requires a supported value", path, "invalid_operand");
      expectOutputs([intType]);
      return;
    }
    case "load_indirect": {
      const target = elementType(input(0));
      if (underlyingKind(input(0)) !== Kind.Pointer || !target)
        fail("indirect operand is not a pointer", path, "invalid_operand");
      expectOutputs([target]);
      return;
    }
    case "store_indirect": {
      const target = elementType(input(0));
      if (underlyingKind(input(0)) !== Kind.Pointer || !target)
        fail("indirect operand is not a pointer", path, "invalid_operand");
      expectInputs([input(0), target]);
      return;
    }
    case "load_field": {
      const target = field(input(0), descriptorString(descriptor, "field", path));
      if (!target) fail("field is not declared by operand type", path, "invalid_operand");
      expectOutputs([target]);
      return;
    }
    case "store_field": {
      const target = field(input(0), descriptorString(descriptor, "field", path));
      if (!target) fail("field is not declared by operand type", path, "invalid_operand");
      expectInputs([input(0), target]);
      return;
    }
    case "load_index":
    case "load_index_ok":
    case "store_index": {
      let collection = input(0).registry.underlying(input(0));
      if (collection.ref.kind === Kind.Pointer) {
        const pointee = elementType(collection);
        if (!pointee) fail("index operand has no element", path, "invalid_operand");
        collection = pointee.registry.underlying(pointee);
      }
      const map = mapParts(collection);
      let element: RuntimeType | undefined;
      let indexType = input(1);
      if (map) [indexType, element] = map;
      else if (
        collection.ref.kind === Kind.Primitive &&
        collection.ref.primitive === PrimitiveKind.PrimitiveString
      )
        element = primitive(fn.module.registry, PrimitiveKind.PrimitiveUint8);
      else element = elementType(collection);
      if (!element) fail("operand is not indexable", path, "invalid_operand");
      if (!map) integerInput(1);
      else if (!assignable(input(1), indexType, 1))
        fail("map key type mismatch", path, "invalid_operand");
      if (instruction.name === "store_index") {
        if (collection.ref.kind === Kind.Primitive)
          fail("string index is not writable", path, "invalid_operand");
        expectInputs([input(0), indexType, element]);
      } else {
        expectOutputs(instruction.name === "load_index_ok" ? [element, boolType] : [element]);
        if (instruction.name === "load_index_ok" && !map)
          fail("comma-ok index requires a map", path, "invalid_operand");
      }
      return;
    }
    case "string_rune_at":
    case "string_next_rune_index":
      if (primitiveKind(input(0)) !== PrimitiveKind.PrimitiveString)
        fail("rune operation requires a string", path, "invalid_operand");
      integerInput(1);
      expectOutputs([
        primitive(
          fn.module.registry,
          instruction.name === "string_rune_at"
            ? PrimitiveKind.PrimitiveInt32
            : PrimitiveKind.PrimitiveInt,
        ),
      ]);
      return;
    case "type_assert":
      expectOutputs([declaredType!]);
      return;
    case "type_assert_ok":
      expectOutputs([declaredType!, boolType]);
      return;
    case "convert":
      expectOutputs([declaredType!]);
      return;
    case "make_sequence": {
      const count = descriptorInteger(descriptor, "element_count", path);
      const element = elementType(declaredType!);
      if (!element) fail("sequence type has no element", path, "invalid_operand");
      expectInputs(Array.from({ length: count }, () => element));
      expectOutputs([declaredType!]);
      return;
    }
    case "make_struct": {
      const names = descriptorList(descriptor, "fields", path).map(String);
      const fields = fieldsOfType(declaredType!);
      expectInputs(
        names.map((name) => {
          const candidate = fields.find((field) => field.name === name)?.type;
          if (!candidate) fail("struct field does not exist", path, "invalid_operand");
          return candidate;
        }),
      );
      expectOutputs([declaredType!]);
      return;
    }
    case "make_map": {
      const parts = mapParts(declaredType!);
      if (!parts) fail("map construction requires a map type", path, "invalid_operand");
      const count = descriptorInteger(descriptor, "entry_count", path);
      const expected: RuntimeType[] = [];
      for (let index = 0; index < count; index++) expected.push(parts[0], parts[1]);
      if (instruction.operands.inputs.length === expected.length + 1) {
        integerInput(expected.length);
        expected.push(input(expected.length));
      }
      expectInputs(expected);
      expectOutputs([declaredType!]);
      return;
    }
    case "make_slice":
      instruction.operands.inputs.forEach((_, index) => integerInput(index));
      expectOutputs([declaredType!]);
      return;
    case "make_waitable":
      integerInput(0);
      expectOutputs([declaredType!]);
      return;
    case "address_of": {
      const target = descriptor.$targetType as RuntimeType;
      const pointer = output(0);
      const element = elementType(pointer);
      if (
        underlyingKind(pointer) !== Kind.Pointer ||
        !element ||
        !identical(resolveAlias(element), target)
      )
        fail("address output does not point to its target type", path, "invalid_operand");
      return;
    }
    case "append": {
      const slice = input(0);
      const element = elementType(slice);
      if (underlyingKind(slice) !== Kind.Slice || !element)
        fail("append requires a slice", path, "invalid_operand");
      if (!identical(output(0), slice))
        fail("append output differs from source slice", path, "invalid_operand");
      if (descriptorBoolean(descriptor, "expand", path)) {
        const expanded = input(1);
        const expandedElement = sequenceElement(expanded);
        const byteString =
          primitiveKind(expanded) === PrimitiveKind.PrimitiveString &&
          primitiveKind(element) === PrimitiveKind.PrimitiveUint8;
        if ((!expandedElement || !assignable(expandedElement, element, 1)) && !byteString)
          fail("expanded append requires the same slice type", path, "invalid_operand");
      } else {
        for (let index = 1; index < instruction.operands.inputs.length; index++)
          if (!assignable(input(index), element, index))
            fail("append element type mismatch", path, "invalid_operand");
      }
      return;
    }
    case "delete": {
      const parts = mapParts(input(0));
      if (!parts) fail("delete requires a map", path, "invalid_operand");
      expectInputs([input(0), parts[0]]);
      return;
    }
    case "clear":
      if (underlyingKind(input(0)) !== Kind.Map && underlyingKind(input(0)) !== Kind.Slice)
        fail("clear requires a map or slice", path, "invalid_operand");
      return;
    case "copy": {
      const destination = sequenceElement(input(0));
      const source = sequenceElement(input(1));
      const byteString =
        destination &&
        primitiveKind(destination) === PrimitiveKind.PrimitiveUint8 &&
        primitiveKind(input(1)) === PrimitiveKind.PrimitiveString;
      if (!destination || ((!source || !assignable(source, destination, 1)) && !byteString))
        fail("copy requires sequences with the same element type", path, "invalid_operand");
      expectOutputs([intType]);
      return;
    }
    case "map_keys": {
      const parts = mapParts(input(0));
      const resultElement = elementType(output(0));
      if (
        !parts ||
        underlyingKind(output(0)) !== Kind.Slice ||
        !resultElement ||
        !identical(parts[0], resultElement)
      )
        fail("map_keys result type does not match the map key", path, "invalid_operand");
      return;
    }
    case "map_iter_init": {
      if (!mapParts(input(0))) fail("map iterator requires a map", path, "invalid_operand");
      return;
    }
    case "map_iter_next": {
      const parts = mapParts(fn.localTypes[descriptor.$local as number]!);
      if (!parts) fail("map iterator local must be a map", path, "invalid_operand");
      expectOutputs([parts[0], parts[1], boolType]);
      return;
    }
    case "map_iter_close":
      if (!mapParts(fn.localTypes[descriptor.$local as number]!))
        fail("map iterator local must be a map", path, "invalid_operand");
      return;
    case "slice": {
      const source = input(0);
      const target = output(0);
      if (
        primitiveKind(source) === PrimitiveKind.PrimitiveString &&
        primitiveKind(target) === PrimitiveKind.PrimitiveString
      ) {
        for (let index = 1; index < instruction.operands.inputs.length; index++)
          if (underlyingKind(input(index)) !== Kind.Void) integerInput(index);
        return;
      }
      const sourceElement = sequenceElement(source);
      const targetElement = elementType(target);
      if (
        !sourceElement ||
        !targetElement ||
        !identical(sourceElement, targetElement) ||
        underlyingKind(target) !== Kind.Slice
      )
        fail("slice result has incompatible element type", path, "invalid_operand");
      for (let index = 1; index < instruction.operands.inputs.length; index++)
        if (underlyingKind(input(index)) !== Kind.Void) integerInput(index);
      return;
    }
    case "waitable_send":
    case "waitable_try_send": {
      const element = elementType(input(0));
      if (underlyingKind(input(0)) !== Kind.Waitable || !element)
        fail("channel operation requires a channel", path, "invalid_operand");
      if (channelDirection(input(0)) === 2)
        fail("channel direction does not permit send", path, "invalid_operand");
      expectInputs([input(0), element]);
      if (instruction.name === "waitable_try_send") expectOutputs([boolType]);
      return;
    }
    case "waitable_recv":
    case "waitable_recv_ok":
    case "waitable_try_recv": {
      const element = elementType(input(0));
      if (underlyingKind(input(0)) !== Kind.Waitable || !element)
        fail("channel operation requires a channel", path, "invalid_operand");
      if (channelDirection(input(0)) === 3)
        fail("channel direction does not permit receive", path, "invalid_operand");
      expectOutputs(instruction.name === "waitable_recv" ? [element] : [element, boolType]);
      return;
    }
    case "waitable_can_recv":
    case "waitable_can_send":
      if (underlyingKind(input(0)) !== Kind.Waitable)
        fail("channel operation requires a channel", path, "invalid_operand");
      if (
        (instruction.name === "waitable_can_recv" && channelDirection(input(0)) === 3) ||
        (instruction.name === "waitable_can_send" && channelDirection(input(0)) === 2)
      )
        fail("channel direction does not permit operation", path, "invalid_operand");
      expectOutputs([boolType]);
      return;
    case "waitable_close":
      if (underlyingKind(input(0)) !== Kind.Waitable)
        fail("channel operation requires a channel", path, "invalid_operand");
      if (channelDirection(input(0)) === 2)
        fail("channel direction does not permit close", path, "invalid_operand");
      return;
    case "get_path": {
      let current = declaredType!;
      if (!identical(input(0), current))
        fail("field path input differs from descriptor", path, "invalid_operand");
      for (const rawIndex of descriptorList(descriptor, "fields", path)) {
        if (underlyingKind(current) === Kind.Pointer) current = elementType(current)!;
        const next = fieldsOfType(current)[Number(rawIndex)];
        if (!next || next.name === "_")
          fail("field path index is invalid", path, "invalid_operand");
        current = next.type;
      }
      expectOutputs([current]);
      return;
    }
    case "index_convert":
      expectOutputs([declaredType!]);
      return;
    case "len_compare_branch":
      integerInput(1);
      return;
    case "min_max":
      return;
    default:
      return;
  }
}

function validateSlotInitialization(fn: PreparedFunction): void {
  const count = fn.instructions.length;
  if (!count) return;
  if (Math.ceil(fn.slotTypes.length / 64) * count > 4 * 1024 * 1024)
    fail("slot initialization analysis exceeds limit", fn.id, "load_limit");
  const successors: number[][] = Array.from({ length: count }, () => []);
  for (let pc = 0; pc < count; pc++) {
    const instruction = fn.instructions[pc]!;
    const addTarget = (target: number): void => {
      // A label after the final executable instruction maps to the normal
      // fallthrough completion PC after labels are stripped.
      if (target < count) successors[pc]!.push(target);
      else if (target > count) fail("jump target is outside function", fn.id, "invalid_jump");
    };
    let fallthrough = true;
    if (
      instruction.name === "compare_branch" ||
      instruction.name === "len_compare_branch" ||
      instruction.name === "update_compare_branch"
    )
      addTarget(instruction.target!);
    else if (instruction.name === "jump" || instruction.name === "jump_if") {
      addTarget(instruction.target!);
      fallthrough = instruction.name === "jump_if";
    } else if (instruction.name === "type_dispatch") {
      addTarget(instruction.typeDispatch!.defaultTarget);
      for (const candidate of instruction.typeDispatch!.cases) addTarget(candidate.target);
      fallthrough = false;
    } else if (["panic", "return", "tail_call_direct"].includes(instruction.name)) {
      fallthrough = false;
    }
    if (fallthrough && pc + 1 < count) successors[pc]!.push(pc + 1);
  }

  const states: Array<Set<number> | undefined> = new Array(count);
  states[0] = new Set();
  const queue = [0];
  const queued = new Set(queue);
  while (queue.length) {
    const pc = queue.pop()!;
    queued.delete(pc);
    const state = new Set(states[pc]!);
    const operands = fn.instructions[pc]!.operands;
    for (const slot of operands.releaseBefore) state.delete(slot);
    for (const [kind, slot] of operands.inputs)
      if (kind === 0 && !state.has(slot))
        fail(
          `temporary slot ${slot} may be uninitialized`,
          `${fn.id}.instructions[${pc}]`,
          "invalid_operand",
        );
    for (const output of operands.outputs) if (output < LOCAL_OUTPUT) state.add(output);
    for (const slot of operands.release) state.delete(slot);
    for (const target of successors[pc]!) {
      const previous = states[target];
      let changed = previous === undefined;
      if (!previous) states[target] = new Set(state);
      else
        for (const slot of [...previous])
          if (!state.has(slot)) {
            previous.delete(slot);
            changed = true;
          }
      if (changed && !queued.has(target)) {
        queued.add(target);
        queue.push(target);
      }
    }
  }
}

function validateTypeDescriptor(
  module: PreparedModule,
  value: unknown,
  path: string,
  allowInvalid = false,
): RuntimeType | undefined {
  const type = descriptorRecord(value, path);
  assertAllowedFields(type, WIRE_FIELDS.TypeRef, path);
  const kind = descriptorInteger(type, "kind", path);
  if (kind === Kind.Invalid && allowInvalid) return undefined;
  if (kind < Kind.Void || kind > Kind.Interface)
    fail("runtime type kind is invalid", `${path}.kind`, "invalid_type");
  const primitive = descriptorInteger(type, "primitive", path, true);
  const namedValue = type.named === undefined ? {} : descriptorRecord(type.named, `${path}.named`);
  assertAllowedFields(namedValue, WIRE_FIELDS.TypeKey, `${path}.named`);
  const ref: TypeRef = {
    kind,
    primitive,
    named: {
      module_path: descriptorString(namedValue, "module_path", `${path}.named`, true),
      decl_id: descriptorString(namedValue, "decl_id", `${path}.named`, true),
    },
    node: descriptorString(type, "node", path, true),
  };
  const runtime = module.registry.runtime(ref);
  if ((ref.node || kind === Kind.Named) && !module.registry.node(runtime))
    fail("type reference does not exist", path, "invalid_type");
  return runtime;
}

function localFrom(
  value: Record<string, unknown>,
  name: string,
  indexes: ReadonlyMap<string, number>,
  path: string,
  optional = false,
): number | undefined {
  const id = descriptorString(value, name, path, optional);
  if (optional && !id) return undefined;
  const index = indexes.get(id);
  if (index === undefined) fail(`unknown ${name}`, `${path}.${name}`, "link_error");
  return index;
}

function descriptorRecord(value: unknown, path: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail("expected object", path);
  return value as Record<string, unknown>;
}

function assertAllowedFields(
  value: Record<string, unknown>,
  fields: readonly string[],
  path: string,
): void {
  const allowed = new Set(fields);
  for (const key of Object.keys(value))
    if (!key.startsWith("$") && !allowed.has(key)) fail(`unknown field ${key}`, `${path}.${key}`);
}

function descriptorString(
  value: Record<string, unknown>,
  name: string,
  path: string,
  optional = false,
): string {
  const field = value[name];
  if (field === undefined && optional) return "";
  if (typeof field !== "string" || (!optional && !field.trim()))
    fail("expected non-empty string", `${path}.${name}`);
  return field;
}

function descriptorInteger(
  value: Record<string, unknown>,
  name: string,
  path: string,
  optional = false,
): number {
  const field = value[name];
  if (field === undefined && optional) return 0;
  if (typeof field !== "number" || !Number.isSafeInteger(field))
    fail("expected integer", `${path}.${name}`);
  return field;
}

function descriptorBoolean(value: Record<string, unknown>, name: string, path: string): boolean {
  const field = value[name];
  if (field === undefined) return false;
  if (typeof field !== "boolean") fail("expected boolean", `${path}.${name}`);
  return field;
}

function descriptorList(value: Record<string, unknown>, name: string, path: string): unknown[] {
  const field = value[name];
  if (field === undefined) return [];
  if (!Array.isArray(field)) fail("expected array", `${path}.${name}`);
  return field;
}

function validateDistinctSlots(slots: readonly number[], path: string): void {
  if (new Set(slots).size !== slots.length) fail("duplicate slot", path, "invalid_operand");
}

function validateU32(value: number, path: string): void {
  if (!Number.isSafeInteger(value) || value < 0 || value > MAX_U32)
    fail("value is outside uint32 range", path, "invalid_operand");
}

function checkedCount(value: number, multiplier: number, path: string): number {
  nonNegative(value, path);
  const result = value * multiplier;
  if (!Number.isSafeInteger(result)) fail("operand count overflows", path, "invalid_operand");
  return result;
}

function nonNegative(value: number, path: string): void {
  if (value < 0) fail("count is negative", path, "invalid_operand");
}

function nonNegativeCounts(descriptor: Record<string, unknown>, path: string): void {
  nonNegative(descriptorInteger(descriptor, "arg_count", path), path);
  nonNegative(descriptorInteger(descriptor, "result_count", path, true), path);
}

function prepareOperands(value: JsonValue, path: string): PreparedOperands {
  const raw = schemaObject(value, path, WIRE_FIELDS.RegisterFlow);
  return {
    inputs: array(raw.inputs, `${path}.inputs`).map((input, index) => {
      const inputPath = `${path}.inputs[${index}]`;
      if (!Array.isArray(input) || input.length !== 2) fail("invalid operand", inputPath);
      return [integer(input[0], `${inputPath}[0]`), integer(input[1], `${inputPath}[1]`)] as const;
    }),
    outputs: array(raw.outputs, `${path}.outputs`).map((item, index) =>
      integer(item, `${path}.outputs[${index}]`),
    ),
    release: array(raw.release, `${path}.release`).map((item, index) =>
      integer(item, `${path}.release[${index}]`),
    ),
    releaseBefore: array(raw.release_before, `${path}.release_before`).map((item, index) =>
      integer(item, `${path}.release_before[${index}]`),
    ),
  };
}

function plainObject(
  value: JsonValue,
  path: string,
  fields: readonly string[],
): Record<string, unknown> {
  const raw = schemaObject(value, path, fields);
  return plain(raw, path) as Record<string, unknown>;
}

function plain(value: JsonValue, path: string): unknown {
  if (value instanceof JsonNumber) {
    const exact = value.integer(path);
    if (exact < BigInt(Number.MIN_SAFE_INTEGER) || exact > BigInt(Number.MAX_SAFE_INTEGER))
      return exact;
    return Number(exact);
  }
  if (Array.isArray(value)) return value.map((item, index) => plain(item, `${path}[${index}]`));
  if (value && typeof value === "object") {
    const result: Record<string, unknown> = {};
    for (const [key, item] of Object.entries(value)) result[key] = plain(item, `${path}.${key}`);
    return result;
  }
  return value;
}
