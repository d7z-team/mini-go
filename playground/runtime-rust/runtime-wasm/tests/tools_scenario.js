// The same tools contract runs in Node and a browser worker.
export async function exerciseTools(
  tools,
  workspace,
  sourceFixture,
  debugFixture,
  queries,
  signal,
) {
  const service = await tools.createLanguageService(undefined, { timeoutMs: 300_000, signal });
  const abort = () => {
    void service.dispose();
  };
  signal?.addEventListener("abort", abort, { once: true });
  if (signal?.aborted) abort();
  try {
    await service.open(workspace);
    const first = await service.analyze();
    const uri = workspace.Packages[0].Files[0].URI;
    const hover = await service.query("hover", { URI: uri, Position: { line: 2, character: 6 } });
    if (!hover.contents.value.includes("Answer")) throw new Error("missing hover symbol");
    for (const { Operation, ...query } of queries)
      await service.query(Operation, { ...query, URI: uri });
    const assembled = await service.sources(sourceFixture.Trees);
    if (
      JSON.stringify(assembled.Packages.map((p) => p.ModulePath)) !==
      JSON.stringify(sourceFixture.Paths)
    )
      throw new Error("source package identities differ");
    if (assembled.Packages[1].Resources.find((r) => r.Path === "assets/data.bin").Data !== "AP8B")
      throw new Error("binary source resource differs");
    for (const failure of sourceFixture.Failures) {
      try {
        await service.sources(failure.Trees);
        throw new Error("invalid source input accepted");
      } catch (error) {
        if (error.code !== "invalid_argument" || !error.message.includes(failure.Error))
          throw error;
      }
    }
    const sourceCancel = new AbortController();
    sourceCancel.abort();
    try {
      await service.sources(sourceFixture.Trees, sourceCancel.signal);
      throw new Error("source cancellation was not observed");
    } catch (error) {
      if (error.name !== "AbortError") throw error;
    }
    await service.update([
      {
        Operation: "open",
        Identity: { URI: uri, ModulePath: "sample", Path: "main.mgo" },
        Version: 1,
        Text: workspace.Packages[0].Files[0].Text,
      },
    ]);
    let snapshot = first.Snapshot;
    for (const version of [2, 3]) {
      await service.update([
        {
          Operation: "change",
          Identity: { URI: uri },
          Version: version,
          Changes: [{ text: workspace.Packages[0].Files[0].Text.replace("42", String(version)) }],
        },
      ]);
      const analysis = await service.analyze();
      if (analysis.Snapshot === snapshot) throw new Error("edit retained the old snapshot");
      if (Object.values(analysis.Diagnostics).some((report) => report.items?.length))
        throw new Error("edit produced diagnostics");
      if ((await service.analyze()).Snapshot !== analysis.Snapshot)
        throw new Error("unchanged analysis lost its snapshot");
      snapshot = analysis.Snapshot;
      const editedHover = await service.query("hover", {
        URI: uri,
        Position: { line: 2, character: 6 },
      });
      if (!editedHover.contents.value.includes("Answer")) throw new Error("edit lost the symbol");
    }
    try {
      await service.query("hover", {
        Snapshot: first.Snapshot,
        URI: uri,
        Position: { line: 2, character: 6 },
      });
      throw new Error("stale snapshot accepted");
    } catch (error) {
      if (error.code !== "stale") throw error;
    }
    const canceled = new AbortController();
    canceled.abort();
    try {
      await service.analyze(canceled.signal);
      throw new Error("canceled analysis accepted");
    } catch (error) {
      if (error.name !== "AbortError") throw error;
    }
    await service.analyze();
    try {
      await service.upgrade(new TextEncoder().encode("invalid image"));
      throw new Error("invalid upgrade accepted");
    } catch (error) {
      if (error.message === "invalid upgrade accepted") throw error;
    }
    await service.query("hover", { URI: uri, Position: { line: 2, character: 6 } });
    await service.analyze();
    const build = await service.prepare({ Symbols: true, EntryPoints: debugFixture.EntryPoints });
    const debug = await tools.createDebugSession(new TextEncoder().encode(build.ImageJSON), {
      symbols: new TextEncoder().encode(build.SymbolsJSON),
      sources: build.Sources,
    });
    try {
      const breakpoints = await debug.request("setBreakpoints", {
        source: { path: debugFixture.Source },
        breakpoints: [{ line: debugFixture.Line }],
      });
      if (!breakpoints.breakpoints[0].verified) throw new Error("breakpoint not bound");
      await debug.request("configurationDone");
      let stop;
      while (!stop) {
        signal?.throwIfAborted();
        stop = (await debug.events()).find((event) => event.event === "stopped");
        if (!stop) await new Promise((resolve) => setTimeout(resolve, 1));
      }
      if (stop?.body.reason !== debugFixture.StopReason) throw new Error("breakpoint stop missing");
      await service.query("hover", { URI: uri, Position: { line: 2, character: 6 } });
      const stack = await debug.request("stackTrace", { threadId: stop.body.threadId });
      if (!stack.stackFrames.length) throw new Error("stack missing");
      await debug.output("stdout", "example output\n");
      const output = await debug.events();
      if (output[0]?.event !== "output") throw new Error("output event missing");
      await debug.request("continue", { threadId: stop.body.threadId });
      let terminated = false;
      while (!terminated) {
        signal?.throwIfAborted();
        terminated = (await debug.events()).some((event) => event.event === "terminated");
        if (!terminated) await new Promise((resolve) => setTimeout(resolve, 1));
      }
      if (!terminated) throw new Error("termination missing");
      if ((await debug.events()).some((event) => event.event === "terminated"))
        throw new Error("duplicate termination");
    } finally {
      await debug.dispose();
    }
    const stats = await service.stats();
    if (stats.activeScopes !== 0n || stats.tasks !== 0n || stats.ffiCalls !== 0n)
      throw new Error("compiler request resources retained");
    return {
      guestHeapBytes: String(stats.heapBytes),
      guestMemoryBytes: String(stats.memoryBytes),
      wasmBytes: stats.wasmBytes,
    };
  } finally {
    signal?.removeEventListener("abort", abort);
    await service.dispose();
    await service.dispose();
  }
}
