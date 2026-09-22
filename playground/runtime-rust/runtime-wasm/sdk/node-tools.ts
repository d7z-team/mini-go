import { MiniGo, createWorker } from "./node.js";
import { LanguageService, type CompilerOptions } from "./tools.js";
import { DebugSession, type DebugOptions } from "./debug.js";

export * from "./tools.js";
export * from "./debug.js";

export function createDebugSession(
  image: Uint8Array | ArrayBuffer,
  options: DebugOptions = {},
): Promise<DebugSession> {
  return DebugSession.create(MiniGo.create, image, options);
}
export async function createLanguageService(
  image?: Uint8Array | ArrayBuffer,
  options: CompilerOptions = {},
): Promise<LanguageService> {
  if (!image) {
    const { readFile } = await import("node:fs/promises");
    image = await readFile(new URL("./tools/compiler.json.gz", import.meta.url), {
      signal: options.signal,
    });
  }
  return LanguageService.create(createWorker, image, options);
}
