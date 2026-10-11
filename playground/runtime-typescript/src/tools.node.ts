import { LanguageService } from "./compiler/language-service.js";
import type { CompilerOptions, CompilerWorkerFactory } from "./compiler/language-service.js";
import { createRuntime } from "./index.node.js";
import { createNodeWorker } from "./platform/node.js";
import { DebugSession } from "./runtime/debug.js";
import type { DebugOptions } from "./runtime/debug.js";
import type { RuntimeClient } from "./runtime/client.js";

export { ToolsError } from "./compiler/language-service.js";
export type * from "./compiler/language-service.js";
export type * from "./runtime/debug.js";

const createCompilerWorker: CompilerWorkerFactory = (options) => {
  const url = options.workerUrl
    ? options.workerUrl instanceof URL
      ? options.workerUrl
      : new URL(options.workerUrl, import.meta.url)
    : new URL("./workers/compiler.node.js", import.meta.url);
  return createNodeWorker(url, "compiler worker");
};

export function createLanguageService(options: CompilerOptions = {}): Promise<LanguageService> {
  return LanguageService.create(createCompilerWorker, options);
}

export function createDebugSession(
  image: Uint8Array | ArrayBuffer,
  options: DebugOptions = {},
): Promise<DebugSession> {
  return DebugSession.create(createRuntime, image, options);
}

export function attachDebugSession(
  runtime: RuntimeClient,
  options: Pick<DebugOptions, "sources" | "entry"> = {},
): Promise<DebugSession> {
  return DebugSession.attach(runtime, options);
}
