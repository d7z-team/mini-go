import { nodeWorkerPort } from "../platform/node.js";
import type { RuntimeRequest, RuntimeResponse } from "../runtime/protocol.js";
import { runRuntimeWorker } from "../runtime/worker.js";

runRuntimeWorker(nodeWorkerPort<RuntimeRequest, RuntimeResponse>("runtime worker"), (callback) =>
  setImmediate(callback),
);
