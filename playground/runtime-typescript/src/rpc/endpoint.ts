import type { RPCFailure, RPCWorkerStats } from "./protocol";
import { deferred, type Deferred } from "../deferred";
import type { RPCBindOptions, RPCContract, RPCMethod, RPCPublishOptions, RPCValue } from "./types";

const ENDPOINT_PROTOCOL = "minigo.rpc.endpoint.v12";
const HELLO = 1;
const READY = 2;
const BIND = 3;
const CALL = 4;
const DECISION = 5;
const DROP = 6;
const CLOSE = 7;
const CANCEL = 8;
const RENEW = 10;
const RENEW_ACK = 11;
const ACCEPTED = 12;
const OFFER = 13;
const DONE = 14;
const LEASE_BINDING = 1;
const LEASE_OPERATION = 2;
const HEADER = new Uint8Array([0x4d, 0x47, 0x52, 0x50, 1]);
const FRAGMENT_HEADER = new Uint8Array([0x4d, 0x47, 0x52, 0x46, 1]);
const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder("utf-8", { fatal: true });

interface Limits {
  maxFrameBytes: number;
  maxMessageBytes: number;
  maxInFlightBytes: number;
  maxBindings: number;
  maxPendingCalls: number;
  maxPendingResults: number;
  maxPendingControls: number;
  maxResources: number;
  maxMethods: number;
  maxValueDepth: number;
  maxValueElements: number;
}

const limits: Limits = {
  maxFrameBytes: 1 << 20,
  maxMessageBytes: 64 << 20,
  maxInFlightBytes: 128 << 20,
  maxBindings: 4096,
  maxPendingCalls: 65536,
  maxPendingResults: 65536,
  maxPendingControls: 4096,
  maxResources: 65536,
  maxMethods: 4096,
  maxValueDepth: 128,
  maxValueElements: 1_000_000,
};

interface ResourceReference {
  epoch: number;
  objectId: number;
  typeHash: string;
}

type WireData =
  | { kind: "nil" }
  | { kind: "bool"; value: boolean }
  | { kind: "int"; value: bigint }
  | { kind: "uint"; value: bigint }
  | { kind: "float"; value: number }
  | { kind: "complex"; value: readonly [number, number] }
  | { kind: "string"; value: string }
  | { kind: "bytes"; value: Uint8Array }
  | { kind: "slice"; value: readonly WireValue[] }
  | { kind: "map"; value: readonly { key: WireValue; value: WireValue }[] }
  | { kind: "struct"; value: readonly { id: number; value: WireValue }[] }
  | { kind: "optional"; value: WireValue }
  | { kind: "wireResource"; value: ResourceReference };

interface WireValue {
  type: string;
  data: WireData;
}

export type RPCResourceReference = ResourceReference;
export type RPCWireValue = WireValue;

interface WireCall {
  method: RPCMethod;
  receiver?: ResourceReference;
  arguments: Uint8Array;
}

interface Frame {
  kind: number;
  protocol: string;
  limits?: Limits;
  leaseTtl: bigint;
  admissionTimeout: bigint;
  maxCallDuration: bigint;
  origin: string;
  id: number;
  targetId: number;
  binding: number;
  reply: boolean;
  epoch: number;
  contract?: RPCContract;
  affinityKey: string;
  labels: Record<string, string>;
  hops: bigint;
  call?: WireCall;
  reference?: ResourceReference;
  values: Uint8Array;
  accept: boolean;
  code: string;
  message: string;
  timeout: bigint;
}

export interface RPCFfiRequest {
  version: string;
  operation: string;
  target: string;
  lease: number;
  contract: RPCContract;
  affinityKey: string;
  labels: Record<string, string>;
  method: RPCMethod;
  receiver?: RPCResourceReference;
  payload: Uint8Array;
  resource?: RPCResourceReference;
  requestId: number;
  code: string;
  message: string;
  timeoutNanos: bigint;
}

export interface RPCFfiResponse {
  version?: string;
  operation?: string;
  lease?: number;
  requestId?: number;
  method?: RPCMethod;
  receiver?: RPCResourceReference;
  payload?: Uint8Array;
  code?: string;
  message?: string;
}

function frame(kind: number): Frame {
  return {
    kind,
    protocol: "",
    leaseTtl: 0n,
    admissionTimeout: 0n,
    maxCallDuration: 0n,
    origin: "",
    id: 0,
    targetId: 0,
    binding: 0,
    reply: false,
    epoch: 0,
    affinityKey: "",
    labels: {},
    hops: 0n,
    values: new Uint8Array(),
    accept: false,
    code: "",
    message: "",
    timeout: 0n,
  };
}

class RPCError extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "RPCError";
  }
}

function status(reason: unknown, fallback = "unknown"): RPCError {
  if (reason instanceof RPCError) return reason;
  if (reason && typeof reason === "object") {
    const value = reason as { code?: unknown; message?: unknown };
    return new RPCError(
      typeof value.code === "string" ? value.code : fallback,
      String(value.message ?? reason),
    );
  }
  return new RPCError(fallback, String(reason));
}

class Encoder {
  private readonly bytes: number[] = [];

  fixed(value: Uint8Array): void {
    for (const byte of value) this.bytes.push(byte);
  }

  uint(input: number | bigint): void {
    let value = typeof input === "number" ? BigInt(input) : input;
    if (value < 0n || value > 0xffff_ffff_ffff_ffffn)
      throw new RPCError("invalid_argument", "RPC unsigned integer is out of range");
    while (value >= 128n) {
      this.bytes.push(Number(value & 127n) | 128);
      value >>= 7n;
    }
    this.bytes.push(Number(value));
  }

  int(value: bigint): void {
    if (value < -(1n << 63n) || value >= 1n << 63n)
      throw new RPCError("invalid_argument", "RPC signed integer is out of range");
    this.uint((value << 1n) ^ (value >> 63n));
  }

  bool(value: boolean): void {
    this.bytes.push(value ? 1 : 0);
  }

  raw(value: Uint8Array): void {
    this.uint(value.byteLength);
    this.fixed(value);
  }

  string(value: string): void {
    this.raw(textEncoder.encode(value));
  }

  float(value: number): void {
    const bytes = new Uint8Array(8);
    new DataView(bytes.buffer).setFloat64(0, value, true);
    this.fixed(bytes);
  }

  result(): Uint8Array {
    return Uint8Array.from(this.bytes);
  }
}

class Decoder {
  private offset = 0;

  constructor(
    private readonly data: Uint8Array,
    limit: number,
  ) {
    if (data.byteLength > limit) throw new RPCError("protocol", "RPC payload byte limit exceeded");
  }

  get remaining(): number {
    return this.data.byteLength - this.offset;
  }

  done(): void {
    if (this.remaining !== 0) throw new RPCError("protocol", "trailing RPC bytes");
  }

  fixed(count: number): Uint8Array {
    if (!Number.isSafeInteger(count) || count < 0 || count > this.remaining)
      throw new RPCError("protocol", "truncated RPC payload");
    const result = this.data.subarray(this.offset, this.offset + count);
    this.offset += count;
    return result;
  }

  uint(): bigint {
    let value = 0n;
    for (let index = 0; index < 10; index++) {
      if (!this.remaining) throw new RPCError("protocol", "truncated RPC varint");
      const byte = this.data[this.offset];
      if (byte === undefined) throw new RPCError("protocol", "truncated RPC varint");
      this.offset++;
      if (index === 9 && byte > 1) throw new RPCError("protocol", "overflowing RPC varint");
      value |= BigInt(byte & 127) << BigInt(index * 7);
      if (byte < 128) {
        if (index !== 0 && byte === 0) throw new RPCError("protocol", "non-canonical RPC varint");
        return value;
      }
    }
    throw new RPCError("protocol", "overflowing RPC varint");
  }

  number(): number {
    const value = this.uint();
    if (value > BigInt(Number.MAX_SAFE_INTEGER))
      throw new RPCError("protocol", "RPC identity exceeds JavaScript capacity");
    return Number(value);
  }

  int(): bigint {
    const value = this.uint();
    return (value >> 1n) ^ -(value & 1n);
  }

  bool(): boolean {
    const value = this.fixed(1)[0];
    if (value === undefined) throw new RPCError("protocol", "truncated RPC bool");
    if (value > 1) throw new RPCError("protocol", "invalid RPC bool");
    return value === 1;
  }

  raw(): Uint8Array {
    const size = this.number();
    return this.fixed(size);
  }

  string(): string {
    try {
      return textDecoder.decode(this.raw());
    } catch {
      throw new RPCError("protocol", "invalid RPC UTF-8");
    }
  }

  float(): number {
    const bytes = this.fixed(8);
    return new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength).getFloat64(0, true);
  }

  count(limit: number): number {
    const value = this.number();
    if (value > limit || value > this.remaining)
      throw new RPCError("protocol", "RPC element limit exceeded");
    return value;
  }
}

function encodeMethod(encoder: Encoder, method: RPCMethod): void {
  encoder.string(method.id);
  encoder.string(method.service);
  encoder.string(method.name);
  encoder.string(method.contractHash);
  encoder.string(method.resourceTypeHash);
}

function decodeMethod(decoder: Decoder): RPCMethod {
  return {
    id: decoder.string(),
    service: decoder.string(),
    name: decoder.string(),
    contractHash: decoder.string(),
    resourceTypeHash: decoder.string(),
  };
}

function encodeReference(encoder: Encoder, reference?: ResourceReference): void {
  encoder.bool(Boolean(reference));
  if (!reference) return;
  encoder.uint(reference.epoch);
  encoder.uint(reference.objectId);
  encoder.string(reference.typeHash);
}

function decodeReference(decoder: Decoder): ResourceReference | undefined {
  if (!decoder.bool()) return;
  const reference = {
    epoch: decoder.number(),
    objectId: decoder.number(),
    typeHash: decoder.string(),
  };
  if (!reference.epoch || !reference.objectId || reference.typeHash.length !== 64)
    throw new RPCError("protocol", "invalid RPC resource reference");
  return reference;
}

const emptyMethod: RPCMethod = {
  id: "",
  service: "",
  name: "",
  contractHash: "",
  resourceTypeHash: "",
};

export function decodeRPCFfiRequest(payload: Uint8Array): RPCFfiRequest {
  const decoder = new Decoder(payload, limits.maxMessageBytes);
  const version = decoder.string();
  const operation = decoder.string();
  const target = decoder.string();
  const lease = decoder.number();
  const protocol = decoder.string();
  const count = decoder.count(limits.maxMethods);
  const methods: RPCMethod[] = [];
  for (let index = 0; index < count; index++) methods.push(decodeMethod(decoder));
  const affinityKey = decoder.string();
  const labelCount = decoder.count(limits.maxMethods);
  const labels: Record<string, string> = {};
  for (let index = 0; index < labelCount; index++) {
    const key = decoder.string();
    if (!key || Object.hasOwn(labels, key)) throw new RPCError("protocol", "invalid RPC labels");
    labels[key] = decoder.string();
  }
  const method = decodeMethod(decoder);
  const receiver = decodeReference(decoder);
  const payloadBytes = Uint8Array.from(decoder.raw());
  const resource = decodeReference(decoder);
  const requestId = decoder.number();
  const code = decoder.string();
  const message = decoder.string();
  const timeoutNanos = decoder.int();
  const result: RPCFfiRequest = {
    version,
    operation,
    target,
    lease,
    contract: { protocol: protocol as RPCContract["protocol"], methods },
    affinityKey,
    labels,
    method,
    payload: payloadBytes,
    requestId,
    code,
    message,
    timeoutNanos,
  };
  if (receiver) result.receiver = receiver;
  if (resource) result.resource = resource;
  decoder.done();
  return result;
}

export function encodeRPCFfiResponse(response: RPCFfiResponse): Uint8Array {
  const encoder = new Encoder();
  encoder.string(response.version ?? "minigo.mrpc.ffi/v3");
  encoder.string(response.operation ?? "");
  encoder.uint(response.lease ?? 0);
  encoder.uint(response.requestId ?? 0);
  encodeMethod(encoder, response.method ?? emptyMethod);
  encodeReference(encoder, response.receiver);
  encoder.raw(response.payload ?? new Uint8Array());
  encoder.string(response.code ?? "");
  encoder.string(response.message ?? "");
  return encoder.result();
}

function limitValues(value: Limits): number[] {
  return [
    value.maxFrameBytes,
    value.maxMessageBytes,
    value.maxInFlightBytes,
    value.maxBindings,
    value.maxPendingCalls,
    value.maxPendingResults,
    value.maxPendingControls,
    value.maxResources,
    value.maxMethods,
    value.maxValueDepth,
    value.maxValueElements,
  ];
}

function encodeFrame(value: Frame, limit: number): Uint8Array {
  const encoder = new Encoder();
  encoder.fixed(HEADER);
  encoder.uint(value.kind);
  encoder.string(value.protocol);
  encoder.bool(Boolean(value.limits));
  if (value.limits) for (const item of limitValues(value.limits)) encoder.uint(item);
  encoder.int(value.leaseTtl);
  encoder.int(value.admissionTimeout);
  encoder.int(value.maxCallDuration);
  encoder.string(value.origin);
  encoder.uint(value.id);
  encoder.uint(value.targetId);
  encoder.uint(value.binding);
  encoder.bool(value.reply);
  encoder.uint(value.epoch);
  encoder.bool(Boolean(value.contract));
  if (value.contract) {
    encoder.string(value.contract.protocol);
    encoder.uint(value.contract.methods.length);
    for (const method of value.contract.methods) encodeMethod(encoder, method);
  }
  encoder.string(value.affinityKey);
  const labels = Object.entries(value.labels).sort(([left], [right]) => left.localeCompare(right));
  encoder.uint(labels.length);
  for (const [key, label] of labels) {
    encoder.string(key);
    encoder.string(label);
  }
  encoder.int(value.hops);
  encoder.bool(Boolean(value.call));
  if (value.call) {
    encodeMethod(encoder, value.call.method);
    encodeReference(encoder, value.call.receiver);
    encoder.raw(value.call.arguments);
  }
  encodeReference(encoder, value.reference);
  encoder.raw(value.values);
  encoder.bool(value.accept);
  encoder.string(value.code);
  encoder.string(value.message);
  encoder.int(value.timeout);
  const result = encoder.result();
  if (result.byteLength > limit)
    throw new RPCError("resource_exhausted", "RPC frame message limit exceeded");
  return result;
}

function decodeFrame(payload: Uint8Array): Frame {
  const decoder = new Decoder(payload, limits.maxMessageBytes);
  if (!equalBytes(decoder.fixed(5), HEADER))
    throw new RPCError("protocol", "invalid RPC frame header");
  const value = frame(decoder.number());
  value.protocol = decoder.string();
  if (decoder.bool()) {
    value.limits = {
      maxFrameBytes: decoder.number(),
      maxMessageBytes: decoder.number(),
      maxInFlightBytes: decoder.number(),
      maxBindings: decoder.number(),
      maxPendingCalls: decoder.number(),
      maxPendingResults: decoder.number(),
      maxPendingControls: decoder.number(),
      maxResources: decoder.number(),
      maxMethods: decoder.number(),
      maxValueDepth: decoder.number(),
      maxValueElements: decoder.number(),
    };
  }
  value.leaseTtl = decoder.int();
  value.admissionTimeout = decoder.int();
  value.maxCallDuration = decoder.int();
  value.origin = decoder.string();
  value.id = decoder.number();
  value.targetId = decoder.number();
  value.binding = decoder.number();
  value.reply = decoder.bool();
  value.epoch = decoder.number();
  if (decoder.bool()) {
    const protocol = decoder.string();
    const count = decoder.count(limits.maxMethods);
    value.contract = { protocol: protocol as RPCContract["protocol"], methods: [] };
    for (let index = 0; index < count; index++)
      (value.contract.methods as RPCMethod[]).push(decodeMethod(decoder));
  }
  value.affinityKey = decoder.string();
  const labels = decoder.count(limits.maxMethods);
  for (let index = 0; index < labels; index++) {
    const key = decoder.string();
    if (!key || Object.hasOwn(value.labels, key))
      throw new RPCError("protocol", "invalid RPC labels");
    value.labels[key] = decoder.string();
  }
  value.hops = decoder.int();
  if (decoder.bool()) {
    const method = decodeMethod(decoder);
    const receiver = decodeReference(decoder);
    const arguments_ = Uint8Array.from(decoder.raw());
    const call: WireCall = {
      method,
      arguments: arguments_,
    };
    if (receiver) call.receiver = receiver;
    value.call = call;
  }
  const reference = decodeReference(decoder);
  if (reference) value.reference = reference;
  value.values = Uint8Array.from(decoder.raw());
  value.accept = decoder.bool();
  value.code = decoder.string();
  value.message = decoder.string();
  value.timeout = decoder.int();
  decoder.done();
  if (!value.origin) throw new RPCError("protocol", "RPC frame has no origin");
  return value;
}

function equalBytes(left: Uint8Array, right: Uint8Array): boolean {
  return (
    left.byteLength === right.byteLength && left.every((value, index) => value === right[index])
  );
}

function encodeValue(
  encoder: Encoder,
  value: WireValue,
  depth: number,
  budget: { value: number },
): void {
  if (depth > limits.maxValueDepth || --budget.value < 0)
    throw new RPCError("resource_exhausted", "RPC value limit exceeded");
  encoder.string(value.type);
  switch (value.data.kind) {
    case "nil":
      encoder.uint(0);
      break;
    case "bool":
      encoder.uint(1);
      encoder.bool(value.data.value);
      break;
    case "int":
      encoder.uint(2);
      encoder.int(value.data.value);
      break;
    case "uint":
      encoder.uint(3);
      encoder.uint(value.data.value);
      break;
    case "float":
      encoder.uint(4);
      encoder.float(value.data.value);
      break;
    case "complex":
      encoder.uint(5);
      encoder.float(value.data.value[0]);
      encoder.float(value.data.value[1]);
      break;
    case "string":
      encoder.uint(6);
      encoder.string(value.data.value);
      break;
    case "bytes":
      encoder.uint(7);
      encoder.raw(value.data.value);
      break;
    case "slice":
      encoder.uint(8);
      encoder.uint(value.data.value.length);
      for (const item of value.data.value)
        encodeValue(encoder, item as WireValue, depth + 1, budget);
      break;
    case "map": {
      encoder.uint(9);
      encoder.uint(value.data.value.length);
      const entries = value.data.value.map((entry) => {
        const key = new Encoder();
        encodeValue(key, entry.key as WireValue, depth + 1, budget);
        return { key: key.result(), value: entry.value as WireValue };
      });
      entries.sort((left, right) => compareBytes(left.key, right.key));
      for (const entry of entries) {
        encoder.fixed(entry.key);
        encodeValue(encoder, entry.value, depth + 1, budget);
      }
      break;
    }
    case "struct":
      encoder.uint(10);
      encoder.uint(value.data.value.length);
      for (const field of [...value.data.value].sort((left, right) => left.id - right.id)) {
        encoder.uint(field.id);
        encodeValue(encoder, field.value as WireValue, depth + 1, budget);
      }
      break;
    case "optional":
      encoder.uint(11);
      encodeValue(encoder, value.data.value as WireValue, depth + 1, budget);
      break;
    case "wireResource":
      encoder.uint(12);
      encoder.uint(value.data.value.epoch);
      encoder.uint(value.data.value.objectId);
      encoder.string(value.data.value.typeHash);
      break;
  }
}

function compareBytes(left: Uint8Array, right: Uint8Array): number {
  for (let index = 0; index < Math.min(left.byteLength, right.byteLength); index++) {
    const leftByte = left[index];
    const rightByte = right[index];
    if (leftByte === undefined || rightByte === undefined)
      throw new RPCError("internal", "byte comparison index is unavailable");
    if (leftByte !== rightByte) return leftByte - rightByte;
  }
  return left.byteLength - right.byteLength;
}

function encodeValues(values: readonly WireValue[]): Uint8Array {
  const encoder = new Encoder();
  encoder.uint(values.length);
  const budget = { value: limits.maxValueElements };
  for (const value of values) encodeValue(encoder, value, 0, budget);
  const result = encoder.result();
  if (result.byteLength > limits.maxMessageBytes)
    throw new RPCError("resource_exhausted", "RPC encoded bytes limit exceeded");
  return result;
}

export function encodeRPCWireValues(values: readonly RPCWireValue[]): Uint8Array {
  return encodeValues(values);
}

function decodeValue(decoder: Decoder, depth: number, budget: { value: number }): WireValue {
  if (depth > limits.maxValueDepth || --budget.value < 0)
    throw new RPCError("protocol", "RPC value limit exceeded");
  const type = decoder.string();
  if (!type) throw new RPCError("protocol", "RPC value has no type");
  switch (decoder.number()) {
    case 0:
      return { type, data: { kind: "nil" } };
    case 1:
      return { type, data: { kind: "bool", value: decoder.bool() } };
    case 2:
      return { type, data: { kind: "int", value: decoder.int() } };
    case 3:
      return { type, data: { kind: "uint", value: decoder.uint() } };
    case 4:
      return { type, data: { kind: "float", value: decoder.float() } };
    case 5:
      return { type, data: { kind: "complex", value: [decoder.float(), decoder.float()] } };
    case 6:
      return { type, data: { kind: "string", value: decoder.string() } };
    case 7:
      return { type, data: { kind: "bytes", value: Uint8Array.from(decoder.raw()) } };
    case 8: {
      const count = decoder.count(budget.value);
      const value: WireValue[] = [];
      for (let index = 0; index < count; index++)
        value.push(decodeValue(decoder, depth + 1, budget));
      return { type, data: { kind: "slice", value } };
    }
    case 9: {
      const count = decoder.count(Math.floor(budget.value / 2));
      const value: Array<{ key: WireValue; value: WireValue }> = [];
      for (let index = 0; index < count; index++)
        value.push({
          key: decodeValue(decoder, depth + 1, budget),
          value: decodeValue(decoder, depth + 1, budget),
        });
      return { type, data: { kind: "map", value } };
    }
    case 10: {
      const count = decoder.count(budget.value);
      const value: Array<{ id: number; value: WireValue }> = [];
      const seen = new Set<number>();
      for (let index = 0; index < count; index++) {
        const id = decoder.number();
        if (!id || seen.has(id))
          throw new RPCError("protocol", "invalid or duplicate RPC field ID");
        seen.add(id);
        value.push({ id, value: decodeValue(decoder, depth + 1, budget) });
      }
      return { type, data: { kind: "struct", value } };
    }
    case 11:
      return { type, data: { kind: "optional", value: decodeValue(decoder, depth + 1, budget) } };
    case 12: {
      const reference = {
        epoch: decoder.number(),
        objectId: decoder.number(),
        typeHash: decoder.string(),
      };
      if (!reference.epoch || !reference.objectId || reference.typeHash.length !== 64)
        throw new RPCError("protocol", "invalid RPC resource value");
      return { type, data: { kind: "wireResource", value: reference } };
    }
    default:
      throw new RPCError("protocol", "unknown RPC value kind");
  }
}

function decodeValues(payload: Uint8Array): WireValue[] {
  const decoder = new Decoder(payload, limits.maxMessageBytes);
  const count = decoder.count(limits.maxValueElements);
  const budget = { value: limits.maxValueElements };
  const result: WireValue[] = [];
  for (let index = 0; index < count; index++) result.push(decodeValue(decoder, 0, budget));
  decoder.done();
  return result;
}

export function decodeRPCWireValues(payload: Uint8Array): RPCWireValue[] {
  return decodeValues(payload);
}

function encodeLeaseTargets(
  bindings: readonly number[],
  operations: readonly number[],
): Uint8Array {
  const encoder = new Encoder();
  encoder.uint(bindings.length + operations.length);
  for (const id of bindings) {
    encoder.fixed(Uint8Array.of(LEASE_BINDING));
    encoder.uint(id);
  }
  for (const id of operations) {
    encoder.fixed(Uint8Array.of(LEASE_OPERATION));
    encoder.uint(id);
  }
  return encoder.result();
}

function decodeLeaseTargets(payload: Uint8Array): { bindings: number[]; operations: number[] } {
  const decoder = new Decoder(payload, limits.maxMessageBytes);
  const count = decoder.count(limits.maxPendingControls);
  const bindings: number[] = [];
  const operations: number[] = [];
  const seen = new Set<string>();
  for (let index = 0; index < count; index++) {
    const kind = decoder.fixed(1)[0];
    const id = decoder.number();
    const key = `${kind}:${id}`;
    if (!id || seen.has(key) || (kind !== LEASE_BINDING && kind !== LEASE_OPERATION))
      throw new RPCError("protocol", "invalid RPC lease target");
    seen.add(key);
    (kind === LEASE_BINDING ? bindings : operations).push(id);
  }
  decoder.done();
  return { bindings, operations };
}

class Assembly {
  private previous = 0;
  private current = 0;
  private total = 0;
  private chunks: Uint8Array[] = [];
  private received = 0;

  push(payload: Uint8Array): Uint8Array | undefined {
    const decoder = new Decoder(payload, limits.maxFrameBytes);
    if (!equalBytes(decoder.fixed(5), FRAGMENT_HEADER))
      throw new RPCError("protocol", "invalid RPC fragment header");
    const id = decoder.number();
    const total = decoder.number();
    const offset = decoder.number();
    if (!total || total > limits.maxMessageBytes || offset > total || !decoder.remaining)
      throw new RPCError("protocol", "invalid RPC fragment bounds");
    const bytes = Uint8Array.from(decoder.fixed(decoder.remaining));
    if (bytes.byteLength > total - offset)
      throw new RPCError("protocol", "invalid RPC fragment bounds");
    if (id === 0) {
      if (offset !== 0 || bytes.byteLength !== total)
        throw new RPCError("protocol", "RPC control fragment must be complete");
      return bytes;
    }
    if (!this.current) {
      if (offset !== 0 || id !== this.previous + 1)
        throw new RPCError("protocol", "RPC fragment sequence is not strictly increasing");
      this.current = id;
      this.total = total;
    }
    if (id !== this.current || total !== this.total || offset !== this.received)
      throw new RPCError("protocol", "RPC fragment is not contiguous");
    this.chunks.push(bytes);
    this.received += bytes.byteLength;
    if (this.received !== total) return;
    const result = new Uint8Array(total);
    let cursor = 0;
    for (const chunk of this.chunks) {
      result.set(chunk, cursor);
      cursor += chunk.byteLength;
    }
    this.previous = id;
    this.current = 0;
    this.total = 0;
    this.chunks = [];
    this.received = 0;
    return result;
  }
}

interface PendingRequest {
  kind: number;
  binding: number;
  operation: number | undefined;
  resolve(value: Frame): void;
  reject(reason: unknown): void;
  timer?: ReturnType<typeof setTimeout>;
}

interface OutboundBinding {
  wire: number;
  epoch: number;
  contract: RPCContract;
  draining: boolean;
}

interface ClientResource {
  binding: number;
  reference: ResourceReference;
  active: boolean;
}

interface ClientResult {
  binding: number;
  call: number;
  resources: number[];
}

interface Publication {
  provider: number;
  contract: RPCContract;
  options: RPCPublishOptions;
  leases: number;
}

interface ProviderResource {
  local: number;
  reference: ResourceReference;
  active: boolean;
}

interface InboundBinding {
  publication: number;
  provider: number;
  providerName: string;
  contract: RPCContract;
  epoch: number;
  resources: Map<number, ProviderResource>;
}

interface ProviderCompletion {
  result: Deferred<{ values: readonly RPCValue[]; error?: RPCFailure }>;
  retained: Deferred<number[]>;
}

interface ResourceCompletion {
  result: Deferred<RPCFailure | undefined>;
}

export type RPCEndpointAction =
  | {
      kind: "providerCall";
      id: number;
      provider: number;
      resource?: number;
      method: string;
      arguments: RPCValue[];
      peer: { identity: string; attributes: Record<string, string> };
      providerName: string;
      timeoutMs?: number;
    }
  | { kind: "providerCancel"; id: number }
  | { kind: "resourceClose"; id: number; resource: number };

export interface RPCEndpointOptions {
  leaseTtlMs?: number;
  admissionTimeoutMs?: number;
  maxCallDurationMs?: number;
}

export class RPCEndpoint {
  private readonly origin = Array.from(crypto.getRandomValues(new Uint8Array(16)), (value) =>
    value.toString(16).padStart(2, "0"),
  ).join("");
  private readonly readyState = deferred<void>();
  private readonly assembly = new Assembly();
  private readonly outgoingFrames: Array<{ id: number; payload: number[] }> = [];
  private readonly actionsQueue: RPCEndpointAction[] = [];
  private readonly pending = new Map<number, PendingRequest>();
  private readonly operations = new Map<number, number>();
  private readonly outboundBindings = new Map<number, OutboundBinding>();
  private readonly clientResources = new Map<number, ClientResource>();
  private readonly clientResults = new Map<number, ClientResult>();
  private readonly publications = new Map<number, Publication>();
  private readonly inboundBindings = new Map<number, InboundBinding>();
  private readonly providerCalls = new Map<number, ProviderCompletion>();
  private readonly resourceCloses = new Map<number, ResourceCompletion>();
  private readonly inboundOperations = new Set<number>();
  private readonly publicationWaiters = new Set<() => void>();
  private peer = "";
  private peerReady = false;
  private nextRequest = 1;
  private nextWireMessage = 0;
  private nextIdentity = 1;
  private nextBinding = 1;
  private nextEpoch = 1;
  private closed = false;
  private maintenance?: ReturnType<typeof setInterval>;
  private readonly leaseTtlMs: number;
  private readonly admissionTimeoutMs: number;
  private readonly maxCallDurationMs: number | undefined;

  constructor(
    options: RPCEndpointOptions,
    private readonly wake: () => void,
    private readonly fatal: (reason: unknown) => void,
  ) {
    this.leaseTtlMs = options.leaseTtlMs ?? 60_000;
    this.admissionTimeoutMs = options.admissionTimeoutMs ?? 10_000;
    this.maxCallDurationMs = options.maxCallDurationMs;
    const hello = frame(HELLO);
    hello.protocol = ENDPOINT_PROTOCOL;
    hello.limits = limits;
    hello.leaseTtl = BigInt(this.leaseTtlMs) * 1_000_000n;
    hello.admissionTimeout = BigInt(this.admissionTimeoutMs) * 1_000_000n;
    hello.maxCallDuration = BigInt(this.maxCallDurationMs ?? 0) * 1_000_000n;
    this.send(hello);
  }

  get ready(): Promise<void> {
    return this.readyState.promise;
  }

  outgoing(): Array<{ id: number; payload: number[] }> {
    return this.outgoingFrames.splice(0);
  }

  sent(_id: number): void {}

  actions(): RPCEndpointAction[] {
    return this.actionsQueue.splice(0);
  }

  receive(fragment: Uint8Array): void {
    if (this.closed) return;
    try {
      const payload = this.assembly.push(fragment);
      if (!payload) return;
      const incoming = decodeFrame(payload);
      void this.handle(incoming).catch((reason) => this.fail(reason));
    } catch (reason) {
      this.fail(reason);
    }
  }

  disconnect(): void {
    this.fail(new RPCError("unavailable", "RPC WebSocket disconnected"));
  }

  private allocateIdentity(): number {
    if (this.nextIdentity >= 0xffff_ffff)
      throw new RPCError("resource_exhausted", "RPC endpoint identity exhausted");
    return this.nextIdentity++;
  }

  private send(outgoing: Frame): void {
    if (this.closed) throw new RPCError("unavailable", "RPC endpoint is closed");
    outgoing.origin = this.origin;
    const payload = encodeFrame(outgoing, limits.maxMessageBytes);
    const control = [ACCEPTED, RENEW, RENEW_ACK, CANCEL, DECISION, DONE].includes(outgoing.kind);
    const message =
      control && payload.byteLength <= limits.maxFrameBytes - 35 ? 0 : ++this.nextWireMessage;
    const chunkSize = limits.maxFrameBytes - 35;
    for (let offset = 0; offset < payload.byteLength; offset += chunkSize) {
      const chunk = payload.subarray(offset, offset + chunkSize);
      const encoder = new Encoder();
      encoder.fixed(FRAGMENT_HEADER);
      encoder.uint(message);
      encoder.uint(payload.byteLength);
      encoder.uint(offset);
      encoder.fixed(chunk);
      this.outgoingFrames.push({ id: this.allocateIdentity(), payload: [...encoder.result()] });
    }
    this.wake();
  }

  private async request(
    outgoing: Frame,
    operation?: number,
    timeoutMs?: number,
  ): Promise<{ id: number; reply: Frame }> {
    await this.ready;
    if (this.closed) throw new RPCError("unavailable", "RPC endpoint is closed");
    const id = outgoing.kind === DECISION ? outgoing.targetId : this.nextRequest++;
    outgoing.id = id;
    const pending = deferred<Frame>();
    const entry: PendingRequest = {
      kind: outgoing.kind,
      binding: outgoing.binding,
      operation,
      resolve: pending.resolve,
      reject: pending.reject,
    };
    if (operation !== undefined) {
      if (this.operations.has(operation))
        throw new RPCError("already_exists", "RPC operation identity is already active");
      this.operations.set(operation, id);
    }
    const duration = timeoutMs ?? (outgoing.kind === RENEW ? this.admissionTimeoutMs : undefined);
    if (duration !== undefined) {
      outgoing.timeout = BigInt(duration) * 1_000_000n;
      entry.timer = setTimeout(() => {
        if (!this.pending.delete(id)) return;
        if (operation !== undefined) this.operations.delete(operation);
        const cancel = frame(CANCEL);
        cancel.targetId = id;
        this.send(cancel);
        pending.reject(new RPCError("deadline_exceeded", "RPC deadline exceeded"));
      }, duration);
    }
    this.pending.set(id, entry);
    try {
      this.send(outgoing);
      return { id, reply: await pending.promise };
    } catch (reason) {
      if (this.pending.delete(id)) clearTimeout(entry.timer);
      throw reason;
    } finally {
      if (operation !== undefined) this.operations.delete(operation);
    }
  }

  cancel(operation: number): void {
    const request = this.operations.get(operation);
    if (!request) return;
    this.operations.delete(operation);
    const pending = this.pending.get(request);
    if (pending) {
      this.pending.delete(request);
      clearTimeout(pending.timer);
      pending.reject(new RPCError("canceled", "RPC call canceled"));
    }
    const cancel = frame(CANCEL);
    cancel.targetId = request;
    this.send(cancel);
  }

  async bind(
    operation: number,
    contract: RPCContract,
    options: Omit<RPCBindOptions, "signal">,
  ): Promise<number> {
    validateContract(contract);
    const request = frame(BIND);
    request.contract = contract;
    request.affinityKey = options.affinityKey ?? "";
    request.labels = { ...options.labels };
    const { reply } = await this.request(request, operation, options.timeoutMs);
    if (!reply.binding || !reply.epoch)
      throw new RPCError("protocol", "RPC peer returned an invalid binding");
    const id = this.allocateIdentity();
    this.outboundBindings.set(id, {
      wire: reply.binding,
      epoch: reply.epoch,
      contract,
      draining: false,
    });
    try {
      await this.renew([reply.binding], []);
    } catch (reason) {
      this.outboundBindings.delete(id);
      throw reason;
    }
    return id;
  }

  async invoke(
    operation: number,
    bindingId: number,
    method: RPCMethod,
    receiver: number | undefined,
    arguments_: readonly RPCValue[],
    timeoutMs?: number,
  ): Promise<{ result: number; values: RPCValue[] }> {
    const binding = this.outboundBindings.get(bindingId);
    if (!binding) throw new RPCError("unavailable", "RPC binding is closed");
    if (binding.draining && receiver === undefined)
      throw new RPCError("unavailable", "RPC binding is closed");
    const supported = binding.contract.methods.find((candidate) => candidate.id === method.id);
    if (!supported || !sameMethod(supported, method))
      throw new RPCError("unimplemented", "RPC method is not bound");
    const request = frame(CALL);
    request.binding = binding.wire;
    const receiverReference =
      receiver === undefined ? undefined : this.clientResource(bindingId, receiver);
    const wireCall: WireCall = {
      method,
      arguments: encodeValues(arguments_.map((value) => this.outboundValue(bindingId, value))),
    };
    if (receiverReference) wireCall.receiver = receiverReference;
    request.call = wireCall;
    const { id: call, reply } = await this.request(request, operation, timeoutMs);
    const resources: number[] = [];
    let values: RPCValue[];
    try {
      values = decodeValues(reply.values).map((value) =>
        this.inboundValue(bindingId, binding, value, resources),
      );
    } catch (reason) {
      await this.decide(binding.wire, call, false).catch(() => {});
      for (const resource of resources) this.clientResources.delete(resource);
      throw reason;
    }
    const result = this.allocateIdentity();
    this.clientResults.set(result, { binding: bindingId, call, resources });
    return { result, values };
  }

  async accept(result: number): Promise<void> {
    const pending = this.clientResults.get(result);
    if (!pending) throw new RPCError("not_found", "RPC result is unavailable");
    this.clientResults.delete(result);
    const binding = this.outboundBindings.get(pending.binding);
    if (!binding) throw new RPCError("unavailable", "RPC binding is closed");
    try {
      await this.decide(binding.wire, pending.call, true);
      for (const resource of pending.resources) {
        const entry = this.clientResources.get(resource);
        if (entry) entry.active = true;
      }
      await this.finalizeBinding(pending.binding);
    } catch (reason) {
      for (const resource of pending.resources) this.clientResources.delete(resource);
      throw reason;
    }
  }

  async discard(result: number): Promise<void> {
    const pending = this.clientResults.get(result);
    if (!pending) throw new RPCError("not_found", "RPC result is unavailable");
    this.clientResults.delete(result);
    const binding = this.outboundBindings.get(pending.binding);
    for (const resource of pending.resources) this.clientResources.delete(resource);
    if (binding) await this.decide(binding.wire, pending.call, false);
    await this.finalizeBinding(pending.binding);
  }

  private async decide(binding: number, call: number, accept: boolean): Promise<void> {
    const request = frame(DECISION);
    request.binding = binding;
    request.targetId = call;
    request.accept = accept;
    await this.request(request);
  }

  async closeBinding(bindingId: number): Promise<void> {
    const binding = this.outboundBindings.get(bindingId);
    if (!binding) return;
    binding.draining = true;
    await this.finalizeBinding(bindingId);
  }

  async closeResource(operation: number, resourceId: number, timeoutMs?: number): Promise<void> {
    const resource = this.clientResources.get(resourceId);
    if (!resource || !resource.active) throw new RPCError("not_found", "RPC resource is closed");
    const binding = this.outboundBindings.get(resource.binding);
    if (!binding) throw new RPCError("unavailable", "RPC binding is closed");
    const request = frame(DROP);
    request.binding = binding.wire;
    request.reference = resource.reference;
    await this.request(request, operation, timeoutMs);
    this.clientResources.delete(resourceId);
    await this.finalizeBinding(resource.binding);
  }

  publish(provider: number, contract: RPCContract, options: RPCPublishOptions): number {
    validateContract(contract);
    if (!provider) throw new RPCError("invalid_argument", "RPC provider identity must be nonzero");
    const id = this.allocateIdentity();
    this.publications.set(id, { provider, contract, options: { ...options }, leases: 0 });
    for (const notify of this.publicationWaiters) notify();
    this.publicationWaiters.clear();
    return id;
  }

  async closePublication(publication: number): Promise<void> {
    if (!this.publications.delete(publication))
      throw new RPCError("not_found", "RPC publication is closed");
    for (const [id, binding] of [...this.inboundBindings]) {
      if (binding.publication !== publication) continue;
      await this.releaseInboundBinding(id, binding);
    }
  }

  providerComplete(
    call: number,
    values: readonly RPCValue[],
    error?: RPCFailure,
  ): Promise<number[]> {
    const completion = this.providerCalls.get(call);
    if (!completion) return Promise.resolve([]);
    completion.result.resolve(error ? { values, error } : { values });
    return completion.retained.promise;
  }

  resourceComplete(close: number, error?: RPCFailure): void {
    const completion = this.resourceCloses.get(close);
    if (!completion) return;
    this.resourceCloses.delete(close);
    completion.result.resolve(error);
  }

  stats(): RPCWorkerStats {
    return {
      bindings: BigInt(this.outboundBindings.size),
      resources: BigInt(this.clientResources.size),
      pendingResults: BigInt(this.clientResults.size),
      publications: BigInt(this.publications.size),
      pendingCalls: BigInt(this.pending.size),
      inboundCalls: BigInt(this.providerCalls.size),
    };
  }

  decodeClientValues(bindingId: number, payload: Uint8Array): RPCValue[] {
    const binding = this.outboundBindings.get(bindingId);
    if (!binding) throw new RPCError("unavailable", "RPC binding is closed");
    return decodeValues(payload).map((value) => {
      if (value.data.kind !== "wireResource") return value as RPCValue;
      const id = this.findClientResource(bindingId, value.data.value);
      return { type: value.type, data: { kind: "resource", value: id } };
    });
  }

  encodeClientValues(bindingId: number, values: readonly RPCValue[]): Uint8Array {
    if (!this.outboundBindings.has(bindingId))
      throw new RPCError("unavailable", "RPC binding is closed");
    return encodeValues(
      values.map((value) => {
        if (value.data.kind !== "resource") return value as WireValue;
        if (typeof value.data.value !== "number")
          throw new RPCError("invalid_argument", "invalid RPC resource identity");
        const resource = this.clientResources.get(value.data.value);
        if (!resource || resource.binding !== bindingId)
          throw new RPCError("invalid_argument", "RPC resource belongs to another binding");
        return { type: value.type, data: { kind: "wireResource", value: resource.reference } };
      }),
    );
  }

  resolveClientReference(bindingId: number, reference: RPCResourceReference): number {
    return this.findClientResource(bindingId, reference);
  }

  async shutdown(): Promise<void> {
    if (this.closed) return;
    const failures: unknown[] = [];
    for (const id of [...this.outboundBindings.keys()])
      await this.closeBinding(id).catch((reason) => failures.push(reason));
    for (const [id, binding] of [...this.inboundBindings])
      await this.releaseInboundBinding(id, binding).catch((reason) => failures.push(reason));
    this.closed = true;
    clearInterval(this.maintenance);
    if (failures.length) throw failures[0];
  }

  private clientResource(binding: number, id: number): ResourceReference {
    const resource = this.clientResources.get(id);
    if (!resource || resource.binding !== binding || !resource.active)
      throw new RPCError(
        "invalid_argument",
        "RPC resource is closed or belongs to another binding",
      );
    return resource.reference;
  }

  private findClientResource(binding: number, reference: ResourceReference): number {
    for (const [id, resource] of this.clientResources) {
      if (
        resource.binding === binding &&
        resource.active &&
        resource.reference.epoch === reference.epoch &&
        resource.reference.objectId === reference.objectId &&
        resource.reference.typeHash === reference.typeHash
      )
        return id;
    }
    throw new RPCError("invalid_argument", "RPC resource is closed or belongs to another binding");
  }

  private async finalizeBinding(bindingId: number): Promise<void> {
    const binding = this.outboundBindings.get(bindingId);
    if (
      !binding?.draining ||
      [...this.clientResources.values()].some((resource) => resource.binding === bindingId) ||
      [...this.clientResults.values()].some((result) => result.binding === bindingId)
    )
      return;
    const request = frame(CLOSE);
    request.binding = binding.wire;
    await this.request(request);
    if (this.outboundBindings.get(bindingId) === binding) this.outboundBindings.delete(bindingId);
  }

  private outboundValue(binding: number, value: RPCValue): WireValue {
    if (value.data.kind === "resource") {
      if (typeof value.data.value !== "number")
        throw new RPCError("invalid_argument", "invalid RPC resource identity");
      const reference = this.clientResource(binding, value.data.value);
      if (reference.typeHash !== value.type)
        throw new RPCError("invalid_argument", "RPC resource type does not match");
      return { type: value.type, data: { kind: "wireResource", value: reference } };
    }
    if (value.data.kind === "localResource")
      throw new RPCError("invalid_argument", "provider resource cannot be used by an RPC client");
    return value as WireValue;
  }

  private inboundValue(
    bindingId: number,
    binding: OutboundBinding,
    value: WireValue,
    created: number[],
  ): RPCValue {
    if (value.data.kind !== "wireResource") return value as RPCValue;
    const reference = value.data.value;
    if (reference.epoch !== binding.epoch || reference.typeHash !== value.type)
      throw new RPCError("protocol", "RPC reply contains invalid resource identity");
    for (const [id, resource] of this.clientResources) {
      if (
        resource.binding === bindingId &&
        resource.reference.objectId === reference.objectId &&
        resource.reference.epoch === reference.epoch
      )
        return { type: value.type, data: { kind: "resource", value: id } };
    }
    if (this.clientResources.size >= limits.maxResources)
      throw new RPCError("resource_exhausted", "RPC resource limit exceeded");
    const id = this.allocateIdentity();
    this.clientResources.set(id, { binding: bindingId, reference, active: false });
    created.push(id);
    return { type: value.type, data: { kind: "resource", value: id } };
  }

  private async renew(bindings: number[], operations: number[]): Promise<void> {
    if (!bindings.length && !operations.length) return;
    const request = frame(RENEW);
    request.values = encodeLeaseTargets(bindings, operations);
    const { reply } = await this.request(request, undefined, this.admissionTimeoutMs);
    const confirmed = decodeLeaseTargets(reply.values);
    const requestedBindings = new Set(bindings);
    const requestedOperations = new Set(operations);
    if (
      confirmed.bindings.some((id) => !requestedBindings.has(id)) ||
      confirmed.operations.some((id) => !requestedOperations.has(id))
    )
      throw new RPCError("protocol", "RPC peer renewed unexpected ownership");
    const activeBindings = new Set(
      [...this.outboundBindings.values()].map((binding) => binding.wire),
    );
    const activeOperations = new Set([...this.clientResults.values()].map((result) => result.call));
    const confirmedBindings = new Set(confirmed.bindings);
    const confirmedOperations = new Set(confirmed.operations);
    if (
      bindings.some((id) => activeBindings.has(id) && !confirmedBindings.has(id)) ||
      operations.some((id) => activeOperations.has(id) && !confirmedOperations.has(id))
    )
      throw new RPCError("unavailable", "RPC peer did not renew active ownership");
  }

  private startMaintenance(): void {
    if (this.maintenance) return;
    this.maintenance = setInterval(
      () => {
        const bindings = [...this.outboundBindings.values()].map((binding) => binding.wire);
        const operations = [...this.clientResults.values()].map((result) => result.call);
        void this.renew(bindings, operations).catch((reason) => this.fail(reason));
      },
      Math.max(100, Math.floor(this.leaseTtlMs / 3)),
    );
  }

  private async handle(incoming: Frame): Promise<void> {
    if (incoming.kind === HELLO) {
      if (incoming.protocol !== ENDPOINT_PROTOCOL || this.peer || incoming.origin === this.origin)
        throw new RPCError("protocol", "invalid or duplicate RPC hello");
      this.peer = incoming.origin;
      const ready = frame(READY);
      this.send(ready);
      return;
    }
    if (!this.peer || incoming.origin !== this.peer)
      throw new RPCError("protocol", "RPC peer origin changed");
    if (incoming.kind === READY) {
      if (this.peerReady) throw new RPCError("protocol", "duplicate RPC ready");
      this.peerReady = true;
      this.readyState.resolve();
      this.startMaintenance();
      return;
    }
    if (incoming.reply) {
      const pending = this.pending.get(incoming.targetId);
      if (!pending) return;
      if (incoming.kind === ACCEPTED) return;
      this.pending.delete(incoming.targetId);
      clearTimeout(pending.timer);
      const compatible =
        (pending.kind === RENEW && incoming.kind === RENEW_ACK) ||
        (pending.kind === CALL &&
          (incoming.kind === OFFER || (incoming.kind === DONE && Boolean(incoming.code)))) ||
        ([BIND, DROP, CLOSE, DECISION].includes(pending.kind) && incoming.kind === DONE);
      if (!compatible || (pending.kind !== BIND && pending.binding !== incoming.binding)) {
        pending.reject(new RPCError("protocol", "RPC response kind mismatch"));
        return;
      }
      if (incoming.code) pending.reject(new RPCError(incoming.code, incoming.message));
      else pending.resolve(incoming);
      return;
    }
    if (incoming.kind === CANCEL) {
      if (this.inboundOperations.has(incoming.targetId)) {
        this.inboundOperations.delete(incoming.targetId);
        const completion = this.providerCalls.get(incoming.targetId);
        if (completion) {
          this.actionsQueue.push({ kind: "providerCancel", id: incoming.targetId });
          this.wake();
        }
      }
      return;
    }
    void this.dispatch(incoming);
  }

  private async dispatch(incoming: Frame): Promise<void> {
    const reply = frame(
      incoming.kind === CALL ? OFFER : incoming.kind === RENEW ? RENEW_ACK : DONE,
    );
    reply.reply = true;
    reply.targetId = incoming.id;
    reply.binding = incoming.binding;
    if ([BIND, CALL, DROP, CLOSE, DECISION].includes(incoming.kind)) {
      const accepted = frame(ACCEPTED);
      accepted.reply = true;
      accepted.targetId = incoming.id;
      accepted.binding = incoming.binding;
      this.send(accepted);
    }
    try {
      switch (incoming.kind) {
        case BIND:
          await this.handleBind(incoming, reply);
          break;
        case CALL:
          await this.handleCall(incoming, reply);
          break;
        case DECISION:
          await this.handleDecision(incoming, reply);
          break;
        case DROP:
          await this.handleDrop(incoming);
          break;
        case CLOSE:
          await this.handleClose(incoming);
          break;
        case RENEW:
          this.handleRenew(incoming, reply);
          break;
        default:
          throw new RPCError("protocol", "unexpected RPC request");
      }
    } catch (reason) {
      const failure = status(reason, "internal");
      reply.code = failure.code;
      reply.message = failure.message;
    }
    this.send(reply);
  }

  private async handleBind(incoming: Frame, reply: Frame): Promise<void> {
    if (!incoming.contract) throw new RPCError("protocol", "RPC bind has no contract");
    const contract = incoming.contract;
    validateContract(contract);
    const deadline = performance.now() + this.admissionTimeoutMs;
    let selected: [number, Publication] | undefined;
    do {
      selected = [...this.publications.entries()]
        .filter(([, publication]) => supports(publication.contract, contract))
        .filter(([, publication]) =>
          Object.entries(incoming.labels).every(
            ([key, value]) => publication.options.labels?.[key] === value,
          ),
        )
        .filter(
          ([, publication]) =>
            !publication.options.maxLeases || publication.leases < publication.options.maxLeases,
        )
        .sort(
          (left, right) => (right[1].options.priority ?? 0) - (left[1].options.priority ?? 0),
        )[0];
      if (selected) break;
      const remaining = deadline - performance.now();
      if (remaining <= 0) break;
      await new Promise<void>((resolve) => {
        const done = () => {
          clearTimeout(timer);
          this.publicationWaiters.delete(done);
          resolve();
        };
        const timer = setTimeout(done, remaining);
        this.publicationWaiters.add(done);
      });
    } while (!this.closed);
    if (!selected) throw new RPCError("unavailable", "no RPC provider satisfies the contract");
    const [publicationId, publication] = selected;
    const binding = this.nextBinding++;
    const epoch = this.nextEpoch++;
    publication.leases++;
    this.inboundBindings.set(binding, {
      publication: publicationId,
      provider: publication.provider,
      providerName: publication.options.name ?? "",
      contract,
      epoch,
      resources: new Map(),
    });
    reply.binding = binding;
    reply.epoch = epoch;
    reply.leaseTtl = BigInt(this.leaseTtlMs) * 1_000_000n;
  }

  private async handleCall(incoming: Frame, reply: Frame): Promise<void> {
    const binding = this.inboundBindings.get(incoming.binding);
    if (!binding || !incoming.call) throw new RPCError("not_found", "RPC binding is closed");
    const method = binding.contract.methods.find(
      (candidate) => candidate.id === incoming.call!.method.id,
    );
    if (!method || !sameMethod(method, incoming.call.method))
      throw new RPCError("unimplemented", "RPC method is not bound");
    const resource = incoming.call.receiver
      ? this.providerResource(binding, incoming.call.receiver).local
      : undefined;
    const arguments_ = decodeValues(incoming.call.arguments).map((value) =>
      this.providerArgument(binding, value),
    );
    const call = incoming.id;
    const completion: ProviderCompletion = { result: deferred(), retained: deferred() };
    this.providerCalls.set(call, completion);
    this.inboundOperations.add(call);
    const timeoutMs = durationMilliseconds(incoming.timeout, this.maxCallDurationMs);
    const action: Extract<RPCEndpointAction, { kind: "providerCall" }> = {
      kind: "providerCall",
      id: call,
      provider: binding.provider,
      method: resource === undefined ? incoming.call.method.id : incoming.call.method.name,
      arguments: arguments_,
      peer: { identity: "", attributes: {} },
      providerName: binding.providerName,
    };
    if (resource !== undefined) action.resource = resource;
    if (timeoutMs !== undefined) action.timeoutMs = timeoutMs;
    this.actionsQueue.push(action);
    this.wake();
    const completed = await completion.result.promise;
    if (completed.error) {
      this.providerCalls.delete(call);
      completion.retained.resolve([]);
      this.inboundOperations.delete(call);
      throw new RPCError(completed.error.code ?? "unknown", completed.error.message);
    }
    const delivered: number[] = [];
    const values = completed.values.map((value) => this.providerResult(binding, value, delivered));
    reply.values = encodeValues(values);
    reply.targetId = call;
    reply.leaseTtl = BigInt(this.leaseTtlMs) * 1_000_000n;
    (completion as ProviderCompletion & { delivered?: number[] }).delivered = delivered;
  }

  private async handleDecision(incoming: Frame, reply: Frame): Promise<void> {
    const completion = this.providerCalls.get(incoming.targetId) as
      | (ProviderCompletion & { delivered?: number[] })
      | undefined;
    if (!completion) throw new RPCError("not_found", "RPC result decision is stale");
    this.providerCalls.delete(incoming.targetId);
    this.inboundOperations.delete(incoming.targetId);
    const delivered = completion.delivered ?? [];
    const binding = this.inboundBindings.get(incoming.binding);
    if (binding) {
      for (const resource of delivered) {
        const entry = [...binding.resources.values()].find(
          (candidate) => candidate.local === resource,
        );
        if (entry && incoming.accept) entry.active = true;
        else if (entry) binding.resources.delete(entry.reference.objectId);
      }
    }
    completion.retained.resolve(incoming.accept ? delivered : []);
    reply.kind = DONE;
  }

  private async handleDrop(incoming: Frame): Promise<void> {
    const binding = this.inboundBindings.get(incoming.binding);
    if (!binding || !incoming.reference) throw new RPCError("not_found", "RPC binding is closed");
    const resource = this.providerResource(binding, incoming.reference);
    if (!resource.active) throw new RPCError("invalid_argument", "RPC resource is provisional");
    await this.closeProviderResource(resource.local);
    binding.resources.delete(resource.reference.objectId);
  }

  private async handleClose(incoming: Frame): Promise<void> {
    const binding = this.inboundBindings.get(incoming.binding);
    if (!binding) return;
    await this.releaseInboundBinding(incoming.binding, binding);
  }

  private handleRenew(incoming: Frame, reply: Frame): void {
    const requested = decodeLeaseTargets(incoming.values);
    reply.values = encodeLeaseTargets(
      requested.bindings.filter((id) => this.inboundBindings.has(id)),
      requested.operations.filter((id) => this.inboundOperations.has(id)),
    );
    reply.leaseTtl = BigInt(this.leaseTtlMs) * 1_000_000n;
  }

  private providerResource(
    binding: InboundBinding,
    reference: ResourceReference,
  ): ProviderResource {
    const resource = binding.resources.get(reference.objectId);
    if (
      !resource ||
      reference.epoch !== binding.epoch ||
      resource.reference.typeHash !== reference.typeHash
    )
      throw new RPCError(
        "invalid_argument",
        "RPC resource is closed or belongs to another binding",
      );
    return resource;
  }

  private providerArgument(binding: InboundBinding, value: WireValue): RPCValue {
    if (value.data.kind !== "wireResource") return value as RPCValue;
    const resource = this.providerResource(binding, value.data.value);
    if (!resource.active) throw new RPCError("invalid_argument", "RPC resource is provisional");
    return { type: value.type, data: { kind: "localResource", value: resource.local } };
  }

  private providerResult(binding: InboundBinding, value: RPCValue, delivered: number[]): WireValue {
    if (value.data.kind === "resource")
      throw new RPCError("invalid_argument", "remote resources cannot be returned by a provider");
    if (value.data.kind !== "localResource") return value as WireValue;
    if (typeof value.data.value !== "number" || !value.data.value)
      throw new RPCError("invalid_argument", "invalid provider resource identity");
    const reference = {
      epoch: binding.epoch,
      objectId: this.allocateIdentity(),
      typeHash: value.type,
    };
    binding.resources.set(reference.objectId, {
      local: value.data.value,
      reference,
      active: false,
    });
    delivered.push(value.data.value);
    return { type: value.type, data: { kind: "wireResource", value: reference } };
  }

  private async closeProviderResource(resource: number): Promise<void> {
    const close = this.allocateIdentity();
    const completion: ResourceCompletion = { result: deferred() };
    this.resourceCloses.set(close, completion);
    this.actionsQueue.push({ kind: "resourceClose", id: close, resource });
    this.wake();
    const failure = await completion.result.promise;
    if (failure) throw new RPCError(failure.code ?? "unknown", failure.message);
  }

  private async releaseInboundBinding(id: number, binding: InboundBinding): Promise<void> {
    this.inboundBindings.delete(id);
    const publication = this.publications.get(binding.publication);
    if (publication && publication.leases) publication.leases--;
    const failures: unknown[] = [];
    for (const resource of binding.resources.values())
      await this.closeProviderResource(resource.local).catch((reason) => failures.push(reason));
    binding.resources.clear();
    if (failures.length) throw failures[0];
  }

  private fail(reason: unknown): void {
    if (this.closed) return;
    this.closed = true;
    clearInterval(this.maintenance);
    const failure = status(reason, "unavailable");
    this.readyState.reject(failure);
    for (const notify of this.publicationWaiters) notify();
    this.publicationWaiters.clear();
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer);
      pending.reject(failure);
    }
    this.pending.clear();
    for (const [id, completion] of this.providerCalls) {
      this.actionsQueue.push({ kind: "providerCancel", id });
      completion.result.reject(failure);
      completion.retained.resolve([]);
    }
    this.providerCalls.clear();
    for (const completion of this.resourceCloses.values()) completion.result.reject(failure);
    this.resourceCloses.clear();
    this.wake();
    this.fatal(failure);
  }
}

function validateContract(contract: RPCContract): void {
  if (contract?.protocol !== "minigo.rpc.contract.v2" || !Array.isArray(contract.methods))
    throw new RPCError("invalid_argument", "invalid RPC contract");
  const ids = new Set<string>();
  for (const method of contract.methods) {
    if (
      !method?.id ||
      !method.service ||
      !method.name ||
      method.contractHash.length !== 64 ||
      (method.resourceTypeHash && method.resourceTypeHash.length !== 64) ||
      ids.has(method.id)
    )
      throw new RPCError("invalid_argument", "invalid RPC contract method");
    ids.add(method.id);
  }
}

function sameMethod(left: RPCMethod, right: RPCMethod): boolean {
  return (
    left.id === right.id &&
    left.service === right.service &&
    left.name === right.name &&
    left.contractHash === right.contractHash &&
    left.resourceTypeHash === right.resourceTypeHash
  );
}

function supports(provider: RPCContract, requested: RPCContract): boolean {
  return requested.methods.every((method) => {
    const candidate = provider.methods.find((value) => value.id === method.id);
    return candidate !== undefined && sameMethod(candidate, method);
  });
}

function durationMilliseconds(timeout: bigint, maximum?: number): number | undefined {
  let value = timeout > 0n ? Number(timeout) / 1_000_000 : undefined;
  if (maximum !== undefined && (value === undefined || maximum < value)) value = maximum;
  return value;
}
