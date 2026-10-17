import { Kind, PrimitiveKind } from "./contract.generated";
import { fail } from "./json";
import {
  boolValue,
  cell,
  cloneValue,
  decodeRune,
  dereference,
  guestEqual,
  indexValue,
  normalize,
  primitive,
  type Cell,
  type RuntimeType,
  type SliceValue,
  type Value,
} from "./value";

const textEncoder = new TextEncoder();

// Prepared instructions keep the operator discriminator as a small integer.  The
// numeric values are private to the runtime image and deliberately do not form a
// wire contract; the compiler still supplies the source spelling at the image
// boundary.  Keeping this table here also makes compare_branch share exactly the
// same operator validation as binary.
export const enum OperatorCode {
  Add = 1,
  Subtract,
  Multiply,
  Divide,
  Remainder,
  BitAnd,
  BitOr,
  BitXor,
  BitClear,
  ShiftLeft,
  ShiftRight,
  Equal,
  NotEqual,
  Less,
  LessEqual,
  Greater,
  GreaterEqual,
  LogicalAnd,
  LogicalOr,
  Complex,
}

export const enum UnaryOperatorCode {
  Plus = 1,
  Minus,
  Not,
  Complement,
  Real,
  Imag,
}

const OPERATOR_CODES: Readonly<Record<string, OperatorCode>> = {
  "+": OperatorCode.Add,
  "-": OperatorCode.Subtract,
  "*": OperatorCode.Multiply,
  "/": OperatorCode.Divide,
  "%": OperatorCode.Remainder,
  "&": OperatorCode.BitAnd,
  "|": OperatorCode.BitOr,
  "^": OperatorCode.BitXor,
  "&^": OperatorCode.BitClear,
  "<<": OperatorCode.ShiftLeft,
  ">>": OperatorCode.ShiftRight,
  "==": OperatorCode.Equal,
  "!=": OperatorCode.NotEqual,
  "<": OperatorCode.Less,
  "<=": OperatorCode.LessEqual,
  ">": OperatorCode.Greater,
  ">=": OperatorCode.GreaterEqual,
  "&&": OperatorCode.LogicalAnd,
  "||": OperatorCode.LogicalOr,
  complex: OperatorCode.Complex,
};

export function operatorCode(operator: string): OperatorCode {
  const code = OPERATOR_CODES[operator];
  if (!code) fail("unknown operator", operator, "invalid_operation");
  return code;
}

export function unaryOperatorCode(operator: string): UnaryOperatorCode {
  switch (operator) {
    case "+":
      return UnaryOperatorCode.Plus;
    case "-":
      return UnaryOperatorCode.Minus;
    case "!":
      return UnaryOperatorCode.Not;
    case "^":
      return UnaryOperatorCode.Complement;
    case "real":
      return UnaryOperatorCode.Real;
    case "imag":
      return UnaryOperatorCode.Imag;
    default:
      fail("unknown unary operator", operator, "invalid_operation");
  }
}

const OPERATOR_NAMES: readonly string[] = [
  "",
  "+",
  "-",
  "*",
  "/",
  "%",
  "&",
  "|",
  "^",
  "&^",
  "<<",
  ">>",
  "==",
  "!=",
  "<",
  "<=",
  ">",
  ">=",
  "&&",
  "||",
  "complex",
];

const UNARY_OPERATOR_NAMES: readonly string[] = ["", "+", "-", "!", "^", "real", "imag"];

export function operatorName(code: number): string {
  const name = OPERATOR_NAMES[code];
  if (!name) fail("unknown operator code", String(code), "invalid_operation");
  return name;
}

function unaryOperatorName(code: number): string {
  const name = UNARY_OPERATOR_NAMES[code];
  if (!name) fail("unknown unary operator code", String(code), "invalid_operation");
  return name;
}

export function unaryCode(code: number, operand: Value): Value {
  const value = cloneValue(operand);
  switch (code) {
    case UnaryOperatorCode.Plus:
      if (["int", "uint", "float", "complex"].includes(value.kind)) return value;
      break;
    case UnaryOperatorCode.Minus:
      if (value.kind === "int") value.value = -(value.value as bigint);
      else if (value.kind === "uint") value.value = -(value.value as bigint);
      else if (value.kind === "float") value.value = -(value.value as number);
      else if (value.kind === "complex") {
        const complex = value.value as { real: number; imag: number };
        value.value = { real: -complex.real, imag: -complex.imag };
      } else break;
      return normalize(value);
    case UnaryOperatorCode.Not:
      if (value.kind === "bool") return { ...value, value: !(value.value as boolean) };
      break;
    case UnaryOperatorCode.Complement:
      if (value.kind === "int" || value.kind === "uint") {
        value.value = ~(value.value as bigint);
        return normalize(value);
      }
      break;
    case UnaryOperatorCode.Real:
    case UnaryOperatorCode.Imag:
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
          value: code === UnaryOperatorCode.Real ? complex.real : complex.imag,
        });
      }
      break;
  }
  fail("operand type does not support operator", unaryOperatorName(code), "invalid_operation");
}

export function unary(operator: string, operand: Value): Value {
  return unaryCode(unaryOperatorCode(operator), operand);
}

export function binary(operator: string, left: Value, right: Value): Value {
  return binaryCode(operatorCode(operator), left, right);
}

/** Execute the prepared binary operator directly on its numeric discriminator. */
export function binaryCode(code: number, left: Value, right: Value): Value {
  const registry = left.type.registry;
  if (code === OperatorCode.Equal || code === OperatorCode.NotEqual)
    return boolValue(registry, guestEqual(left, right) === (code === OperatorCode.Equal));
  if (code === OperatorCode.Complex && left.kind === "float" && right.kind === "float") {
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
  if (code === OperatorCode.ShiftLeft || code === OperatorCode.ShiftRight) {
    if (!(["int", "uint"].includes(left.kind) && ["int", "uint"].includes(right.kind)))
      invalid(operatorName(code));
    const shift = right.value as bigint;
    if (shift < 0n) fail("negative shift count", operatorName(code), "panic");
    let result: bigint;
    if (shift >= 64n)
      result =
        code === OperatorCode.ShiftRight && left.kind === "int" && (left.value as bigint) < 0n
          ? -1n
          : 0n;
    else
      result =
        code === OperatorCode.ShiftLeft
          ? (left.value as bigint) << shift
          : (left.value as bigint) >> shift;
    return normalize({ ...left, value: result });
  }
  if (left.kind === "bool" && right.kind === "bool") {
    if (code === OperatorCode.LogicalAnd)
      return boolValue(registry, (left.value as boolean) && (right.value as boolean));
    if (code === OperatorCode.LogicalOr)
      return boolValue(registry, (left.value as boolean) || (right.value as boolean));
    invalid(operatorName(code));
  }
  if (left.kind === "string" && right.kind === "string") {
    const a = left.value as Uint8Array;
    const b = right.value as Uint8Array;
    if (code === OperatorCode.Add) {
      const joined = new Uint8Array(a.length + b.length);
      joined.set(a);
      joined.set(b, a.length);
      return { ...left, value: joined };
    }
    return boolValue(registry, compareCode(code, left, right));
  }
  if (left.kind === "complex" && right.kind === "complex") {
    const a = left.value as { real: number; imag: number };
    const b = right.value as { real: number; imag: number };
    let real: number, imag: number;
    if (code === OperatorCode.Add) [real, imag] = [a.real + b.real, a.imag + b.imag];
    else if (code === OperatorCode.Subtract) [real, imag] = [a.real - b.real, a.imag - b.imag];
    else if (code === OperatorCode.Multiply)
      [real, imag] = [a.real * b.real - a.imag * b.imag, a.real * b.imag + a.imag * b.real];
    else if (code === OperatorCode.Divide)
      [real, imag] = complexDivide(a.real, a.imag, b.real, b.imag);
    else invalid(operatorName(code));
    return normalize({ ...left, value: { real: real!, imag: imag! } });
  }
  if (left.kind === "float" && right.kind === "float") {
    const a = left.value as number;
    const b = right.value as number;
    if (code >= OperatorCode.Less && code <= OperatorCode.GreaterEqual)
      return boolValue(registry, compareCode(code, left, right));
    const result =
      code === OperatorCode.Add
        ? a + b
        : code === OperatorCode.Subtract
          ? a - b
          : code === OperatorCode.Multiply
            ? a * b
            : code === OperatorCode.Divide
              ? a / b
              : undefined;
    if (result === undefined) invalid(operatorName(code));
    return normalize({ ...left, value: result });
  }
  if ((left.kind === "int" || left.kind === "uint") && left.kind === right.kind) {
    const a = left.value as bigint;
    const b = right.value as bigint;
    if (code >= OperatorCode.Less && code <= OperatorCode.GreaterEqual)
      return boolValue(registry, compareCode(code, left, right));
    if ((code === OperatorCode.Divide || code === OperatorCode.Remainder) && b === 0n)
      fail("integer divide by zero", operatorName(code), "panic");
    let result: bigint;
    switch (code) {
      case OperatorCode.Add:
        result = a + b;
        break;
      case OperatorCode.Subtract:
        result = a - b;
        break;
      case OperatorCode.Multiply:
        result = a * b;
        break;
      case OperatorCode.Divide:
        result = a / b;
        break;
      case OperatorCode.Remainder:
        result = a % b;
        break;
      case OperatorCode.BitAnd:
        result = a & b;
        break;
      case OperatorCode.BitOr:
        result = a | b;
        break;
      case OperatorCode.BitXor:
        result = a ^ b;
        break;
      case OperatorCode.BitClear:
        result = a & ~b;
        break;
      default:
        invalid(operatorName(code));
    }
    return normalize({ ...left, value: result! });
  }
  invalid(operatorName(code));
}

/** Evaluate a prepared binary operator without allocating a bool Value. */
export function compareCode(code: number, left: Value, right: Value): boolean {
  if (code === OperatorCode.Equal || code === OperatorCode.NotEqual) {
    const equal = guestEqual(left, right);
    return code === OperatorCode.Equal ? equal : !equal;
  }
  let comparison: number;
  if (left.kind === "string" && right.kind === "string")
    comparison = compareBytes(left.value as Uint8Array, right.value as Uint8Array);
  else if (left.kind === "float" && right.kind === "float") {
    const a = left.value as number;
    const b = right.value as number;
    if (Number.isNaN(a) || Number.isNaN(b)) return false;
    comparison = a < b ? -1 : a > b ? 1 : 0;
  } else if ((left.kind === "int" || left.kind === "uint") && left.kind === right.kind) {
    const a = left.value as bigint;
    const b = right.value as bigint;
    comparison = a < b ? -1 : a > b ? 1 : 0;
  } else {
    invalid(operatorName(code));
  }
  switch (code) {
    case OperatorCode.Less:
      return comparison! < 0;
    case OperatorCode.LessEqual:
      return comparison! <= 0;
    case OperatorCode.Greater:
      return comparison! > 0;
    case OperatorCode.GreaterEqual:
      return comparison! >= 0;
    default:
      invalid(operatorName(code));
  }
}

export function convert(
  value: Value,
  type: RuntimeType,
  createCell: (value: Value) => Cell = cell,
): Value {
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
      const bytes = value.value as Uint8Array;
      const runes: Value[] = [];
      for (let offset = 0; offset < bytes.length; ) {
        const [rune, width] = decodeRune(bytes, offset);
        runes.push({ type: targetElement, kind: "int", value: BigInt(rune) });
        offset += width;
      }
      backing = { type, kind: "array", value: runes };
      length = runes.length;
    } else invalid("convert");
    return {
      type,
      kind: "slice",
      value: {
        storage: { root: createCell(backing!), path: [] },
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
    if (
      primitiveKind === PrimitiveKind.PrimitiveString &&
      (value.kind === "slice" || value.kind === "nil")
    ) {
      const sourceElement = value.type.registry.element(value.type.registry.underlying(value.type));
      if (value.kind === "nil" && sourceElement) {
        const element = sourceElement.registry.underlying(sourceElement).ref.primitive;
        if (element === PrimitiveKind.PrimitiveUint8 || element === PrimitiveKind.PrimitiveInt32)
          return { type, kind: "string", value: new Uint8Array() };
      }
      if (value.kind !== "slice") invalid("convert");
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
                Array.from({ length: slice.length }, (_, index) =>
                  Number(indexValue(backing, slice.start + index).value as bigint),
                ),
              );
        return { type, kind: "string", value: bytes };
      }
      if (
        sourceElement &&
        sourceElement.registry.underlying(sourceElement).ref.primitive ===
          PrimitiveKind.PrimitiveInt32
      ) {
        const values = Array.from({ length: slice.length }, (_, index) =>
          indexValue(backing, slice.start + index),
        );
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
