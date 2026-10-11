import { nodeWorkerPort } from "../platform/node.js";
import type { RPCRequest, RPCResponse } from "../rpc/protocol.js";
import { runRPCWorker } from "../rpc/worker.js";

runRPCWorker(nodeWorkerPort<RPCRequest, RPCResponse>("RPC worker"), (callback) =>
  setImmediate(callback),
);
