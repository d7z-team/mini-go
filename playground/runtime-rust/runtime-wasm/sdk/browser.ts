import { Runtime } from "./runtime.js";
import type { WorkerFactory } from "./protocol.js";
import type { Options } from "./types.js";

export { RPC } from "./browser-rpc.js";
export type * from "./rpc-types.js";
export { values, UNLIMITED_STEPS } from "./runtime.js";
export type * from "./types.js";
export type MiniGo = Runtime;
export const MiniGo = {
  create(image: Uint8Array | ArrayBuffer, options: Options = {}): Promise<MiniGo> {
    return Runtime.create(image, options, createWorker);
  },
};

export const createWorker: WorkerFactory = (options) => {
  const worker = options.workerUrl
    ? new Worker(options.workerUrl, { type: "module" })
    : new Worker(new URL("./browser-worker.js", import.meta.url), { type: "module" });
  return {
    send: (message, transfer = []) => worker.postMessage(message, transfer),
    listen(message, failure) {
      worker.onmessage = (event) => message(event.data);
      worker.onerror = (event) => failure(event.error ?? event.message);
      worker.onmessageerror = () => failure(new Error("worker message could not be decoded"));
    },
    terminate: () => worker.terminate(),
  };
};
