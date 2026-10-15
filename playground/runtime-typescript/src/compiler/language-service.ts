import {
  copyTransferBytes,
  serializeError,
  type WorkerConnection,
  type WorkerFailure,
} from "../worker-channel";
import { deferred } from "../deferred";
import type { CompilerCommand, CompilerResponse } from "./protocol";

export interface CompilerOptions {
  /** Override the compiler Worker entry when assets are deployed separately. */
  workerUrl?: string | URL;
  /** Override the Go compiler WASM module inside the compiler Worker. */
  compilerUrl?: string | URL;
  signal?: AbortSignal;
  /** Request budget including queueing and worker recovery. Defaults to 30 seconds. */
  timeoutMs?: number;
}

export interface CompilerUpgrade {
  cleanupError?: WorkerFailure;
}

export interface CompilerStats {
  requests: bigint;
  callMilliseconds: number;
  wasmBytes: number;
  restarts: bigint;
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
  Image?: Uint8Array;
  Symbols?: Uint8Array;
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

type CompilerConnection = WorkerConnection<CompilerCommand, CompilerResponse>;
export type CompilerWorkerFactory = (options: CompilerOptions) => CompilerConnection;

const MAX_BYTES = 64 << 20;
const MAX_GENERATION = (1n << 64n) - 1n;
const textEncoder = new TextEncoder();
const strictTextDecoder = new TextDecoder("utf-8", { fatal: true });

interface Peer {
  connection: CompilerConnection;
  generation: bigint;
  waiting:
    | { id: number; accept(message: CompilerResponse): void; fail(error: unknown): void }
    | undefined;
  dead: boolean;
}

interface Job {
  controller: AbortController;
  bytes: number;
  action(signal: AbortSignal): Promise<unknown>;
  resolve(value: unknown): void;
  reject(error: unknown): void;
  cleanup(): void;
}

/** A serialized owner for one stateful Go compiler Worker. */
export class LanguageService {
  private peer: Peer | undefined;
  private generation = 0n;
  private sequence = 0;
  private queue: Job[] = [];
  private active: Job | undefined;
  private queuedBytes = 0;
  private closing = false;
  private recovery: Record<string, unknown> | undefined;
  private session = "";
  private revision = "";
  private snapshot = "";
  private restarts = 0n;
  private readonly closed = deferred<void>();

  private constructor(
    private readonly factory: CompilerWorkerFactory,
    private readonly options: CompilerOptions & { timeoutMs: number },
  ) {}

  static async create(
    factory: CompilerWorkerFactory,
    options: CompilerOptions = {},
  ): Promise<LanguageService> {
    const timeoutMs = options.timeoutMs ?? 30_000;
    if (!Number.isInteger(timeoutMs) || timeoutMs <= 0 || timeoutMs > 2_147_483_647)
      throw new ToolsError("invalid_argument", "invalid compiler timeoutMs");
    if (options.signal?.aborted) throw options.signal.reason;
    const configuration: CompilerOptions & { timeoutMs: number } = { timeoutMs };
    if (options.workerUrl !== undefined) configuration.workerUrl = options.workerUrl.toString();
    if (options.compilerUrl !== undefined)
      configuration.compilerUrl = options.compilerUrl.toString();
    const service = new LanguageService(factory, configuration);
    try {
      await service.request({ Operation: "hello" }, options.signal);
      return service;
    } catch (error) {
      await service.dispose();
      throw error;
    }
  }

  private terminatePeer(peer: Peer, error: unknown): WorkerFailure | undefined {
    if (peer.dead) return;
    peer.dead = true;
    let cleanupError: WorkerFailure | undefined;
    try {
      peer.connection.send({ kind: "compilerClose", generation: peer.generation, id: 0 });
    } catch {
      // Termination below owns final cleanup.
    }
    try {
      peer.connection.terminate();
    } catch (failure) {
      cleanupError = serializeError(failure);
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
    input: Uint8Array | undefined,
    signal: AbortSignal,
  ): Promise<CompilerResponse> {
    return new Promise((resolve, reject) => {
      if (signal.aborted) {
        reject(signal.reason);
        return;
      }
      const cleanup = () => {
        signal.removeEventListener("abort", cancel);
        peer.waiting = undefined;
      };
      const cancel = () =>
        this.terminatePeer(
          peer,
          signal.reason ?? new ToolsError("canceled", "compiler request canceled"),
        );
      peer.waiting = {
        id,
        fail: (error) => {
          cleanup();
          reject(error);
        },
        accept: (reply) => {
          cleanup();
          if (reply.error)
            reject(new ToolsError(reply.error.code ?? "internal", reply.error.message));
          else resolve(reply);
        },
      };
      signal.addEventListener("abort", cancel, { once: true });
      try {
        if (id === 0) {
          const command: CompilerCommand = {
            kind: "compilerCreate",
            generation: peer.generation,
          };
          if (this.options.compilerUrl !== undefined)
            command.compilerUrl = this.options.compilerUrl.toString();
          peer.connection.send(command);
        } else if (input)
          peer.connection.send({ kind: "compilerRequest", generation: peer.generation, id, input });
        else peer.connection.send({ kind: "compilerStats", generation: peer.generation, id });
      } catch (error) {
        this.terminatePeer(peer, error);
      }
    });
  }

  private async connect(signal: AbortSignal): Promise<Peer> {
    if (this.generation === MAX_GENERATION)
      throw new ToolsError("budget", "compiler generations exhausted");
    const peer: Peer = {
      connection: this.factory(this.options),
      generation: ++this.generation,
      waiting: undefined,
      dead: false,
    };
    this.restarts++;
    peer.connection.listen(
      (reply) => {
        if (
          reply.kind === "compilerResponse" &&
          !peer.dead &&
          reply.generation === peer.generation &&
          reply.id === peer.waiting?.id
        )
          peer.waiting.accept(reply);
      },
      (error) => this.terminatePeer(peer, error),
    );
    try {
      await this.exchange(peer, 0, undefined, signal);
      if (this.recovery) {
        const reply = await this.exchange(
          peer,
          this.nextID(),
          encodeRequest(this.recovery),
          signal,
        );
        const restored = decodeResponse(reply.value);
        this.acceptState(restored);
      }
      return peer;
    } catch (error) {
      this.terminatePeer(peer, error);
      throw error;
    }
  }

  private enqueue<T>(
    bytes: number,
    action: (signal: AbortSignal) => Promise<T>,
    signal?: AbortSignal,
    timeout = this.options.timeoutMs,
  ): Promise<T> {
    if (this.closing) return Promise.reject(new ToolsError("closed", "language service closed"));
    if (signal?.aborted) return Promise.reject(signal.reason);
    if (
      this.queue.length + Number(Boolean(this.active)) >= 128 ||
      bytes > MAX_BYTES - this.queuedBytes
    )
      return Promise.reject(new ToolsError("budget", "language request queue exhausted"));
    if (timeout <= 0)
      return Promise.reject(new ToolsError("deadline", "compiler request deadline"));
    this.queuedBytes += bytes;
    return new Promise<T>((resolve, reject) => {
      const controller = new AbortController();
      const abort = () =>
        controller.abort(signal?.reason ?? new ToolsError("canceled", "compiler request canceled"));
      signal?.addEventListener("abort", abort, { once: true });
      const timer = setTimeout(
        () => controller.abort(new ToolsError("deadline", "compiler request deadline")),
        timeout,
      );
      const job: Job = {
        controller,
        bytes,
        action,
        resolve: (value) => resolve(value as T),
        reject,
        cleanup: () => {
          clearTimeout(timer);
          signal?.removeEventListener("abort", abort);
          this.queuedBytes -= bytes;
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
      .action(job.controller.signal)
      .then(job.resolve, job.reject)
      .finally(() => {
        job.cleanup();
        this.active = undefined;
        if (this.closing) this.closed.resolve();
        else this.drain();
      });
  }

  request(input: Record<string, unknown>, signal?: AbortSignal): Promise<ToolsResponse> {
    const request: Record<string, unknown> = {
      Format: "mini-go-tools",
      Version: 3,
      Session: this.session,
      Revision: this.revision,
      ...input,
    };
    const encoded = encodeRequest(request);
    let timeout = this.options.timeoutMs;
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
      encoded.byteLength,
      async (activeSignal) => {
        const peer = this.peer ?? (await this.connect(activeSignal));
        this.peer = peer;
        const reply = await this.exchange(peer, this.nextID(), encoded, activeSignal);
        const response = decodeResponse(reply.value);
        this.acceptState(response);
        if (response.Error) throw new ToolsError(response.Error.Code, response.Error.Message);
        return response;
      },
      signal,
      timeout,
    );
  }

  private acceptState(response: ToolsResponse): void {
    if (response.Format !== "mini-go-tools" || response.Version !== 3)
      throw new ToolsError("identity", "invalid compiler response identity");
    if (response.Session) this.session = response.Session;
    if (response.Revision) this.revision = response.Revision;
    if (response.Analysis?.Snapshot) this.snapshot = response.Analysis.Snapshot;
    if (response.Recovery) this.recovery = structuredClone(response.Recovery);
  }

  async open(input: WorkspaceInput, signal?: AbortSignal): Promise<string> {
    const response = await this.request({ Operation: "workspace/open", ...input }, signal);
    this.snapshot = "";
    return response.Revision!;
  }
  async update(changes: DocumentUpdate[], signal?: AbortSignal): Promise<string> {
    const response = await this.request({ Operation: "document/update", Changes: changes }, signal);
    this.snapshot = "";
    return response.Revision!;
  }
  async replaceWorkspace(
    input: WorkspaceInput,
    changes: DocumentUpdate[] = [],
    signal?: AbortSignal,
  ): Promise<string> {
    const response = await this.request(
      { Operation: "workspace/update", ...input, Changes: changes },
      signal,
    );
    this.snapshot = "";
    return response.Revision!;
  }
  async analyze(signal?: AbortSignal): Promise<Analysis> {
    return (await this.request({ Operation: "workspace/analyze" }, signal)).Analysis!;
  }
  async query<T = unknown>(
    operation: string,
    parameters: Record<string, unknown> = {},
    signal?: AbortSignal,
  ): Promise<T> {
    return (
      await this.request(
        {
          Operation: `language/${operation}`,
          Query: { Snapshot: this.snapshot, ...parameters },
        },
        signal,
      )
    ).Value as T;
  }
  async sources(trees: SourceTree[], signal?: AbortSignal): Promise<SourcePackages> {
    return (await this.request({ Operation: "workspace/sources", Trees: trees }, signal))
      .Value as SourcePackages;
  }
  prepare(options: Record<string, unknown> = {}, signal?: AbortSignal): Promise<ToolsResponse> {
    return this.request(
      { Operation: "build/prepare", Build: { Revision: this.revision, ...options } },
      signal,
    );
  }

  stats(): Promise<CompilerStats> {
    return this.enqueue(0, async (signal) => {
      if (!this.peer) throw new ToolsError("closed", "compiler worker unavailable");
      const reply = await this.exchange(this.peer, this.nextID(), undefined, signal);
      if (!reply.stats) throw new ToolsError("internal", "compiler stats unavailable");
      return { ...reply.stats, restarts: this.restarts };
    });
  }

  upgrade(compilerUrl?: string | URL, signal?: AbortSignal): Promise<CompilerUpgrade> {
    return this.enqueue(
      0,
      async (activeSignal) => {
        const previousURL = this.options.compilerUrl;
        if (compilerUrl === undefined) delete this.options.compilerUrl;
        else this.options.compilerUrl = compilerUrl.toString();
        let candidate: Peer;
        try {
          candidate = await this.connect(activeSignal);
          const hello = decodeResponse(
            (
              await this.exchange(
                candidate,
                this.nextID(),
                encodeRequest({ Format: "mini-go-tools", Version: 3, Operation: "hello" }),
                activeSignal,
              )
            ).value,
          );
          if (hello.Error) throw new ToolsError(hello.Error.Code, hello.Error.Message);
        } catch (error) {
          if (previousURL === undefined) delete this.options.compilerUrl;
          else this.options.compilerUrl = previousURL;
          throw error;
        }
        const previous = this.peer;
        this.peer = candidate;
        const cleanupError = previous
          ? this.terminatePeer(previous, new ToolsError("closed", "compiler upgraded"))
          : undefined;
        return cleanupError ? { cleanupError } : {};
      },
      signal,
    );
  }

  dispose(): Promise<void> {
    if (this.closing) return this.closed.promise;
    this.closing = true;
    const error = new ToolsError("closed", "language service closed");
    for (const job of this.queue.splice(0)) {
      job.cleanup();
      job.reject(error);
    }
    this.active?.controller.abort(error);
    if (this.peer) this.terminatePeer(this.peer, error);
    this.recovery = undefined;
    if (!this.active) this.closed.resolve();
    return this.closed.promise;
  }
}

function encodeRequest(request: Record<string, unknown>): Uint8Array {
  const bytes = textEncoder.encode(JSON.stringify(request));
  if (bytes.byteLength > MAX_BYTES)
    throw new ToolsError("budget", "compiler input exceeds byte limit");
  return bytes;
}

function decodeResponse(value: Uint8Array | undefined): ToolsResponse {
  if (!value || value.byteLength < 20)
    throw new ToolsError("identity", "invalid compiler response frame");
  const magic = new Uint8Array([0x4d, 0x47, 0x54, 0x33, 0x0d, 0x0a, 0x1a, 0x0a]);
  for (let index = 0; index < magic.length; index++)
    if (value[index] !== magic[index])
      throw new ToolsError("identity", "invalid compiler response magic");
  const view = new DataView(value.buffer, value.byteOffset, value.byteLength);
  const metadataLength = view.getUint32(8, true);
  const imageLength = view.getUint32(12, true);
  const symbolsLength = view.getUint32(16, true);
  if (20 + metadataLength + imageLength + symbolsLength !== value.byteLength)
    throw new ToolsError("identity", "invalid compiler response lengths");
  const metadataEnd = 20 + metadataLength;
  const imageEnd = metadataEnd + imageLength;
  let response: ToolsResponse;
  try {
    response = JSON.parse(
      strictTextDecoder.decode(value.subarray(20, metadataEnd)),
    ) as ToolsResponse;
    if (imageLength) response.Image = copyTransferBytes(value.subarray(metadataEnd, imageEnd));
    else delete response.Image;
    if (symbolsLength) response.Symbols = copyTransferBytes(value.subarray(imageEnd));
    else delete response.Symbols;
  } catch {
    throw new ToolsError("identity", "invalid compiler response encoding");
  }
  return response;
}
