import init from "./wasm/mini_go_wasm.js";
import * as wasm from "./wasm/mini_go_wasm.js";
import { runRPCWorker, type WasmRPCDriver } from "./rpc-worker.js";
import type { RPCRequest } from "./rpc-protocol.js";

runRPCWorker(
  {
    send: (message) => globalThis.postMessage(message),
    listen: (handler) => {
      globalThis.onmessage = (event: MessageEvent<RPCRequest>) => handler(event.data);
    },
  },
  async (url) => ({
    output: await init({
      module_or_path: url ?? new URL("./wasm/mini_go_wasm_bg.wasm", import.meta.url),
    }),
    create: (options) =>
      new (wasm as unknown as { WasmRpc: new (options: unknown) => WasmRPCDriver }).WasmRpc(
        options,
      ),
  }),
);
