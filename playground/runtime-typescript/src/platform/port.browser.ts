import type { WorkerPort } from "../worker-channel";

/** Adapt the browser worker global to the shared worker protocol. */
export function browserWorkerPort<Input, Output>(): WorkerPort<Input, Output> {
  return {
    send: (message, transfer = []) => globalThis.postMessage(message, { transfer }),
    listen: (handler) => {
      globalThis.onmessage = (event: MessageEvent<Input>) => handler(event.data);
    },
  };
}
