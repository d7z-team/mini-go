import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { test } from "vitest";
import { createRuntime } from "@d7z-team/mini-go";
import * as nativeRPC from "@d7z-team/mini-go/rpc";
import { exerciseNativeRPC } from "./support/native-rpc.js";
import { exerciseRPC } from "./support/rpc.js";
import { runtimeVector, sealImage } from "./support/runtime-vectors.js";
import { root, startPeer } from "./support/peer.js";
import * as nativeBindings from "../../../testdata/rpc/generated/typescript/service";

test("Node runtime executes an optimized image without taking caller buffers", async (t) => {
  const vector = runtimeVector("parallel_cpu");
  const image = vector.bytes;
  const owned = Uint8Array.from(image);
  const vm = await createRuntime(image);
  t.onTestFinished(() => vm.terminate());
  assert.deepEqual(image, owned, "caller retains image ownership");
  const call = vm.start("default");
  assert.equal((await call.result).roots[0].data.Integer, BigInt(vector.result_integer));
  await call.settled;
  const stats = await vm.stats();
  assert.ok(stats.steps > 0n, "steps");
  assert.ok(stats.majorCollections + stats.minorCollections > 0n, "managed collection");
  assert.ok(stats.totalAllocatedBytes >= stats.occupiedBytes, "memory accounting");
  await vm.close();
});

test("Node runtime enforces budgets and publishes hot patches atomically", async (t) => {
  const initial = runtimeVector("any_reassign");
  const replacement = runtimeVector("arithmetic");
  const tampered = Uint8Array.from(initial.bytes);
  const hashOffset = new TextDecoder().decode(tampered).lastIndexOf('"hash":"') + 8;
  tampered[hashOffset] = tampered[hashOffset] === 0x30 ? 0x31 : 0x30;
  await assert.rejects(createRuntime(tampered), /hash mismatch/);
  const malformed = JSON.parse(new TextDecoder().decode(initial.bytes));
  const artifact = Object.values(malformed.packages)[0].artifact;
  artifact.functions[0].code.operands[0].outputs = [0xffffffff];
  await assert.rejects(createRuntime(sealImage(malformed)), { code: "invalid_operand" });
  await assert.rejects(createRuntime(initial.bytes, { load: { maxArtifactBytes: 1 } }), {
    code: "load_limit",
  });
  for (const options of [
    { limits: { maxGuestBytes: 0 } },
    { load: { maxPackages: 0.5 } },
    { rpcOptions: { leaseTtlMs: 0 } },
  ]) {
    await assert.rejects(createRuntime(initial.bytes, options), /invalid/);
  }

  const limited = await createRuntime(replacement.bytes, { limits: { maxSteps: 1 } });
  t.onTestFinished(() => limited.terminate());
  await assert.rejects(limited.start("default").result, { code: "step_limit" });
  await limited.close();

  const vm = await createRuntime(initial.bytes, { limits: { maxSteps: 0 } });
  t.onTestFinished(() => vm.terminate());
  await assert.rejects(vm.patch(Uint8Array.of(1, 2, 3)), { code: "invalid_image" });
  await assert.rejects(vm.patch(initial.bytes), { code: "unchanged" });
  assert.equal((await vm.stats()).generation, 1n);
  await vm.patch(replacement.bytes);
  assert.equal((await vm.stats()).generation, 2n);
  const call = vm.start("default");
  assert.equal((await call.result).roots[0].data.Integer, BigInt(replacement.result_integer));
  await call.settled;
  assert.equal((await vm.stats()).activeScopes, 0n);
  await vm.close();

  const retained = await createRuntime(initial.bytes, {
    limits: { maxRetainedRevisions: 1 },
  });
  t.onTestFinished(() => retained.terminate());
  await assert.rejects(retained.patch(replacement.bytes), { code: "resource_limit" });
  assert.equal((await retained.stats()).generation, 1n);
  await retained.close();
});

test(
  "Node WebSocket RPC interoperates with Go in both directions",
  { timeout: 30_000 },
  async (t) => {
    const address = await startPeer(t);
    await exerciseRPC(
      createRuntime,
      (name) => readFile(path.join(root, `testdata/rpc/images/${name}.json.gz`)),
      address,
    );
  },
);

test(
  "Node generated TypeScript RPC interoperates with Go in both directions",
  { timeout: 30_000 },
  async (t) => {
    const address = await startPeer(t);
    await exerciseNativeRPC(nativeRPC, address, nativeBindings);
  },
);
