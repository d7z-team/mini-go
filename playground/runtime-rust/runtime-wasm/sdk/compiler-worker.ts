import { WasmCompiler, type InitOutput } from "./wasm/mini_go_wasm.js";
import { serializeError, type CompilerCommand, type WorkerPort } from "./protocol.js";

/** The Rust owner controls recovery and guest protocol; this adapter only pumps slices. */
export function compilerWorker(
  port: WorkerPort,
  load: (url?: string) => Promise<InitOutput>,
  enqueue?: (callback: () => void) => void,
): (message: CompilerCommand) => Promise<void> {
  let vm: WasmCompiler | undefined;
  let generation = 0n;
  let active: number | undefined;
  let awaiting = false;
  let closed = false;
  let memory: WebAssembly.Memory;
  let channel: MessageChannel | undefined;
  if (!enqueue) {
    channel = new MessageChannel();
    channel.port1.onmessage = () => drive();
    enqueue = () => channel!.port2.postMessage(null);
  }
  const schedule = enqueue;
  const fail = (id: number, error: unknown) => {
    active = undefined;
    const reusable = vm?.reusable() ?? false;
    if (!reusable) {
      vm?.free();
      vm = undefined;
    }
    port.send({ kind: "compilerResponse", generation, id, error: serializeError(error), reusable });
  };
  const drive = () => {
    if (!vm || active === undefined || awaiting || closed) return;
    try {
      const result = vm.poll(4096) as
        | "running"
        | "pending"
        | { value: string; restore: Uint8Array };
      if (typeof result === "object") {
        awaiting = true;
        port.send({ kind: "compilerResponse", generation, id: active, ...result });
      } else if (result === "pending") setTimeout(drive, 1);
      else schedule(drive);
    } catch (error) {
      fail(active, error);
    }
  };
  return async (message) => {
    if (closed) return;
    if (message.kind === "compilerCreate") {
      if (generation !== 0n) return;
      generation = message.generation;
      try {
        memory = (await load(message.wasmUrl)).memory;
        if (closed) return;
        vm = new WasmCompiler(message.image, generation, message.restore);
        port.send({ kind: "compilerResponse", generation, id: 0 });
      } catch (error) {
        fail(0, error);
      }
      return;
    }
    if (message.generation !== generation) return;
    if (message.kind === "compilerClose") {
      closed = true;
      channel?.port1.close();
      channel?.port2.close();
      vm?.free();
      vm = undefined;
      return;
    }
    try {
      if (!vm) throw new Error("compiler worker unavailable");
      switch (message.kind) {
        case "compilerRequest":
          if (active !== undefined) throw new Error("compiler response awaiting acknowledgment");
          active = message.id;
          vm.start(message.input, message.timeout);
          schedule(drive);
          break;
        case "compilerAck":
          if (active !== message.id || !awaiting) return;
          vm.acknowledge();
          active = undefined;
          awaiting = false;
          break;
        case "compilerCancel":
          if (active !== message.id) return;
          vm.cancel();
          if (awaiting)
            fail(message.id, Object.assign(new Error("request canceled"), { code: "canceled" }));
          break;
        case "compilerStats":
          port.send({
            kind: "compilerResponse",
            generation,
            id: message.id,
            stats: { ...vm.stats(), wasmBytes: memory.buffer.byteLength },
          });
          break;
      }
    } catch (error) {
      fail(message.id, error);
    }
  };
}
