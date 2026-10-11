// The same wire/resource scenario runs in a browser page and a Node caller.
export async function exerciseRPC(createRuntime, load, address) {
  const rpcUrl = `${address.replace("http", "ws")}/rpc`;
  const rpcOptions = { leaseTtlMs: 1_000, admissionTimeoutMs: 10_000 };
  const client = await createRuntime(await load("call"), { rpcUrl, rpcOptions });
  try {
    const call = client.start("default");
    const callResult = await call.result;
    if (callResult.roots[0].data.Integer !== 42n)
      throw new Error("Go resource round trip mismatch");
    await call.settled;
    await client.close();
  } finally {
    client.terminate();
  }

  // A debugger pause yields to maintenance. Synchronous patch preparation does
  // not, so exercise it separately using the normal production lease.
  for (const patch of [false, true]) {
    const provider = await createRuntime(await load("provider"), {
      rpcUrl,
      ...(patch ? {} : { rpcOptions }),
    });
    try {
      const server = provider.start("default");
      const response = await fetch(`${address}/exercise`);
      if (!response.ok) throw new Error(await response.text());
      const report = await response.json();
      if (report.echo !== 3 || !report.recursive || !report.canceled || report.resource !== 42)
        throw new Error(`Go reverse call mismatch: ${JSON.stringify(report)}`);
      await provider.pause();
      const pending = fetch(`${address}/exercise`);
      if (!patch) await new Promise((resolve) => setTimeout(resolve, 2_200));
      if (patch) {
        let nextImage = await load("provider-patch");
        if (nextImage[0] === 0x1f && nextImage[1] === 0x8b) {
          nextImage = new Uint8Array(
            await new Response(
              new Blob([nextImage]).stream().pipeThrough(new DecompressionStream("gzip")),
            ).arrayBuffer(),
          );
        }
        const beforePatch = (await provider.stats()).generation;
        const patchError = await provider.patch(new Uint8Array([0x7b])).then(
          () => undefined,
          (error) => error,
        );
        if (!patchError || patchError.code !== "invalid_image")
          throw new Error("invalid patch must preserve the current revision");
        if ((await provider.stats()).generation !== beforePatch)
          throw new Error("rejected patch changed the RPC owner's revision");
        await provider.patch(nextImage);
        if ((await provider.stats()).generation !== beforePatch + 1n)
          throw new Error("RPC patch did not commit a new revision");
      }
      await provider.resume();
      const resumed = await pending;
      if (!resumed.ok) throw new Error(await resumed.text());
      const resumedReport = await resumed.json();
      if (resumedReport.resource !== 42 || resumedReport.echo !== 3)
        throw new Error(
          `RPC ownership changed across pause/patch: ${JSON.stringify(resumedReport)}`,
        );
      server.cancel();
      await server.result.catch(() => {});
      await provider.close();
    } finally {
      provider.terminate();
    }
  }
}
