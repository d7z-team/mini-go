import { RPCEndpoint } from "./endpoint.js";
import {
  attachRPCSocket,
  connectRPCSocket,
  flushRPCSocket,
  type RPCTransportOwner,
} from "./transport.js";
import type { RPCFailure, RPCRequest, RPCWorkerPort } from "./protocol.js";

function failure(reason: unknown): RPCFailure {
  if (reason && typeof reason === "object") {
    const value = reason as { code?: unknown; message?: unknown };
    const result: RPCFailure = { message: String(value.message ?? reason) };
    if (typeof value.code === "string") result.code = value.code;
    return result;
  }
  return { message: String(reason) };
}

/** Runs one pure TypeScript endpoint in a platform Worker. */
export function runRPCWorker(
  port: RPCWorkerPort,
  enqueue: (callback: () => void) => void = (callback) => setTimeout(callback, 0),
): void {
  let rpc: RPCEndpoint | undefined;
  let socket: WebSocket | undefined;
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

  function drive(): void {
    if (!rpc || closed) return;
    try {
      for (const action of rpc.actions()) send(action);
      clearTimeout(flushTimer);
      flushTimer = undefined;
      if (flushRPCSocket(socket, rpc as RPCTransportOwner, outgoing))
        flushTimer = setTimeout(schedule, 10);
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

  function requireRPC(): RPCEndpoint {
    if (!rpc || closing) throw new Error("RPC connection is unavailable");
    return rpc;
  }

  async function create(data: Extract<RPCRequest, { kind: "create" }>): Promise<void> {
    if (rpc || closing) throw new Error("worker already owns an RPC connection");
    const connected = await connectRPCSocket(data.url);
    if (closing) {
      connected.close();
      return;
    }
    socket = connected;
    rpc = new RPCEndpoint(data.options, schedule, fail);
    attachRPCSocket(socket, rpc, schedule, fail, () =>
      fail(new Error("RPC WebSocket disconnected")),
    );
    schedule();
    await rpc.ready;
    if (!closed) send({ kind: "ready" });
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
      if (data.kind === "create") void create(data).catch(fail);
      else if (data.kind === "close") void shutdown();
      else if (data.kind === "cancel") rpc?.cancel(data.id);
      else if (data.kind === "providerResult") {
        const settled = rpc
          ? rpc.providerComplete(data.call, data.values, data.error)
          : Promise.resolve([]);
        void settled
          .then((retained) => send({ kind: "providerSettled", id: data.call, retained }))
          .finally(schedule)
          .catch(fail);
      } else if (data.kind === "resourceClosed") rpc?.resourceComplete(data.close, data.error);
      else {
        const owner = requireRPC();
        switch (data.kind) {
          case "bind":
            respond(data.id, () => owner.bind(data.id, data.contract, data.options));
            break;
          case "invoke":
            respond(data.id, () =>
              owner.invoke(
                data.id,
                data.binding,
                data.method,
                data.receiver,
                data.arguments,
                data.timeoutMs,
              ),
            );
            break;
          case "accept":
            respond(data.id, () => owner.accept(data.result));
            break;
          case "discard":
            respond(data.id, () => owner.discard(data.result));
            break;
          case "closeBinding":
            respond(data.id, () => owner.closeBinding(data.binding));
            break;
          case "closeResource":
            respond(data.id, () => owner.closeResource(data.id, data.resource, data.timeoutMs));
            break;
          case "publish":
            respond(data.id, () => owner.publish(data.provider, data.contract, data.options));
            break;
          case "closePublication":
            respond(data.id, () => owner.closePublication(data.publication));
            break;
          case "stats":
            respond(data.id, () => owner.stats());
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
