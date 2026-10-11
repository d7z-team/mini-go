import { serializeError, type WorkerPort } from "../worker-channel.js";
import type { CompilerCommand, CompilerResponse } from "./protocol.js";

export interface GoCompiler {
  call(input: Uint8Array): Uint8Array;
  readonly wasmBytes: number;
}

export function runCompilerWorker(
  port: WorkerPort<CompilerCommand, CompilerResponse>,
  load: (url?: string) => Promise<GoCompiler>,
): void {
  let compiler: GoCompiler | undefined;
  let generation = 0n;
  let requests = 0n;
  let callMilliseconds = 0;
  let closed = false;

  port.listen(async (message) => {
    if (closed || (generation !== 0n && message.generation !== generation)) return;
    if (message.kind === "compilerCreate") {
      if (generation !== 0n) return;
      generation = message.generation;
      try {
        compiler = await load(message.compilerUrl);
        if (!closed) port.send({ kind: "compilerResponse", generation, id: 0 });
      } catch (error) {
        port.send({ kind: "compilerResponse", generation, id: 0, error: serializeError(error) });
      }
      return;
    }
    if (message.kind === "compilerClose") {
      closed = true;
      compiler = undefined;
      return;
    }
    if (!compiler) {
      port.send({
        kind: "compilerResponse",
        generation,
        id: message.id,
        error: serializeError(new Error("compiler worker unavailable")),
      });
      return;
    }
    if (message.kind === "compilerStats") {
      port.send({
        kind: "compilerResponse",
        generation,
        id: message.id,
        stats: { requests, callMilliseconds, wasmBytes: compiler.wasmBytes },
      });
      return;
    }
    if (message.kind !== "compilerRequest") return;
    const started = performance.now();
    try {
      const value = compiler.call(message.input);
      requests++;
      callMilliseconds += performance.now() - started;
      port.send({ kind: "compilerResponse", generation, id: message.id, value });
    } catch (error) {
      port.send({
        kind: "compilerResponse",
        generation,
        id: message.id,
        error: serializeError(error),
      });
    }
  });
}

interface GoRuntime {
  importObject: WebAssembly.Imports;
  run(instance: WebAssembly.Instance): Promise<void>;
}

type GoGlobal = typeof globalThis & {
  Go: new () => GoRuntime;
  __miniGoToolsCall?: (input: Uint8Array) => Uint8Array;
};

export async function instantiateGoCompiler(bytes: Uint8Array): Promise<GoCompiler> {
  const global = globalThis as GoGlobal;
  if (typeof global.Go !== "function") throw new Error("Go WebAssembly support is not loaded");
  const go = new global.Go();
  const instantiated = await WebAssembly.instantiate(
    Uint8Array.from(bytes).buffer,
    go.importObject,
  );
  const running = go.run(instantiated.instance);
  await Promise.resolve();
  if (!global.__miniGoToolsCall) {
    await Promise.race([
      running.then(() => {
        throw new Error("Go compiler exited during initialization");
      }),
      new Promise<void>((resolve, reject) => {
        let attempts = 0;
        const poll = () => {
          if (global.__miniGoToolsCall) resolve();
          else if (++attempts > 100)
            reject(new Error("Go compiler did not publish its entry point"));
          else setTimeout(poll, 0);
        };
        poll();
      }),
    ]);
  }
  const call = global.__miniGoToolsCall;
  if (!call) throw new Error("Go compiler entry point is unavailable");
  return {
    wasmBytes: bytes.byteLength,
    call(input) {
      const output = call(input);
      if (!(output instanceof Uint8Array)) throw new Error("Go compiler returned invalid bytes");
      return Uint8Array.from(output);
    },
  };
}
