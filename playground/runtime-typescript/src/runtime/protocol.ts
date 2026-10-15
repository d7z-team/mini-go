import type { WorkerConnection, WorkerFailure, WorkerPort } from "../worker-channel";
import type { Bindings, Frame, FrameRef, HostValue, Options, Snapshot, Stats } from "./types";

export type RuntimeConfiguration = Omit<
  Options,
  "signal" | "provider" | "providerModule" | "workerUrl"
> & { providerModule?: string };

export type RuntimeControl =
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

export type RuntimeCommand = RuntimeControl & { id: number };

export type RuntimeControlResult =
  | void
  | Frame[]
  | Bindings
  | bigint[]
  | Stats
  | Record<string, unknown>
  | Record<string, unknown>[];

export interface RuntimeStart {
  kind: "start";
  id: number;
  entry: string;
  arguments: HostValue[];
}

export type RuntimeRequest =
  | RuntimeCommand
  | RuntimeStart
  | { kind: "create"; image: Uint8Array; options: RuntimeConfiguration }
  | { kind: "cancel"; id: number }
  | { kind: "close" }
  | { kind: "hostResult"; id: number; payload: Uint8Array; error?: string };

export type RuntimeResponse =
  | { kind: "limits"; maxInputBytes: number; maxHostResultBytes: number; maxImageBytes: number }
  | { kind: "ready" | "closed" }
  | { kind: "fatal"; error: WorkerFailure }
  | { kind: "result"; id: number; value?: Snapshot; error?: WorkerFailure }
  | { kind: "settled"; id: number; error?: WorkerFailure }
  | { kind: "response"; id: number; value?: RuntimeControlResult; error?: WorkerFailure }
  | { kind: "host"; id: number; route: string; payload: Uint8Array }
  | { kind: "hostCancel"; id: number };

export type RuntimeWorkerConnection = WorkerConnection<RuntimeRequest, RuntimeResponse>;
export type RuntimeWorkerFactory = (options: Options) => RuntimeWorkerConnection;
export type RuntimeWorkerPort = WorkerPort<RuntimeRequest, RuntimeResponse>;
