import { createNodeWorker } from "./platform/node.js";
import { RPCClient } from "./rpc/client.js";
import type { RPCConnectOptions, RPCConnection } from "./rpc/types.js";

export * from "./rpc/types.js";

export function connectRPC(
  url: string | URL,
  options: RPCConnectOptions = {},
): Promise<RPCConnection> {
  return RPCClient.connect(url, options, (configuration) => {
    const workerURL = configuration.workerUrl
      ? configuration.workerUrl instanceof URL
        ? configuration.workerUrl
        : new URL(configuration.workerUrl, import.meta.url)
      : new URL("./workers/rpc.node.js", import.meta.url);
    return createNodeWorker(workerURL, "RPC worker");
  });
}
