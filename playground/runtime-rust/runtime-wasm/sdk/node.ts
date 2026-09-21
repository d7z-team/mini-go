import { Worker } from "node:worker_threads";
import { Runtime } from "./runtime.js";
import type { Options } from "./types.js";

export { RPC } from "./node-rpc.js";
export type * from "./rpc-types.js";
export { values, UNLIMITED_STEPS } from "./runtime.js";
export type * from "./types.js";
export type MiniGo = Runtime;
export const MiniGo = {
  create(image: Uint8Array | ArrayBuffer, options: Options = {}): Promise<MiniGo> {
    return Runtime.create(image, options, () => {
      const worker = new Worker(new URL("./node-worker.js", import.meta.url), { execArgv: [] });
      let terminated = false;
      return {
        send: (message, transfer = []) => worker.postMessage(message, transfer),
        listen(message, failure) {
          worker.on("message", message);
          worker.on("messageerror", failure);
          worker.on("error", failure);
          worker.on("exit", (code) => {
            if (!terminated) failure(new Error(`runtime worker exited (${code})`));
          });
        },
        terminate() {
          terminated = true;
          void worker.terminate();
        },
      };
    });
  },
};
