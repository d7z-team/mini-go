import { createNodeConnection } from "./node-connection.js";
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
      return createNodeConnection(workerURL, "RPC worker");
    });
  },
};
