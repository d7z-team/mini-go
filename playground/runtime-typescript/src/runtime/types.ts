import type { Snapshot, VMOptions } from "../vm/types";

export type * from "../vm/types";

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

export interface Options extends VMOptions {
  signal?: AbortSignal;
  provider?: (request: HostRequest) => Uint8Array | Promise<Uint8Array>;
  /** Absolute URL of a worker provider module exporting call and optional close. */
  providerModule?: string | URL;
  /** Override the browser Worker entry URL when deploying assets separately. */
  workerUrl?: string | URL;
  rpcUrl?: string;
  rpcOptions?: RPCOptions;
}
