export async function exerciseCompilerWorkloads(tools, workloads) {
  const measurements = [];
  for (const workload of workloads) {
    const service = await tools.createLanguageService();
    try {
      const started = performance.now();
      await service.open({
        Root: "sample",
        Packages: [
          {
            Namespace: "module:sample",
            ModulePath: "sample",
            Files: [{ Path: "main.mgo", Text: workload.Source }],
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
      if (first.Snapshot !== second.Snapshot)
        throw new Error("unchanged analysis lost its snapshot");
      measurements.push({ name: workload.Name, openMs, warmMs: performance.now() - analyzed });
    } finally {
      await service.dispose();
    }
  }
  return measurements;
}
