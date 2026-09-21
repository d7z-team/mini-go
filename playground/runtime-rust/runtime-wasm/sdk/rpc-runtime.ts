import type {
  RPCBindOptions,
  RPCBinding,
  RPCCallOptions,
  RPCConnectOptions,
  RPCConnection,
  RPCContract,
  RPCMethod,
  RPCProvider,
  RPCProvidedResource,
  RPCPublication,
  RPCPublishOptions,
  RPCResource,
  RPCServerContext,
  RPCStats,
  RPCValue,
} from "./rpc-types.js";
import { RPCStatus } from "./rpc-types.js";
import type {
  RPCFailure,
  RPCResponse,
  RPCWorkerConnection,
  RPCWorkerFactory,
  RPCWorkerStats,
} from "./rpc-protocol.js";
import { deferred, type Deferred } from "./deferred.js";

const maximumIdentity = 0xffff_ffff;
const maximumDurationMs = 9_223_372_036_854;

function failure(reason: unknown, fallback = "unknown"): RPCFailure {
  if (reason instanceof RPCStatus) return { code: reason.code, message: reason.message };
  if (reason instanceof Error) return { code: fallback, message: reason.message };
  return { code: fallback, message: String(reason) };
}

function validateDuration(value: number | undefined, name: string): void {
  if (
    value !== undefined &&
    (!Number.isSafeInteger(value) || value <= 0 || value > maximumDurationMs)
  )
    throw new RPCStatus("invalid_argument", `${name} must be a positive integer`);
}

function canceled(signal: AbortSignal): RPCStatus {
  const reason = signal.reason;
  return new RPCStatus(
    "canceled",
    reason instanceof Error ? reason.message : String(reason ?? "RPC operation canceled"),
  );
}

async function waitFor<T>(promise: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (!signal) return promise;
  if (signal.aborted) throw canceled(signal);
  return new Promise<T>((resolve, reject) => {
    const abort = () => reject(canceled(signal));
    signal.addEventListener("abort", abort, { once: true });
    promise.then(
      (value) => {
        signal.removeEventListener("abort", abort);
        resolve(value);
      },
      (reason) => {
        signal.removeEventListener("abort", abort);
        reject(reason);
      },
    );
  });
}

interface ProviderResourceEntry {
  provided: RPCProvidedResource;
  closing?: Promise<void>;
}

export class RPCConnectionOwner implements RPCConnection {
  private readonly ready = deferred<void>();
  private readonly closed = deferred<void>();
  private readonly commands = new Map<number, Deferred<unknown>>();
  private readonly providers = new Map<number, RPCProvider>();
  private readonly providerResources = new Map<number, ProviderResourceEntry>();
  private readonly providerCalls = new Map<number, AbortController>();
  private readonly providerDeliveries = new Map<number, number[]>();
  private readonly clientResources = new Map<number, ClientResource>();
  private nextIdentity = 0;
  private closing = false;
  private terminated = false;
  private closeStarted?: Promise<void>;

  static async connect(
    url: string | URL,
    options: RPCConnectOptions,
    factory: RPCWorkerFactory,
  ): Promise<RPCConnectionOwner> {
    const address = String(url);
    if (!address) throw new RPCStatus("invalid_argument", "RPC URL is required");
    validateDuration(options.leaseTtlMs, "leaseTtlMs");
    validateDuration(options.admissionTimeoutMs, "admissionTimeoutMs");
    validateDuration(options.maxCallDurationMs, "maxCallDurationMs");
    if (options.signal?.aborted) throw canceled(options.signal);
    const connection = new RPCConnectionOwner(factory(options));
    const abort = () => {
      connection.ready.reject(canceled(options.signal!));
      connection.terminate(options.signal!.reason);
    };
    options.signal?.addEventListener("abort", abort, { once: true });
    try {
      connection.worker.send({
        kind: "create",
        url: address,
        options: {
          leaseTtlMs: options.leaseTtlMs,
          admissionTimeoutMs: options.admissionTimeoutMs,
          maxCallDurationMs: options.maxCallDurationMs,
          wasmUrl: options.wasmUrl?.toString(),
        },
      });
      await connection.ready.promise;
      return connection;
    } catch (reason) {
      connection.terminate(reason);
      throw reason;
    } finally {
      options.signal?.removeEventListener("abort", abort);
    }
  }

  private constructor(private readonly worker: RPCWorkerConnection) {
    worker.listen(
      (message) => this.handleWorkerMessage(message),
      (reason) => this.terminate(reason),
    );
  }

  private allocateIdentity(): number {
    if (this.nextIdentity >= maximumIdentity)
      throw new RPCStatus("resource_exhausted", "RPC JavaScript identity exhausted");
    return ++this.nextIdentity;
  }

  private handleWorkerMessage(message: RPCResponse): void {
    switch (message.kind) {
      case "ready":
        this.ready.resolve();
        break;
      case "closed":
        void this.dispose();
        break;
      case "fatal":
        this.terminate(message.error);
        break;
      case "response": {
        const command = this.commands.get(message.id);
        if (command) {
          this.commands.delete(message.id);
          message.error
            ? command.reject(new RPCStatus(message.error.code ?? "unknown", message.error.message))
            : command.resolve(message.value);
        }
        break;
      }
      case "providerCall":
        void this.handleProviderCall(message).catch((reason) => this.terminate(reason));
        break;
      case "providerCancel":
        this.providerCalls
          .get(message.id)
          ?.abort(new RPCStatus("canceled", "remote RPC caller canceled"));
        break;
      case "providerSettled":
        void this.settleProviderResources(message.id, message.retained).catch((reason) =>
          this.terminate(reason),
        );
        break;
      case "resourceClose":
        void this.handleProviderResourceClose(message.id, message.resource).catch((reason) =>
          this.terminate(reason),
        );
        break;
    }
  }

  private requestWorker<T>(
    message: Parameters<RPCWorkerConnection["send"]>[0] & { id: number },
    signal?: AbortSignal,
    cancelWorker = false,
  ): Promise<T> {
    if (this.closing || this.terminated)
      return Promise.reject(new RPCStatus("unavailable", "RPC connection is closing"));
    if (signal?.aborted) return Promise.reject(canceled(signal));
    const pending = deferred<unknown>();
    this.commands.set(message.id, pending);
    const abort = () => {
      const command = this.commands.get(message.id);
      if (!command) return;
      this.commands.delete(message.id);
      command.reject(canceled(signal!));
      if (cancelWorker) {
        try {
          this.worker.send({ kind: "cancel", id: message.id });
        } catch (reason) {
          this.terminate(reason);
        }
      }
    };
    signal?.addEventListener("abort", abort, { once: true });
    try {
      this.worker.send(message);
    } catch (reason) {
      this.commands.delete(message.id);
      signal?.removeEventListener("abort", abort);
      pending.reject(reason);
    }
    return pending.promise.finally(() => signal?.removeEventListener("abort", abort)) as Promise<T>;
  }

  async bind(contract: RPCContract, options: RPCBindOptions = {}): Promise<RPCBinding> {
    validateDuration(options.timeoutMs, "timeoutMs");
    const id = this.allocateIdentity();
    const binding = await this.requestWorker<number>(
      {
        kind: "bind",
        id,
        contract,
        options: {
          affinityKey: options.affinityKey ?? "",
          labels: { ...options.labels },
          timeoutMs: options.timeoutMs,
        },
      },
      options.signal,
      true,
    );
    return new ClientBinding(this, binding);
  }

  async publish(provider: RPCProvider, options: RPCPublishOptions = {}): Promise<RPCPublication> {
    if (!provider || !provider.contract || typeof provider.invoke !== "function")
      throw new RPCStatus("invalid_argument", "generated RPC provider is required");
    if (options.priority !== undefined && !Number.isSafeInteger(options.priority))
      throw new RPCStatus("invalid_argument", "priority must be an integer");
    for (const [name, value] of [
      ["weight", options.weight],
      ["maxLeases", options.maxLeases],
    ] as const)
      if (value !== undefined && (!Number.isSafeInteger(value) || value < 0))
        throw new RPCStatus("invalid_argument", `${name} must be a non-negative integer`);
    if (options.maxLeases !== undefined && options.maxLeases > maximumIdentity)
      throw new RPCStatus("invalid_argument", "maxLeases exceeds the WASM capacity limit");
    const providerID = this.allocateIdentity();
    this.providers.set(providerID, provider);
    try {
      const id = this.allocateIdentity();
      const publication = await this.requestWorker<number>({
        kind: "publish",
        id,
        provider: providerID,
        contract: provider.contract,
        options,
      });
      return new ClientPublication(this, publication, providerID);
    } catch (reason) {
      this.providers.delete(providerID);
      throw reason;
    }
  }

  async invoke<T>(
    binding: ClientBinding,
    receiver: ClientResource | undefined,
    method: RPCMethod,
    arguments_: readonly RPCValue[],
    decode: (values: readonly RPCValue[]) => T,
    options: RPCCallOptions = {},
  ): Promise<T> {
    validateDuration(options.timeoutMs, "timeoutMs");
    const id = this.allocateIdentity();
    const pending = await this.requestWorker<{ result: number; values: readonly RPCValue[] }>(
      {
        kind: "invoke",
        id,
        binding: binding.id,
        method,
        receiver: receiver?.id,
        arguments: arguments_,
        timeoutMs: options.timeoutMs,
      },
      options.signal,
      true,
    );
    const resourceIDs: number[] = [];
    try {
      if (!Array.isArray(pending.values))
        throw new RPCStatus("protocol", "RPC result is not a value list");
      for (const value of pending.values) {
        if (!value || typeof value !== "object" || !value.data)
          throw new RPCStatus("protocol", "RPC result contains an invalid value");
        if (value.data.kind === "resource") resourceIDs.push(value.data.value);
      }
      if (options.signal?.aborted) throw canceled(options.signal);
      const value = decode(pending.values);
      await this.requestWorker<void>({
        kind: "accept",
        id: this.allocateIdentity(),
        result: pending.result,
      });
      return value;
    } catch (reason) {
      await this.requestWorker<void>({
        kind: "discard",
        id: this.allocateIdentity(),
        result: pending.result,
      }).catch(() => {});
      for (const resource of resourceIDs) this.invalidateResource(resource);
      throw reason;
    }
  }

  adoptResource(binding: ClientBinding, value: RPCValue, typeHash: string): RPCResource | null {
    if (value.type !== typeHash)
      throw new RPCStatus("protocol", "RPC resource type does not match its contract");
    if (value.data.kind === "nil") return null;
    if (value.data.kind !== "resource")
      throw new RPCStatus("protocol", "RPC method returned an invalid resource");
    const existing = this.clientResources.get(value.data.value);
    if (existing) {
      if (existing.binding !== binding || existing.typeHash !== typeHash)
        throw new RPCStatus("protocol", "RPC resource identity conflicts with its binding");
      return existing;
    }
    const resource = new ClientResource(this, binding, value.data.value, typeHash);
    this.clientResources.set(resource.id, resource);
    return resource;
  }

  invalidateResource(id: number): void {
    const resource = this.clientResources.get(id);
    if (resource) resource.invalidate();
    this.clientResources.delete(id);
  }

  async closeBinding(id: number): Promise<void> {
    await this.requestWorker<void>({
      kind: "closeBinding",
      id: this.allocateIdentity(),
      binding: id,
    });
  }

  async closeResource(resource: ClientResource, options: RPCCallOptions): Promise<void> {
    validateDuration(options.timeoutMs, "timeoutMs");
    const id = this.allocateIdentity();
    await this.requestWorker<void>(
      { kind: "closeResource", id, resource: resource.id, timeoutMs: options.timeoutMs },
      options.signal,
      true,
    );
    this.invalidateResource(resource.id);
  }

  async closePublication(id: number, provider: number): Promise<void> {
    await this.requestWorker<void>({
      kind: "closePublication",
      id: this.allocateIdentity(),
      publication: id,
    });
    this.providers.delete(provider);
  }

  private hydrateProviderArguments(arguments_: readonly RPCValue[]): RPCValue[] {
    return arguments_.map((value) => {
      if (value.data.kind !== "localResource" || typeof value.data.value !== "number") return value;
      const resource = this.providerResources.get(value.data.value);
      if (!resource) throw new RPCStatus("not_found", "JavaScript provider resource is closed");
      if (resource.provided.descriptor.typeHash !== value.type)
        throw new RPCStatus("protocol", "JavaScript provider resource type does not match");
      return { type: value.type, data: { kind: "localResource", value: resource.provided } };
    });
  }

  private materializeProviderResults(values: readonly RPCValue[]): {
    values: RPCValue[];
    resources: number[];
  } {
    const resources: Array<{ id: number; provided: RPCProvidedResource }> = [];
    const encoded = values.map((value) => {
      if (value.data.kind === "resource")
        throw new RPCStatus("internal", "RPC provider returned a remote resource handle");
      if (value.data.kind !== "localResource") return value;
      if (typeof value.data.value === "number")
        throw new RPCStatus("internal", "RPC provider returned a forged local resource handle");
      const entry = { id: this.allocateIdentity(), provided: value.data.value };
      resources.push(entry);
      return { type: value.type, data: { kind: "localResource", value: entry.id } } as RPCValue;
    });
    for (const resource of resources)
      this.providerResources.set(resource.id, { provided: resource.provided });
    return { values: encoded, resources: resources.map((resource) => resource.id) };
  }

  private closeProviderResource(resource: ProviderResourceEntry): Promise<void> {
    resource.closing ??= Promise.resolve().then(() =>
      resource.provided.descriptor.close(resource.provided.handler),
    );
    return resource.closing;
  }

  private async releaseProviderResource(id: number): Promise<void> {
    const resource = this.providerResources.get(id);
    if (!resource) return;
    const closing = this.closeProviderResource(resource);
    await closing;
    if (this.providerResources.get(id) === resource) this.providerResources.delete(id);
  }

  private async releaseProviderResources(ids: readonly number[]): Promise<void> {
    await Promise.all(ids.map((id) => this.releaseProviderResource(id)));
  }

  private async releaseProviderValues(values: readonly RPCValue[]): Promise<void> {
    await Promise.all(
      values.map(async (value) => {
        if (value.data.kind !== "localResource" || typeof value.data.value === "number") return;
        const provided = value.data.value;
        await Promise.resolve().then(() => provided.descriptor.close(provided.handler));
      }),
    );
  }

  private async settleProviderResources(call: number, retained: readonly number[]): Promise<void> {
    const resources = this.providerDeliveries.get(call);
    this.providerDeliveries.delete(call);
    if (!Array.isArray(retained))
      throw new RPCStatus("protocol", "RPC provider settlement is not a resource list");
    if (!resources) {
      if (retained.length)
        throw new RPCStatus("protocol", "RPC provider retained an unknown resource");
      return;
    }
    const delivered = new Set(resources);
    const owned = new Set<number>();
    for (const id of retained) {
      if (!Number.isSafeInteger(id) || id <= 0 || !delivered.has(id) || owned.has(id))
        throw new RPCStatus("protocol", "RPC provider retained an invalid resource");
      owned.add(id);
    }
    await this.releaseProviderResources(resources.filter((id) => !owned.has(id)));
  }

  private async handleProviderCall(
    message: Extract<RPCResponse, { kind: "providerCall" }>,
  ): Promise<void> {
    const controller = new AbortController();
    this.providerCalls.set(message.id, controller);
    let result: readonly RPCValue[] | undefined;
    let deliveredResources: number[] | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    if (message.timeoutMs !== undefined) {
      const deadline = performance.now() + Math.max(0, message.timeoutMs);
      const arm = () => {
        const remaining = deadline - performance.now();
        if (remaining <= 0) {
          controller.abort(new RPCStatus("deadline_exceeded", "RPC deadline exceeded"));
          return;
        }
        timer = setTimeout(arm, Math.min(2_147_483_647, remaining));
      };
      arm();
    }
    const context: RPCServerContext = {
      signal: controller.signal,
      peer: message.peer,
      provider: message.providerName,
    };
    try {
      const arguments_ = this.hydrateProviderArguments(message.arguments);
      if (message.resource !== undefined) {
        const resource = this.providerResources.get(message.resource);
        if (!resource) throw new RPCStatus("not_found", "JavaScript provider resource is closed");
        result = await resource.provided.descriptor.invoke(
          resource.provided.handler,
          context,
          message.method,
          arguments_,
        );
      } else {
        const provider = this.providers.get(message.provider);
        if (!provider) throw new RPCStatus("unavailable", "JavaScript provider is closed");
        result = await provider.invoke(context, message.method, arguments_);
      }
      if (!Array.isArray(result))
        throw new RPCStatus("internal", "RPC provider returned an invalid result list");
      if (controller.signal.aborted || this.closing || this.terminated) {
        const abandoned = result;
        result = undefined;
        await this.releaseProviderValues(abandoned);
        throw canceled(controller.signal);
      }
      const delivered = this.materializeProviderResults(result);
      deliveredResources = delivered.resources;
      if (delivered.resources.length) this.providerDeliveries.set(message.id, delivered.resources);
      this.worker.send({
        kind: "providerResult",
        call: message.id,
        values: delivered.values,
      });
    } catch (reason) {
      if (deliveredResources) {
        this.providerDeliveries.delete(message.id);
        await this.releaseProviderResources(deliveredResources).catch(() => {});
      } else if (result) {
        await this.releaseProviderValues(result).catch(() => {});
      }
      if (!this.terminated)
        this.worker.send({
          kind: "providerResult",
          call: message.id,
          values: [],
          error: failure(reason, controller.signal.aborted ? "canceled" : "internal"),
        });
    } finally {
      clearTimeout(timer);
      this.providerCalls.delete(message.id);
    }
  }

  private async handleProviderResourceClose(close: number, resourceID: number): Promise<void> {
    if (!this.providerResources.has(resourceID)) {
      this.worker.send({ kind: "resourceClosed", close });
      return;
    }
    try {
      await this.releaseProviderResource(resourceID);
      this.worker.send({ kind: "resourceClosed", close });
    } catch (reason) {
      this.worker.send({ kind: "resourceClosed", close, error: failure(reason, "internal") });
    }
  }

  async stats(): Promise<RPCStats> {
    const stats = await this.requestWorker<RPCWorkerStats>({
      kind: "stats",
      id: this.allocateIdentity(),
    });
    return stats as RPCStats;
  }

  close(options: { signal?: AbortSignal } = {}): Promise<void> {
    if (!this.closeStarted) {
      this.closing = true;
      this.closeStarted = this.closed.promise;
      try {
        this.worker.send({ kind: "close" });
      } catch (reason) {
        this.terminate(reason);
      }
    }
    return waitFor(this.closeStarted, options.signal);
  }

  terminate(reason: unknown = new RPCStatus("unavailable", "RPC worker terminated")): void {
    if (this.terminated) return;
    this.terminated = true;
    this.closing = true;
    this.worker.terminate();
    const status = RPCStatus.from(reason, "unavailable");
    const providerResources = this.releaseOwnedState(status);
    this.closed.reject(status);
    for (const resource of providerResources) {
      void this.closeProviderResource(resource).catch(() => {});
    }
  }

  private async dispose(): Promise<void> {
    if (this.terminated) return;
    this.terminated = true;
    this.closing = true;
    const status = new RPCStatus("unavailable", "RPC connection closed");
    const providerResources = this.releaseOwnedState(status);
    this.worker.terminate();
    const cleanup = await Promise.allSettled(
      providerResources.map((resource) => this.closeProviderResource(resource)),
    );
    const cleanupFailure = cleanup.find(
      (result): result is PromiseRejectedResult => result.status === "rejected",
    );
    cleanupFailure
      ? this.closed.reject(RPCStatus.from(cleanupFailure.reason, "internal"))
      : this.closed.resolve();
  }

  private releaseOwnedState(status: RPCStatus): ProviderResourceEntry[] {
    this.ready.reject(status);
    for (const command of this.commands.values()) command.reject(status);
    this.commands.clear();
    for (const controller of this.providerCalls.values()) controller.abort(status);
    this.providerCalls.clear();
    for (const resource of this.clientResources.values()) resource.invalidate();
    this.clientResources.clear();
    this.providers.clear();
    this.providerDeliveries.clear();
    const providerResources = [...this.providerResources.values()];
    this.providerResources.clear();
    return providerResources;
  }
}

class ClientBinding implements RPCBinding {
  private closed = false;
  private closing?: Promise<void>;

  constructor(
    private readonly connection: RPCConnectionOwner,
    readonly id: number,
  ) {}

  invoke<T>(
    method: RPCMethod,
    arguments_: readonly RPCValue[],
    decode: (values: readonly RPCValue[]) => T,
    options?: RPCCallOptions,
  ): Promise<T> {
    if (this.closed) return Promise.reject(new RPCStatus("unavailable", "RPC binding is closed"));
    return this.connection.invoke(this, undefined, method, arguments_, decode, options);
  }

  invokeResource<T>(
    resource: ClientResource,
    method: RPCMethod,
    arguments_: readonly RPCValue[],
    decode: (values: readonly RPCValue[]) => T,
    options?: RPCCallOptions,
  ): Promise<T> {
    if (resource.invalid)
      return Promise.reject(new RPCStatus("not_found", "RPC resource is closed"));
    return this.connection.invoke(this, resource, method, arguments_, decode, options);
  }

  resource(value: RPCValue, typeHash: string): RPCResource | null {
    return this.connection.adoptResource(this, value, typeHash);
  }

  close(): Promise<void> {
    if (!this.closing) {
      this.closed = true;
      this.closing = this.connection.closeBinding(this.id);
    }
    return this.closing;
  }
}

class ClientResource implements RPCResource {
  invalid = false;
  private closing?: Promise<void>;

  constructor(
    private readonly connection: RPCConnectionOwner,
    readonly binding: ClientBinding,
    readonly id: number,
    readonly typeHash: string,
  ) {}

  invoke<T>(
    method: RPCMethod,
    arguments_: readonly RPCValue[],
    decode: (values: readonly RPCValue[]) => T,
    options?: RPCCallOptions,
  ): Promise<T> {
    if (this.invalid) return Promise.reject(new RPCStatus("not_found", "RPC resource is closed"));
    return this.binding.invokeResource(this, method, arguments_, decode, options);
  }

  value(): RPCValue {
    if (this.invalid) throw new RPCStatus("not_found", "RPC resource is closed");
    return { type: this.typeHash, data: { kind: "resource", value: this.id } };
  }

  close(options: RPCCallOptions = {}): Promise<void> {
    if (this.invalid) return Promise.resolve();
    this.closing ??= this.connection.closeResource(this, options).catch((reason) => {
      this.closing = undefined;
      throw reason;
    });
    return this.closing;
  }

  invalidate(): void {
    this.invalid = true;
  }
}

class ClientPublication implements RPCPublication {
  private closing?: Promise<void>;

  constructor(
    private readonly connection: RPCConnectionOwner,
    private readonly id: number,
    private readonly provider: number,
  ) {}

  close(options: { signal?: AbortSignal } = {}): Promise<void> {
    this.closing ??= this.connection.closePublication(this.id, this.provider).catch((reason) => {
      this.closing = undefined;
      throw reason;
    });
    return waitFor(this.closing, options.signal);
  }
}
