import runtimeWorkerURL from "../runtime/worker.browser?worker&url";
import compilerWorkerURL from "../compiler/worker.browser?worker&url";
import rpcWorkerURL from "../rpc/worker.browser?worker&url";
import compilerWasmURL from "../../.build/compiler/mini-go-compiler.wasm?url&no-inline";
import type { CompilerWorkerFactory } from "../compiler/language-service";
import type { RuntimeWorkerFactory } from "../runtime/protocol";
import type { RPCConnectOptions } from "../rpc/types";
import type { RPCWorkerFactory } from "../rpc/protocol";
import type { WorkerConnection } from "../worker-channel";

/** Adapt browser Worker objects to the shared connection protocol. */
export function createBrowserWorker<Input, Output>(
  url: string | URL,
  name: string,
): WorkerConnection<Input, Output> {
  const worker = new Worker(url, { type: "module" });
  return {
    send: (message, transfer = []) => worker.postMessage(message, transfer),
    listen(message, failure) {
      worker.onmessage = (event: MessageEvent<Output>) => message(event.data);
      worker.onerror = (event) => failure(event.error ?? event.message);
      worker.onmessageerror = () => failure(new Error(`${name} message could not be decoded`));
    },
    terminate: () => worker.terminate(),
  };
}

export const defaultCompilerURL = compilerWasmURL;

export const createRuntimeWorker: RuntimeWorkerFactory = (options) =>
  createBrowserWorker(options.workerUrl || runtimeWorkerURL, "runtime worker");

export const createCompilerWorker: CompilerWorkerFactory = (options) =>
  createBrowserWorker(options.workerUrl || compilerWorkerURL, "compiler worker");

export const createRPCWorker: RPCWorkerFactory = (configuration: RPCConnectOptions) =>
  createBrowserWorker(configuration.workerUrl || rpcWorkerURL, "RPC worker");
