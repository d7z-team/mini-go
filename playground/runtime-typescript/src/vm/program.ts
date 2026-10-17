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
import {
  operatorCode,
  unaryOperatorCode,
  type OperatorCode,
  type UnaryOperatorCode,
} from "./operators";
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

export interface PreparedInstruction {
  opcode: number;
  name: string;
  /** Index in the wire instruction array, retained for source/debug mapping. */
  wirePC: number;
  descriptor: Record<string, unknown> | undefined;
  operands: PreparedOperands;
  arity: readonly [number, number];
  inputScratch: boolean;
  /** Effects not already accounted for at a value storage boundary. */
  changesReachability: boolean;
  target?: number;
  local?: number;
  upvalue?: number;
  global?: number;
  constant?: number;
  type?: RuntimeType;
  /** First two operands and first two outputs are read without inputScratch. */
  input0?: readonly [number, number];
  input1?: readonly [number, number];
  output0?: number;
  output1?: number;
  operatorCode?: OperatorCode | UnaryOperatorCode;
  /** Valid only in the caller's source Program; old logical calls resolve current by identity. */
  callTarget?: PreparedFunction;
  method?: string;
  intrinsic?: string;
  field?: string;
  fieldPath?: readonly number[];
  /** Prepared frame-local index for map iterator state. */
  iteratorLocal?: number;
  captures?: readonly Record<string, unknown>[];
  selectIndex?: number;
  selectCases?: readonly PreparedSelectCase[];
}

export interface PreparedFunction {
  readonly id: string;
  readonly module: PreparedModule;
  readonly revisionLocal: boolean;
  readonly signature: FunctionSignature;
  readonly localTypes: RuntimeType[];
  readonly localIndexes: Map<string, number>;
  readonly cellLocals: ReadonlySet<number>;
  readonly resultLocals: number[];
  readonly upvalueTypes: RuntimeType[];
  readonly upvalueIndexes: Map<string, number>;
  readonly slotTypes: RuntimeType[];
  readonly maxInputArity: number;
  readonly instructions: PreparedInstruction[];
  readonly labels: Map<string, number>;
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
  readonly constantUntyped: boolean[];
  readonly constantIndexes: Map<string, number>;
  readonly globals: Cell[];
  readonly globalIndexes: Map<string, number>;
  readonly functions: Map<string, PreparedFunction>;
  readonly exports: Map<string, { kind: string; id: string; type?: RuntimeType; untyped: boolean }>;
  readonly requirements: Set<string>;
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

export class Program {
  private constructor(
    readonly hash: string,
    readonly root: string,
    readonly compilerID: string,
    readonly targetTags: readonly string[],
    readonly capabilities: readonly string[],
    readonly entries: Map<string, ProgramEntry>,
    readonly modules: Map<string, PreparedModule>,
  ) {}

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
      entries.set(prepared.name, prepared);
    }
    if (!entries.size) fail("execution image contains no entries", "image.entries");
    const imageHash = string(image.hash, "image.hash");
    const calculatedHash = await sha256Hex(document.replacingProperty(image, "hash", '""'));
    if (calculatedHash !== imageHash) fail("execution image hash mismatch", "image.hash");
    return new Program(imageHash, root, compilerID, targetTags, capabilities, entries, modules);
  }

  function(module: string, id: string): PreparedFunction {
    const fn = this.modules.get(module)?.functions.get(id);
    if (!fn) throw new VMError(`unknown function ${module}.${id}`, "link_error", "function");
    return fn;
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
    module.constantIndexes.set(id, module.constants.length);
    module.constants.push(decodeConstant(type, constant.value));
    module.constantUntyped.push(boolean(constant.untyped, `${path}.untyped`));
  }
  for (const [index, item] of array(artifact.globals, `${module.path}.globals`).entries()) {
    const path = `${module.path}.globals[${index}]`;
    const global = schemaObject(item, path, WIRE_FIELDS.Global);
    const id = string(global.id, `${path}.id`);
    if (module.globalIndexes.has(id)) fail("duplicate global", `${path}.id`);
    if (!global.type) fail("global is missing type", `${path}.type`);
    const type = module.registry.runtime(parseTypeRef(global.type, `${path}.type`));
    module.globalIndexes.set(id, module.globals.length);
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
    module.exports.set(name, declaration);
  }

  const functions = array(artifact.functions, `${module.path}.functions`);
  for (const [index, item] of functions.entries()) {
    const path = `${module.path}.functions[${index}]`;
    const raw = schemaObject(item, path, WIRE_FIELDS.Function);
    const id = string(raw.id, `${path}.id`);
    if (module.functions.has(id)) fail("duplicate function", `${path}.id`);
    if (!raw.signature || !raw.code) fail("function is incomplete", path);
    const code = schemaObject(raw.code, `${path}.code`, WIRE_FIELDS.SlotCode);
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
      if (!Array.isArray(item) || item.length !== 3)
        fail("instruction must contain opcode, descriptor and operands", instructionPath);
      const opcode = integer(item[0], `${instructionPath}[0]`);
      const descriptorIndex = integer(item[1], `${instructionPath}[1]`);
      const operandIndex = integer(item[2], `${instructionPath}[2]`);
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
          arity[0] > 2 ||
          [17, 21, 22, 23, 24, 26, 27, 28, 29, 35, 38, 65, 66, 67, 72].includes(opcode),
      });
      maxInputArity = Math.max(maxInputArity, prepared.operands.inputs.length);
      executablePC++;
    }
    wireToExecutablePC.push(executablePC);
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
      revisionLocal: boolean(raw.revision_local, `${path}.revision_local`),
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
      labels,
      wireToExecutablePC,
      executableToWirePC,
    };
    module.functions.set(id, fn);
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
      owner.requirements.add(dependencyPath);
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
    prepareInstructionMetadata(instruction);
  }
  validateSlotInitialization(fn);
}

function prepareInstructionMetadata(instruction: PreparedInstruction): void {
  const input0 = instruction.operands.inputs[0];
  const input1 = instruction.operands.inputs[1];
  const output0 = instruction.operands.outputs[0];
  const output1 = instruction.operands.outputs[1];
  if (input0 !== undefined) instruction.input0 = input0;
  if (input1 !== undefined) instruction.input1 = input1;
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
        descriptorRecord(capture, instruction.name),
      ),
    );
  }
  if (instruction.name === "select") {
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
  const validateAddress = (value: unknown, addressPath: string, allowExport: boolean): void => {
    const address = descriptorRecord(value, addressPath);
    assertAllowedFields(address, WIRE_FIELDS.AddressPayload, addressPath);
    const kind = descriptorString(address, "kind", addressPath);
    if (kind === "local")
      address.$index = localFrom(address, "local", fn.localIndexes, addressPath);
    else if (kind === "upvalue")
      address.$index = localFrom(address, "upvalue", fn.upvalueIndexes, addressPath);
    else if (kind === "global")
      address.$index = localFrom(address, "global", fn.module.globalIndexes, addressPath);
    else if (kind === "export" && allowExport) {
      const modulePath = descriptorString(address, "module_path", addressPath);
      if (!fn.module.requirements.has(modulePath))
        fail("export address module is not a dependency", addressPath, "link_error");
      const exported = modules
        .get(modulePath)
        ?.exports.get(descriptorString(address, "export", addressPath));
      if (exported?.kind !== "global")
        fail("export address does not resolve to a global", addressPath, "link_error");
    } else fail("invalid address kind", `${addressPath}.kind`, "invalid_operand");
    for (const [index, raw] of descriptorList(address, "path", addressPath).entries()) {
      const segmentPath = `${addressPath}.path[${index}]`;
      const segment = descriptorRecord(raw, segmentPath);
      assertAllowedFields(segment, WIRE_FIELDS.AddressPathSegment, segmentPath);
      const segmentKind = descriptorString(segment, "kind", segmentPath);
      if (segmentKind === "field") descriptorString(segment, "field", segmentPath);
      else if (segmentKind === "index")
        segment.$index = localFrom(segment, "local", fn.localIndexes, segmentPath);
      else if (segmentKind !== "indirect")
        fail("invalid address path segment", `${segmentPath}.kind`, "invalid_operand");
    }
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
    case "spawn":
      nonNegativeCounts(descriptor, path);
      if (descriptorInteger(descriptor, "result_count", path, true) !== 0)
        fail("spawn cannot produce results", path, "invalid_call");
      break;
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
      const captures = descriptorList(descriptor, "captures", path);
      if (captures.length !== callee.upvalueTypes.length)
        fail("closure capture count does not match target", path, "invalid_call");
      captures.forEach((capture, index) =>
        validateAddress(capture, `${path}.captures[${index}]`, false),
      );
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
      if (!target.exports.has(descriptorString(descriptor, "export", path)))
        fail("export does not exist", path, "link_error");
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

function validateSlotInitialization(fn: PreparedFunction): void {
  const count = fn.instructions.length;
  if (!count) return;
  if (Math.ceil(fn.slotTypes.length / 64) * count > 4 * 1024 * 1024)
    fail("slot initialization analysis exceeds limit", fn.id, "load_limit");
  const successors: number[][] = Array.from({ length: count }, () => []);
  for (let pc = 0; pc < count; pc++) {
    const instruction = fn.instructions[pc]!;
    const descriptor = instruction.descriptor ?? {};
    const addLabel = (value: unknown): void => {
      const target = fn.labels.get(String(value));
      if (target === undefined) fail("unknown jump label", fn.id, "invalid_jump");
      // A label after the final executable instruction maps to the normal
      // fallthrough completion PC after labels are stripped.
      if (target < count) successors[pc]!.push(target);
      else if (target > count) fail("jump target is outside function", fn.id, "invalid_jump");
    };
    let fallthrough = true;
    if (instruction.name === "compare_branch" || instruction.name === "len_compare_branch")
      addLabel(descriptor.label);
    else if (instruction.name === "jump" || instruction.name === "jump_if") {
      addLabel(descriptor.label);
      fallthrough = instruction.name === "jump_if";
    } else if (instruction.name === "type_dispatch") {
      addLabel(descriptor.default);
      for (const value of descriptorList(descriptor, "cases", fn.id))
        addLabel(descriptorRecord(value, fn.id).label);
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
  indexes: Map<string, number>,
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
  const raw = schemaObject(value, path, WIRE_FIELDS.SlotOperands);
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
