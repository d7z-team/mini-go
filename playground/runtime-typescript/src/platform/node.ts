import { parentPort, Worker } from "node:worker_threads";
import type { WorkerConnection, WorkerPort } from "../worker-channel.js";

/** Adapt Node events; an owner-initiated termination is not a worker failure. */
export function createNodeWorker<Input, Output>(
  url: URL,
  name: string,
): WorkerConnection<Input, Output> {
  const worker = new Worker(url, { execArgv: [] });
  let terminated = false;
  return {
    send: (message, transfer = []) => worker.postMessage(message, transfer),
    listen(message, failure) {
      worker.on("message", message);
      worker.on("messageerror", failure);
      worker.on("error", failure);
      worker.on("exit", (code) => {
        if (!terminated) failure(new Error(`${name} exited (${code})`));
      });
    },
    terminate() {
      terminated = true;
      void worker.terminate();
    },
  };
}

export function nodeWorkerPort<Input, Output>(name: string): WorkerPort<Input, Output> {
  if (!parentPort) throw new Error(`${name} requires a parent message port`);
  const port = parentPort;
  return {
    send: (message) => port.postMessage(message),
    listen: (handler) => port.on("message", handler),
  };
}
