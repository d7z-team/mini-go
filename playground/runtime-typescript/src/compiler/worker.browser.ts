import "../../.build/compiler/wasm_exec";
import { instantiateGoCompiler, runCompilerWorker } from "./worker";
import { browserWorkerPort } from "../platform/port.browser";
import type { CompilerCommand, CompilerResponse } from "./protocol";

runCompilerWorker(browserWorkerPort<CompilerCommand, CompilerResponse>(), async (override) => {
  if (!override) throw new Error("compiler WASM URL was not provided");
  const response = await fetch(override);
  if (!response.ok) throw new Error(`compiler WASM: HTTP ${response.status}`);
  return instantiateGoCompiler(new Uint8Array(await response.arrayBuffer()));
});
