import type { Bindings, Frame, FrameRef, HostValue, Options, Snapshot, Stats } from "./types.js";

/** Own the visible bytes, including Buffer and offset views, before transfer or retention. */
export function copyBytes(source: Uint8Array | ArrayBuffer): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(source instanceof Uint8Array ? source : new Uint8Array(source));
}

export interface Failure {
  message: string;
  code?: string;
  path?: string;
}
export function serializeError(value: unknown): Failure {
  if (value && typeof value === "object" && "message" in value) {
    return {
      message: String(value.message),
      code: "code" in value && typeof value.code === "string" ? value.code : undefined,
      path: "path" in value && typeof value.path === "string" ? value.path : undefined,
    };
  }
  return { message: String(value) };
}
export function toError(value: unknown): Error {
  if (value instanceof Error) return value;
  const failure = serializeError(value);
  return Object.assign(new Error(failure.message), failure);
}
export type Configuration = Omit<
  Options,
  "signal" | "provider" | "providerModule" | "workerUrl" | "wasmUrl"
> & { providerModule?: string; wasmUrl?: string };
export type Control =
  | {
      kind: "debugOpen";
      sources: Record<string, { module: string; path: string; text: string }>;
      entry: string;
    }
  | { kind: "debugRequest"; command: string; arguments: Record<string, unknown> }
  | { kind: "debugEvents" }
  | { kind: "debugOutput"; category: string; text: string }
  | { kind: "pause" | "resume" | "stack" | "stats" }
  | { kind: "patch"; image: Uint8Array }
  | { kind: "bindings"; frame: FrameRef }
  | { kind: "breakpoints"; module: string; file: string; lines: (number | bigint)[] };
export type Command = Control & { id: number };
export type ControlResult =
  | void
  | Frame[]
  | Bindings
  | bigint[]
  | Stats
  | Record<string, unknown>
  | Record<string, unknown>[];
export interface Start {
  kind: "start";
  id: number;
  entry: string;
  arguments: HostValue[];
}
export type Request =
  | CompilerCommand
  | Command
  | Start
  | { kind: "create"; image: Uint8Array; options: Configuration }
  | { kind: "cancel"; id: number }
  | { kind: "close" }
  | { kind: "hostResult"; id: number; payload: Uint8Array; error?: string };
export type Response =
  | CompilerResponse
  | { kind: "ready" | "closed" }
  | { kind: "fatal"; error: Failure }
  | { kind: "result"; id: number; value?: Snapshot; error?: Failure }
  | { kind: "settled"; id: number; error?: Failure }
  | { kind: "response"; id: number; value?: ControlResult; error?: Failure }
  | { kind: "host"; id: number; route: string; payload: Uint8Array }
  | { kind: "hostCancel"; id: number };

export type CompilerCommand =
  | {
      kind: "compilerCreate";
      generation: bigint;
      image: Uint8Array;
      restore: Uint8Array;
      wasmUrl?: string;
    }
  | { kind: "compilerRequest"; generation: bigint; id: number; input: string; timeout: number }
  | { kind: "compilerAck" | "compilerCancel"; generation: bigint; id: number }
  | { kind: "compilerClose" | "compilerStats"; generation: bigint; id: number };
export type CompilerResponse = {
  kind: "compilerResponse";
  reusable?: boolean;
  generation: bigint;
  id: number;
  value?: string;
  restore?: Uint8Array;
  stats?: Stats;
  error?: Failure;
};

/** The adapters own platform workers; the runtime owns admission and lifetime. */
export interface WorkerConnection {
  send(message: Request, transfer?: ArrayBuffer[]): void;
  listen(message: (value: Response) => void, failure: (reason: unknown) => void): void;
  terminate(): void;
}
export type WorkerFactory = (options: Options) => WorkerConnection;
export interface WorkerPort {
  send(message: Response): void;
  listen(handler: (message: Request) => void): void;
}
export type Action =
  | { kind: "call"; id: number; route: string; payload: number[] }
  | { kind: "cancel"; id: number }
  | { kind: "decision"; id: number; accepted: boolean }
  | { kind: "close" };
export interface PumpState {
  running: boolean;
  ready: boolean;
  closed: boolean;
  delay?: number;
  error?: string;
  executions: { id: number; state: string; settled: boolean; scopeError?: string }[];
}
