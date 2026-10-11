import "../compiler/wasm_exec.js";
import { instantiateGoCompiler, runCompilerWorker } from "../compiler/worker.js";
import { browserWorkerPort } from "../platform/browser.js";
import type { CompilerCommand, CompilerResponse } from "../compiler/protocol.js";

runCompilerWorker(browserWorkerPort<CompilerCommand, CompilerResponse>(), async (override) => {
  const response = await fetch(
    override ?? new URL("../compiler/mini-go-compiler.wasm", import.meta.url),
  );
  if (!response.ok) throw new Error(`compiler WASM: HTTP ${response.status}`);
  return instantiateGoCompiler(new Uint8Array(await response.arrayBuffer()));
});
