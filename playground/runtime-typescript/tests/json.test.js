import assert from "node:assert/strict";
import { test } from "vitest";
import { JsonNumber, VMError, decodeJson } from "../src/vm/json";

const encoder = new TextEncoder();

async function parse(source, maxBytes = 1_000_000) {
  return decodeJson(encoder.encode(source), maxBytes);
}

test("JSON loader keeps only image hash and artifact source spans", async () => {
  const source = JSON.stringify({
    format: "mini-go-execution-image",
    hash: "old",
    packages: {
      "example/main": {
        artifact: { format: "mini-go-ir", version: 30, module: { path: "example/main" } },
        artifact_hash: "artifact",
      },
    },
  });
  const document = await parse(source);
  const image = document.value;
  const packageValue = image.packages["example/main"];
  const artifact = packageValue.artifact;
  assert.equal(
    new TextDecoder().decode(document.bytes(artifact)),
    '{"format":"mini-go-ir","version":30,"module":{"path":"example/main"}}',
  );
  const replaced = new TextDecoder().decode(document.replacingProperty(image, "hash", '"new"'));
  assert.deepEqual(JSON.parse(replaced).hash, "new");
  assert.throws(() => document.bytes(image), VMError);
  assert.throws(() => document.replacingProperty(artifact, "hash", '"new"'), VMError);
});

test("JSON loader preserves exact numbers and legal whitespace", async () => {
  const document = await parse('{ "value" : 9223372036854775808, "text" : "plain" }');
  assert.ok(document.value.value instanceof JsonNumber);
  assert.equal(document.value.value.raw, "9223372036854775808");
  assert.equal(document.value.text, "plain");
  const escaped = await parse('{"text":"line\\n\\u754c"}');
  assert.equal(escaped.value.text, "line\n界");
});

test("JSON loader rejects duplicate keys and non-JSON whitespace", async () => {
  await assert.rejects(
    () => parse('{"packages":{"m":{"artifact":{"x":1,"x":2}}}}'),
    /duplicate object key x/,
  );
  await assert.rejects(() => parse('{"x":\u00a01}'), /expected JSON value|trailing JSON input/);
  await assert.rejects(() => parse('{"x":1,}'), /expected object key/);
});

test("JSON loader enforces UTF-8 and byte limits", async () => {
  await assert.rejects(() => decodeJson(Uint8Array.from([0xff]), 100), /valid UTF-8/);
  await assert.rejects(() => parse('{"x":"large"}', 5), /byte limit/);
});
