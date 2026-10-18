import assert from "node:assert/strict";
import { test } from "vitest";
import { PrimitiveKind } from "../src/vm/contract.generated";
import {
  binaryIntegerBits,
  compareIntegerBits,
  OperatorCode,
  unaryIntegerBits,
  UnaryOperatorCode,
} from "../src/vm/operators";

function bits(value) {
  const normalized = BigInt.asUintN(64, value);
  return [Number(normalized & 0xffff_ffffn), Number(normalized >> 32n)];
}

function value(words, signed, width = 64) {
  const raw = (BigInt(words[1]) << 32n) | BigInt(words[0]);
  return signed ? BigInt.asIntN(width, raw) : BigInt.asUintN(width, raw);
}

test("pair-u32 integer lanes preserve arithmetic, bitwise, shifts and comparisons", () => {
  let seed = 0x9e37_79b9;
  const next = () => {
    seed ^= seed << 13;
    seed ^= seed >>> 17;
    seed ^= seed << 5;
    return seed >>> 0;
  };
  const output = new Uint32Array(2);
  for (let index = 0; index < 2_000; index++) {
    const leftWords = [next(), next()];
    const rightWords = [next(), next()];
    const left = value(leftWords, false);
    const right = value(rightWords, false);
    for (const [operator, expected] of [
      [OperatorCode.Add, left + right],
      [OperatorCode.Subtract, left - right],
      [OperatorCode.Multiply, left * right],
      [OperatorCode.BitAnd, left & right],
      [OperatorCode.BitOr, left | right],
      [OperatorCode.BitXor, left ^ right],
      [OperatorCode.BitClear, left & ~right],
    ]) {
      binaryIntegerBits(
        operator,
        PrimitiveKind.PrimitiveUint64,
        ...leftWords,
        ...rightWords,
        output,
        0,
      );
      assert.equal(value(output, false), BigInt.asUintN(64, expected));
    }
    for (const operator of [
      OperatorCode.Equal,
      OperatorCode.NotEqual,
      OperatorCode.Less,
      OperatorCode.LessEqual,
      OperatorCode.Greater,
      OperatorCode.GreaterEqual,
    ]) {
      const expected =
        operator === OperatorCode.Equal
          ? left === right
          : operator === OperatorCode.NotEqual
            ? left !== right
            : operator === OperatorCode.Less
              ? left < right
              : operator === OperatorCode.LessEqual
                ? left <= right
                : operator === OperatorCode.Greater
                  ? left > right
                  : left >= right;
      assert.equal(
        compareIntegerBits(operator, PrimitiveKind.PrimitiveUint64, ...leftWords, ...rightWords),
        expected,
      );
    }
  }
});

test("pair-u32 lanes normalize signed widths and use BigInt only for divide and remainder", () => {
  const output = new Uint32Array(2);
  const minusOne = bits(-1n);
  const two = bits(2n);
  unaryIntegerBits(UnaryOperatorCode.Minus, PrimitiveKind.PrimitiveInt8, ...two, output, 0);
  assert.equal(value(output, true, 8), -2n);
  binaryIntegerBits(
    OperatorCode.ShiftRight,
    PrimitiveKind.PrimitiveInt64,
    ...minusOne,
    ...bits(63n),
    output,
    0,
  );
  assert.equal(value(output, true), -1n);
  binaryIntegerBits(
    OperatorCode.Divide,
    PrimitiveKind.PrimitiveInt64,
    ...bits(-(1n << 62n)),
    ...bits(3n),
    output,
    0,
  );
  assert.equal(value(output, true), -(1n << 62n) / 3n);
  binaryIntegerBits(
    OperatorCode.Remainder,
    PrimitiveKind.PrimitiveInt64,
    ...bits(-19n),
    ...bits(7n),
    output,
    0,
  );
  assert.equal(value(output, true), -5n);
});
