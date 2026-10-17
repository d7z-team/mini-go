/** A client-side owner controls a platform worker through this channel. */
export interface WorkerConnection<Input, Output> {
  send(message: Input, transfer?: ArrayBuffer[]): void;
  listen(message: (value: Output) => void, failure: (reason: unknown) => void): void;
  terminate(): void;
}

/** A worker entry adapts its platform message port to this channel. */
export interface WorkerPort<Input, Output> {
  send(message: Output, transfer?: ArrayBuffer[]): void;
  listen(handler: (message: Input) => void): void;
}

export interface WorkerFailure {
  message: string;
  code?: string;
  path?: string;
}

export function serializeError(value: unknown): WorkerFailure {
  if (!value || typeof value !== "object" || !("message" in value))
    return { message: String(value) };
  const failure: WorkerFailure = { message: String(value.message) };
  if ("code" in value && typeof value.code === "string") failure.code = value.code;
  if ("path" in value && typeof value.path === "string") failure.path = value.path;
  return failure;
}

export function toError(value: unknown): Error {
  if (value instanceof Error) return value;
  const failure = serializeError(value);
  return Object.assign(new Error(failure.message), failure);
}

/** Own the visible bytes, including Buffer and offset views, before transfer or retention. */
export function copyTransferBytes(source: Uint8Array | ArrayBuffer): Uint8Array<ArrayBuffer> {
  return Uint8Array.from(source instanceof Uint8Array ? source : new Uint8Array(source));
}
