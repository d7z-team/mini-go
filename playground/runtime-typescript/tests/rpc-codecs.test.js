import test from "node:test";
import assert from "node:assert/strict";
import { bytesCodec, messageCodec, optionalCodec, rpcCodecs } from "@d7z-team/mini-go/rpc";

test("RPC codecs preserve exact numeric and nil states", () => {
  assert.throws(() => rpcCodecs.int64.encode(1), /bigint/);
  assert.throws(() => rpcCodecs.int64.encode(1n << 63n), /range/);
  assert.throws(() => rpcCodecs.uint32.encode(-1), /range/);
  assert.equal(rpcCodecs.int64.decode(rpcCodecs.int64.encode(-(1n << 63n))), -(1n << 63n));
  assert.equal(
    rpcCodecs.uint64.decode(rpcCodecs.uint64.encode((1n << 64n) - 1n)),
    (1n << 64n) - 1n,
  );
  assert.ok(Object.is(rpcCodecs.float32.decode(rpcCodecs.float32.encode(-0)), -0));
  assert.deepEqual(rpcCodecs.complex64.decode(rpcCodecs.complex64.encode({ re: 1 / 3, im: -0 })), {
    re: Math.fround(1 / 3),
    im: -0,
  });

  const bytes = bytesCodec();
  const source = new Uint8Array([1, 2]);
  const encoded = bytes.encode(source);
  source[0] = 9;
  assert.deepEqual(bytes.decode(encoded), new Uint8Array([1, 2]));
  assert.equal(bytes.decode(bytes.encode(null)), null);

  const optionalBytes = optionalCodec("optional[[]uint8]", bytes);
  assert.equal(optionalBytes.decode(optionalBytes.encode(undefined)), undefined);
  assert.equal(optionalBytes.decode(optionalBytes.encode(null)), null);
  assert.deepEqual(optionalBytes.decode(optionalBytes.encode(new Uint8Array())), new Uint8Array());
});

test("RPC message codec rejects incomplete, duplicate and unknown wire fields", () => {
  const codec = messageCodec("example.Item", [{ id: 1, name: "value", codec: rpcCodecs.int32 }]);
  assert.deepEqual(codec.decode(codec.encode({ value: 42 })), { value: 42 });
  assert.throws(
    () => codec.decode({ type: "example.Item", data: { kind: "struct", value: [] } }),
    /missing/,
  );
  assert.throws(
    () =>
      codec.decode({
        type: "example.Item",
        data: {
          kind: "struct",
          value: [
            { id: 1, value: rpcCodecs.int32.encode(1) },
            { id: 1, value: rpcCodecs.int32.encode(2) },
          ],
        },
      }),
    /field ID/,
  );
  assert.throws(
    () =>
      codec.decode({
        type: "example.Item",
        data: {
          kind: "struct",
          value: [
            { id: 1, value: rpcCodecs.int32.encode(1) },
            { id: 2, value: rpcCodecs.int32.encode(2) },
          ],
        },
      }),
    /unknown/,
  );

  for (const malformed of [
    { type: "int64", data: { kind: "int", value: 1 } },
    { type: "float64", data: { kind: "float", value: "1" } },
    { type: "complex128", data: { kind: "complex", value: [1] } },
    { type: "bool", data: { kind: "bool", value: 1 } },
    { type: "string", data: { kind: "string", value: 1 } },
    { type: "[]uint8", data: { kind: "bytes", value: [1, 2] } },
  ]) {
    const decoder = {
      int64: rpcCodecs.int64,
      float64: rpcCodecs.float64,
      complex128: rpcCodecs.complex128,
      bool: rpcCodecs.bool,
      string: rpcCodecs.string,
      "[]uint8": bytesCodec(),
    }[malformed.type];
    assert.throws(() => decoder.decode(malformed), /invalid MRPC/);
  }
});
