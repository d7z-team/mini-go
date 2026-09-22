import type { Runtime } from "./runtime.js";
import type { Options } from "./types.js";
import type { ToolsResponse } from "./tools.js";
export interface DebugSource {
  module: string;
  path: string;
  text: string;
}
export interface DebugOptions extends Options {
  sources?: Record<string, DebugSource>;
  entry?: string;
}

/** DAP state and lazy variable handles are owned by the shared Rust adapter. */
export class DebugSession {
  output(category: string, text: string): Promise<void> {
    return this.runtime.debugOutput(category, text);
  }
  private disposed?: Promise<void>;
  private constructor(
    private runtime: Runtime,
    private owned: boolean,
  ) {}
  static async create(
    factory: RuntimeFactory,
    image: Uint8Array | ArrayBuffer,
    options: DebugOptions = {},
  ): Promise<DebugSession> {
    const { sources, entry, ...runtimeOptions } = options;
    const runtime = await factory(image, runtimeOptions);
    try {
      return await DebugSession.bind(runtime, { sources, entry }, true);
    } catch (error) {
      await runtime.close();
      throw error;
    }
  }
  static async fromBuild(
    factory: RuntimeFactory,
    build: ToolsResponse,
    options: DebugOptions = {},
  ): Promise<DebugSession> {
    if (!build.ImageJSON) throw new Error("build did not produce an execution image");
    return DebugSession.create(factory, new TextEncoder().encode(build.ImageJSON), {
      ...options,
      sources: build.Sources,
      symbols: build.SymbolsJSON ? new TextEncoder().encode(build.SymbolsJSON) : undefined,
    });
  }
  static async bind(
    runtime: Runtime,
    options: Pick<DebugOptions, "sources" | "entry"> = {},
    owned = false,
  ): Promise<DebugSession> {
    await runtime.debugOpen(options.sources ?? {}, options.entry ?? "default");
    return new DebugSession(runtime, owned);
  }
  async request(
    command: string,
    arguments_: Record<string, unknown> = {},
  ): Promise<Record<string, unknown>> {
    if (this.disposed) throw new Error("debug session disposed");
    const response = this.runtime.debugRequest(command, arguments_);
    if (command === "disconnect" || command === "terminate") {
      this.disposed = response.then(
        async () => {
          if (this.owned) await this.runtime.close();
        },
        (error) => {
          this.disposed = undefined;
          throw error;
        },
      );
      await this.disposed;
    }
    return response;
  }
  events(): Promise<Record<string, unknown>[]> {
    return this.runtime.debugEvents();
  }
  async *watch(signal?: AbortSignal): AsyncGenerator<Record<string, unknown>> {
    while (!this.disposed && !signal?.aborted) {
      for (const event of await this.events()) yield event;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
  dispose(): Promise<void> {
    this.disposed ??= (async () => {
      try {
        await this.runtime.debugRequest("disconnect");
      } finally {
        if (this.owned) await this.runtime.close();
      }
    })();
    return this.disposed;
  }
}

type RuntimeFactory = (image: Uint8Array | ArrayBuffer, options: Options) => Promise<Runtime>;
