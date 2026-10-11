import type { HostReply, RPCOptions } from "./types.js";
import {
  RPCEndpoint,
  decodeRPCFfiRequest,
  decodeRPCWireValues,
  encodeRPCFfiResponse,
  encodeRPCWireValues,
  type RPCEndpointAction,
  type RPCFfiResponse,
  type RPCResourceReference,
  type RPCWireValue,
} from "../rpc/endpoint.js";
import { attachRPCSocket, connectRPCSocket, flushRPCSocket } from "../rpc/transport.js";
import type { RPCMethod, RPCValue } from "../rpc/types.js";

const FFI_PROTOCOL = "minigo.mrpc.ffi/v3";

interface ClientLease {
  kind: "client";
  binding: number;
}

interface ProviderEvent {
  response: RPCFfiResponse;
  consumed?(): void;
}

interface ProviderLease {
  kind: "provider";
  provider: number;
  publication: number;
  contract: { methods: readonly RPCMethod[] };
  events: ProviderEvent[];
  waiters: Set<() => void>;
  resources: Map<number, RPCResourceReference>;
}

type Lease = ClientLease | ProviderLease;

interface OwnedResult {
  lease: number;
  endpointResult: number;
}

function errorStatus(reason: unknown): { code: string; message: string } {
  if (reason && typeof reason === "object") {
    const value = reason as { code?: unknown; message?: unknown };
    return {
      code: typeof value.code === "string" ? value.code : "unknown",
      message: String(value.message ?? reason),
    };
  }
  return { code: "unknown", message: String(reason) };
}

function hostReply(
  payload: Uint8Array,
  consumed?: () => void | Promise<void>,
  discard?: () => void | Promise<void>,
): HostReply {
  const reply: HostReply = { payload };
  if (consumed) reply.consumed = consumed;
  if (discard) reply.discard = discard;
  return reply;
}

/** Adapts the VM's binary MRPC FFI session to the shared TypeScript endpoint. */
export class RuntimeRPCBridge {
  private readonly endpoint: RPCEndpoint;
  private readonly outgoing: { id: number; payload: number[] }[] = [];
  private readonly leases = new Map<number, Lease>();
  private readonly results = new Map<number, OwnedResult>();
  private nextIdentity = 1;
  private closed = false;

  static async connect(
    url: string,
    options: RPCOptions,
    schedule: (delay?: number) => void,
    fatal: (reason: unknown) => void,
  ): Promise<RuntimeRPCBridge> {
    const socket = await connectRPCSocket(url);
    const endpoint = new RPCEndpoint(options, schedule, fatal);
    const rpc = new RuntimeRPCBridge(socket, endpoint, schedule);
    attachRPCSocket(socket, endpoint, schedule, fatal, () =>
      fatal(new Error("RPC WebSocket disconnected")),
    );
    rpc.pump();
    await endpoint.ready;
    return rpc;
  }

  private constructor(
    private readonly socket: WebSocket,
    endpoint: RPCEndpoint,
    private readonly schedule: (delay?: number) => void,
  ) {
    this.endpoint = endpoint;
  }

  pump(): void {
    if (this.closed) return;
    for (const action of this.endpoint.actions()) this.handleEndpointAction(action);
    if (flushRPCSocket(this.socket, this.endpoint, this.outgoing)) this.schedule(10);
  }

  async call(payload: Uint8Array, signal: AbortSignal): Promise<HostReply> {
    const request = decodeRPCFfiRequest(payload);
    if (request.version !== FFI_PROTOCOL || request.timeoutNanos < 0n)
      throw new Error("invalid MRPC FFI version or timeout");
    try {
      return await this.handle(request, signal);
    } catch (reason) {
      const failure = errorStatus(reason);
      return hostReply(encodeRPCFfiResponse({ code: failure.code, message: failure.message }));
    } finally {
      this.schedule();
    }
  }

  async close(): Promise<void> {
    if (this.closed) return;
    const cleanups: Promise<unknown>[] = [];
    for (const [id, lease] of this.leases) {
      if (lease.kind === "client") cleanups.push(this.endpoint.closeBinding(lease.binding));
      else cleanups.push(this.endpoint.closePublication(lease.publication));
      this.leases.delete(id);
    }
    for (const result of this.results.values())
      cleanups.push(this.endpoint.discard(result.endpointResult));
    this.results.clear();
    const settled = await Promise.allSettled(cleanups);
    await this.endpoint.shutdown();
    this.pump();
    this.closed = true;
    this.socket.close();
    const failure = settled.find(
      (result): result is PromiseRejectedResult => result.status === "rejected",
    );
    if (failure) throw failure.reason;
  }

  private allocateIdentity(): number {
    if (this.nextIdentity >= Number.MAX_SAFE_INTEGER)
      throw Object.assign(new Error("RPC FFI identity exhausted"), { code: "resource_exhausted" });
    return this.nextIdentity++;
  }

  private timeoutMilliseconds(timeout: bigint): number | undefined {
    if (timeout === 0n) return;
    return Math.max(1, Math.ceil(Number(timeout) / 1_000_000));
  }

  private async handle(
    request: ReturnType<typeof decodeRPCFfiRequest>,
    signal: AbortSignal,
  ): Promise<HostReply> {
    const timeoutMs = this.timeoutMilliseconds(request.timeoutNanos);
    switch (request.operation) {
      case "open": {
        const operation = this.allocateIdentity();
        const abort = () => this.endpoint.cancel(operation);
        signal.addEventListener("abort", abort, { once: true });
        try {
          const options = {
            affinityKey: request.affinityKey,
            labels: request.labels,
          };
          if (timeoutMs !== undefined) Object.assign(options, { timeoutMs });
          const binding = await this.endpoint.bind(operation, request.contract, options);
          const lease = this.allocateIdentity();
          this.leases.set(lease, { kind: "client", binding });
          return hostReply(encodeRPCFfiResponse({ lease }), undefined, () =>
            this.closeLease(lease),
          );
        } finally {
          signal.removeEventListener("abort", abort);
        }
      }
      case "call":
      case "call_owned": {
        const lease = this.clientLease(request.lease);
        const operation = this.allocateIdentity();
        const receiver = request.receiver
          ? this.endpoint.resolveClientReference(lease.binding, request.receiver)
          : undefined;
        const arguments_ = this.endpoint.decodeClientValues(lease.binding, request.payload);
        const abort = () => this.endpoint.cancel(operation);
        signal.addEventListener("abort", abort, { once: true });
        try {
          const result = await this.endpoint.invoke(
            operation,
            lease.binding,
            request.method,
            receiver,
            arguments_,
            timeoutMs,
          );
          const response: RPCFfiResponse = {
            payload: this.endpoint.encodeClientValues(lease.binding, result.values),
          };
          if (request.operation === "call_owned") {
            const id = this.allocateIdentity();
            this.results.set(id, { lease: request.lease, endpointResult: result.result });
            response.requestId = id;
            return hostReply(encodeRPCFfiResponse(response), undefined, () =>
              this.discardResult(id),
            );
          }
          let decided = false;
          const decide = (accepted: boolean) => {
            if (decided) return;
            decided = true;
            return accepted
              ? this.endpoint.accept(result.result)
              : this.endpoint.discard(result.result);
          };
          return hostReply(
            encodeRPCFfiResponse(response),
            () => decide(true),
            () => decide(false),
          );
        } finally {
          signal.removeEventListener("abort", abort);
        }
      }
      case "accept_result":
      case "discard_result": {
        const result = this.results.get(request.requestId);
        if (!result || result.lease !== request.lease)
          throw Object.assign(new Error("RPC result is unavailable"), { code: "not_found" });
        this.results.delete(request.requestId);
        if (request.operation === "accept_result")
          await this.endpoint.accept(result.endpointResult);
        else await this.endpoint.discard(result.endpointResult);
        return hostReply(encodeRPCFfiResponse({}));
      }
      case "release": {
        const lease = this.clientLease(request.lease);
        if (!request.resource)
          throw Object.assign(new Error("release requires a resource"), {
            code: "invalid_argument",
          });
        const resource = this.endpoint.resolveClientReference(lease.binding, request.resource);
        const operation = this.allocateIdentity();
        const abort = () => this.endpoint.cancel(operation);
        signal.addEventListener("abort", abort, { once: true });
        try {
          await this.endpoint.closeResource(operation, resource, timeoutMs);
        } finally {
          signal.removeEventListener("abort", abort);
        }
        return hostReply(encodeRPCFfiResponse({}));
      }
      case "close":
        await this.closeLease(request.lease);
        return hostReply(encodeRPCFfiResponse({}));
      case "open_provider": {
        const provider = this.allocateIdentity();
        const publication = this.endpoint.publish(provider, request.contract, {
          name: request.target,
        });
        const lease = this.allocateIdentity();
        this.leases.set(lease, {
          kind: "provider",
          provider,
          publication,
          contract: request.contract,
          events: [],
          waiters: new Set(),
          resources: new Map(),
        });
        return hostReply(encodeRPCFfiResponse({ lease }), undefined, () => this.closeLease(lease));
      }
      case "accept": {
        const lease = this.providerLease(request.lease);
        const event = await this.nextProviderEvent(lease, signal);
        let completed = false;
        const consume = () => {
          if (completed) return;
          completed = true;
          event.consumed?.();
        };
        return hostReply(encodeRPCFfiResponse(event.response), consume, () => {
          if (completed) return;
          completed = true;
          lease.events.unshift(event);
          this.notifyProvider(lease);
        });
      }
      case "respond": {
        const lease = this.providerLease(request.lease);
        const values = request.code
          ? []
          : decodeRPCWireValues(request.payload).map((value) =>
              this.providerResultValue(lease, value),
            );
        const delivered = values.flatMap((value) =>
          value.data.kind === "localResource" && typeof value.data.value === "number"
            ? [value.data.value]
            : [],
        );
        void this.endpoint
          .providerComplete(
            request.requestId,
            values,
            request.code ? { code: request.code, message: request.message } : undefined,
          )
          .then((retained) => {
            const keep = new Set(retained);
            for (const resource of delivered)
              if (!keep.has(resource)) this.queueResourceRelease(lease, resource);
          })
          .catch(() => {});
        return hostReply(encodeRPCFfiResponse({}));
      }
      case "close_provider":
        await this.closeLease(request.lease);
        return hostReply(encodeRPCFfiResponse({}));
      default:
        throw Object.assign(new Error("unknown MRPC FFI operation"), {
          code: "invalid_argument",
        });
    }
  }

  private clientLease(id: number): ClientLease {
    const lease = this.leases.get(id);
    if (lease?.kind !== "client")
      throw Object.assign(new Error("RPC lease is closed or is not a client"), {
        code: "unavailable",
      });
    return lease;
  }

  private providerLease(id: number): ProviderLease {
    const lease = this.leases.get(id);
    if (lease?.kind !== "provider")
      throw Object.assign(new Error("RPC provider lease is closed"), { code: "unavailable" });
    return lease;
  }

  private async closeLease(id: number): Promise<void> {
    const lease = this.leases.get(id);
    if (!lease) return;
    this.leases.delete(id);
    for (const [result, entry] of [...this.results]) {
      if (entry.lease !== id) continue;
      this.results.delete(result);
      await this.endpoint.discard(entry.endpointResult).catch(() => {});
    }
    if (lease.kind === "client") {
      await this.endpoint.closeBinding(lease.binding);
    } else await this.endpoint.closePublication(lease.publication);
  }

  private async discardResult(id: number): Promise<void> {
    const result = this.results.get(id);
    if (!result) return;
    this.results.delete(id);
    await this.endpoint.discard(result.endpointResult);
  }

  private handleEndpointAction(action: RPCEndpointAction): void {
    if (action.kind === "providerCall") {
      const lease = [...this.leases.values()].find(
        (candidate): candidate is ProviderLease =>
          candidate.kind === "provider" && candidate.provider === action.provider,
      );
      if (!lease) {
        void this.endpoint.providerComplete(action.id, [], {
          code: "unavailable",
          message: "RPC guest provider is closed",
        });
        return;
      }
      try {
        const receiver =
          action.resource === undefined
            ? undefined
            : this.providerReference(lease, action.resource);
        const method = lease.contract.methods.find((candidate) =>
          receiver ? candidate.name === action.method : candidate.id === action.method,
        );
        if (!method) throw new Error(`unknown RPC provider method ${action.method}`);
        const values = action.arguments.map((value) => this.providerArgumentValue(lease, value));
        const response: RPCFfiResponse = {
          operation: "call",
          requestId: action.id,
          method,
          payload: encodeRPCWireValues(values),
        };
        if (receiver) response.receiver = receiver;
        this.queueProviderEvent(lease, { response });
      } catch (reason) {
        const failure = errorStatus(reason);
        void this.endpoint.providerComplete(action.id, [], failure);
      }
      return;
    }
    if (action.kind === "providerCancel") {
      for (const lease of this.leases.values())
        if (lease.kind === "provider")
          this.queueProviderEvent(lease, {
            response: { operation: "cancel", requestId: action.id },
          });
      return;
    }
    const lease = [...this.leases.values()].find(
      (candidate): candidate is ProviderLease =>
        candidate.kind === "provider" && candidate.resources.has(action.resource),
    );
    if (!lease) {
      this.endpoint.resourceComplete(action.id);
      return;
    }
    this.queueResourceRelease(lease, action.resource, () =>
      this.endpoint.resourceComplete(action.id),
    );
  }

  private providerArgumentValue(lease: ProviderLease, value: RPCValue): RPCWireValue {
    if (value.data.kind !== "localResource") return value as RPCWireValue;
    if (typeof value.data.value !== "number") throw new Error("invalid provider resource value");
    return {
      type: value.type,
      data: { kind: "wireResource", value: this.providerReference(lease, value.data.value) },
    };
  }

  private providerResultValue(lease: ProviderLease, value: RPCWireValue): RPCValue {
    if (value.data.kind !== "wireResource") return value as RPCValue;
    const resource = this.allocateIdentity();
    lease.resources.set(resource, value.data.value);
    return { type: value.type, data: { kind: "localResource", value: resource } };
  }

  private providerReference(lease: ProviderLease, resource: number): RPCResourceReference {
    const reference = lease.resources.get(resource);
    if (!reference)
      throw Object.assign(new Error("RPC guest resource is closed"), { code: "not_found" });
    return reference;
  }

  private queueResourceRelease(
    lease: ProviderLease,
    resource: number,
    completed?: () => void,
  ): void {
    const reference = lease.resources.get(resource);
    if (!reference) {
      completed?.();
      return;
    }
    this.queueProviderEvent(lease, {
      response: { operation: "release", receiver: reference },
      consumed: () => {
        lease.resources.delete(resource);
        completed?.();
      },
    });
  }

  private queueProviderEvent(lease: ProviderLease, event: ProviderEvent): void {
    lease.events.push(event);
    this.notifyProvider(lease);
  }

  private notifyProvider(lease: ProviderLease): void {
    for (const notify of lease.waiters) notify();
    lease.waiters.clear();
    this.schedule();
  }

  private async nextProviderEvent(
    lease: ProviderLease,
    signal: AbortSignal,
  ): Promise<ProviderEvent> {
    while (!lease.events.length) {
      if (signal.aborted) throw signal.reason;
      await new Promise<void>((resolve, reject) => {
        const ready = () => {
          signal.removeEventListener("abort", abort);
          resolve();
        };
        const abort = () => {
          lease.waiters.delete(ready);
          reject(signal.reason);
        };
        lease.waiters.add(ready);
        signal.addEventListener("abort", abort, { once: true });
      });
    }
    return lease.events.shift()!;
  }
}
