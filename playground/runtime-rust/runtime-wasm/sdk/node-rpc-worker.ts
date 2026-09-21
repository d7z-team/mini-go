import { readFile } from "node:fs/promises";
import { parentPort } from "node:worker_threads";
import init from "./wasm/mini_go_wasm.js";
import * as wasm from "./wasm/mini_go_wasm.js";
import { runRPCWorker, type WasmRPCDriver } from "./rpc-worker.js";

if (!parentPort) throw new Error("RPC worker requires a parent message port");
const port = parentPort;
runRPCWorker(
  {
    send: (message) => port.postMessage(message),
    listen: (handler) => port.on("message", handler),
  },
  async (override) => {
    const url = override
      ? new URL(override, import.meta.url)
      : new URL("./wasm/mini_go_wasm_bg.wasm", import.meta.url);
    return {
      output: await init({ module_or_path: url.protocol === "file:" ? await readFile(url) : url }),
      create: (options) =>
        new (wasm as unknown as { WasmRpc: new (options: unknown) => WasmRPCDriver }).WasmRpc(
          options,
        ),
    };
  },
  (callback) => setImmediate(callback),
);
