import type {
  Bindings,
  Frame as PublicFrame,
  FrameRef,
  HostValue,
  Snapshot,
  Stats,
  VMOptions,
} from "./types";
import { Kind, PrimitiveKind, STORAGE_COST } from "./contract.generated";
import { VMError, fail } from "./json";
import {
  binaryCode,
  binaryIntegerBits,
  binaryScalar,
  compareCode,
  compareIntegerBits,
  compareScalar,
  convert,
  unaryCode,
  unaryIntegerBits,
  unaryScalar,
  OperatorCode,
  type ScalarValue,
} from "./operators";
import {
  Program,
  type LoadOptions,
  type PreparedAddressPlan,
  type PreparedExport,
  type PreparedFunction,
  type PreparedInstruction,
  type PreparedModule,
  type PreparedSelectCase,
  type PreparedTypeDispatch,
} from "./program";
import {
  advanceAddress,
  asIndex,
  assign,
  assertComparable,
  boolValue,
  cellAccess,
  cloneValue,
  decodeRune,
  dereference,
  fieldValue,
  guestEqual,
  importHostValue,
  indexValue,
  intValue,
  nilValue,
  normalize,
  primitive,
  setIndex,
  snapshot,
  stringValue,
  storageAddress,
  zero,
  detachTypeClosure,
  type Address,
  type Cell,
  type Channel,
  type DynamicType,
  type FunctionValue,
  type RuntimeType,
  type SliceValue,
  type TypeNode,
  type TypeRegistry,
  type Value,
} from "./value";
import { GuestMemory, type GuestControlLease, type GuestControlReservation } from "./guest-memory";
import type { GuestHandle, ManagedLayout } from "./managed-heap";
import { FrameStorage } from "./frame-storage";
import { TypedMap } from "./typed-map";
import { TypedChannel } from "./typed-channel";
import {
  decodeStorage,
  encodeStorage,
  aggregateKind,
  packedAggregate,
  packedFields,
  packedItems,
  packedLength,
  prepareArrayStorageLayout,
  prepareStorageLayout,
  remapStorageLayout,
  resolveStorageLocation,
  storageReferences,
  writePackedField,
  type PackedAggregate,
  type PackedStorage,
  type StorageLayout,
  type StorageOwner,
} from "./aggregate-storage";

const LOCAL_OUTPUT = 0x80000000;
const DEFAULT_MAX_IMAGE_BYTES = 64 * 1024 * 1024;
const FRAME_CACHE_ENTRY_BYTES = 64;
const FRAME_CACHE_VALUE_BYTES = 16;
const TASK_INSTRUCTION_QUANTUM = 512;

function isIntegerPrimitive(kind: number | undefined): kind is number {
  return (
    kind !== undefined &&
    kind >= PrimitiveKind.PrimitiveInt &&
    kind <= PrimitiveKind.PrimitiveUintptr
  );
}
const EMPTY_CAPTURES: Address[] = [];
const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();
const strictTextDecoder = new TextDecoder("utf-8", { fatal: true });
function isFreshScalar(value: Value): boolean {
  return (
    value.kind === "bool" ||
    value.kind === "int" ||
    value.kind === "uint" ||
    value.kind === "float" ||
    value.kind === "complex"
  );
}

function isFreshScalarProducer(opcode: number): boolean {
  return (
    opcode === 4 ||
    opcode === 6 ||
    opcode === 7 ||
    opcode === 33 ||
    opcode === 34 ||
    opcode === 36 ||
    opcode === 37 ||
    opcode === 41
  );
}

function compareLength(code: number, length: number, other: Value): boolean {
  if (other.kind !== "int" && other.kind !== "uint")
    invalid("length comparison requires an integer", "len_compare_branch");
  const right = other.value as bigint;
  const left = BigInt(length);
  switch (code) {
    case OperatorCode.Less:
      return left < right;
    case OperatorCode.LessEqual:
      return left <= right;
    case OperatorCode.Greater:
      return left > right;
    case OperatorCode.GreaterEqual:
      return left >= right;
    case OperatorCode.Equal:
      return left === right;
    case OperatorCode.NotEqual:
      return left !== right;
    default:
      invalid("invalid length comparison operator", String(code));
  }
}

function minMaxValues(values: readonly Value[], operator: string): Value {
  if (!values.length) invalid("min/max requires at least one value", "min_max");
  if (operator !== "min" && operator !== "max") invalid("invalid min/max operator", "min_max");
  const chooseMin = operator === "min";
  let best = values[0]!;
  for (let index = 1; index < values.length; index++) {
    const candidate = values[index]!;
    let replace = false;
    if (best.kind === "float" && candidate.kind === "float") {
      const left = best.value as number;
      const right = candidate.value as number;
      if (Number.isNaN(left)) continue;
      if (Number.isNaN(right)) {
        best = candidate;
        continue;
      }
      if (left === 0 && right === 0) {
        const leftNegative = Object.is(left, -0);
        const rightNegative = Object.is(right, -0);
        replace = chooseMin ? rightNegative && !leftNegative : !rightNegative && leftNegative;
      } else replace = chooseMin ? right < left : right > left;
    } else if (best.kind === "string" && candidate.kind === "string") {
      replace = compareCode(chooseMin ? OperatorCode.Less : OperatorCode.Greater, candidate, best);
    } else if ((best.kind === "int" || best.kind === "uint") && best.kind === candidate.kind) {
      const left = best.value as bigint;
      const right = candidate.value as bigint;
      replace = chooseMin ? right < left : right > left;
    } else invalid("operand types do not support min/max", "min_max");
    if (replace) best = candidate;
  }
  return best;
}

export type VMAction =
  | { kind: "call"; id: number; route: string; payload: number[] }
  | { kind: "cancel"; id: number }
  | { kind: "decision"; id: number; accepted: boolean }
  | { kind: "close" };

export interface VMPumpState {
  running: boolean;
  ready: boolean;
  closed: boolean;
  delay?: number;
  error?: string;
  executions: { id: number; state: string; settled: boolean; scopeError?: string }[];
}

interface Frame {
  readonly fn: PreparedFunction;
  readonly revision: Revision;
  readonly profileKey: string;
  readonly locals: (Cell | undefined)[];
  readonly localBank: FrameStorage;
  readonly upvalues: Address[];
  readonly slotBank: FrameStorage;
  readonly resultBank: FrameStorage;
  /** Reused only for the synchronous duration of one instruction. */
  readonly inputScratch: Value[];
  readonly integerScratch: Uint32Array;
  mapIterators?: (MapIteratorState | undefined)[];
  readonly returnOutputs: readonly number[];
  pc: number;
  rootPC?: number;
  defers?: Cell[];
  deferMemory?: GuestControlLease[];
  returning?: FrameStorage;
  returningCount?: number;
  panic?: Cell;
  recovered: boolean;
  deferred: boolean;
  memory: GuestControlLease;
  readonly resident: { release(): void };
  readonly storageBytes: number;
  scratchCapacity: number;
  initializing?: string;
  tailReturn?: boolean;
  completion?:
    | { kind: "reflectCall"; outputs: readonly number[] }
    | {
        kind: "dynamicCallback";
        outputs: readonly number[];
        resultTypes: RuntimeType[];
        reflectResults?: boolean;
      };
}

interface ReusableFrameStorage {
  fn: PreparedFunction;
  locals: (Cell | undefined)[];
  upvalues: Address[];
  localBank: FrameStorage;
  slotBank: FrameStorage;
  resultBank: FrameStorage;
  inputScratch: Value[];
  integerScratch: Uint32Array;
  scratchCapacity: number;
  bytes: number;
  resident: { release(): void };
  cacheIndex: number;
}

interface PreparedFrameLayout {
  readonly locals: readonly StorageLayout[];
  readonly slots: readonly StorageLayout[];
  readonly results: readonly StorageLayout[];
  readonly resultTypes: readonly RuntimeType[];
  readonly logical: bigint;
}

type LocalSlot = Value | Cell;

type MapIterators = NonNullable<Frame["mapIterators"]>;

function isLocalCell(slot: LocalSlot | undefined): slot is Cell {
  return Boolean(slot && "id" in slot);
}

function localValue(slot: LocalSlot): Value {
  return isLocalCell(slot) ? slot.read() : slot;
}

interface Task {
  readonly id: number;
  readonly scope: number;
  frames: Frame[];
  state: "runnable" | "blocked" | "done" | "failed" | "canceled";
  blocked?:
    | { kind: "send"; channel: Channel; value: Cell }
    | {
        kind: "receive";
        channel: Channel;
        outputs: readonly number[];
        receiveOK: boolean;
        reflected?: boolean;
      }
    | { kind: "select" }
    | {
        kind: "reflectSelect";
        cases: ReflectedSelectCase[];
        outputs: readonly number[];
        memory?: GuestControlLease;
      }
    | { kind: "ffi"; id: number };
  error?: VMError;
  memory?: GuestControlLease;
  /** Logical fuel paid for the current fused micro-op before it commits. */
  chargedInstructionSteps?: number;
}

interface ExecutionRecord {
  readonly id: number;
  readonly scope: number;
  state: "Running" | "Completed" | "Failed" | "Canceled";
  settled: boolean;
  result?: Snapshot;
  error?: VMError;
  scopeError?: VMError;
}

interface Scope {
  tasks: Set<number>;
  execution?: number;
  rootTask?: number;
  error?: VMError;
}

interface Revision {
  generation: number;
  program: Program;
  constants?: GuestHandle;
}

interface PendingHostCall {
  task: number;
  outputs: readonly number[];
  resultType: RuntimeType;
  memory: GuestControlLease;
  reply?: { payload: Uint8Array; error?: string };
  replyMemory?: GuestControlLease;
  cancelSent?: boolean;
}

interface TimerRecord {
  readonly id: number;
  readonly channel: Channel;
  readonly deadline: number;
  readonly memory: GuestControlLease;
  canceled: boolean;
}

interface ReflectedView {
  current?: Value;
  declaredType?: RuntimeType;
  target?: Address;
  refresh?: Cell;
  addressable: boolean;
  settable: boolean;
  interfaceable: boolean;
  embeddedReadOnly: boolean;
}

interface ReflectedSelectCase {
  index: number;
  channel?: Cell;
  send?: Cell;
  receive: boolean;
  default: boolean;
}

interface MapIteratorState {
  map: TypedMap | undefined;
  entries: Uint32Array;
  offset: number;
  memory?: GuestControlLease;
  resident?: { release(): void };
}

interface DebugLocation {
  file: string;
  line: number;
  column: number;
}

interface FunctionDebugInfo {
  name: string;
  locations: Map<number, DebugLocation[]>;
  localNames: Map<string, string>;
}

interface DebugEvent {
  reason: "breakpoint" | "pause" | "step" | "exception";
  task: number;
}

export class VirtualMachine {
  private revision: Revision;
  private readonly revisions = new Map<number, Revision>();
  private generation = 1;
  private nextTask = 1;
  private nextScope = 1;
  private nextHostCall = 1;
  private nextTimer = 1;
  private readonly tasks = new Map<number, Task>();
  private readonly runnable: number[] = [];
  private runnableHead = 0;
  private readonly scopes = new Map<number, Scope>();
  private readonly executions = new Map<number, ExecutionRecord>();
  private readonly pendingHost = new Map<number, PendingHostCall>();
  private readonly pendingActions: VMAction[] = [];
  private readonly timers = new Map<number, TimerRecord>();
  private readonly frameCache: ReusableFrameStorage[] = [];
  private frameCacheByFunction = new WeakMap<PreparedFunction, ReusableFrameStorage[]>();
  private frameCacheBytes = 0;
  private readonly initialized = new Set<string>();
  private readonly initializing = new Set<string>();
  private readonly options: VMOptions;
  private paused = false;
  private closing = false;
  private closed = false;
  private ready = false;
  private initializationError?: VMError;
  // Keep the overwhelmingly common range as a machine Number.  Promotion is
  // one-way and happens only after the exact integer range is exhausted; all
  // public counters continue to expose BigInt.
  private steps = 0;
  private stepsOverflow?: bigint;
  private ffiCalls = 0n;
  private ffiBytes = 0n;
  private epoch = 1n;
  private readonly guestMemory: GuestMemory;
  private readonly storageOwner: StorageOwner;
  private readonly allocationRoots: GuestHandle[] = [];
  private gcNanos = 0n;
  private nextMajorBytes = 8n * 1024n * 1024n;
  /** Cell-to-handle owner table used only to publish explicit GC roots. */
  private readonly managedCells = new Map<Cell, NonNullable<Cell["managedHandle"]>>();
  private managedCellsByHandle = new Map<string, Cell>();
  private readonly managedStorageLayouts = new Map<
    string,
    { layout: StorageLayout; type: RuntimeType }
  >();
  private readonly channelHandles = new WeakMap<object, GuestHandle>();
  private readonly mapHandles = new WeakMap<object, GuestHandle>();
  private readonly managedValueHandles = new WeakMap<object, GuestHandle>();
  private readonly stringStorage = new WeakMap<
    Uint8Array,
    { handle: GuestHandle; offset: number }
  >();
  /**
   * Storage payloads use a numeric type id. Intern by registry and canonical
   * shape rather than RuntimeType object identity: registry.runtime() creates
   * fresh wrappers for the same type, and identity-only interning otherwise
   * retains one metadata entry per encode/decode cycle.
   */
  private readonly storageTypeIDs = new Map<string, number>();
  private readonly ownedTypes = new WeakMap<TypeRegistry, Map<string, RuntimeType>>();
  private readonly ownedLayouts = new WeakMap<StorageLayout, StorageLayout>();
  /** Type descriptors held by an in-progress reserve/build/publish operation. */
  private readonly storageConstructionPins = new Map<number, number>();
  private readonly storageDataPins = new Map<number, number>();
  private readonly storageConstructionOwners: Map<number, number>[] = [];
  private storageConstructionDepth = 0;
  private readonly storageAllocationScopes: {
    handles: GuestHandle[];
    done: boolean;
  }[] = [];
  private readonly storageRegistryIDs = new WeakMap<TypeRegistry, number>();
  private nextStorageRegistryID = 1;
  // Type descriptors belong to loaded programs/revisions; payload ids remain
  // stable for the VM lifetime and are released together with the VM ledger.
  private readonly storageTypes: (RuntimeType | undefined)[] = [];
  private readonly storageTypeKeys: (string | undefined)[] = [];
  private readonly freeStorageTypeIDs: number[] = [];
  private mapsByHandle = new Map<string, TypedMap>();
  private channelsByHandle = new Map<string, Channel>();
  private readonly functionHandles = new WeakMap<object, GuestHandle>();
  private readonly functionsByHandle = new Map<
    string,
    {
      module: string;
      function: string;
      revision: number;
      kind: Value["kind"];
      captures: RuntimeType[];
      receiver: boolean;
      dynamicType: RuntimeType;
    }
  >();
  private readonly preparedFrameLayouts = new WeakMap<PreparedFunction, PreparedFrameLayout>();
  private readonly storageLayouts = new WeakMap<RuntimeType, StorageLayout>();
  private referencedRevisions = new Set<number>();
  private readonly dynamicTypes = new Map<string, RuntimeType>();
  private readonly dynamicTypeLeases = new Map<
    string,
    { memory: GuestControlLease; resident: { release(): void }; bytes: number }
  >();
  /** Test-only fault point; production options never expose allocation faults. */
  private testAllocationFault?: {
    phase: "reserve" | "build" | "publish";
    remaining: number;
  };
  // Reflection keys refer to descriptors owned by the loaded revision set.
  // The map is a lookup cache, not a guest root or a second storage ledger.
  private readonly reflectedTypes = new Map<string, RuntimeType>();
  private readonly reflectedTypeBytes = new WeakMap<Uint8Array, string>();
  private readonly reflectedTypeOwners = new Map<string, Set<string>>();
  /** A reflect.Type value can cross a frame boundary before its key string is
   * published into managed storage. Ownership transfers to the key backing
   * record when retainReflectedType observes that publication. */
  private readonly pendingReflectedTypes = new Set<string>();
  private dynamicTypeBytes = 0;
  private readonly debugFunctions = new Map<string, FunctionDebugInfo>();
  private symbolProgramHash = "";
  private readonly debugBreakpoints = new Set<string>();
  private readonly debugEventsQueue: Record<string, unknown>[] = [];
  private readonly debugFrames = new Map<number, FrameRef>();
  private readonly debugVariables = new Map<number, FrameRef>();
  private readonly debugSources = new Map<string, { module: string; path: string; text: string }>();
  private debugEntry = "default";
  private debugExecution?: number;
  private debugTerminated = false;
  private debugConfigured = false;
  private nextDebugHandle = 1;
  private debugIgnoreBreakpoint = "";
  private debugStepTask: number | undefined;
  private readonly opcodeProfile = new Map<string, bigint>();
  private profileDispatches = 0n;
  private readonly functionProfile = new Map<
    string,
    { calls: bigint; steps: bigint; nanos: bigint }
  >();
  private readonly profileTiming: boolean;
  private readonly maxSteps: bigint | undefined;
  private readonly maxStepsNumber: number | undefined;

  readonly maxInputBytes: number;
  readonly maxHostResultBytes: number;
  readonly maxImageBytes: number;

  private constructor(program: Program, options: VMOptions) {
    this.options = options;
    this.profileTiming = options.profile === true && options.profileTiming === true;
    this.guestMemory = new GuestMemory(options.limits);
    this.guestMemory.managedHeap.onReclaim = (slot, generation) => {
      const key = `${slot}:${generation}`;
      this.reflectedTypeOwners.delete(key);
      const cell = this.managedCellsByHandle.get(key);
      if (cell) this.managedCells.delete(cell);
      this.managedCellsByHandle.delete(key);
      this.managedStorageLayouts.delete(key);
      const map = this.mapsByHandle.get(key);
      if (map) map.close();
      this.mapsByHandle.delete(key);
      const channel = this.channelsByHandle.get(key);
      if (channel) channel.close();
      this.channelsByHandle.delete(key);
      this.functionsByHandle.delete(key);
    };
    this.storageOwner = {
      heap: this.guestMemory.managedHeap,
      // Collection-owned containers pin their layouts after construction has
      // registered all descriptors. Keep registration itself unpinned so a
      // failed reserve/build/publish sequence can release every lease.
      registerLayout: (layout) => this.guestMemory.managedHeap.registerLayout(layout),
      retainLayout: (layout) => this.guestMemory.managedHeap.pinLayout(layout),
      releaseLayout: (layout) => this.guestMemory.managedHeap.unpinLayout(layout),
      allocate: (layout, count, logical) => this.allocatePayload(layout, count, logical),
      release: (handle) => {
        for (let index = this.allocationRoots.length - 1; index >= 0; index--) {
          const candidate = this.allocationRoots[index]!;
          if (candidate.slot === handle.slot && candidate.generation === handle.generation)
            this.allocationRoots.splice(index, 1);
        }
        this.guestMemory.managedHeap.release(handle);
      },
      beginAllocation: () => this.beginStorageAllocation(),
      encode: (value, layout, type) => {
        const releaseConstruction = this.beginStorageConstruction();
        try {
          return this.encodePayload(value, layout, type);
        } finally {
          releaseConstruction();
        }
      },
      encodeInto: (value, layout, type, target) => {
        const releaseConstruction = this.beginStorageConstruction();
        try {
          return this.encodePayload(value, layout, type, target);
        } finally {
          releaseConstruction();
        }
      },
      decode: (type, layout, bytes) => this.decodePayload(type, layout, bytes),
      copyAggregate: (value) => {
        this.allocationRoots.push(...this.referencesInValue(value));
        return this.managedCell(value).read();
      },
      beginConstruction: () => this.beginStorageConstruction(),
      retainDynamic: (layout, bytes) => this.retainStorageDynamic(layout, bytes),
      retain: (layout, bytes) =>
        this.allocationRoots.push(
          ...storageReferences(
            layout,
            new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength),
          ),
        ),
    };
    const limits = options.limits;
    const configuredMaxSteps = limits?.maxSteps;
    this.maxSteps =
      configuredMaxSteps === undefined
        ? 100_000_000n
        : configuredMaxSteps === 0 ||
            configuredMaxSteps === 0n ||
            configuredMaxSteps === -1 ||
            configuredMaxSteps === -1n
          ? undefined
          : BigInt(configuredMaxSteps);
    this.maxStepsNumber =
      this.maxSteps !== undefined && this.maxSteps <= BigInt(Number.MAX_SAFE_INTEGER)
        ? Number(this.maxSteps)
        : undefined;
    this.revision = { generation: this.generation, program };
    this.revisions.set(this.generation, this.revision);
    this.maxInputBytes = options.limits?.maxFfiBytes ?? 8 * 1024 * 1024;
    this.maxHostResultBytes = options.limits?.maxFfiResultBytes ?? 8 * 1024 * 1024;
    this.maxImageBytes = options.load?.maxImageBytes ?? DEFAULT_MAX_IMAGE_BYTES;
    this.attachManagedGlobals();
    this.initializeRoot();
    this.collectHeap(true);
  }

  static async create(image: Uint8Array, options: VMOptions): Promise<VirtualMachine> {
    const limits = loadOptions(options);
    const program = await Program.load(image, limits);
    const capabilities = new Set(options.capabilities ?? []);
    const unavailable = program.capabilities.find((capability) => !capabilities.has(capability));
    if (unavailable)
      throw new VMError(
        `host capability is unavailable: ${unavailable}`,
        "capability_unavailable",
        "image.capabilities",
      );
    const machine = new VirtualMachine(program, options);
    if (options.symbols) await machine.loadDebugSymbols(options.symbols);
    return machine;
  }

  private initializeRoot(): void {
    const root = this.revision.program.root;
    const module = this.revision.program.module(root);
    if (!module) fail("root module is unavailable", "image.root", "link_error");
    const initializer = module.functions.get("fn.init");
    if (!initializer) {
      this.initialized.add(root);
      this.ready = true;
      return;
    }
    const scope: Scope = { tasks: new Set() };
    this.scopes.set(0, scope);
    this.initializing.add(root);
    const task = this.createTask(0, initializer, [], []);
    task.frames[0]!.initializing = root;
  }

  private attachManagedGlobals(revision = this.revision): void {
    let count = 0;
    for (const module of revision.program.moduleValues())
      count +=
        module.globals.length + module.constants.filter((value) => value.kind === "slice").length;
    const heap = this.guestMemory.managedHeap;
    const anchor = this.allocatePayload(
      { stride: 8, references: [0] },
      count,
      BigInt(STORAGE_COST.record_header + count * 8),
    );
    revision.constants = anchor;
    let offset = 0;
    for (const module of revision.program.moduleValues()) {
      for (const constant of module.constants) {
        if (constant.kind !== "slice") continue;
        const slice = constant.value as SliceValue;
        if (!slice.storage.root.managedHandle)
          slice.storage = {
            root: this.managedCell(slice.storage.root.read()),
            path: slice.storage.path,
          };
        heap.writeReference(anchor, offset, slice.storage.root.managedHandle!);
        offset += 8;
      }
      for (let index = 0; index < module.globals.length; index++) {
        const global = module.globals[index]!;
        if (!global.managedHandle) module.globals[index] = this.managedCell(global.read());
        heap.writeReference(anchor, offset, module.globals[index]!.managedHandle!);
        offset += 8;
      }
    }
  }

  private async loadDebugSymbols(input: Uint8Array): Promise<void> {
    let bytes = Uint8Array.from(input);
    const maximum = this.options.load?.maxArtifactBytes ?? 64 * 1024 * 1024;
    if (bytes[0] === 0x1f && bytes[1] === 0x8b) {
      if (typeof DecompressionStream !== "function")
        throw new VMError("gzip symbols are unavailable", "invalid_symbols", "symbols");
      const reader = new Blob([bytes.buffer])
        .stream()
        .pipeThrough(new DecompressionStream("gzip"))
        .getReader();
      const chunks: Uint8Array[] = [];
      let length = 0;
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        length += value.byteLength;
        if (length > maximum) {
          await reader.cancel();
          throw new VMError("symbols exceed byte limit", "load_limit", "symbols");
        }
        chunks.push(value);
      }
      bytes = new Uint8Array(length);
      let offset = 0;
      for (const chunk of chunks) {
        bytes.set(chunk, offset);
        offset += chunk.byteLength;
      }
    }
    if (bytes.byteLength > maximum)
      throw new VMError("symbols exceed byte limit", "load_limit", "symbols");
    let symbols: unknown;
    try {
      symbols = JSON.parse(strictTextDecoder.decode(bytes));
    } catch {
      throw new VMError("invalid program symbols", "invalid_symbols", "symbols");
    }
    if (!symbols || typeof symbols !== "object")
      throw new VMError("invalid program symbols", "invalid_symbols", "symbols");
    const document = symbols as {
      format?: unknown;
      version?: unknown;
      program_hash?: unknown;
      packages?: unknown;
    };
    if (
      document.format !== "mini-go-program-symbols" ||
      document.version !== 1 ||
      document.program_hash !== this.revision.program.hash ||
      !document.packages ||
      typeof document.packages !== "object"
    )
      throw new VMError("program symbols do not match the image", "invalid_symbols", "symbols");
    this.symbolProgramHash = this.revision.program.hash;
    for (const [modulePath, unknownPackage] of Object.entries(document.packages)) {
      if (!unknownPackage || typeof unknownPackage !== "object") continue;
      const packageSymbols = unknownPackage as { files?: unknown; functions?: unknown };
      const files = new Map<string, string>();
      if (Array.isArray(packageSymbols.files))
        for (const item of packageSymbols.files) {
          if (!item || typeof item !== "object") continue;
          const file = item as { id?: unknown; path?: unknown };
          if (typeof file.id === "string" && typeof file.path === "string")
            files.set(file.id, file.path);
        }
      if (!Array.isArray(packageSymbols.functions)) continue;
      for (const item of packageSymbols.functions) {
        if (!item || typeof item !== "object") continue;
        const fn = item as {
          id?: unknown;
          name?: unknown;
          locals?: unknown;
          locations?: unknown;
        };
        if (typeof fn.id !== "string") continue;
        const info: FunctionDebugInfo = {
          name: typeof fn.name === "string" && fn.name ? fn.name : fn.id,
          locations: new Map(),
          localNames: new Map(),
        };
        if (Array.isArray(fn.locals))
          for (const unknownLocal of fn.locals) {
            if (!unknownLocal || typeof unknownLocal !== "object") continue;
            const local = unknownLocal as { id?: unknown; name?: unknown };
            if (typeof local.id === "string" && typeof local.name === "string" && local.name)
              info.localNames.set(local.id, local.name);
          }
        if (Array.isArray(fn.locations))
          for (const unknownLocation of fn.locations) {
            if (!unknownLocation || typeof unknownLocation !== "object") continue;
            const location = unknownLocation as { pc?: unknown; points?: unknown };
            if (!Number.isSafeInteger(location.pc) || !Array.isArray(location.points)) continue;
            const points: DebugLocation[] = [];
            for (const unknownPoint of location.points) {
              if (!unknownPoint || typeof unknownPoint !== "object") continue;
              const point = unknownPoint as { file?: unknown; line?: unknown; column?: unknown };
              if (
                typeof point.file === "string" &&
                Number.isSafeInteger(point.line) &&
                Number.isSafeInteger(point.column)
              )
                points.push({
                  file: files.get(point.file) ?? point.file,
                  line: point.line as number,
                  column: point.column as number,
                });
            }
            const prepared = this.revision.program.module(modulePath)?.functions.get(fn.id ?? "");
            const pc = prepared?.wireToExecutablePC[location.pc as number];
            if (points.length && pc !== undefined)
              info.locations.set(pc, [...(info.locations.get(pc) ?? []), ...points]);
          }
        this.debugFunctions.set(`${modulePath}\0${fn.id}`, info);
      }
    }
  }

  start(entryName: string, arguments_: HostValue[]): number {
    if (!this.ready || this.closing)
      throw new VMError("instance is unavailable", "closed", "instance");
    const entry = this.revision.program.entry(entryName);
    if (!entry) throw new VMError(`unknown entry ${entryName}`, "missing_entry", "entry");
    const fn = this.revision.program.function(entry.module, entry.function);
    if (arguments_.length !== fn.signature.params.length)
      throw new VMError("entry argument count mismatch", "invalid_argument", "arguments");
    const values = arguments_.map((value, index) =>
      this.convertValue(importHostValue(value, fn.module.registry), fn.localTypes[index]!),
    );
    const scopeID = this.nextScope++;
    const executionID = scopeID;
    this.scopes.set(scopeID, { tasks: new Set(), execution: executionID });
    this.executions.set(executionID, {
      id: executionID,
      scope: scopeID,
      state: "Running",
      settled: false,
    });
    let task: Task;
    const operationRootCount = this.allocationRoots.length;
    this.allocationRoots.push(...values.flatMap((value) => [...this.referencesInValue(value)]));
    try {
      task = this.createTask(scopeID, fn, values, []);
    } catch (error) {
      this.allocationRoots.length = operationRootCount;
      this.scopes.delete(scopeID);
      this.executions.delete(executionID);
      this.collectHeap(true);
      if (error instanceof VMError && error.code === "collection_required")
        throw new VMError("guest memory budget exceeded", "allocation_limit", "memory");
      throw error;
    }
    this.allocationRoots.length = operationRootCount;
    this.scopes.get(scopeID)!.rootTask = task.id;
    return executionID;
  }

  private createTask(
    scope: number,
    fn: PreparedFunction,
    arguments_: Value[],
    captures: Address[],
    revision: Revision = this.revision,
  ): Task {
    const maxTasks = this.options.limits?.maxTasks ?? 4096;
    if (this.tasks.size >= maxTasks)
      throw new VMError("task limit exceeded", "task_limit", "scheduler");
    const taskMemory = this.reserveControl(BigInt(STORAGE_COST.control_record));
    const task: Task = {
      id: this.nextTask++,
      scope,
      frames: [],
      state: "runnable",
    };
    try {
      this.pushFrame(task, this.createFrame(fn, revision, arguments_, captures, [], false));
      task.memory = taskMemory.commit();
      this.tasks.set(task.id, task);
      this.scopes.get(scope)!.tasks.add(task.id);
      this.runnable.push(task.id);
      return task;
    } catch (error) {
      taskMemory.abort();
      task.memory?.release();
      throw error;
    }
  }

  private localCell(frame: Frame, index: number): Cell {
    const type = frame.fn.localTypes[index];
    if (!type) invalid("local index is invalid", frame.fn.id);
    const slot = frame.locals[index] ?? frame.localBank.read(index) ?? zero(type);
    if (isLocalCell(slot)) return slot;
    const root = this.managedCell(slot);
    frame.locals[index] = root;

    return root;
  }

  private frameLocal(frame: Frame, index: number): LocalSlot {
    const slot = frame.locals[index];
    if (slot) return slot;
    if (frame.fn.cellLocals.has(index)) return this.localCell(frame, index);
    const value = frame.localBank.read(index);
    if (!value) invalid("local index is invalid", frame.fn.id);
    return value;
  }

  private frameSlot(frame: Frame, index: number): Value {
    const value = frame.slotBank.read(index);
    if (!value) invalid("temporary is not initialized", "operand");
    return value;
  }

  private scalarOperand(frame: Frame, operand: readonly [number, number]): ScalarValue {
    return this.scalarOperandAt(frame, operand[0], operand[1]);
  }

  private scalarOperandAt(frame: Frame, kind: number, index: number): ScalarValue {
    const value =
      kind === 0
        ? frame.slotBank.readScalar(index)
        : kind === 1
          ? (frame.fn.module.constants[index]?.value as ScalarValue | undefined)
          : kind === 2
            ? frame.localBank.readScalar(index)
            : undefined;
    if (value === undefined) invalid("scalar operand is not initialized", "operand");
    return value;
  }

  private integerOperandBits(
    frame: Frame,
    operand: readonly [number, number],
    targetOffset: number,
  ): void {
    this.integerOperandBitsAt(frame, operand[0], operand[1], targetOffset);
  }

  private integerOperandBitsAt(
    frame: Frame,
    kind: number,
    index: number,
    targetOffset: number,
  ): void {
    let found = false;
    if (kind === 0)
      found = frame.slotBank.readIntegerBits(index, frame.integerScratch, targetOffset);
    else if (kind === 1) {
      const bits = frame.fn.module.constantIntegerBits;
      frame.integerScratch[targetOffset] = bits[index * 2]!;
      frame.integerScratch[targetOffset + 1] = bits[index * 2 + 1]!;
      found = true;
    } else if (kind === 2)
      found = frame.localBank.readIntegerBits(index, frame.integerScratch, targetOffset);
    if (!found) invalid("integer operand is not initialized", "operand");
  }

  private writeIntegerOutput(frame: Frame, target: number, low: number, high: number): void {
    const bank = (target & LOCAL_OUTPUT) !== 0 ? frame.localBank : frame.slotBank;
    const index = (target & LOCAL_OUTPUT) !== 0 ? target & ~LOCAL_OUTPUT : target;
    if (!bank.writeIntegerBits(index, low, high))
      invalid("integer output storage mismatch", frame.fn.id);
  }

  private writeScalarOutput(frame: Frame, target: number, value: ScalarValue): void {
    const bank = (target & LOCAL_OUTPUT) !== 0 ? frame.localBank : frame.slotBank;
    const index = (target & LOCAL_OUTPUT) !== 0 ? target & ~LOCAL_OUTPUT : target;
    if (!bank.writeScalar(index, value)) invalid("scalar output storage mismatch", frame.fn.id);
  }

  /** Create a stable cell and publish its backing payload in the managed
   * directory. Bytes/string data are read from the managed page. */
  private managedCell(
    value: Value,
    storageLayout = this.storageLayoutForManagedValue(value),
  ): Cell {
    if (storageLayout.dynamics.length === 0) return this.createManagedCell(value, storageLayout);
    const releaseConstruction = this.beginStorageConstruction();
    try {
      return this.createManagedCell(value, storageLayout);
    } finally {
      releaseConstruction();
    }
  }

  private createManagedCell(value: Value, storageLayout: StorageLayout): Cell {
    const storedType = this.ownedType(value.type);
    const sourceLayout = storageLayout;
    storageLayout =
      this.ownedLayouts.get(sourceLayout) ??
      remapStorageLayout(sourceLayout, (type) => this.ownedType(type));
    this.ownedLayouts.set(sourceLayout, storageLayout);
    const initial = value.kind === "bytes" ? (value.value as Uint8Array) : undefined;
    const codec = value.kind !== "bytes";
    const payload = codec ? this.encodePayload(value, storageLayout) : initial;
    const bytes =
      BigInt(STORAGE_COST.record_header) +
      (initial ? BigInt(initial.length) : storageLayout.logical);
    const payloadLength = payload?.byteLength ?? storageLayout.size;
    const handle = this.allocatePayload(
      {
        stride: Math.max(8, payloadLength),
        references: codec ? storageLayout.references : [],
        dynamics: codec ? storageLayout.dynamics : [],
      },
      1,
      bytes,
      payload,
    );
    const kind = value.kind;
    const packedStorage: PackedStorage = {
      handle,
      read: (offset, length) => this.guestMemory.managedHeap.read(handle, offset, length),
      write: (offset, bytes) => this.guestMemory.managedHeap.write(handle, bytes, offset),
    };
    let root: Cell;
    root = cellAccess(
      () => {
        const aggregate =
          codec && (aggregateKind(storedType) ?? (storageLayout.element ? "array" : undefined));
        if (aggregate)
          return {
            type: storedType,
            kind: aggregate,
            value: {
              storage: packedStorage,
              offset: 0,
              layout: storageLayout,
              type: storedType,
              owner: this.storageOwner,
            } satisfies PackedAggregate,
          };
        if (codec)
          return this.decodePayload(
            storedType,
            storageLayout,
            this.guestMemory.managedHeap.read(handle, 0, payloadLength),
          );
        return {
          type: storedType,
          kind,
          value: this.guestMemory.managedHeap.read(handle, 0, payloadLength),
        };
      },
      (next) => {
        const releaseConstruction = this.beginStorageConstruction();
        try {
          const encoded = codec
            ? this.encodePayload(next, storageLayout, storedType)
            : (next.value as Uint8Array);
          if (encoded.byteLength !== payloadLength)
            invalid("cell assignment changes its storage layout", "memory.cell");
          this.guestMemory.managedHeap.write(handle, encoded);
        } finally {
          releaseConstruction();
        }
      },
    );
    root.managedHandle = handle;
    root.managedLocation = { handle, offset: 0, type: storedType, layout: storageLayout };
    root.readLocation = (location) => {
      if (location.handle.slot !== handle.slot || location.handle.generation !== handle.generation)
        return undefined;
      const aggregate =
        aggregateKind(location.type) ?? (location.layout.element ? "array" : undefined);
      if (aggregate)
        return {
          type: location.type,
          kind: aggregate,
          value: {
            storage: packedStorage,
            offset: location.offset,
            layout: location.layout,
            type: location.type,
            owner: this.storageOwner,
          } satisfies PackedAggregate,
        };
      return this.decodePayload(
        location.type,
        location.layout,
        this.guestMemory.managedHeap.read(handle, location.offset, location.layout.size),
      );
    };
    root.writeLocation = (location, next) => {
      if (location.handle.slot !== handle.slot || location.handle.generation !== handle.generation)
        return false;
      const releaseConstruction = this.beginStorageConstruction();
      try {
        const encoded = this.encodePayload(next, location.layout, location.type);
        this.guestMemory.managedHeap.write(handle, encoded, location.offset);
        return true;
      } finally {
        releaseConstruction();
      }
    };
    this.managedStorageLayouts.set(`${handle.slot}:${handle.generation}`, {
      layout: storageLayout,
      type: storedType,
    });
    (this.managedCellsByHandle ?? (this.managedCellsByHandle = new Map())).set(
      `${handle.slot}:${handle.generation}`,
      root,
    );
    if (this.managedCells) this.managedCells.set(root, root.managedHandle);
    return root;
  }

  private storageLayoutForManagedValue(value: Value): StorageLayout {
    if (value.kind === "bytes")
      return prepareArrayStorageLayout(
        primitive(value.type.registry, PrimitiveKind.PrimitiveUint8),
        (value.value as Uint8Array).length,
      );
    if (
      value.kind === "array" &&
      value.type.registry.underlying(value.type).ref.kind === Kind.Slice
    )
      return prepareArrayStorageLayout(
        this.elementType(value.type),
        packedLength(value) ?? (value.value as Value[]).length,
      );
    const cached = this.storageLayouts.get(value.type);
    if (cached) return cached;
    const layout = prepareStorageLayout(value.type);
    this.storageLayouts.set(value.type, layout);
    return layout;
  }

  private storageLayout(type: RuntimeType): StorageLayout {
    const cached = this.storageLayouts.get(type);
    if (cached) return cached;
    const layout = prepareStorageLayout(type);
    this.storageLayouts.set(type, layout);
    return layout;
  }

  private ownedType(type: RuntimeType): RuntimeType {
    let entries = this.ownedTypes.get(type.registry);
    const key = type.registry.key(type);
    const known = entries?.get(key);
    if (known) return known;
    const closure = detachTypeClosure(type);
    for (const [source, detached] of closure.entries) {
      let sourceEntries = this.ownedTypes.get(source.registry);
      if (!sourceEntries) this.ownedTypes.set(source.registry, (sourceEntries = new Map()));
      sourceEntries.set(source.registry.key(source), detached);
      let detachedEntries = this.ownedTypes.get(detached.registry);
      if (!detachedEntries) this.ownedTypes.set(detached.registry, (detachedEntries = new Map()));
      detachedEntries.set(detached.registry.key(detached), detached);
    }
    entries = this.ownedTypes.get(type.registry);
    return entries?.get(key) ?? closure.root;
  }

  private storageTypeID(type: RuntimeType): number {
    type = this.ownedType(type);
    let registryID = this.storageRegistryIDs.get(type.registry);
    if (registryID === undefined) {
      registryID = this.nextStorageRegistryID++;
      this.storageRegistryIDs.set(type.registry, registryID);
    }
    const key = `${registryID}:${type.registry.key(type)}`;
    const known = this.storageTypeIDs.get(key);
    if (known !== undefined) {
      if (this.storageConstructionDepth > 0) {
        this.storageConstructionPins.set(known, (this.storageConstructionPins.get(known) ?? 0) + 1);
        const owner = this.storageConstructionOwners[this.storageConstructionOwners.length - 1];
        if (owner) owner.set(known, (owner.get(known) ?? 0) + 1);
      }
      return known;
    }
    const id = this.freeStorageTypeIDs.pop() ?? this.storageTypes.length + 1;
    if (id > 0xffff_ffff) fail("storage type index overflow", "memory.type", "allocation_limit");
    this.storageTypes[id - 1] = type;
    this.storageTypeKeys[id - 1] = key;
    this.storageTypeIDs.set(key, id);
    if (this.storageConstructionDepth > 0) {
      this.storageConstructionPins.set(id, (this.storageConstructionPins.get(id) ?? 0) + 1);
      const owner = this.storageConstructionOwners[this.storageConstructionOwners.length - 1];
      if (owner) owner.set(id, (owner.get(id) ?? 0) + 1);
    }
    return id;
  }

  private beginStorageConstruction(): () => void {
    this.storageConstructionDepth++;
    const owned = new Map<number, number>();
    this.storageConstructionOwners.push(owned);
    return () => {
      const owner = this.storageConstructionOwners.pop();
      if (owner)
        for (const [id, ownedCount] of owner) {
          const count = this.storageConstructionPins.get(id) ?? 0;
          if (count <= ownedCount) this.storageConstructionPins.delete(id);
          else this.storageConstructionPins.set(id, count - ownedCount);
        }
      this.storageConstructionDepth--;
    };
  }

  /**
   * Container mutation scope for allocations made while encoding a pending
   * entry. Nested allocations are merged into the parent on commit; aborting
   * releases only handles created by this scope, so borrowed input roots stay
   * untouched.
   */
  private beginStorageAllocation(): { commit(): void; abort(): void } {
    const scope = { handles: [] as GuestHandle[], done: false };
    this.storageAllocationScopes.push(scope);
    const finish = (commit: boolean): void => {
      if (scope.done) return;
      scope.done = true;
      const current = this.storageAllocationScopes.pop();
      if (current !== scope) invalid("storage allocation scopes are unbalanced", "memory.scope");
      if (commit) {
        const parent = this.storageAllocationScopes[this.storageAllocationScopes.length - 1];
        parent?.handles.push(...scope.handles);
        return;
      }
      for (let index = scope.handles.length - 1; index >= 0; index--) {
        const handle = scope.handles[index]!;
        if (this.guestMemory.managedHeap.has(handle)) this.storageOwner.release(handle);
      }
    };
    return { commit: () => finish(true), abort: () => finish(false) };
  }

  private retainStorageDynamic(layout: StorageLayout, bytes: Uint8Array): () => void {
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const ids = new Map<number, number>();
    for (const offset of layout.dynamics) {
      const id = view.getUint32(offset, true);
      if (!id) continue;
      ids.set(id, (ids.get(id) ?? 0) + 1);
      this.storageDataPins.set(id, (this.storageDataPins.get(id) ?? 0) + 1);
    }
    return () => {
      for (const [id, count] of ids) {
        const current = this.storageDataPins.get(id) ?? 0;
        if (current <= count) this.storageDataPins.delete(id);
        else this.storageDataPins.set(id, current - count);
      }
    };
  }

  private storageType(id: number): RuntimeType {
    const type = this.storageTypes[id - 1];
    if (!type) invalid("dynamic storage type is unavailable", "memory.type");
    return type;
  }

  private storageBox(handle: GuestHandle): Value {
    const cell = this.managedCellsByHandle.get(`${handle.slot}:${handle.generation}`);
    if (!cell) invalid("dynamic storage box is unavailable", "memory.handle");
    return cell.read();
  }

  private collectionReferenceHandle(value: Value | undefined): GuestHandle | undefined {
    if (!value) return undefined;
    switch (value.kind) {
      case "pointer":
        return (value.value as Address).root.managedHandle;
      case "slice":
        return (value.value as SliceValue).storage.root.managedHandle;
      case "map":
        return this.mapHandles.get(value.value as object);
      case "channel":
        return this.channelHandles.get(value.value as object);
      case "function":
      case "method":
      case "dynamicFunction":
        return this.storeFunction(value);
      case "string":
      case "bytes":
      case "bool":
      case "int":
      case "uint":
      case "float":
      case "complex":
      case "array":
      case "struct": {
        const object = value as object;
        const existing = this.managedValueHandles.get(object);
        if (existing && this.guestMemory.managedHeap.has(existing)) return existing;
        const handle = this.managedCell(value).managedHandle;
        if (handle) this.managedValueHandles.set(object, handle);
        return handle;
      }
      default:
        return undefined;
    }
  }

  private *referencesInValue(
    value: Value,
    seen = new Set<object>(),
  ): IterableIterator<GuestHandle> {
    const packed = packedAggregate(value);
    if (packed) {
      if (packed.storage.handle) {
        yield packed.storage.handle;
      } else {
        const bytes = packed.storage.read(packed.offset, packed.layout.size);
        yield* storageReferences(
          packed.layout,
          new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength),
        );
      }
      return;
    }
    const keep = (handle: GuestHandle | undefined): GuestHandle[] =>
      handle && this.guestMemory.managedHeap.has(handle) ? [handle] : [];
    switch (value.kind) {
      case "pointer":
        yield* keep((value.value as Address).root.managedHandle);
        return;
      case "slice":
        yield* keep((value.value as SliceValue).storage.root.managedHandle);
        return;
      case "map": {
        const handle = this.mapHandles.get(value.value as object);
        yield* keep(handle);
        return;
      }
      case "channel": {
        const handle = this.channelHandles.get(value.value as object);
        yield* keep(handle);
        return;
      }
      case "function":
      case "dynamicFunction":
      case "method": {
        const handle = this.functionHandles.get(value.value as object);
        yield* keep(handle);
        if (value.kind === "method") {
          const method = value.value as { function: Value; receiver?: Value };
          yield* this.referencesInValue(method.function, seen);
          if (method.receiver) yield* this.referencesInValue(method.receiver, seen);
        }
        return;
      }
      case "string":
      case "bytes": {
        const storage = this.stringStorage.get(value.value as Uint8Array);
        yield* keep(storage?.handle);
        return;
      }
      case "interface":
        yield* this.referencesInValue(value.value as Value, seen);
        return;
      case "array":
        if (seen.has(value.value as object)) return;
        seen.add(value.value as object);
        for (const item of packedItems(value) ?? (value.value as Value[]))
          yield* this.referencesInValue(item, seen);
        return;
      case "struct":
        if (seen.has(value.value as object)) return;
        seen.add(value.value as object);
        for (const [, item] of packedFields(value) ?? (value.value as Map<string, Value>))
          yield* this.referencesInValue(item, seen);
        return;
      default:
        return;
    }
  }

  private storeFunction(value: Value): GuestHandle {
    const releaseConstruction = this.beginStorageConstruction();
    try {
      return this.createFunctionRecord(value);
    } finally {
      releaseConstruction();
    }
  }

  private createFunctionRecord(value: Value): GuestHandle {
    const fn =
      value.kind === "dynamicFunction"
        ? (value.value as { callback: FunctionValue }).callback
        : (value.value as FunctionValue);
    const identity = value.value as object;
    const existing = this.functionHandles.get(identity);
    if (existing && this.guestMemory.managedHeap.has(existing)) return existing;
    const registry = value.type.registry;
    const dynamicType = this.ownedType(
      registry.runtime({
        kind: Kind.Any,
        primitive: 0,
        named: emptyKey(),
        node: "",
      }),
    );
    const captures = fn.captures.map((address) =>
      this.ownedType(
        this.dynamicType(registry, {
          kind: Kind.Pointer,
          elem: dereference(address).type,
        }),
      ),
    );
    const size = captures.length * 24 + 32;
    const bytes = new Uint8Array(size);
    for (let index = 0; index < captures.length; index++) {
      const type = captures[index]!;
      bytes.set(
        this.encodePayload(
          { type, kind: "pointer", value: fn.captures[index] },
          this.storageLayout(type),
        ),
        index * 24,
      );
    }
    if (fn.receiver)
      bytes.set(
        this.encodePayload(
          { type: dynamicType, kind: "interface", value: fn.receiver },
          this.storageLayout(dynamicType),
        ),
        captures.length * 24,
      );
    const constants = fn.revision ? this.revisions.get(fn.revision)?.constants : undefined;
    if (constants) {
      const view = new DataView(bytes.buffer);
      view.setUint32(size - 8, constants.slot, true);
      view.setUint32(size - 4, constants.generation, true);
    }
    const handle = this.allocatePayload(
      {
        stride: size,
        references: [...captures.map((_, index) => index * 24), size - 8],
        dynamics: [captures.length * 24],
      },
      1,
      BigInt(STORAGE_COST.record_header + STORAGE_COST.reference) +
        BigInt(captures.length) * BigInt(STORAGE_COST.reference) +
        BigInt(fn.receiver ? STORAGE_COST.dynamic : 0),
      bytes,
    );
    this.functionsByHandle.set(`${handle.slot}:${handle.generation}`, {
      module: fn.module,
      function: fn.function,
      revision: fn.revision,
      kind: value.kind,
      captures,
      receiver: fn.receiver !== undefined,
      dynamicType,
    });
    this.functionHandles.set(identity, handle);
    return handle;
  }

  private loadFunction(handle: GuestHandle, type: RuntimeType): Value {
    const record = this.functionsByHandle.get(`${handle.slot}:${handle.generation}`);
    if (!record) invalid("function record is unavailable", "memory.handle");
    const heap = this.guestMemory.managedHeap;
    const captures = record.captures.map(
      (type, index) =>
        this.decodePayload(type, this.storageLayout(type), heap.read(handle, index * 24, 24))
          .value as Address,
    );
    const fn: FunctionValue = {
      module: record.module,
      function: record.function,
      revision: record.revision,
      captures,
    };
    if (record.receiver)
      fn.receiver = this.decodePayload(
        record.dynamicType,
        this.storageLayout(record.dynamicType),
        heap.read(handle, captures.length * 24, 24),
      ).value as Value;
    const payload = record.kind === "dynamicFunction" ? { callback: fn } : fn;
    this.functionHandles.set(payload, handle);
    return { type, kind: record.kind, value: payload };
  }

  private setLocal(frame: Frame, index: number, value: Value): void {
    if (index < 0 || index >= frame.locals.length) invalid("local index is invalid", frame.fn.id);
    const slot = frame.locals[index];
    if (slot) slot.write(this.coerceForStorage(value, frame.fn.localTypes[index]!));
    else if (frame.fn.cellLocals.has(index)) frame.locals[index] = this.managedCell(value);
    else if (
      !frame.localBank.write(index, this.coerceForStorage(value, frame.fn.localTypes[index]!))
    )
      invalid("local storage type mismatch", frame.fn.id);
  }

  private clearFrameSlot(frame: Frame, slot: number): void {
    frame.slotBank.clear(slot);
  }

  private mapIterators(frame: Frame): MapIterators {
    return (frame.mapIterators ??= []);
  }

  private reserveControl(bytes: bigint): GuestControlReservation {
    try {
      this.injectAllocationFault("reserve");
      return this.guestMemory.reserveControl(bytes);
    } catch (error) {
      if (!(error instanceof VMError) || error.code !== "collection_required") throw error;
      this.collectHeap(true);
      try {
        return this.guestMemory.reserveControl(bytes);
      } catch (retryError) {
        if (retryError instanceof VMError && retryError.code === "collection_required")
          throw new VMError("guest memory budget exceeded", "allocation_limit", "memory");
        throw retryError;
      }
    }
  }

  private reserveResident(bytes: bigint): { release(): void } {
    try {
      return this.guestMemory.managedHeap.reserveResident(bytes);
    } catch (error) {
      if (!(error instanceof VMError) || error.code !== "allocation_limit") throw error;
      this.collectHeap(true);
      return this.guestMemory.managedHeap.reserveResident(bytes);
    }
  }

  private setReturning(frame: Frame, values: readonly Value[]): void {
    this.releaseReturning(frame);
    const types = this.preparedFrameLayout(frame.fn).resultTypes;
    if (values.length > types.length) invalid("return count exceeds signature", frame.fn.id);
    // The result region belongs to this frame activation and stays rooted
    // through deferred calls. Only its payload is reset between activations.
    frame.returning = frame.resultBank;
    frame.returningCount = values.length;
    for (let index = 0; index < values.length; index++) {
      const value = this.coerceForStorage(values[index]!, types[index]!);
      if (!frame.resultBank.write(index, value)) invalid("return storage mismatch", frame.fn.id);
    }
  }

  private releaseReturning(frame: Frame): void {
    frame.resultBank.reset();
    delete frame.returning;
    delete frame.returningCount;
  }

  private releaseMapIterator(iterator: MapIteratorState): void {
    iterator.memory?.release();
    delete iterator.memory;
    iterator.resident?.release();
    delete iterator.resident;
  }

  private releaseMapIterators(frame: Frame): void {
    for (const iterator of frame.mapIterators ?? [])
      if (iterator) this.releaseMapIterator(iterator);
    frame.mapIterators?.splice(0);
  }

  private preparedFrameLayout(fn: PreparedFunction): PreparedFrameLayout {
    const cached = this.preparedFrameLayouts.get(fn);
    if (cached) return cached;
    const locals = fn.localTypes.map((type) => this.storageLayout(type));
    const slots = fn.slotTypes.map((type) => this.storageLayout(type));
    const resultTypes = fn.signature.results.map((type) => fn.module.registry.runtime(type));
    const results = resultTypes.map((type) => this.storageLayout(type));
    const logical =
      locals.reduce(
        (bytes, layout, index) =>
          bytes + (fn.cellLocals.has(index) ? 8n : FrameStorage.logicalByteLength([layout])),
        0n,
      ) +
      FrameStorage.logicalByteLength(slots) +
      FrameStorage.logicalByteLength(results);
    const prepared = { locals, slots, results, resultTypes, logical } satisfies PreparedFrameLayout;
    this.preparedFrameLayouts.set(fn, prepared);
    return prepared;
  }

  private acquireFrameStorage(fn: PreparedFunction): ReusableFrameStorage | undefined {
    const bucket = this.frameCacheByFunction.get(fn);
    const storage = bucket?.pop();
    if (!storage) return undefined;
    if (bucket?.length === 0) this.frameCacheByFunction.delete(fn);
    const index = storage.cacheIndex;
    const last = this.frameCache.pop()!;
    if (last !== storage) {
      this.frameCache[index] = last;
      last.cacheIndex = index;
    }
    this.frameCacheBytes -= storage.bytes;
    return storage;
  }

  private recycleFrame(frame: Frame): void {
    this.releaseReturning(frame);
    this.releaseMapIterators(frame);
    for (const memory of frame.deferMemory ?? []) memory.release();
    frame.deferMemory?.splice(0);
    frame.defers?.splice(0);
    frame.memory.release();
    frame.inputScratch.length = 0;
    frame.locals.length = 0;
    frame.upvalues.length = 0;
    frame.localBank.reset();
    frame.slotBank.reset();
    const limit = this.options.limits?.maxFrameCacheBytes ?? 8 * 1024 * 1024;
    const bytes = frame.storageBytes;
    if (this.closing || this.closed || bytes > limit || this.frameCacheBytes + bytes > limit) {
      frame.resident.release();
      return;
    }
    const storage: ReusableFrameStorage = {
      fn: frame.fn,
      locals: frame.locals,
      upvalues: frame.upvalues,
      localBank: frame.localBank,
      slotBank: frame.slotBank,
      resultBank: frame.resultBank,
      inputScratch: frame.inputScratch,
      integerScratch: frame.integerScratch,
      scratchCapacity: frame.scratchCapacity,
      bytes,
      resident: frame.resident,
      cacheIndex: this.frameCache.length,
    };
    this.frameCache.push(storage);
    let bucket = this.frameCacheByFunction.get(frame.fn);
    if (!bucket) this.frameCacheByFunction.set(frame.fn, (bucket = []));
    bucket.push(storage);
    this.frameCacheBytes += bytes;
  }

  private createFrame(
    fn: PreparedFunction,
    revision: Revision,
    arguments_: Value[],
    captures: Address[],
    returnOutputs: readonly number[],
    deferred: boolean,
  ): Frame {
    const releaseConstruction = this.beginStorageConstruction();
    try {
      return this.createFrameInternal(fn, revision, arguments_, captures, returnOutputs, deferred);
    } finally {
      releaseConstruction();
    }
  }

  private createFrameInternal(
    fn: PreparedFunction,
    revision: Revision,
    arguments_: Value[],
    captures: Address[],
    returnOutputs: readonly number[],
    deferred: boolean,
  ): Frame {
    if (arguments_.length !== fn.signature.params.length)
      throw new VMError("call argument count mismatch", "invalid_call", fn.id);
    if (captures.length !== fn.upvalueTypes.length)
      throw new VMError("closure capture count mismatch", "invalid_call", fn.id);
    const profileKey = this.options.profile ? `${fn.module.path}.${fn.id}` : "";
    const prepared = this.preparedFrameLayout(fn);
    const guestFrameBytes = BigInt(STORAGE_COST.frame_header) + prepared.logical;
    const reservation = this.reserveControl(guestFrameBytes);
    const storage = this.acquireFrameStorage(fn);
    const storageBytes =
      storage?.bytes ??
      FRAME_CACHE_ENTRY_BYTES +
        FrameStorage.byteLength(fn.localTypes, prepared.locals) +
        FrameStorage.byteLength(fn.slotTypes, prepared.slots) +
        FrameStorage.byteLength(prepared.resultTypes, prepared.results) +
        (fn.localTypes.length + fn.upvalueTypes.length + fn.maxInputArity) *
          FRAME_CACHE_VALUE_BYTES +
        6 * Uint32Array.BYTES_PER_ELEMENT;
    let resident;
    try {
      resident = storage?.resident ?? this.reserveResident(BigInt(storageBytes));
    } catch (error) {
      reservation.abort();
      throw error;
    }
    let localBank: FrameStorage | undefined;
    let slotBank: FrameStorage | undefined;
    let resultBank: FrameStorage | undefined;
    try {
      const locals: (Cell | undefined)[] = storage?.locals ?? new Array(fn.localTypes.length);
      const upvalues = storage?.upvalues ?? new Array<Address>(captures.length);
      localBank =
        storage?.localBank ?? new FrameStorage(fn.localTypes, this.storageOwner, prepared.locals);
      slotBank =
        storage?.slotBank ?? new FrameStorage(fn.slotTypes, this.storageOwner, prepared.slots);
      resultBank =
        storage?.resultBank ??
        new FrameStorage(prepared.resultTypes, this.storageOwner, prepared.results);
      for (let index = 0; index < fn.localTypes.length; index++) {
        locals[index] = undefined;
        if (!fn.cellLocals.has(index)) localBank.initializeZero(index);
      }
      for (let index = 0; index < arguments_.length; index++) {
        const value = this.coerceForStorage(arguments_[index]!, fn.localTypes[index]!);
        if (fn.cellLocals.has(index)) locals[index] = this.managedCell(value);
        else if (!localBank.write(index, value)) invalid("frame argument storage mismatch", fn.id);
      }
      for (let index = 0; index < captures.length; index++) upvalues[index] = captures[index]!;
      const memory = reservation.commit();
      const frame: Frame = {
        fn,
        revision,
        profileKey,
        locals,
        localBank,
        upvalues,
        slotBank,
        resultBank,
        inputScratch: storage?.inputScratch ?? [],
        integerScratch: storage?.integerScratch ?? new Uint32Array(6),
        returnOutputs,
        pc: 0,
        recovered: false,
        deferred,
        memory,
        resident,
        storageBytes,
        scratchCapacity: Math.max(storage?.scratchCapacity ?? 0, fn.maxInputArity),
      };
      return frame;
    } catch (error) {
      localBank?.reset();
      slotBank?.reset();
      resultBank?.reset();
      reservation.abort();
      resident.release();
      throw error;
    }
  }

  private pushFrame(task: Task, frame: Frame): void {
    if (task.frames.length >= (this.options.limits?.maxFrames ?? 1024)) {
      this.recycleFrame(frame);
      throw new VMError("frame limit exceeded", "frame_limit", frame.fn.id);
    }
    task.frames.push(frame);
    if (frame.profileKey) {
      const profile = this.functionProfile.get(frame.profileKey) ?? {
        calls: 0n,
        steps: 0n,
        nanos: 0n,
      };
      profile.calls++;
      this.functionProfile.set(frame.profileKey, profile);
    }
  }

  private checkSequence(length: number, path: string): void {
    if (!Number.isSafeInteger(length) || length < 0 || length > 0xffff_ffff)
      throw new VMError("sequence length exceeds representation range", "allocation_limit", path);
  }

  pump(budget: number): VMPumpState {
    if (this.closed) return this.pumpState(false);
    for (const [id, pending] of this.pendingHost)
      if (pending.reply) this.processHostReply(id, pending);
    this.fireTimers();
    if (this.paused) return this.pumpState(false);
    let remaining = Math.max(0, Math.floor(budget));
    while (remaining > 0 && this.runnableHead < this.runnable.length) {
      const id = this.runnable[this.runnableHead++]!;
      const task = this.tasks.get(id);
      if (!task || task.state !== "runnable") continue;
      let executed = 0;
      while (
        remaining > 0 &&
        executed < TASK_INSTRUCTION_QUANTUM &&
        task.state === "runnable" &&
        !this.paused
      ) {
        const fastFrame = task.frames[task.frames.length - 1];
        const fastEnd = fastFrame?.fn.fastRunEnd[fastFrame.pc] ?? 0;
        if (
          fastFrame &&
          fastEnd > fastFrame.pc &&
          this.stepsOverflow === undefined &&
          !this.options.profile &&
          !this.profileTiming &&
          !this.debugConfigured &&
          this.debugStepTask === undefined &&
          !task.chargedInstructionSteps
        ) {
          let fastSteps = 0;
          while (fastFrame.pc < fastEnd && task.state === "runnable") {
            const pc = fastFrame.pc;
            const instruction = fastFrame.fn.instructions[pc]!;
            const cost = instruction.logicalCost;
            if (
              !instruction.fastScalar ||
              cost > remaining - fastSteps ||
              cost > TASK_INSTRUCTION_QUANTUM - executed - fastSteps ||
              (this.maxStepsNumber !== undefined && cost > this.maxStepsNumber - this.steps) ||
              this.steps > Number.MAX_SAFE_INTEGER - cost
            )
              break;
            this.steps += cost;
            fastFrame.pc = pc + 1;
            for (const slot of instruction.operands.releaseBefore)
              this.clearFrameSlot(fastFrame, slot);
            this.executeFastScalar(fastFrame, instruction);
            for (const slot of instruction.operands.release) this.clearFrameSlot(fastFrame, slot);
            fastSteps += cost;
            if (task.frames[task.frames.length - 1] !== fastFrame || fastFrame.pc !== pc + 1) break;
          }
          if (fastSteps) {
            remaining -= fastSteps;
            executed += fastSteps;
            this.allocationRoots.length = 0;
            continue;
          }
        }
        if (!task.chargedInstructionSteps && this.shouldStopBeforeInstruction(task)) break;
        const beforeSteps = this.steps;
        const beforeOverflow = this.stepsOverflow;
        let counted = false;
        try {
          const frame = task.frames[task.frames.length - 1];
          const profileKey = frame?.profileKey ?? "";
          const profileStarted = this.profileTiming && profileKey ? performance.now() : 0;
          try {
            counted = this.step(task);
          } finally {
            if (this.profileTiming && profileKey) {
              const profile = this.functionProfile.get(profileKey)!;
              profile.nanos += BigInt(
                Math.max(0, Math.floor((performance.now() - profileStarted) * 1_000_000)),
              );
            }
          }
        } catch (error) {
          counted = this.steps !== beforeSteps || this.stepsOverflow !== beforeOverflow;
          const runtimeError = toVMError(error);
          if (runtimeError.code === "panic" && task.frames.length) {
            const frame = task.frames[task.frames.length - 1]!;
            frame.panic = this.managedCell(
              stringValue(frame.fn.module.registry, textEncoder.encode(runtimeError.message)),
            );
            this.setReturning(frame, []);
          } else {
            this.failTask(task, runtimeError);
          }
        }
        if (counted) {
          remaining--;
          executed++;
        }
        if (this.debugStepTask === id && this.tasks.has(id) && counted) {
          this.debugStepTask = undefined;
          this.stopForDebugging("step", task);
        }
        this.allocationRoots.length = 0;
      }
      if (task.state === "runnable") this.runnable.push(id);
      if (this.paused) break;
    }
    if (this.runnableHead) {
      this.runnable.splice(0, this.runnableHead);
      this.runnableHead = 0;
    }
    if (this.closing && this.tasks.size === 0 && this.pendingHost.size === 0) {
      this.finishClose();
    }
    return this.pumpState(this.runnable.length > 0);
  }

  private shouldStopBeforeInstruction(task: Task): boolean {
    if (!this.debugConfigured || !task.frames.length) return false;
    const current = task.frames[task.frames.length - 1]!;
    if (current.revision.program.hash !== this.symbolProgramHash) return false;
    const key = `${task.id}:${current.fn.module.path}:${current.fn.id}:${current.pc}`;
    if (this.debugIgnoreBreakpoint === key) {
      this.debugIgnoreBreakpoint = "";
      return false;
    }
    const locations = this.debugFunctions
      .get(`${current.fn.module.path}\0${current.fn.id}`)
      ?.locations.get(current.pc);
    if (
      !locations?.some((location) =>
        this.debugBreakpoints.has(`${current.fn.module.path}\0${location.file}\0${location.line}`),
      )
    )
      return false;
    this.debugIgnoreBreakpoint = key;
    this.stopForDebugging("breakpoint", task);
    return true;
  }

  private stopForDebugging(reason: DebugEvent["reason"], task: Task): void {
    this.paused = true;
    this.epoch++;
    this.debugFrames.clear();
    this.debugVariables.clear();
    this.debugEventsQueue.push({
      event: "stopped",
      body: { reason, threadId: task.id, allThreadsStopped: true },
    });
  }

  private pumpState(running: boolean): VMPumpState {
    let delay: number | undefined;
    const now = performance.now();
    for (const timer of this.timers.values()) {
      if (!timer.canceled) delay = Math.min(delay ?? Infinity, Math.max(0, timer.deadline - now));
    }
    const state: VMPumpState = {
      running,
      ready: this.ready,
      closed: this.closed,
      executions: [...this.executions.values()].map((execution) => {
        const status: VMPumpState["executions"][number] = {
          id: execution.id,
          state: execution.state,
          settled: execution.settled,
        };
        if (execution.scopeError) status.scopeError = execution.scopeError.message;
        return status;
      }),
    };
    if (delay !== undefined) state.delay = delay;
    if (this.initializationError) state.error = this.initializationError.message;
    return state;
  }

  private step(task: Task): boolean {
    if (!task.frames.length) {
      this.completeTask(task, []);
      return false;
    }
    if (
      this.maxSteps !== undefined &&
      (this.maxStepsNumber !== undefined
        ? this.steps >= this.maxStepsNumber
        : (this.stepsOverflow ?? BigInt(this.steps)) >= this.maxSteps)
    )
      throw new VMError("step limit exceeded", "step_limit", "execution");
    const frame = task.frames[task.frames.length - 1]!;
    if (frame.returning) {
      this.finishFrame(task, frame);
      return false;
    }
    if (frame.pc >= frame.fn.instructions.length) {
      this.setReturning(frame, this.namedResults(frame));
      this.finishFrame(task, frame);
      return false;
    }
    frame.rootPC = frame.pc;
    const instruction = frame.fn.instructions[frame.pc]!;
    if (this.stepsOverflow !== undefined) this.stepsOverflow++;
    else if (this.steps === Number.MAX_SAFE_INTEGER) this.stepsOverflow = BigInt(this.steps) + 1n;
    else this.steps++;
    if (frame.profileKey) {
      const profile = this.functionProfile.get(frame.profileKey);
      if (profile) profile.steps++;
    }
    const charged = (task.chargedInstructionSteps ?? 0) + 1;
    if (charged < instruction.logicalCost) {
      task.chargedInstructionSteps = charged;
      return true;
    }
    delete task.chargedInstructionSteps;
    frame.pc++;
    if (frame.profileKey) {
      this.opcodeProfile.set(
        instruction.name,
        (this.opcodeProfile.get(instruction.name) ?? 0n) + 1n,
      );
      this.profileDispatches++;
    }
    for (const slot of instruction.operands.releaseBefore) this.clearFrameSlot(frame, slot);
    try {
      this.execute(task, frame, instruction);
    } finally {
      frame.inputScratch.length = 0;
      if (task.state === "runnable" && task.frames[task.frames.length - 1] === frame)
        delete frame.rootPC;
    }
    for (const slot of instruction.operands.release) this.clearFrameSlot(frame, slot);
    return true;
  }

  private executeFastScalar(frame: Frame, instruction: PreparedInstruction): void {
    const primitive = instruction.scalarPrimitive;
    const operator = instruction.operatorCode!;
    const output = instruction.output0!;
    const input0 = instruction.input0!;
    const input1 = instruction.input1!;
    const input2 = instruction.input2!;
    switch (instruction.opcode) {
      case 4:
        if (isIntegerPrimitive(primitive)) this.writeIntegerOutput(frame, output, 0, 0);
        else
          this.writeScalarOutput(
            frame,
            output,
            primitive === PrimitiveKind.PrimitiveBool ? false : 0,
          );
        return;
      case 6:
        if (isIntegerPrimitive(primitive)) {
          this.integerOperandBitsAt(frame, input0[0], input0[1], 0);
          unaryIntegerBits(
            operator,
            primitive,
            frame.integerScratch[0]!,
            frame.integerScratch[1]!,
            frame.integerScratch,
            4,
          );
          this.writeIntegerOutput(
            frame,
            output,
            frame.integerScratch[4]!,
            frame.integerScratch[5]!,
          );
        } else
          this.writeScalarOutput(
            frame,
            output,
            unaryScalar(operator, primitive!, this.scalarOperandAt(frame, input0[0], input0[1])),
          );
        return;
      case 7:
        if (isIntegerPrimitive(primitive)) {
          this.integerOperandBitsAt(frame, input0[0], input0[1], 0);
          this.integerOperandBitsAt(frame, input1[0], input1[1], 2);
          if (operator >= OperatorCode.Equal && operator <= OperatorCode.GreaterEqual)
            this.writeScalarOutput(
              frame,
              output,
              compareIntegerBits(
                operator,
                primitive,
                frame.integerScratch[0]!,
                frame.integerScratch[1]!,
                frame.integerScratch[2]!,
                frame.integerScratch[3]!,
              ),
            );
          else {
            binaryIntegerBits(
              operator,
              primitive,
              frame.integerScratch[0]!,
              frame.integerScratch[1]!,
              frame.integerScratch[2]!,
              frame.integerScratch[3]!,
              frame.integerScratch,
              4,
            );
            this.writeIntegerOutput(
              frame,
              output,
              frame.integerScratch[4]!,
              frame.integerScratch[5]!,
            );
          }
        } else
          this.writeScalarOutput(
            frame,
            output,
            binaryScalar(
              operator,
              primitive!,
              this.scalarOperandAt(frame, input0[0], input0[1]),
              this.scalarOperandAt(frame, input1[0], input1[1]),
            ),
          );
        return;
      case 8: {
        const local = instruction.local ?? instruction.input0![1];
        if (isIntegerPrimitive(primitive)) {
          if (!frame.localBank.readIntegerBits(local, frame.integerScratch, 0))
            invalid("local integer storage mismatch", frame.fn.id);
          this.writeIntegerOutput(
            frame,
            output,
            frame.integerScratch[0]!,
            frame.integerScratch[1]!,
          );
        } else this.writeScalarOutput(frame, output, frame.localBank.readScalar(local)!);
        return;
      }
      case 9: {
        const local = instruction.local ?? instruction.input0![1];
        if (isIntegerPrimitive(primitive)) {
          this.integerOperandBitsAt(frame, input0[0], input0[1], 0);
          if (
            !frame.localBank.writeIntegerBits(
              local,
              frame.integerScratch[0]!,
              frame.integerScratch[1]!,
            )
          )
            invalid("local integer storage mismatch", frame.fn.id);
        } else if (
          !frame.localBank.writeScalar(local, this.scalarOperandAt(frame, input0[0], input0[1]))
        )
          invalid("local scalar storage mismatch", frame.fn.id);
        return;
      }
      case 15:
        frame.pc = instruction.target!;
        return;
      case 16: {
        const condition = this.scalarOperandAt(frame, input0[0], input0[1]);
        if (condition !== Boolean(instruction.negate)) frame.pc = instruction.target!;
        return;
      }
      case 68: {
        let compared: boolean;
        if (isIntegerPrimitive(primitive)) {
          this.integerOperandBitsAt(frame, input0[0], input0[1], 0);
          this.integerOperandBitsAt(frame, input1[0], input1[1], 2);
          compared = compareIntegerBits(
            operator,
            primitive,
            frame.integerScratch[0]!,
            frame.integerScratch[1]!,
            frame.integerScratch[2]!,
            frame.integerScratch[3]!,
          );
        } else
          compared = compareScalar(
            operator,
            this.scalarOperandAt(frame, input0[0], input0[1]),
            this.scalarOperandAt(frame, input1[0], input1[1]),
          );
        if (compared === Boolean(instruction.when)) frame.pc = instruction.target!;
        return;
      }
      case 73: {
        const local = instruction.local ?? instruction.input0![1];
        if (isIntegerPrimitive(primitive)) {
          this.integerOperandBitsAt(frame, input0[0], input0[1], 0);
          this.integerOperandBitsAt(frame, input1[0], input1[1], 2);
          this.integerOperandBitsAt(frame, input2[0], input2[1], 4);
          binaryIntegerBits(
            instruction.updateOperatorCode!,
            primitive,
            frame.integerScratch[0]!,
            frame.integerScratch[1]!,
            frame.integerScratch[2]!,
            frame.integerScratch[3]!,
            frame.integerScratch,
            0,
          );
          if (
            !frame.localBank.writeIntegerBits(
              local,
              frame.integerScratch[0]!,
              frame.integerScratch[1]!,
            )
          )
            invalid("update comparison local storage mismatch", frame.fn.id);
          if (
            compareIntegerBits(
              operator,
              primitive,
              frame.integerScratch[0]!,
              frame.integerScratch[1]!,
              frame.integerScratch[4]!,
              frame.integerScratch[5]!,
            ) === Boolean(instruction.when)
          )
            frame.pc = instruction.target!;
        } else {
          const updated = binaryScalar(
            instruction.updateOperatorCode!,
            primitive!,
            this.scalarOperandAt(frame, input0[0], input0[1]),
            this.scalarOperandAt(frame, input1[0], input1[1]),
          );
          if (!frame.localBank.writeScalar(local, updated))
            invalid("update comparison local storage mismatch", frame.fn.id);
          if (
            compareScalar(operator, updated, this.scalarOperandAt(frame, input2[0], input2[1])) ===
            Boolean(instruction.when)
          )
            frame.pc = instruction.target!;
        }
        return;
      }
      default:
        invalid("invalid fast scalar opcode", instruction.name);
    }
  }

  private execute(task: Task, frame: Frame, instruction: PreparedInstruction): void {
    // Variable-arity operations and min_max retain the frame scratch buffer.
    // Read their operands once so input0/input1 do not clone the first two
    // values again. The scratch buffer is only valid for this instruction.
    let inputs: Value[] | undefined;
    let input0: Value | undefined;
    let input1: Value | undefined;
    if (instruction.inputScratch) {
      inputs = this.inputs(frame, instruction);
      input0 = inputs[0];
      input1 = inputs[1];
    } else if (instruction.scalarPrimitive === undefined) {
      input0 = instruction.input0 ? this.readOperand(frame, instruction.input0) : undefined;
      input1 = instruction.input1 ? this.readOperand(frame, instruction.input1) : undefined;
    }
    switch (instruction.opcode) {
      case 1:
        this.typeDispatch(frame, instruction.typeDispatch!);
        return;
      case 2:
        this.select(task, frame, instruction);
        return;
      case 3: {
        const constant = instruction.constant!;
        if (isIntegerPrimitive(instruction.scalarPrimitive)) {
          const bits = frame.fn.module.constantIntegerBits;
          this.writeIntegerOutput(
            frame,
            instruction.output0!,
            bits[constant * 2]!,
            bits[constant * 2 + 1]!,
          );
        } else if (instruction.scalarPrimitive !== undefined)
          this.writeScalarOutput(
            frame,
            instruction.output0!,
            frame.fn.module.constants[constant]!.value as ScalarValue,
          );
        else this.outputOne(frame, instruction, frame.fn.module.constants[constant]!);
        return;
      }
      case 4:
        if (isIntegerPrimitive(instruction.scalarPrimitive))
          this.writeIntegerOutput(frame, instruction.output0!, 0, 0);
        else if (instruction.scalarPrimitive !== undefined) {
          const kind = instruction.scalarPrimitive;
          this.writeScalarOutput(
            frame,
            instruction.output0!,
            kind === PrimitiveKind.PrimitiveBool
              ? false
              : kind === PrimitiveKind.PrimitiveFloat32 || kind === PrimitiveKind.PrimitiveFloat64
                ? 0
                : 0n,
          );
        } else
          this.outputOne(
            frame,
            instruction,
            zero(this.outputType(frame, instruction.operands.outputs[0]!)),
          );
        return;
      case 5:
        return;
      case 6:
        if (isIntegerPrimitive(instruction.scalarPrimitive)) {
          this.integerOperandBits(frame, instruction.input0!, 0);
          unaryIntegerBits(
            instruction.operatorCode!,
            instruction.scalarPrimitive,
            frame.integerScratch[0]!,
            frame.integerScratch[1]!,
            frame.integerScratch,
            4,
          );
          this.writeIntegerOutput(
            frame,
            instruction.output0!,
            frame.integerScratch[4]!,
            frame.integerScratch[5]!,
          );
        } else if (instruction.scalarPrimitive !== undefined)
          this.writeScalarOutput(
            frame,
            instruction.output0!,
            unaryScalar(
              instruction.operatorCode!,
              instruction.scalarPrimitive,
              this.scalarOperand(frame, instruction.input0!),
            ),
          );
        else this.outputOne(frame, instruction, unaryCode(instruction.operatorCode!, input0!));
        return;
      case 7:
        if (isIntegerPrimitive(instruction.scalarPrimitive)) {
          this.integerOperandBits(frame, instruction.input0!, 0);
          this.integerOperandBits(frame, instruction.input1!, 2);
          if (
            instruction.operatorCode! >= OperatorCode.Equal &&
            instruction.operatorCode! <= OperatorCode.GreaterEqual
          )
            this.writeScalarOutput(
              frame,
              instruction.output0!,
              compareIntegerBits(
                instruction.operatorCode!,
                instruction.scalarPrimitive,
                frame.integerScratch[0]!,
                frame.integerScratch[1]!,
                frame.integerScratch[2]!,
                frame.integerScratch[3]!,
              ),
            );
          else {
            binaryIntegerBits(
              instruction.operatorCode!,
              instruction.scalarPrimitive,
              frame.integerScratch[0]!,
              frame.integerScratch[1]!,
              frame.integerScratch[2]!,
              frame.integerScratch[3]!,
              frame.integerScratch,
              4,
            );
            this.writeIntegerOutput(
              frame,
              instruction.output0!,
              frame.integerScratch[4]!,
              frame.integerScratch[5]!,
            );
          }
        } else if (instruction.scalarPrimitive !== undefined)
          this.writeScalarOutput(
            frame,
            instruction.output0!,
            binaryScalar(
              instruction.operatorCode!,
              instruction.scalarPrimitive,
              this.scalarOperand(frame, instruction.input0!),
              this.scalarOperand(frame, instruction.input1!),
            ),
          );
        else
          this.outputOne(
            frame,
            instruction,
            binaryCode(instruction.operatorCode!, input0!, input1!),
          );
        return;
      case 8:
      case 9: {
        const index = instruction.local!;
        if (isIntegerPrimitive(instruction.scalarPrimitive)) {
          if (instruction.opcode === 8) {
            if (!frame.localBank.readIntegerBits(index, frame.integerScratch, 0))
              invalid("local integer storage mismatch", frame.fn.id);
            this.writeIntegerOutput(
              frame,
              instruction.output0!,
              frame.integerScratch[0]!,
              frame.integerScratch[1]!,
            );
          } else {
            this.integerOperandBits(frame, instruction.input0!, 0);
            if (
              !frame.localBank.writeIntegerBits(
                index,
                frame.integerScratch[0]!,
                frame.integerScratch[1]!,
              )
            )
              invalid("local integer storage mismatch", frame.fn.id);
          }
        } else if (instruction.scalarPrimitive !== undefined) {
          if (instruction.opcode === 8)
            this.writeScalarOutput(frame, instruction.output0!, frame.localBank.readScalar(index)!);
          else if (
            !frame.localBank.writeScalar(index, this.scalarOperand(frame, instruction.input0!))
          )
            invalid("local scalar storage mismatch", frame.fn.id);
        } else if (instruction.opcode === 8)
          this.outputOne(frame, instruction, localValue(this.frameLocal(frame, index)));
        else if (instruction.rebind && frame.fn.cellLocals.has(index)) {
          const value = this.coerceForStorage(input0!, frame.fn.localTypes[index]!);

          frame.locals[index] = this.managedCell(value);
        } else {
          this.setLocal(frame, index, this.coerceForStorage(input0!, frame.fn.localTypes[index]!));
        }
        return;
      }
      case 10:
      case 11: {
        const index = instruction.upvalue!;
        const address = frame.upvalues[index]!;
        if (instruction.opcode === 10) this.outputOne(frame, instruction, dereference(address));
        else {
          assign(address, this.coerceForStorage(input0!, frame.fn.upvalueTypes[index]!));
        }
        return;
      }
      case 12:
      case 13: {
        const index = instruction.global!;
        if (instruction.opcode === 12)
          this.outputOne(frame, instruction, frame.fn.module.globals[index]!.read());
        else {
          frame.fn.module.globals[index]!.write(
            this.coerceForStorage(input0!, frame.fn.module.globals[index]!.read().type),
          );
        }
        return;
      }
      case 14:
        return;
      case 15:
        frame.pc = instruction.target!;
        return;
      case 16: {
        const condition = this.scalarOperand(frame, instruction.input0!);
        if (typeof condition !== "boolean") invalid("jump condition is not bool", "jump_if");
        if (condition !== Boolean(instruction.negate)) frame.pc = instruction.target!;
        return;
      }
      case 17:
        this.setReturning(frame, inputs!);
        return;
      case 18:
        frame.panic = this.managedCell(
          input0!.kind === "nil"
            ? stringValue(
                frame.fn.module.registry,
                textEncoder.encode("panic called with nil argument"),
              )
            : input0!,
        );
        this.setReturning(frame, []);
        return;
      case 19:
        this.outputOne(frame, instruction, this.recover(task, frame));
        return;
      case 20: {
        const value = input0;
        if (value?.kind !== "function") invalid("deferred value is not callable", "defer");
        const ownerDepth = instruction.ownerDepth ?? 0;
        const owner = task.frames[task.frames.length - ownerDepth - 1];
        if (!owner) invalid("defer owner depth exceeds stack", "defer");
        const memory = this.reserveControl(BigInt(STORAGE_COST.control_record)).commit();
        try {
          (owner.defers ??= []).push(this.managedCell(value));
          (owner.deferMemory ??= []).push(memory);
        } catch (error) {
          memory.release();
          throw error;
        }
        return;
      }
      case 21:
      case 22:
      case 23:
        this.call(task, frame, instruction, inputs!);
        return;
      case 24:
        this.callInterface(task, frame, instruction, inputs!);
        return;
      case 25:
        this.outputOne(frame, instruction, this.makeClosure(frame, instruction));
        return;
      case 26:
        this.outputOne(frame, instruction, this.makeSequence(instruction, inputs!));
        return;
      case 27:
        this.outputOne(frame, instruction, this.makeMap(instruction, inputs!));
        return;
      case 28:
        this.outputOne(frame, instruction, this.makeStruct(instruction, inputs!));
        return;
      case 29:
        this.outputOne(
          frame,
          instruction,
          this.makeSlice(instruction.type!, Boolean(instruction.hasCapacity), inputs!),
        );
        return;
      case 30:
        this.outputOne(frame, instruction, this.makeChannel(instruction, input0!));
        return;
      case 31: {
        const [value] = this.loadIndex(input0!, input1!);
        this.outputOne(frame, instruction, value);
        return;
      }
      case 32: {
        const [value, ok] = this.loadIndex(input0!, input1!);
        this.outputTwo(frame, instruction, value, boolValue(frame.fn.module.registry, ok));
        return;
      }
      case 33: {
        const bytes = expectBytes(input0!, "string_rune_at");
        const index = asIndex(input1!);
        const [rune] = decodeRune(bytes, index);
        this.outputOne(frame, instruction, {
          type: this.outputType(frame, instruction.operands.outputs[0]!),
          kind: "int",
          value: BigInt.asIntN(32, BigInt(rune)),
        });
        return;
      }
      case 34: {
        const bytes = expectBytes(input0!, "string_next_rune_index");
        const index = asIndex(input1!);
        const [, width] = decodeRune(bytes, index);
        this.outputOne(
          frame,
          instruction,
          intValue(frame.fn.module.registry, BigInt(index + width)),
        );
        return;
      }
      case 35:
        this.outputOne(frame, instruction, this.slice(frame, inputs!));
        return;
      case 36:
      case 37:
        this.outputOne(
          frame,
          instruction,
          intValue(
            frame.fn.module.registry,
            BigInt(this.measureLength(input0!, instruction.opcode === 37)),
          ),
        );
        return;
      case 38: {
        const values = inputs!;
        const target = instruction.output0!;
        if ((target & LOCAL_OUTPUT) === 0 && frame.slotBank.read(target) === undefined) {
          // Publish the existing slice as the temporary output root before an
          // append can mutate shared backing. A pressure trace then sees both
          // the reserved root and the source throughout backing allocation.
          this.writeOutput(frame, target, values[0]!);
        }
        this.outputOne(frame, instruction, this.append(Boolean(instruction.expand), values));
        return;
      }
      case 39:
        this.deleteMap(input0!, input1!);
        return;
      case 40:
        this.clear(input0!);
        return;
      case 41:
        this.outputOne(
          frame,
          instruction,
          intValue(frame.fn.module.registry, BigInt(this.copy(input0!, input1!))),
        );
        return;
      case 42:
        this.outputOne(frame, instruction, this.mapKeys(frame, input0!));
        return;
      case 43:
        if (instruction.iteratorLocal === undefined)
          invalid("map iterator local is unavailable", "map_iter_init");
        this.mapIterInit(frame, instruction.iteratorLocal, input0!);
        return;
      case 44:
        this.outputs(frame, instruction.operands.outputs, this.mapIterNext(frame, instruction));
        return;
      case 45:
        {
          const local = instruction.iteratorLocal;
          if (local === undefined) invalid("map iterator local is unavailable", "map_iter_close");
          const iterator = frame.mapIterators?.[local];
          if (iterator) this.releaseMapIterator(iterator);
          if (frame.mapIterators) frame.mapIterators[local] = undefined;
        }
        return;
      case 46:
        this.outputOne(frame, instruction, fieldValue(input0!, instruction.field!));
        return;
      case 47:
        this.storeIndex(input0!, input1!, inputs![2]!);
        return;
      case 48:
        this.storeField(input0!, instruction.field!, input1!);
        return;
      case 49:
      case 50: {
        const type = instruction.type!;
        const asserted = this.assertType(input0!, type);
        if (instruction.opcode === 49) {
          if (!asserted) fail("interface conversion failed", "type_assert", "panic");
          this.outputOne(frame, instruction, asserted);
        } else
          this.outputTwo(
            frame,
            instruction,
            asserted ?? zero(type),
            boolValue(frame.fn.module.registry, Boolean(asserted)),
          );
        return;
      }
      case 51:
        this.outputOne(frame, instruction, this.convertValue(input0!, instruction.type!));
        return;
      case 52:
        this.outputOne(frame, instruction, this.addressOf(frame, instruction.address!));
        return;
      case 53: {
        if (input0!.kind !== "pointer") fail("nil pointer dereference", "load_indirect", "panic");
        this.outputOne(frame, instruction, dereference(input0!.value as Address));
        return;
      }
      case 54: {
        if (input0!.kind !== "pointer") fail("nil pointer dereference", "store_indirect", "panic");
        const address = input0!.value as Address;

        assign(address, input1!);
        return;
      }
      case 55:
        this.send(task, input0!, input1!);
        return;
      case 56:
      case 57:
        this.receive(task, frame, instruction, input0!, instruction.opcode === 57);
        return;
      case 58:
        this.outputOne(
          frame,
          instruction,
          boolValue(frame.fn.module.registry, this.canReceive(input0!)),
        );
        return;
      case 59: {
        const received = this.tryReceive(input0!);
        this.outputTwo(
          frame,
          instruction,
          received?.[0] ?? zero(this.channelElement(input0!.type)),
          boolValue(frame.fn.module.registry, Boolean(received)),
        );
        return;
      }
      case 60:
        this.outputOne(
          frame,
          instruction,
          boolValue(frame.fn.module.registry, this.trySend(input0!, input1!, task)),
        );
        return;
      case 61:
        this.outputOne(
          frame,
          instruction,
          boolValue(frame.fn.module.registry, this.canSend(input0!)),
        );
        return;
      case 62:
        this.closeChannel(input0!);
        this.wakeSelects();
        return;
      case 63:
        this.initializeModule(task, frame, instruction.modulePath!);
        return;
      case 64:
        this.outputOne(
          frame,
          instruction,
          this.loadExport(frame, instruction.exportModule!, instruction.exported!),
        );
        return;
      case 65:
        this.spawn(task, frame, instruction, inputs!);
        return;
      case 66:
        this.callFFI(task, frame, instruction, inputs!);
        return;
      case 67:
        this.intrinsic(task, frame, instruction, instruction.intrinsic!, inputs!);
        return;
      case 68: {
        let compared: boolean;
        if (isIntegerPrimitive(instruction.scalarPrimitive)) {
          this.integerOperandBits(frame, instruction.input0!, 0);
          this.integerOperandBits(frame, instruction.input1!, 2);
          compared = compareIntegerBits(
            instruction.operatorCode!,
            instruction.scalarPrimitive,
            frame.integerScratch[0]!,
            frame.integerScratch[1]!,
            frame.integerScratch[2]!,
            frame.integerScratch[3]!,
          );
        } else
          compared =
            instruction.scalarPrimitive === undefined
              ? compareCode(instruction.operatorCode!, input0!, input1!)
              : compareScalar(
                  instruction.operatorCode!,
                  this.scalarOperand(frame, instruction.input0!),
                  this.scalarOperand(frame, instruction.input1!),
                );
        if (compared === Boolean(instruction.when)) frame.pc = instruction.target!;
        return;
      }
      case 69:
        this.outputOne(
          frame,
          instruction,
          this.getPath(instruction.type!, input0!, instruction.fieldPath!),
        );
        return;
      case 70: {
        const type = instruction.type!;
        const value = this.indexConvert(input0!, input1!, type);
        this.outputOne(frame, instruction, value);
        return;
      }
      case 71: {
        const length = this.measureLength(input0!, false);
        if (compareLength(instruction.operatorCode!, length, input1!) === Boolean(instruction.when))
          frame.pc = instruction.target!;
        return;
      }
      case 72:
        this.outputOne(frame, instruction, minMaxValues(inputs!, instruction.minMax!));
        return;
      case 73: {
        const local = instruction.input0![1];
        if (isIntegerPrimitive(instruction.scalarPrimitive)) {
          this.integerOperandBits(frame, instruction.input0!, 0);
          this.integerOperandBits(frame, instruction.input1!, 2);
          this.integerOperandBits(frame, instruction.input2!, 4);
          binaryIntegerBits(
            instruction.updateOperatorCode!,
            instruction.scalarPrimitive,
            frame.integerScratch[0]!,
            frame.integerScratch[1]!,
            frame.integerScratch[2]!,
            frame.integerScratch[3]!,
            frame.integerScratch,
            0,
          );
          if (
            !frame.localBank.writeIntegerBits(
              local,
              frame.integerScratch[0]!,
              frame.integerScratch[1]!,
            )
          )
            invalid("update comparison local storage mismatch", frame.fn.id);
          if (
            compareIntegerBits(
              instruction.operatorCode!,
              instruction.scalarPrimitive,
              frame.integerScratch[0]!,
              frame.integerScratch[1]!,
              frame.integerScratch[4]!,
              frame.integerScratch[5]!,
            ) === Boolean(instruction.when)
          )
            frame.pc = instruction.target!;
        } else {
          const updated = binaryScalar(
            instruction.updateOperatorCode!,
            instruction.scalarPrimitive!,
            this.scalarOperand(frame, instruction.input0!),
            this.scalarOperand(frame, instruction.input1!),
          );
          if (!frame.localBank.writeScalar(local, updated))
            invalid("update comparison local storage mismatch", frame.fn.id);
          if (
            compareScalar(
              instruction.operatorCode!,
              updated,
              this.scalarOperand(frame, instruction.input2!),
            ) === Boolean(instruction.when)
          )
            frame.pc = instruction.target!;
        }
        return;
      }
      default:
        invalid("unknown opcode", String(instruction.opcode));
    }
  }

  private inputs(frame: Frame, instruction: PreparedInstruction): Value[] {
    const values = frame.inputScratch;
    values.length = 0;
    for (const [kind, index] of instruction.operands.inputs) {
      if (kind === 0) {
        values.push(this.frameSlot(frame, index));
        continue;
      }
      if (kind === 1) {
        const value = frame.fn.module.constants[index];
        if (!value) invalid("constant index is invalid", "operand");
        values.push(value);
        continue;
      }
      if (kind === 2) {
        values.push(localValue(this.frameLocal(frame, index)));
        continue;
      }
      invalid("unknown operand kind", "operand");
    }
    return values;
  }

  private readOperand(frame: Frame, operand: readonly [number, number]): Value {
    const [kind, index] = operand;
    if (kind === 0) {
      return this.frameSlot(frame, index);
    }
    if (kind === 1) {
      const value = frame.fn.module.constants[index];
      if (!value) invalid("constant index is invalid", "operand");
      return value;
    }
    if (kind === 2) {
      return localValue(this.frameLocal(frame, index));
    }
    invalid("unknown operand kind", "operand");
  }

  private outputs(frame: Frame, targets: readonly number[], values: Value[]): void {
    if (targets.length !== values.length)
      invalid(
        `result count mismatch: expected ${targets.length}, received ${values.length}`,
        frame.fn.id,
      );
    // Producers materialize all values before this loop. Direct targets are
    // verifier-checked as distinct private locals, while call results and map
    // entries retain their own child/map owner. The per-write overlap scratch
    // in FrameStorage therefore covers the only aliasing case: a value that
    // already points into its target bank.
    for (let index = 0; index < targets.length; index++)
      this.writeOutput(frame, targets[index]!, values[index]!);
  }

  private outputOne(frame: Frame, instruction: PreparedInstruction, value: Value): void {
    const target = instruction.output0;
    if (instruction.operands.outputs.length !== 1 || target === undefined)
      invalid("result count mismatch", frame.fn.id);
    this.writeOutput(frame, target, value, isFreshScalarProducer(instruction.opcode));
  }

  private outputTwo(
    frame: Frame,
    instruction: PreparedInstruction,
    first: Value,
    second: Value,
  ): void {
    const firstTarget = instruction.output0;
    const secondTarget = instruction.output1;
    if (
      instruction.operands.outputs.length !== 2 ||
      firstTarget === undefined ||
      secondTarget === undefined
    )
      invalid("result count mismatch", frame.fn.id);
    this.writeOutput(frame, firstTarget, first);
    this.writeOutput(frame, secondTarget, second);
  }

  private writeOutput(frame: Frame, target: number, value: Value, fresh = false): void {
    if ((target & LOCAL_OUTPUT) !== 0) {
      const local = target & ~LOCAL_OUTPUT;
      const type = frame.fn.localTypes[local]!;
      this.setLocal(
        frame,
        local,
        fresh && value.type === type && isFreshScalar(value)
          ? value
          : this.coerceForStorage(value, type),
      );
      return;
    }
    if (target >= frame.fn.slotTypes.length) invalid("slot output index is invalid", frame.fn.id);

    const type = frame.fn.slotTypes[target]!;
    const stored =
      fresh && value.type === type && isFreshScalar(value)
        ? value
        : this.coerceForStorage(value, type);
    if (!frame.slotBank.write(target, stored))
      invalid("temporary storage type mismatch", frame.fn.id);
  }

  private outputType(frame: Frame, target: number): RuntimeType {
    if ((target & LOCAL_OUTPUT) !== 0) return frame.fn.localTypes[target & ~LOCAL_OUTPUT]!;
    const type = frame.fn.slotTypes[target];
    if (!type) invalid("output type is unavailable", frame.fn.id);
    return type;
  }

  private namedResults(frame: Frame): Value[] {
    return frame.fn.resultLocals.map((index) => localValue(this.frameLocal(frame, index)));
  }

  private finishFrame(task: Task, frame: Frame): void {
    if (frame.defers?.length) {
      const deferredCell = frame.defers.pop()!;
      const memory = frame.deferMemory?.pop();
      memory?.release();
      this.allocationRoots.push(deferredCell.managedHandle!);
      const deferred = deferredCell.read().value as FunctionValue;
      const revision = this.functionRevision(deferred);
      const fn = revision.program.function(deferred.module, deferred.function);
      this.pushFrame(task, this.createFrame(fn, revision, [], deferred.captures, [], true));
      return;
    }
    if (!frame.returning) this.setReturning(frame, this.namedResults(frame));
    const returning = frame.returning!;
    this.allocationRoots.push(...returning.allReferences());
    let results = returning.values(frame.returningCount);

    frame.memory.release();
    task.frames.pop();
    if (frame.initializing) {
      this.initializing.delete(frame.initializing);
      this.initialized.add(frame.initializing);
    }
    if (frame.panic && !frame.recovered) {
      if (!task.frames.length) {
        this.failTask(task, new VMError(formatPanic(frame.panic.read()), "panic", "panic"));
        this.recycleFrame(frame);
        return;
      }
      const caller = task.frames[task.frames.length - 1]!;
      caller.panic = frame.panic;
      this.setReturning(caller, []);
      this.recycleFrame(frame);
      return;
    }
    if (frame.recovered || frame.fn.resultLocals.length > 0) results = this.namedResults(frame);
    if (frame.deferred) results = [];
    if (!task.frames.length) {
      this.completeTask(task, results);
      this.recycleFrame(frame);
      return;
    }
    const caller = task.frames[task.frames.length - 1]!;
    if (frame.completion?.kind === "reflectCall") {
      const sliceType = this.outputType(caller, frame.completion.outputs[0]!);
      const elementType = this.elementType(sliceType);
      const reflected = results.map((value) =>
        this.reflectValue(caller, elementType, this.ownedView(value)),
      );
      this.outputs(caller, frame.completion.outputs, [
        this.sliceFromValues(sliceType, reflected),
        stringValue(caller.fn.module.registry, new Uint8Array()),
        boolValue(caller.fn.module.registry, true),
      ]);
    } else if (frame.completion?.kind === "dynamicCallback") {
      if (results.length !== 1)
        fail(
          "reflect: MakeFunc callback returned wrong result count",
          "reflect.make_func",
          "panic",
        );
      const reflected = this.sliceValues(results[0]!);
      if (reflected.length !== frame.completion.resultTypes.length)
        fail(
          "reflect: MakeFunc callback returned wrong result count",
          "reflect.make_func",
          "panic",
        );
      const values = reflected.map((value, index) => {
        const current = this.reflectedView(value).current;
        if (!current)
          fail("reflect: MakeFunc callback returned invalid Value", "reflect.make_func", "panic");
        try {
          return this.coerce(
            current,
            frame.completion!.kind === "dynamicCallback"
              ? frame.completion!.resultTypes[index]!
              : current.type,
          );
        } catch (error) {
          fail(
            error instanceof Error ? error.message : String(error),
            "reflect.make_func",
            "panic",
          );
        }
      });
      if (frame.completion.reflectResults) {
        const sliceType = this.outputType(caller, frame.completion.outputs[0]!);
        const elementType = this.elementType(sliceType);
        this.outputs(caller, frame.completion.outputs, [
          this.sliceFromValues(
            sliceType,
            values.map((value) => this.reflectValue(caller, elementType, this.ownedView(value))),
          ),
          stringValue(caller.fn.module.registry, new Uint8Array()),
          boolValue(caller.fn.module.registry, true),
        ]);
      } else this.outputs(caller, frame.completion.outputs, values);
    } else if (frame.tailReturn) this.setReturning(caller, results);
    else this.outputs(caller, frame.returnOutputs, results);
    delete caller.rootPC;
    this.recycleFrame(frame);
  }

  private recover(task: Task, frame: Frame): Value {
    let deferredIndex = task.frames.length - 1;
    if (!frame.deferred) deferredIndex--;
    const deferred = task.frames[deferredIndex];
    const owner = task.frames[deferredIndex - 1];
    if (!deferred?.deferred || !owner)
      return nilValue(
        frame.fn.module.registry.runtime({
          kind: Kind.Any,
          primitive: 0,
          named: emptyKey(),
          node: "",
        }),
      );
    if (!owner.panic || owner.recovered)
      return nilValue(
        frame.fn.module.registry.runtime({
          kind: Kind.Any,
          primitive: 0,
          named: emptyKey(),
          node: "",
        }),
      );
    owner.recovered = true;
    return {
      type: frame.fn.module.registry.runtime({
        kind: Kind.Any,
        primitive: 0,
        named: emptyKey(),
        node: "",
      }),
      kind: "interface",
      value: cloneValue(owner.panic.read()),
    };
  }

  private call(task: Task, frame: Frame, instruction: PreparedInstruction, inputs: Value[]): void {
    const indirect = instruction.opcode === 21;
    const tail = instruction.opcode === 23;
    let arguments_ = inputs;
    let revision: Revision;
    let fn: PreparedFunction;
    let captures: Address[] = EMPTY_CAPTURES;
    if (indirect) {
      const callee = inputs[0];
      arguments_ = inputs.slice(1);
      if (
        !callee ||
        (callee.kind !== "function" &&
          callee.kind !== "method" &&
          callee.kind !== "dynamicFunction")
      )
        fail("call of nil function", "call", "panic");
      if (callee.kind === "dynamicFunction") {
        this.callDynamicFunction(task, frame, instruction, callee, arguments_);
        return;
      }
      const fnValue = callee.value as FunctionValue;
      if (callee.kind === "method" && fnValue.receiver)
        arguments_ = [cloneValue(fnValue.receiver), ...arguments_];
      revision = this.functionRevision(fnValue);
      fn = revision.program.function(fnValue.module, fnValue.function);
      captures = fnValue.captures;
    } else {
      const source = instruction.callTarget!;
      revision = frame.revision;
      fn = source;
    }
    const child = this.createFrame(
      fn,
      revision,
      arguments_,
      captures,
      tail && !frame.defers?.length ? frame.returnOutputs : instruction.operands.outputs,
      false,
    );
    if (tail && !frame.defers?.length) {
      task.frames[task.frames.length - 1] = child;
      if (child.profileKey) {
        const profile = this.functionProfile.get(child.profileKey) ?? {
          calls: 0n,
          steps: 0n,
          nanos: 0n,
        };
        profile.calls++;
        this.functionProfile.set(child.profileKey, profile);
      }
    } else {
      child.tailReturn = tail;
      this.pushFrame(task, child);
    }
  }

  private functionRevision(fn: FunctionValue): Revision {
    const revision = this.revisions.get(fn.revision);
    if (!revision)
      throw new VMError("function revision is no longer available", "stale_revision", "call");
    return revision;
  }

  private callDynamicFunction(
    task: Task,
    frame: Frame,
    instruction: PreparedInstruction,
    callee: Value,
    arguments_: Value[],
    reflectResults = false,
  ): void {
    const dynamic = callee.value as { callback: FunctionValue };
    const signature = this.typeSignature(callee.type);
    if (!signature || signature.params.length !== arguments_.length)
      fail("reflect: dynamic call argument count mismatch", "reflect.make_func", "panic");
    const revision = this.functionRevision(dynamic.callback);
    const callback = revision.program.function(dynamic.callback.module, dynamic.callback.function);
    if (callback.signature.params.length !== 1)
      fail("reflect: invalid MakeFunc callback", "reflect.make_func", "panic");
    const sliceType = callback.module.registry.runtime(callback.signature.params[0]!.type);
    const elementType = this.elementType(sliceType);
    const reflected = arguments_.map((value) =>
      this.reflectValue(frame, elementType, this.ownedView(value)),
    );
    const child = this.createFrame(
      callback,
      revision,
      [this.sliceFromValues(sliceType, reflected)],
      dynamic.callback.captures,
      [],
      false,
    );
    child.completion = {
      kind: "dynamicCallback",
      outputs: instruction.operands.outputs,
      resultTypes: signature.results,
      reflectResults,
    };
    this.pushFrame(task, child);
  }

  private makeClosure(frame: Frame, instruction: PreparedInstruction): Value {
    const captures = instruction.captures!.map((capture) => this.resolveAddress(frame, capture));
    const target = instruction.callTarget!;
    return {
      type: frame.fn.module.registry.runtime({
        kind: Kind.Function,
        primitive: 0,
        named: emptyKey(),
        node: "",
      }),
      kind: "function",
      value: {
        module: target.module.path,
        function: target.id,
        captures,
        revision: frame.revision.generation,
      } satisfies FunctionValue,
    };
  }

  private makeSequence(instruction: PreparedInstruction, inputs: Value[]): Value {
    this.checkSequence(inputs.length, "make_sequence");
    const type = instruction.type!;
    const kind = type.registry.underlying(type).ref.kind;
    if (kind === Kind.Slice) {
      const storage = this.managedCell({ type, kind: "array", value: inputs.map(cloneValue) });
      return {
        type,
        kind: "slice",
        value: {
          storage: { root: storage, path: [] },
          start: 0,
          length: inputs.length,
          capacity: inputs.length,
        } satisfies SliceValue,
      };
    }
    return { type, kind: "array", value: inputs.map(cloneValue) };
  }

  private makeStruct(instruction: PreparedInstruction, inputs: Value[]): Value {
    const type = instruction.type!;
    const node = type.registry.node(type.registry.underlying(type));
    const values = new Map<string, Value>();
    for (const field of node?.fields ?? [])
      values.set(field.name, zero(type.registry.runtime(field.type)));
    const names = instruction.structFields!;
    for (let index = 0; index < names.length; index++)
      values.set(names[index]!, cloneValue(inputs[index]!));
    return { type, kind: "struct", value: values };
  }

  private allocatePayload(
    layout: number | ManagedLayout,
    count: number,
    logical: bigint,
    bytes?: Uint8Array,
  ): GuestHandle {
    const heap = this.guestMemory.managedHeap;
    const layoutID = typeof layout === "number" ? layout : heap.registerLayout(layout);
    // This operation owns its pin through every reserve/collection retry.
    // Container owners retain separate pins for future backing allocations.
    heap.pinLayout(layoutID);
    try {
      if (heap.oldGuestBytes > this.nextMajorBytes) this.collectHeap(true);
      let reservation;
      try {
        this.injectAllocationFault("reserve");
        reservation = heap.reserve(layoutID, count, logical);
      } catch (error) {
        if (
          !(error instanceof VMError) ||
          !["collection_required", "allocation_limit"].includes(error.code)
        )
          throw error;
        this.collectHeap(false);
        try {
          reservation = heap.reserve(layoutID, count, logical);
        } catch (retryError) {
          if (
            !(retryError instanceof VMError) ||
            !["collection_required", "allocation_limit"].includes(retryError.code)
          )
            throw retryError;
          this.collectHeap(true);
          try {
            reservation = heap.reserve(layoutID, count, logical);
          } catch (finalError) {
            if (finalError instanceof VMError && finalError.code === "collection_required")
              throw new VMError("guest memory budget exceeded", "allocation_limit", "memory");
            throw finalError;
          }
        }
      }
      try {
        this.injectAllocationFault("build");
        if (bytes) reservation.bytes().set(bytes);
        this.injectAllocationFault("publish");
        const handle = reservation.commit();
        this.allocationRoots.push(handle);
        const scope = this.storageAllocationScopes[this.storageAllocationScopes.length - 1];
        scope?.handles.push(handle);
        return handle;
      } catch (error) {
        reservation.abort();
        throw error;
      }
    } finally {
      heap.unpinLayout(layoutID);
    }
  }

  private injectAllocationFault(phase: "reserve" | "build" | "publish"): void {
    const fault = this.testAllocationFault;
    if (!fault || fault.phase !== phase || fault.remaining <= 0) return;
    fault.remaining--;
    if (fault.remaining === 0)
      throw new VMError(`injected ${phase} allocation failure`, "injected_fault", "memory");
  }

  private encodePayload(
    value: Value,
    layout: StorageLayout,
    expectedType: RuntimeType = value.type,
    target?: Uint8Array,
  ): Uint8Array {
    const heap = this.guestMemory.managedHeap;
    const bytes = target ?? new Uint8Array(layout.size);
    if (bytes.byteLength < layout.size) invalid("storage target is too small", "memory.layout");
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    encodeStorage(
      value,
      layout,
      bytes,
      0,
      {
        typeID: (type) => this.storageTypeID(type),
        encodeBox: (value) => this.managedCell(value).managedHandle!,
        writeReference: (offset, handle) => {
          if (!handle) return;
          view.setUint32(offset, handle.slot, true);
          view.setUint32(offset + 4, handle.generation, true);
        },
        readReference: () => undefined,
        encodeReference: (nested) => this.collectionReferenceHandle(nested),
        encodeBytes: (payload) => {
          const reflectedKey = this.reflectedTypeBytes.get(payload);
          const existing = this.stringStorage.get(payload);
          if (existing && heap.has(existing.handle)) {
            if (reflectedKey) this.retainReflectedType(existing.handle, reflectedKey);
            return { ...existing, type: value.type };
          }
          const handle = this.allocatePayload(
            { stride: 1 },
            payload.length,
            BigInt(STORAGE_COST.record_header + payload.length),
            payload,
          );
          this.stringStorage.set(payload, { handle, offset: 0 });
          if (reflectedKey) this.retainReflectedType(handle, reflectedKey);
          return { handle, offset: 0, type: value.type };
        },
        encodeAddress: (address) => {
          const location = storageAddress(address);
          if (location) return location;
          const handle = address.root.managedHandle;
          if (!handle) invalid("address has no managed owner", "memory.handle");
          const storage = this.managedStorageLayouts.get(`${handle.slot}:${handle.generation}`);
          if (!storage) invalid("address layout is unavailable", "memory.handle");
          return resolveStorageLocation(
            { handle, offset: 0, type: storage.type },
            storage.layout,
            address.path,
          );
        },
      },
      expectedType,
    );
    return bytes;
  }

  private decodePayload(valueType: RuntimeType, layout: StorageLayout, bytes: Uint8Array): Value {
    const heap = this.guestMemory.managedHeap;
    const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    return decodeStorage(valueType, layout, bytes, 0, {
      typeByID: (id) => this.storageType(id),
      decodeBox: (handle) => this.storageBox(handle),
      writeReference: () => undefined,
      readReference: (offset) => ({
        slot: view.getUint32(offset, true),
        generation: view.getUint32(offset + 4, true),
      }),
      decodeBytes: (location, length) => {
        const bytes = heap.read(location.handle, location.offset, length);
        this.stringStorage.set(bytes, { handle: location.handle, offset: location.offset });
        return bytes;
      },
      decodeAddress: (location) => {
        const key = `${location.handle.slot}:${location.handle.generation}`;
        const root = this.managedCellsByHandle.get(key);
        const storage = this.managedStorageLayouts.get(key);
        if (!root || !storage) invalid("address owner is unavailable", "memory.handle");
        const targetLayout = location.layout ?? prepareStorageLayout(location.type);
        if (
          !Number.isSafeInteger(location.offset) ||
          location.offset < 0 ||
          location.offset + targetLayout.size > storage.layout.size
        )
          invalid(
            `address offset ${location.offset} is invalid for ${storage.type.registry.key(storage.type)} (${storage.layout.size} bytes) targeting ${location.type.registry.key(location.type)}`,
            "memory.handle",
          );
        return {
          root,
          path: [],
          location: {
            handle: location.handle,
            offset: location.offset,
            type: location.type,
            layout: targetLayout,
          },
        };
      },
      resolveValue: (handle, type) => {
        const key = `${handle.slot}:${handle.generation}`;
        const map = this.mapsByHandle.get(key);
        if (map) return { type, kind: "map", value: map };
        const channel = this.channelsByHandle.get(key);
        if (channel) return { type, kind: "channel", value: channel };
        if (this.functionsByHandle.has(key)) return this.loadFunction(handle, type);
        invalid("reference owner is unavailable", "memory.handle");
      },
    });
  }

  private createMap(type: RuntimeType): TypedMap {
    const underlying = type.registry.underlying(type);
    const node = underlying.registry.node(underlying);
    const keyType =
      underlying.dynamic?.key ?? (node?.key ? underlying.registry.runtime(node.key) : undefined);
    const valueType =
      underlying.dynamic?.elem ?? (node?.elem ? underlying.registry.runtime(node.elem) : undefined);
    if (!keyType || !valueType) invalid("map type is missing key or element", "map");
    const ownedKeyType = this.ownedType(keyType);
    const ownedValueType = this.ownedType(valueType);
    const map = new TypedMap(
      this.storageOwner,
      ownedKeyType,
      ownedValueType,
      remapStorageLayout(this.storageLayout(keyType), (item) => this.ownedType(item)),
      remapStorageLayout(this.storageLayout(valueType), (item) => this.ownedType(item)),
    );
    this.mapHandles.set(map, map.handle);
    this.mapsByHandle.set(`${map.handle.slot}:${map.handle.generation}`, map);
    return map;
  }

  private makeMap(instruction: PreparedInstruction, inputs: Value[]): Value {
    const type = instruction.type!;
    const map = this.createMap(type);
    const count = instruction.entryCount!;
    this.checkSequence(count, "make_map");
    for (let index = 0; index < count; index++) {
      const key = inputs[index * 2]!;
      const value = inputs[index * 2 + 1]!;
      assertComparable(key);
      map.set(key, value);
    }
    return { type, kind: "map", value: map };
  }

  private makeSlice(type: RuntimeType, hasCapacity: boolean, inputs: Value[]): Value {
    const length = asIndex(inputs[0]!);
    const capacity = hasCapacity ? asIndex(inputs[1]!) : length;
    if (length > capacity) fail("slice length exceeds capacity", "make_slice", "panic");
    this.checkSequence(capacity, "make_slice");
    const element = this.elementType(type);
    const byteSlice =
      element.registry.underlying(element).ref.kind === Kind.Primitive &&
      element.registry.underlying(element).ref.primitive === PrimitiveKind.PrimitiveUint8;
    const backing: Value = byteSlice
      ? { type, kind: "bytes", value: new Uint8Array(capacity) }
      : { type, kind: "array", value: [] };
    return {
      type,
      kind: "slice",
      value: {
        storage: {
          root: this.managedCell(backing, prepareArrayStorageLayout(element, capacity)),
          path: [],
        },
        start: 0,
        length,
        capacity,
      } satisfies SliceValue,
    };
  }

  private createChannel(type: RuntimeType, capacity: number): TypedChannel {
    this.checkSequence(capacity, "make_waitable");
    const element = this.channelElement(type);
    const channel = new TypedChannel(this.storageOwner, this.ownedType(element), capacity);
    this.channelHandles.set(channel, channel.handle);
    this.channelsByHandle.set(`${channel.handle.slot}:${channel.handle.generation}`, channel);
    return channel;
  }

  private makeChannel(instruction: PreparedInstruction, capacity: Value): Value {
    const type = instruction.type!;
    const size = asIndex(capacity);
    this.checkSequence(size, "make_waitable");
    const channel = this.createChannel(type, size);
    return {
      type,
      kind: "channel",
      value: channel,
    };
  }

  private elementType(type: RuntimeType): RuntimeType {
    const element = type.registry.element(type);
    if (!element) invalid("container has no element type", "type");
    return element;
  }

  private loadIndex(object: Value, key: Value): [Value, boolean] {
    if (object.kind === "map") {
      assertComparable(key);
      const entry = (object.value as TypedMap).get(key);
      return [entry ? entry.value : zero(this.elementType(object.type)), Boolean(entry)];
    }
    if (
      object.kind === "nil" &&
      object.type.registry.underlying(object.type).ref.kind === Kind.Map
    ) {
      assertComparable(key);
      return [zero(this.elementType(object.type)), false];
    }
    return [indexValue(object, asIndex(key)), true];
  }

  private indexConvert(object: Value, key: Value, type: RuntimeType): Value {
    const index = asIndex(key);
    if (object.kind === "string" || object.kind === "bytes") {
      const bytes = object.value as Uint8Array;
      if (index >= bytes.length) fail("index out of range", "index", "panic");
      const primitiveKind = type.registry.underlying(type).ref.primitive;
      const raw = BigInt(bytes[index]!);
      if (
        primitiveKind >= PrimitiveKind.PrimitiveInt &&
        primitiveKind <= PrimitiveKind.PrimitiveInt64
      )
        return normalize({ type, kind: "int", value: raw });
      if (
        primitiveKind >= PrimitiveKind.PrimitiveUint &&
        primitiveKind <= PrimitiveKind.PrimitiveUintptr
      )
        return normalize({ type, kind: "uint", value: raw });
    }
    const [value] = this.loadIndex(object, key);
    return this.convertValue(value, type);
  }

  private storeIndex(object: Value, key: Value, value: Value): void {
    if (object.kind === "map") {
      assertComparable(key);

      const map = object.value as TypedMap;
      map.set(key, value);
      return;
    }
    if (
      object.kind === "nil" &&
      object.type.registry.underlying(object.type).ref.kind === Kind.Map
    ) {
      assertComparable(key);
      fail("assignment to entry in nil map", "store_index", "panic");
    }
    const index = asIndex(key);
    setIndex(object, index, value);
  }

  private storeField(object: Value, name: string, value: Value): void {
    if (object.kind === "pointer") object = dereference(object.value as Address);
    if (object.kind !== "struct") invalid("field store targets non-struct", "store_field");
    if (writePackedField(object, name, value)) return;
    const fields = object.value as Map<string, Value>;
    const previous = fields.get(name);
    if (!previous) invalid("unknown field", name);
    fields.set(name, cloneValue(value));
  }

  private slice(frame: Frame, inputs: Value[]): Value {
    let object = inputs[0]!;
    let storage: Address | undefined;
    const low = asIndex(inputs[1]!);
    const high = asIndex(inputs[2]!);
    const maxValue = inputs[3]!;
    const explicitMax =
      maxValue.kind !== "nil" && (maxValue.kind !== "int" || (maxValue.value as bigint) >= 0n);
    const max = explicitMax ? asIndex(maxValue) : undefined;
    if (object.kind === "nil") {
      const kind = object.type.registry.underlying(object.type).ref.kind;
      if (kind === Kind.Slice) {
        if (low !== 0 || high !== 0 || (max !== undefined && max !== 0))
          fail("slice bounds out of range", "slice", "panic");
        return cloneValue(object);
      }
    }
    if (object.kind === "pointer") {
      storage = object.value as Address;
      object = dereference(storage);
    }
    if (object.kind === "string") {
      const bytes = object.value as Uint8Array;
      if (max !== undefined || low > high || high > bytes.length)
        fail("slice bounds out of range", "slice", "panic");
      return { ...object, value: bytes.slice(low, high) };
    }
    if (object.kind === "array" || object.kind === "bytes") {
      const capacity =
        object.kind === "array"
          ? (packedLength(object) ?? (object.value as Value[]).length)
          : (object.value as Uint8Array).length;
      if (low > high || high > (max ?? capacity) || (max ?? capacity) > capacity)
        fail("slice bounds out of range", "slice", "panic");
      const type = this.outputType(
        frame,
        frame.fn.instructions[frame.pc - 1]!.operands.outputs[0]!,
      );
      return {
        type,
        kind: "slice",
        value: {
          storage: storage ?? { root: this.managedCell(object), path: [] },
          start: low,
          length: high - low,
          capacity: (max ?? capacity) - low,
        } satisfies SliceValue,
      };
    }
    if (object.kind === "slice") {
      const original = object.value as SliceValue;
      const limit = max ?? original.capacity;
      if (low > high || high > limit || limit > original.capacity)
        fail("slice bounds out of range", "slice", "panic");
      return {
        ...object,
        value: {
          ...original,
          start: original.start + low,
          length: high - low,
          capacity: limit - low,
        },
      };
    }
    fail("value is not sliceable", "slice", "type_error");
  }

  private measureLength(value: Value, wantCapacity: boolean): number {
    if (value.kind === "pointer") value = dereference(value.value as Address);
    if (value.kind === "nil") {
      const underlying = value.type.registry.underlying(value.type);
      if (underlying.ref.kind === Kind.Pointer) {
        const element = value.type.registry.element(underlying);
        const node = element && element.registry.node(element.registry.underlying(element));
        if (node?.kind === Kind.Array) return node.length;
      }
      return 0;
    }
    if (value.kind === "string" && !wantCapacity) return (value.value as Uint8Array).length;
    if (value.kind === "array") return packedLength(value) ?? (value.value as Value[]).length;
    if (value.kind === "bytes") return (value.value as Uint8Array).length;
    if (value.kind === "slice")
      return wantCapacity
        ? (value.value as SliceValue).capacity
        : (value.value as SliceValue).length;
    if (value.kind === "map" && !wantCapacity) return (value.value as TypedMap).size;
    if (value.kind === "channel")
      return wantCapacity
        ? (value.value as Channel).capacity
        : (value.value as Channel).queue.length;
    invalid("value has no length", wantCapacity ? "cap" : "len");
  }

  private append(expand: boolean, inputs: Value[]): Value {
    const original = inputs[0]!;
    if (original.kind !== "slice" && original.kind !== "nil")
      invalid("append target is not slice", "append");
    const element = this.elementType(original.type);
    const underlyingElement = element.registry.underlying(element);
    const isByteSlice =
      underlyingElement.ref.kind === Kind.Primitive &&
      underlyingElement.ref.primitive === PrimitiveKind.PrimitiveUint8;
    const originalBytes =
      isByteSlice && original.kind === "slice" ? this.byteSliceView(original) : undefined;
    let values: Value[] = [];
    let appendedBytes: Uint8Array | undefined;
    let appendedByte: number | undefined;
    if (expand) {
      const source = inputs[1]!;
      const sourceBytes = isByteSlice ? this.byteSliceView(source) : undefined;
      if (sourceBytes)
        appendedBytes = sourceBytes.bytes.subarray(
          sourceBytes.offset,
          sourceBytes.offset + sourceBytes.length,
        );
      else values = this.sliceValues(source);
    } else {
      values = inputs.slice(1);
      if (isByteSlice) {
        if (values.length === 1) {
          const value = values[0]!;
          if (value.kind === "uint" || value.kind === "int")
            appendedByte = Number(value.value as bigint);
        } else if (values.length > 1) {
          const bytes = new Uint8Array(values.length);
          let valid = true;
          for (let index = 0; index < values.length; index++) {
            const value = values[index]!;
            if (value.kind !== "uint" && value.kind !== "int") {
              valid = false;
              break;
            }
            bytes[index] = Number(value.value as bigint);
          }
          if (valid) appendedBytes = bytes;
        }
      }
    }
    if (
      (appendedBytes || appendedByte !== undefined) &&
      original.kind === "slice" &&
      !originalBytes
    ) {
      if (expand && values.length === 0) values = this.sliceValues(inputs[1]!);
      appendedBytes = undefined;
      appendedByte = undefined;
    }
    const appendLength = appendedBytes?.length ?? (appendedByte === undefined ? values.length : 1);
    if (appendLength === 0) return original;
    let slice: SliceValue;
    if (original.kind === "nil") {
      slice = {
        storage: {
          root: this.managedCell({ type: original.type, kind: "array", value: [] }),
          path: [],
        },
        start: 0,
        length: 0,
        capacity: 0,
      };
    } else slice = original.value as SliceValue;
    const needed = slice.length + appendLength;
    this.checkSequence(needed, "append");
    if (needed > slice.capacity) {
      const capacity = Math.max(needed, Math.max(1, slice.capacity * 2));
      this.checkSequence(capacity, "append capacity");
      const oldBytes =
        (appendedBytes || appendedByte !== undefined) && original.kind === "slice"
          ? originalBytes
          : undefined;
      const old = oldBytes ? undefined : this.sliceValues(original);

      const backing: Value =
        appendedBytes || appendedByte !== undefined
          ? { type: original.type, kind: "bytes", value: new Uint8Array(capacity) }
          : {
              type: original.type,
              kind: "array",
              value: Array.from({ length: capacity }, () => zero(element)),
            };
      if (oldBytes)
        (backing.value as Uint8Array).set(
          oldBytes.bytes.subarray(oldBytes.offset, oldBytes.offset + oldBytes.length),
        );
      else for (let index = 0; index < old!.length; index++) setIndex(backing, index, old![index]!);
      const root = this.managedCell(backing);

      slice = {
        storage: { root, path: [] },
        start: 0,
        length: oldBytes?.length ?? old!.length,
        capacity,
      };
    }
    const result: Value = {
      type: original.type,
      kind: "slice",
      value: { ...slice, length: needed },
    };
    if (appendedBytes || appendedByte !== undefined) {
      const target = this.byteSliceView(result);
      if (!target) invalid("append byte backing is not addressable", "append");
      if (appendedBytes) target.bytes.set(appendedBytes, target.offset + slice.length);
      else target.bytes[target.offset + slice.length] = appendedByte!;
      target.commit?.();
    } else
      for (let index = 0; index < values.length; index++)
        setIndex(result, slice.length + index, values[index]!);

    return result;
  }

  private sliceValues(value: Value): Value[] {
    if (value.kind === "nil") return [];
    if (value.kind === "string")
      return [...(value.value as Uint8Array)].map((byte) => ({
        type: primitive(value.type.registry, PrimitiveKind.PrimitiveUint8),
        kind: "uint" as const,
        value: BigInt(byte),
      }));
    if (value.kind === "array")
      return [...(packedItems(value) ?? (value.value as Value[]))].map(cloneValue);
    if (value.kind !== "slice") invalid("expected slice", "slice");
    const slice = value.value as SliceValue;
    return Array.from({ length: slice.length }, (_, index) => cloneValue(indexValue(value, index)));
  }

  private byteSliceView(
    value: Value,
  ): { bytes: Uint8Array; offset: number; length: number; commit?: () => void } | undefined {
    if (value.kind === "string" || value.kind === "bytes")
      return {
        bytes: value.value as Uint8Array,
        offset: 0,
        length: (value.value as Uint8Array).length,
      };
    if (value.kind !== "slice") return undefined;
    const elementType = this.elementType(value.type);
    const element = elementType.registry.underlying(elementType);
    if (
      element.ref.kind !== Kind.Primitive ||
      element.ref.primitive !== PrimitiveKind.PrimitiveUint8
    )
      return undefined;
    const slice = value.value as SliceValue;
    const location = storageAddress(slice.storage);
    if (!location) invalid("byte slice has no managed backing", "memory.handle");
    const handle = location.handle;
    const offset = location.offset + slice.start;
    const bytes = this.guestMemory.managedHeap.read(handle, offset, slice.capacity);
    return {
      bytes,
      offset: 0,
      length: slice.length,
      commit: () => this.guestMemory.managedHeap.write(handle, bytes, offset),
    };
  }

  private copy(destination: Value, source: Value): number {
    if (destination.kind === "nil") {
      if (destination.type.registry.underlying(destination.type).ref.kind !== Kind.Slice)
        invalid("copy destination is not slice", "copy");
      return 0;
    }
    if (destination.kind !== "slice") invalid("copy destination is not slice", "copy");
    const slice = destination.value as SliceValue;
    const destinationBytes = this.byteSliceView(destination);
    const sourceBytes = this.byteSliceView(source);
    if (destinationBytes && sourceBytes) {
      const count = Math.min(destinationBytes.length, sourceBytes.length);
      destinationBytes.bytes.set(
        sourceBytes.bytes.subarray(sourceBytes.offset, sourceBytes.offset + count),
        destinationBytes.offset,
      );
      destinationBytes.commit?.();
      return count;
    }
    const values = this.sliceValues(source);
    const count = Math.min(slice.length, values.length);

    for (let index = 0; index < count; index++) setIndex(destination, index, values[index]!);
    return count;
  }

  private deleteMap(value: Value, key: Value): void {
    assertComparable(key);
    if (value.kind === "nil") return;
    if (value.kind !== "map") invalid("delete target is not map", "delete");
    (value.value as TypedMap).delete(key);
  }

  private clear(value: Value): void {
    if (value.kind === "nil") return;
    if (value.kind === "map") {
      (value.value as TypedMap).clear();
    } else if (value.kind === "slice") {
      const slice = value.value as SliceValue;
      const element = this.elementType(value.type);
      for (let index = 0; index < slice.length; index++) setIndex(value, index, zero(element));
    } else invalid("clear target is not map or slice", "clear");
  }

  private mapKeys(frame: Frame, value: Value): Value {
    const keys =
      value.kind === "nil"
        ? []
        : [...(value.value as TypedMap).entries()].map((entry) => entry.key);
    const type = frame.fn.slotTypes[frame.fn.instructions[frame.pc - 1]!.operands.outputs[0]!]!;
    return { type, kind: "array", value: keys };
  }

  private mapIterInit(frame: Frame, local: number, value: Value): void {
    const map = value.kind === "nil" ? undefined : (value.value as TypedMap);
    const iterators = this.mapIterators(frame);
    const previous = iterators[local];
    const bytes = BigInt(STORAGE_COST.control_record + (map?.size ?? 0) * 8);
    const reservation = this.reserveControl(bytes);
    let resident: { release(): void } | undefined;
    let entries: Uint32Array;
    try {
      resident = this.reserveResident(bytes);
      entries = map?.snapshot() ?? new Uint32Array();
    } catch (error) {
      resident?.release();
      reservation.abort();
      throw error;
    }
    const memory = reservation.commit();
    if (previous) this.releaseMapIterator(previous);
    iterators[local] = {
      map,
      entries,
      offset: 0,
      memory,
      resident,
    };
  }

  private mapIterNext(frame: Frame, instruction: PreparedInstruction): Value[] {
    const local = instruction.iteratorLocal;
    if (local === undefined) invalid("map iterator local is unavailable", "map_iter_next");
    const iterator = frame.mapIterators?.[local];
    if (!iterator) invalid("map iterator is not initialized", "map_iter_next");
    while (iterator.offset < iterator.entries.length) {
      const index = iterator.entries[iterator.offset++]!;
      const generation = iterator.entries[iterator.offset++]!;
      const current = iterator.map?.entry(index, generation);
      if (current) return [current.key, current.value, boolValue(frame.fn.module.registry, true)];
    }
    const currentInstruction = frame.fn.instructions[frame.pc - 1]!;
    return [
      zero(this.outputType(frame, currentInstruction.operands.outputs[0]!)),
      zero(this.outputType(frame, currentInstruction.operands.outputs[1]!)),
      boolValue(frame.fn.module.registry, false),
    ];
  }

  private addressOf(frame: Frame, plan: PreparedAddressPlan): Value {
    const address = this.resolveAddress(frame, plan);
    const type = this.dynamicType(frame.fn.module.registry, {
      kind: Kind.Pointer,
      elem: plan.targetType,
    });
    return { type, kind: "pointer", value: address };
  }

  private resolveAddress(frame: Frame, plan: PreparedAddressPlan): Address {
    let address: Address;
    if (plan.kind === "local") address = { root: this.localCell(frame, plan.index), path: [] };
    else if (plan.kind === "upvalue") address = frame.upvalues[plan.index]!;
    else if (plan.kind === "global")
      address = { root: frame.fn.module.globals[plan.index]!, path: [] };
    else address = { root: plan.module!.globals[plan.index]!, path: [] };
    for (const segment of plan.path) {
      if (segment.kind === "field") {
        let current = dereference(address!);
        if (current.kind === "interface") current = current.value as Value;
        if (
          current.kind === "nil" &&
          current.type.registry.underlying(current.type).ref.kind === Kind.Pointer
        )
          fail("nil pointer dereference", "address", "panic");
        if (current.kind === "pointer") {
          address = current.value as Address;
        }
        address = advanceAddress(address!, { kind: "field", name: segment.field });
      } else if (segment.kind === "index") {
        let index = asIndex(localValue(this.frameLocal(frame, segment.local)));
        let current = dereference(address!);
        if (current.kind === "pointer") {
          address = current.value as Address;
          current = dereference(address);
        }
        if (current.kind === "slice") {
          const slice = current.value as SliceValue;
          if (index >= slice.length) fail("slice index out of range", "address", "panic");
          address = slice.storage;
          index += slice.start;
        }
        address = advanceAddress(address!, { kind: "index", index });
      } else {
        const current = dereference(address!);
        if (current.kind !== "pointer") fail("nil pointer dereference", "address", "panic");
        address = current.value as Address;
      }
    }
    return address!;
  }

  private assertType(value: Value, type: RuntimeType): Value | undefined {
    const dynamic = value.kind === "interface" ? (value.value as Value) : value;
    if (value.kind === "nil" && this.isInterface(value.type)) return undefined;
    if (type.registry.identical(dynamic.type, type)) return cloneValue(dynamic);
    if (this.isInterface(type) && this.implements(dynamic.type, type))
      return { type, kind: "interface", value: cloneValue(dynamic) };
    return undefined;
  }

  private coerce(value: Value, type: RuntimeType): Value {
    if (type.registry.identical(value.type, type)) return { ...cloneValue(value), type };
    if (this.isInterface(type)) {
      let dynamic = value;
      if (value.kind === "interface") dynamic = value.value as Value;
      if (value.kind === "nil" && this.isInterface(value.type)) return nilValue(type);
      if (!this.implements(dynamic.type, type))
        invalid("dynamic type does not implement interface", "assignment");
      return { type, kind: "interface", value: cloneValue(dynamic) };
    }
    if (value.kind === "nil" && this.isInterface(value.type)) return zero(type);
    const source = value.type.registry.underlying(value.type);
    const destination = type.registry.underlying(type);
    if (
      source.registry.identical(source, destination) ||
      (source.ref.kind === destination.ref.kind &&
        source.ref.kind !== Kind.Primitive &&
        source.ref.kind !== Kind.Named)
    )
      return { ...cloneValue(value), type };
    if (value.kind === "function" && destination.ref.kind === Kind.Function)
      return { ...cloneValue(value), type };
    invalid("value is not assignable", "assignment");
  }

  /**
   * Storage writers already establish value ownership. Avoid materializing an
   * intermediate clone when the prepared destination has the exact type.
   */
  private coerceForStorage(value: Value, type: RuntimeType): Value {
    return value.type.registry.identical(value.type, type) ? value : this.coerce(value, type);
  }

  private convertValue(value: Value, type: RuntimeType): Value {
    if (this.isInterface(type) || value.kind === "function") return this.coerce(value, type);
    const target = type.registry.underlying(type);
    const source = value.type.registry.underlying(value.type);
    if (
      target.ref.kind === Kind.Pointer &&
      source.ref.kind === Kind.Pointer &&
      (value.kind === "pointer" || value.kind === "nil")
    )
      return { ...cloneValue(value), type };
    if (target.ref.kind === Kind.Pointer && value.kind === "slice") {
      const targetElement = type.registry.element(target);
      const targetNode =
        targetElement &&
        targetElement.registry.node(targetElement.registry.underlying(targetElement));
      const slice = value.value as SliceValue;
      if (targetElement && targetNode?.kind === Kind.Array && slice.length >= targetNode.length) {
        return {
          type,
          kind: "pointer",
          value: advanceAddress(slice.storage, {
            kind: "arrayView",
            start: slice.start,
            length: targetNode.length,
            type: targetElement,
          }),
        };
      }
    }
    if (
      target.ref.kind === Kind.Pointer &&
      source.ref.kind === Kind.Slice &&
      value.kind === "nil"
    ) {
      const targetElement = type.registry.element(target);
      const targetNode =
        targetElement &&
        targetElement.registry.node(targetElement.registry.underlying(targetElement));
      if (targetNode?.kind === Kind.Array && targetNode.length === 0) return nilValue(type);
      fail("slice is too short for array pointer conversion", "convert", "panic");
    }
    return convert(value, type, (backing) => this.managedCell(backing));
  }

  private isInterface(type: RuntimeType): boolean {
    const underlying = type.registry.underlying(type);
    return underlying.ref.kind === Kind.Any || underlying.ref.kind === Kind.Interface;
  }

  private implements(source: RuntimeType, target: RuntimeType): boolean {
    const targetNode =
      target.registry.node(target) ?? target.registry.node(target.registry.underlying(target));
    const required = targetNode?.methods ?? [];
    if (!required.length) return true;
    const sourceNode = this.methodNode(source);
    return required.every((method) =>
      sourceNode?.methods.some((candidate) => candidate.name === method.name),
    );
  }

  private methodNode(type: RuntimeType): TypeNode | undefined {
    const direct = type.registry.node(type);
    if (direct?.methods.length) return direct;
    const underlying = type.registry.underlying(type);
    if (underlying.ref.kind === Kind.Pointer) {
      const element = type.registry.element(underlying);
      if (element) return element.registry.node(element);
    }
    return type.registry.node(underlying);
  }

  private typeDispatch(frame: Frame, plan: PreparedTypeDispatch): void {
    const value = localValue(this.frameLocal(frame, plan.subject));
    const dynamic = value.kind === "interface" ? (value.value as Value) : value;
    for (const candidate of plan.cases) {
      if (
        (value.kind === "nil" && !candidate.type) ||
        (value.kind !== "nil" && candidate.type && this.assertType(dynamic, candidate.type))
      ) {
        if (candidate.binding !== undefined) {
          this.setLocal(
            frame,
            candidate.binding,
            candidate.original ? cloneValue(value) : cloneValue(dynamic),
          );
        }
        frame.pc = candidate.target;
        return;
      }
    }
    if (plan.defaultBinding !== undefined)
      this.setLocal(frame, plan.defaultBinding, cloneValue(value));
    frame.pc = plan.defaultTarget;
  }

  private getPath(declaredType: RuntimeType, source: Value, fields: readonly number[]): Value {
    let value = source;
    let type = declaredType;
    for (const index of fields) {
      type = type.registry.underlying(type);
      while (type.ref.kind === Kind.Pointer) {
        if (value.kind !== "pointer") fail("nil pointer dereference", "get_path", "panic");
        value = dereference(value.value as Address);
        type = this.elementType(type);
        type = type.registry.underlying(type);
      }
      const node = type.registry.node(type);
      const field = node?.fields[index];
      if (!field) invalid(`field path is invalid at index ${index}`, "get_path");
      value = fieldValue(value, field.name);
      type = type.registry.runtime(field.type);
    }
    return cloneValue(value);
  }

  private loadExport(frame: Frame, module: PreparedModule, exported: PreparedExport): Value {
    if (exported.kind === "const")
      return cloneValue(
        module.constants[requiredIndex(module.constantIndexes, exported.id, "constant")]!,
      );
    if (exported.kind === "global")
      return cloneValue(
        module.globals[requiredIndex(module.globalIndexes, exported.id, "global")]!.read(),
      );
    if (exported.kind === "function")
      return {
        type:
          exported.type ??
          frame.fn.module.registry.runtime({
            kind: Kind.Function,
            primitive: 0,
            named: emptyKey(),
            node: "",
          }),
        kind: "function",
        value: {
          module: module.path,
          function: exported.id,
          captures: [],
          revision: this.revision.generation,
        } satisfies FunctionValue,
      };
    invalid("unsupported export kind", "load_export");
  }

  private initializeModule(task: Task, caller: Frame, modulePath: string): void {
    if (this.initialized.has(modulePath) || this.initializing.has(modulePath)) return;
    const module = this.revision.program.module(modulePath);
    if (!module) invalid("unknown module", modulePath);
    const fn = module.functions.get("fn.init");
    if (!fn) {
      this.initialized.add(modulePath);
      return;
    }
    this.initializing.add(modulePath);
    const frame = this.createFrame(fn, this.revision, [], [], [], false);
    frame.initializing = modulePath;
    this.pushFrame(task, frame);
    void caller;
  }

  private spawn(task: Task, frame: Frame, instruction: PreparedInstruction, inputs: Value[]): void {
    const maxTasks = this.options.limits?.maxTasks ?? 4096;
    if (this.tasks.size >= maxTasks)
      throw new VMError("task limit exceeded", "task_limit", "scheduler");
    let fnValue: FunctionValue;
    let args = inputs;
    if (!instruction.callTarget) {
      const value = inputs[0];
      if (!value || value.kind !== "function") fail("spawn of nil function", "spawn", "panic");
      fnValue = value.value as FunctionValue;
      args = inputs.slice(1);
    } else {
      const target = instruction.callTarget;
      fnValue = {
        module: target.module.path,
        function: target.id,
        captures: [],
        revision: frame.revision.generation,
      };
    }
    const revision = this.functionRevision(fnValue);
    const fn = revision.program.function(fnValue.module, fnValue.function);
    this.createTask(task.scope, fn, args, fnValue.captures, revision);
  }

  private send(task: Task, channelValue: Value, value: Value): void {
    const channel = this.channel(channelValue);
    if (channel.closed) fail("send on closed channel", "send", "panic");
    const receiverIndex = channel.receivers.findIndex(
      (id) => this.tasks.get(id)?.state === "blocked",
    );
    if (receiverIndex >= 0) {
      const receiver = this.tasks.get(channel.receivers[receiverIndex]!)!;
      const blocked = receiver.blocked;
      if (blocked?.kind === "receive") {
        const frame = receiver.frames[receiver.frames.length - 1]!;
        const values = blocked.reflected
          ? [
              this.reflectValue(
                frame,
                this.outputType(frame, blocked.outputs[0]!),
                this.ownedView(value),
              ),
              boolValue(frame.fn.module.registry, true),
              stringValue(frame.fn.module.registry, new Uint8Array()),
              boolValue(frame.fn.module.registry, true),
            ]
          : [cloneValue(value)];
        if (blocked.receiveOK && !blocked.reflected)
          values.push(boolValue(frame.fn.module.registry, true));
        this.outputs(frame, blocked.outputs, values);
        channel.receivers.splice(receiverIndex, 1);
        this.unblock(receiver);
      }
      this.wakeSelects();
      return;
    }
    if (channel.queue.length < channel.capacity) {
      const queued = cloneValue(value);
      channel.queue.push(queued);

      this.wakeSelects();
      return;
    }

    const queued = this.managedCell(value);
    channel.senders.push({ task: task.id, value: queued });

    this.block(task, { kind: "send", channel, value: queued });
    this.wakeSelects();
  }

  private receive(
    task: Task,
    frame: Frame,
    instruction: PreparedInstruction,
    channelValue: Value,
    receiveOK: boolean,
  ): void {
    const channel = this.channel(channelValue);
    const received = this.tryReceive(channelValue);
    if (received) {
      const values = [received[0]];
      if (receiveOK) values.push(boolValue(frame.fn.module.registry, received[1]));
      this.outputs(frame, instruction.operands.outputs, values);
      return;
    }
    if (channel.closed) {
      const values = [zero(this.channelElement(channelValue.type))];
      if (receiveOK) values.push(boolValue(frame.fn.module.registry, false));
      this.outputs(frame, instruction.operands.outputs, values);
      return;
    }
    channel.receivers.push(task.id);
    this.block(task, {
      kind: "receive",
      channel,
      outputs: instruction.operands.outputs,
      receiveOK,
    });
    this.wakeSelects();
  }

  private tryReceive(channelValue: Value): [Value, boolean] | undefined {
    const channel = this.channel(channelValue);
    if (channel.queue.length) {
      const sender = channel.senders[0];
      const value = channel.queue.shift(sender?.value.read())!;
      if (sender) {
        channel.senders.shift();
        const task = this.tasks.get(sender.task);
        if (task) {
          this.unblock(task);
        }
      }

      this.wakeSelects();
      return [value, true];
    }
    const sender = channel.senders.shift();
    if (sender) {
      this.allocationRoots.push(sender.value.managedHandle!);
      const task = this.tasks.get(sender.task);
      if (task) {
        this.unblock(task);
      }

      this.wakeSelects();
      return [sender.value.read(), true];
    }
    if (channel.closed) return [zero(this.channelElement(channelValue.type)), false];
    return undefined;
  }

  private trySend(channelValue: Value, value: Value, task: Task): boolean {
    const channel = this.channel(channelValue);
    if (channel.closed) fail("send on closed channel", "send", "panic");
    if (
      channel.receivers.some((id) => this.tasks.get(id)?.state === "blocked") ||
      channel.queue.length < channel.capacity
    ) {
      this.send(task, channelValue, value);
      return true;
    }
    return false;
  }

  private canReceive(value: Value): boolean {
    if (value.kind === "nil") return false;
    const channel = this.channel(value);
    return channel.closed || channel.queue.length > 0 || channel.senders.length > 0;
  }

  private canSend(value: Value): boolean {
    if (value.kind === "nil") return false;
    const channel = this.channel(value);
    return (
      !channel.closed && (channel.queue.length < channel.capacity || channel.receivers.length > 0)
    );
  }

  private closeChannel(value: Value): void {
    const channel = this.channel(value);
    if (channel.closed) fail("close of closed channel", "close", "panic");
    channel.closed = true;
    for (const id of channel.receivers.splice(0)) {
      const task = this.tasks.get(id);
      const blocked = task?.blocked;
      if (task && blocked?.kind === "receive") {
        const frame = task.frames[task.frames.length - 1]!;
        const element = zero(this.channelElement(value.type));
        const values = blocked.reflected
          ? [
              this.reflectValue(
                frame,
                this.outputType(frame, blocked.outputs[0]!),
                this.ownedView(element),
              ),
              boolValue(frame.fn.module.registry, false),
              stringValue(frame.fn.module.registry, new Uint8Array()),
              boolValue(frame.fn.module.registry, true),
            ]
          : [element];
        if (blocked.receiveOK && !blocked.reflected)
          values.push(boolValue(frame.fn.module.registry, false));
        this.outputs(frame, blocked.outputs, values);
        this.unblock(task);
      }
    }
    for (const sender of channel.senders.splice(0)) {
      const senderTask = this.tasks.get(sender.task);
      if (senderTask)
        this.failTask(senderTask, new VMError("send on closed channel", "panic", "send"));
    }
  }

  private channel(value: Value): Channel {
    if (value.kind === "nil")
      fail("operation on nil channel would block forever", "channel", "deadlock");
    if (value.kind !== "channel") invalid("expected channel", "channel");
    return value.value as Channel;
  }

  private channelElement(type: RuntimeType): RuntimeType {
    return this.elementType(type);
  }

  private block(task: Task, blocked: NonNullable<Task["blocked"]>): void {
    task.state = "blocked";
    task.blocked = blocked;
  }

  private releaseBlocked(task: Task): void {
    const blocked = task.blocked;
    if (!blocked) return;
    if (blocked.kind === "reflectSelect") blocked.memory?.release();
    if (blocked.kind === "ffi") {
      const pending = this.pendingHost.get(blocked.id);
      if (pending && !pending.cancelSent) {
        pending.cancelSent = true;
        this.pendingActions.push({ kind: "cancel", id: blocked.id });
      }
    }
    delete task.blocked;
  }

  private unblock(task: Task): void {
    if (task.state !== "blocked") return;
    const frame = task.frames[task.frames.length - 1];
    if (frame) delete frame.rootPC;
    task.state = "runnable";
    delete task.blocked;
    this.runnable.push(task.id);
  }

  private select(task: Task, frame: Frame, instruction: PreparedInstruction): void {
    const cases = instruction.selectCases!;
    for (let index = 0; index < cases.length; index++) {
      const selected = cases[index]!;
      const channelIndex = selected.channel;
      const channelValue = localValue(this.frameLocal(frame, channelIndex));
      if (selected.send !== undefined) {
        if (!this.canSend(channelValue)) continue;
        const sendIndex = selected.send;
        this.send(task, channelValue, localValue(this.frameLocal(frame, sendIndex)));
      } else {
        if (!this.canReceive(channelValue)) continue;
        const received = this.tryReceive(channelValue)!;
        if (selected.value !== undefined) {
          const local = selected.value;
          this.setLocal(frame, local, received[0]);
        }
        if (selected.ok !== undefined) {
          const local = selected.ok;
          this.setLocal(frame, local, boolValue(frame.fn.module.registry, received[1]));
        }
      }
      this.setLocal(
        frame,
        instruction.selectIndex!,
        intValue(frame.fn.module.registry, BigInt(index)),
      );
      return;
    }
    if (this.pairSelect(task, frame, instruction, cases)) return;
    if (instruction.selectDefault) {
      this.setLocal(frame, instruction.selectIndex!, intValue(frame.fn.module.registry, -1n));
      return;
    }
    frame.pc--;
    this.block(task, { kind: "select" });
  }

  private wakeSelects(): void {
    for (const task of [...this.tasks.values()]) {
      if (task.blocked?.kind === "select") this.unblock(task);
      else if (task.blocked?.kind === "reflectSelect") {
        const blocked = task.blocked;
        const frame = task.frames[task.frames.length - 1]!;
        for (const item of blocked.cases) {
          if (item.channel) this.allocationRoots.push(item.channel.managedHandle!);
          if (item.send) this.allocationRoots.push(item.send.managedHandle!);
        }
        delete task.blocked;
        task.state = "runnable";
        if (this.completeReflectSelect(task, frame, blocked.outputs, blocked.cases)) {
          blocked.memory?.release();
          this.runnable.push(task.id);
        } else {
          task.state = "blocked";
          task.blocked = blocked;
        }
      }
    }
  }

  private completeReflectSelect(
    task: Task,
    frame: Frame,
    outputs: readonly number[],
    cases: ReflectedSelectCase[],
  ): boolean {
    const selected = (index: number, value: Value | undefined, ok: boolean): boolean => {
      this.outputs(frame, outputs, [
        intValue(frame.fn.module.registry, BigInt(index)),
        value
          ? this.reflectValue(frame, this.outputType(frame, outputs[1]!), this.ownedView(value))
          : zero(this.outputType(frame, outputs[1]!)),
        boolValue(frame.fn.module.registry, ok),
        stringValue(frame.fn.module.registry, new Uint8Array()),
        boolValue(frame.fn.module.registry, true),
      ]);
      return true;
    };
    for (const item of cases) {
      if (!item.channel || item.default) continue;
      if (item.receive) {
        const received = this.tryReceive(item.channel.read());
        if (received) return selected(item.index, received[0], received[1]);
      } else if (item.send && this.trySend(item.channel.read(), item.send.read(), task)) {
        return selected(item.index, undefined, false);
      }
    }
    const fallback = cases.find((item) => item.default);
    return fallback ? selected(fallback.index, undefined, false) : false;
  }

  private pairSelect(
    task: Task,
    frame: Frame,
    instruction: PreparedInstruction,
    cases: readonly PreparedSelectCase[],
  ): boolean {
    for (let index = 0; index < cases.length; index++) {
      const selected = cases[index]!;
      const channelValue = localValue(this.frameLocal(frame, selected.channel));
      if (channelValue.kind !== "channel" || (channelValue.value as Channel).capacity !== 0)
        continue;
      for (const other of this.tasks.values()) {
        if (other.id === task.id || other.blocked?.kind !== "select") continue;
        const otherFrame = other.frames[other.frames.length - 1]!;
        const otherInstruction = otherFrame.fn.instructions[otherFrame.pc];
        if (!otherInstruction || otherInstruction.opcode !== 2) continue;
        const otherCases = otherInstruction.selectCases!;
        for (let otherIndex = 0; otherIndex < otherCases.length; otherIndex++) {
          const candidate = otherCases[otherIndex]!;
          if ((selected.send !== undefined) === (candidate.send !== undefined)) continue;
          const otherChannel = localValue(this.frameLocal(otherFrame, candidate.channel));
          if (otherChannel.kind !== "channel" || otherChannel.value !== channelValue.value)
            continue;
          const senderFrame = selected.send !== undefined ? frame : otherFrame;
          const sender = selected.send !== undefined ? selected : candidate;
          const receiverFrame = selected.send !== undefined ? otherFrame : frame;
          const receiver = selected.send !== undefined ? candidate : selected;
          const sent = localValue(this.frameLocal(senderFrame, sender.send!));
          if (receiver.value !== undefined)
            this.setLocal(receiverFrame, receiver.value, cloneValue(sent));
          if (receiver.ok !== undefined)
            this.setLocal(
              receiverFrame,
              receiver.ok,
              boolValue(receiverFrame.fn.module.registry, true),
            );
          this.setLocal(
            frame,
            instruction.selectIndex!,
            intValue(frame.fn.module.registry, BigInt(index)),
          );
          this.setLocal(
            otherFrame,
            otherInstruction.selectIndex!,
            intValue(otherFrame.fn.module.registry, BigInt(otherIndex)),
          );
          otherFrame.pc++;
          this.unblock(other);
          return true;
        }
      }
    }
    return false;
  }

  private callFFI(
    task: Task,
    frame: Frame,
    instruction: PreparedInstruction,
    inputs: Value[],
  ): void {
    const route = textDecoder.decode(expectBytes(inputs[0]!, "call_ffi"));
    const payload = this.bytesFromSlice(inputs[1]!);
    if (payload.length > this.maxInputBytes)
      fail("FFI payload exceeds limit", "call_ffi", "ffi_limit");
    if (this.pendingHost.size >= (this.options.limits?.maxPendingCalls ?? 65_536))
      fail("pending FFI call limit exceeded", "call_ffi", "ffi_limit");
    const id = this.nextHostCall++;
    this.ffiCalls++;
    this.ffiBytes += BigInt(payload.length);
    const memory = this.reserveControl(BigInt(STORAGE_COST.control_record)).commit();
    this.pendingHost.set(id, {
      task: task.id,
      outputs: instruction.operands.outputs,
      resultType: this.outputType(frame, instruction.operands.outputs[0]!),
      memory,
    });
    this.pendingActions.push({ kind: "call", id, route, payload: [...payload] });
    this.block(task, { kind: "ffi", id });
  }

  complete(id: number, payload: Uint8Array, error?: string): void {
    const pending = this.pendingHost.get(id);
    if (!pending) return;
    if (pending.reply) return;
    pending.reply =
      error === undefined
        ? { payload: Uint8Array.from(payload) }
        : { payload: Uint8Array.from(payload), error };
    this.processHostReply(id, pending);
  }

  private processHostReply(id: number, pending: PendingHostCall): void {
    const reply = pending.reply;
    if (!reply) return;
    const task = this.tasks.get(pending.task);
    if (!task || task.state !== "blocked") {
      this.pendingHost.delete(id);
      pending.replyMemory?.release();
      delete pending.replyMemory;
      pending.memory.release();
      this.pendingActions.push({ kind: "decision", id, accepted: false });
      return;
    }
    let materializationError: VMError | undefined;
    try {
      if (!pending.replyMemory)
        pending.replyMemory = this.reserveControl(BigInt(reply.payload.byteLength)).commit();
      const frame = task.frames[task.frames.length - 1]!;
      const status = reply.error ? (reply.error.includes("route unavailable") ? 1n : 2n) : 0n;
      const value = this.byteSlice(pending.resultType, reply.payload);
      this.outputs(frame, pending.outputs, [
        value,
        stringValue(frame.fn.module.registry, textEncoder.encode(reply.error ?? "")),
        intValue(frame.fn.module.registry, status),
      ]);
    } catch (error) {
      // Allocation helpers already collect and retry before returning a
      // terminal failure. A ready reply must reach a decision even when its
      // payload cannot fit; polling must not replay the host call or retry
      // the same permanent quota failure indefinitely.
      materializationError = toVMError(error);
    }
    this.pendingHost.delete(id);
    pending.replyMemory?.release();
    delete pending.replyMemory;
    pending.memory.release();
    this.pendingActions.push({
      kind: "decision",
      id,
      accepted: materializationError === undefined && !reply.error,
    });
    if (materializationError) this.failTask(task, materializationError);
    else this.unblock(task);
    delete pending.reply;
  }

  decisionDone(_id: number): void {}
  hostClosed(): void {}

  actions(): VMAction[] {
    return this.pendingActions.splice(0);
  }

  private byteSlice(type: RuntimeType, bytes: Uint8Array): Value {
    const storage = this.managedCell({ type, kind: "bytes", value: Uint8Array.from(bytes) });
    return {
      type,
      kind: "slice",
      value: {
        storage: { root: storage, path: [] },
        start: 0,
        length: bytes.length,
        capacity: bytes.length,
      } satisfies SliceValue,
    };
  }

  private bytesFromSlice(value: Value): Uint8Array {
    if (value.kind === "nil") return new Uint8Array();
    if (value.kind === "string" || value.kind === "bytes")
      return Uint8Array.from(value.value as Uint8Array);
    if (value.kind !== "slice") invalid("expected byte slice", "bytes");
    const slice = value.value as SliceValue;
    const backing = dereference(slice.storage);
    if (backing.kind === "bytes")
      return (backing.value as Uint8Array).slice(slice.start, slice.start + slice.length);
    return Uint8Array.from(this.sliceValues(value).map((item) => Number(item.value as bigint)));
  }

  private intrinsic(
    task: Task,
    frame: Frame,
    instruction: PreparedInstruction,
    id: string,
    inputs: Value[],
  ): void {
    const output = (...values: Value[]) =>
      this.outputs(frame, instruction.operands.outputs, values);
    switch (id) {
      case "strings.index_byte": {
        const bytes = expectBytes(inputs[0]!, id);
        const start = asIndex(inputs[1]!);
        const end = asIndex(inputs[2]!);
        const target = Number(inputs[3]!.value as bigint);
        let found = -1;
        for (let index = start; index < Math.min(end, bytes.length); index++)
          if (bytes[index] === target) {
            found = index;
            break;
          }
        output(intValue(frame.fn.module.registry, BigInt(found)));
        return;
      }
      case "math.float64_bits":
        output({
          type: this.outputType(frame, instruction.operands.outputs[0]!),
          kind: "uint",
          value: floatBits(inputs[0]!.value as number, 64),
        });
        return;
      case "math.float64_from_bits":
        output({
          type: this.outputType(frame, instruction.operands.outputs[0]!),
          kind: "float",
          value: bitsFloat(inputs[0]!.value as bigint, 64),
        });
        return;
      case "math.float32_bits":
        output({
          type: this.outputType(frame, instruction.operands.outputs[0]!),
          kind: "uint",
          value: floatBits(inputs[0]!.value as number, 32),
        });
        return;
      case "math.float32_from_bits":
        output({
          type: this.outputType(frame, instruction.operands.outputs[0]!),
          kind: "float",
          value: bitsFloat(inputs[0]!.value as bigint, 32),
        });
        return;
      case "time.now": {
        const milliseconds = Date.now();
        output(
          intValue(frame.fn.module.registry, BigInt(Math.floor(milliseconds / 1000))),
          intValue(frame.fn.module.registry, BigInt(milliseconds % 1000) * 1_000_000n),
        );
        return;
      }
      case "time.timer_start": {
        const channel = this.channel(inputs[0]!);
        const seconds = inputs[1]!.value as bigint;
        const nanos = inputs[2]!.value as bigint;
        const delay = Math.max(0, Number(seconds * 1000n + nanos / 1_000_000n));
        const memory = this.reserveControl(BigInt(STORAGE_COST.control_record)).commit();
        const timer: TimerRecord = {
          id: this.nextTimer++,
          channel,
          deadline: performance.now() + delay,
          memory,
          canceled: false,
        };
        this.timers.set(timer.id, timer);
        return;
      }
      case "time.timer_stop": {
        const channel = this.channel(inputs[0]!);
        const timer = [...this.timers.values()].find(
          (item) => item.channel === channel && !item.canceled,
        );
        if (timer) {
          timer.canceled = true;
          this.timers.delete(timer.id);
          timer.memory.release();
        }
        output(boolValue(frame.fn.module.registry, Boolean(timer)));
        return;
      }
      case "ffi.call":
        this.callFFI(task, frame, instruction, inputs);
        return;
      case "sync.mutex_try_lock": {
        const mutex = this.mutex(inputs[0]!);
        const acquired = mutex.length === 0;
        if (acquired) mutex.push(boolValue(frame.fn.module.registry, true));
        output(boolValue(frame.fn.module.registry, acquired));
        return;
      }
      case "sync.mutex_unlock": {
        const mutex = this.mutex(inputs[0]!);
        if (mutex.length === 0) fail("unlock of unlocked mutex", id, "panic");
        mutex.shift();
        this.wakeSelects();
        return;
      }
      case "sync.mutex_lock": {
        const mutex = this.mutex(inputs[0]!);
        if (mutex.length === 0) {
          mutex.push(boolValue(frame.fn.module.registry, true));
          return;
        }
        frame.pc--;
        this.block(task, { kind: "select" });
        return;
      }
      case "crypto.sha256.block":
        output(sha256Block(inputs[0]!, this.bytesFromSlice(inputs[1]!)));
        return;
      case "crypto.rand.read": {
        const destination = this.byteSliceView(inputs[0]!);
        if (!destination) invalid("random destination is not a byte slice", id);
        const bytes = destination.bytes.subarray(
          destination.offset,
          destination.offset + destination.length,
        );
        crypto.getRandomValues(bytes);
        destination.commit?.();
        output(
          intValue(frame.fn.module.registry, BigInt(bytes.length)),
          stringValue(frame.fn.module.registry, new Uint8Array()),
          boolValue(frame.fn.module.registry, true),
        );
        return;
      }
      default:
        if (id.startsWith("reflect.")) {
          this.reflectIntrinsic(task, frame, instruction, id, inputs);
          return;
        }
        invalid("unsupported intrinsic", id);
    }
  }

  private mutex(value: Value): TypedChannel {
    if (value.kind !== "pointer") invalid("mutex state requires an address", "mutex");
    const address = value.value as Address;
    let state = dereference(address);
    if (state.kind === "nil") {
      state = { type: state.type, kind: "channel", value: this.createChannel(state.type, 1) };
      assign(address, state);
    }
    return this.channel(state);
  }

  private reflectIntrinsic(
    task: Task,
    frame: Frame,
    instruction: PreparedInstruction,
    id: string,
    inputs: Value[],
  ): void {
    const output = (...values: Value[]) =>
      this.outputs(frame, instruction.operands.outputs, values);
    const success = (...values: Value[]) =>
      output(
        ...values,
        stringValue(frame.fn.module.registry, new Uint8Array()),
        boolValue(frame.fn.module.registry, true),
      );
    try {
      if (id === "reflect.type_of") {
        const dynamic = this.dynamicValue(inputs[0]!);
        output(
          dynamic
            ? this.reflectType(
                frame,
                this.outputType(frame, instruction.operands.outputs[0]!),
                dynamic.type,
              )
            : zero(this.outputType(frame, instruction.operands.outputs[0]!)),
        );
        return;
      }
      if (id === "reflect.value_of") {
        const dynamic = this.dynamicValue(inputs[0]!);
        output(
          this.reflectValue(
            frame,
            this.outputType(frame, instruction.operands.outputs[0]!),
            dynamic ? this.ownedView(dynamic) : this.invalidView(),
          ),
        );
        return;
      }
      if (id === "reflect.deep_equal") {
        output(boolValue(frame.fn.module.registry, this.deepEqual(inputs[0]!, inputs[1]!)));
        return;
      }
      if (id === "reflect.same_reference") {
        const leftInput = inputs[0];
        const rightInput = inputs[1];
        if (!leftInput || !rightInput) fail("reflect: missing comparison input", id, "reflect");
        const left = this.reflectedView(leftInput);
        const right = this.reflectedView(rightInput);
        output(
          boolValue(frame.fn.module.registry, this.sameReference(left.current, right.current)),
        );
        return;
      }
      if (id === "reflect.type_kind") {
        success({
          type: this.outputType(frame, instruction.operands.outputs[0]!),
          kind: "uint",
          value: BigInt(this.reflectKind(this.reflectedType(inputs[0]!))),
        });
        return;
      }
      if (id === "reflect.type_measure") {
        const type = this.reflectedType(inputs[0]!);
        const property = textDecoder.decode(expectBytes(inputs[1]!, id));
        success({
          type: this.outputType(frame, instruction.operands.outputs[0]!),
          kind: "uint",
          value: BigInt(this.reflectMeasure(type, property)),
        });
        return;
      }
      if (id === "reflect.type_descriptor") {
        success(
          this.reflectDescriptor(
            frame,
            this.outputType(frame, instruction.operands.outputs[0]!),
            this.reflectedType(inputs[0]!),
          ),
        );
        return;
      }
      if (
        id === "reflect.assignable_to" ||
        id === "reflect.convertible_to" ||
        id === "reflect.implements"
      ) {
        const source = this.reflectedType(inputs[0]!);
        const target = this.reflectedType(inputs[1]!);
        const answer =
          id === "reflect.implements"
            ? this.implements(source, target)
            : id === "reflect.assignable_to"
              ? source.registry.identical(source, target) ||
                (this.isInterface(target) && this.implements(source, target))
              : this.reflectConvertible(source, target);
        output(boolValue(frame.fn.module.registry, answer));
        return;
      }
      if (
        [
          "reflect.pointer_to",
          "reflect.slice_of",
          "reflect.array_of",
          "reflect.map_of",
          "reflect.chan_of",
          "reflect.func_of",
          "reflect.struct_of",
        ].includes(id)
      ) {
        success(this.reflectConstructType(frame, instruction, id, inputs));
        return;
      }
      if (id === "reflect.type_field") {
        success(this.reflectTypeField(frame, this.reflectedType(inputs[0]!), asIndex(inputs[1]!)));
        return;
      }
      if (id === "reflect.type_method") {
        success(
          this.reflectMethod(
            frame,
            this.outputType(frame, instruction.operands.outputs[0]!),
            this.reflectedType(inputs[0]!),
            asIndex(inputs[1]!),
          ),
        );
        return;
      }
      if (
        id === "reflect.zero" ||
        id === "reflect.new" ||
        id === "reflect.make_slice" ||
        id === "reflect.make_map" ||
        id === "reflect.make_chan"
      ) {
        const type = this.reflectedType(inputs[0]!);
        let value: Value;
        if (id === "reflect.new") {
          const root = this.managedCell(zero(type));
          value = {
            type: this.dynamicType(type.registry, { kind: Kind.Pointer, elem: type }),
            kind: "pointer",
            value: { root, path: [] } satisfies Address,
          };
        } else if (id === "reflect.make_slice") {
          if (asIndex(inputs[1]!) > asIndex(inputs[2]!))
            fail("reflect.MakeSlice: length exceeds capacity", id, "reflect");
          value = this.makeSlice(type, true, inputs.slice(1));
        } else if (id === "reflect.make_map") {
          const map = this.createMap(type);
          value = { type, kind: "map", value: map };
        } else if (id === "reflect.make_chan") {
          const channel = this.createChannel(type, asIndex(inputs[1]!));
          value = {
            type,
            kind: "channel",
            value: channel,
          };
        } else value = zero(type);
        success(
          this.reflectValue(
            frame,
            this.outputType(frame, instruction.operands.outputs[0]!),
            this.ownedView(value),
          ),
        );
        return;
      }
      if (id === "reflect.value_current") {
        success(
          this.reflectValue(
            frame,
            this.outputType(frame, instruction.operands.outputs[0]!),
            this.reflectedView(inputs[0]!),
          ),
        );
        return;
      }
      if (id === "reflect.value_equal") {
        const left = this.reflectedView(inputs[0]!).current;
        const right = this.reflectedView(inputs[1]!).current;
        success(
          boolValue(
            frame.fn.module.registry,
            !left || !right ? left === right : guestEqual(left, right),
          ),
        );
        return;
      }
      if (
        id === "reflect.value_elem" ||
        id === "reflect.value_addr" ||
        id === "reflect.value_index" ||
        id === "reflect.value_field_by_index" ||
        id === "reflect.value_slice" ||
        id === "reflect.value_slice3"
      ) {
        const view = this.reflectViewOperation(id, inputs);
        success(
          this.reflectValue(frame, this.outputType(frame, instruction.operands.outputs[0]!), view),
        );
        return;
      }
      if (id === "reflect.value_convert") {
        const view = this.reflectedView(inputs[0]!);
        if (!view.current) fail("reflect: Convert of invalid Value", id, "reflect");
        success(
          this.reflectValue(
            frame,
            this.outputType(frame, instruction.operands.outputs[0]!),
            this.ownedView(this.convertValue(view.current, this.reflectedType(inputs[1]!))),
          ),
        );
        return;
      }
      if (id === "reflect.value_set") {
        const target = this.reflectedView(inputs[0]!);
        const source = this.reflectedView(inputs[1]!).current;
        if (!target.settable || !target.target || !source)
          fail("reflect: value is not settable", id, "reflect");
        assign(target.target, this.coerce(source, dereference(target.target).type));
        success();
        return;
      }
      if (id === "reflect.value_append" || id === "reflect.value_append_slice") {
        const target = this.reflectedView(inputs[0]!).current;
        if (!target) fail("reflect: append of invalid Value", id, "reflect");
        const element = this.elementType(target.type);
        if (id === "reflect.value_append_slice") {
          const source = this.reflectedView(inputs[1]!).current;
          if (
            !source ||
            source.type.registry.underlying(source.type).ref.kind !== Kind.Slice ||
            !element.registry.identical(element, this.elementType(source.type))
          )
            fail("reflect: AppendSlice requires matching slice element types", id, "reflect");
        }
        const values =
          id === "reflect.value_append"
            ? this.sliceValues(inputs[1]!).map(
                (item) =>
                  this.reflectedView(item).current ??
                  fail("reflect: invalid append value", id, "reflect"),
              )
            : this.sliceValues(
                this.reflectedView(inputs[1]!).current ??
                  fail("reflect: invalid append slice", id, "reflect"),
              );
        for (let index = 0; index < values.length; index++) {
          const value = values[index]!;
          if (
            value.type.ref.kind === Kind.Named &&
            element.ref.kind === Kind.Named &&
            !element.registry.identical(element, value.type)
          )
            fail("reflect: append value is not assignable", id, "reflect");
          values[index] = this.coerce(value, element);
        }
        const appended = this.append(false, [target, ...values]);
        success(
          this.reflectValue(
            frame,
            this.outputType(frame, instruction.operands.outputs[0]!),
            this.ownedView(appended),
          ),
        );
        return;
      }
      if (id === "reflect.value_copy") {
        const destination = this.reflectedView(inputs[0]!).current;
        const source = this.reflectedView(inputs[1]!).current;
        if (!destination || !source) fail("reflect: Copy of invalid Value", id, "reflect");
        const element = this.elementType(destination.type);
        const sourceType = source.type.registry.underlying(source.type);
        const validSource =
          sourceType.ref.kind === Kind.Primitive &&
          sourceType.ref.primitive === PrimitiveKind.PrimitiveString
            ? element.ref.kind === Kind.Primitive &&
              element.ref.primitive === PrimitiveKind.PrimitiveUint8
            : sourceType.ref.kind === Kind.Slice &&
              element.registry.identical(element, this.elementType(source.type));
        if (!validSource) fail("reflect: Copy requires matching element types", id, "reflect");
        success(intValue(frame.fn.module.registry, BigInt(this.copy(destination, source))));
        return;
      }
      if (
        id === "reflect.value_set_len" ||
        id === "reflect.value_set_cap" ||
        id === "reflect.value_grow"
      ) {
        this.reflectResize(id, this.reflectedView(inputs[0]!), asIndex(inputs[1]!));
        success();
        return;
      }
      if (id === "reflect.value_swap") {
        const view = this.reflectedView(inputs[0]!);
        if (!view.current) fail("reflect: Swapper of invalid Value", id, "reflect");
        const left = asIndex(inputs[1]!);
        const right = asIndex(inputs[2]!);
        const temporary = cloneValue(indexValue(view.current, left));
        setIndex(view.current, left, indexValue(view.current, right));
        setIndex(view.current, right, temporary);
        success();
        return;
      }
      if (id === "reflect.value_set_map_index") {
        const map = this.reflectedView(inputs[0]!).current;
        const key = this.reflectedView(inputs[1]!).current;
        const element = this.reflectedView(inputs[2]!);
        if (!map || !key || map.kind !== "map")
          fail("reflect: SetMapIndex of non-map Value", id, "reflect");
        if (!element.current) this.deleteMap(map, key);
        else this.storeIndex(map, key, element.current);
        success();
        return;
      }
      if (id === "reflect.value_method") {
        const receiver = this.reflectedView(inputs[0]!).current;
        const methodValue = inputs[1]!;
        if (!receiver || methodValue.kind !== "struct")
          fail("reflect: invalid Method", id, "reflect");
        const callableValue = fieldValue(methodValue, "Func");
        const callableView = callableValue ? this.reflectedView(callableValue).current : undefined;
        const signature = fieldValue(methodValue, "Type");
        if (!callableView || callableView.kind !== "function" || !signature)
          fail("reflect: invalid Method", id, "reflect");
        const method = {
          function: callableView.value as FunctionValue,
          type: this.reflectedType(signature),
        };
        let boundType = method.type;
        if (method.type.dynamic) {
          const dynamic: DynamicType = { ...method.type.dynamic };
          if (method.type.dynamic.params) dynamic.params = method.type.dynamic.params.slice(1);
          else delete dynamic.params;
          boundType = this.dynamicType(method.type.registry, dynamic);
        }
        const callable: Value = {
          type: boundType,
          kind: "method",
          value: { ...method.function, receiver: cloneValue(receiver) } satisfies FunctionValue,
        };
        success(
          this.reflectValue(
            frame,
            this.outputType(frame, instruction.operands.outputs[0]!),
            this.ownedView(callable),
          ),
        );
        return;
      }
      if (id === "reflect.make_func") {
        const type = this.reflectedType(inputs[0]!);
        const callback = valueFromInterface(inputs[1]!);
        if (callback.kind !== "function")
          fail("reflect.MakeFunc: callback is not callable", id, "reflect");
        const callable: Value = {
          type,
          kind: "dynamicFunction",
          value: { callback: callback.value as FunctionValue },
        };
        success(
          this.reflectValue(
            frame,
            this.outputType(frame, instruction.operands.outputs[0]!),
            this.ownedView(callable),
          ),
        );
        return;
      }
      if (id === "reflect.value_try_recv" || id === "reflect.value_recv") {
        const channel = this.reflectedView(inputs[0]!).current;
        if (!channel) fail("reflect: receive on invalid Value", id, "reflect");
        const received = this.tryReceive(channel);
        if (received) {
          output(
            this.reflectValue(
              frame,
              this.outputType(frame, instruction.operands.outputs[0]!),
              this.ownedView(received[0]),
            ),
            boolValue(frame.fn.module.registry, received[1]),
            stringValue(frame.fn.module.registry, new Uint8Array()),
            boolValue(frame.fn.module.registry, true),
          );
        } else if (id === "reflect.value_try_recv") {
          output(
            zero(this.outputType(frame, instruction.operands.outputs[0]!)),
            boolValue(frame.fn.module.registry, false),
            stringValue(frame.fn.module.registry, new Uint8Array()),
            boolValue(frame.fn.module.registry, true),
          );
        } else {
          const raw = this.channel(channel);
          raw.receivers.push(task.id);
          this.block(task, {
            kind: "receive",
            channel: raw,
            outputs: instruction.operands.outputs,
            receiveOK: true,
            reflected: true,
          });
        }
        return;
      }
      if (id === "reflect.value_try_send" || id === "reflect.value_send") {
        const channel = this.reflectedView(inputs[0]!).current;
        const value = this.reflectedView(inputs[1]!).current;
        if (!channel || !value) fail("reflect: send of invalid Value", id, "reflect");
        if (id === "reflect.value_try_send") {
          const sent = this.trySend(channel, value, task);
          success(boolValue(frame.fn.module.registry, sent));
        } else {
          success();
          this.send(task, channel, value);
        }
        return;
      }
      if (id === "reflect.value_close") {
        const channel = this.reflectedView(inputs[0]!).current;
        if (!channel) fail("reflect: close of invalid Value", id, "reflect");
        this.closeChannel(channel);
        success();
        return;
      }
      if (id === "reflect.select") {
        const cases = this.sliceValues(inputs[0]!).map((value, index): ReflectedSelectCase => {
          if (value.kind !== "struct") fail("reflect.Select: invalid case", id, "reflect");
          const direction = asIndex(fieldValue(value, "Dir"));
          if (direction === 3) return { index, receive: false, default: true };
          if (direction !== 1 && direction !== 2)
            fail("reflect.Select: invalid direction", id, "reflect");
          const channel = this.reflectedView(fieldValue(value, "Chan")).current;
          const selected: ReflectedSelectCase = {
            index,
            receive: direction === 2,
            default: false,
          };
          if (channel) selected.channel = this.managedCell(channel);
          if (direction === 1) {
            const send = this.reflectedView(fieldValue(value, "Send")).current;
            if (send) selected.send = this.managedCell(send);
          }
          return selected;
        });
        if (!this.completeReflectSelect(task, frame, instruction.operands.outputs, cases)) {
          const memory = this.reserveControl(BigInt(STORAGE_COST.control_record)).commit();
          this.block(task, {
            kind: "reflectSelect",
            cases,
            outputs: instruction.operands.outputs,
            memory,
          });
        }
        return;
      }
      if (id === "reflect.value_call") {
        this.reflectCall(task, frame, instruction, inputs);
        return;
      }
      this.reflectFailure(frame, instruction, `unsupported reflection operation ${id}`);
    } catch (error) {
      if (
        error instanceof VMError &&
        (error.code === "reflect" ||
          error.code === "type_error" ||
          error.code === "invalid_operation")
      ) {
        this.reflectFailure(frame, instruction, error.message);
        return;
      }
      throw error;
    }
  }

  private invalidView(): ReflectedView {
    return { addressable: false, settable: false, interfaceable: false, embeddedReadOnly: false };
  }

  private ownedView(value: Value): ReflectedView {
    // managedCell encodes the value into stable storage, so cloning before
    // publication only creates a second aggregate graph. The view is
    // non-addressable; callers that need a mutable/addressable value go
    // through reflectedView/reflectValue and their explicit target path.
    const refresh = this.managedCell(value);
    return {
      current: refresh.read(),
      refresh,
      addressable: false,
      settable: false,
      interfaceable: true,
      embeddedReadOnly: false,
    };
  }

  private dynamicValue(value: Value): Value | undefined {
    while (value.kind === "interface") value = value.value as Value;
    const kind = value.type.registry.underlying(value.type).ref.kind;
    return value.kind === "nil" && (kind === Kind.Any || kind === Kind.Interface)
      ? undefined
      : value;
  }

  private reflectedView(value: Value): ReflectedView {
    if (value.kind !== "struct") fail("reflect: invalid Value", "reflect", "reflect");
    if (!fieldValue(value, "valid").value) return this.invalidView();
    const result: ReflectedView = {
      addressable: Boolean(fieldValue(value, "addressable").value),
      settable: Boolean(fieldValue(value, "settable").value),
      interfaceable: Boolean(fieldValue(value, "interfaceable").value),
      embeddedReadOnly: Boolean(fieldValue(value, "embeddedReadOnly").value),
    };
    const reflectedType = fieldValue(value, "valueType");
    if (reflectedType) result.declaredType = this.reflectedType(reflectedType);
    const target = fieldValue(value, "target");
    const pointer = target ? valueFromInterface(target) : undefined;
    if (pointer?.kind === "pointer") {
      const address = pointer.value as Address;
      result.current = cloneValue(dereference(address));
      if (result.addressable) result.target = address;
      else if (address.path.length === 0) result.refresh = address.root;
    } else {
      const data = fieldValue(value, "data");
      if (data) result.current = cloneValue(valueFromInterface(data));
    }
    if (
      result.current &&
      result.declaredType &&
      !result.declaredType.registry.identical(result.declaredType, result.current.type)
    ) {
      const declared = result.declaredType.registry.underlying(result.declaredType).ref.kind;
      if (declared === Kind.Any || declared === Kind.Interface)
        result.current = {
          type: result.declaredType,
          kind: "interface",
          value: result.current,
        };
    }
    return result;
  }

  private reflectedType(value: Value): RuntimeType {
    if (value.kind === "interface") value = value.value as Value;
    const key = value.kind === "struct" ? fieldValue(value, "key") : undefined;
    const type =
      key?.kind === "string"
        ? this.reflectedTypes.get(textDecoder.decode(key.value as Uint8Array))
        : undefined;
    if (!type) fail("reflect: invalid Type", "reflect", "reflect");
    if (key?.kind === "string") {
      const storage = this.stringStorage.get(key.value as Uint8Array);
      if (storage && this.guestMemory.managedHeap.has(storage.handle))
        this.retainReflectedType(storage.handle, textDecoder.decode(key.value as Uint8Array));
    }
    return type;
  }

  private retainReflectedType(handle: GuestHandle, key: string): void {
    this.pendingReflectedTypes.delete(key);
    const owner = `${handle.slot}:${handle.generation}`;
    let keys = this.reflectedTypeOwners.get(owner);
    if (!keys) {
      keys = new Set();
      this.reflectedTypeOwners.set(owner, keys);
    }
    keys.add(key);
  }

  private namedReflectType(frame: Frame, name: string): RuntimeType {
    const registry = this.revision.program.module("reflect")?.registry ?? frame.fn.module.registry;
    return registry.runtime({
      kind: Kind.Named,
      primitive: 0,
      named: { module_path: "reflect", decl_id: name },
      node: "",
    });
  }

  private dynamicType(
    registry: RuntimeType["registry"],
    dynamic: NonNullable<RuntimeType["dynamic"]>,
  ): RuntimeType {
    const candidate: RuntimeType = {
      module: registry.module,
      registry,
      ref: { kind: dynamic.kind, primitive: 0, named: emptyKey(), node: "" },
      dynamic,
    };
    const canonical = `${registry.module}\0${registry.key(candidate)}`;
    const existing = this.dynamicTypes.get(canonical);
    if (existing) return existing;
    const bytes = textEncoder.encode(canonical).byteLength;
    if (this.dynamicTypes.size >= (this.options.limits?.maxDynamicTypes ?? 4096))
      fail("dynamic type count exceeds limit", "type", "dynamic_type_limit");
    if (
      this.dynamicTypeBytes + bytes >
      Number(this.options.limits?.maxDynamicTypeBytes ?? 16 * 1024 * 1024)
    )
      fail("dynamic type bytes exceed limit", "type", "allocation_limit");
    const metadataBytes = BigInt(STORAGE_COST.control_record + bytes);
    const memory = this.reserveControl(metadataBytes).commit();
    let resident: { release(): void };
    try {
      resident = this.reserveResident(metadataBytes);
    } catch (error) {
      memory.release();
      throw error;
    }
    const owned = this.ownedType(candidate);
    this.dynamicTypes.set(canonical, owned);
    this.dynamicTypeLeases.set(canonical, { memory, resident, bytes });
    this.dynamicTypeBytes += bytes;
    return owned;
  }

  private reflectStruct(frame: Frame, name: string, fields: Record<string, Value>): Value {
    const value = zero(this.namedReflectType(frame, name));
    if (value.kind !== "struct")
      fail(`reflect: invalid ${name} schema`, "reflect", "invalid_operation");
    const values = value.value as Map<string, Value>;
    for (const [field, source] of Object.entries(fields)) {
      const destination = values.get(field);
      if (!destination) continue;
      values.set(field, this.coerce(source, destination.type));
    }
    return value;
  }

  private reflectType(frame: Frame, interfaceType: RuntimeType, type: RuntimeType): Value {
    type = this.ownedType(type);
    const key = type.registry.key(type);
    this.pendingReflectedTypes.add(key);
    this.reflectedTypes.set(key, type);
    const keyBytes = textEncoder.encode(key);
    this.reflectedTypeBytes.set(keyBytes, key);
    const concrete = this.reflectStruct(frame, "runtimeType", {
      key: stringValue(frame.fn.module.registry, keyBytes),
    });
    return { type: interfaceType, kind: "interface", value: concrete };
  }

  private reflectValue(frame: Frame, type: RuntimeType, source: ReflectedView): Value {
    const view = source.target
      ? { ...source, current: cloneValue(dereference(source.target)) }
      : source.refresh
        ? { ...source, current: cloneValue(source.refresh.read()) }
        : source;
    const result = zero(type);
    if (!view.current || result.kind !== "struct") return result;
    const current = view.current;
    const fields = result.value as Map<string, Value>;
    const put = (name: string, value: Value): void => {
      const destination = fields.get(name);
      if (destination) fields.set(name, this.coerce(value, destination.type));
    };
    put("valid", boolValue(frame.fn.module.registry, true));
    const valueType = fields.get("valueType");
    if (valueType)
      fields.set(
        "valueType",
        this.reflectType(frame, valueType.type, view.declaredType ?? current.type),
      );
    put("data", current);
    if (view.target || view.refresh) {
      const address = view.target ?? { root: view.refresh!, path: [] };
      put("target", {
        type: this.dynamicType(current.type.registry, { kind: Kind.Pointer, elem: current.type }),
        kind: "pointer",
        value: address,
      });
    }
    put("addressable", boolValue(frame.fn.module.registry, view.addressable));
    put("settable", boolValue(frame.fn.module.registry, view.settable));
    put("interfaceable", boolValue(frame.fn.module.registry, view.interfaceable));
    put("embeddedReadOnly", boolValue(frame.fn.module.registry, view.embeddedReadOnly));
    put("zero", boolValue(frame.fn.module.registry, this.isZeroValue(current, new Set())));
    put("nilValue", boolValue(frame.fn.module.registry, current.kind === "nil"));
    const length =
      current.kind === "string"
        ? (current.value as Uint8Array).length
        : current.kind === "array"
          ? (packedLength(current) ?? (current.value as Value[]).length)
          : current.kind === "slice"
            ? (current.value as SliceValue).length
            : current.kind === "map"
              ? (current.value as TypedMap).size
              : current.kind === "channel"
                ? (current.value as Channel).queue.length
                : current.kind === "struct"
                  ? (packedLength(current) ?? (current.value as Map<string, Value>).size)
                  : 0;
    const capacity =
      current.kind === "array"
        ? (packedLength(current) ?? (current.value as Value[]).length)
        : current.kind === "slice"
          ? (current.value as SliceValue).capacity
          : current.kind === "channel"
            ? (current.value as Channel).capacity
            : 0;
    put("length", intValue(frame.fn.module.registry, BigInt(length)));
    put("capacity", intValue(frame.fn.module.registry, BigInt(capacity)));
    if (current.kind === "bool") put("boolValue", current);
    else if (current.kind === "int")
      put("intValue", {
        ...current,
        type: primitive(frame.fn.module.registry, PrimitiveKind.PrimitiveInt64),
      });
    else if (current.kind === "uint")
      put("uintValue", {
        ...current,
        type: primitive(frame.fn.module.registry, PrimitiveKind.PrimitiveUint64),
      });
    else if (current.kind === "float")
      put("floatValue", {
        ...current,
        type: primitive(frame.fn.module.registry, PrimitiveKind.PrimitiveFloat64),
      });
    else if (current.kind === "complex")
      put("complexValue", {
        ...current,
        type: primitive(frame.fn.module.registry, PrimitiveKind.PrimitiveComplex128),
      });
    else if (current.kind === "string") put("stringValue", current);
    if (current.kind === "map") {
      const destination = fields.get("mapEntries");
      if (destination) {
        const entryType = this.elementType(destination.type);
        const entries = [...(current.value as TypedMap).entries()].map((entry) => {
          const reflected = zero(entryType);
          if (reflected.kind !== "struct") return reflected;
          const values = reflected.value as Map<string, Value>;
          const key = values.get("Key");
          const item = values.get("Value");
          if (key) values.set("Key", this.reflectValue(frame, key.type, this.ownedView(entry.key)));
          if (item)
            values.set("Value", this.reflectValue(frame, item.type, this.ownedView(entry.value)));
          return reflected;
        });
        fields.set("mapEntries", this.sliceFromValues(destination.type, entries));
      }
    }
    return result;
  }

  private isZeroValue(value: Value, seen: Set<number>): boolean {
    if (value.kind === "nil") return true;
    if (value.kind === "bool") return value.value === false;
    if (value.kind === "int" || value.kind === "uint") return value.value === 0n;
    if (value.kind === "float") return value.value === 0;
    if (value.kind === "complex") {
      const complex = value.value as { real: number; imag: number };
      return complex.real === 0 && complex.imag === 0;
    }
    if (value.kind === "string") return (value.value as Uint8Array).length === 0;
    if (value.kind === "interface") return this.isZeroValue(value.value as Value, seen);
    if (value.kind === "pointer") {
      const address = value.value as Address;
      if (seen.has(address.root.id)) return false;
      seen.add(address.root.id);
      return false;
    }
    if (value.kind === "array")
      return [...(packedItems(value) ?? (value.value as Value[]))].every((item) =>
        this.isZeroValue(item, seen),
      );
    if (value.kind === "struct")
      return [...(packedFields(value) ?? (value.value as Map<string, Value>))].every(
        ([name, item]) => name === "_" || this.isZeroValue(item, seen),
      );
    return false;
  }

  private reflectKind(type: RuntimeType): number {
    const underlying = type.registry.underlying(type);
    if (underlying.ref.kind === Kind.Primitive) {
      if (underlying.ref.primitive === PrimitiveKind.PrimitiveBool) return 1;
      if (underlying.ref.primitive === PrimitiveKind.PrimitiveString) return 24;
      if (underlying.ref.primitive === PrimitiveKind.PrimitiveError) return 20;
      if (underlying.ref.primitive === PrimitiveKind.PrimitiveFunction) return 19;
      if (
        underlying.ref.primitive >= PrimitiveKind.PrimitiveInt &&
        underlying.ref.primitive <= PrimitiveKind.PrimitiveComplex128
      )
        return underlying.ref.primitive - 1;
    }
    return (
      new Map<number, number>([
        [Kind.Array, 17],
        [Kind.Waitable, 18],
        [Kind.Function, 19],
        [Kind.Any, 20],
        [Kind.Interface, 20],
        [Kind.Map, 21],
        [Kind.Pointer, 22],
        [Kind.Slice, 23],
        [Kind.Struct, 25],
      ]).get(underlying.ref.kind) ?? 0
    );
  }

  private reflectMeasure(type: RuntimeType, property: string): number {
    const node = type.registry.node(type.registry.underlying(type));
    if (property === "fields") return type.dynamic?.fields?.length ?? node?.fields.length ?? 0;
    if (property === "methods") return this.methodNode(type)?.methods.length ?? 0;
    const primitive = type.registry.underlying(type).ref.primitive;
    const size =
      primitive === PrimitiveKind.PrimitiveBool ||
      primitive === PrimitiveKind.PrimitiveInt8 ||
      primitive === PrimitiveKind.PrimitiveUint8
        ? 1
        : primitive === PrimitiveKind.PrimitiveInt16 || primitive === PrimitiveKind.PrimitiveUint16
          ? 2
          : primitive === PrimitiveKind.PrimitiveInt32 ||
              primitive === PrimitiveKind.PrimitiveUint32 ||
              primitive === PrimitiveKind.PrimitiveFloat32
            ? 4
            : primitive === PrimitiveKind.PrimitiveComplex128
              ? 16
              : 8;
    if (property === "bits")
      return primitive >= PrimitiveKind.PrimitiveInt &&
        primitive <= PrimitiveKind.PrimitiveComplex128
        ? size * 8
        : 0;
    if (property === "align" || property === "field_align") return Math.min(size, 8);
    if (property === "size") return size;
    fail("reflect: unknown type measure", property, "reflect");
  }

  private reflectDescriptor(frame: Frame, outputType: RuntimeType, type: RuntimeType): Value {
    const underlying = type.registry.underlying(type);
    const node = underlying.registry.node(underlying);
    const dynamic = underlying.dynamic;
    const element =
      dynamic?.elem ?? (node?.elem ? underlying.registry.runtime(node.elem) : undefined);
    const key = dynamic?.key ?? (node?.key ? underlying.registry.runtime(node.key) : undefined);
    const signature = this.typeSignature(type);
    const typeSlice = (values: RuntimeType[], destination: RuntimeType | undefined): Value => {
      if (!destination)
        return nilValue(
          this.dynamicType(frame.fn.module.registry, {
            kind: Kind.Slice,
            elem: this.namedReflectType(frame, "Type"),
          }),
        );
      return this.sliceFromValues(
        destination,
        values.map((item) => this.reflectType(frame, this.elementType(destination), item)),
      );
    };
    const descriptor = zero(outputType);
    if (descriptor.kind !== "struct")
      fail("reflect: invalid type descriptor schema", "reflect", "invalid_operation");
    const fields = descriptor.value as Map<string, Value>;
    const set = (name: string, value: Value): void => {
      const destination = fields.get(name);
      if (destination) fields.set(name, this.coerce(value, destination.type));
    };
    set("key", stringValue(frame.fn.module.registry, textEncoder.encode(type.registry.key(type))));
    set(
      "typeName",
      stringValue(
        frame.fn.module.registry,
        textEncoder.encode(type.ref.kind === Kind.Named ? type.ref.named.decl_id : ""),
      ),
    );
    set(
      "pkgPath",
      stringValue(
        frame.fn.module.registry,
        textEncoder.encode(type.ref.kind === Kind.Named ? type.ref.named.module_path : ""),
      ),
    );
    set(
      "displayName",
      stringValue(frame.fn.module.registry, textEncoder.encode(this.typeDisplay(type))),
    );
    set("variadic", boolValue(frame.fn.module.registry, signature?.variadic ?? false));
    set("align", intValue(frame.fn.module.registry, BigInt(this.reflectMeasure(type, "align"))));
    set(
      "fieldAlign",
      intValue(frame.fn.module.registry, BigInt(this.reflectMeasure(type, "field_align"))),
    );
    set("size", {
      type: primitive(frame.fn.module.registry, PrimitiveKind.PrimitiveUintptr),
      kind: "uint",
      value: BigInt(this.reflectMeasure(type, "size")),
    });
    set("bits", intValue(frame.fn.module.registry, BigInt(this.reflectMeasure(type, "bits"))));
    set(
      "chanDir",
      intValue(
        frame.fn.module.registry,
        BigInt(
          (dynamic?.direction ?? node?.direction ?? 0) === 1
            ? 3
            : (dynamic?.direction ?? node?.direction ?? 0) === 2
              ? 1
              : 2,
        ),
      ),
    );
    set("kind", {
      type: primitive(frame.fn.module.registry, PrimitiveKind.PrimitiveUint),
      kind: "uint",
      value: BigInt(this.reflectKind(type)),
    });
    const elemField = fields.get("elem");
    if (element && elemField) fields.set("elem", this.reflectType(frame, elemField.type, element));
    const keyField = fields.get("keyType");
    if (key && keyField) fields.set("keyType", this.reflectType(frame, keyField.type, key));
    set("length", intValue(frame.fn.module.registry, BigInt(dynamic?.length ?? node?.length ?? 0)));
    if (signature) {
      const inputField = fields.get("inputs");
      const outputField = fields.get("outputs");
      if (inputField) fields.set("inputs", typeSlice(signature.params, inputField.type));
      if (outputField) fields.set("outputs", typeSlice(signature.results, outputField.type));
    }
    set("comparable", boolValue(frame.fn.module.registry, this.typeComparable(type, new Set())));
    set(
      "fieldCount",
      intValue(
        frame.fn.module.registry,
        BigInt(dynamic?.fields?.length ?? node?.fields.length ?? 0),
      ),
    );
    set(
      "methodCount",
      intValue(frame.fn.module.registry, BigInt(this.methodNode(type)?.methods.length ?? 0)),
    );
    return descriptor;
  }

  private typeSignature(
    type: RuntimeType,
  ): { params: RuntimeType[]; results: RuntimeType[]; variadic: boolean } | undefined {
    if (type.dynamic?.kind === Kind.Function)
      return {
        params: type.dynamic.params ?? [],
        results: type.dynamic.results ?? [],
        variadic: type.dynamic.variadic ?? false,
      };
    const underlying = type.registry.underlying(type);
    const node = underlying.registry.node(underlying);
    if (!node?.signature) return undefined;
    return {
      params: node.signature.params.map((item) => underlying.registry.runtime(item.type)),
      results: node.signature.results.map((item) => underlying.registry.runtime(item)),
      variadic: node.signature.variadic,
    };
  }

  private typeFields(
    type: RuntimeType,
  ): { name: string; type: RuntimeType; tag: string; embedded: boolean }[] {
    if (type.dynamic?.fields) return type.dynamic.fields;
    const underlying = type.registry.underlying(type);
    const node = underlying.registry.node(underlying);
    return (node?.fields ?? []).map((field) => ({
      ...field,
      type: underlying.registry.runtime(field.type),
    }));
  }

  private typeDisplay(type: RuntimeType): string {
    if (type.ref.kind === Kind.Named)
      return `${type.ref.named.module_path}.${type.ref.named.decl_id}`;
    const underlying = type.registry.underlying(type);
    if (underlying.ref.kind === Kind.Primitive) {
      return (
        [
          "invalid",
          "bool",
          "string",
          "int",
          "int8",
          "int16",
          "int32",
          "int64",
          "uint",
          "uint8",
          "uint16",
          "uint32",
          "uint64",
          "uintptr",
          "float32",
          "float64",
          "complex64",
          "complex128",
          "error",
          "func()",
        ][underlying.ref.primitive] ?? "invalid"
      );
    }
    const element = type.registry.element(underlying);
    if (underlying.ref.kind === Kind.Pointer)
      return `*${element ? this.typeDisplay(element) : "invalid"}`;
    if (underlying.ref.kind === Kind.Slice)
      return `[]${element ? this.typeDisplay(element) : "invalid"}`;
    if (underlying.ref.kind === Kind.Array)
      return `[${underlying.dynamic?.length ?? underlying.registry.node(underlying)?.length ?? 0}]${element ? this.typeDisplay(element) : "invalid"}`;
    if (underlying.ref.kind === Kind.Map) return "map";
    if (underlying.ref.kind === Kind.Struct) return "struct";
    if (underlying.ref.kind === Kind.Function) return "func";
    if (underlying.ref.kind === Kind.Interface || underlying.ref.kind === Kind.Any)
      return "interface {}";
    return "invalid";
  }

  private typeComparable(type: RuntimeType, seen: Set<string>): boolean {
    const key = type.registry.key(type);
    if (seen.has(key)) return true;
    seen.add(key);
    const kind = type.registry.underlying(type).ref.kind;
    if (kind === Kind.Slice || kind === Kind.Map || kind === Kind.Function) return false;
    if (kind === Kind.Array) return this.typeComparable(this.elementType(type), seen);
    if (kind === Kind.Struct)
      return this.typeFields(type).every(
        (field) => field.name === "_" || this.typeComparable(field.type, seen),
      );
    return true;
  }

  private reflectConvertible(source: RuntimeType, target: RuntimeType): boolean {
    if (source.registry.identical(source, target)) return true;
    const a = source.registry.underlying(source).ref.kind;
    const b = target.registry.underlying(target).ref.kind;
    if (a === Kind.Primitive && b === Kind.Primitive) return true;
    return (
      a === b ||
      (b === Kind.Interface && this.implements(source, target)) ||
      (a === Kind.Slice && b === Kind.Pointer)
    );
  }

  private reflectConstructType(
    frame: Frame,
    instruction: PreparedInstruction,
    id: string,
    inputs: Value[],
  ): Value {
    const registry = frame.fn.module.registry;
    let type: RuntimeType;
    if (id === "reflect.pointer_to" || id === "reflect.slice_of") {
      type = this.dynamicType(registry, {
        kind: id === "reflect.pointer_to" ? Kind.Pointer : Kind.Slice,
        elem: this.reflectedType(inputs[0]!),
      });
    } else if (id === "reflect.array_of") {
      type = this.dynamicType(registry, {
        kind: Kind.Array,
        length: asIndex(inputs[0]!),
        elem: this.reflectedType(inputs[1]!),
      });
    } else if (id === "reflect.map_of") {
      const key = this.reflectedType(inputs[0]!);
      if (!this.typeComparable(key, new Set()))
        fail("reflect.MapOf: invalid key type", id, "reflect");
      type = this.dynamicType(registry, {
        kind: Kind.Map,
        key,
        elem: this.reflectedType(inputs[1]!),
      });
    } else if (id === "reflect.chan_of") {
      const direction = asIndex(inputs[0]!);
      if (direction < 1 || direction > 3) fail("reflect.ChanOf: invalid direction", id, "reflect");
      type = this.dynamicType(registry, {
        kind: Kind.Waitable,
        direction: direction === 3 ? 1 : direction === 1 ? 2 : 3,
        elem: this.reflectedType(inputs[1]!),
      });
    } else if (id === "reflect.func_of") {
      type = this.dynamicType(registry, {
        kind: Kind.Function,
        params: this.sliceValues(inputs[0]!).map((value) => this.reflectedType(value)),
        results: this.sliceValues(inputs[1]!).map((value) => this.reflectedType(value)),
        variadic: Boolean(inputs[2]!.value),
      });
    } else {
      const fields = this.sliceValues(inputs[0]!).map((field) => {
        if (field.kind !== "struct") fail("reflect.StructOf: invalid field", id, "reflect");
        return {
          name: textDecoder.decode(expectBytes(fieldValue(field, "Name"), id)),
          type: this.reflectedType(fieldValue(field, "Type")),
          tag: textDecoder.decode(expectBytes(fieldValue(field, "Tag"), id)),
          embedded: Boolean(fieldValue(field, "Anonymous").value),
        };
      });
      type = this.dynamicType(registry, { kind: Kind.Struct, fields });
    }
    return this.reflectType(frame, this.outputType(frame, instruction.operands.outputs[0]!), type);
  }

  private reflectTypeField(frame: Frame, type: RuntimeType, index: number): Value {
    const field = this.typeFields(type)[index];
    if (!field) fail("reflect: Field index out of range", "reflect.type_field", "reflect");
    const indexType = this.dynamicType(frame.fn.module.registry, {
      kind: Kind.Slice,
      elem: primitive(frame.fn.module.registry, PrimitiveKind.PrimitiveInt),
    });
    return this.reflectStruct(frame, "StructField", {
      Name: stringValue(frame.fn.module.registry, textEncoder.encode(field.name)),
      PkgPath: stringValue(
        frame.fn.module.registry,
        textEncoder.encode(/^[A-Z]/.test(field.name) ? "" : type.module),
      ),
      Type: this.reflectType(frame, this.namedReflectType(frame, "Type"), field.type),
      Tag: stringValue(frame.fn.module.registry, textEncoder.encode(field.tag)),
      Index: this.sliceFromValues(indexType, [intValue(frame.fn.module.registry, BigInt(index))]),
      Anonymous: boolValue(frame.fn.module.registry, field.embedded),
    });
  }

  private reflectMethod(
    frame: Frame,
    outputType: RuntimeType,
    type: RuntimeType,
    index: number,
  ): Value {
    const node = this.methodNode(type);
    const method = node?.methods[index];
    if (!method?.function_id)
      fail("reflect: Method index out of range", "reflect.type_method", "reflect");
    const registry = type.registry;
    const receiver = registry.runtime(method.receiver);
    const params = [
      receiver,
      ...method.signature.params.map((parameter) => registry.runtime(parameter.type)),
    ];
    const results = method.signature.results.map((result) => registry.runtime(result));
    const callableType = this.dynamicType(registry, {
      kind: Kind.Function,
      params,
      results,
      variadic: method.signature.variadic,
    });
    const functionValue: FunctionValue = {
      module: method.module_path || type.module,
      function: method.function_id,
      captures: [],
      revision: this.revision.generation,
    };
    const callable: Value = { type: callableType, kind: "function", value: functionValue };
    const value = this.reflectStruct(frame, "Method", {
      Name: stringValue(frame.fn.module.registry, textEncoder.encode(method.name)),
      PkgPath: stringValue(
        frame.fn.module.registry,
        textEncoder.encode(/^[A-Z]/.test(method.name) ? "" : type.module),
      ),
      Type: this.reflectType(frame, this.namedReflectType(frame, "Type"), callableType),
      Func: this.reflectValue(
        frame,
        this.namedReflectType(frame, "Value"),
        this.ownedView(callable),
      ),
      Index: intValue(frame.fn.module.registry, BigInt(index)),
    });
    value.type = outputType;
    return value;
  }

  private reflectViewOperation(id: string, inputs: Value[]): ReflectedView {
    let view = this.reflectedView(inputs[0]!);
    if (!view.current) fail("reflect: operation on invalid Value", id, "reflect");
    if (id === "reflect.value_elem") {
      if (view.current.kind === "pointer") {
        const target = view.current.value as Address;
        return {
          current: cloneValue(dereference(target)),
          target,
          addressable: true,
          settable: true,
          interfaceable: true,
          embeddedReadOnly: false,
        };
      }
      if (view.current.kind === "interface")
        return this.ownedView(valueFromInterface(view.current));
      fail("reflect: Elem of unsupported Value", id, "reflect");
    }
    if (id === "reflect.value_addr") {
      if (!view.addressable || !view.target)
        fail("reflect: Addr of unaddressable Value", id, "reflect");
      return this.ownedView({
        type: this.dynamicType(view.current.type.registry, {
          kind: Kind.Pointer,
          elem: view.current.type,
        }),
        kind: "pointer",
        value: view.target,
      });
    }
    if (id === "reflect.value_index") {
      const index = asIndex(inputs[1]!);
      const current = indexValue(view.current, index);
      let target: Address | undefined;
      if (view.current.kind === "slice") {
        const slice = view.current.value as SliceValue;
        target = advanceAddress(slice.storage, { kind: "index", index: slice.start + index });
      } else if (view.target && view.current.kind === "array") {
        target = advanceAddress(view.target, { kind: "index", index });
      }
      const result: ReflectedView = {
        current: cloneValue(current),
        addressable: Boolean(target),
        settable:
          Boolean(target) && view.interfaceable && (view.current.kind === "slice" || view.settable),
        interfaceable: view.interfaceable,
        embeddedReadOnly: view.embeddedReadOnly,
      };
      if (target) result.target = target;
      return result;
    }
    if (id === "reflect.value_field_by_index") {
      let accessible = view.interfaceable || view.embeddedReadOnly;
      for (const item of this.sliceValues(inputs[1]!)) {
        const index = asIndex(item);
        const field = this.typeFields(view.current.type)[index];
        if (!field || view.current.kind !== "struct")
          fail("reflect: Field index out of range", id, "reflect");
        view.current = fieldValue(view.current, field.name);
        view.declaredType = field.type;
        if (view.target)
          view.target = advanceAddress(view.target, { kind: "field", name: field.name });
        const exported = /^[A-Z]/.test(field.name);
        view.interfaceable = accessible && exported;
        view.embeddedReadOnly = accessible && !exported && field.embedded;
        accessible &&= exported || field.embedded;
      }
      view.addressable = Boolean(view.target);
      view.settable = view.addressable && view.interfaceable;
      if (!view.target) delete view.refresh;
      return view;
    }
    const low = asIndex(inputs[1]!);
    const high = asIndex(inputs[2]!);
    const max = id === "reflect.value_slice3" ? asIndex(inputs[3]!) : undefined;
    const current = view.current;
    if (current.kind !== "slice" && current.kind !== "array" && current.kind !== "string")
      fail("reflect: Slice of unsupported Value", id, "reflect");
    if (current.kind === "slice") {
      const source = current.value as SliceValue;
      const limit = max ?? source.capacity;
      if (low > high || high > limit || limit > source.capacity)
        fail("reflect: slice bounds out of range", id, "reflect");
      return this.ownedView({
        ...current,
        value: { ...source, start: source.start + low, length: high - low, capacity: limit - low },
      });
    }
    if (current.kind === "string") {
      const bytes = current.value as Uint8Array;
      if (max !== undefined || low > high || high > bytes.length)
        fail("reflect: slice bounds out of range", id, "reflect");
      return this.ownedView({ ...current, value: bytes.slice(low, high) });
    }
    const length = packedLength(current) ?? (current.value as Value[]).length;
    const limit = max ?? length;
    if (low > high || high > limit || limit > length)
      fail("reflect: slice bounds out of range", id, "reflect");
    const sliceType = this.dynamicType(current.type.registry, {
      kind: Kind.Slice,
      elem: this.elementType(current.type),
    });
    const storage = view.target ?? { root: this.managedCell(current), path: [] };
    return this.ownedView({
      type: sliceType,
      kind: "slice",
      value: {
        storage,
        start: low,
        length: high - low,
        capacity: limit - low,
      } satisfies SliceValue,
    });
  }

  private reflectResize(id: string, view: ReflectedView, amount: number): void {
    if (!view.current || view.current.kind !== "slice")
      fail("reflect: operation on non-slice Value", id, "reflect");
    const original = view.current.value as SliceValue;
    let slice = original;
    if (id === "reflect.value_set_len") {
      if (amount > slice.capacity) fail("reflect: slice length out of range", id, "reflect");
      slice = { ...slice, length: amount };
    } else if (id === "reflect.value_set_cap") {
      if (amount < slice.length || amount > slice.capacity)
        fail("reflect: slice capacity out of range", id, "reflect");
      slice = { ...slice, capacity: amount };
    } else if (slice.length + amount > slice.capacity) {
      const values = this.sliceValues(view.current);
      const capacity = Math.max(slice.length + amount, Math.max(1, slice.capacity * 2));
      const element = this.elementType(view.current.type);
      values.push(...Array.from({ length: capacity - values.length }, () => zero(element)));
      const grown = this.sliceFromValues(view.current.type, values, slice.length);
      slice = grown.value as SliceValue;
    }
    const updated = { ...view.current, value: slice };
    if (view.target) assign(view.target, updated);
    else if (view.refresh) view.refresh.write(updated);
    else Object.assign(original, slice);
  }

  private sliceFromValues(type: RuntimeType, values: Value[], length = values.length): Value {
    const element = this.elementType(type);
    const underlying = element.registry.underlying(element);
    if (
      underlying.ref.kind === Kind.Primitive &&
      underlying.ref.primitive === PrimitiveKind.PrimitiveUint8
    ) {
      const bytes = new Uint8Array(values.length);
      for (let index = 0; index < values.length; index++)
        bytes[index] = Number(values[index]!.value as bigint);
      return {
        type,
        kind: "slice",
        value: {
          storage: {
            root: this.managedCell({ type, kind: "bytes", value: bytes }),
            path: [],
          },
          start: 0,
          length,
          capacity: values.length,
        } satisfies SliceValue,
      };
    }
    return {
      type,
      kind: "slice",
      value: {
        storage: {
          root: this.managedCell({ type, kind: "array", value: values.map(cloneValue) }),
          path: [],
        },
        start: 0,
        length,
        capacity: values.length,
      } satisfies SliceValue,
    };
  }

  private sameReference(left?: Value, right?: Value): boolean {
    if (!left || !right || !left.type.registry.identical(left.type, right.type)) return false;
    if (left.kind === "nil" || right.kind === "nil") return left.kind === right.kind;
    if (left.kind === "map") return left.value === right.value;
    if (left.kind === "pointer" && right.kind === "pointer")
      return addressKey(left.value as Address) === addressKey(right.value as Address);
    if (left.kind === "slice" && right.kind === "slice") {
      const a = left.value as SliceValue;
      const b = right.value as SliceValue;
      return (
        addressKey(a.storage) === addressKey(b.storage) &&
        a.start === b.start &&
        a.length === b.length
      );
    }
    return false;
  }

  private deepEqual(left: Value, right: Value): boolean {
    const pending: [Value, Value][] = [[left, right]];
    const seen = new Set<string>();
    const seenObjects: [object, object][] = [];
    while (pending.length) {
      let [a, b] = pending.pop()!;
      if (a.kind === "interface") a = a.value as Value;
      if (b.kind === "interface") b = b.value as Value;
      if (!a.type.registry.identical(a.type, b.type) || a.kind !== b.kind) return false;
      if (a.kind === "pointer" && b.kind === "pointer") {
        const aa = a.value as Address;
        const bb = b.value as Address;
        const key = `${addressKey(aa)}\0${addressKey(bb)}`;
        if (!seen.has(key)) {
          seen.add(key);
          pending.push([dereference(aa), dereference(bb)]);
        }
      } else if (a.kind === "array") {
        const alength = packedLength(a) ?? (a.value as Value[]).length;
        const blength = packedLength(b) ?? (b.value as Value[]).length;
        if (alength !== blength) return false;
        for (let index = 0; index < alength; index++)
          pending.push([indexValue(a, index), indexValue(b, index)]);
      } else if (a.kind === "struct") {
        const av = packedFields(a) ?? (a.value as Map<string, Value>);
        const bv = packedFields(b) ?? (b.value as Map<string, Value>);
        const left = [...av];
        const right = new Map(bv);
        if (left.length !== right.size) return false;
        for (const [name, value] of left) {
          const other = right.get(name);
          if (!other) return false;
          pending.push([value, other]);
        }
      } else if (a.kind === "slice") {
        const ao = a.value as object;
        const bo = b.value as object;
        if (
          seenObjects.some(([leftObject, rightObject]) => leftObject === ao && rightObject === bo)
        )
          continue;
        seenObjects.push([ao, bo]);
        const av = this.sliceValues(a);
        const bv = this.sliceValues(b);
        if (av.length !== bv.length) return false;
        for (let index = 0; index < av.length; index++) pending.push([av[index]!, bv[index]!]);
      } else if (a.kind === "map") {
        const ao = a.value as object;
        const bo = b.value as object;
        if (
          seenObjects.some(([leftObject, rightObject]) => leftObject === ao && rightObject === bo)
        )
          continue;
        seenObjects.push([ao, bo]);
        const av = a.value as TypedMap;
        const bv = b.value as TypedMap;
        if (av.size !== bv.size) return false;
        for (const entry of av.entries()) {
          const other = bv.get(entry.key);
          if (!other) return false;
          pending.push([entry.value, other.value]);
        }
      } else if (!guestEqual(a, b)) return false;
    }
    return true;
  }

  private reflectCall(
    task: Task,
    frame: Frame,
    instruction: PreparedInstruction,
    inputs: Value[],
  ): void {
    const callable = this.reflectedView(inputs[0]!).current;
    if (
      !callable ||
      (callable.kind !== "function" &&
        callable.kind !== "method" &&
        callable.kind !== "dynamicFunction")
    )
      fail("reflect: Call of non-function Value", "reflect.value_call", "reflect");
    let arguments_ = this.sliceValues(inputs[1]!).map((value) => {
      const current = this.reflectedView(value).current;
      if (!current) fail("reflect: Call using invalid Value", "reflect.value_call", "reflect");
      return current;
    });
    const signature = this.typeSignature(callable.type);
    if (!signature)
      fail("reflect: callable signature is unavailable", "reflect.value_call", "reflect");
    const sliceCall = Boolean(inputs[2]!.value);
    if (signature.variadic && !sliceCall) {
      const fixed = signature.params.length - 1;
      if (arguments_.length < fixed)
        fail("reflect: Call with too few arguments", "reflect.value_call", "reflect");
      const variadicType = signature.params[fixed]!;
      arguments_ = [
        ...arguments_.slice(0, fixed),
        this.sliceFromValues(variadicType, arguments_.slice(fixed)),
      ];
    }
    if (callable.kind === "dynamicFunction") {
      this.callDynamicFunction(task, frame, instruction, callable, arguments_, true);
      return;
    }
    const functionValue = callable.value as FunctionValue;
    if (callable.kind === "method" && functionValue.receiver)
      arguments_ = [cloneValue(functionValue.receiver), ...arguments_];
    const revision = this.functionRevision(functionValue);
    const fn = revision.program.function(functionValue.module, functionValue.function);
    const child = this.createFrame(fn, revision, arguments_, functionValue.captures, [], false);
    child.completion = { kind: "reflectCall", outputs: instruction.operands.outputs };
    this.pushFrame(task, child);
  }

  /** Owner roots contain typed slots and stable cells; payload edges belong to the heap. */
  private *heapRoots(): IterableIterator<GuestHandle> {
    yield* this.allocationRoots;
    if (this.revision.constants) yield this.revision.constants;
    for (const task of this.tasks.values()) {
      for (const frame of task.frames) {
        if (frame.revision.constants) yield frame.revision.constants;
        for (const value of frame.inputScratch) yield* this.referencesInValue(value);
        const plan = frame.fn.references!;
        const pc = frame.returning
          ? plan.atPC.length - 1
          : Math.min(frame.rootPC ?? frame.pc, plan.atPC.length - 1);
        const map = plan.maps[plan.atPC[pc]!]!;
        const live = (index: number): boolean => Boolean(map[index >>> 5]! & (1 << (index & 31)));
        for (let index = 0; index < frame.locals.length; index++) {
          if (!live(index)) continue;
          const local = frame.locals[index];
          if (local) yield local.managedHandle!;
          else yield* frame.localBank.references(index);
        }
        const temporaryOffset = frame.locals.length;
        for (let index = 0; index < frame.fn.slotTypes.length; index++)
          if (live(temporaryOffset + index)) yield* frame.slotBank.references(index);
        const upvalueOffset = temporaryOffset + frame.fn.slotTypes.length;
        for (let index = 0; index < frame.upvalues.length; index++)
          if (live(upvalueOffset + index)) yield frame.upvalues[index]!.root.managedHandle!;
        for (const deferred of frame.defers ?? []) yield deferred.managedHandle!;
        if (frame.returning) yield* frame.returning.allReferences();
        if (frame.panic) yield frame.panic.managedHandle!;
        for (const iterator of frame.mapIterators ?? [])
          if (iterator?.map) yield iterator.map.handle;
      }
      const blocked = task.blocked;
      if (blocked?.kind === "send") yield blocked.value.managedHandle!;
      if (blocked?.kind === "send" || blocked?.kind === "receive") yield blocked.channel.handle;
      if (blocked?.kind === "reflectSelect")
        for (const selected of blocked.cases) {
          if (selected.channel) yield selected.channel.managedHandle!;
          if (selected.send) yield selected.send.managedHandle!;
        }
    }
    for (const timer of this.timers.values()) if (!timer.canceled) yield timer.channel.handle;
  }

  private collectHeap(major: boolean): void {
    const started = performance.now();
    const heap = this.guestMemory.managedHeap;
    if (major) heap.collectMajor(this.heapRoots());
    else heap.collectMinor(this.heapRoots());
    if (major) {
      this.nextMajorBytes = heap.stats().occupiedBytes * 2n + 8n * 1024n * 1024n;
      this.referencedRevisions = new Set([this.generation]);
      for (const [owner, record] of this.functionsByHandle) {
        const separator = owner.indexOf(":");
        if (separator < 1) continue;
        const handle = {
          slot: Number(owner.slice(0, separator)),
          generation: Number(owner.slice(separator + 1)),
        };
        if (heap.has(handle)) this.referencedRevisions.add(record.revision);
      }
      for (const [generation, revision] of this.revisions) {
        if (revision.constants && heap.has(revision.constants))
          this.referencedRevisions.add(generation);
        else if (generation !== this.generation) this.revisions.delete(generation);
      }
      this.reclaimStorageMetadata();
    }
    this.gcNanos += BigInt(Math.max(0, Math.floor((performance.now() - started) * 1_000_000)));
  }

  /**
   * Drop storage type wrappers that no live managed payload or frame bank can
   * decode. The numeric id slot becomes reusable; no host object graph is
   * walked and no metadata is charged as a hidden guest allocation.
   */
  private reclaimStorageMetadata(): void {
    const liveIDs = new Set<number>();
    const liveTypes = new Set<RuntimeType>();
    const heap = this.guestMemory.managedHeap;
    heap.forEachPublishedRecord((handle, stride, dynamics, count) => {
      for (let element = 0; element < count; element++) {
        const base = element * stride;
        for (const offset of dynamics) liveIDs.add(heap.readU32(handle, base + offset));
      }
    });
    for (const id of this.storageConstructionPins.keys()) liveIDs.add(id);
    for (const id of this.storageDataPins.keys()) liveIDs.add(id);
    for (const task of this.tasks.values())
      for (const frame of task.frames) {
        frame.localBank.collectDynamicTypeIDs(liveIDs);
        frame.slotBank.collectDynamicTypeIDs(liveIDs);
        frame.returning?.collectDynamicTypeIDs(liveIDs);
        frame.localBank.collectRuntimeTypes(liveTypes);
        frame.slotBank.collectRuntimeTypes(liveTypes);
        frame.returning?.collectRuntimeTypes(liveTypes);
        for (const type of frame.fn.localTypes) liveTypes.add(type);
        for (const type of frame.fn.slotTypes) liveTypes.add(type);
        for (const type of frame.fn.upvalueTypes) liveTypes.add(type);
      }
    for (const cached of this.frameCache) {
      cached.localBank.collectDynamicTypeIDs(liveIDs);
      cached.slotBank.collectDynamicTypeIDs(liveIDs);
    }
    for (const map of this.mapsByHandle.values()) map.collectRuntimeTypes(liveTypes);
    for (const channel of this.channelsByHandle.values()) channel.collectRuntimeTypes(liveTypes);
    for (const record of this.functionsByHandle.values()) {
      liveTypes.add(record.dynamicType);
      for (const type of record.captures) liveTypes.add(type);
    }
    for (const storage of this.managedStorageLayouts.values()) liveTypes.add(storage.type);
    for (const index of liveIDs) {
      const type = this.storageTypes[index - 1];
      if (type) liveTypes.add(type);
    }
    for (const [owner, keys] of this.reflectedTypeOwners) {
      const separator = owner.indexOf(":");
      const slot = Number(owner.slice(0, separator));
      const generation = Number(owner.slice(separator + 1));
      if (separator < 1 || !Number.isSafeInteger(slot) || !Number.isSafeInteger(generation))
        continue;
      const handle = { slot, generation };
      if (!heap.has(handle)) continue;
      for (const key of keys) {
        const type = this.reflectedTypes.get(key);
        if (type) liveTypes.add(type);
      }
    }
    for (let index = 0; index < this.storageTypes.length; index++) {
      const id = index + 1;
      const type = this.storageTypes[index];
      if (!type || liveIDs.has(id)) continue;
      const key = this.storageTypeKeys[index];
      if (key !== undefined) this.storageTypeIDs.delete(key);
      this.storageTypes[index] = undefined;
      this.storageTypeKeys[index] = undefined;
      this.freeStorageTypeIDs.push(id);
    }

    const liveRegistries = new Set<TypeRegistry>();
    for (const revision of this.revisions.values())
      for (const module of revision.program.moduleValues()) liveRegistries.add(module.registry);
    for (const type of liveTypes) liveRegistries.add(type.registry);
    for (const [key, type] of this.reflectedTypes) {
      if (this.pendingReflectedTypes.has(key)) continue;
      if (type.dynamic && !liveTypes.has(type)) this.reflectedTypes.delete(key);
      else if (!liveRegistries.has(type.registry)) this.reflectedTypes.delete(key);
    }
    for (const [canonical, type] of this.dynamicTypes) {
      if (liveTypes.has(type)) continue;
      const lease = this.dynamicTypeLeases.get(canonical);
      lease?.memory.release();
      lease?.resident.release();
      if (lease) this.dynamicTypeBytes -= lease.bytes;
      this.dynamicTypeLeases.delete(canonical);
      this.dynamicTypes.delete(canonical);
    }
  }
  private reflectFailure(frame: Frame, instruction: PreparedInstruction, message: string): void {
    const values = instruction.operands.outputs.map((target, index) => {
      const type = this.outputType(frame, target);
      if (index === instruction.operands.outputs.length - 2)
        return stringValue(frame.fn.module.registry, textEncoder.encode(message));
      if (index === instruction.operands.outputs.length - 1)
        return boolValue(frame.fn.module.registry, false);
      return zero(type);
    });
    this.outputs(frame, instruction.operands.outputs, values);
  }

  private callInterface(
    task: Task,
    frame: Frame,
    instruction: PreparedInstruction,
    inputs: Value[],
  ): void {
    const wrapped = inputs[0]!;
    const receiver = wrapped.kind === "interface" ? (wrapped.value as Value) : wrapped;
    const node = this.methodNode(receiver.type);
    const method = node?.methods.find((item) => item.name === instruction.method);
    if (!method?.function_id)
      fail("dynamic type has no matching method", "call_interface", "panic");
    const module = method.module_path || receiver.type.module;
    const fnValue: FunctionValue = {
      module,
      function: method.function_id,
      captures: [],
      revision: frame.revision.generation,
    };
    const revision = this.functionRevision(fnValue);
    const fn = revision.program.function(fnValue.module, fnValue.function);
    this.pushFrame(
      task,
      this.createFrame(
        fn,
        revision,
        [cloneValue(receiver), ...inputs.slice(1)],
        [],
        instruction.operands.outputs,
        false,
      ),
    );
  }

  private fireTimers(): void {
    const now = performance.now();
    for (const timer of [...this.timers.values()]) {
      if (timer.canceled || timer.deadline > now) continue;
      this.timers.delete(timer.id);
      timer.memory.release();
      if (!timer.channel.closed && timer.channel.queue.length < Math.max(1, timer.channel.capacity))
        timer.channel.queue.push(
          boolValue(this.revision.program.moduleValues().next().value!.registry, true),
        );
      this.wakeSelects();
    }
  }

  private completeTask(task: Task, values: Value[]): void {
    task.state = "done";
    const scope = this.scopes.get(task.scope)!;
    scope.tasks.delete(task.id);
    this.tasks.delete(task.id);
    if (task.scope === 0) {
      this.ready = true;
      if (scope.tasks.size === 0) this.scopes.delete(0);
    }
    if (scope.execution !== undefined) {
      const execution = this.executions.get(scope.execution)!;
      if (scope.rootTask === task.id && execution.state === "Running") {
        execution.state = "Completed";
        execution.result = snapshot(values);
      }
      if (scope.tasks.size === 0) {
        execution.settled = true;
        if (scope.error) execution.scopeError = scope.error;
        else delete execution.scopeError;
      }
    }
    task.memory?.release();
    delete task.memory;
  }

  private failTask(task: Task, error: VMError): void {
    task.state = error.code === "canceled" ? "canceled" : "failed";
    task.error = error;
    const initialization = task.frames.find((frame) => frame.initializing)?.initializing;
    if (initialization || task.scope === 0) {
      if (initialization) this.initializing.delete(initialization);
      if (!(this.closing && error.code === "canceled")) this.initializationError = error;
      this.closing = true;
    }
    const scope = this.scopes.get(task.scope);
    scope?.tasks.delete(task.id);
    this.tasks.delete(task.id);
    this.releaseBlocked(task);
    for (const frame of task.frames) this.recycleFrame(frame);
    task.frames.length = 0;
    if (task.scope === 0 && scope?.tasks.size === 0) this.scopes.delete(0);
    if (scope?.execution !== undefined) {
      const execution = this.executions.get(scope.execution)!;
      if (scope.rootTask === task.id && execution.state === "Running") {
        execution.state = task.state === "canceled" ? "Canceled" : "Failed";
        execution.error = error;
      } else scope.error ??= error;
      if (scope.tasks.size === 0) {
        execution.settled = true;
        if (scope.error) execution.scopeError = scope.error;
        else delete execution.scopeError;
      }
    }
    task.memory?.release();
    delete task.memory;
  }

  result(id: number): Snapshot {
    const execution = this.executions.get(id);
    if (!execution) throw new VMError("unknown execution", "missing_execution", "execution");
    if (execution.error) throw execution.error;
    if (!execution.result) throw new VMError("execution is not complete", "pending", "execution");
    return execution.result;
  }

  release(id: number): void {
    const execution = this.executions.get(id);
    if (!execution) return;
    this.executions.delete(id);
    this.scopes.delete(execution.scope);
  }

  cancel(id: number): void {
    const execution = this.executions.get(id);
    if (!execution || execution.settled) return;
    for (const taskID of [...(this.scopes.get(execution.scope)?.tasks ?? [])]) {
      const task = this.tasks.get(taskID);
      if (task) this.failTask(task, new VMError("execution canceled", "canceled", "execution"));
    }
  }

  pause(): void {
    this.paused = true;
  }

  resume(): void {
    this.paused = false;
  }

  async patch(image: Uint8Array): Promise<void> {
    if (this.initializing.size)
      throw new VMError("module initialization is active", "initialization_active", "patch");
    this.collectHeap(true);
    const retainedLimit = this.options.limits?.maxRetainedRevisions ?? 16;
    const retained = [...this.referencedRevisions].filter(
      (generation) => generation !== this.generation && this.revisions.has(generation),
    ).length;
    if (retained + 2 > retainedLimit)
      throw new VMError("retained revision limit exceeded", "resource_limit", "patch");
    const program = await Program.load(image, loadOptions(this.options));
    const current = this.revision.program;
    if (program.hash === current.hash)
      throw new VMError("target is already current", "unchanged", "patch");
    if (program.root !== current.root)
      throw new VMError("root module changed", "root_changed", "patch");
    if (
      program.targetTags.length !== current.targetTags.length ||
      program.targetTags.some((tag, index) => tag !== current.targetTags[index])
    )
      throw new VMError("build target changed", "target_changed", "patch");
    const capabilities = new Set(this.options.capabilities ?? []);
    if (program.capabilities.some((capability) => !capabilities.has(capability)))
      throw new VMError("host capability is unavailable", "capability_unavailable", "patch");

    if (this.generation >= Number.MAX_SAFE_INTEGER)
      throw new VMError("revision generation exhausted", "revision_limit", "patch");
    const revision: Revision = { generation: this.generation + 1, program };
    // Preserve global cells whose identity and layout match. New code binds to
    // these cells while active frames keep their old revision.
    for (const module of program.moduleValues()) {
      const old = this.revision.program.module(module.path);
      if (!old) continue;
      for (const [id, index] of module.globalIndexes) {
        const prior = old.globalIndexes.get(id);
        if (
          prior !== undefined &&
          old.globals[prior]!.read().type.registry.identical(
            old.globals[prior]!.read().type,
            module.globals[index]!.read().type,
          )
        )
          module.globals[index] = old.globals[prior]!;
      }
    }
    this.attachManagedGlobals(revision);
    this.generation = revision.generation;
    this.revision = revision;
    this.revisions.set(this.generation, revision);
    this.collectHeap(true);
    for (const generation of this.revisions.keys())
      if (!this.referencedRevisions.has(generation)) this.revisions.delete(generation);
    this.epoch++;
  }

  stack(): PublicFrame[] {
    const task = [...this.tasks.values()].find(
      (item) => item.state === "runnable" || item.state === "blocked",
    );
    if (!task) return [];
    return [...task.frames].reverse().map((frame, depth) => {
      const pc = this.paused ? frame.pc : Math.max(0, frame.pc - 1);
      const info =
        frame.revision.program.hash === this.symbolProgramHash
          ? this.debugFunctions.get(`${frame.fn.module.path}\0${frame.fn.id}`)
          : undefined;
      const lastWirePC = frame.fn.executableToWirePC[frame.fn.executableToWirePC.length - 1] ?? 0;
      return {
        reference: { epoch: this.epoch, task: BigInt(task.id), depth: BigInt(depth) },
        generation: BigInt(frame.revision.generation),
        scope: BigInt(task.scope),
        program_hash: frame.revision.program.hash,
        module: frame.fn.module.path,
        function: info?.name ?? frame.fn.id,
        pc: BigInt(frame.fn.executableToWirePC[pc] ?? lastWirePC),
        locations: (info?.locations.get(pc) ?? []).map((location) => ({
          file: location.file,
          line: BigInt(location.line),
          column: BigInt(location.column),
        })),
      };
    });
  }

  bindings(reference: FrameRef): Bindings {
    if (reference.epoch !== this.epoch)
      throw new VMError("stale frame reference", "stale", "debug");
    const task = this.tasks.get(Number(reference.task));
    const frame = task?.frames[task.frames.length - 1 - Number(reference.depth)];
    if (!frame) throw new VMError("frame is unavailable", "stale", "debug");
    const symbols =
      frame.revision.program.hash === this.symbolProgramHash
        ? this.debugFunctions.get(`${frame.fn.module.path}\0${frame.fn.id}`)
        : undefined;
    const names = [...frame.fn.localIndexes.keys()].map(
      (name) => symbols?.localNames.get(name) ?? name,
    );
    return {
      ...snapshot(
        frame.locals.map((local, index) =>
          cloneValue(localValue(local ?? frame.localBank.read(index)!)),
        ),
      ),
      names,
    };
  }

  breakpoints(module: string, file: string, lines: (number | bigint)[]): bigint[] {
    const prefix = `${module}\0${file}\0`;
    for (const key of [...this.debugBreakpoints])
      if (key.startsWith(prefix)) this.debugBreakpoints.delete(key);
    const available = new Set<number>();
    for (const [key, fn] of this.debugFunctions) {
      if (!key.startsWith(`${module}\0`)) continue;
      for (const locations of fn.locations.values())
        for (const location of locations) if (location.file === file) available.add(location.line);
    }
    const actual: bigint[] = [];
    for (const requested of lines) {
      const line = Number(requested);
      if (!Number.isSafeInteger(line) || line <= 0 || !available.has(line)) continue;
      this.debugBreakpoints.add(`${prefix}${line}`);
      actual.push(BigInt(line));
    }
    return actual;
  }

  debugOpen(
    sources: Record<string, { module: string; path: string; text: string }>,
    entry: string,
  ): void {
    if (this.debugConfigured)
      throw new VMError("debug target is already running", "invalid_request", "debug");
    this.debugSources.clear();
    for (const [path, source] of Object.entries(sources)) this.debugSources.set(path, source);
    this.debugEntry = entry;
  }

  debugRequest(command: string, arguments_: Record<string, unknown>): Record<string, unknown> {
    if (command === "initialize")
      return {
        supportsConfigurationDoneRequest: true,
        supportsTerminateRequest: true,
        supportsDelayedStackTraceLoading: true,
      };
    if (command === "disconnect" || command === "terminate") {
      if (this.debugExecution !== undefined) this.cancel(this.debugExecution);
      this.debugConfigured = false;
      return {};
    }
    if (command === "source") {
      const source = arguments_.source as { path?: unknown } | undefined;
      const path = typeof source?.path === "string" ? source.path : "";
      const value = this.debugSources.get(path);
      if (!value) throw new VMError("unknown source", "invalid_request", "debug.source");
      return { content: value.text, mimeType: "text/x-go" };
    }
    if (command === "setBreakpoints") {
      const source = arguments_.source as { path?: unknown } | undefined;
      const path = typeof source?.path === "string" ? source.path : "";
      const value = this.debugSources.get(path);
      if (!value) throw new VMError("unknown source path", "invalid_request", "debug.breakpoints");
      const requested = Array.isArray(arguments_.breakpoints)
        ? arguments_.breakpoints.flatMap((item) => {
            const line =
              item && typeof item === "object" ? (item as { line?: unknown }).line : undefined;
            return typeof line === "number" || typeof line === "bigint" ? [line] : [];
          })
        : [];
      const actual = new Set(this.breakpoints(value.module, value.path, requested).map(Number));
      return {
        breakpoints: requested.map((line) => ({
          line: Number(line),
          verified: actual.has(Number(line)),
          source: { path },
        })),
      };
    }
    if (command === "configurationDone") {
      if (this.debugConfigured)
        throw new VMError("configuration already completed", "invalid_request", "debug");
      this.debugExecution = this.start(this.debugEntry, []);
      this.debugConfigured = true;
      this.debugTerminated = false;
      return {};
    }
    if (command === "threads") {
      return {
        threads: [...this.tasks.values()].map((task) => ({
          id: task.id,
          name: `task ${task.id} (${task.state})`,
        })),
      };
    }
    if (command === "stackTrace") {
      const taskID = Number(arguments_.threadId);
      const task = this.tasks.get(taskID);
      if (!task) throw new VMError("unknown thread", "invalid_request", "debug.thread");
      const all = [...task.frames].reverse();
      const start = Math.max(0, Number(arguments_.startFrame ?? 0));
      const levels = Math.min(1000, Math.max(1, Number(arguments_.levels ?? 100)));
      const frames = all.slice(start, start + levels).map((frame, offset) => {
        const depth = start + offset;
        const reference = { epoch: this.epoch, task: BigInt(task.id), depth: BigInt(depth) };
        const handle = this.nextDebugHandle++;
        this.debugFrames.set(handle, reference);
        const pc = frame.pc;
        const info =
          frame.revision.program.hash === this.symbolProgramHash
            ? this.debugFunctions.get(`${frame.fn.module.path}\0${frame.fn.id}`)
            : undefined;
        const location = info?.locations.get(pc)?.[0];
        const source = location
          ? ([...this.debugSources.entries()].find(
              ([, candidate]) =>
                candidate.module === frame.fn.module.path && candidate.path === location.file,
            )?.[0] ?? location.file)
          : "";
        return {
          id: handle,
          name: info?.name ?? frame.fn.id,
          source: { path: source },
          line: location?.line ?? 1,
          column: location?.column ?? 1,
        };
      });
      return { stackFrames: frames, totalFrames: all.length };
    }
    if (command === "scopes") {
      const frame = this.debugFrames.get(Number(arguments_.frameId));
      if (!frame) throw new VMError("stale frame", "invalid_request", "debug.frame");
      const handle = this.nextDebugHandle++;
      this.debugVariables.set(handle, frame);
      return { scopes: [{ name: "Locals", variablesReference: handle, expensive: false }] };
    }
    if (command === "variables") {
      const reference = this.debugVariables.get(Number(arguments_.variablesReference));
      if (!reference)
        throw new VMError("stale variable reference", "invalid_request", "debug.variables");
      const values = this.bindings(reference);
      const start = Math.max(0, Number(arguments_.start ?? 0));
      const count = Math.min(1000, Math.max(1, Number(arguments_.count ?? 100)));
      return {
        variables: values.roots.slice(start, start + count).map((value, index) => ({
          name: values.names[start + index] ?? String(start + index),
          value: debugValueSummary(value),
          variablesReference: 0,
        })),
      };
    }
    if (command === "pause") {
      const task = this.tasks.get(Number(arguments_.threadId)) ?? this.tasks.values().next().value;
      if (task) this.stopForDebugging("pause", task);
      return {};
    }
    if (["continue", "next", "stepIn", "stepOut"].includes(command)) {
      const task = this.tasks.get(Number(arguments_.threadId)) ?? this.tasks.values().next().value;
      if (!task) throw new VMError("unknown thread", "invalid_request", "debug.thread");
      this.paused = false;
      this.epoch++;
      this.debugFrames.clear();
      this.debugVariables.clear();
      this.debugStepTask = command === "continue" ? undefined : task.id;
      return { allThreadsContinued: true };
    }
    throw new VMError("unsupported DAP request", "invalid_request", "debug.command");
  }
  debugEvents(): Record<string, unknown>[] {
    if (
      this.debugConfigured &&
      !this.debugTerminated &&
      this.debugExecution !== undefined &&
      this.executions.get(this.debugExecution)?.settled
    ) {
      this.debugTerminated = true;
      this.debugEventsQueue.push({ event: "terminated", body: {} });
    }
    return this.debugEventsQueue.splice(0);
  }
  debugOutput(category: string, text: string): void {
    this.debugEventsQueue.push({ event: "output", body: { category, output: text } });
  }

  stats(): Stats {
    if (this.closed) this.guestMemory.close();
    const memory = this.guestMemory.stats();
    const stats: Stats = {
      state: this.closed ? "Closed" : this.closing ? "Closing" : this.paused ? "Paused" : "Running",
      steps: this.stepsOverflow ?? BigInt(this.steps),
      ...memory,
      gcNanos: this.gcNanos,
      activeScopes: BigInt(this.scopes.size),
      tasks: BigInt(this.tasks.size),
      timers: BigInt([...this.timers.values()].filter((timer) => !timer.canceled).length),
      ffiCalls: this.ffiCalls,
      ffiBytes: this.ffiBytes,
      generation: BigInt(this.generation),
    };
    if (this.options.profile)
      stats.profile = {
        dispatches: this.profileDispatches,
        opcodes: Object.fromEntries(this.opcodeProfile),
        functions: Object.fromEntries(
          Array.from(this.functionProfile, ([key, value]) => [key, { ...value }]),
        ),
      };
    return stats;
  }

  private finishClose(): void {
    if (this.closed) return;
    for (const timer of this.timers.values()) timer.memory.release();
    this.timers.clear();
    for (const lease of this.dynamicTypeLeases.values()) {
      lease.memory.release();
      lease.resident.release();
    }
    this.dynamicTypeLeases.clear();
    this.dynamicTypes.clear();
    this.dynamicTypeBytes = 0;
    this.reflectedTypes.clear();
    this.reflectedTypeOwners.clear();
    this.pendingReflectedTypes.clear();
    this.storageTypeIDs.clear();
    this.storageConstructionPins.clear();
    this.storageDataPins.clear();
    this.storageConstructionOwners.length = 0;
    this.storageConstructionDepth = 0;
    this.storageTypes.length = 0;
    this.storageTypeKeys.length = 0;
    this.freeStorageTypeIDs.length = 0;
    this.managedCells.clear();
    this.managedCellsByHandle.clear();
    this.managedStorageLayouts.clear();
    this.mapsByHandle.clear();
    this.channelsByHandle.clear();
    this.functionsByHandle.clear();
    this.allocationRoots.length = 0;
    this.closed = true;
    this.guestMemory.close();
    this.pendingActions.push({ kind: "close" });
  }

  close(): void {
    if (this.closing) return;
    this.closing = true;
    for (const storage of this.frameCache) storage.resident.release();
    this.frameCache.length = 0;
    this.frameCacheBytes = 0;
    // WeakMap entries retain their bucket arrays while the VM is retained by
    // a caller. Drop the whole index so cached frame payloads cannot survive
    // close through a PreparedFunction key held by the caller.
    this.frameCacheByFunction = new WeakMap();
    for (const execution of this.executions.values()) this.cancel(execution.id);
    for (const taskID of [...(this.scopes.get(0)?.tasks ?? [])]) {
      const task = this.tasks.get(taskID);
      if (task) this.failTask(task, new VMError("VM closed", "canceled", "vm"));
    }
    for (const [id, pending] of this.pendingHost) {
      if (pending.cancelSent) continue;
      pending.cancelSent = true;
      this.pendingActions.push({ kind: "cancel", id });
    }
    if (!this.tasks.size && !this.pendingHost.size) this.finishClose();
  }
}

function loadOptions(options: VMOptions): LoadOptions {
  return {
    maxImageBytes: options.load?.maxImageBytes ?? DEFAULT_MAX_IMAGE_BYTES,
    maxArtifactBytes: options.load?.maxArtifactBytes ?? 64 * 1024 * 1024,
    maxPackages: options.load?.maxPackages ?? 1024,
    maxTypeNodes: options.load?.maxTypeNodes ?? 100_000,
  };
}

function requiredIndex(indexes: ReadonlyMap<string, number>, value: string, kind: string): number {
  const index = indexes.get(value);
  if (index === undefined) invalid(`unknown ${kind}`, value);
  return index;
}

function debugValueSummary(value: HostValue): string {
  const data = value.data;
  if (data === "Nil") return "nil";
  if ("Bool" in data) return String(data.Bool);
  if ("Integer" in data) return data.Integer.toString();
  if ("Unsigned" in data) return data.Unsigned.toString();
  if ("FloatBits" in data) return `float(${data.FloatBits})`;
  if ("ComplexBits" in data) return `complex(${data.ComplexBits.real}, ${data.ComplexBits.imag})`;
  if ("String" in data) return JSON.stringify(textDecoder.decode(data.String));
  if ("Array" in data) return `[${data.Array.length}]`;
  if ("Struct" in data) return `{${Object.keys(data.Struct).join(", ")}}`;
  if ("Map" in data) return `map(${data.Map})`;
  if ("MapEntries" in data) return `map[${data.MapEntries.length}]`;
  if ("Slice" in data) return `slice(${data.Slice.length}/${data.Slice.capacity})`;
  if ("Pointer" in data) return `pointer(${data.Pointer.object})`;
  if ("Channel" in data) return `channel(${data.Channel.queued.length}/${data.Channel.capacity})`;
  if ("Resource" in data) return `resource(${data.Resource})`;
  return "value";
}

function invalid(message: string, path: string): never {
  throw new VMError(message, "invalid_operation", path);
}

function emptyKey(): { module_path: string; decl_id: string } {
  return { module_path: "", decl_id: "" };
}

function valueFromInterface(value: Value): Value {
  return value.kind === "interface" ? (value.value as Value) : value;
}

function addressKey(address: Address): string {
  const location = storageAddress(address);
  if (location) return `${location.handle.slot}:${location.handle.generation}:${location.offset}`;
  return `${address.root.id}:${address.path
    .map((segment) => {
      if (segment.kind === "field") return `f:${segment.name}`;
      if (segment.kind === "index") return `i:${segment.index}`;
      if (segment.kind === "arrayView") return `a:${segment.start}:${segment.length}`;
      return `t:${segment.type.registry.key(segment.type)}`;
    })
    .join("/")}`;
}

function expectBytes(value: Value, path: string): Uint8Array {
  if (value.kind !== "string" && value.kind !== "bytes") invalid("expected bytes", path);
  return value.value as Uint8Array;
}

function formatPanic(value: Value): string {
  if (value.kind === "string") return textDecoder.decode(value.value as Uint8Array);
  return String(value.value ?? "panic");
}

function toVMError(error: unknown): VMError {
  if (error instanceof VMError) return error;
  if (error instanceof Error) {
    const wrapped = new VMError(error.message, "internal", "runtime");
    wrapped.cause = error;
    return wrapped;
  }
  return new VMError(String(error), "internal", "runtime");
}

function floatBits(value: number, width: 32 | 64): bigint {
  const buffer = new ArrayBuffer(width / 8);
  const view = new DataView(buffer);
  if (width === 32) {
    view.setFloat32(0, value, true);
    return BigInt(view.getUint32(0, true));
  }
  view.setFloat64(0, value, true);
  return view.getBigUint64(0, true);
}

function bitsFloat(value: bigint, width: 32 | 64): number {
  const buffer = new ArrayBuffer(width / 8);
  const view = new DataView(buffer);
  if (width === 32) {
    view.setUint32(0, Number(BigInt.asUintN(32, value)), true);
    return view.getFloat32(0, true);
  }
  view.setBigUint64(0, BigInt.asUintN(64, value), true);
  return view.getFloat64(0, true);
}

function sha256Block(state: Value, bytes: Uint8Array): Value {
  if (
    state.kind !== "array" ||
    (packedLength(state) ?? (state.value as Value[]).length) !== 8 ||
    bytes.length % 64 !== 0
  )
    invalid("sha256 state layout is invalid", "crypto.sha256.block");
  const stateValues = Array.from({ length: 8 }, (_, index) => indexValue(state, index));
  const hash = stateValues.map((word) => {
    if (word.kind !== "uint") invalid("sha256 state word is invalid", "crypto.sha256.block");
    return Number(BigInt.asUintN(32, word.value as bigint));
  });
  const constants = [
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
  ];
  const rotate = (value: number, count: number): number =>
    (value >>> count) | (value << (32 - count));
  for (let offset = 0; offset < bytes.length; offset += 64) {
    const words = new Uint32Array(64);
    for (let index = 0; index < 16; index++) {
      const base = offset + index * 4;
      words[index] =
        ((bytes[base]! << 24) |
          (bytes[base + 1]! << 16) |
          (bytes[base + 2]! << 8) |
          bytes[base + 3]!) >>>
        0;
    }
    for (let index = 16; index < 64; index++) {
      const a = words[index - 15]!;
      const b = words[index - 2]!;
      const s0 = rotate(a, 7) ^ rotate(a, 18) ^ (a >>> 3);
      const s1 = rotate(b, 17) ^ rotate(b, 19) ^ (b >>> 10);
      words[index] = (words[index - 16]! + s0 + words[index - 7]! + s1) >>> 0;
    }
    let [a, b, c, d, e, f, g, h] = hash;
    for (let index = 0; index < 64; index++) {
      const sum1 = rotate(e!, 6) ^ rotate(e!, 11) ^ rotate(e!, 25);
      const choice = (e! & f!) ^ (~e! & g!);
      const first = (h! + sum1 + choice + constants[index]! + words[index]!) >>> 0;
      const sum0 = rotate(a!, 2) ^ rotate(a!, 13) ^ rotate(a!, 22);
      const majority = (a! & b!) ^ (a! & c!) ^ (b! & c!);
      const second = (sum0 + majority) >>> 0;
      h = g;
      g = f;
      f = e;
      e = (d! + first) >>> 0;
      d = c;
      c = b;
      b = a;
      a = (first + second) >>> 0;
    }
    hash[0] = (hash[0]! + a!) >>> 0;
    hash[1] = (hash[1]! + b!) >>> 0;
    hash[2] = (hash[2]! + c!) >>> 0;
    hash[3] = (hash[3]! + d!) >>> 0;
    hash[4] = (hash[4]! + e!) >>> 0;
    hash[5] = (hash[5]! + f!) >>> 0;
    hash[6] = (hash[6]! + g!) >>> 0;
    hash[7] = (hash[7]! + h!) >>> 0;
  }
  return {
    ...state,
    value: hash.map((word, index) => ({
      ...stateValues[index]!,
      kind: "uint" as const,
      value: BigInt(word),
    })),
  };
}
