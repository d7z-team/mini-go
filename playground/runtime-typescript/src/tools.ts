import { createCompilerWorker, defaultCompilerURL } from "#platform";
import { LanguageService } from "./compiler/language-service";
import type { CompilerOptions } from "./compiler/language-service";
import { createRuntime } from "./index";
import { DebugSession } from "./runtime/debug";
import type { DebugOptions } from "./runtime/debug";
import type { RuntimeClient } from "./runtime/client";

export { ToolsError } from "./compiler/language-service";
export type * from "./compiler/language-service";
export type * from "./runtime/debug";

export function createLanguageService(options: CompilerOptions = {}): Promise<LanguageService> {
  return LanguageService.create(createCompilerWorker, {
    ...options,
    compilerUrl: options.compilerUrl ?? defaultCompilerURL,
  });
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
