// The same tools contract runs in Node and a browser worker.
export async function exerciseTools(tools, workspace, sourceFixture, debugFixture, queries) {
  const service = await tools.createLanguageService();
  const incrementalTimes = [];
  const queryTimes = [];
  const memorySamples = [];
  let cancellationMs;
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
    for (let version = 2; version <= 101; version++) {
      const started = performance.now();
      await service.update([
        {
          Operation: "change",
          Identity: { URI: uri },
          Version: version,
          Changes: [{ text: workspace.Packages[0].Files[0].Text.replace("42", String(version)) }],
        },
      ]);
      await service.analyze();
      incrementalTimes.push(performance.now() - started);
      const queried = performance.now();
      await service.query("hover", { URI: uri, Position: { line: 2, character: 6 } });
      queryTimes.push(performance.now() - queried);
      if (version % 10 === 0) {
        const stats = await service.stats();
        memorySamples.push({
          heapBytes: String(stats.heapBytes),
          memoryBytes: String(stats.memoryBytes),
          allocatedBytes: String(stats.memoryAllocatedBytes),
          wasmBytes: stats.wasmBytes,
        });
      }
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
    const runningCancel = new AbortController();
    let canceledAt;
    const cancelTimer = setTimeout(() => {
      canceledAt = performance.now();
      runningCancel.abort();
    }, 10);
    try {
      await service.prepare(
        { Symbols: true, EntryPoints: debugFixture.EntryPoints },
        runningCancel.signal,
      );
      throw new Error("active build cancellation was not observed");
    } catch (error) {
      if (error.name !== "AbortError") throw error;
      cancellationMs = performance.now() - canceledAt;
    } finally {
      clearTimeout(cancelTimer);
    }
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
      for (let attempt = 0; attempt < 1000 && !stop; attempt++) {
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
      for (let attempt = 0; attempt < 1000 && !terminated; attempt++) {
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
    incrementalTimes.sort((a, b) => a - b);
    queryTimes.sort((a, b) => a - b);
    return {
      incrementalP95: incrementalTimes[Math.floor(incrementalTimes.length * 0.95)],
      queryP95: queryTimes[Math.floor(queryTimes.length * 0.95)],
      cancellationMs,
      guestHeapBytes: String(stats.heapBytes),
      guestMemoryBytes: String(stats.memoryBytes),
      memorySamples,
      wasmBytes: stats.wasmBytes,
    };
  } finally {
    await service.dispose();
    await service.dispose();
  }
}
