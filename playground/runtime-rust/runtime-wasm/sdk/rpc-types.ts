/** Stable MRPC method identity generated from one contract catalog. */
export interface RPCMethod {
  readonly id: string;
  readonly service: string;
  readonly name: string;
  readonly contractHash: string;
  readonly resourceTypeHash: string;
}

export interface RPCContract {
  readonly protocol: "minigo.rpc.contract.v2";
  readonly methods: readonly RPCMethod[];
}

export interface RPCComplex {
  readonly re: number;
  readonly im: number;
}

export interface RPCMapEntry {
  readonly key: RPCValue;
  readonly value: RPCValue;
}

export interface RPCField {
  readonly id: number;
  readonly value: RPCValue;
}

export interface RPCProvidedResource<H = unknown> {
  readonly descriptor: RPCResourceDescriptor<H>;
  readonly handler: H;
}

export type RPCData =
  | { readonly kind: "nil" }
  | { readonly kind: "bool"; readonly value: boolean }
  | { readonly kind: "int"; readonly value: bigint }
  | { readonly kind: "uint"; readonly value: bigint }
  | { readonly kind: "float"; readonly value: number }
  | { readonly kind: "complex"; readonly value: readonly [number, number] }
  | { readonly kind: "string"; readonly value: string }
  | { readonly kind: "bytes"; readonly value: Uint8Array }
  | { readonly kind: "slice"; readonly value: readonly RPCValue[] }
  | { readonly kind: "map"; readonly value: readonly RPCMapEntry[] }
  | { readonly kind: "struct"; readonly value: readonly RPCField[] }
  | { readonly kind: "optional"; readonly value: RPCValue }
  | { readonly kind: "resource"; readonly value: number }
  | {
      readonly kind: "localResource";
      readonly value: number | RPCProvidedResource;
    };

export interface RPCValue {
  readonly type: string;
  readonly data: RPCData;
}

export interface RPCCodec<T> {
  encode(value: T): RPCValue;
  decode(value: RPCValue): T;
}

export interface RPCCallOptions {
  signal?: AbortSignal;
  /** Relative deadline. Omit it for calls that may run indefinitely. */
  timeoutMs?: number;
}

export interface RPCBindOptions extends RPCCallOptions {
  affinityKey?: string;
  labels?: Readonly<Record<string, string>>;
}

export interface RPCPublishOptions {
  name?: string;
  priority?: number;
  weight?: number;
  maxLeases?: number;
  labels?: Readonly<Record<string, string>>;
}

export interface RPCConnectOptions {
  signal?: AbortSignal;
  leaseTtlMs?: number;
  admissionTimeoutMs?: number;
  maxCallDurationMs?: number;
  workerUrl?: string | URL;
  wasmUrl?: string | URL;
}

export interface RPCPeer {
  readonly identity: string;
  readonly attributes: Readonly<Record<string, string>>;
}

export interface RPCServerContext {
  readonly signal: AbortSignal;
  readonly peer: RPCPeer;
  readonly provider: string;
}

export interface RPCProvider {
  readonly contract: RPCContract;
  invoke(
    context: RPCServerContext,
    method: string,
    arguments_: readonly RPCValue[],
  ): readonly RPCValue[] | Promise<readonly RPCValue[]>;
}

export interface RPCResourceDescriptor<H> {
  readonly typeHash: string;
  invoke(
    handler: H,
    context: RPCServerContext,
    method: string,
    arguments_: readonly RPCValue[],
  ): readonly RPCValue[] | Promise<readonly RPCValue[]>;
  close(handler: H): void | Promise<void>;
}

export interface RPCResource {
  readonly typeHash: string;
  /** Encodes this handle for a parameter on its owning binding. */
  value(): RPCValue;
  invoke<T>(
    method: RPCMethod,
    arguments_: readonly RPCValue[],
    decode: (values: readonly RPCValue[]) => T,
    options?: RPCCallOptions,
  ): Promise<T>;
  close(options?: RPCCallOptions): Promise<void>;
}

export interface RPCBinding {
  invoke<T>(
    method: RPCMethod,
    arguments_: readonly RPCValue[],
    decode: (values: readonly RPCValue[]) => T,
    options?: RPCCallOptions,
  ): Promise<T>;
  resource(value: RPCValue, typeHash: string): RPCResource | null;
  close(): Promise<void>;
}

export interface RPCPublication {
  close(options?: { signal?: AbortSignal }): Promise<void>;
}

export interface RPCStats {
  readonly bindings: bigint;
  readonly resources: bigint;
  readonly pendingResults: bigint;
  readonly publications: bigint;
  readonly pendingCalls: bigint;
  readonly inboundCalls: bigint;
  readonly wasmBytes: number;
}

export interface RPCConnection {
  bind(contract: RPCContract, options?: RPCBindOptions): Promise<RPCBinding>;
  publish(provider: RPCProvider, options?: RPCPublishOptions): Promise<RPCPublication>;
  stats(): Promise<RPCStats>;
  close(options?: { signal?: AbortSignal }): Promise<void>;
  terminate(reason?: unknown): void;
}

export class RPCStatus extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "RPCStatus";
  }

  static from(reason: unknown, fallback = "unknown"): RPCStatus {
    if (reason instanceof RPCStatus) return reason;
    if (reason && typeof reason === "object") {
      const value = reason as { code?: unknown; message?: unknown };
      const code = typeof value.code === "string" ? value.code : fallback;
      return new RPCStatus(code, String(value.message ?? reason));
    }
    return new RPCStatus(fallback, String(reason));
  }
}

function invalid(message: string): never {
  throw new RPCStatus("invalid_argument", message);
}

function protocol(message: string): never {
  throw new RPCStatus("protocol", message);
}

function expect(value: RPCValue, type: string, kind: RPCData["kind"]): RPCData {
  if (!value || value.type !== type || value.data?.kind !== kind)
    protocol(`invalid MRPC ${type} value`);
  return value.data;
}

function integerCodec(type: string, signed: boolean, bits: number): RPCCodec<number | bigint> {
  const minimum = signed ? -(1n << BigInt(bits - 1)) : 0n;
  const maximum = signed ? (1n << BigInt(bits - 1)) - 1n : (1n << BigInt(bits)) - 1n;
  const kind = signed ? "int" : "uint";
  const wide = bits === 64;
  return {
    encode(value) {
      if (wide && typeof value !== "bigint") invalid(`${type} requires bigint`);
      if (!wide && (typeof value !== "number" || !Number.isSafeInteger(value)))
        invalid(`${type} requires an exact integer number`);
      const number = BigInt(value);
      if (number < minimum || number > maximum) invalid(`${type} is out of range`);
      return { type, data: { kind, value: number } as RPCData };
    },
    decode(value) {
      const data = expect(value, type, kind) as Extract<RPCData, { kind: "int" | "uint" }>;
      if (typeof data.value !== "bigint") protocol(`invalid MRPC ${type} value`);
      if (data.value < minimum || data.value > maximum) protocol(`${type} is out of range`);
      return wide ? data.value : Number(data.value);
    },
  };
}

function floatCodec(type: "float32" | "float64"): RPCCodec<number> {
  return {
    encode(value) {
      if (typeof value !== "number") invalid(`${type} requires number`);
      return {
        type,
        data: { kind: "float", value: type === "float32" ? Math.fround(value) : value },
      };
    },
    decode(value) {
      const data = expect(value, type, "float") as Extract<RPCData, { kind: "float" }>;
      if (typeof data.value !== "number") protocol(`invalid MRPC ${type} value`);
      return type === "float32" ? Math.fround(data.value) : data.value;
    },
  };
}

function complexCodec(type: "complex64" | "complex128"): RPCCodec<RPCComplex> {
  return {
    encode(value) {
      if (!value || typeof value.re !== "number" || typeof value.im !== "number")
        invalid(`${type} requires real and imaginary numbers`);
      const round = type === "complex64" ? Math.fround : (number: number) => number;
      return { type, data: { kind: "complex", value: [round(value.re), round(value.im)] } };
    },
    decode(value) {
      const data = expect(value, type, "complex") as Extract<RPCData, { kind: "complex" }>;
      if (
        !Array.isArray(data.value) ||
        data.value.length !== 2 ||
        typeof data.value[0] !== "number" ||
        typeof data.value[1] !== "number"
      )
        protocol(`invalid MRPC ${type} value`);
      const round = type === "complex64" ? Math.fround : (number: number) => number;
      return { re: round(data.value[0]), im: round(data.value[1]) };
    },
  };
}

export const rpcCodecs = {
  bool: {
    encode(value: boolean): RPCValue {
      if (typeof value !== "boolean") invalid("bool requires boolean");
      return { type: "bool", data: { kind: "bool", value } };
    },
    decode(value: RPCValue): boolean {
      const decoded = (expect(value, "bool", "bool") as Extract<RPCData, { kind: "bool" }>).value;
      if (typeof decoded !== "boolean") protocol("invalid MRPC bool value");
      return decoded;
    },
  } satisfies RPCCodec<boolean>,
  string: {
    encode(value: string): RPCValue {
      if (typeof value !== "string") invalid("string requires a string");
      return { type: "string", data: { kind: "string", value } };
    },
    decode(value: RPCValue): string {
      const decoded = (expect(value, "string", "string") as Extract<RPCData, { kind: "string" }>)
        .value;
      if (typeof decoded !== "string") protocol("invalid MRPC string value");
      return decoded;
    },
  } satisfies RPCCodec<string>,
  int8: integerCodec("int8", true, 8) as RPCCodec<number>,
  int16: integerCodec("int16", true, 16) as RPCCodec<number>,
  int32: integerCodec("int32", true, 32) as RPCCodec<number>,
  int64: integerCodec("int64", true, 64) as RPCCodec<bigint>,
  uint8: integerCodec("uint8", false, 8) as RPCCodec<number>,
  uint16: integerCodec("uint16", false, 16) as RPCCodec<number>,
  uint32: integerCodec("uint32", false, 32) as RPCCodec<number>,
  uint64: integerCodec("uint64", false, 64) as RPCCodec<bigint>,
  float32: floatCodec("float32"),
  float64: floatCodec("float64"),
  complex64: complexCodec("complex64"),
  complex128: complexCodec("complex128"),
};

export function enumCodec(type: string): RPCCodec<number> {
  const base = rpcCodecs.int32;
  return {
    encode(value) {
      const encoded = base.encode(value);
      return { type, data: encoded.data };
    },
    decode(value) {
      if (value.type !== type) protocol(`invalid MRPC ${type} value`);
      return base.decode({ type: "int32", data: value.data });
    },
  };
}

/** Defers named codec lookup so recursive and mutually recursive messages initialize safely. */
export function lazyCodec<T>(resolve: () => RPCCodec<T>): RPCCodec<T> {
  return {
    encode(value) {
      return resolve().encode(value);
    },
    decode(value) {
      return resolve().decode(value);
    },
  };
}

export function bytesCodec(type = "[]uint8"): RPCCodec<Uint8Array | null> {
  return {
    encode(value) {
      if (value === null) return { type, data: { kind: "nil" } };
      if (!(value instanceof Uint8Array)) invalid(`${type} requires Uint8Array or null`);
      return { type, data: { kind: "bytes", value: Uint8Array.from(value) } };
    },
    decode(value) {
      if (value.type !== type) protocol(`invalid MRPC ${type} value`);
      if (value.data.kind === "nil") return null;
      if (value.data.kind !== "bytes" || !(value.data.value instanceof Uint8Array))
        protocol(`invalid MRPC ${type} value`);
      return Uint8Array.from(value.data.value);
    },
  };
}

export function sliceCodec<T>(type: string, element: RPCCodec<T>): RPCCodec<T[] | null> {
  return {
    encode(value) {
      if (value === null) return { type, data: { kind: "nil" } };
      if (!Array.isArray(value)) invalid(`${type} requires an array or null`);
      return { type, data: { kind: "slice", value: value.map((item) => element.encode(item)) } };
    },
    decode(value) {
      if (value.type !== type) protocol(`invalid MRPC ${type} value`);
      if (value.data.kind === "nil") return null;
      if (value.data.kind !== "slice" || !Array.isArray(value.data.value))
        protocol(`invalid MRPC ${type} value`);
      return value.data.value.map((item) => element.decode(item));
    },
  };
}

export function mapCodec<K, V>(
  type: string,
  key: RPCCodec<K>,
  element: RPCCodec<V>,
): RPCCodec<Map<K, V> | null> {
  return {
    encode(value) {
      if (value === null) return { type, data: { kind: "nil" } };
      if (!(value instanceof Map)) invalid(`${type} requires Map or null`);
      return {
        type,
        data: {
          kind: "map",
          value: [...value].map(([itemKey, itemValue]) => ({
            key: key.encode(itemKey),
            value: element.encode(itemValue),
          })),
        },
      };
    },
    decode(value) {
      if (value.type !== type) protocol(`invalid MRPC ${type} value`);
      if (value.data.kind === "nil") return null;
      if (value.data.kind !== "map" || !Array.isArray(value.data.value))
        protocol(`invalid MRPC ${type} value`);
      const decoded = new Map<K, V>();
      for (const entry of value.data.value) {
        if (!entry || typeof entry !== "object" || !("key" in entry) || !("value" in entry))
          protocol(`invalid MRPC ${type} map entry`);
        decoded.set(key.decode(entry.key), element.decode(entry.value));
      }
      return decoded;
    },
  };
}

export function optionalCodec<T>(type: string, element: RPCCodec<T>): RPCCodec<T | undefined> {
  return {
    encode(value) {
      return value === undefined
        ? { type, data: { kind: "nil" } }
        : { type, data: { kind: "optional", value: element.encode(value) } };
    },
    decode(value) {
      if (value.type !== type) protocol(`invalid MRPC ${type} value`);
      if (value.data.kind === "nil") return undefined;
      if (value.data.kind !== "optional") protocol(`invalid MRPC ${type} value`);
      return element.decode(value.data.value);
    },
  };
}

export interface RPCMessageField<T> {
  readonly id: number;
  readonly name: keyof T & string;
  readonly codec: RPCCodec<unknown>;
}

export function messageCodec<T extends object>(
  type: string,
  fields: readonly RPCMessageField<T>[],
): RPCCodec<T> {
  return {
    encode(value) {
      if (!value || typeof value !== "object") invalid(`${type} requires an object`);
      return {
        type,
        data: {
          kind: "struct",
          value: fields.map((field) => ({
            id: field.id,
            value: field.codec.encode(value[field.name]),
          })),
        },
      };
    },
    decode(value) {
      const data = expect(value, type, "struct") as Extract<RPCData, { kind: "struct" }>;
      if (!Array.isArray(data.value)) protocol(`invalid MRPC ${type} value`);
      const byID = new Map<number, RPCValue>();
      for (const field of data.value) {
        if (
          !field ||
          typeof field !== "object" ||
          !Number.isSafeInteger(field.id) ||
          field.id <= 0 ||
          !("value" in field) ||
          byID.has(field.id)
        )
          protocol(`invalid ${type} field ID`);
        byID.set(field.id, field.value);
      }
      const decoded: Record<string, unknown> = {};
      for (const field of fields) {
        const encoded = byID.get(field.id);
        if (!encoded) protocol(`missing ${type}.${field.name}`);
        byID.delete(field.id);
        decoded[field.name] = field.codec.decode(encoded);
      }
      if (byID.size) protocol(`unknown ${type} field`);
      return decoded as T;
    },
  };
}

export function providedResource<H>(
  typeHash: string,
  descriptor: RPCResourceDescriptor<H>,
  handler: H | null,
): RPCValue {
  if (handler === null) return { type: typeHash, data: { kind: "nil" } };
  if (descriptor.typeHash !== typeHash)
    invalid("RPC resource descriptor type does not match its contract");
  return {
    type: typeHash,
    data: { kind: "localResource", value: { descriptor, handler } },
  };
}

export function localResource<H>(
  value: RPCValue,
  typeHash: string,
  descriptor: RPCResourceDescriptor<H>,
): H | null {
  if (value.type !== typeHash) protocol("RPC resource type does not match");
  if (value.data.kind === "nil") return null;
  if (value.data.kind !== "localResource" || typeof value.data.value === "number")
    protocol("RPC provider resource is unavailable");
  if (value.data.value.descriptor !== descriptor)
    protocol("RPC provider resource descriptor does not match");
  return value.data.value.handler as H;
}
