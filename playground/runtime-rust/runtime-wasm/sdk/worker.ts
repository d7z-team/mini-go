import { WasmVm, type InitOutput } from "./wasm/mini_go_wasm.js";
import { deferred, type Deferred } from "./deferred.js";
import {
  serializeError,
  type Action,
  type Failure,
  type PumpState,
  type Start,
  type WorkerPort,
  type ControlResult,
} from "./protocol.js";
import type { HostReply, WorkerProvider } from "./types.js";
import {
  attachRPCSocket,
  connectRPCSocket,
  flushRPCSocket,
  type RPCTransportOwner,
} from "./rpc-network.js";

export function runWorker(
  port: WorkerPort,
  load: (url?: string) => Promise<InitOutput>,
  enqueue?: (callback: () => void) => void,
): void {
  interface RpcTransport extends RPCTransportOwner {
    close_network(): Promise<unknown>;
  }
  let vm: WasmVm & Partial<RpcTransport>,
    provider: WorkerProvider | undefined,
    socket: WebSocket | undefined,
    wasmMemory: WebAssembly.Memory;
  let closing = false,
    closed = false,
    finalizing = false,
    scheduled = false,
    ready = false;
  let timer: ReturnType<typeof setTimeout> | undefined, initializationError: string | undefined;
  let timerDeadline = Infinity;
  interface Call extends Start {
    returned?: boolean;
    error?: Failure;
  }
  interface HostCall {
    controller: AbortController;
    result?: HostReply;
    work?: Promise<void>;
    cleanup?: Promise<void>;
  }
  const calls = new Map<number, Call>(),
    queued: Call[] = [],
    hosts = new Map<number, HostCall>(),
    mainReplies = new Map<number, Deferred<Uint8Array>>();
  const resume = () => {
    scheduled = false;
    drive();
  };
  if (!enqueue) {
    const channel = new MessageChannel();
    channel.port1.onmessage = resume;
    enqueue = () => channel.port2.postMessage(null);
  }
  const enqueueTurn = enqueue;
  const send = port.send;
  const failWorker = (error: unknown) => {
    if (!closed) {
      closed = true;
      clearTimeout(timer);
      socket?.close();
      send({ kind: "fatal", error: serializeError(error) });
    }
  };
  function schedule(delay = 0) {
    if (closed || scheduled) return;
    if (delay > 0) {
      const deadline = performance.now() + delay;
      if (timer !== undefined && timerDeadline <= deadline) return;
      clearTimeout(timer);
      timerDeadline = deadline;
      timer = setTimeout(
        () => {
          timer = undefined;
          schedule();
        },
        Math.min(2_147_483_647, delay),
      );
      return;
    }
    scheduled = true;
    clearTimeout(timer);
    timer = undefined;
    enqueueTurn(resume);
  }
  function rejectCall(id: number, reason: unknown) {
    const error = serializeError(reason);
    send({ kind: "result", id, error });
    send({ kind: "settled", id, error });
  }
  (globalThis as typeof globalThis & { __miniGoWake: () => void }).__miniGoWake = schedule;

  async function dispatchHostAction(action: Action): Promise<void> {
    if (action.kind === "call") {
      const controller = new AbortController();
      const record: HostCall = { controller };
      hosts.set(action.id, record);
      record.work = (async () => {
        try {
          const payload = Uint8Array.from(action.payload);
          let reply: Uint8Array | HostReply;
          if (provider)
            reply = await provider.call({
              route: action.route,
              payload,
              signal: controller.signal,
            });
          else {
            const pending = deferred<Uint8Array>();
            mainReplies.set(action.id, pending);
            send({ kind: "host", id: action.id, route: action.route, payload });
            reply = await pending.promise;
          }
          record.result = reply instanceof Uint8Array ? { payload: reply } : reply;
          const bytes = record.result?.payload;
          if (!(bytes instanceof Uint8Array))
            throw new Error("provider must return Uint8Array or {payload, consumed, discard}");
          if (bytes.byteLength > 4 * 1024 * 1024) throw new Error("host result exceeds WASM limit");
          vm.complete(action.id, bytes, undefined);
        } catch (error) {
          vm.complete(action.id, new Uint8Array(), String(error));
        } finally {
          schedule();
        }
      })();
      record.work.catch(failWorker);
    } else if (action.kind === "cancel") {
      hosts.get(action.id)?.controller.abort();
      send({ kind: "hostCancel", id: action.id });
    } else if (action.kind === "decision") {
      const record = hosts.get(action.id);
      if (record) {
        record.cleanup = (async () => {
          try {
            await record.result?.[action.accepted ? "consumed" : "discard"]?.();
          } finally {
            vm.decision_done(action.id);
            hosts.delete(action.id);
            schedule();
          }
        })();
        await record.cleanup;
      }
    } else if (action.kind === "close") {
      // Completion decisions may be queued while promises are settling. Drain
      // them before acknowledging session close, including asynchronous discard.
      for (const record of hosts.values()) record.controller.abort();
      await Promise.all([...hosts.values()].map((record) => record.work));
      for (const next of vm.actions()) await dispatchHostAction(next);
      await Promise.all([...hosts.values()].map((record) => record.cleanup));
      if (hosts.size) throw new Error("host session closed with undecided results");
      await provider?.close?.();
      vm.host_closed();
      schedule();
    }
  }

  const outgoing: { id: number; payload: number[] }[] = [];
  function flushNetwork() {
    if (flushRPCSocket(socket, vm, outgoing)) schedule(10);
  }

  function drive() {
    if (!vm || closed) return;
    try {
      if (finalizing) {
        flushNetwork();
        return;
      }
      let state: PumpState;
      const until = performance.now() + 3;
      do {
        state = vm.pump(256);
      } while (state.running && performance.now() < until);
      for (const action of vm.actions()) dispatchHostAction(action).catch(failWorker);
      flushNetwork();
      if (state.error && !closing) {
        closing = true;
        initializationError = state.error;
      }
      if (state.ready && !ready && !closing) {
        ready = true;
        send({ kind: "ready" });
      }
      for (const execution of state.executions) {
        const call = calls.get(execution.id);
        if (!call) continue;
        if (!call.returned && ["Completed", "Failed", "Canceled"].includes(execution.state)) {
          call.returned = true;
          try {
            send({ kind: "result", id: call.id, value: vm.result(execution.id) });
          } catch (error) {
            call.error = serializeError(error);
            send({ kind: "result", id: call.id, error: call.error });
          }
        }
        if (execution.settled) {
          send({
            kind: "settled",
            id: call.id,
            error:
              call.error ??
              (execution.scopeError ? serializeError(execution.scopeError) : undefined),
          });
          calls.delete(execution.id);
          vm.release(execution.id);
        }
      }
      if (
        ready &&
        !closing &&
        ![...calls.values()].some((call) => !call.returned) &&
        queued.length
      ) {
        const call = queued.shift()!;
        try {
          const id = vm.start(call.entry, call.arguments);
          calls.set(id, call);
          schedule();
        } catch (error) {
          rejectCall(call.id, error);
          schedule();
        }
      }
      if (state.closed) {
        finalizing = true;
        if (!closing) closing = true;
        Promise.resolve(vm.close_network?.()).then(() => {
          socket?.close();
          closed = true;
          clearTimeout(timer);
          send(
            initializationError
              ? { kind: "fatal", error: serializeError(initializationError) }
              : { kind: "closed" },
          );
        }, failWorker);
        return;
      }
      if (state.running) schedule();
      else if (state.delay !== null && state.delay !== undefined)
        schedule(Math.max(0, state.delay));
    } catch (error) {
      failWorker(error);
    }
  }

  port.listen(async (data) => {
    try {
      if (data.kind === "create") {
        if (vm) throw new Error("worker already owns an instance");
        const { providerModule, rpcUrl, wasmUrl, ...configuration } = data.options;
        const options = { ...configuration, rpc: Boolean(rpcUrl) };
        wasmMemory = (await load(wasmUrl)).memory;
        if (providerModule) provider = await import(providerModule);
        if (rpcUrl) {
          socket = await connectRPCSocket(rpcUrl);
        }
        vm = new WasmVm(data.image, options);
        if (closing) vm.close();
        if (socket) {
          attachRPCSocket(socket, vm as RpcTransport, schedule, failWorker);
        }
      } else if (data.kind === "hostResult") {
        const reply = mainReplies.get(data.id);
        mainReplies.delete(data.id);
        if (reply) data.error ? reply.reject(new Error(data.error)) : reply.resolve(data.payload);
      } else if (data.kind === "start") {
        if (closing || queued.length + calls.size >= 128)
          throw new Error("instance call queue unavailable");
        queued.push(data);
      } else if (data.kind === "cancel") {
        const index = queued.findIndex((call) => call.id === data.id);
        if (index >= 0) {
          queued.splice(index, 1);
          rejectCall(data.id, "execution canceled");
        }
        for (const [id, call] of calls) if (call.id === data.id) vm.cancel(id);
      } else if (data.kind === "close") {
        closing = true;
        for (const call of queued.splice(0)) {
          rejectCall(call.id, "instance closing");
        }
        vm?.close();
      } else {
        try {
          let value: ControlResult = undefined;
          if (data.kind === "pause") {
            vm.pause();
            vm.pump(1);
          } else if (data.kind === "resume") vm.resume();
          else if (data.kind === "patch") vm.patch(data.image);
          else if (data.kind === "stack") value = vm.stack();
          else if (data.kind === "bindings") value = vm.bindings(data.frame);
          else if (data.kind === "debugOpen") vm.debug_open(data.sources, data.entry);
          else if (data.kind === "debugRequest")
            value = vm.debug_request(data.command, data.arguments);
          else if (data.kind === "debugEvents") value = vm.debug_events();
          else if (data.kind === "debugOutput") vm.debug_output(data.category, data.text);
          else if (data.kind === "breakpoints")
            value = vm.breakpoints(data.module, data.file, data.lines);
          else if (data.kind === "stats")
            value = { ...vm.stats(), wasmBytes: wasmMemory.buffer.byteLength };
          else throw new Error("unknown worker command");
          send({ kind: "response", id: data.id, value });
        } catch (error) {
          send({ kind: "response", id: data.id, error: serializeError(error) });
        }
      }
      schedule();
    } catch (error) {
      failWorker(error);
    }
  });
}
