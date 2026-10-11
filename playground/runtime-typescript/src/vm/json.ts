const textEncoder = new TextEncoder();
const strictTextDecoder = new TextDecoder("utf-8", { fatal: true });

export class JsonNumber {
  constructor(readonly raw: string) {}

  integer(path: string): bigint {
    if (!/^-?(?:0|[1-9][0-9]*)$/.test(this.raw)) fail("invalid integer", path);
    return BigInt(this.raw);
  }

  number(path: string): number {
    const value = Number(this.raw);
    if (!Number.isFinite(value)) fail("number is out of range", path);
    return value;
  }
}

export type JsonValue =
  | null
  | boolean
  | string
  | JsonNumber
  | JsonValue[]
  | { [key: string]: JsonValue };

interface Span {
  start: number;
  end: number;
}

export class JsonDocument {
  constructor(
    readonly value: JsonValue,
    private readonly source: string,
    private readonly spans: WeakMap<object, Span>,
    private readonly properties: WeakMap<object, Map<string, Span>>,
  ) {}

  bytes(value: object): Uint8Array {
    const span = this.spans.get(value);
    if (!span) fail("JSON value has no source span", "image");
    return textEncoder.encode(this.source.slice(span.start, span.end));
  }

  replacingProperty(value: object, key: string, replacement: string): Uint8Array {
    const span = this.spans.get(value);
    const property = this.properties.get(value)?.get(key);
    if (!span || !property) fail(`JSON property ${key} has no source span`, `image.${key}`);
    return textEncoder.encode(
      this.source.slice(span.start, property.start) +
        replacement +
        this.source.slice(property.end, span.end),
    );
  }
}

export class VMError extends Error {
  constructor(
    message: string,
    readonly code = "invalid_image",
    readonly path = "image",
  ) {
    super(message);
  }
}

export function fail(message: string, path: string, code = "invalid_image"): never {
  throw new VMError(message, code, path);
}

export async function decodeJson(input: Uint8Array, maxBytes: number): Promise<JsonDocument> {
  let bytes = input;
  if (input[0] === 0x1f && input[1] === 0x8b) {
    if (typeof DecompressionStream !== "function")
      fail("gzip images are unavailable in this environment", "image");
    const owned = Uint8Array.from(input);
    const stream = new Blob([owned.buffer]).stream().pipeThrough(new DecompressionStream("gzip"));
    const reader = stream.getReader();
    const chunks: Uint8Array[] = [];
    let length = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      length += value.byteLength;
      if (length > maxBytes) {
        await reader.cancel();
        fail("image exceeds byte limit", "image", "load_limit");
      }
      chunks.push(value);
    }
    bytes = new Uint8Array(length);
    let offset = 0;
    for (const chunk of chunks) {
      bytes.set(chunk, offset);
      offset += chunk.byteLength;
    }
  }
  if (bytes.byteLength > maxBytes) fail("image exceeds byte limit", "image", "load_limit");
  let text: string;
  try {
    text = strictTextDecoder.decode(bytes);
  } catch {
    fail("image is not valid UTF-8", "image");
  }
  return new StrictJsonParser(text!).parse();
}

class StrictJsonParser {
  private offset = 0;
  private nodes = 0;
  private readonly spans = new WeakMap<object, Span>();
  private readonly properties = new WeakMap<object, Map<string, Span>>();

  constructor(private readonly source: string) {}

  parse(): JsonDocument {
    const value = this.value(0, "$", true);
    this.space();
    if (this.offset !== this.source.length) this.error("trailing JSON input");
    return new JsonDocument(value, this.source, this.spans, this.properties);
  }

  private value(depth: number, path: string, count: boolean): JsonValue {
    this.space();
    if (depth > 128) fail("JSON nesting exceeds limit", path, "load_limit");
    if (count && ++this.nodes > 2_000_000)
      fail("JSON node count exceeds limit", path, "load_limit");
    switch (this.source[this.offset]) {
      case "{":
        return this.object(depth + 1, path);
      case "[":
        return this.array(depth + 1, path);
      case '"':
        return this.string();
      case "t":
        return this.keyword("true", true);
      case "f":
        return this.keyword("false", false);
      case "n":
        return this.keyword("null", null);
      default:
        return this.number();
    }
  }

  private object(depth: number, path: string): { [key: string]: JsonValue } {
    const start = this.offset;
    this.offset++;
    const result: { [key: string]: JsonValue } = Object.create(null) as {
      [key: string]: JsonValue;
    };
    this.space();
    if (this.source[this.offset] === "}") {
      this.offset++;
      this.spans.set(result, { start, end: this.offset });
      return result;
    }
    const properties = new Map<string, Span>();
    this.properties.set(result, properties);
    for (;;) {
      if (this.source[this.offset] !== '"') this.error("expected object key");
      const key = this.string();
      if (Object.hasOwn(result, key)) fail(`duplicate object key ${key}`, `${path}.${key}`);
      this.space();
      if (this.source[this.offset++] !== ":") this.error("expected ':'");
      this.space();
      const valueStart = this.offset;
      result[key] = this.value(depth, `${path}.${key}`, true);
      properties.set(key, { start: valueStart, end: this.offset });
      this.space();
      const delimiter = this.source[this.offset++];
      if (delimiter === "}") {
        this.spans.set(result, { start, end: this.offset });
        return result;
      }
      if (delimiter !== ",") this.error("expected ',' or '}'");
      this.space();
    }
  }

  private array(depth: number, path: string): JsonValue[] {
    const start = this.offset;
    this.offset++;
    const result: JsonValue[] = [];
    this.space();
    if (this.source[this.offset] === "]") {
      this.offset++;
      this.spans.set(result, { start, end: this.offset });
      return result;
    }
    for (;;) {
      result.push(this.value(depth, `${path}[${result.length}]`, true));
      this.space();
      const delimiter = this.source[this.offset++];
      if (delimiter === "]") {
        this.spans.set(result, { start, end: this.offset });
        return result;
      }
      if (delimiter !== ",") this.error("expected ',' or ']'");
      this.space();
    }
  }

  private string(): string {
    const start = this.offset++;
    for (;;) {
      const code = this.source.charCodeAt(this.offset++);
      if (code === 0x22) break;
      if (code === 0x5c) {
        const escaped = this.source.charCodeAt(this.offset++);
        if (escaped === 0x75) {
          for (let index = 0; index < 4; index++) {
            const digit = this.source.charCodeAt(this.offset++);
            if (
              !(
                (digit >= 0x30 && digit <= 0x39) ||
                (digit >= 0x41 && digit <= 0x46) ||
                (digit >= 0x61 && digit <= 0x66)
              )
            )
              this.error("invalid Unicode escape");
          }
        } else if (![0x22, 0x5c, 0x2f, 0x62, 0x66, 0x6e, 0x72, 0x74].includes(escaped)) {
          this.error("invalid string escape");
        }
      } else if (!Number.isFinite(code) || code < 0x20) {
        this.error("invalid string");
      }
    }
    try {
      return JSON.parse(this.source.slice(start, this.offset)) as string;
    } catch {
      this.error("invalid string");
    }
  }

  private number(): JsonNumber {
    const rest = this.source.slice(this.offset);
    const match = /^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/.exec(rest);
    if (!match) this.error("expected JSON value");
    this.offset += match![0].length;
    return new JsonNumber(match![0]);
  }

  private keyword<T extends boolean | null>(word: string, value: T): T {
    if (!this.source.startsWith(word, this.offset)) this.error("invalid JSON value");
    this.offset += word.length;
    return value;
  }

  private space(): void {
    while (/\s/.test(this.source[this.offset] ?? "") && this.offset < this.source.length)
      this.offset++;
  }

  private error(message: string): never {
    fail(`${message} at byte ${this.offset}`, "image");
  }
}

export function object(value: JsonValue, path: string): Record<string, JsonValue> {
  if (!value || typeof value !== "object" || Array.isArray(value) || value instanceof JsonNumber)
    fail("expected object", path);
  return value as Record<string, JsonValue>;
}

export function array(value: JsonValue | undefined, path: string): JsonValue[] {
  if (value === undefined) return [];
  if (!Array.isArray(value)) fail("expected array", path);
  return value;
}

export function string(value: JsonValue | undefined, path: string, optional = false): string {
  if (value === undefined && optional) return "";
  if (typeof value !== "string") fail("expected string", path);
  return value;
}

export function integer(value: JsonValue | undefined, path: string, optional = false): number {
  if (value === undefined && optional) return 0;
  if (!(value instanceof JsonNumber)) fail("expected integer", path);
  const exact = value.integer(path);
  if (exact < BigInt(Number.MIN_SAFE_INTEGER) || exact > BigInt(Number.MAX_SAFE_INTEGER))
    fail("integer exceeds JavaScript index range", path);
  return Number(exact);
}

export function boolean(value: JsonValue | undefined, path: string): boolean {
  if (value === undefined) return false;
  if (typeof value !== "boolean") fail("expected boolean", path);
  return value;
}

export async function sha256Hex(value: Uint8Array): Promise<string> {
  const owned = Uint8Array.from(value);
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", owned.buffer));
  return [...digest].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}
