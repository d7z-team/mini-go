import { createBrowserWorker } from "./platform/browser.js";
import { RPCClient } from "./rpc/client.js";
import type { RPCConnectOptions, RPCConnection } from "./rpc/types.js";

export * from "./rpc/types.js";

export function connectRPC(
  url: string | URL,
  options: RPCConnectOptions = {},
): Promise<RPCConnection> {
  return RPCClient.connect(url, options, (configuration) =>
    createBrowserWorker(
      configuration.workerUrl || new URL("./workers/rpc.browser.js", import.meta.url),
      "RPC worker",
    ),
  );
}
