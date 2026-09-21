import { Worker } from "node:worker_threads";
import { RPCConnectionOwner } from "./rpc-runtime.js";
import type { RPCConnectOptions, RPCConnection } from "./rpc-types.js";

export * from "./rpc-types.js";

export const RPC = {
  connect(url: string | URL, options: RPCConnectOptions = {}): Promise<RPCConnection> {
    return RPCConnectionOwner.connect(url, options, (configuration) => {
      const workerURL = configuration.workerUrl
        ? configuration.workerUrl instanceof URL
          ? configuration.workerUrl
          : new URL(configuration.workerUrl, import.meta.url)
        : new URL("./node-rpc-worker.js", import.meta.url);
      const worker = new Worker(workerURL, { execArgv: [] });
      let terminated = false;
      return {
        send: (message) => worker.postMessage(message),
        listen(message, failure) {
          worker.on("message", message);
          worker.on("messageerror", failure);
          worker.on("error", failure);
          worker.on("exit", (code) => {
            if (!terminated) failure(new Error(`RPC worker exited (${code})`));
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
