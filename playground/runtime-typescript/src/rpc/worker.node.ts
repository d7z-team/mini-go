import { nodeWorkerPort } from "../platform/port.node";
import type { RPCRequest, RPCResponse } from "./protocol";
import { runRPCWorker } from "./worker";

runRPCWorker(nodeWorkerPort<RPCRequest, RPCResponse>("RPC worker"), (callback) =>
  setImmediate(callback),
);
