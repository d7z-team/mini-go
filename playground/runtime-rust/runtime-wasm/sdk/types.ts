export type TypeIdentity =
  | "Void"
  | "Any"
  | { Primitive: number }
  | { Pointer: TypeIdentity }
  | { Slice: TypeIdentity }
  | { Named: { module_path: string; decl_id: string } }
  | { Structural: { module: string; node: string } };
export interface HostValue {
  typ: TypeIdentity;
  data: HostData;
}
export type HostData =
  | "Nil"
  | { Bool: boolean }
  | { Integer: bigint }
  | { Unsigned: bigint }
  | { FloatBits: bigint }
  | { ComplexBits: { real: bigint; imag: bigint } }
  | { String: Uint8Array }
  | { Array: HostValue[] }
  | { Struct: Record<string, HostValue> }
  | { Interface: HostValue }
  | { Pointer: HostAddress }
  | { Slice: { storage: HostAddress; start: bigint; length: bigint; capacity: bigint } }
  | { Map: bigint }
  | { MapEntries: [HostValue, HostValue][] }
  | { Resource: bigint }
  | { Function: { module: string; function: string; captures: HostAddress[] } }
  | { DynamicFunction: HostValue }
  | { Method: { function: HostValue; receiver?: HostValue } }
  | { Channel: { capacity: bigint; closed: boolean; queued: HostValue[] } }
  | { Mutex: { locked: boolean; waiting: bigint; granted: boolean } };
export type PathElement =
  | { Field: string }
  | { Index: bigint }
  | { ArrayView: { start: bigint; length: bigint; typ: TypeIdentity } }
  | { TypeView: TypeIdentity };
export interface HostAddress {
  object: bigint;
  path: PathElement[];
}
export interface Snapshot {
  roots: HostValue[];
  objects: HostValue[];
}
export interface Execution {
  result: Promise<Snapshot>;
  settled: Promise<void>;
  cancel(): void;
}
export interface HostRequest {
  route: string;
  payload: Uint8Array;
  signal: AbortSignal;
}
export interface HostReply {
  payload: Uint8Array;
  consumed?: () => void | Promise<void>;
  discard?: () => void | Promise<void>;
}
export interface WorkerProvider {
  call(request: HostRequest): Uint8Array | HostReply | Promise<Uint8Array | HostReply>;
  close?(): void | Promise<void>;
}
export interface RPCOptions {
  leaseTtlMs?: number;
  admissionTimeoutMs?: number;
  maxCallDurationMs?: number;
}
export interface Options {
  /** Select the bounded compiler image and execution envelope. */
  workload?: "runtime" | "compiler";
  signal?: AbortSignal;
  provider?: (request: HostRequest) => Uint8Array | Promise<Uint8Array>;
  /** Absolute URL of a worker provider module exporting call and optional close. */
  providerModule?: string | URL;
  /** Override the browser Worker entry URL when deploying assets separately. */
  workerUrl?: string | URL;
  /** Override the WASM asset URL (HTTP(S), or file: in Node.js). */
  wasmUrl?: string | URL;
  rpcUrl?: string;
  rpcOptions?: RPCOptions;
  capabilities?: string[];
  /** 0/default: 100 million, -1: unlimited; positive values fit signed i64. */
  maxSteps?: number | bigint;
  maxHeapBytes?: number | bigint;
  maxPendingCalls?: number;
  symbols?: Uint8Array;
}
export interface FrameRef {
  epoch: bigint;
  task: bigint;
  depth: bigint;
}
export interface SourceLocation {
  file: string;
  line: bigint;
  column: bigint;
}
export interface Frame {
  reference: FrameRef;
  generation: bigint;
  scope: bigint;
  program_hash: string;
  module: string;
  function: string;
  pc: bigint;
  locations: SourceLocation[];
}
export interface Stats {
  state: string;
  steps: bigint;
  heapBytes: bigint;
  heapObjects: bigint;
  heapPeakBytes: bigint;
  heapAllocatedBytes: bigint;
  memoryBytes: bigint;
  memoryPeakBytes: bigint;
  memoryAllocatedBytes: bigint;
  activeScopes: bigint;
  tasks: bigint;
  timers: bigint;
  ffiCalls: bigint;
  ffiBytes: bigint;
  generation: bigint;
  wasmBytes: number;
}
export type Bindings = Snapshot & { names: string[] };
