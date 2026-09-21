import {
  copyBytes,
  toError,
  type Control,
  type ControlResult,
  type Response,
  type WorkerConnection,
  type WorkerFactory,
} from "./protocol.js";
import { deferred, type Deferred } from "./deferred.js";
import type {
  Bindings,
  Execution,
  Frame,
  FrameRef,
  HostValue,
  Options,
  Snapshot,
  Stats,
} from "./types.js";

// Limit serialization work before handing arbitrary values to a worker or WASM.
function checkInput(value: unknown, maxBytes = 8 * 1024 * 1024): void {
  const pending: [unknown, number][] = [[value, 0]];
  let bytes = 0,
    nodes = 0;
  while (pending.length) {
    const [item, depth] = pending.pop()!;
    if (++nodes > 100_000 || depth > 64) throw new Error("host value structure exceeds limits");
    if (typeof item === "string") bytes += item.length * 2;
    else if (ArrayBuffer.isView(item) || item instanceof ArrayBuffer) bytes += item.byteLength;
    else if (item && typeof item === "object") {
      for (const child of Object.values(item)) pending.push([child, depth + 1]);
    }
    if (bytes > maxBytes || pending.length > 100_000)
      throw new Error("host value bytes exceed limits");
  }
}

export const values = {
  int(value: bigint | number): HostValue {
    if (typeof value === "number" && !Number.isSafeInteger(value))
      throw new Error("integer input requires an exact number or BigInt");
    const integer = BigInt(value);
    if (integer < -(1n << 63n) || integer >= 1n << 63n)
      throw new Error("integer input exceeds signed 64-bit range");
    return { typ: { Primitive: 3 }, data: { Integer: integer } };
  },
  bool(value: boolean): HostValue {
    return { typ: { Primitive: 1 }, data: { Bool: value } };
  },
  string(value: string): HostValue {
    return { typ: { Primitive: 2 }, data: { String: new TextEncoder().encode(value) } };
  },
  bytes(value: Uint8Array): HostValue {
    return { typ: { Slice: { Primitive: 9 } }, data: { String: Uint8Array.from(value) } };
  },
};

export const UNLIMITED_STEPS = -1;

/** A worker owns one instance. Explicit close waits for asynchronous cleanup. */
export class Runtime {
  private readonly ready = deferred<void>();
  private readonly closed = deferred<void>();
  private readonly closeWaiters = new Set<(error?: Error) => void>();
  private next = 0;
  private readonly calls = new Map<
    number,
    { result: Deferred<Snapshot>; settled: Deferred<void>; unsubscribe(): void }
  >();
  private readonly commands = new Map<number, Deferred<ControlResult>>();
  private readonly hostCalls = new Map<number, AbortController>();
  private closing = false;
  private terminated = false;

  static async create(
    image: Uint8Array | ArrayBuffer,
    options: Options,
    factory: WorkerFactory,
  ): Promise<Runtime> {
    const { signal, provider, workerUrl: _, providerModule, wasmUrl, ...rest } = options;
    if (signal?.aborted) throw toError(signal.reason ?? "initialization canceled");
    if (options.maxSteps !== undefined) {
      const steps = options.maxSteps;
      if (
        (typeof steps === "number" && !Number.isSafeInteger(steps)) ||
        steps < -1 ||
        steps > 9223372036854775807n
      )
        throw new Error("maxSteps requires -1, 0, or a positive signed 64-bit integer");
    }
    const maxImageBytes = (options.workload === "compiler" ? 64 : 32) * 1024 * 1024;
    if (image.byteLength > maxImageBytes) throw new Error("image exceeds WASM limit");
    const configuration = {
      ...rest,
      providerModule: providerModule?.toString(),
      wasmUrl: wasmUrl?.toString(),
    };
    checkInput(configuration);
    const vm = new Runtime(
      factory(options),
      provider,
      options.workload === "compiler" ? 64 * 1024 * 1024 : 8 * 1024 * 1024,
    );
    const abort = () => {
      vm.ready.reject(toError(signal?.reason ?? "initialization canceled"));
      void vm.close();
    };
    signal?.addEventListener("abort", abort, { once: true });
    try {
      const bytes = copyBytes(image);
      vm.worker.send({ kind: "create", image: bytes, options: configuration }, [bytes.buffer]);
      if (signal?.aborted) abort();
      await vm.ready.promise;
      return vm;
    } catch (reason) {
      if (!vm.closing) vm.terminate(reason);
      throw reason;
    } finally {
      signal?.removeEventListener("abort", abort);
    }
  }

  private constructor(
    private readonly worker: WorkerConnection,
    private readonly provider: Options["provider"],
    private readonly maxInputBytes: number,
  ) {
    worker.listen(
      (message) => this.message(message),
      (reason) => this.terminate(reason),
    );
  }

  private message(message: Response): void {
    switch (message.kind) {
      case "ready":
        this.ready.resolve();
        break;
      case "fatal":
        this.terminate(message.error);
        break;
      case "result": {
        const call = this.calls.get(message.id);
        if (call)
          message.error
            ? call.result.reject(toError(message.error))
            : call.result.resolve(message.value!);
        break;
      }
      case "settled": {
        const call = this.calls.get(message.id);
        if (call) {
          message.error ? call.settled.reject(toError(message.error)) : call.settled.resolve();
          call.unsubscribe();
          this.calls.delete(message.id);
        }
        break;
      }
      case "response": {
        const command = this.commands.get(message.id);
        if (command) {
          this.commands.delete(message.id);
          message.error ? command.reject(toError(message.error)) : command.resolve(message.value);
        }
        break;
      }
      case "host":
        void this.host(message).catch((reason) => this.terminate(reason));
        break;
      case "hostCancel":
        this.hostCalls.get(message.id)?.abort();
        break;
      case "closed":
        this.dispose();
        break;
    }
  }

  private async host({ id, route, payload }: Extract<Response, { kind: "host" }>): Promise<void> {
    const controller = new AbortController();
    this.hostCalls.set(id, controller);
    try {
      if (!this.provider) throw new Error(`host route unavailable: ${route}`);
      const bytes = await this.provider({ route, payload, signal: controller.signal });
      if (!(bytes instanceof Uint8Array)) throw new Error("provider must return Uint8Array");
      if (bytes.byteLength > 4 * 1024 * 1024) throw new Error("host result exceeds limit");
      if (!this.terminated) this.worker.send({ kind: "hostResult", id, payload: bytes });
    } catch (reason) {
      if (!this.terminated)
        this.worker.send({
          kind: "hostResult",
          id,
          error: String(reason),
          payload: new Uint8Array(),
        });
    } finally {
      this.hostCalls.delete(id);
    }
  }

  start(
    entry: string,
    arguments_: HostValue[] = [],
    { signal, timeoutMs }: { signal?: AbortSignal; timeoutMs?: number } = {},
  ): Execution {
    if (this.closing || this.terminated) throw new Error("instance is closing");
    if (this.calls.size >= 128) throw new Error("call queue is full");
    checkInput(arguments_, this.maxInputBytes);
    if (
      timeoutMs !== undefined &&
      (!Number.isFinite(timeoutMs) || timeoutMs < 0 || timeoutMs > Number.MAX_SAFE_INTEGER)
    )
      throw new Error("invalid call timeout");
    const id = ++this.next,
      result = deferred<Snapshot>(),
      settled = deferred<void>();
    const started = performance.now();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let canceled = false;
    const unsubscribe = () => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", cancel);
    };
    const cancel = () => {
      unsubscribe();
      if (!canceled && !this.terminated && this.calls.has(id)) {
        canceled = true;
        this.worker.send({ kind: "cancel", id });
      }
    };
    const scheduleDeadline = () => {
      if (timeoutMs === undefined || canceled || this.terminated || !this.calls.has(id)) return;
      const remaining = timeoutMs - (performance.now() - started);
      if (remaining <= 0) {
        cancel();
      } else {
        timer = setTimeout(scheduleDeadline, Math.min(2_147_483_647, remaining));
      }
    };
    this.calls.set(id, { result, settled, unsubscribe });
    try {
      this.worker.send({ kind: "start", id, entry, arguments: arguments_ });
    } catch (reason) {
      unsubscribe();
      this.calls.delete(id);
      throw reason;
    }
    signal?.addEventListener("abort", cancel, { once: true });
    if (signal?.aborted) cancel();
    // Even a zero deadline requests cancellation asynchronously after start.
    if (timeoutMs !== undefined && !canceled && !this.terminated && this.calls.has(id))
      timer = setTimeout(
        scheduleDeadline,
        Math.min(2_147_483_647, Math.max(0, timeoutMs - (performance.now() - started))),
      );
    return { result: result.promise, settled: settled.promise, cancel };
  }

  private command<T extends ControlResult>(command: Control): Promise<T> {
    if (this.closing || this.terminated) return Promise.reject(new Error("instance is closing"));
    if (this.commands.size >= 128) return Promise.reject(new Error("control queue is full"));
    try {
      checkInput(command);
    } catch (reason) {
      return Promise.reject(reason);
    }
    const id = ++this.next,
      pending = deferred<ControlResult>();
    this.commands.set(id, pending);
    try {
      this.worker.send({ ...command, id });
    } catch (reason) {
      this.commands.delete(id);
      pending.reject(reason);
    }
    return pending.promise as Promise<T>;
  }
  pause(): Promise<void> {
    return this.command({ kind: "pause" });
  }
  resume(): Promise<void> {
    return this.command({ kind: "resume" });
  }
  patch(image: Uint8Array): Promise<void> {
    return this.command({ kind: "patch", image });
  }
  stack(): Promise<Frame[]> {
    return this.command({ kind: "stack" });
  }
  bindings(frame: FrameRef): Promise<Bindings> {
    return this.command({ kind: "bindings", frame });
  }
  debugOpen(
    sources: Record<string, { module: string; path: string; text: string }>,
    entry = "default",
  ): Promise<void> {
    return this.command({ kind: "debugOpen", sources, entry });
  }
  debugRequest(
    command: string,
    arguments_: Record<string, unknown> = {},
  ): Promise<Record<string, unknown>> {
    return this.command({ kind: "debugRequest", command, arguments: arguments_ });
  }
  debugEvents(): Promise<Record<string, unknown>[]> {
    return this.command({ kind: "debugEvents" });
  }
  debugOutput(category: string, text: string): Promise<void> {
    return this.command({ kind: "debugOutput", category, text });
  }
  breakpoints(module: string, file: string, lines: (number | bigint)[]): Promise<bigint[]> {
    return this.command({ kind: "breakpoints", module, file, lines });
  }
  stats(): Promise<Stats> {
    return this.command({ kind: "stats" });
  }

  close({ signal }: { signal?: AbortSignal } = {}): Promise<void> {
    if (!this.closing && !this.terminated) {
      this.closing = true;
      try {
        this.worker.send({ kind: "close" });
      } catch (reason) {
        this.terminate(reason);
      }
    }
    if (!signal || this.terminated) return this.closed.promise;
    if (signal.aborted) return Promise.reject(toError(signal.reason ?? "wait canceled"));
    // Detach canceled waits even if the host never completes cleanup.
    return new Promise((resolve, reject) => {
      const finish = (error?: Error) => {
        this.closeWaiters.delete(finish);
        signal.removeEventListener("abort", abort);
        error ? reject(error) : resolve();
      };
      const abort = () => finish(toError(signal.reason ?? "wait canceled"));
      this.closeWaiters.add(finish);
      signal.addEventListener("abort", abort, { once: true });
    });
  }
  terminate(reason: unknown = new Error("worker terminated")): void {
    const failure = toError(reason);
    this.ready.reject(failure);
    this.dispose(failure);
  }
  private dispose(closeError?: Error): void {
    if (this.terminated) return;
    this.terminated = true;
    closeError ? this.closed.reject(closeError) : this.closed.resolve();
    for (const waiter of this.closeWaiters) waiter(closeError);
    this.closeWaiters.clear();
    this.worker.terminate();
    const reason = closeError ?? new Error("instance closed");
    for (const call of this.calls.values()) {
      call.unsubscribe();
      call.result.reject(reason);
      call.settled.reject(reason);
    }
    for (const command of this.commands.values()) command.reject(reason);
    for (const controller of this.hostCalls.values()) controller.abort(reason);
    this.calls.clear();
    this.commands.clear();
    this.hostCalls.clear();
  }
}
