import { browserWorkerPort } from "../platform/browser.js";
import type { RuntimeRequest, RuntimeResponse } from "../runtime/protocol.js";
import { runRuntimeWorker } from "../runtime/worker.js";

runRuntimeWorker(browserWorkerPort<RuntimeRequest, RuntimeResponse>());
