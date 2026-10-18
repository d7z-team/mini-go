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

export type ScalarValue = boolean | bigint | number;

function integerWidth(kind: number): number {
  switch (kind) {
    case PrimitiveKind.PrimitiveInt8:
    case PrimitiveKind.PrimitiveUint8:
      return 8;
    case PrimitiveKind.PrimitiveInt16:
    case PrimitiveKind.PrimitiveUint16:
      return 16;
    case PrimitiveKind.PrimitiveInt32:
    case PrimitiveKind.PrimitiveUint32:
      return 32;
    default:
      return 64;
  }
}

function signedInteger(kind: number): boolean {
  return kind >= PrimitiveKind.PrimitiveInt && kind <= PrimitiveKind.PrimitiveInt64;
}

function normalizeIntegerBits(kind: number, output: Uint32Array, offset: number): void {
  const width = integerWidth(kind);
  if (width === 64) return;
  const mask = width === 32 ? 0xffff_ffff : 2 ** width - 1;
  let low = (output[offset]! & mask) >>> 0;
  const negative = signedInteger(kind) && (low & (2 ** (width - 1))) !== 0;
  if (negative && width < 32) low = (low | ~mask) >>> 0;
  output[offset] = low;
  output[offset + 1] = negative ? 0xffff_ffff : 0;
}

function pairBigInt(low: number, high: number, signed: boolean): bigint {
  const bits = (BigInt(high) << 32n) | BigInt(low);
  return signed ? BigInt.asIntN(64, bits) : bits;
}

function writePairBigInt(value: bigint, output: Uint32Array, offset: number): void {
  const bits = BigInt.asUintN(64, value);
  output[offset] = Number(bits & 0xffff_ffffn);
  output[offset + 1] = Number(bits >> 32n);
}

/** Compare normalized pair-u32 lanes without constructing BigInt values. */
export function compareIntegerBits(
  code: number,
  kind: number,
  leftLow: number,
  leftHigh: number,
  rightLow: number,
  rightHigh: number,
): boolean {
  const equal = leftLow === rightLow && leftHigh === rightHigh;
  if (code === OperatorCode.Equal || code === OperatorCode.NotEqual)
    return code === OperatorCode.Equal ? equal : !equal;
  const signed = signedInteger(kind);
  const leftOrder = signed ? (leftHigh ^ 0x8000_0000) >>> 0 : leftHigh;
  const rightOrder = signed ? (rightHigh ^ 0x8000_0000) >>> 0 : rightHigh;
  const less = leftOrder < rightOrder || (leftOrder === rightOrder && leftLow < rightLow);
  const greater = leftOrder > rightOrder || (leftOrder === rightOrder && leftLow > rightLow);
  switch (code) {
    case OperatorCode.Less:
      return less;
    case OperatorCode.LessEqual:
      return less || equal;
    case OperatorCode.Greater:
      return greater;
    case OperatorCode.GreaterEqual:
      return greater || equal;
    default:
      invalid(operatorName(code));
  }
}

/** Execute the common integer operations directly on pair-u32 lanes. */
export function binaryIntegerBits(
  code: number,
  kind: number,
  leftLow: number,
  leftHigh: number,
  rightLow: number,
  rightHigh: number,
  output: Uint32Array,
  offset: number,
): void {
  let low = 0;
  let high = 0;
  switch (code) {
    case OperatorCode.Add:
      low = (leftLow + rightLow) >>> 0;
      high = (leftHigh + rightHigh + (low < leftLow ? 1 : 0)) >>> 0;
      break;
    case OperatorCode.Subtract:
      low = (leftLow - rightLow) >>> 0;
      high = (leftHigh - rightHigh - (leftLow < rightLow ? 1 : 0)) >>> 0;
      break;
    case OperatorCode.Multiply: {
      low = Math.imul(leftLow, rightLow) >>> 0;
      const leftLow16 = leftLow & 0xffff;
      const leftHigh16 = leftLow >>> 16;
      const rightLow16 = rightLow & 0xffff;
      const rightHigh16 = rightLow >>> 16;
      const carry =
        Math.floor((leftLow16 * rightLow16) / 0x1_0000) +
        leftHigh16 * rightLow16 +
        leftLow16 * rightHigh16;
      const upper = leftHigh16 * rightHigh16 + Math.floor(carry / 0x1_0000);
      high = (upper + Math.imul(leftHigh, rightLow) + Math.imul(leftLow, rightHigh)) >>> 0;
      break;
    }
    case OperatorCode.BitAnd:
      low = leftLow & rightLow;
      high = leftHigh & rightHigh;
      break;
    case OperatorCode.BitOr:
      low = leftLow | rightLow;
      high = leftHigh | rightHigh;
      break;
    case OperatorCode.BitXor:
      low = leftLow ^ rightLow;
      high = leftHigh ^ rightHigh;
      break;
    case OperatorCode.BitClear:
      low = leftLow & ~rightLow;
      high = leftHigh & ~rightHigh;
      break;
    case OperatorCode.ShiftLeft:
    case OperatorCode.ShiftRight: {
      const shift = rightHigh === 0 ? rightLow : 64;
      if (shift >= 64) {
        low =
          code === OperatorCode.ShiftRight && signedInteger(kind) && (leftHigh & 0x8000_0000) !== 0
            ? 0xffff_ffff
            : 0;
        high = low;
      } else if (shift === 0) {
        low = leftLow;
        high = leftHigh;
      } else if (code === OperatorCode.ShiftLeft) {
        if (shift < 32) {
          low = leftLow << shift;
          high = (leftHigh << shift) | (leftLow >>> (32 - shift));
        } else {
          low = 0;
          high = leftLow << (shift - 32);
        }
      } else if (shift < 32) {
        low = (leftLow >>> shift) | (leftHigh << (32 - shift));
        high = signedInteger(kind) ? leftHigh >> shift : leftHigh >>> shift;
      } else {
        low = signedInteger(kind) ? leftHigh >> (shift - 32) : leftHigh >>> (shift - 32);
        high = signedInteger(kind) && (leftHigh & 0x8000_0000) !== 0 ? 0xffff_ffff : 0;
      }
      break;
    }
    case OperatorCode.Divide:
    case OperatorCode.Remainder: {
      if (rightLow === 0 && rightHigh === 0)
        fail("integer divide by zero", operatorName(code), "panic");
      const signed = signedInteger(kind);
      const left = pairBigInt(leftLow, leftHigh, signed);
      const right = pairBigInt(rightLow, rightHigh, signed);
      writePairBigInt(code === OperatorCode.Divide ? left / right : left % right, output, offset);
      normalizeIntegerBits(kind, output, offset);
      return;
    }
    default:
      invalid(operatorName(code));
  }
  output[offset] = low;
  output[offset + 1] = high;
  normalizeIntegerBits(kind, output, offset);
}

export function unaryIntegerBits(
  code: number,
  kind: number,
  low: number,
  high: number,
  output: Uint32Array,
  offset: number,
): void {
  if (code === UnaryOperatorCode.Plus) {
    output[offset] = low;
    output[offset + 1] = high;
  } else if (code === UnaryOperatorCode.Minus) {
    const nextLow = -low >>> 0;
    output[offset] = nextLow;
    output[offset + 1] = (~high + (nextLow === 0 ? 1 : 0)) >>> 0;
  } else if (code === UnaryOperatorCode.Complement) {
    output[offset] = ~low;
    output[offset + 1] = ~high;
  } else invalid(unaryOperatorName(code));
  normalizeIntegerBits(kind, output, offset);
}

function normalizeScalar(kind: number, value: ScalarValue): ScalarValue {
  if (
    kind >= PrimitiveKind.PrimitiveInt &&
    kind <= PrimitiveKind.PrimitiveInt64 &&
    typeof value === "bigint"
  )
    return BigInt.asIntN(integerWidth(kind), value);
  if (
    kind >= PrimitiveKind.PrimitiveUint &&
    kind <= PrimitiveKind.PrimitiveUintptr &&
    typeof value === "bigint"
  )
    return BigInt.asUintN(integerWidth(kind), value);
  if (kind === PrimitiveKind.PrimitiveFloat32 && typeof value === "number")
    return Math.fround(value);
  return value;
}

/** Execute a verified unary operation directly on a typed frame lane. */
export function unaryScalar(code: number, kind: number, operand: ScalarValue): ScalarValue {
  switch (code) {
    case UnaryOperatorCode.Plus:
      return operand;
    case UnaryOperatorCode.Minus:
      if (typeof operand === "bigint" || typeof operand === "number")
        return normalizeScalar(kind, -operand);
      break;
    case UnaryOperatorCode.Not:
      if (typeof operand === "boolean") return !operand;
      break;
    case UnaryOperatorCode.Complement:
      if (typeof operand === "bigint") return normalizeScalar(kind, ~operand);
      break;
  }
  fail("operand type does not support operator", unaryOperatorName(code), "invalid_operation");
}

/** Compare verified primitive lanes without allocating temporary Values. */
export function compareScalar(code: number, left: ScalarValue, right: ScalarValue): boolean {
  if (code === OperatorCode.Equal || code === OperatorCode.NotEqual) {
    const equal = left === right;
    return code === OperatorCode.Equal ? equal : !equal;
  }
  if (
    (typeof left !== "bigint" && typeof left !== "number") ||
    typeof left !== typeof right ||
    (typeof left === "number" && (Number.isNaN(left) || Number.isNaN(right as number)))
  )
    return false;
  switch (code) {
    case OperatorCode.Less:
      return left < (right as never);
    case OperatorCode.LessEqual:
      return left <= (right as never);
    case OperatorCode.Greater:
      return left > (right as never);
    case OperatorCode.GreaterEqual:
      return left >= (right as never);
    default:
      invalid(operatorName(code));
  }
}

/** Execute a verified non-string, non-complex binary operation on frame lanes. */
export function binaryScalar(
  code: number,
  kind: number,
  left: ScalarValue,
  right: ScalarValue,
): ScalarValue {
  if (code >= OperatorCode.Equal && code <= OperatorCode.GreaterEqual)
    return compareScalar(code, left, right);
  if (typeof left === "boolean" && typeof right === "boolean") {
    if (code === OperatorCode.LogicalAnd) return left && right;
    if (code === OperatorCode.LogicalOr) return left || right;
    invalid(operatorName(code));
  }
  if (typeof left === "number" && typeof right === "number") {
    const result =
      code === OperatorCode.Add
        ? left + right
        : code === OperatorCode.Subtract
          ? left - right
          : code === OperatorCode.Multiply
            ? left * right
            : code === OperatorCode.Divide
              ? left / right
              : undefined;
    if (result === undefined) invalid(operatorName(code));
    return normalizeScalar(kind, result);
  }
  if (typeof left !== "bigint" || typeof right !== "bigint") invalid(operatorName(code));
  if (code === OperatorCode.ShiftLeft || code === OperatorCode.ShiftRight) {
    if (right < 0n) fail("negative shift count", operatorName(code), "panic");
    const signed = kind >= PrimitiveKind.PrimitiveInt && kind <= PrimitiveKind.PrimitiveInt64;
    const result =
      right >= 64n
        ? code === OperatorCode.ShiftRight && signed && left < 0n
          ? -1n
          : 0n
        : code === OperatorCode.ShiftLeft
          ? left << right
          : left >> right;
    return normalizeScalar(kind, result);
  }
  if ((code === OperatorCode.Divide || code === OperatorCode.Remainder) && right === 0n)
    fail("integer divide by zero", operatorName(code), "panic");
  let result: bigint;
  switch (code) {
    case OperatorCode.Add:
      result = left + right;
      break;
    case OperatorCode.Subtract:
      result = left - right;
      break;
    case OperatorCode.Multiply:
      result = left * right;
      break;
    case OperatorCode.Divide:
      result = left / right;
      break;
    case OperatorCode.Remainder:
      result = left % right;
      break;
    case OperatorCode.BitAnd:
      result = left & right;
      break;
    case OperatorCode.BitOr:
      result = left | right;
      break;
    case OperatorCode.BitXor:
      result = left ^ right;
      break;
    case OperatorCode.BitClear:
      result = left & ~right;
      break;
    default:
      invalid(operatorName(code));
  }
  return normalizeScalar(kind, result);
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
