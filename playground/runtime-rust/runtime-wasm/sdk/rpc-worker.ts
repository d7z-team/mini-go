import type { InitOutput } from "./wasm/mini_go_wasm.js";
import {
  attachRPCSocket,
  connectRPCSocket,
  flushRPCSocket,
  type RPCTransportOwner,
} from "./rpc-network.js";
import type {
  RPCFailure,
  RPCRequest,
  RPCResponse,
  RPCWorkerPort,
  RPCWorkerStats,
} from "./rpc-protocol.js";
import type { RPCValue } from "./rpc-types.js";

interface RPCActionProviderCall {
  kind: "providerCall";
  id: number;
  provider: number;
  resource?: number;
  method: string;
  arguments: RPCValue[];
  peer: { identity: string; attributes: Record<string, string> };
  providerName: string;
  timeoutMs?: number;
}

type RPCAction =
  | RPCActionProviderCall
  | { kind: "providerCancel"; id: number }
  | { kind: "resourceClose"; id: number; resource: number };

export interface WasmRPCDriver extends RPCTransportOwner {
  bind(operation: number, contract: unknown, options: unknown): Promise<number>;
  invoke(
    operation: number,
    binding: number,
    method: unknown,
    receiver: number | undefined,
    arguments_: unknown,
    options: unknown,
  ): Promise<{ result: number; values: RPCValue[] }>;
  accept(result: number): Promise<void>;
  discard(result: number): Promise<void>;
  close_binding(binding: number): void;
  close_resource(operation: number, resource: number, options: unknown): Promise<void>;
  publish(provider: number, contract: unknown, options: unknown): number;
  close_publication(publication: number): Promise<void>;
  cancel(operation: number): void;
  actions(): RPCAction[];
  provider_complete(
    call: number,
    values: readonly RPCValue[],
    code?: string,
    message?: string,
  ): Promise<number[]>;
  resource_complete(close: number, code?: string, message?: string): void;
  stats(): RPCWorkerStats;
  shutdown(): Promise<void>;
}

export interface WasmRPCModule {
  output: InitOutput;
  create(options: unknown): WasmRPCDriver;
}

function failure(reason: unknown): RPCFailure {
  if (reason && typeof reason === "object") {
    const value = reason as { code?: unknown; message?: unknown };
    return {
      code: typeof value.code === "string" ? value.code : undefined,
      message: String(value.message ?? reason),
    };
  }
  return { message: String(reason) };
}

/** Runs one endpoint-only RPC owner in a platform Worker. */
export function runRPCWorker(
  port: RPCWorkerPort,
  load: (url?: string) => Promise<WasmRPCModule>,
  enqueue: (callback: () => void) => void = (callback) => setTimeout(callback, 0),
): void {
  let rpc: WasmRPCDriver | undefined;
  let socket: WebSocket | undefined;
  let wasmMemory: WebAssembly.Memory | undefined;
  let scheduled = false;
  let closed = false;
  let closing = false;
  let flushTimer: ReturnType<typeof setTimeout> | undefined;
  const outgoing: { id: number; payload: number[] }[] = [];
  const send = port.send;

  const fail = (reason: unknown) => {
    if (closed) return;
    closed = true;
    closing = true;
    clearTimeout(flushTimer);
    rpc?.disconnect();
    socket?.close();
    send({ kind: "fatal", error: failure(reason) });
  };
  const schedule = () => {
    if (closed || scheduled) return;
    scheduled = true;
    enqueue(() => {
      scheduled = false;
      drive();
    });
  };
  (globalThis as typeof globalThis & { __miniGoWake: () => void }).__miniGoWake = schedule;

  function drive(): void {
    if (!rpc || closed) return;
    try {
      for (const action of rpc.actions()) send(action);
      clearTimeout(flushTimer);
      flushTimer = undefined;
      if (flushRPCSocket(socket, rpc, outgoing)) {
        flushTimer = setTimeout(schedule, 10);
      }
    } catch (reason) {
      fail(reason);
    }
  }

  function respond(id: number, work: () => unknown | Promise<unknown>): void {
    void Promise.resolve()
      .then(work)
      .then(
        (value) => send({ kind: "response", id, value }),
        (reason) => send({ kind: "response", id, error: failure(reason) }),
      )
      .finally(schedule)
      .catch(fail);
  }

  function requireRPC(): WasmRPCDriver {
    if (!rpc || closing) throw new Error("RPC connection is unavailable");
    return rpc;
  }

  async function create(data: Extract<RPCRequest, { kind: "create" }>): Promise<void> {
    if (rpc || closing) throw new Error("worker already owns an RPC connection");
    const module = await load(data.options.wasmUrl);
    if (closing) return;
    const connected = await connectRPCSocket(data.url);
    if (closing) {
      connected.close();
      return;
    }
    wasmMemory = module.output.memory;
    socket = connected;
    rpc = module.create({
      leaseTtlMs: data.options.leaseTtlMs,
      admissionTimeoutMs: data.options.admissionTimeoutMs,
      maxCallDurationMs: data.options.maxCallDurationMs,
    });
    attachRPCSocket(socket, rpc, schedule, fail, () =>
      fail(new Error("RPC WebSocket disconnected")),
    );
    send({ kind: "ready" });
    schedule();
  }

  async function shutdown(): Promise<void> {
    if (closing) return;
    closing = true;
    if (!rpc) {
      closed = true;
      send({ kind: "closed" });
      return;
    }
    try {
      const pending = rpc.shutdown();
      schedule();
      await pending;
      drive();
      closed = true;
      clearTimeout(flushTimer);
      socket?.close();
      send({ kind: "closed" });
    } catch (reason) {
      fail(reason);
    }
  }

  port.listen((data) => {
    try {
      if (data.kind === "create") {
        void create(data).catch(fail);
      } else if (data.kind === "close") {
        void shutdown();
      } else if (data.kind === "cancel") {
        rpc?.cancel(data.id);
      } else if (data.kind === "providerResult") {
        const settled = rpc
          ? rpc.provider_complete(data.call, data.values, data.error?.code, data.error?.message)
          : Promise.resolve([]);
        void settled
          .then((retained) => send({ kind: "providerSettled", id: data.call, retained }))
          .finally(schedule)
          .catch(fail);
      } else if (data.kind === "resourceClosed") {
        rpc?.resource_complete(data.close, data.error?.code, data.error?.message);
      } else {
        const owner = requireRPC();
        switch (data.kind) {
          case "bind":
            respond(data.id, () => owner.bind(data.id, data.contract, data.options));
            break;
          case "invoke":
            respond(data.id, () =>
              owner.invoke(data.id, data.binding, data.method, data.receiver, data.arguments, {
                timeoutMs: data.timeoutMs,
              }),
            );
            break;
          case "accept":
            respond(data.id, () => owner.accept(data.result));
            break;
          case "discard":
            respond(data.id, () => owner.discard(data.result));
            break;
          case "closeBinding":
            respond(data.id, () => owner.close_binding(data.binding));
            break;
          case "closeResource":
            respond(data.id, () =>
              owner.close_resource(data.id, data.resource, { timeoutMs: data.timeoutMs }),
            );
            break;
          case "publish":
            respond(data.id, () => owner.publish(data.provider, data.contract, data.options));
            break;
          case "closePublication":
            respond(data.id, () => owner.close_publication(data.publication));
            break;
          case "stats":
            respond(data.id, () => ({
              ...owner.stats(),
              wasmBytes: wasmMemory?.buffer.byteLength ?? 0,
            }));
            break;
        }
      }
      schedule();
    } catch (reason) {
      if ("id" in data) send({ kind: "response", id: data.id, error: failure(reason) });
      else fail(reason);
    }
  });
}
