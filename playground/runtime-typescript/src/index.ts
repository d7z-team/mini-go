import { createRuntimeWorker } from "#platform";
import { RuntimeClient, UNLIMITED_STEPS, values } from "./runtime/client";
import type { Options } from "./runtime/types";

export { UNLIMITED_STEPS, values };
export type { RuntimeClient as Runtime } from "./runtime/client";
export type * from "./runtime/types";

export function createRuntime(
  image: Uint8Array | ArrayBuffer,
  options: Options = {},
): Promise<RuntimeClient> {
  return RuntimeClient.create(image, options, createRuntimeWorker);
}
