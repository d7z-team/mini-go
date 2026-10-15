import { readFile } from "node:fs/promises";
import "../../.build/compiler/wasm_exec";
import { instantiateGoCompiler, runCompilerWorker } from "./worker";
import type { CompilerCommand, CompilerResponse } from "./protocol";
import { nodeWorkerPort } from "../platform/port.node";

runCompilerWorker(
  nodeWorkerPort<CompilerCommand, CompilerResponse>("compiler worker"),
  async (override) => {
    if (!override) throw new Error("compiler WASM URL was not provided");
    const url = new URL(override);
    if (url.protocol === "file:")
      return instantiateGoCompiler(Uint8Array.from(await readFile(url)));
    const response = await fetch(url);
    if (!response.ok) throw new Error(`compiler WASM: HTTP ${response.status}`);
    return instantiateGoCompiler(new Uint8Array(await response.arrayBuffer()));
  },
);
