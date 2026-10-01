export async function exerciseCompilerWorkload(tools, workload) {
  const service = await tools.createLanguageService();
  try {
    const started = performance.now();
    const uri = "file:///sample/main.mgo";
    await service.open({
      Root: "sample",
      Packages: [
        {
          Namespace: "module:sample",
          ModulePath: "sample",
          Files: [{ Path: "main.mgo", URI: uri, Text: workload.Source }],
        },
      ],
    });
    const openMs = performance.now() - started;
    const first = await service.analyze();
    for (const report of Object.values(first.Diagnostics)) {
      if (report.items?.length) throw new Error(JSON.stringify(report));
    }
    if (first.WorkspaceDiagnostics?.length) throw new Error(JSON.stringify(first));
    const analyzed = performance.now();
    const second = await service.analyze();
    if (first.Snapshot !== second.Snapshot) throw new Error("unchanged analysis lost its snapshot");
    const measurement = { name: workload.Name, openMs, warmMs: performance.now() - analyzed };
    if (workload.Name === "rpc") {
      const hover = await service.query("hover", {
        URI: uri,
        Position: { line: 2, character: 6 },
      });
      if (!hover.contents.value.includes("Main")) throw new Error("RPC entry hover is missing");
      await service.update([
        {
          Operation: "open",
          Identity: { URI: uri, ModulePath: "sample", Path: "main.mgo" },
          Version: 1,
          Text: workload.Source,
        },
      ]);
      await service.update([
        {
          Operation: "change",
          Identity: { URI: uri },
          Version: 2,
          Changes: [{ text: workload.Source.replace("Signed:41", "Signed:40") }],
        },
      ]);
      const edited = await service.analyze();
      if (edited.Snapshot === first.Snapshot) throw new Error("RPC edit reused stale analysis");
      for (const report of Object.values(edited.Diagnostics)) {
        if (report.items?.length) throw new Error(JSON.stringify(report));
      }
      if (edited.WorkspaceDiagnostics?.length) throw new Error(JSON.stringify(edited));
      const build = await service.prepare({
        Symbols: true,
        EntryPoints: [{ Name: "default", ModulePath: "sample", Function: "Main" }],
      });
      if (!build.ImageJSON || !build.SymbolsJSON)
        throw new Error("RPC build is missing image or symbols");
    }
    return measurement;
  } finally {
    await service.dispose();
  }
}
