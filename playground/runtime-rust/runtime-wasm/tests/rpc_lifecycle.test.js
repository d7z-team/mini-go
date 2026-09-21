import assert from "node:assert/strict";
import test from "node:test";
import { RPCConnectionOwner } from "../dist/rpc-runtime.js";
import { RPCStatus, providedResource, rpcCodecs } from "@d7z-team/mini-go/rpc";

const contract = {
  protocol: "minigo.rpc.contract.v2",
  methods: [],
};
const method = {
  id: "test.v1::Service.Call",
  service: "test.v1::Service",
  name: "Call",
  contractHash: "contract",
  resourceTypeHash: "",
};

async function eventually(predicate) {
  for (let attempt = 0; attempt < 100; attempt++) {
    const value = predicate();
    if (value) return value;
    await new Promise((resolve) => setImmediate(resolve));
  }
  assert.fail("expected RPC lifecycle event was not observed");
}

async function harness(t) {
  let receive;
  let fail;
  const sent = [];
  const state = { onInvoke: undefined, rejectClose: false, holdClose: false, terminated: false };
  const respond = (id, value) => queueMicrotask(() => receive({ kind: "response", id, value }));
  const worker = {
    send(message) {
      if (state.rejectClose && message.kind === "close") throw new Error("close send failed");
      sent.push(message);
      switch (message.kind) {
        case "create":
          queueMicrotask(() => receive({ kind: "ready" }));
          break;
        case "bind":
          respond(message.id, 11);
          break;
        case "publish":
          respond(message.id, 22);
          break;
        case "accept":
        case "discard":
        case "closeBinding":
        case "closeResource":
        case "closePublication":
          respond(message.id);
          break;
        case "stats":
          respond(message.id, {
            bindings: 0n,
            resources: 0n,
            pendingResults: 0n,
            publications: 0n,
            pendingCalls: 0n,
            inboundCalls: 0n,
            wasmBytes: 0,
          });
          break;
        case "invoke":
          state.onInvoke?.(message);
          break;
        case "close":
          if (!state.holdClose) queueMicrotask(() => receive({ kind: "closed" }));
          break;
      }
    },
    listen(onMessage, onFailure) {
      receive = onMessage;
      fail = onFailure;
    },
    terminate() {
      state.terminated = true;
    },
  };
  const connection = await RPCConnectionOwner.connect("ws://example.test/rpc", {}, () => worker);
  t.after(() => connection.terminate());
  return {
    connection,
    sent,
    state,
    receive: (message) => receive(message),
    fail: (reason) => fail(reason),
  };
}

test("RPC connection options reject durations that cannot cross the WASM boundary", async () => {
  let workers = 0;
  for (const leaseTtlMs of [0, -1, 0.5, Number.MAX_SAFE_INTEGER]) {
    await assert.rejects(
      RPCConnectionOwner.connect("ws://example.test/rpc", { leaseTtlMs }, () => {
        workers++;
        throw new Error("worker should not be created");
      }),
      (reason) => reason instanceof RPCStatus && reason.code === "invalid_argument",
    );
  }
  assert.equal(workers, 0);
});

test("RPC result decoding decides provisional ownership exactly once", async (t) => {
  const h = await harness(t);
  const binding = await h.connection.bind(contract);

  h.state.onInvoke = (request) =>
    queueMicrotask(() =>
      h.receive({ kind: "response", id: request.id, value: { result: 31, values: null } }),
    );
  await assert.rejects(
    binding.invoke(method, [], () => assert.fail("malformed values reached the decoder")),
    (reason) => reason instanceof RPCStatus && reason.code === "protocol",
  );
  assert.equal(h.sent.filter((message) => message.kind === "discard").at(-1).result, 31);

  h.state.onInvoke = (request) =>
    queueMicrotask(() =>
      h.receive({
        kind: "response",
        id: request.id,
        value: { result: 32, values: [rpcCodecs.int64.encode(42n)] },
      }),
    );
  assert.equal(
    await binding.invoke(method, [], (values) => rpcCodecs.int64.decode(values[0])),
    42n,
  );
  assert.equal(h.sent.filter((message) => message.kind === "accept").at(-1).result, 32);

  const controller = new AbortController();
  h.state.onInvoke = () => {};
  const canceled = binding.invoke(method, [], () => undefined, { signal: controller.signal });
  const operation = h.sent.filter((message) => message.kind === "invoke").at(-1).id;
  controller.abort(new Error("stop waiting"));
  await assert.rejects(
    canceled,
    (reason) => reason instanceof RPCStatus && reason.code === "canceled",
  );
  assert.ok(
    h.sent.some((message) => message.kind === "cancel" && message.id === operation),
    "abort was not forwarded to the worker owner",
  );

  await binding.close();
  await h.connection.close();
});

test("provider resources settle, close once and reject late replies", async (t) => {
  const h = await harness(t);
  let closes = 0;
  let releaseLate;
  const descriptor = {
    typeHash: "resource-hash",
    invoke() {
      return [];
    },
    close(handler) {
      handler.closed = true;
      closes++;
    },
  };
  const resource = () => providedResource(descriptor.typeHash, descriptor, { closed: false });
  const publication = await h.connection.publish({
    contract,
    async invoke(_context, incoming) {
      if (incoming === "late") {
        await new Promise((resolve) => {
          releaseLate = resolve;
        });
        return [resource()];
      }
      if (incoming === "generic") {
        throw Object.assign(new Error("provider failed"), { code: "permission_denied" });
      }
      if (incoming === "status") throw new RPCStatus("permission_denied", "denied");
      if (incoming === "forged")
        return [{ type: "resource-hash", data: { kind: "localResource", value: 99 } }];
      if (incoming === "partial") return [resource(), resource()];
      return [resource()];
    },
  });
  const provider = h.sent.find((message) => message.kind === "publish").provider;
  const call = (id, name) =>
    h.receive({
      kind: "providerCall",
      id,
      provider,
      method: name,
      arguments: [],
      peer: { identity: "peer", attributes: {} },
      providerName: "test",
    });
  const result = (id) =>
    eventually(() =>
      h.sent.find((message) => message.kind === "providerResult" && message.call === id),
    );

  call(41, "discard");
  const discarded = await result(41);
  const discardedID = discarded.values[0].data.value;
  h.receive({ kind: "providerSettled", id: 41, retained: [] });
  await eventually(() => closes === 1);

  call(42, "accept");
  const accepted = await result(42);
  const acceptedID = accepted.values[0].data.value;
  assert.notEqual(acceptedID, discardedID);
  h.receive({ kind: "providerSettled", id: 42, retained: [acceptedID] });
  h.receive({ kind: "resourceClose", id: 51, resource: acceptedID });
  await eventually(() =>
    h.sent.find((message) => message.kind === "resourceClosed" && message.close === 51),
  );
  h.receive({ kind: "resourceClose", id: 52, resource: acceptedID });
  await eventually(() =>
    h.sent.find((message) => message.kind === "resourceClosed" && message.close === 52),
  );
  assert.equal(closes, 2, "provider resource close was not idempotent");

  call(45, "partial");
  const partial = await result(45);
  const partialIDs = partial.values.map((value) => value.data.value);
  h.receive({ kind: "providerSettled", id: 45, retained: [partialIDs[0]] });
  await eventually(() => closes === 3);
  h.receive({ kind: "resourceClose", id: 53, resource: partialIDs[0] });
  await eventually(() =>
    h.sent.find((message) => message.kind === "resourceClosed" && message.close === 53),
  );
  assert.equal(closes, 4, "partially retained resources did not settle independently");

  call(43, "generic");
  assert.equal((await result(43)).error.code, "internal");
  call(44, "status");
  assert.equal((await result(44)).error.code, "permission_denied");
  call(48, "forged");
  assert.equal((await result(48)).error.code, "internal");

  call(46, "late");
  h.receive({ kind: "providerCancel", id: 46 });
  releaseLate();
  const late = await result(46);
  assert.equal(late.error.code, "canceled");
  assert.equal(closes, 5, "late provider resource was not released locally");

  call(47, "orphan");
  const orphan = await result(47);
  h.receive({ kind: "providerSettled", id: 47, retained: [orphan.values[0].data.value] });

  await publication.close();
  await h.connection.close();
  assert.equal(closes, 6, "connection close did not release the remaining provider resource");
});

test("provider resource close failures retain one cleanup result", async (t) => {
  const h = await harness(t);
  let closes = 0;
  const descriptor = {
    typeHash: "resource-hash",
    invoke() {
      return [];
    },
    close() {
      closes++;
      throw new RPCStatus("internal", "resource close failed");
    },
  };
  const publication = await h.connection.publish({
    contract,
    invoke() {
      return [providedResource(descriptor.typeHash, descriptor, {})];
    },
  });
  const provider = h.sent.find((message) => message.kind === "publish").provider;
  h.receive({
    kind: "providerCall",
    id: 61,
    provider,
    method: "open",
    arguments: [],
    peer: { identity: "peer", attributes: {} },
    providerName: "test",
  });
  const result = await eventually(() =>
    h.sent.find((message) => message.kind === "providerResult" && message.call === 61),
  );
  const resource = result.values[0].data.value;
  h.receive({ kind: "providerSettled", id: 61, retained: [resource] });
  for (const close of [62, 63]) {
    h.receive({ kind: "resourceClose", id: close, resource });
    const response = await eventually(() =>
      h.sent.find((message) => message.kind === "resourceClosed" && message.close === close),
    );
    assert.equal(response.error.code, "internal");
  }
  assert.equal(closes, 1, "a failed resource close was invoked more than once");

  await publication.close();
  await assert.rejects(h.connection.close(), /resource close failed/);
  assert.equal(closes, 1);
});

test("forced termination releases retained provider resources once", async (t) => {
  const h = await harness(t);
  let closes = 0;
  const descriptor = {
    typeHash: "resource-hash",
    invoke() {
      return [];
    },
    close() {
      closes++;
    },
  };
  await h.connection.publish({
    contract,
    invoke() {
      return [providedResource(descriptor.typeHash, descriptor, {})];
    },
  });
  const provider = h.sent.find((message) => message.kind === "publish").provider;
  h.receive({
    kind: "providerCall",
    id: 71,
    provider,
    method: "open",
    arguments: [],
    peer: { identity: "peer", attributes: {} },
    providerName: "test",
  });
  const result = await eventually(() =>
    h.sent.find((message) => message.kind === "providerResult" && message.call === 71),
  );
  h.receive({ kind: "providerSettled", id: 71, retained: [result.values[0].data.value] });

  h.connection.terminate(new RPCStatus("unavailable", "forced shutdown"));
  await eventually(() => closes === 1);
  h.connection.terminate();
  assert.equal(closes, 1);
  assert.equal(h.state.terminated, true);
});

test("connection close send failure is terminal for every waiter", async (t) => {
  const h = await harness(t);
  h.state.rejectClose = true;
  await assert.rejects(h.connection.close(), /close send failed/);
  await assert.rejects(h.connection.close(), /close send failed/);
  assert.equal(h.state.terminated, true);
});
