import { nodeWorkerPort } from "../platform/port.node";
import type { RuntimeRequest, RuntimeResponse } from "./protocol";
import { runRuntimeWorker } from "./worker";

runRuntimeWorker(nodeWorkerPort<RuntimeRequest, RuntimeResponse>("runtime worker"), (callback) =>
  setImmediate(callback),
);
