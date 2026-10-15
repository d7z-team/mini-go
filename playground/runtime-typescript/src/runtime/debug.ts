import type { RuntimeClient } from "./client";
import type { Options } from "./types";

export interface DebugSource {
  module: string;
  path: string;
  text: string;
}

export interface DebugOptions extends Options {
  sources?: Record<string, DebugSource>;
  entry?: string;
}

type RuntimeFactory = (image: Uint8Array | ArrayBuffer, options: Options) => Promise<RuntimeClient>;

/** DAP state and lazy variable handles are owned by the runtime worker. */
export class DebugSession {
  private disposed: Promise<void> | undefined;

  private constructor(
    private readonly runtime: RuntimeClient,
    private readonly owned: boolean,
  ) {}

  static async create(
    factory: RuntimeFactory,
    image: Uint8Array | ArrayBuffer,
    options: DebugOptions = {},
  ): Promise<DebugSession> {
    const { sources, entry, ...runtimeOptions } = options;
    const runtime = await factory(image, runtimeOptions);
    try {
      return await DebugSession.open(runtime, sources, entry, true);
    } catch (error) {
      await runtime.close();
      throw error;
    }
  }

  static attach(
    runtime: RuntimeClient,
    options: Pick<DebugOptions, "sources" | "entry"> = {},
  ): Promise<DebugSession> {
    return DebugSession.open(runtime, options.sources, options.entry, false);
  }

  private static async open(
    runtime: RuntimeClient,
    sources: Record<string, DebugSource> | undefined,
    entry: string | undefined,
    owned: boolean,
  ): Promise<DebugSession> {
    await runtime.debugOpen(sources ?? {}, entry ?? "default");
    return new DebugSession(runtime, owned);
  }

  output(category: string, text: string): Promise<void> {
    return this.runtime.debugOutput(category, text);
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
