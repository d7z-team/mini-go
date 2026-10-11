import type {
  Bindings,
  Frame as PublicFrame,
  FrameRef,
  HostValue,
  Snapshot,
  Stats,
  VMOptions,
} from "./types.js";
import { Kind, PrimitiveKind } from "./contract.generated.js";
import { VMError, fail } from "./json.js";
import { binary, convert, unary } from "./operators.js";
import {
  Program,
  type LoadOptions,
  type PreparedFunction,
  type PreparedInstruction,
} from "./program.js";
import {
  asIndex,
  assign,
  assertComparable,
  boolValue,
  cell,
  cloneValue,
  dereference,
  fieldValue,
  guestEqual,
  importHostValue,
  indexValue,
  intValue,
  nilValue,
  primitive,
  setIndex,
  snapshot,
  stringValue,
  zero,
  type Address,
  type AddressPath,
  type Cell,
  type Channel,
  type DynamicType,
  type FunctionValue,
  type GuestMap,
  type RuntimeType,
  type SliceValue,
  type TypeNode,
  type Value,
} from "./value.js";
import { GUEST_COST, GuestMemory } from "./guest-memory.js";

const LOCAL_OUTPUT = 0x80000000;
const DEFAULT_MAX_IMAGE_BYTES = 64 * 1024 * 1024;
const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();
const strictTextDecoder = new TextDecoder("utf-8", { fatal: true });

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
  readonly locals: Cell[];
  readonly upvalues: Address[];
  readonly slots: (Value | undefined)[];
  readonly mapIterators: Map<
    string,
    { map: GuestMap; entries: { key: Value; value: Value }[]; offset: number }
  >;
  readonly returnOutputs: readonly number[];
  pc: number;
  defers: FunctionValue[];
  returning?: Value[];
  panic?: Value;
  recovered: boolean;
  deferred: boolean;
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

interface Task {
  readonly id: number;
  readonly scope: number;
  frames: Frame[];
  state: "runnable" | "blocked" | "done" | "failed" | "canceled";
  blocked?:
    | { kind: "send"; channel: Channel; value: Value }
    | {
        kind: "receive";
        channel: Channel;
        outputs: readonly number[];
        receiveOK: boolean;
        reflected?: boolean;
      }
    | { kind: "select" }
    | { kind: "reflectSelect"; cases: ReflectedSelectCase[]; outputs: readonly number[] }
    | { kind: "ffi"; id: number };
  result?: Value[];
  error?: VMError;
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
}

interface PendingHostCall {
  task: number;
  outputs: readonly number[];
  resultType: RuntimeType;
}

interface TimerRecord {
  readonly id: number;
  readonly channel: Channel;
  readonly deadline: number;
  canceled: boolean;
}

interface ReflectedView {
  current?: Value;
  target?: Address;
  refresh?: Cell;
  addressable: boolean;
  settable: boolean;
  interfaceable: boolean;
  embeddedReadOnly: boolean;
}

interface ReflectedSelectCase {
  index: number;
  channel?: Value;
  send?: Value;
  receive: boolean;
  default: boolean;
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

interface MemoryCensus {
  liveHeapBytes: bigint;
  liveHeapObjects: bigint;
  liveGuestBytes: bigint;
  strings: number;
  maxSequence: number;
  scannedEdges: bigint;
  revisions: Set<number>;
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
  private readonly initialized = new Set<string>();
  private readonly initializing = new Set<string>();
  private readonly options: VMOptions;
  private readonly enforceMemoryLimitsEveryStep: boolean;
  private paused = false;
  private closing = false;
  private closed = false;
  private ready = false;
  private initializationError?: VMError;
  private steps = 0n;
  private ffiCalls = 0n;
  private ffiBytes = 0n;
  private epoch = 1n;
  private readonly guestMemory: GuestMemory;
  private gcCollections = 0n;
  private gcScannedObjects = 0n;
  private gcTracedObjects = 0n;
  private gcScannedEdges = 0n;
  private gcReclaimedBytes = 0n;
  private gcNanos = 0n;
  private referencedRevisions = new Set<number>();
  private readonly dynamicTypes = new Map<string, RuntimeType>();
  private dynamicTypeBytes = 0;
  private readonly debugFunctions = new Map<string, FunctionDebugInfo>();
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
  private readonly functionProfile = new Map<
    string,
    { calls: bigint; steps: bigint; nanos: bigint }
  >();

  readonly maxInputBytes: number;
  readonly maxHostResultBytes: number;
  readonly maxImageBytes: number;

  private constructor(program: Program, options: VMOptions) {
    this.options = options;
    this.guestMemory = new GuestMemory(options.limits);
    const limits = options.limits;
    this.enforceMemoryLimitsEveryStep = Boolean(
      limits &&
        (limits.maxObjects !== undefined ||
          limits.maxHeapBytes !== undefined ||
          limits.maxAllocatedBytes !== undefined ||
          limits.maxStringBytes !== undefined ||
          limits.maxValueDepth !== undefined ||
          limits.maxSequenceElements !== undefined),
    );
    this.revision = { generation: this.generation, program };
    this.revisions.set(this.generation, this.revision);
    this.maxInputBytes = options.limits?.maxFfiBytes ?? 8 * 1024 * 1024;
    this.maxHostResultBytes = options.limits?.maxFfiResultBytes ?? 8 * 1024 * 1024;
    this.maxImageBytes = options.load?.maxImageBytes ?? DEFAULT_MAX_IMAGE_BYTES;
    this.initializeRoot();
    this.updateMemoryCensus(false);
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
    const module = this.revision.program.modules.get(root);
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
            if (points.length) info.locations.set(location.pc as number, points);
          }
        this.debugFunctions.set(`${modulePath}\0${fn.id}`, info);
      }
    }
  }

  start(entryName: string, arguments_: HostValue[]): number {
    if (!this.ready || this.closing)
      throw new VMError("instance is unavailable", "closed", "instance");
    const entry = this.revision.program.entries.get(entryName);
    if (!entry) throw new VMError(`unknown entry ${entryName}`, "missing_entry", "entry");
    const fn = this.revision.program.function(entry.module, entry.function);
    if (arguments_.length !== fn.signature.params.length)
      throw new VMError("entry argument count mismatch", "invalid_argument", "arguments");
    const values = arguments_.map((value) => importHostValue(value, fn.module.registry));
    const scopeID = this.nextScope++;
    const executionID = scopeID;
    this.scopes.set(scopeID, { tasks: new Set(), execution: executionID });
    this.executions.set(executionID, {
      id: executionID,
      scope: scopeID,
      state: "Running",
      settled: false,
    });
    const task = this.createTask(scopeID, fn, values, []);
    this.scopes.get(scopeID)!.rootTask = task.id;
    try {
      this.updateMemoryCensus(true);
    } catch (error) {
      this.failTask(task, toVMError(error));
    }
    return executionID;
  }

  private createTask(
    scope: number,
    fn: PreparedFunction,
    arguments_: Value[],
    captures: Address[],
  ): Task {
    const maxTasks = this.options.limits?.maxTasks ?? 4096;
    if (this.tasks.size >= maxTasks)
      throw new VMError("task limit exceeded", "task_limit", "scheduler");
    const task: Task = {
      id: this.nextTask++,
      scope,
      frames: [this.createFrame(fn, this.revision, arguments_, captures, [], false)],
      state: "runnable",
    };
    this.tasks.set(task.id, task);
    this.scopes.get(scope)!.tasks.add(task.id);
    this.runnable.push(task.id);
    return task;
  }

  private createFrame(
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
    if (this.options.profile) {
      const key = `${fn.module.path}.${fn.id}`;
      const profile = this.functionProfile.get(key) ?? { calls: 0n, steps: 0n, nanos: 0n };
      profile.calls++;
      this.functionProfile.set(key, profile);
    }
    const locals = fn.localTypes.map((type) => cell(zero(type)));
    for (let index = 0; index < arguments_.length; index++)
      locals[index]!.value = this.coerce(arguments_[index]!, fn.localTypes[index]!);
    return {
      fn,
      revision,
      locals,
      upvalues: captures,
      slots: new Array(fn.slotTypes.length),
      mapIterators: new Map(),
      returnOutputs,
      pc: 0,
      defers: [],
      recovered: false,
      deferred,
    };
  }

  private pushFrame(task: Task, frame: Frame): void {
    if (task.frames.length >= (this.options.limits?.maxFrames ?? 1024))
      throw new VMError("frame limit exceeded", "frame_limit", frame.fn.id);
    task.frames.push(frame);
  }

  private checkSequence(length: number, path: string): void {
    if (length > (this.options.limits?.maxSequenceElements ?? 1_000_000))
      throw new VMError("sequence element limit exceeded", "allocation_limit", path);
  }

  pump(budget: number): VMPumpState {
    if (this.closed) return this.pumpState(false);
    this.fireTimers();
    if (this.paused) return this.pumpState(false);
    for (let count = 0; count < budget && this.runnableHead < this.runnable.length; count++) {
      const id = this.runnable[this.runnableHead++]!;
      const task = this.tasks.get(id);
      if (!task || task.state !== "runnable") continue;
      if (this.shouldStopBeforeInstruction(task)) {
        this.runnableHead--;
        break;
      }
      try {
        const frame = task.frames[task.frames.length - 1];
        const instruction = frame?.fn.instructions[frame.pc];
        const profileKey =
          this.options.profile && frame ? `${frame.fn.module.path}.${frame.fn.id}` : "";
        const profileStarted = profileKey ? performance.now() : 0;
        if (instruction && profileKey)
          this.opcodeProfile.set(
            instruction.name,
            (this.opcodeProfile.get(instruction.name) ?? 0n) + 1n,
          );
        try {
          this.step(task);
        } finally {
          if (profileKey) {
            const profile = this.functionProfile.get(profileKey)!;
            profile.steps++;
            profile.nanos += BigInt(
              Math.max(0, Math.floor((performance.now() - profileStarted) * 1_000_000)),
            );
          }
        }
        if (this.enforceMemoryLimitsEveryStep || (this.steps & 63n) === 0n)
          this.updateMemoryCensus(true);
      } catch (error) {
        const runtimeError = toVMError(error);
        if (runtimeError.code === "panic" && task.frames.length) {
          const frame = task.frames[task.frames.length - 1]!;
          frame.panic = stringValue(
            frame.fn.module.registry,
            textEncoder.encode(runtimeError.message),
          );
          frame.returning = [];
        } else {
          this.failTask(task, runtimeError);
        }
      }
      if (this.debugStepTask === id && this.tasks.has(id)) {
        this.debugStepTask = undefined;
        this.stopForDebugging("step", task);
      }
      if (task.state === "runnable") this.runnable.push(id);
      if (this.paused) break;
    }
    if (this.runnableHead) {
      this.runnable.splice(0, this.runnableHead);
      this.runnableHead = 0;
    }
    if (this.closing && this.tasks.size === 0 && this.pendingHost.size === 0) {
      this.closed = true;
      this.guestMemory.release();
      this.pendingActions.push({ kind: "close" });
    }
    return this.pumpState(this.runnable.length > 0);
  }

  private shouldStopBeforeInstruction(task: Task): boolean {
    if (!this.debugConfigured || !task.frames.length) return false;
    const current = task.frames[task.frames.length - 1]!;
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

  private step(task: Task): void {
    if (!task.frames.length) {
      this.completeTask(task, []);
      return;
    }
    const configuredSteps = this.options.limits?.maxSteps;
    const maxSteps = configuredSteps === 0 || configuredSteps === 0n ? undefined : configuredSteps;
    if (maxSteps !== -1 && maxSteps !== -1n) {
      const limit = BigInt(maxSteps ?? 100_000_000);
      if (this.steps >= limit) throw new VMError("step limit exceeded", "step_limit", "execution");
    }
    const frame = task.frames[task.frames.length - 1]!;
    if (frame.returning) {
      this.finishFrame(task, frame);
      return;
    }
    if (frame.pc >= frame.fn.instructions.length) {
      frame.returning = this.namedResults(frame);
      this.finishFrame(task, frame);
      return;
    }
    const instruction = frame.fn.instructions[frame.pc++]!;
    this.steps++;
    for (const slot of instruction.operands.releaseBefore) frame.slots[slot] = undefined;
    this.execute(task, frame, instruction);
    for (const slot of instruction.operands.release) frame.slots[slot] = undefined;
  }

  private execute(task: Task, frame: Frame, instruction: PreparedInstruction): void {
    const inputs = this.inputs(frame, instruction);
    const descriptor = instruction.descriptor ?? {};
    const output = (...values: Value[]) =>
      this.outputs(frame, instruction.operands.outputs, values);
    switch (instruction.opcode) {
      case 1:
        this.typeDispatch(frame, descriptor);
        return;
      case 2:
        this.select(task, frame, descriptor);
        return;
      case 3: {
        const constant = descriptorInteger(descriptor.$constant);
        output(cloneValue(frame.fn.module.constants[constant]!));
        return;
      }
      case 4:
        output(zero(this.outputType(frame, instruction.operands.outputs[0]!)));
        return;
      case 5:
        return;
      case 6:
        output(unary(descriptorText(descriptor.operator), inputs[0]!));
        return;
      case 7:
        output(binary(descriptorText(descriptor.operator), inputs[0]!, inputs[1]!));
        return;
      case 8:
      case 9: {
        const index = descriptorInteger(descriptor.$local);
        if (instruction.opcode === 8) output(cloneValue(frame.locals[index]!.value));
        else if (Boolean(descriptor.rebind))
          frame.locals[index] = cell(this.coerce(inputs[0]!, frame.fn.localTypes[index]!));
        else frame.locals[index]!.value = this.coerce(inputs[0]!, frame.fn.localTypes[index]!);
        return;
      }
      case 10:
      case 11: {
        const index = descriptorInteger(descriptor.$upvalue);
        const address = frame.upvalues[index]!;
        if (instruction.opcode === 10) output(cloneValue(dereference(address)));
        else assign(address, this.coerce(inputs[0]!, frame.fn.upvalueTypes[index]!));
        return;
      }
      case 12:
      case 13: {
        const index = descriptorInteger(descriptor.$global);
        if (instruction.opcode === 12) output(cloneValue(frame.fn.module.globals[index]!.value));
        else
          frame.fn.module.globals[index]!.value = this.coerce(
            inputs[0]!,
            frame.fn.module.globals[index]!.value.type,
          );
        return;
      }
      case 14:
        return;
      case 15:
        frame.pc = descriptorInteger(descriptor.$target);
        return;
      case 16: {
        const condition = inputs[0];
        if (condition?.kind !== "bool") invalid("jump condition is not bool", "jump_if");
        if ((condition.value as boolean) !== Boolean(descriptor.negate))
          frame.pc = descriptorInteger(descriptor.$target);
        return;
      }
      case 17:
        frame.returning = inputs.map(cloneValue);
        return;
      case 18:
        frame.panic =
          inputs[0]!.kind === "nil"
            ? stringValue(
                frame.fn.module.registry,
                textEncoder.encode("panic called with nil argument"),
              )
            : cloneValue(inputs[0]!);
        frame.returning = [];
        return;
      case 19:
        output(this.recover(task, frame));
        return;
      case 20: {
        const value = inputs[0];
        if (value?.kind !== "function") invalid("deferred value is not callable", "defer");
        const ownerDepth = descriptorInteger(descriptor.owner_depth, 0);
        const owner = task.frames[task.frames.length - ownerDepth - 1];
        if (!owner) invalid("defer owner depth exceeds stack", "defer");
        owner.defers.push(value.value as FunctionValue);
        return;
      }
      case 21:
      case 22:
      case 23:
        this.call(task, frame, instruction, inputs, descriptor);
        return;
      case 24:
        this.callInterface(task, frame, instruction, inputs, descriptor);
        return;
      case 25:
        output(this.makeClosure(frame, descriptor));
        return;
      case 26:
        output(this.makeSequence(frame, descriptor, inputs));
        return;
      case 27:
        output(this.makeMap(frame, descriptor, inputs));
        return;
      case 28:
        output(this.makeStruct(frame, descriptor, inputs));
        return;
      case 29:
        output(this.makeSlice(frame, descriptor, inputs));
        return;
      case 30:
        output(this.makeChannel(frame, descriptor, inputs[0]!));
        return;
      case 31: {
        const [value] = this.loadIndex(inputs[0]!, inputs[1]!);
        output(value);
        return;
      }
      case 32: {
        const [value, ok] = this.loadIndex(inputs[0]!, inputs[1]!);
        output(value, boolValue(frame.fn.module.registry, ok));
        return;
      }
      case 33: {
        const bytes = expectBytes(inputs[0]!, "string_rune_at");
        const index = asIndex(inputs[1]!);
        const [rune] = decodeRune(bytes, index);
        output(intValue(frame.fn.module.registry, BigInt(rune)));
        return;
      }
      case 34: {
        const bytes = expectBytes(inputs[0]!, "string_next_rune_index");
        const index = asIndex(inputs[1]!);
        const [, width] = decodeRune(bytes, index);
        output(intValue(frame.fn.module.registry, BigInt(index + width)));
        return;
      }
      case 35:
        output(this.slice(frame, inputs));
        return;
      case 36:
      case 37:
        output(
          intValue(
            frame.fn.module.registry,
            BigInt(this.length(inputs[0]!, instruction.opcode === 37)),
          ),
        );
        return;
      case 38:
        output(this.append(descriptor, inputs));
        return;
      case 39:
        this.deleteMap(inputs[0]!, inputs[1]!);
        return;
      case 40:
        this.clear(inputs[0]!);
        return;
      case 41:
        output(intValue(frame.fn.module.registry, BigInt(this.copy(inputs[0]!, inputs[1]!))));
        return;
      case 42:
        output(this.mapKeys(frame, inputs[0]!));
        return;
      case 43:
        this.mapIterInit(frame, descriptor, inputs[0]!);
        return;
      case 44:
        output(...this.mapIterNext(frame, descriptor));
        return;
      case 45:
        frame.mapIterators.delete(descriptorText(descriptor.local));
        return;
      case 46:
        output(cloneValue(fieldValue(inputs[0]!, descriptorText(descriptor.field))));
        return;
      case 47:
        this.storeIndex(inputs[0]!, inputs[1]!, inputs[2]!);
        return;
      case 48:
        this.storeField(inputs[0]!, descriptorText(descriptor.field), inputs[1]!);
        return;
      case 49:
      case 50: {
        const type = this.descriptorType(frame, descriptor);
        const asserted = this.assertType(inputs[0]!, type);
        if (instruction.opcode === 49) {
          if (!asserted) fail("interface conversion failed", "type_assert", "panic");
          output(asserted);
        } else
          output(asserted ?? zero(type), boolValue(frame.fn.module.registry, Boolean(asserted)));
        return;
      }
      case 51:
        output(this.convertValue(inputs[0]!, this.descriptorType(frame, descriptor)));
        return;
      case 52:
        output(this.addressOf(frame, descriptor));
        return;
      case 53: {
        if (inputs[0]!.kind !== "pointer")
          fail("nil pointer dereference", "load_indirect", "panic");
        output(cloneValue(dereference(inputs[0]!.value as Address)));
        return;
      }
      case 54:
        if (inputs[0]!.kind !== "pointer")
          fail("nil pointer dereference", "store_indirect", "panic");
        assign(inputs[0]!.value as Address, inputs[1]!);
        return;
      case 55:
        this.send(task, inputs[0]!, inputs[1]!);
        return;
      case 56:
      case 57:
        this.receive(task, frame, instruction, inputs[0]!, instruction.opcode === 57);
        return;
      case 58:
        output(boolValue(frame.fn.module.registry, this.canReceive(inputs[0]!)));
        return;
      case 59: {
        const received = this.tryReceive(inputs[0]!);
        output(
          received?.[0] ?? zero(this.channelElement(inputs[0]!.type)),
          boolValue(frame.fn.module.registry, Boolean(received)),
        );
        return;
      }
      case 60:
        output(boolValue(frame.fn.module.registry, this.trySend(inputs[0]!, inputs[1]!, task)));
        return;
      case 61:
        output(boolValue(frame.fn.module.registry, this.canSend(inputs[0]!)));
        return;
      case 62:
        this.closeChannel(inputs[0]!);
        this.wakeSelects();
        return;
      case 63:
        this.initializeModule(task, frame, descriptorText(descriptor.module_path));
        return;
      case 64:
        output(this.loadExport(frame, descriptor));
        return;
      case 65:
        this.spawn(task, frame, inputs, descriptor);
        return;
      case 66:
        this.callFFI(task, frame, instruction, inputs);
        return;
      case 67:
        this.intrinsic(task, frame, instruction, descriptorText(descriptor.id), inputs);
        return;
      case 68: {
        const matched = binary(descriptorText(descriptor.operator), inputs[0]!, inputs[1]!);
        if (matched.kind !== "bool") invalid("comparison did not return bool", "compare_branch");
        if ((matched.value as boolean) === Boolean(descriptor.when))
          frame.pc = descriptorInteger(descriptor.$target);
        return;
      }
      case 69:
        output(this.getPath(descriptor, inputs[0]!));
        return;
      default:
        invalid("unknown opcode", String(instruction.opcode));
    }
  }

  private inputs(frame: Frame, instruction: PreparedInstruction): Value[] {
    return instruction.operands.inputs.map(([kind, index]) => {
      if (kind === 0) {
        const value = frame.slots[index];
        if (!value) invalid("temporary is not initialized", "operand");
        return cloneValue(value);
      }
      if (kind === 1) {
        const value = frame.fn.module.constants[index];
        if (!value) invalid("constant index is invalid", "operand");
        return cloneValue(value);
      }
      if (kind === 2) {
        const value = frame.locals[index]?.value;
        if (!value) invalid("local index is invalid", "operand");
        return cloneValue(value);
      }
      invalid("unknown operand kind", "operand");
    });
  }

  private outputs(frame: Frame, targets: readonly number[], values: Value[]): void {
    if (targets.length !== values.length)
      invalid(
        `result count mismatch: expected ${targets.length}, received ${values.length}`,
        frame.fn.id,
      );
    for (let index = 0; index < targets.length; index++) {
      const target = targets[index]!;
      if ((target & LOCAL_OUTPUT) !== 0) {
        const local = target & ~LOCAL_OUTPUT;
        if (!frame.locals[local]) invalid("local output index is invalid", frame.fn.id);
        frame.locals[local]!.value = this.coerce(values[index]!, frame.fn.localTypes[local]!);
      } else {
        if (target >= frame.slots.length) invalid("slot output index is invalid", frame.fn.id);
        frame.slots[target] = this.coerce(values[index]!, frame.fn.slotTypes[target]!);
      }
    }
  }

  private outputType(frame: Frame, target: number): RuntimeType {
    if ((target & LOCAL_OUTPUT) !== 0) return frame.fn.localTypes[target & ~LOCAL_OUTPUT]!;
    const type = frame.fn.slotTypes[target];
    if (!type) invalid("output type is unavailable", frame.fn.id);
    return type;
  }

  private namedResults(frame: Frame): Value[] {
    return frame.fn.resultLocals.map((index) => cloneValue(frame.locals[index]!.value));
  }

  private finishFrame(task: Task, frame: Frame): void {
    if (frame.defers.length) {
      const deferred = frame.defers.pop()!;
      const fn = this.resolveFunction(deferred, frame.revision);
      this.pushFrame(
        task,
        this.createFrame(fn, this.functionRevision(deferred), [], deferred.captures, [], true),
      );
      return;
    }
    let results = frame.returning ?? this.namedResults(frame);
    task.frames.pop();
    if (frame.initializing) {
      this.initializing.delete(frame.initializing);
      this.initialized.add(frame.initializing);
    }
    if (frame.panic && !frame.recovered) {
      if (!task.frames.length) {
        this.failTask(task, new VMError(formatPanic(frame.panic), "panic", "panic"));
        return;
      }
      const caller = task.frames[task.frames.length - 1]!;
      caller.panic = frame.panic;
      caller.returning = [];
      return;
    }
    if (frame.recovered || frame.fn.resultLocals.length > 0) results = this.namedResults(frame);
    if (frame.deferred) results = [];
    if (!task.frames.length) {
      this.completeTask(task, results);
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
    } else if (frame.tailReturn) caller.returning = results;
    else this.outputs(caller, frame.returnOutputs, results);
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
      value: cloneValue(owner.panic),
    };
  }

  private call(
    task: Task,
    frame: Frame,
    instruction: PreparedInstruction,
    inputs: Value[],
    descriptor: Record<string, unknown>,
  ): void {
    const indirect = instruction.opcode === 21;
    const tail = instruction.opcode === 23;
    let fnValue: FunctionValue;
    let arguments_ = inputs;
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
      fnValue = callee.value as FunctionValue;
      if (callee.kind === "method" && fnValue.receiver)
        arguments_ = [cloneValue(fnValue.receiver), ...arguments_];
    } else {
      fnValue = {
        module:
          descriptorText(descriptor.module_path, frame.fn.module.path) || frame.fn.module.path,
        function: descriptorText(descriptor.function),
        captures: [],
        revision: frame.revision.generation,
      };
    }
    const revision = this.functionRevision(fnValue);
    const fn = this.resolveFunction(fnValue, revision);
    const child = this.createFrame(
      fn,
      revision,
      arguments_,
      fnValue.captures,
      tail && frame.defers.length === 0 ? frame.returnOutputs : instruction.operands.outputs,
      false,
    );
    if (tail && frame.defers.length === 0) task.frames[task.frames.length - 1] = child;
    else {
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
    const callback = this.resolveFunction(dynamic.callback, revision);
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

  private resolveFunction(fn: FunctionValue, revision: Revision): PreparedFunction {
    const source = revision.program.modules.get(fn.module)?.functions.get(fn.function);
    if (source?.revisionLocal) return source;
    return this.revision.program.function(fn.module, fn.function);
  }

  private makeClosure(frame: Frame, descriptor: Record<string, unknown>): Value {
    const captures = descriptorList(descriptor.captures).map((capture) =>
      this.resolveAddress(frame, descriptorRecord(capture)),
    );
    return {
      type: frame.fn.module.registry.runtime({
        kind: Kind.Function,
        primitive: 0,
        named: emptyKey(),
        node: "",
      }),
      kind: "function",
      value: {
        module:
          descriptorText(descriptor.module_path, frame.fn.module.path) || frame.fn.module.path,
        function: descriptorText(descriptor.function),
        captures,
        revision: frame.revision.generation,
      } satisfies FunctionValue,
    };
  }

  private makeSequence(frame: Frame, descriptor: Record<string, unknown>, inputs: Value[]): Value {
    this.checkSequence(inputs.length, "make_sequence");
    const type = this.descriptorType(frame, descriptor);
    const kind = type.registry.underlying(type).ref.kind;
    if (kind === Kind.Slice) {
      const storage = cell({ type, kind: "array", value: inputs.map(cloneValue) });
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

  private makeStruct(frame: Frame, descriptor: Record<string, unknown>, inputs: Value[]): Value {
    const type = this.descriptorType(frame, descriptor);
    const node = type.registry.node(type.registry.underlying(type));
    const values = new Map<string, Value>();
    for (const field of node?.fields ?? [])
      values.set(field.name, zero(type.registry.runtime(field.type)));
    const names = descriptorList(descriptor.fields).map((value) => descriptorText(value));
    for (let index = 0; index < names.length; index++)
      values.set(names[index]!, cloneValue(inputs[index]!));
    return { type, kind: "struct", value: values };
  }

  private makeMap(frame: Frame, descriptor: Record<string, unknown>, inputs: Value[]): Value {
    const type = this.descriptorType(frame, descriptor);
    const entries: GuestMap["entries"] = [];
    const count = descriptorInteger(descriptor.entry_count);
    this.checkSequence(count, "make_map");
    for (let index = 0; index < count; index++) {
      const key = inputs[index * 2]!;
      const value = inputs[index * 2 + 1]!;
      assertComparable(key);
      const existing = entries.find((entry) => guestEqual(entry.key, key));
      if (existing) existing.value = cloneValue(value);
      else entries.push({ key: cloneValue(key), value: cloneValue(value) });
    }
    return { type, kind: "map", value: { entries } satisfies GuestMap };
  }

  private makeSlice(frame: Frame, descriptor: Record<string, unknown>, inputs: Value[]): Value {
    const type = this.descriptorType(frame, descriptor);
    const length = asIndex(inputs[0]!);
    const capacity = Boolean(descriptor.has_capacity) ? asIndex(inputs[1]!) : length;
    if (length > capacity) fail("slice length exceeds capacity", "make_slice", "panic");
    this.checkSequence(capacity, "make_slice");
    const element = this.elementType(type);
    const byteSlice =
      element.registry.underlying(element).ref.kind === Kind.Primitive &&
      element.registry.underlying(element).ref.primitive === PrimitiveKind.PrimitiveUint8;
    const backing: Value = byteSlice
      ? { type, kind: "bytes", value: new Uint8Array(capacity) }
      : { type, kind: "array", value: Array.from({ length: capacity }, () => zero(element)) };
    return {
      type,
      kind: "slice",
      value: {
        storage: { root: cell(backing), path: [] },
        start: 0,
        length,
        capacity,
      } satisfies SliceValue,
    };
  }

  private makeChannel(frame: Frame, descriptor: Record<string, unknown>, capacity: Value): Value {
    const type = this.descriptorType(frame, descriptor);
    const size = asIndex(capacity);
    this.checkSequence(size, "make_waitable");
    return {
      type,
      kind: "channel",
      value: {
        capacity: size,
        closed: false,
        queue: [],
        senders: [],
        receivers: [],
      } satisfies Channel,
    };
  }

  private descriptorType(frame: Frame, descriptor: Record<string, unknown>): RuntimeType {
    void frame;
    return descriptor.$type as RuntimeType;
  }

  private elementType(type: RuntimeType): RuntimeType {
    const element = type.registry.element(type);
    if (!element) invalid("container has no element type", "type");
    return element;
  }

  private loadIndex(object: Value, key: Value): [Value, boolean] {
    if (object.kind === "map") {
      assertComparable(key);
      const entry = (object.value as GuestMap).entries.find((item) => guestEqual(item.key, key));
      return [
        entry ? cloneValue(entry.value) : zero(this.elementType(object.type)),
        Boolean(entry),
      ];
    }
    if (
      object.kind === "nil" &&
      object.type.registry.underlying(object.type).ref.kind === Kind.Map
    ) {
      assertComparable(key);
      return [zero(this.elementType(object.type)), false];
    }
    return [cloneValue(indexValue(object, asIndex(key))), true];
  }

  private storeIndex(object: Value, key: Value, value: Value): void {
    if (object.kind === "map") {
      assertComparable(key);
      const map = object.value as GuestMap;
      const entry = map.entries.find((item) => guestEqual(item.key, key));
      if (entry) entry.value = cloneValue(value);
      else map.entries.push({ key: cloneValue(key), value: cloneValue(value) });
      return;
    }
    if (
      object.kind === "nil" &&
      object.type.registry.underlying(object.type).ref.kind === Kind.Map
    ) {
      assertComparable(key);
      fail("assignment to entry in nil map", "store_index", "panic");
    }
    setIndex(object, asIndex(key), value);
  }

  private storeField(object: Value, name: string, value: Value): void {
    if (object.kind === "pointer") object = dereference(object.value as Address);
    if (object.kind !== "struct") invalid("field store targets non-struct", "store_field");
    const fields = object.value as Map<string, Value>;
    if (!fields.has(name)) invalid("unknown field", name);
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
          ? (object.value as Value[]).length
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
          storage: storage ?? { root: cell(object), path: [] },
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

  private length(value: Value, capacity: boolean): number {
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
    if (value.kind === "string" && !capacity) return (value.value as Uint8Array).length;
    if (value.kind === "array") return (value.value as Value[]).length;
    if (value.kind === "bytes") return (value.value as Uint8Array).length;
    if (value.kind === "slice")
      return capacity ? (value.value as SliceValue).capacity : (value.value as SliceValue).length;
    if (value.kind === "map" && !capacity) return (value.value as GuestMap).entries.length;
    if (value.kind === "channel")
      return capacity ? (value.value as Channel).capacity : (value.value as Channel).queue.length;
    invalid("value has no length", capacity ? "cap" : "len");
  }

  private append(descriptor: Record<string, unknown>, inputs: Value[]): Value {
    const original = inputs[0]!;
    if (original.kind !== "slice" && original.kind !== "nil")
      invalid("append target is not slice", "append");
    let values = inputs.slice(1);
    if (Boolean(descriptor.expand)) values = this.sliceValues(values[0]!);
    const element = this.elementType(original.type);
    let slice: SliceValue;
    if (original.kind === "nil") {
      slice = {
        storage: { root: cell({ type: original.type, kind: "array", value: [] }), path: [] },
        start: 0,
        length: 0,
        capacity: 0,
      };
    } else slice = original.value as SliceValue;
    const needed = slice.length + values.length;
    this.checkSequence(needed, "append");
    if (needed > slice.capacity) {
      const capacity = Math.max(needed, Math.max(1, slice.capacity * 2));
      const old = this.sliceValues(original);
      const byteSlice =
        element.registry.underlying(element).ref.primitive === PrimitiveKind.PrimitiveUint8;
      const backing: Value = byteSlice
        ? { type: original.type, kind: "bytes", value: new Uint8Array(capacity) }
        : {
            type: original.type,
            kind: "array",
            value: Array.from({ length: capacity }, () => zero(element)),
          };
      const root = cell(backing);
      slice = { storage: { root, path: [] }, start: 0, length: old.length, capacity };
      for (let index = 0; index < old.length; index++) setIndex(backing, index, old[index]!);
    }
    const result: Value = {
      type: original.type,
      kind: "slice",
      value: { ...slice, length: needed },
    };
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
    if (value.kind === "array") return (value.value as Value[]).map(cloneValue);
    if (value.kind !== "slice") invalid("expected slice", "slice");
    const slice = value.value as SliceValue;
    return Array.from({ length: slice.length }, (_, index) => cloneValue(indexValue(value, index)));
  }

  private copy(destination: Value, source: Value): number {
    const values = this.sliceValues(source);
    if (destination.kind !== "slice") invalid("copy destination is not slice", "copy");
    const slice = destination.value as SliceValue;
    const count = Math.min(slice.length, values.length);
    for (let index = 0; index < count; index++) setIndex(destination, index, values[index]!);
    return count;
  }

  private deleteMap(value: Value, key: Value): void {
    assertComparable(key);
    if (value.kind === "nil") return;
    if (value.kind !== "map") invalid("delete target is not map", "delete");
    const entries = (value.value as GuestMap).entries;
    const index = entries.findIndex((entry) => guestEqual(entry.key, key));
    if (index >= 0) entries.splice(index, 1);
  }

  private clear(value: Value): void {
    if (value.kind === "nil") return;
    if (value.kind === "map") (value.value as GuestMap).entries.length = 0;
    else if (value.kind === "slice") {
      const slice = value.value as SliceValue;
      const element = this.elementType(value.type);
      for (let index = 0; index < slice.length; index++) setIndex(value, index, zero(element));
    } else invalid("clear target is not map or slice", "clear");
  }

  private mapKeys(frame: Frame, value: Value): Value {
    const keys =
      value.kind === "nil"
        ? []
        : (value.value as GuestMap).entries.map((entry) => cloneValue(entry.key));
    const type = frame.fn.slotTypes[frame.fn.instructions[frame.pc - 1]!.operands.outputs[0]!]!;
    return { type, kind: "array", value: keys };
  }

  private mapIterInit(frame: Frame, descriptor: Record<string, unknown>, value: Value): void {
    const map: GuestMap = value.kind === "nil" ? { entries: [] } : (value.value as GuestMap);
    frame.mapIterators.set(descriptorText(descriptor.local), {
      map,
      entries: [...map.entries],
      offset: 0,
    });
  }

  private mapIterNext(frame: Frame, descriptor: Record<string, unknown>): Value[] {
    const iterator = frame.mapIterators.get(descriptorText(descriptor.local));
    if (!iterator) invalid("map iterator is not initialized", "map_iter_next");
    while (iterator.offset < iterator.entries.length) {
      const entry = iterator.entries[iterator.offset++]!;
      const current = iterator.map.entries.includes(entry) ? entry : undefined;
      if (current)
        return [
          cloneValue(current.key),
          cloneValue(current.value),
          boolValue(frame.fn.module.registry, true),
        ];
    }
    const instruction = frame.fn.instructions[frame.pc - 1]!;
    return [
      zero(this.outputType(frame, instruction.operands.outputs[0]!)),
      zero(this.outputType(frame, instruction.operands.outputs[1]!)),
      boolValue(frame.fn.module.registry, false),
    ];
  }

  private addressOf(frame: Frame, descriptor: Record<string, unknown>): Value {
    const address = this.resolveAddress(frame, descriptor);
    const target = dereference(address).type;
    const type: RuntimeType = {
      module: frame.fn.module.path,
      registry: frame.fn.module.registry,
      ref: { kind: Kind.Pointer, primitive: 0, named: emptyKey(), node: "" },
    };
    void target;
    return { type, kind: "pointer", value: address };
  }

  private resolveAddress(frame: Frame, descriptor: Record<string, unknown>): Address {
    const kind = descriptorText(descriptor.kind);
    let address: Address;
    if (kind === "local") {
      const index = descriptorInteger(descriptor.$index);
      address = { root: frame.locals[index]!, path: [] };
    } else if (kind === "upvalue") {
      const index = descriptorInteger(descriptor.$index);
      address = { root: frame.upvalues[index]!.root, path: [...frame.upvalues[index]!.path] };
    } else if (kind === "global") {
      const index = descriptorInteger(descriptor.$index);
      address = { root: frame.fn.module.globals[index]!, path: [] };
    } else if (kind === "export") {
      const module = this.revision.program.modules.get(descriptorText(descriptor.module_path));
      const exported = module?.exports.get(descriptorText(descriptor.export));
      const index =
        exported?.kind === "global" ? module!.globalIndexes.get(exported.id) : undefined;
      if (index === undefined) invalid("export address is invalid", "address_of");
      address = { root: module!.globals[index]!, path: [] };
    } else invalid("unknown address kind", "address_of");
    const path: AddressPath[] = [...address!.path];
    for (const segmentValue of descriptorList(descriptor.path)) {
      const segment = descriptorRecord(segmentValue);
      const segmentKind = descriptorText(segment.kind);
      if (segmentKind === "field")
        path.push({ kind: "field", name: descriptorText(segment.field) });
      else if (segmentKind === "index") {
        const local = descriptorInteger(segment.$index);
        path.push({ kind: "index", index: asIndex(frame.locals[local]!.value) });
      } else if (segmentKind === "indirect") {
        const current = dereference({ root: address!.root, path });
        if (current.kind !== "pointer") fail("nil pointer dereference", "address", "panic");
        address = current.value as Address;
        path.length = 0;
        path.push(...address.path);
      } else invalid("unknown address path", "address_of");
    }
    return { root: address!.root, path };
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
          value: {
            root: slice.storage.root,
            path: [
              ...slice.storage.path,
              {
                kind: "arrayView",
                start: slice.start,
                length: targetNode.length,
                type: targetElement,
              },
            ],
          } satisfies Address,
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
    return convert(value, type);
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

  private typeDispatch(frame: Frame, descriptor: Record<string, unknown>): void {
    const subject = descriptorInteger(descriptor.$subject);
    const value = frame.locals[subject]!.value;
    const dynamic = value.kind === "interface" ? (value.value as Value) : value;
    for (const item of descriptorList(descriptor.cases)) {
      const candidate = descriptorRecord(item);
      const candidateType = candidate.$type as RuntimeType | undefined;
      if (
        (value.kind === "nil" && !candidateType) ||
        (value.kind !== "nil" && candidateType && this.assertType(dynamic, candidateType))
      ) {
        if (candidate.$binding !== undefined) {
          const binding = descriptorInteger(candidate.$binding);
          frame.locals[binding]!.value = Boolean(candidate.original)
            ? cloneValue(value)
            : cloneValue(dynamic);
        }
        frame.pc = descriptorInteger(candidate.$target);
        return;
      }
    }
    if (descriptor.$default_local !== undefined) {
      const binding = descriptorInteger(descriptor.$default_local);
      frame.locals[binding]!.value = cloneValue(value);
    }
    frame.pc = descriptorInteger(descriptor.$target);
  }

  private getPath(descriptor: Record<string, unknown>, source: Value): Value {
    let value = source;
    let type = descriptor.$type as RuntimeType;
    for (const rawIndex of descriptorList(descriptor.fields)) {
      const index = descriptorInteger(rawIndex);
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

  private loadExport(frame: Frame, descriptor: Record<string, unknown>): Value {
    const module = this.revision.program.modules.get(descriptorText(descriptor.module_path));
    const exported = module?.exports.get(descriptorText(descriptor.export));
    if (!module || !exported) invalid("unknown export", "load_export");
    if (exported.kind === "const")
      return cloneValue(
        module.constants[requiredIndex(module.constantIndexes, exported.id, "constant")]!,
      );
    if (exported.kind === "global")
      return cloneValue(
        module.globals[requiredIndex(module.globalIndexes, exported.id, "global")]!.value,
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
    const module = this.revision.program.modules.get(modulePath);
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

  private spawn(
    task: Task,
    frame: Frame,
    inputs: Value[],
    descriptor: Record<string, unknown>,
  ): void {
    const maxTasks = this.options.limits?.maxTasks ?? 4096;
    if (this.tasks.size >= maxTasks)
      throw new VMError("task limit exceeded", "task_limit", "scheduler");
    let fnValue: FunctionValue;
    let args = inputs;
    if (!descriptor.function) {
      const value = inputs[0];
      if (!value || value.kind !== "function") fail("spawn of nil function", "spawn", "panic");
      fnValue = value.value as FunctionValue;
      args = inputs.slice(1);
    } else {
      fnValue = {
        module:
          descriptorText(descriptor.module_path, frame.fn.module.path) || frame.fn.module.path,
        function: descriptorText(descriptor.function),
        captures: [],
        revision: frame.revision.generation,
      };
    }
    const revision = this.functionRevision(fnValue);
    const fn = this.resolveFunction(fnValue, revision);
    const child: Task = {
      id: this.nextTask++,
      scope: task.scope,
      frames: [this.createFrame(fn, revision, args, fnValue.captures, [], false)],
      state: "runnable",
    };
    this.tasks.set(child.id, child);
    this.scopes.get(task.scope)!.tasks.add(child.id);
    this.runnable.push(child.id);
  }

  private send(task: Task, channelValue: Value, value: Value): void {
    const channel = this.channel(channelValue);
    if (channel.closed) fail("send on closed channel", "send", "panic");
    const receiverIndex = channel.receivers.findIndex(
      (id) => this.tasks.get(id)?.state === "blocked",
    );
    if (receiverIndex >= 0) {
      const receiver = this.tasks.get(channel.receivers.splice(receiverIndex, 1)[0]!)!;
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
        this.unblock(receiver);
      }
      this.wakeSelects();
      return;
    }
    if (channel.queue.length < channel.capacity) {
      channel.queue.push(cloneValue(value));
      this.wakeSelects();
      return;
    }
    channel.senders.push({ task: task.id, value: cloneValue(value) });
    this.block(task, { kind: "send", channel, value: cloneValue(value) });
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
  }

  private tryReceive(channelValue: Value): [Value, boolean] | undefined {
    const channel = this.channel(channelValue);
    if (channel.queue.length) {
      const value = channel.queue.shift()!;
      const sender = channel.senders.shift();
      if (sender) {
        channel.queue.push(sender.value);
        const task = this.tasks.get(sender.task);
        if (task) this.unblock(task);
      }
      this.wakeSelects();
      return [value, true];
    }
    const sender = channel.senders.shift();
    if (sender) {
      const task = this.tasks.get(sender.task);
      if (task) this.unblock(task);
      this.wakeSelects();
      return [sender.value, true];
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
      const task = this.tasks.get(sender.task);
      if (task) this.failTask(task, new VMError("send on closed channel", "panic", "send"));
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

  private unblock(task: Task): void {
    if (task.state !== "blocked") return;
    task.state = "runnable";
    delete task.blocked;
    this.runnable.push(task.id);
  }

  private select(task: Task, frame: Frame, descriptor: Record<string, unknown>): void {
    const cases = descriptorList(descriptor.cases);
    for (let index = 0; index < cases.length; index++) {
      const selected = descriptorRecord(cases[index]);
      const channelIndex = descriptorInteger(selected.$channel);
      const channelValue = frame.locals[channelIndex]!.value;
      if (selected.$send !== undefined) {
        if (!this.canSend(channelValue)) continue;
        const sendIndex = descriptorInteger(selected.$send);
        this.send(task, channelValue, frame.locals[sendIndex]!.value);
      } else {
        if (!this.canReceive(channelValue)) continue;
        const received = this.tryReceive(channelValue)!;
        if (selected.$value !== undefined) {
          const local = descriptorInteger(selected.$value);
          frame.locals[local]!.value = received[0];
        }
        if (selected.$ok !== undefined) {
          const local = descriptorInteger(selected.$ok);
          frame.locals[local]!.value = boolValue(frame.fn.module.registry, received[1]);
        }
      }
      frame.locals[descriptorInteger(descriptor.$index)]!.value = intValue(
        frame.fn.module.registry,
        BigInt(index),
      );
      return;
    }
    if (this.pairSelect(task, frame, descriptor, cases)) return;
    if (Boolean(descriptor.default)) {
      frame.locals[descriptorInteger(descriptor.$index)]!.value = intValue(
        frame.fn.module.registry,
        -1n,
      );
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
        delete task.blocked;
        task.state = "runnable";
        if (this.completeReflectSelect(task, frame, blocked.outputs, blocked.cases))
          this.runnable.push(task.id);
        else {
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
        const received = this.tryReceive(item.channel);
        if (received) return selected(item.index, received[0], received[1]);
      } else if (item.send && this.trySend(item.channel, item.send, task)) {
        return selected(item.index, undefined, false);
      }
    }
    const fallback = cases.find((item) => item.default);
    return fallback ? selected(fallback.index, undefined, false) : false;
  }

  private pairSelect(
    task: Task,
    frame: Frame,
    descriptor: Record<string, unknown>,
    cases: unknown[],
  ): boolean {
    for (let index = 0; index < cases.length; index++) {
      const selected = descriptorRecord(cases[index]);
      const channelValue = frame.locals[descriptorInteger(selected.$channel)]!.value;
      if (channelValue.kind !== "channel" || (channelValue.value as Channel).capacity !== 0)
        continue;
      for (const other of this.tasks.values()) {
        if (other.id === task.id || other.blocked?.kind !== "select") continue;
        const otherFrame = other.frames[other.frames.length - 1]!;
        const otherInstruction = otherFrame.fn.instructions[otherFrame.pc];
        if (!otherInstruction || otherInstruction.opcode !== 2 || !otherInstruction.descriptor)
          continue;
        const otherDescriptor = otherInstruction.descriptor;
        const otherCases = descriptorList(otherDescriptor.cases);
        for (let otherIndex = 0; otherIndex < otherCases.length; otherIndex++) {
          const candidate = descriptorRecord(otherCases[otherIndex]);
          if ((selected.$send !== undefined) === (candidate.$send !== undefined)) continue;
          const otherChannel = otherFrame.locals[descriptorInteger(candidate.$channel)]!.value;
          if (otherChannel.kind !== "channel" || otherChannel.value !== channelValue.value)
            continue;
          const senderFrame = selected.$send !== undefined ? frame : otherFrame;
          const sender = selected.$send !== undefined ? selected : candidate;
          const receiverFrame = selected.$send !== undefined ? otherFrame : frame;
          const receiver = selected.$send !== undefined ? candidate : selected;
          const sent = senderFrame.locals[descriptorInteger(sender.$send)]!.value;
          if (receiver.$value !== undefined)
            receiverFrame.locals[descriptorInteger(receiver.$value)]!.value = cloneValue(sent);
          if (receiver.$ok !== undefined)
            receiverFrame.locals[descriptorInteger(receiver.$ok)]!.value = boolValue(
              receiverFrame.fn.module.registry,
              true,
            );
          frame.locals[descriptorInteger(descriptor.$index)]!.value = intValue(
            frame.fn.module.registry,
            BigInt(index),
          );
          otherFrame.locals[descriptorInteger(otherDescriptor.$index)]!.value = intValue(
            otherFrame.fn.module.registry,
            BigInt(otherIndex),
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
    this.pendingHost.set(id, {
      task: task.id,
      outputs: instruction.operands.outputs,
      resultType: this.outputType(frame, instruction.operands.outputs[0]!),
    });
    this.pendingActions.push({ kind: "call", id, route, payload: [...payload] });
    this.block(task, { kind: "ffi", id });
  }

  complete(id: number, payload: Uint8Array, error?: string): void {
    const pending = this.pendingHost.get(id);
    if (!pending) return;
    this.pendingHost.delete(id);
    const task = this.tasks.get(pending.task);
    if (!task || task.state !== "blocked") return;
    const frame = task.frames[task.frames.length - 1]!;
    const status = error ? (error.includes("route unavailable") ? 1n : 2n) : 0n;
    const value = this.byteSlice(pending.resultType, payload);
    this.outputs(frame, pending.outputs, [
      value,
      stringValue(frame.fn.module.registry, textEncoder.encode(error ?? "")),
      intValue(frame.fn.module.registry, status),
    ]);
    this.pendingActions.push({ kind: "decision", id, accepted: !error });
    this.unblock(task);
  }

  decisionDone(_id: number): void {}
  hostClosed(): void {}

  actions(): VMAction[] {
    return this.pendingActions.splice(0);
  }

  private byteSlice(type: RuntimeType, bytes: Uint8Array): Value {
    const storage = cell({ type, kind: "bytes", value: Uint8Array.from(bytes) });
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
        const timer: TimerRecord = {
          id: this.nextTimer++,
          channel,
          deadline: performance.now() + delay,
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
        if (timer) timer.canceled = true;
        output(boolValue(frame.fn.module.registry, Boolean(timer)));
        return;
      }
      case "ffi.call":
        this.callFFI(task, frame, instruction, inputs);
        return;
      case "sync.mutex_try_lock": {
        const mutex = this.mutex(inputs[0]!);
        const acquired = !mutex.locked;
        if (acquired) mutex.locked = true;
        output(boolValue(frame.fn.module.registry, acquired));
        return;
      }
      case "sync.mutex_unlock": {
        const mutex = this.mutex(inputs[0]!);
        if (!mutex.locked) fail("unlock of unlocked mutex", id, "panic");
        mutex.locked = false;
        return;
      }
      case "sync.mutex_lock": {
        const mutex = this.mutex(inputs[0]!);
        if (!mutex.locked) {
          mutex.locked = true;
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
        const bytes = this.bytesFromSlice(inputs[0]!);
        crypto.getRandomValues(bytes);
        const destination = inputs[0]!;
        if (destination.kind === "slice") {
          const slice = destination.value as SliceValue;
          const backing = dereference(slice.storage);
          for (let index = 0; index < bytes.length; index++)
            setIndex(backing, slice.start + index, {
              type: primitive(frame.fn.module.registry, PrimitiveKind.PrimitiveUint8),
              kind: "uint",
              value: BigInt(bytes[index]!),
            });
        }
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

  private mutex(value: Value): { locked: boolean; waiting: number; granted: boolean } {
    if (value.kind === "pointer") value = dereference(value.value as Address);
    if (value.kind === "nil") {
      value.kind = "mutex";
      value.value = { locked: false, waiting: 0, granted: false };
    }
    if (value.kind !== "mutex") invalid("expected mutex", "mutex");
    return value.value as { locked: boolean; waiting: number; granted: boolean };
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
          const root = cell(zero(type));
          value = {
            type: this.dynamicType(type.registry, { kind: Kind.Pointer, elem: type }),
            kind: "pointer",
            value: { root, path: [] } satisfies Address,
          };
        } else if (id === "reflect.make_slice") {
          const length = asIndex(inputs[1]!);
          const capacity = asIndex(inputs[2]!);
          if (length > capacity) fail("reflect.MakeSlice: length exceeds capacity", id, "reflect");
          const element = this.elementType(type);
          value = this.sliceFromValues(
            type,
            Array.from({ length: capacity }, () => zero(element)),
            length,
          );
        } else if (id === "reflect.make_map") {
          value = { type, kind: "map", value: { entries: [] } satisfies GuestMap };
        } else if (id === "reflect.make_chan") {
          value = {
            type,
            kind: "channel",
            value: {
              capacity: asIndex(inputs[1]!),
              closed: false,
              queue: [],
              senders: [],
              receivers: [],
            } satisfies Channel,
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
        const descriptor = { expand: false };
        const appended = this.append(descriptor, [target, ...values]);
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
        const method = inputs[1]!.metadata as
          | { function: FunctionValue; type: RuntimeType }
          | undefined;
        if (!receiver || !method) fail("reflect: invalid Method", id, "reflect");
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
          const fields = value.value as Map<string, Value>;
          const direction = asIndex(fields.get("Dir")!);
          if (direction === 3) return { index, receive: false, default: true };
          if (direction !== 1 && direction !== 2)
            fail("reflect.Select: invalid direction", id, "reflect");
          const channel = this.reflectedView(fields.get("Chan")!).current;
          const selected: ReflectedSelectCase = {
            index,
            receive: direction === 2,
            default: false,
          };
          if (channel) selected.channel = channel;
          if (direction === 1) {
            const send = this.reflectedView(fields.get("Send")!).current;
            if (send) selected.send = send;
          }
          return selected;
        });
        if (!this.completeReflectSelect(task, frame, instruction.operands.outputs, cases))
          this.block(task, { kind: "reflectSelect", cases, outputs: instruction.operands.outputs });
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
    const refresh = cell(cloneValue(value));
    return {
      current: cloneValue(refresh.value),
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
    const view = value.metadata as ReflectedView | undefined;
    if (!view) {
      if (
        value.type.ref.kind === Kind.Named &&
        value.type.ref.named.module_path === "reflect" &&
        value.type.ref.named.decl_id === "Value"
      )
        return this.invalidView();
      fail("reflect: invalid Value", "reflect", "reflect");
    }
    const result: ReflectedView = { ...view };
    if (view.target) {
      result.current = cloneValue(dereference(view.target));
      result.target = { root: view.target.root, path: [...view.target.path] };
    } else {
      delete result.target;
      if (view.refresh) result.current = cloneValue(view.refresh.value);
      else if (view.current) result.current = cloneValue(view.current);
      else delete result.current;
    }
    return result;
  }

  private reflectedType(value: Value): RuntimeType {
    if (value.kind === "interface") value = value.value as Value;
    const type = value.metadata as RuntimeType | undefined;
    if (!type) fail("reflect: invalid Type", "reflect", "reflect");
    return type;
  }

  private namedReflectType(frame: Frame, name: string): RuntimeType {
    const registry =
      this.revision.program.modules.get("reflect")?.registry ?? frame.fn.module.registry;
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
    this.guestMemory.reserve(BigInt(bytes) * GUEST_COST.byte);
    this.dynamicTypes.set(canonical, candidate);
    this.dynamicTypeBytes += bytes;
    return candidate;
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
    const concrete = this.reflectStruct(frame, "runtimeType", {
      key: stringValue(frame.fn.module.registry, textEncoder.encode(type.registry.key(type))),
    });
    concrete.metadata = type;
    return { type: interfaceType, kind: "interface", value: concrete };
  }

  private reflectValue(frame: Frame, type: RuntimeType, source: ReflectedView): Value {
    const view = source.target
      ? { ...source, current: cloneValue(dereference(source.target)) }
      : source.refresh
        ? { ...source, current: cloneValue(source.refresh.value) }
        : source;
    const result = zero(type);
    const metadata: ReflectedView = { ...view };
    if (view.current) metadata.current = cloneValue(view.current);
    else delete metadata.current;
    if (view.target) metadata.target = { root: view.target.root, path: [...view.target.path] };
    else delete metadata.target;
    result.metadata = metadata;
    if (!view.current || result.kind !== "struct") return result;
    const current = view.current;
    const fields = result.value as Map<string, Value>;
    const put = (name: string, value: Value): void => {
      const destination = fields.get(name);
      if (destination) fields.set(name, this.coerce(value, destination.type));
    };
    put("valid", boolValue(frame.fn.module.registry, true));
    const valueType = fields.get("valueType");
    if (valueType) fields.set("valueType", this.reflectType(frame, valueType.type, current.type));
    put("data", current);
    if (view.target || view.refresh) {
      const address = view.target ?? { root: view.refresh!, path: [] };
      put("target", {
        type: this.dynamicType(current.type.registry, { kind: Kind.Pointer, elem: current.type }),
        kind: "pointer",
        value: { root: address.root, path: [...address.path] } satisfies Address,
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
          ? (current.value as Value[]).length
          : current.kind === "slice"
            ? (current.value as SliceValue).length
            : current.kind === "map"
              ? (current.value as GuestMap).entries.length
              : current.kind === "channel"
                ? (current.value as Channel).queue.length
                : current.kind === "struct"
                  ? (current.value as Map<string, Value>).size
                  : 0;
    const capacity =
      current.kind === "array"
        ? (current.value as Value[]).length
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
        const entries = (current.value as GuestMap).entries.map((entry) => {
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
      return (value.value as Value[]).every((item) => this.isZeroValue(item, seen));
    if (value.kind === "struct")
      return [...(value.value as Map<string, Value>)].every(
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
        const values = field.value as Map<string, Value>;
        return {
          name: textDecoder.decode(expectBytes(values.get("Name")!, id)),
          type: this.reflectedType(values.get("Type")!),
          tag: textDecoder.decode(expectBytes(values.get("Tag")!, id)),
          embedded: Boolean(values.get("Anonymous")?.value),
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
    value.metadata = { function: functionValue, type: callableType };
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
        target = {
          root: slice.storage.root,
          path: [...slice.storage.path, { kind: "index", index: slice.start + index }],
        };
      } else if (view.target && view.current.kind === "array") {
        target = { root: view.target.root, path: [...view.target.path, { kind: "index", index }] };
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
        if (view.target)
          view.target = {
            root: view.target.root,
            path: [...view.target.path, { kind: "field", name: field.name }],
          };
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
    const values = current.value as Value[];
    const limit = max ?? values.length;
    if (low > high || high > limit || limit > values.length)
      fail("reflect: slice bounds out of range", id, "reflect");
    const sliceType = this.dynamicType(current.type.registry, {
      kind: Kind.Slice,
      elem: this.elementType(current.type),
    });
    const storage = view.target ?? { root: cell(current), path: [] };
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
    else if (view.refresh) view.refresh.value = updated;
    else Object.assign(original, slice);
  }

  private sliceFromValues(type: RuntimeType, values: Value[], length = values.length): Value {
    return {
      type,
      kind: "slice",
      value: {
        storage: { root: cell({ type, kind: "array", value: values.map(cloneValue) }), path: [] },
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
        const av = a.value as Value[];
        const bv = b.value as Value[];
        if (av.length !== bv.length) return false;
        for (let index = 0; index < av.length; index++) pending.push([av[index]!, bv[index]!]);
      } else if (a.kind === "struct") {
        const av = a.value as Map<string, Value>;
        const bv = b.value as Map<string, Value>;
        if (av.size !== bv.size) return false;
        for (const [name, value] of av) {
          const other = bv.get(name);
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
        const av = (a.value as GuestMap).entries;
        const bv = (b.value as GuestMap).entries;
        if (av.length !== bv.length) return false;
        for (const entry of av) {
          const other = bv.find((candidate) => guestEqual(entry.key, candidate.key));
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
    const fn = this.resolveFunction(functionValue, revision);
    const child = this.createFrame(fn, revision, arguments_, functionValue.captures, [], false);
    child.completion = { kind: "reflectCall", outputs: instruction.operands.outputs };
    this.pushFrame(task, child);
  }

  private updateMemoryCensus(enforce: boolean): void {
    const started = performance.now();
    const census: MemoryCensus = {
      liveHeapBytes: 0n,
      liveHeapObjects: 0n,
      liveGuestBytes: 0n,
      strings: 0,
      maxSequence: 0,
      scannedEdges: 0n,
      revisions: new Set([this.generation]),
    };
    const seenCells = new Set<Cell>();
    const seenObjects = new Set<object>();
    const pending: Array<{ value?: Value; cell?: Cell; channel?: Channel; depth: number }> = [];
    const pushValue = (value: Value | undefined, depth: number): void => {
      if (value) pending.push({ value, depth });
    };
    const pushCell = (root: Cell | undefined, depth: number): void => {
      if (root) pending.push({ cell: root, depth });
    };
    const pushChannel = (channel: Channel | undefined, depth: number): void => {
      if (channel) pending.push({ channel, depth });
    };

    for (const module of this.revision.program.modules.values())
      for (const root of module.globals) pushCell(root, 0);
    for (const task of this.tasks.values()) {
      for (const frame of task.frames) {
        census.revisions.add(frame.revision.generation);
        for (const root of frame.locals) pushCell(root, 0);
        for (const address of frame.upvalues) pushCell(address.root, 0);
        for (const value of frame.slots) pushValue(value, 0);
        for (const deferred of frame.defers) {
          census.revisions.add(deferred.revision);
          for (const address of deferred.captures) pushCell(address.root, 0);
          pushValue(deferred.receiver, 0);
        }
        for (const value of frame.returning ?? []) pushValue(value, 0);
        pushValue(frame.panic, 0);
        for (const iterator of frame.mapIterators.values())
          for (const entry of iterator.entries) {
            pushValue(entry.key, 0);
            pushValue(entry.value, 0);
          }
      }
      if (task.blocked?.kind === "send") pushValue(task.blocked.value, 0);
      if (task.blocked?.kind === "send" || task.blocked?.kind === "receive")
        pushChannel(task.blocked.channel, 0);
      if (task.blocked?.kind === "reflectSelect")
        for (const selected of task.blocked.cases) {
          pushValue(selected.channel, 0);
          pushValue(selected.send, 0);
        }
      for (const value of task.result ?? []) pushValue(value, 0);
    }
    for (const timer of this.timers.values()) if (!timer.canceled) pushChannel(timer.channel, 0);

    const maxDepth = this.options.limits?.maxValueDepth ?? 128;
    while (pending.length) {
      const item = pending.pop()!;
      if (item.depth > maxDepth)
        throw new VMError("value depth exceeds limit", "allocation_limit", "value");
      if (item.cell) {
        if (seenCells.has(item.cell)) continue;
        seenCells.add(item.cell);
        census.liveHeapObjects++;
        census.liveHeapBytes += GUEST_COST.node;
        const stored = item.cell.value;
        if (stored.kind === "bytes")
          census.liveHeapBytes += BigInt((stored.value as Uint8Array).byteLength) * GUEST_COST.byte;
        else if (stored.kind === "array")
          census.liveHeapBytes += BigInt((stored.value as Value[]).length) * GUEST_COST.slot;
        else if (stored.kind === "struct")
          census.liveHeapBytes +=
            BigInt((stored.value as Map<string, Value>).size) * GUEST_COST.mapEntry;
        pushValue(stored, item.depth + 1);
        continue;
      }
      if (item.channel) {
        if (seenObjects.has(item.channel)) continue;
        seenObjects.add(item.channel);
        census.liveGuestBytes += GUEST_COST.node + BigInt(item.channel.capacity) * GUEST_COST.slot;
        census.maxSequence = Math.max(census.maxSequence, item.channel.capacity);
        census.scannedEdges += BigInt(item.channel.queue.length + item.channel.senders.length);
        for (const value of item.channel.queue) pushValue(value, item.depth + 1);
        for (const sender of item.channel.senders) pushValue(sender.value, item.depth + 1);
        continue;
      }
      const value = item.value!;
      census.liveGuestBytes += GUEST_COST.node;
      switch (value.kind) {
        case "string":
        case "bytes": {
          const length = (value.value as Uint8Array).byteLength;
          census.liveGuestBytes += BigInt(length) * GUEST_COST.byte;
          census.maxSequence = Math.max(census.maxSequence, length);
          if (value.kind === "string") census.strings += length;
          break;
        }
        case "array": {
          const values = value.value as Value[];
          if (seenObjects.has(values)) break;
          seenObjects.add(values);
          census.liveGuestBytes += BigInt(values.length) * GUEST_COST.slot;
          census.maxSequence = Math.max(census.maxSequence, values.length);
          census.scannedEdges += BigInt(values.length);
          for (const nested of values) pushValue(nested, item.depth + 1);
          break;
        }
        case "struct": {
          const fields = value.value as Map<string, Value>;
          if (seenObjects.has(fields)) break;
          seenObjects.add(fields);
          census.liveGuestBytes += BigInt(fields.size) * GUEST_COST.mapEntry;
          census.scannedEdges += BigInt(fields.size);
          for (const nested of fields.values()) pushValue(nested, item.depth + 1);
          break;
        }
        case "interface":
          census.scannedEdges++;
          pushValue(value.value as Value, item.depth + 1);
          break;
        case "pointer":
          census.scannedEdges++;
          pushCell((value.value as Address).root, item.depth + 1);
          break;
        case "slice": {
          const slice = value.value as SliceValue;
          census.maxSequence = Math.max(census.maxSequence, slice.capacity);
          census.scannedEdges++;
          pushCell(slice.storage.root, item.depth + 1);
          break;
        }
        case "map": {
          const map = value.value as GuestMap;
          if (seenObjects.has(map)) break;
          seenObjects.add(map);
          census.liveGuestBytes += BigInt(map.entries.length) * GUEST_COST.mapEntry;
          census.maxSequence = Math.max(census.maxSequence, map.entries.length);
          census.scannedEdges += BigInt(map.entries.length * 2);
          for (const entry of map.entries) {
            pushValue(entry.key, item.depth + 1);
            pushValue(entry.value, item.depth + 1);
          }
          break;
        }
        case "channel":
          census.scannedEdges++;
          pushChannel(value.value as Channel, item.depth + 1);
          break;
        case "function":
        case "method": {
          const fn = value.value as FunctionValue;
          census.revisions.add(fn.revision);
          census.scannedEdges += BigInt(fn.captures.length + (fn.receiver ? 1 : 0));
          for (const address of fn.captures) pushCell(address.root, item.depth + 1);
          pushValue(fn.receiver, item.depth + 1);
          break;
        }
        case "dynamicFunction": {
          const callback = (value.value as { callback: FunctionValue }).callback;
          census.revisions.add(callback.revision);
          census.scannedEdges += BigInt(callback.captures.length);
          for (const address of callback.captures) pushCell(address.root, item.depth + 1);
          break;
        }
      }
    }

    census.liveGuestBytes += census.liveHeapBytes + BigInt(this.dynamicTypeBytes) * GUEST_COST.byte;
    if (enforce) {
      if (census.strings > (this.options.limits?.maxStringBytes ?? 64 * 1024 * 1024))
        throw new VMError("string byte limit exceeded", "string_limit", "string");
      this.checkSequence(census.maxSequence, "value");
    }
    this.gcReclaimedBytes += this.guestMemory.publish(census, enforce);
    this.referencedRevisions = census.revisions;
    this.gcCollections++;
    this.gcScannedObjects += census.liveHeapObjects;
    this.gcTracedObjects += census.liveHeapObjects;
    this.gcScannedEdges += census.scannedEdges;
    this.gcNanos += BigInt(Math.max(0, Math.floor((performance.now() - started) * 1_000_000)));
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
    descriptor: Record<string, unknown>,
  ): void {
    const wrapped = inputs[0]!;
    const receiver = wrapped.kind === "interface" ? (wrapped.value as Value) : wrapped;
    const node = this.methodNode(receiver.type);
    const method = node?.methods.find((item) => item.name === descriptorText(descriptor.method));
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
    const fn = this.resolveFunction(fnValue, revision);
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
      if (!timer.channel.closed && timer.channel.queue.length < Math.max(1, timer.channel.capacity))
        timer.channel.queue.push(
          boolValue(this.revision.program.modules.values().next().value!.registry, true),
        );
      this.wakeSelects();
    }
  }

  private completeTask(task: Task, values: Value[]): void {
    task.state = "done";
    task.result = values;
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
  }

  private failTask(task: Task, error: VMError): void {
    task.state = error.code === "canceled" ? "canceled" : "failed";
    task.error = error;
    const initialization = task.frames.find((frame) => frame.initializing)?.initializing;
    if (initialization || task.scope === 0) {
      if (initialization) this.initializing.delete(initialization);
      this.initializationError = error;
      this.closing = true;
    }
    const scope = this.scopes.get(task.scope);
    scope?.tasks.delete(task.id);
    this.tasks.delete(task.id);
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
    this.updateMemoryCensus(false);
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

    for (const [modulePath, oldModule] of current.modules) {
      const module = program.modules.get(modulePath);
      if (!module) throw new VMError("module was removed", "module_removed", modulePath);
      if (module.packageName !== oldModule.packageName)
        throw new VMError("package name changed", "package_changed", modulePath);
      if (module.globals.length !== oldModule.globals.length)
        throw new VMError("global declarations changed", "global_shape_changed", modulePath);
      for (const [id, oldIndex] of oldModule.globalIndexes) {
        const index = module.globalIndexes.get(id);
        if (
          index === undefined ||
          !oldModule.globals[oldIndex]!.value.type.registry.identical(
            oldModule.globals[oldIndex]!.value.type,
            module.globals[index]!.value.type,
          )
        )
          throw new VMError("global declaration changed", "global_shape_changed", modulePath);
      }
      for (const [name, oldExport] of oldModule.exports) {
        const exported = module.exports.get(name);
        if (
          !exported ||
          oldExport.kind !== exported.kind ||
          oldExport.id !== exported.id ||
          oldExport.untyped !== exported.untyped ||
          Boolean(oldExport.type) !== Boolean(exported.type) ||
          (oldExport.type &&
            exported.type &&
            !oldExport.type.registry.identical(oldExport.type, exported.type))
        )
          throw new VMError("export changed shape", "export_shape_changed", modulePath);
      }
      for (const [id, oldFunction] of oldModule.functions) {
        if (oldFunction.revisionLocal) continue;
        const fn = module.functions.get(id);
        if (!fn || fn.revisionLocal)
          throw new VMError("logical function was removed", "call_shape_changed", modulePath);
        const oldParameters = oldFunction.signature.params;
        const parameters = fn.signature.params;
        const oldResults = oldFunction.signature.results;
        const results = fn.signature.results;
        const oldUpvalues = [...oldFunction.upvalueIndexes.keys()];
        const upvalues = [...fn.upvalueIndexes.keys()];
        if (
          oldFunction.signature.variadic !== fn.signature.variadic ||
          oldParameters.length !== parameters.length ||
          oldResults.length !== results.length ||
          oldUpvalues.length !== upvalues.length ||
          oldParameters.some(
            (parameter, index) =>
              !oldModule.registry.identical(
                oldModule.registry.runtime(parameter.type),
                module.registry.runtime(parameters[index]!.type),
              ),
          ) ||
          oldResults.some(
            (result, index) =>
              !oldModule.registry.identical(
                oldModule.registry.runtime(result),
                module.registry.runtime(results[index]!),
              ),
          ) ||
          oldUpvalues.some(
            (upvalue, index) =>
              upvalue !== upvalues[index] ||
              !oldFunction.upvalueTypes[index]!.registry.identical(
                oldFunction.upvalueTypes[index]!,
                fn.upvalueTypes[index]!,
              ),
          )
        )
          throw new VMError("function signature changed", "call_shape_changed", modulePath);
      }
    }
    if (this.generation >= Number.MAX_SAFE_INTEGER)
      throw new VMError("revision generation exhausted", "revision_limit", "patch");
    this.generation++;
    const revision = { generation: this.generation, program };
    // Preserve global cells whose identity and layout match. New code binds to
    // these cells while active frames keep their old revision.
    for (const [modulePath, module] of program.modules) {
      const old = this.revision.program.modules.get(modulePath);
      if (!old) continue;
      for (const [id, index] of module.globalIndexes) {
        const prior = old.globalIndexes.get(id);
        if (prior !== undefined) module.globals[index] = old.globals[prior]!;
      }
    }
    this.revision = revision;
    this.revisions.set(this.generation, revision);
    this.updateMemoryCensus(false);
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
      const info = this.debugFunctions.get(`${frame.fn.module.path}\0${frame.fn.id}`);
      return {
        reference: { epoch: this.epoch, task: BigInt(task.id), depth: BigInt(depth) },
        generation: BigInt(frame.revision.generation),
        scope: BigInt(task.scope),
        program_hash: frame.revision.program.hash,
        module: frame.fn.module.path,
        function: info?.name ?? frame.fn.id,
        pc: BigInt(pc),
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
    const symbols = this.debugFunctions.get(`${frame.fn.module.path}\0${frame.fn.id}`);
    const names = [...frame.fn.localIndexes.keys()].map(
      (name) => symbols?.localNames.get(name) ?? name,
    );
    return { ...snapshot(frame.locals.map((local) => cloneValue(local.value))), names };
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
        const info = this.debugFunctions.get(`${frame.fn.module.path}\0${frame.fn.id}`);
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
    if (this.closed) this.guestMemory.release();
    else this.updateMemoryCensus(false);
    const memory = this.guestMemory.stats();
    const stats: Stats = {
      state: this.closed ? "Closed" : this.closing ? "Closing" : this.paused ? "Paused" : "Running",
      steps: this.steps,
      liveHeapBytes: memory.liveHeapBytes,
      liveHeapObjects: memory.liveHeapObjects,
      peakHeapBytes: memory.peakHeapBytes,
      totalHeapAllocatedBytes: memory.totalHeapAllocatedBytes,
      gcCollections: this.gcCollections,
      gcScannedObjects: this.gcScannedObjects,
      gcTracedObjects: this.gcTracedObjects,
      gcScannedEdges: this.gcScannedEdges,
      gcReclaimedBytes: this.gcReclaimedBytes,
      gcNanos: this.gcNanos,
      liveGuestBytes: memory.liveGuestBytes,
      peakGuestBytes: memory.peakGuestBytes,
      totalGuestAllocatedBytes: memory.totalGuestAllocatedBytes,
      guestAllocatedSinceCensusBytes: memory.guestAllocatedSinceCensusBytes,
      guestAllocations: memory.guestAllocations,
      activeScopes: BigInt(this.scopes.size),
      tasks: BigInt(this.tasks.size),
      timers: BigInt([...this.timers.values()].filter((timer) => !timer.canceled).length),
      ffiCalls: this.ffiCalls,
      ffiBytes: this.ffiBytes,
      generation: BigInt(this.generation),
    };
    if (this.options.profile)
      stats.profile = {
        opcodes: Object.fromEntries(this.opcodeProfile),
        functions: Object.fromEntries(this.functionProfile),
      };
    return stats;
  }

  close(): void {
    if (this.closing) return;
    this.closing = true;
    for (const execution of this.executions.values()) this.cancel(execution.id);
    for (const id of this.pendingHost.keys()) this.pendingActions.push({ kind: "cancel", id });
    if (!this.tasks.size && !this.pendingHost.size) {
      this.closed = true;
      this.guestMemory.release();
      this.pendingActions.push({ kind: "close" });
    }
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

function requiredIndex(indexes: Map<string, number>, value: unknown, kind: string): number {
  const index = indexes.get(descriptorText(value));
  if (index === undefined) invalid(`unknown ${kind}`, descriptorText(value));
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

function descriptorText(value: unknown, fallback = ""): string {
  if (value === undefined && fallback !== undefined) return fallback;
  if (typeof value !== "string") invalid("expected string descriptor", "descriptor");
  return value;
}

function descriptorInteger(value: unknown, fallback?: number): number {
  if (value === undefined && fallback !== undefined) return fallback;
  if (typeof value !== "number" || !Number.isSafeInteger(value))
    invalid("expected integer descriptor", "descriptor");
  return value;
}

function descriptorList(value: unknown): unknown[] {
  if (value === undefined) return [];
  if (!Array.isArray(value)) invalid("expected descriptor array", "descriptor");
  return value;
}

function descriptorRecord(value: unknown): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value))
    invalid("expected descriptor object", "descriptor");
  return value as Record<string, unknown>;
}

function emptyKey(): { module_path: string; decl_id: string } {
  return { module_path: "", decl_id: "" };
}

function valueFromInterface(value: Value): Value {
  return value.kind === "interface" ? (value.value as Value) : value;
}

function addressKey(address: Address): string {
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

function decodeRune(bytes: Uint8Array, offset: number): [number, number] {
  if (offset < 0 || offset >= bytes.length) fail("string index out of range", "string", "panic");
  const first = bytes[offset]!;
  if (first < 0x80) return [first, 1];
  let width = first >= 0xf0 ? 4 : first >= 0xe0 ? 3 : first >= 0xc2 ? 2 : 1;
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

function formatPanic(value: Value): string {
  if (value.kind === "string") return textDecoder.decode(value.value as Uint8Array);
  return String(value.value ?? "panic");
}

function toVMError(error: unknown): VMError {
  if (error instanceof VMError) return error;
  if (error instanceof Error) return new VMError(error.message, "internal", "runtime");
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
  if (state.kind !== "array" || (state.value as Value[]).length !== 8 || bytes.length % 64 !== 0)
    invalid("sha256 state layout is invalid", "crypto.sha256.block");
  const stateValues = state.value as Value[];
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
