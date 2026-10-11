import { readFile } from "node:fs/promises";
import "../compiler/wasm_exec.js";
import { instantiateGoCompiler, runCompilerWorker } from "../compiler/worker.js";
import type { CompilerCommand, CompilerResponse } from "../compiler/protocol.js";
import { nodeWorkerPort } from "../platform/node.js";

runCompilerWorker(
  nodeWorkerPort<CompilerCommand, CompilerResponse>("compiler worker"),
  async (override) => {
    const url = override
      ? new URL(override)
      : new URL("../compiler/mini-go-compiler.wasm", import.meta.url);
    if (url.protocol === "file:")
      return instantiateGoCompiler(Uint8Array.from(await readFile(url)));
    const response = await fetch(url);
    if (!response.ok) throw new Error(`compiler WASM: HTTP ${response.status}`);
    return instantiateGoCompiler(new Uint8Array(await response.arrayBuffer()));
  },
);
