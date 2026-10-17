import { parentPort } from "node:worker_threads";
import type { WorkerPort } from "../worker-channel";

export function nodeWorkerPort<Input, Output>(name: string): WorkerPort<Input, Output> {
  if (!parentPort) throw new Error(`${name} requires a parent message port`);
  const port = parentPort;
  return {
    send: (message, transfer = []) => port.postMessage(message, transfer),
    listen: (handler) => port.on("message", handler),
  };
}
