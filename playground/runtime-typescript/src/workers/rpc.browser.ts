import { browserWorkerPort } from "../platform/browser.js";
import type { RPCRequest, RPCResponse } from "../rpc/protocol.js";
import { runRPCWorker } from "../rpc/worker.js";

runRPCWorker(browserWorkerPort<RPCRequest, RPCResponse>());
