import { createRPCWorker } from "#platform";
import { RPCClient } from "./client";
import type { RPCConnectOptions, RPCConnection } from "./types";

export * from "./types";

export function connectRPC(
  url: string | URL,
  options: RPCConnectOptions = {},
): Promise<RPCConnection> {
  return RPCClient.connect(url, options, createRPCWorker);
}
