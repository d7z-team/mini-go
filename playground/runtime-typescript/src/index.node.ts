import { createNodeWorker } from "./platform/node.js";
import { RuntimeClient, UNLIMITED_STEPS, values } from "./runtime/client.js";
import type { RuntimeWorkerFactory } from "./runtime/protocol.js";
import type { Options } from "./runtime/types.js";

export { UNLIMITED_STEPS, values };
export type { RuntimeClient as Runtime } from "./runtime/client.js";
export type * from "./runtime/types.js";

const createRuntimeWorker: RuntimeWorkerFactory = (options) => {
  const url = options.workerUrl
    ? options.workerUrl instanceof URL
      ? options.workerUrl
      : new URL(options.workerUrl, import.meta.url)
    : new URL("./workers/runtime.node.js", import.meta.url);
  return createNodeWorker(url, "runtime worker");
};

export function createRuntime(
  image: Uint8Array | ArrayBuffer,
  options: Options = {},
): Promise<RuntimeClient> {
  return RuntimeClient.create(image, options, createRuntimeWorker);
}
