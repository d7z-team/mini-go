import { RPCConnectionOwner } from "./rpc-runtime.js";
import { createBrowserConnection } from "./browser-connection.js";
import type { RPCConnectOptions, RPCConnection } from "./rpc-types.js";

export * from "./rpc-types.js";

export const RPC = {
  connect(url: string | URL, options: RPCConnectOptions = {}): Promise<RPCConnection> {
    return RPCConnectionOwner.connect(url, options, (configuration) =>
      createBrowserConnection(
        configuration.workerUrl || new URL("./browser-rpc-worker.js", import.meta.url),
        "RPC worker",
      ),
    );
  },
};
