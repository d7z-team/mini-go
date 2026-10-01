import { Worker } from "node:worker_threads";
import type { Request, Response, WorkerConnection } from "./protocol.js";

/** Adapt Node events; an owner-initiated termination is not a worker failure. */
export function createNodeConnection<Input = Request, Output = Response>(
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
