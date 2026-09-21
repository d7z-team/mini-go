import type {
  RPCBindOptions,
  RPCConnectOptions,
  RPCContract,
  RPCMethod,
  RPCPublishOptions,
  RPCStats,
  RPCValue,
} from "./rpc-types.js";

export interface RPCFailure {
  message: string;
  code?: string;
}

export type RPCConfiguration = Omit<RPCConnectOptions, "signal" | "workerUrl" | "wasmUrl"> & {
  wasmUrl?: string;
};

export type RPCRequest =
  | { kind: "create"; url: string; options: RPCConfiguration }
  | {
      kind: "bind";
      id: number;
      contract: RPCContract;
      options: Omit<RPCBindOptions, "signal">;
    }
  | {
      kind: "invoke";
      id: number;
      binding: number;
      method: RPCMethod;
      receiver?: number;
      arguments: readonly RPCValue[];
      timeoutMs?: number;
    }
  | { kind: "accept" | "discard"; id: number; result: number }
  | { kind: "closeBinding"; id: number; binding: number }
  | { kind: "closeResource"; id: number; resource: number; timeoutMs?: number }
  | {
      kind: "publish";
      id: number;
      provider: number;
      contract: RPCContract;
      options: RPCPublishOptions;
    }
  | { kind: "closePublication"; id: number; publication: number }
  | { kind: "stats"; id: number }
  | { kind: "cancel"; id: number }
  | {
      kind: "providerResult";
      call: number;
      values: readonly RPCValue[];
      error?: RPCFailure;
    }
  | { kind: "resourceClosed"; close: number; error?: RPCFailure }
  | { kind: "close" };

export type RPCResponse =
  | { kind: "ready" | "closed" }
  | { kind: "fatal"; error: RPCFailure }
  | { kind: "response"; id: number; value?: unknown; error?: RPCFailure }
  | {
      kind: "providerCall";
      id: number;
      provider: number;
      resource?: number;
      method: string;
      arguments: readonly RPCValue[];
      peer: { identity: string; attributes: Record<string, string> };
      providerName: string;
      timeoutMs?: number;
    }
  | { kind: "providerCancel"; id: number }
  | { kind: "providerSettled"; id: number; retained: readonly number[] }
  | { kind: "resourceClose"; id: number; resource: number };

export interface RPCWorkerConnection {
  send(message: RPCRequest): void;
  listen(message: (value: RPCResponse) => void, failure: (reason: unknown) => void): void;
  terminate(): void;
}

export type RPCWorkerFactory = (options: RPCConnectOptions) => RPCWorkerConnection;

export interface RPCWorkerPort {
  send(message: RPCResponse): void;
  listen(handler: (message: RPCRequest) => void): void;
}

export type RPCWorkerStats = Omit<RPCStats, "wasmBytes">;
