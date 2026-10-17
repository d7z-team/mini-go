import assert from "node:assert/strict";
import { test } from "vitest";
import { PrimitiveKind } from "../src/vm/contract.generated";
import { TypedChannel } from "../src/vm/typed-channel";
import { primitive } from "../src/vm/value";
import { storageFixture } from "./support/storage-owner";

test("channel ring copies values, wraps and clears consumed references", () => {
  const { registry, owner, heap } = storageFixture();
  const type = primitive(registry, PrimitiveKind.PrimitiveString);
  const value = (text) => ({ type, kind: "string", value: new TextEncoder().encode(text) });
  const channel = new TypedChannel(owner, type, 2);
  heap.addRoot(channel.handle);
  heap.collectMinor();
  heap.collectMinor();
  const first = value("first");
  channel.push(first);
  first.value.fill(0);
  channel.push(value("second"));
  heap.collectMinor();
  assert.equal(new TextDecoder().decode(channel.shift().value), "first");
  channel.push(value("third"));
  heap.collectMajor();
  assert.deepEqual(
    Array.from(channel, (entry) => new TextDecoder().decode(entry.value)),
    ["second", "third"],
  );
  channel.shift();
  channel.shift();
  heap.collectMajor();
  assert.equal(channel.length, 0);
  assert.equal(channel.shift(), undefined);
  const backing = heap.readReference(channel.handle, 16);
  assert.deepEqual(heap.readReference(backing, 0), { slot: 0, generation: 0 });
  assert.deepEqual(heap.readReference(backing, 24), { slot: 0, generation: 0 });
  channel.closed = true;
  assert.equal(channel.closed, true);
});

test("failed channel encoding preserves the queued values and cursor", () => {
  const { registry, owner, failAllocation } = storageFixture();
  const type = primitive(registry, PrimitiveKind.PrimitiveString);
  const value = (text) => ({ type, kind: "string", value: new TextEncoder().encode(text) });
  const channel = new TypedChannel(owner, type, 2);
  channel.push(value("retained"));
  failAllocation(1);
  assert.throws(() => channel.push(value("rejected")), /injected allocation/);
  assert.equal(channel.length, 1);
  assert.equal(new TextDecoder().decode(channel.shift().value), "retained");
  assert.equal(channel.length, 0);
});
