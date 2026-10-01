import { Runtime } from "./runtime.js";
import { createBrowserConnection } from "./browser-connection.js";
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

export const createWorker: WorkerFactory = (options) =>
  createBrowserConnection(
    options.workerUrl || new URL("./browser-worker.js", import.meta.url),
    "worker",
  );
