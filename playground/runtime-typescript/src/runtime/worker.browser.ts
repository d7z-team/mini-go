import { browserWorkerPort } from "../platform/port.browser";
import type { RuntimeRequest, RuntimeResponse } from "./protocol";
import { runRuntimeWorker } from "./worker";

runRuntimeWorker(browserWorkerPort<RuntimeRequest, RuntimeResponse>());
