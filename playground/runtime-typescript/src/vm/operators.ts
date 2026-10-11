import { Kind, PrimitiveKind } from "./contract.generated.js";
import { fail } from "./json.js";
import {
  boolValue,
  cell,
  cloneValue,
  dereference,
  guestEqual,
  normalize,
  primitive,
  type RuntimeType,
  type SliceValue,
  type Value,
} from "./value.js";

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

export function unary(operator: string, operand: Value): Value {
  const value = cloneValue(operand);
  switch (operator) {
    case "+":
      if (["int", "uint", "float", "complex"].includes(value.kind)) return value;
      break;
    case "-":
      if (value.kind === "int") value.value = -(value.value as bigint);
      else if (value.kind === "uint") value.value = -(value.value as bigint);
      else if (value.kind === "float") value.value = -(value.value as number);
      else if (value.kind === "complex") {
        const complex = value.value as { real: number; imag: number };
        value.value = { real: -complex.real, imag: -complex.imag };
      } else break;
      return normalize(value);
    case "!":
      if (value.kind === "bool") return { ...value, value: !(value.value as boolean) };
      break;
    case "^":
      if (value.kind === "int" || value.kind === "uint") {
        value.value = ~(value.value as bigint);
        return normalize(value);
      }
      break;
    case "real":
    case "imag":
      if (value.kind === "complex") {
        const complex = value.value as { real: number; imag: number };
        const underlying = value.type.registry.underlying(value.type);
        return normalize({
          type: primitive(
            value.type.registry,
            underlying.ref.primitive === PrimitiveKind.PrimitiveComplex64
              ? PrimitiveKind.PrimitiveFloat32
              : PrimitiveKind.PrimitiveFloat64,
          ),
          kind: "float",
          value: operator === "real" ? complex.real : complex.imag,
        });
      }
  }
  fail("operand type does not support operator", operator, "invalid_operation");
}

export function binary(operator: string, left: Value, right: Value): Value {
  const registry = left.type.registry;
  if (operator === "==" || operator === "!=")
    return boolValue(registry, guestEqual(left, right) === (operator === "=="));
  if (operator === "complex" && left.kind === "float" && right.kind === "float") {
    const underlying = registry.underlying(left.type);
    return normalize({
      type: primitive(
        registry,
        underlying.ref.primitive === PrimitiveKind.PrimitiveFloat32
          ? PrimitiveKind.PrimitiveComplex64
          : PrimitiveKind.PrimitiveComplex128,
      ),
      kind: "complex",
      value: { real: left.value as number, imag: right.value as number },
    });
  }
  if (operator === "<<" || operator === ">>") {
    if (!(["int", "uint"].includes(left.kind) && ["int", "uint"].includes(right.kind)))
      invalid(operator);
    const shift = right.value as bigint;
    if (shift < 0n) fail("negative shift count", operator, "panic");
    let result: bigint;
    if (shift >= 64n)
      result = operator === ">>" && left.kind === "int" && (left.value as bigint) < 0n ? -1n : 0n;
    else
      result =
        operator === "<<" ? (left.value as bigint) << shift : (left.value as bigint) >> shift;
    return normalize({ ...left, value: result });
  }
  if (left.kind === "bool" && right.kind === "bool") {
    if (operator === "&&")
      return boolValue(registry, (left.value as boolean) && (right.value as boolean));
    if (operator === "||")
      return boolValue(registry, (left.value as boolean) || (right.value as boolean));
    invalid(operator);
  }
  if (left.kind === "string" && right.kind === "string") {
    const a = left.value as Uint8Array;
    const b = right.value as Uint8Array;
    if (operator === "+") {
      const joined = new Uint8Array(a.length + b.length);
      joined.set(a);
      joined.set(b, a.length);
      return { ...left, value: joined };
    }
    const comparison = compareBytes(a, b);
    return boolValue(registry, comparisonResult(operator, comparison));
  }
  if (left.kind === "complex" && right.kind === "complex") {
    const a = left.value as { real: number; imag: number };
    const b = right.value as { real: number; imag: number };
    let real: number, imag: number;
    if (operator === "+") [real, imag] = [a.real + b.real, a.imag + b.imag];
    else if (operator === "-") [real, imag] = [a.real - b.real, a.imag - b.imag];
    else if (operator === "*")
      [real, imag] = [a.real * b.real - a.imag * b.imag, a.real * b.imag + a.imag * b.real];
    else if (operator === "/") [real, imag] = complexDivide(a.real, a.imag, b.real, b.imag);
    else invalid(operator);
    return normalize({ ...left, value: { real: real!, imag: imag! } });
  }
  if (left.kind === "float" && right.kind === "float") {
    const a = left.value as number;
    const b = right.value as number;
    if (operator === "<") return boolValue(registry, a < b);
    if (operator === "<=") return boolValue(registry, a <= b);
    if (operator === ">") return boolValue(registry, a > b);
    if (operator === ">=") return boolValue(registry, a >= b);
    const result =
      operator === "+"
        ? a + b
        : operator === "-"
          ? a - b
          : operator === "*"
            ? a * b
            : operator === "/"
              ? a / b
              : undefined;
    if (result === undefined) invalid(operator);
    return normalize({ ...left, value: result });
  }
  if ((left.kind === "int" || left.kind === "uint") && left.kind === right.kind) {
    const a = left.value as bigint;
    const b = right.value as bigint;
    if (["<", "<=", ">", ">="].includes(operator))
      return boolValue(registry, comparisonResult(operator, a < b ? -1 : a > b ? 1 : 0));
    if ((operator === "/" || operator === "%") && b === 0n)
      fail("integer divide by zero", operator, "panic");
    let result: bigint;
    switch (operator) {
      case "+":
        result = a + b;
        break;
      case "-":
        result = a - b;
        break;
      case "*":
        result = a * b;
        break;
      case "/":
        result = a / b;
        break;
      case "%":
        result = a % b;
        break;
      case "&":
        result = a & b;
        break;
      case "|":
        result = a | b;
        break;
      case "^":
        result = a ^ b;
        break;
      case "&^":
        result = a & ~b;
        break;
      default:
        invalid(operator);
    }
    return normalize({ ...left, value: result! });
  }
  invalid(operator);
}

export function convert(value: Value, type: RuntimeType): Value {
  const target = type.registry.underlying(type);
  if (target.ref.kind === Kind.Any) return value;
  if (type.registry.identical(value.type, type)) return { ...value, type };
  const targetElement = target.registry.element(target);
  if (target.ref.kind === Kind.Slice && targetElement && value.kind === "string") {
    const element = targetElement.registry.underlying(targetElement);
    let backing: Value;
    let length: number;
    if (
      element.ref.kind === Kind.Primitive &&
      element.ref.primitive === PrimitiveKind.PrimitiveUint8
    ) {
      const bytes = Uint8Array.from(value.value as Uint8Array);
      backing = { type, kind: "bytes", value: bytes };
      length = bytes.length;
    } else if (
      element.ref.kind === Kind.Primitive &&
      element.ref.primitive === PrimitiveKind.PrimitiveInt32
    ) {
      const text = textDecoder.decode(value.value as Uint8Array);
      const runes = [...text].map((rune) => ({
        type: targetElement,
        kind: "int" as const,
        value: BigInt(rune.codePointAt(0)!),
      }));
      backing = { type, kind: "array", value: runes };
      length = runes.length;
    } else invalid("convert");
    return {
      type,
      kind: "slice",
      value: {
        storage: { root: cell(backing!), path: [] },
        start: 0,
        length: length!,
        capacity: length!,
      } satisfies SliceValue,
    };
  }
  if (target.ref.kind === Kind.Primitive) {
    const primitiveKind = target.ref.primitive;
    if (
      primitiveKind >= PrimitiveKind.PrimitiveInt &&
      primitiveKind <= PrimitiveKind.PrimitiveInt64
    ) {
      if (value.kind === "int" || value.kind === "uint")
        return normalize({ type, kind: "int", value: BigInt.asIntN(64, value.value as bigint) });
      if (value.kind === "float")
        return normalize({ type, kind: "int", value: BigInt(Math.trunc(value.value as number)) });
    }
    if (
      primitiveKind >= PrimitiveKind.PrimitiveUint &&
      primitiveKind <= PrimitiveKind.PrimitiveUintptr
    ) {
      if (value.kind === "int" || value.kind === "uint")
        return normalize({ type, kind: "uint", value: BigInt.asUintN(64, value.value as bigint) });
      if (value.kind === "float")
        return normalize({ type, kind: "uint", value: BigInt(Math.trunc(value.value as number)) });
    }
    if (
      primitiveKind === PrimitiveKind.PrimitiveFloat32 ||
      primitiveKind === PrimitiveKind.PrimitiveFloat64
    ) {
      if (value.kind === "int" || value.kind === "uint")
        return normalize({ type, kind: "float", value: Number(value.value as bigint) });
      if (value.kind === "float") return normalize({ ...value, type });
    }
    if (
      primitiveKind === PrimitiveKind.PrimitiveComplex64 ||
      primitiveKind === PrimitiveKind.PrimitiveComplex128
    ) {
      if (value.kind === "complex") return normalize({ ...value, type });
    }
    if (
      primitiveKind === PrimitiveKind.PrimitiveString &&
      (value.kind === "int" || value.kind === "uint")
    ) {
      let rune = value.value as bigint;
      if (rune < 0n || rune > 0x10ffffn || (rune >= 0xd800n && rune <= 0xdfffn)) rune = 0xfffdn;
      return {
        type,
        kind: "string",
        value: textEncoder.encode(String.fromCodePoint(Number(rune))),
      };
    }
    if (primitiveKind === PrimitiveKind.PrimitiveString && value.kind === "slice") {
      const sourceElement = value.type.registry.element(value.type.registry.underlying(value.type));
      const slice = value.value as SliceValue;
      const backing = dereference(slice.storage);
      if (
        sourceElement &&
        sourceElement.registry.underlying(sourceElement).ref.primitive ===
          PrimitiveKind.PrimitiveUint8
      ) {
        const bytes =
          backing.kind === "bytes"
            ? (backing.value as Uint8Array).slice(slice.start, slice.start + slice.length)
            : Uint8Array.from(
                (backing.value as Value[])
                  .slice(slice.start, slice.start + slice.length)
                  .map((item) => Number(item.value as bigint)),
              );
        return { type, kind: "string", value: bytes };
      }
      if (
        sourceElement &&
        sourceElement.registry.underlying(sourceElement).ref.primitive ===
          PrimitiveKind.PrimitiveInt32
      ) {
        const values = (backing.value as Value[]).slice(slice.start, slice.start + slice.length);
        const text = values
          .map((item) => {
            const rune = Number(item.value as bigint);
            return String.fromCodePoint(
              rune < 0 || rune > 0x10ffff || (rune >= 0xd800 && rune <= 0xdfff) ? 0xfffd : rune,
            );
          })
          .join("");
        return { type, kind: "string", value: textEncoder.encode(text) };
      }
    }
  }
  const source = value.type.registry.underlying(value.type);
  if (source.ref.kind === target.ref.kind) return { ...value, type };
  fail("invalid conversion", "convert", "invalid_operation");
}

function invalid(operator: string): never {
  fail("operand types do not support operator", operator, "invalid_operation");
}

function comparisonResult(operator: string, order: number): boolean {
  if (operator === "<") return order < 0;
  if (operator === "<=") return order <= 0;
  if (operator === ">") return order > 0;
  if (operator === ">=") return order >= 0;
  invalid(operator);
}

function compareBytes(left: Uint8Array, right: Uint8Array): number {
  const length = Math.min(left.length, right.length);
  for (let index = 0; index < length; index++) {
    if (left[index]! < right[index]!) return -1;
    if (left[index]! > right[index]!) return 1;
  }
  return left.length < right.length ? -1 : left.length > right.length ? 1 : 0;
}

function complexDivide(a: number, b: number, c: number, d: number): [number, number] {
  let real: number;
  let imag: number;
  if (Math.abs(c) >= Math.abs(d)) {
    const ratio = d / c;
    const denominator = c + d * ratio;
    real = (a + b * ratio) / denominator;
    imag = (b - a * ratio) / denominator;
  } else {
    const ratio = c / d;
    const denominator = d + c * ratio;
    real = (a * ratio + b) / denominator;
    imag = (b * ratio - a) / denominator;
  }
  if (Number.isNaN(real) && Number.isNaN(imag)) {
    if (c === 0 && d === 0 && (!Number.isNaN(a) || !Number.isNaN(b))) {
      real = Math.sign(c || 1) * Infinity * a;
      imag = Math.sign(c || 1) * Infinity * b;
    } else if (
      (!Number.isFinite(a) || !Number.isFinite(b)) &&
      Number.isFinite(c) &&
      Number.isFinite(d)
    ) {
      const normalizedA = Number.isFinite(a) ? 0 : Math.sign(a);
      const normalizedB = Number.isFinite(b) ? 0 : Math.sign(b);
      real = Infinity * (normalizedA * c + normalizedB * d);
      imag = Infinity * (normalizedB * c - normalizedA * d);
    } else if (
      (!Number.isFinite(c) || !Number.isFinite(d)) &&
      Number.isFinite(a) &&
      Number.isFinite(b)
    ) {
      const normalizedC = Number.isFinite(c) ? 0 : Math.sign(c);
      const normalizedD = Number.isFinite(d) ? 0 : Math.sign(d);
      real = 0 * (a * normalizedC + b * normalizedD);
      imag = 0 * (b * normalizedC - a * normalizedD);
    }
  }
  return [real, imag];
}
