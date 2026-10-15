import { Worker } from "node:worker_threads";
import runtimeWorkerURL from "../runtime/worker.node?worker&url";
import compilerWorkerURL from "../compiler/worker.node?worker&url";
import rpcWorkerURL from "../rpc/worker.node?worker&url";
import compilerWasmURL from "../../.build/compiler/mini-go-compiler.wasm?url&no-inline";
import type { CompilerWorkerFactory } from "../compiler/language-service";
import type { RuntimeWorkerFactory } from "../runtime/protocol";
import type { RPCConnectOptions } from "../rpc/types";
import type { RPCWorkerFactory } from "../rpc/protocol";
import type { WorkerConnection } from "../worker-channel";

/** Adapt Node worker_threads events; owner termination is not a worker failure. */
export function createNodeWorker<Input, Output>(
  url: string | URL,
  name: string,
): WorkerConnection<Input, Output> {
  const worker = new Worker(url, { execArgv: [] });
  let terminated = false;
  return {
    send: (message, transfer = []) => worker.postMessage(message, transfer),
    listen(message, failure) {
      worker.on("message", message);
      worker.on("messageerror", failure);
      worker.on("error", failure);
      worker.on("exit", (code) => {
        if (!terminated) failure(new Error(`${name} exited (${code})`));
      });
    },
    terminate() {
      terminated = true;
      void worker.terminate();
    },
  };
}

function resolveWorkerURL(value: string | URL | undefined, fallback: string): URL {
  return value === undefined
    ? new URL(fallback, import.meta.url)
    : value instanceof URL
      ? value
      : new URL(value, import.meta.url);
}

export const defaultCompilerURL = compilerWasmURL;

export const createRuntimeWorker: RuntimeWorkerFactory = (options) =>
  createNodeWorker(resolveWorkerURL(options.workerUrl, runtimeWorkerURL), "runtime worker");

export const createCompilerWorker: CompilerWorkerFactory = (options) =>
  createNodeWorker(resolveWorkerURL(options.workerUrl, compilerWorkerURL), "compiler worker");

export const createRPCWorker: RPCWorkerFactory = (configuration: RPCConnectOptions) =>
  createNodeWorker(resolveWorkerURL(configuration.workerUrl, rpcWorkerURL), "RPC worker");
