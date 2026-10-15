import { browserWorkerPort } from "../platform/port.browser";
import type { RPCRequest, RPCResponse } from "./protocol";
import { runRPCWorker } from "./worker";

runRPCWorker(browserWorkerPort<RPCRequest, RPCResponse>());
