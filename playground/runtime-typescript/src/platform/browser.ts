import type { WorkerConnection, WorkerPort } from "../worker-channel.js";

/** Adapt browser events without owning the caller's protocol. */
export function createBrowserWorker<Input, Output>(
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

export function browserWorkerPort<Input, Output>(): WorkerPort<Input, Output> {
  return {
    send: (message) => globalThis.postMessage(message),
    listen: (handler) => {
      globalThis.onmessage = (event: MessageEvent<Input>) => handler(event.data);
    },
  };
}
