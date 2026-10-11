import { LanguageService } from "./compiler/language-service.js";
import type { CompilerOptions, CompilerWorkerFactory } from "./compiler/language-service.js";
import { createRuntime } from "./index.browser.js";
import { createBrowserWorker } from "./platform/browser.js";
import { DebugSession } from "./runtime/debug.js";
import type { DebugOptions } from "./runtime/debug.js";
import type { RuntimeClient } from "./runtime/client.js";

export { ToolsError } from "./compiler/language-service.js";
export type * from "./compiler/language-service.js";
export type * from "./runtime/debug.js";

const createCompilerWorker: CompilerWorkerFactory = (options) =>
  createBrowserWorker(
    options.workerUrl || new URL("./workers/compiler.browser.js", import.meta.url),
    "compiler worker",
  );

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
