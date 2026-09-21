import { RPCConnectionOwner } from "./rpc-runtime.js";
import type { RPCConnectOptions, RPCConnection } from "./rpc-types.js";

export * from "./rpc-types.js";

export const RPC = {
  connect(url: string | URL, options: RPCConnectOptions = {}): Promise<RPCConnection> {
    return RPCConnectionOwner.connect(url, options, (configuration) => {
      const worker = configuration.workerUrl
        ? new Worker(configuration.workerUrl, { type: "module" })
        : new Worker(new URL("./browser-rpc-worker.js", import.meta.url), { type: "module" });
      return {
        send: (message) => worker.postMessage(message),
        listen(message, failure) {
          worker.onmessage = (event) => message(event.data);
          worker.onerror = (event) => failure(event.error ?? event.message);
          worker.onmessageerror = () =>
            failure(new Error("RPC worker message could not be decoded"));
        },
        terminate: () => worker.terminate(),
      };
    });
  },
};
