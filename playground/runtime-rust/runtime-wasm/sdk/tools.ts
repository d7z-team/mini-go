import {
  copyBytes,
  serializeError,
  type Failure,
  type WorkerFactory,
  type WorkerConnection,
  type CompilerResponse,
} from "./protocol.js";
import type { Options, Stats } from "./types.js";

export type CompilerOptions = Pick<Options, "workerUrl" | "wasmUrl" | "signal">;
export interface CompilerUpgrade {
  cleanupError?: Failure;
}

export interface SourceFile {
  Path: string;
  Text: string;
  OriginPath?: string;
  URI?: string;
}
export interface SourcePackage {
  Editable?: boolean | null;
  Namespace: string;
  PackagePath?: string;
  ModulePath: string;
  Files: SourceFile[];
  TestFiles?: SourceFile[];
  SelectionTarget?: { tags?: string[] | null };
  SourceCandidates?:
    | {
        Path: string;
        Hash: string;
        Size: number;
        Selected: boolean;
        Test: boolean;
      }[]
    | null;
  Resources?: { Path: string; Data: string | null; SourcePath?: string }[];
}
export interface SourceTree {
  ModulePath: string;
  Editable?: boolean | null;
  Files: { Path: string; Data: string; URI?: string }[];
}
export interface SourcePackages {
  Packages: SourcePackage[];
}
export interface WorkspaceInput {
  Root: string;
  Tags?: string[];
  Packages: SourcePackage[];
}
export interface Position {
  line: number;
  character: number;
}
export interface Range {
  start: Position;
  end: Position;
}
export interface DocumentUpdate {
  Operation: "open" | "change" | "close";
  Identity: { URI: string; ModulePath?: string; Path?: string };
  Version?: number;
  Text?: string;
  Changes?: { range?: Range; text: string }[];
}
export interface Analysis {
  Revision: string;
  Snapshot: string;
  CheckedPackages: number;
  ReusedPackages: number;
  Diagnostics: Record<string, unknown>;
  WorkspaceDiagnostics?: unknown[];
}
export interface ToolsResponse {
  Format: string;
  Version: number;
  CompilerID: string;
  Session: string;
  Revision?: string;
  Analysis?: Analysis;
  Value?: unknown;
  ImageJSON?: string;
  SymbolsJSON?: string;
  Sources?: Record<string, { module: string; path: string; text: string }>;
  Diagnostics?: unknown[];
  Error?: { Code: string; Message: string } | null;
  Recovery?: Record<string, unknown> | null;
}
export class ToolsError extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "ToolsError";
  }
}

const MAX_BYTES = 64 << 20;
const MAX_GENERATION = (1n << 64n) - 1n;
interface Peer {
  connection: WorkerConnection;
  generation: bigint;
  waiting?: { id: number; accept(message: CompilerResponse): void; fail(error: unknown): void };
  dead: boolean;
}
interface Job {
  controller: AbortController;
  deadline: number;
  action(signal: AbortSignal, deadline: number): Promise<unknown>;
  resolve(value: unknown): void;
  reject(error: unknown): void;
  cleanup(): void;
}

/** A bounded delivery owner. Guest protocol and restoration are implemented in Rust. */
export class LanguageService {
  private peer?: Peer;
  private restore = new Uint8Array();
  private generation = 0n;
  private sequence = 0;
  private queue: Job[] = [];
  private active?: Job;
  private bytes = 0;
  private closing = false;
  private closePromise?: Promise<void>;
  private closeResolve?: () => void;
  private constructor(
    private readonly factory: WorkerFactory,
    private image: Uint8Array,
    private readonly options: CompilerOptions,
  ) {}
  static async create(
    factory: WorkerFactory,
    image: Uint8Array | ArrayBuffer,
    options: CompilerOptions = {},
  ): Promise<LanguageService> {
    if (image.byteLength > MAX_BYTES)
      throw new ToolsError("load_limit", "compiler image too large");
    const service = new LanguageService(factory, copyBytes(image), {
      ...options,
      signal: undefined,
      workerUrl: options.workerUrl?.toString(),
      wasmUrl: options.wasmUrl?.toString(),
    });
    try {
      await service.request({ Operation: "hello" }, options.signal);
      return service;
    } catch (error) {
      await service.dispose();
      throw error;
    }
  }
  private stop(peer: Peer, error: unknown): Failure | undefined {
    if (peer.dead) return;
    peer.dead = true;
    let cleanupError: Failure | undefined;
    try {
      peer.connection.send({ kind: "compilerClose", generation: peer.generation, id: 0 });
    } catch {
      /* Termination also releases an unreachable worker. */
    }
    try {
      peer.connection.terminate();
    } catch (error) {
      cleanupError = serializeError(error);
    }
    const waiting = peer.waiting;
    peer.waiting = undefined;
    waiting?.fail(error);
    if (this.peer === peer) this.peer = undefined;
    return cleanupError;
  }
  private nextID(): number {
    if (this.sequence >= Number.MAX_SAFE_INTEGER)
      throw new ToolsError("budget", "compiler request IDs exhausted");
    return ++this.sequence;
  }
  private exchange(
    peer: Peer,
    id: number,
    signal: AbortSignal,
    send: () => void,
    commit?: (reply: CompilerResponse) => void,
  ): Promise<CompilerResponse> {
    return new Promise((resolve, reject) => {
      if (signal.aborted) {
        reject(signal.reason);
        return;
      }
      if (peer.dead) {
        reject(new ToolsError("closed", "compiler worker unavailable"));
        return;
      }
      let hardStop: ReturnType<typeof setTimeout> | undefined;
      const cleanup = () => {
        clearTimeout(hardStop);
        signal.removeEventListener("abort", cancel);
        peer.waiting = undefined;
      };
      const cancel = () => {
        try {
          peer.connection.send({ kind: "compilerCancel", generation: peer.generation, id });
        } catch (error) {
          this.stop(peer, error);
          return;
        }
        hardStop = setTimeout(() => this.stop(peer, signal.reason), 2000);
      };
      peer.waiting = {
        id,
        fail: (error) => {
          cleanup();
          reject(signal.aborted ? signal.reason : error);
        },
        accept: (reply) => {
          if (this.active && performance.now() >= this.active.deadline && !signal.aborted) {
            this.active.controller.abort(
              new ToolsError("deadline", "compiler response delivery deadline"),
            );
          }
          if (signal.aborted) {
            this.stop(peer, signal.reason);
            return;
          }
          if (reply.error) {
            const error = new ToolsError(reply.error.code ?? "internal", reply.error.message);
            if (reply.reusable) {
              cleanup();
              reject(error);
            } else this.stop(peer, error);
            return;
          }
          try {
            if (reply.restore && reply.restore.byteLength > MAX_BYTES)
              throw new ToolsError("budget", "compiler restore state too large");
            commit?.(reply);
          } catch (error) {
            this.stop(peer, error);
            return;
          }
          cleanup();
          resolve(reply);
          if (reply.restore) {
            try {
              peer.connection.send({ kind: "compilerAck", generation: peer.generation, id });
            } catch (error) {
              this.stop(peer, error);
            }
          }
        },
      };
      signal.addEventListener("abort", cancel, { once: true });
      try {
        send();
      } catch (error) {
        this.stop(peer, error);
      }
    });
  }
  private async connect(image: Uint8Array, signal: AbortSignal): Promise<Peer> {
    if (this.generation === MAX_GENERATION)
      throw new ToolsError("budget", "compiler generations exhausted");
    const peer: Peer = {
      connection: this.factory(this.options),
      generation: ++this.generation,
      dead: false,
    };
    peer.connection.listen(
      (reply) => {
        if (
          reply.kind !== "compilerResponse" ||
          peer.dead ||
          reply.generation !== peer.generation ||
          reply.id !== peer.waiting?.id
        )
          return;
        peer.waiting.accept(reply);
      },
      (error) => this.stop(peer, error),
    );
    try {
      await this.exchange(peer, 0, signal, () =>
        peer.connection.send({
          kind: "compilerCreate",
          generation: peer.generation,
          image,
          restore: this.restore,
          wasmUrl: this.options.wasmUrl?.toString(),
        }),
      );
      return peer;
    } catch (error) {
      this.stop(peer, error);
      throw error;
    }
  }
  private enqueue<T>(
    bytes: number,
    action: (signal: AbortSignal, deadline: number) => Promise<T>,
    signal?: AbortSignal,
    timeout = 30_000,
  ): Promise<T> {
    if (this.closing) return Promise.reject(new ToolsError("closed", "language service closed"));
    if (signal?.aborted) return Promise.reject(signal.reason);
    if (this.queue.length + Number(!!this.active) >= 128 || bytes > MAX_BYTES - this.bytes)
      return Promise.reject(new ToolsError("budget", "language request queue exhausted"));
    if (timeout <= 0)
      return Promise.reject(new ToolsError("deadline", "compiler request deadline"));
    this.bytes += bytes;
    return new Promise<T>((resolve, reject) => {
      const controller = new AbortController();
      const abort = () => controller.abort(signal?.reason);
      signal?.addEventListener("abort", abort, { once: true });
      const timer = setTimeout(
        () => controller.abort(new ToolsError("deadline", "compiler request deadline")),
        timeout,
      );
      const job: Job = {
        controller,
        deadline: performance.now() + timeout,
        action,
        resolve: (value) => resolve(value as T),
        reject,
        cleanup: () => {
          clearTimeout(timer);
          signal?.removeEventListener("abort", abort);
          this.bytes -= bytes;
        },
      };
      controller.signal.addEventListener(
        "abort",
        () => {
          const index = this.queue.indexOf(job);
          if (index >= 0) {
            this.queue.splice(index, 1);
            job.cleanup();
            reject(controller.signal.reason);
          }
        },
        { once: true },
      );
      this.queue.push(job);
      this.drain();
    });
  }
  private drain(): void {
    if (this.active || this.closing) return;
    const job = this.queue.shift();
    if (!job) return;
    this.active = job;
    void job
      .action(job.controller.signal, job.deadline)
      .then(job.resolve, job.reject)
      .finally(() => {
        job.cleanup();
        this.active = undefined;
        if (this.closing) this.closeResolve?.();
        else this.drain();
      });
  }
  request(input: Record<string, unknown>, signal?: AbortSignal): Promise<ToolsResponse> {
    const text = JSON.stringify(input);
    const bytes = new TextEncoder().encode(text).byteLength;
    let timeout = 30_000;
    if (input.Deadline !== undefined) {
      try {
        timeout = Math.max(
          0,
          Math.min(
            timeout,
            Number((BigInt(String(input.Deadline)) - BigInt(Date.now()) * 1_000_000n) / 1_000_000n),
          ),
        );
      } catch {
        return Promise.reject(new ToolsError("invalid_argument", "invalid request deadline"));
      }
    }
    return this.enqueue(
      bytes,
      async (signal, deadline) => {
        const peer = this.peer ?? (await this.connect(this.image, signal));
        this.peer = peer;
        const id = this.nextID();
        let response!: ToolsResponse;
        await this.exchange(
          peer,
          id,
          signal,
          () =>
            peer.connection.send({
              kind: "compilerRequest",
              generation: peer.generation,
              id,
              input: text,
              timeout: Math.max(0, Math.floor(deadline - performance.now())),
            }),
          (reply) => {
            if (!reply.restore || !reply.value)
              throw new ToolsError("identity", "invalid compiler delivery");
            response = JSON.parse(reply.value) as ToolsResponse;
            this.restore = copyBytes(reply.restore);
          },
        );
        return response;
      },
      signal,
      timeout,
    );
  }
  async open(input: WorkspaceInput, signal?: AbortSignal): Promise<string> {
    return (await this.request({ Operation: "workspace/open", ...input }, signal)).Revision!;
  }
  async update(changes: DocumentUpdate[], signal?: AbortSignal): Promise<string> {
    return (await this.request({ Operation: "document/update", Changes: changes }, signal))
      .Revision!;
  }
  async replaceWorkspace(
    input: WorkspaceInput,
    changes: DocumentUpdate[] = [],
    signal?: AbortSignal,
  ): Promise<string> {
    return (
      await this.request({ Operation: "workspace/update", ...input, Changes: changes }, signal)
    ).Revision!;
  }
  async analyze(signal?: AbortSignal): Promise<Analysis> {
    return (await this.request({ Operation: "workspace/analyze" }, signal)).Analysis!;
  }
  async query<T = unknown>(
    operation: string,
    parameters: Record<string, unknown> = {},
    signal?: AbortSignal,
  ): Promise<T> {
    return (await this.request({ Operation: `language/${operation}`, Query: parameters }, signal))
      .Value as T;
  }
  async sources(trees: SourceTree[], signal?: AbortSignal): Promise<SourcePackages> {
    return (await this.request({ Operation: "workspace/sources", Trees: trees }, signal))
      .Value as SourcePackages;
  }
  prepare(options: Record<string, unknown> = {}, signal?: AbortSignal): Promise<ToolsResponse> {
    return this.request({ Operation: "build/prepare", Build: options }, signal);
  }
  stats(): Promise<Stats> {
    return this.enqueue(0, async (signal) => {
      if (!this.peer) throw new ToolsError("closed", "compiler worker unavailable");
      const peer = this.peer,
        id = this.nextID();
      return (
        await this.exchange(peer, id, signal, () =>
          peer.connection.send({ kind: "compilerStats", generation: peer.generation, id }),
        )
      ).stats!;
    });
  }
  upgrade(image: Uint8Array | ArrayBuffer, signal?: AbortSignal): Promise<CompilerUpgrade> {
    if (image.byteLength > MAX_BYTES)
      return Promise.reject(new ToolsError("load_limit", "compiler image too large"));
    const bytes = copyBytes(image);
    return this.enqueue(
      bytes.byteLength,
      async (signal, deadline) => {
        const candidate = await this.connect(bytes, signal);
        const previous = this.peer;
        try {
          const id = this.nextID();
          await this.exchange(
            candidate,
            id,
            signal,
            () =>
              candidate.connection.send({
                kind: "compilerRequest",
                generation: candidate.generation,
                id,
                input: '{"Operation":"hello"}',
                timeout: Math.max(0, Math.floor(deadline - performance.now())),
              }),
            (reply) => {
              if (!reply.restore)
                throw new ToolsError("identity", "missing compiler restore state");
              this.restore = copyBytes(reply.restore);
              this.image = bytes;
              this.peer = candidate;
            },
          );
          return {
            cleanupError: previous
              ? this.stop(previous, new ToolsError("closed", "compiler upgraded"))
              : undefined,
          };
        } catch (error) {
          this.stop(candidate, error);
          throw error;
        }
      },
      signal,
    );
  }
  dispose(): Promise<void> {
    if (this.closePromise) return this.closePromise;
    this.closing = true;
    this.closePromise = new Promise((resolve) => {
      this.closeResolve = resolve;
    });
    const closed = new ToolsError("closed", "language service closed");
    for (const job of this.queue.splice(0)) {
      job.cleanup();
      job.reject(closed);
    }
    this.active?.controller.abort(closed);
    if (this.peer) this.stop(this.peer, closed);
    this.restore = new Uint8Array();
    this.image = new Uint8Array();
    if (!this.active) this.closeResolve?.();
    return this.closePromise;
  }
}
