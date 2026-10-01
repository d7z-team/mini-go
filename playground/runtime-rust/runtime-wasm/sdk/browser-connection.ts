import type { Request, Response, WorkerConnection } from "./protocol.js";

/** Adapt browser events for runtime and RPC owners without owning their protocol. */
export function createBrowserConnection<Input = Request, Output = Response>(
  url: string | URL,
  name: string,
): WorkerConnection<Input, Output> {
  const worker = new Worker(url, { type: "module" });
  return {
    send: (message, transfer = []) => worker.postMessage(message, transfer),
    listen(message, failure) {
      worker.onmessage = (event: MessageEvent<Output>) => message(event.data);
      worker.onerror = (event) => failure(event.error ?? event.message);
      worker.onmessageerror = () => failure(new Error(`${name} message could not be decoded`));
    },
    terminate: () => worker.terminate(),
  };
}
