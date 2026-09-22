import { MiniGo, createWorker } from "./browser.js";
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
    const response = await fetch(new URL("./tools/compiler.json.gz", import.meta.url), {
      signal: options.signal,
    });
    if (!response.ok) throw new Error(`compiler image: HTTP ${response.status}`);
    image = await response.arrayBuffer();
  }
  return LanguageService.create(createWorker, image, options);
}
