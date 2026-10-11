import type { WorkerFailure } from "../worker-channel.js";

export type CompilerCommand =
  | {
      kind: "compilerCreate";
      generation: bigint;
      compilerUrl?: string;
    }
  | { kind: "compilerRequest"; generation: bigint; id: number; input: Uint8Array }
  | { kind: "compilerClose" | "compilerStats"; generation: bigint; id: number };

export type CompilerResponse = {
  kind: "compilerResponse";
  generation: bigint;
  id: number;
  value?: Uint8Array;
  stats?: { requests: bigint; callMilliseconds: number; wasmBytes: number };
  error?: WorkerFailure;
};
