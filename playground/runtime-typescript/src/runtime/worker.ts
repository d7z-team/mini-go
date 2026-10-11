import { deferred, type Deferred } from "../deferred.js";
import { serializeError, type WorkerFailure } from "../worker-channel.js";
import type { RuntimeControlResult, RuntimeStart, RuntimeWorkerPort } from "./protocol.js";
import type { HostReply, WorkerProvider } from "./types.js";
import { VirtualMachine, type VMAction } from "../vm/vm.js";
import { RuntimeRPCBridge } from "./rpc-bridge.js";

/** Own one pure TypeScript VM inside a browser or Node worker. */
export function runRuntimeWorker(
  port: RuntimeWorkerPort,
  enqueue?: (callback: () => void) => void,
): void {
  let vm: VirtualMachine | undefined;
  let provider: WorkerProvider | undefined;
  let rpc: RuntimeRPCBridge | undefined;
  let closing = false,
    closed = false,
    scheduled = false,
    ready = false,
    finalizing = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let timerDeadline = Infinity;
  let initializationError: string | undefined;
  interface Call extends RuntimeStart {
    internal?: number;
    returned?: boolean;
    error?: WorkerFailure;
  }
  interface HostCall {
    controller: AbortController;
    result?: HostReply;
    work?: Promise<void>;
    cleanup?: Promise<void>;
  }
  const calls = new Map<number, Call>();
  const internalCalls = new Map<number, Call>();
  const queued: Call[] = [];
  const hosts = new Map<number, HostCall>();
  const mainReplies = new Map<number, Deferred<Uint8Array>>();
  if (!enqueue) {
    const channel = new MessageChannel();
    channel.port1.onmessage = () => {
      scheduled = false;
      drive();
    };
    enqueue = (callback) => {
      channel.port1.onmessage = () => {
        scheduled = false;
        callback();
      };
      channel.port2.postMessage(null);
    };
  }
  const enqueueTurn = enqueue;
  const send = port.send;

  const failWorker = (error: unknown) => {
    if (closed) return;
    closed = true;
    clearTimeout(timer);
    send({ kind: "fatal", error: serializeError(error) });
  };

  function schedule(delay = 0): void {
    if (closed || scheduled) return;
    if (delay > 0) {
      const deadline = performance.now() + delay;
      if (timer !== undefined && timerDeadline <= deadline) return;
      clearTimeout(timer);
      timerDeadline = deadline;
      timer = setTimeout(
        () => {
          timer = undefined;
          timerDeadline = Infinity;
          schedule();
        },
        Math.min(2_147_483_647, delay),
      );
      return;
    }
    scheduled = true;
    clearTimeout(timer);
    timer = undefined;
    enqueueTurn(() => {
      scheduled = false;
      drive();
    });
  }

  function rejectCall(call: Call, reason: unknown): void {
    const error = serializeError(reason);
    send({ kind: "result", id: call.id, error });
    send({ kind: "settled", id: call.id, error });
    calls.delete(call.id);
    if (call.internal !== undefined) internalCalls.delete(call.internal);
  }

  async function dispatchHostAction(action: VMAction): Promise<void> {
    if (!vm) return;
    if (action.kind === "call") {
      const controller = new AbortController();
      const record: HostCall = { controller };
      hosts.set(action.id, record);
      record.work = (async () => {
        try {
          const payload = Uint8Array.from(action.payload);
          let reply: Uint8Array | HostReply;
          if (action.route === "minigo.mrpc/v2" && rpc) {
            reply = await rpc.call(payload, controller.signal);
          } else if (provider) {
            reply = await provider.call({
              route: action.route,
              payload,
              signal: controller.signal,
            });
          } else {
            const pending = deferred<Uint8Array>();
            mainReplies.set(action.id, pending);
            send({ kind: "host", id: action.id, route: action.route, payload });
            reply = await pending.promise;
          }
          record.result = reply instanceof Uint8Array ? { payload: reply } : reply;
          if (!(record.result.payload instanceof Uint8Array))
            throw new Error("provider must return Uint8Array or HostReply");
          if (record.result.payload.byteLength > vm!.maxHostResultBytes)
            throw new Error("host result exceeds limit");
          vm!.complete(action.id, record.result.payload);
        } catch (error) {
          vm!.complete(action.id, new Uint8Array(), String(error));
        } finally {
          schedule();
        }
      })();
      record.work.catch(failWorker);
      return;
    }
    if (action.kind === "cancel") {
      hosts.get(action.id)?.controller.abort();
      send({ kind: "hostCancel", id: action.id });
      return;
    }
    if (action.kind === "decision") {
      const record = hosts.get(action.id);
      if (!record) return;
      record.cleanup = (async () => {
        try {
          await record.result?.[action.accepted ? "consumed" : "discard"]?.();
        } finally {
          vm?.decisionDone(action.id);
          hosts.delete(action.id);
          schedule();
        }
      })();
      await record.cleanup;
      return;
    }
    for (const record of hosts.values()) record.controller.abort();
    await Promise.all([...hosts.values()].map((record) => record.work));
    await Promise.all([...hosts.values()].map((record) => record.cleanup));
    const [providerClose, rpcClose] = await Promise.allSettled([provider?.close?.(), rpc?.close()]);
    if (providerClose.status === "rejected") throw providerClose.reason;
    if (rpcClose.status === "rejected") throw rpcClose.reason;
    vm.hostClosed();
    finalizing = false;
    closed = true;
    clearTimeout(timer);
    send(
      initializationError
        ? { kind: "fatal", error: serializeError(initializationError) }
        : { kind: "closed" },
    );
  }

  function drive(): void {
    if (!vm || closed || finalizing) return;
    try {
      rpc?.pump();
      let state;
      const until = performance.now() + 3;
      do {
        state = vm.pump(256);
      } while (state.running && performance.now() < until);
      for (const action of vm.actions()) {
        if (action.kind === "close") finalizing = true;
        void dispatchHostAction(action).catch(failWorker);
      }
      if (state.error && !closing) {
        closing = true;
        initializationError = state.error;
      }
      if (state.ready && !ready && !closing) {
        ready = true;
        send({ kind: "ready" });
      }
      for (const execution of state.executions) {
        const call = internalCalls.get(execution.id);
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
          const error =
            call.error ?? (execution.scopeError ? serializeError(execution.scopeError) : undefined);
          send(error ? { kind: "settled", id: call.id, error } : { kind: "settled", id: call.id });
          calls.delete(call.id);
          internalCalls.delete(execution.id);
          vm.release(execution.id);
        }
      }
      if (
        ready &&
        !closing &&
        ![...calls.values()].some((call) => call.internal !== undefined && !call.returned) &&
        queued.length
      ) {
        const call = queued.shift()!;
        try {
          call.internal = vm.start(call.entry, call.arguments);
          calls.set(call.id, call);
          internalCalls.set(call.internal, call);
          schedule();
        } catch (error) {
          rejectCall(call, error);
        }
      }
      if (state.running) schedule();
      else if (state.delay !== undefined) schedule(Math.max(0, state.delay));
    } catch (error) {
      failWorker(error);
    }
  }

  port.listen(async (data) => {
    try {
      if ("generation" in data) throw new Error("compiler commands require the compiler worker");
      if (data.kind === "create") {
        if (vm) throw new Error("worker already owns an instance");
        const { providerModule, rpcUrl, rpcOptions, ...configuration } = data.options;
        if (providerModule) provider = (await import(providerModule)) as WorkerProvider;
        if (rpcUrl)
          rpc = await RuntimeRPCBridge.connect(rpcUrl, rpcOptions ?? {}, schedule, failWorker);
        vm = await VirtualMachine.create(data.image, configuration);
        send({
          kind: "limits",
          maxInputBytes: vm.maxInputBytes,
          maxHostResultBytes: vm.maxHostResultBytes,
          maxImageBytes: vm.maxImageBytes,
        });
      } else if (data.kind === "hostResult") {
        const reply = mainReplies.get(data.id);
        mainReplies.delete(data.id);
        if (reply) data.error ? reply.reject(new Error(data.error)) : reply.resolve(data.payload);
      } else if (data.kind === "start") {
        if (closing || queued.length + calls.size >= 128)
          throw new Error("instance call queue unavailable");
        const call: Call = data;
        queued.push(call);
      } else if (data.kind === "cancel") {
        const index = queued.findIndex((call) => call.id === data.id);
        if (index >= 0) rejectCall(queued.splice(index, 1)[0]!, "execution canceled");
        const call = calls.get(data.id);
        if (call?.internal !== undefined) vm?.cancel(call.internal);
      } else if (data.kind === "close") {
        closing = true;
        for (const call of queued.splice(0)) rejectCall(call, "instance closing");
        vm?.close();
      } else {
        if (!vm) throw new Error("worker is not initialized");
        let value: RuntimeControlResult = undefined;
        if (data.kind === "pause") vm.pause();
        else if (data.kind === "resume") vm.resume();
        else if (data.kind === "patch") await vm.patch(data.image);
        else if (data.kind === "stack") value = vm.stack();
        else if (data.kind === "bindings") value = vm.bindings(data.frame);
        else if (data.kind === "debugOpen") vm.debugOpen(data.sources, data.entry);
        else if (data.kind === "debugRequest")
          value = vm.debugRequest(data.command, data.arguments);
        else if (data.kind === "debugEvents") value = vm.debugEvents();
        else if (data.kind === "debugOutput") vm.debugOutput(data.category, data.text);
        else if (data.kind === "breakpoints")
          value = vm.breakpoints(data.module, data.file, data.lines);
        else if (data.kind === "stats") value = vm.stats();
        else throw new Error("unknown worker command");
        send({ kind: "response", id: data.id, value });
      }
      schedule();
    } catch (error) {
      if (
        "id" in data &&
        typeof data.id === "number" &&
        !["start", "cancel", "hostResult"].includes(data.kind)
      )
        send({ kind: "response", id: data.id, error: serializeError(error) });
      else failWorker(error);
    }
  });
}
