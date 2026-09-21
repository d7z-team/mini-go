// The same generated TypeScript binding runs unchanged in Node.js and browsers.
export async function exerciseNativeRPC(api, address) {
  const { RPC, RPCStatus, rpcCodecs } = api;
  const { LaboratoryClient, LaboratoryContract, createLaboratoryProvider } = await import(
    "../.build/test-bindings/service.js"
  );
  const rpcUrl = `${address.replace("http", "ws")}/rpc`;
  const connectOptions = { leaseTtlMs: 1_000, admissionTimeoutMs: 10_000 };
  const check = (condition, message) => {
    if (!condition) throw new Error(message);
  };

  const connection = await RPC.connect(rpcUrl, connectOptions);
  try {
    let client = await LaboratoryClient.bind(connection);
    for (const [data, optionalData] of [
      [null, undefined],
      [new Uint8Array(), null],
      [new Uint8Array([0, 255]), new Uint8Array()],
    ]) {
      const packet = {
        data,
        values: [-1n, 9223372036854775807n],
        labels: new Map([
          ["b", "2"],
          ["a", "1"],
        ]),
        optionalData,
        details: { label: "中".repeat(512), mode: 99 },
        scalars: {
          i8: -128,
          i16: -32768,
          i32: -2147483648,
          i64: -9223372036854775808n,
          u8: 255,
          u16: 65535,
          u32: 4294967295,
          u64: 18446744073709551615n,
          f32: -0,
          f64: Number.MIN_VALUE,
          c64: { re: Infinity, im: -Infinity },
          c128: { re: Number.MIN_VALUE, im: -0 },
          flags: new Map([
            [false, "no"],
            [true, "yes"],
          ]),
          numbers: new Map([
            [-9223372036854775808n, "low"],
            [9223372036854775807n, "high"],
          ]),
          type: "keyword",
          next: undefined,
        },
      };
      const echoed = await client.echo(packet);
      check(
        echoed.data === null
          ? data === null
          : data !== null && echoed.data.every((value, index) => value === data[index]),
        "binary nil/empty/value distinction changed",
      );
      check(
        echoed.optionalData === undefined
          ? optionalData === undefined
          : echoed.optionalData === null
            ? optionalData === null
            : optionalData instanceof Uint8Array && echoed.optionalData.length === 0,
        "optional binary three-state value changed",
      );
      check(echoed.values[1] === 9223372036854775807n, "signed 64-bit value changed");
      check(echoed.scalars.u64 === 18446744073709551615n, "unsigned 64-bit value changed");
      check(Object.is(echoed.scalars.f32, -0), "float32 negative zero changed");
      check(Object.is(echoed.scalars.c128.im, -0), "complex negative zero changed");
      check(echoed.labels.get("a") === "1", "Map value changed");
    }
    const tree = await client.tree({ value: 1n, next: { value: 2n, next: undefined } });
    check(tree.next?.value === 2n, "recursive message changed");
    const [counter, details] = await client.open(40n);
    check(counter && details.label === "counter", "resource result metadata changed");
    check((await counter.add(2n)) === 42n, "resource invocation failed");
    check((await client.read(counter)) === 42n, "resource argument failed");
    await client.close();
    check((await counter.add(0n)) === 42n, "resource did not outlive service drain");
    await counter.close();
    await counter.close();

    client = await LaboratoryClient.bind(connection);
    // Keep the call active across multiple one-second lease renewals so the
    // explicit deadline is exercised after transport admission.
    const waitError = await client.wait({ timeoutMs: 2_500 }).then(
      () => undefined,
      (reason) => reason,
    );
    check(
      waitError instanceof RPCStatus && waitError.code === "deadline_exceeded",
      `deadline status changed: ${waitError?.code ?? "unknown"}: ${waitError?.message ?? String(waitError)}`,
    );
    await client.close();

    // Decode fails before acceptance. The provisional remote resource must be discarded.
    const raw = await connection.bind(LaboratoryContract);
    const open = LaboratoryContract.methods.find(
      (method) => method.service.endsWith("::Laboratory") && method.name === "Open",
    );
    check(open, "generated Open method is missing");
    const decodeError = await raw
      .invoke(open, [rpcCodecs.int64.encode(1n)], () => {
        throw new RPCStatus("protocol", "injected decoder rejection");
      })
      .then(
        () => undefined,
        (reason) => reason,
      );
    check(decodeError?.code === "protocol", "decoder rejection was not preserved");
    const stats = await connection.stats();
    check(
      stats.resources === 0n && stats.pendingResults === 0n,
      "discard retained provisional state",
    );
    await raw.close();
  } finally {
    await connection.close();
  }

  // A transport break is terminal: an unlimited call fails and all local
  // handles become unavailable instead of waiting for an implicit timeout.
  const disconnected = await RPC.connect(rpcUrl, connectOptions);
  try {
    const client = await LaboratoryClient.bind(disconnected);
    const waiting = client.wait();
    const response = await fetch(`${address}/disconnect`, { method: "POST" });
    check(response.status === 204, "peer disconnect failed");
    const failure = await waiting.then(
      () => undefined,
      (reason) => reason,
    );
    check(
      failure instanceof RPCStatus && failure.code === "unavailable",
      "disconnect did not terminate pending work",
    );
  } finally {
    await disconnected.close().catch(() => {});
  }

  const providerConnection = await RPC.connect(rpcUrl, connectOptions);
  let released = 0;
  let settleLateWait;
  const lateWait = new Promise((resolve) => {
    settleLateWait = resolve;
  });
  class Counter {
    constructor(value) {
      this.value = value;
      this.closed = false;
    }
    add(_context, delta) {
      if (this.closed) throw new RPCStatus("not_found", "counter is closed");
      this.value += delta;
      return this.value;
    }
    close() {
      if (!this.closed) {
        this.closed = true;
        released++;
      }
    }
  }
  const publication = await providerConnection.publish(
    createLaboratoryProvider({
      echo(_context, packet) {
        return packet;
      },
      tree(_context, node) {
        return node;
      },
      open(_context, initial) {
        return [new Counter(initial), { label: "counter", mode: 0 }];
      },
      read(context, counter) {
        if (!counter) throw new RPCStatus("invalid_argument", "counter is required");
        return counter.add(context, 0n);
      },
      async wait() {
        // Resolve after the Go caller's deadline to verify that a late provider
        // completion cannot revive the canceled call.
        await new Promise((resolve) => setTimeout(resolve, 60));
        settleLateWait();
        return 1n;
      },
    }),
    { name: "typescript" },
  );
  try {
    const response = await fetch(`${address}/exercise`);
    if (!response.ok) throw new Error(await response.text());
    const report = await response.json();
    check(
      report.echo === 3 && report.recursive && report.resource === 42 && report.canceled,
      `Go reverse call mismatch: ${JSON.stringify(report)}`,
    );
    await lateWait;
  } finally {
    await publication.close();
    await providerConnection.close();
  }
  check(released === 1, `provider resource close count ${released}`);
}
