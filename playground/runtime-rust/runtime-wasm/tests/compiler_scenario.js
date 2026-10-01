// This scenario runs unchanged in Node and browser workers.
async function compilerRequest(vm, values, request, signal) {
  const execution = vm.start(
    "default",
    [values.bytes(new TextEncoder().encode(JSON.stringify(request)))],
    { signal },
  );
  const snapshot = await execution.result;
  await execution.settled;
  const data = snapshot.roots[0].data;
  let bytes;
  if ("String" in data) bytes = data.String;
  else {
    const { storage, start, length } = data.Slice;
    if (storage.path.length) throw new Error("unexpected compiler result address");
    const backing = snapshot.objects[Number(storage.object)].data;
    bytes =
      "String" in backing
        ? backing.String
        : Uint8Array.from(backing.Array, (value) => Number(value.data.Unsigned));
    bytes = bytes.slice(Number(start), Number(start + length));
  }
  const text = new TextDecoder().decode(bytes);
  const response = JSON.parse(text);
  if (response.Image) {
    // Preserve raw numeric tokens and string escapes that participate in the
    // image hash; JS Number cannot represent every compiler integer constant.
    const field = /"Image"\s*:\s*(\{)/.exec(text);
    if (!field) throw new Error("compiler response is missing the raw image");
    const start = field.index + field[0].length - 1;
    let depth = 0;
    let quoted = false;
    for (let i = start; i < text.length; i++) {
      const c = text[i];
      if (quoted) {
        if (c === "\\") i++;
        else if (c === '"') quoted = false;
      } else if (c === '"') quoted = true;
      else if (c === "{") depth++;
      else if (c === "}" && --depth === 0) {
        response.ImageJSON = text.slice(start, i + 1);
        break;
      }
    }
    if (!response.ImageJSON) throw new Error("compiler response image is incomplete");
    delete response.Image;
  }
  return response;
}

export async function exerciseCompiler(MiniGo, values, image, signal) {
  const vm = await MiniGo.create(image, {
    workload: "compiler",
    maxSteps: 5_000_000,
    signal,
  });
  try {
    let envelope;
    for (let i = 0; i < 3; i++) {
      const request =
        i === 0
          ? {}
          : {
              Format: envelope.Format,
              Version: envelope.Version,
              Operation: "check",
              Root: "probe",
              Packages: [
                {
                  Namespace: "module:probe",
                  PackagePath: "",
                  ModulePath: "probe",
                  Files: [{ Path: "main.mgo", Text: "package main\nfunc main() {}\n" }],
                },
              ],
            };
      envelope = await compilerRequest(vm, values, request, signal);
      if (i === 0 ? !envelope.Error : envelope.Error || envelope.Diagnostics?.length) {
        throw new Error(`compiler response: ${JSON.stringify(envelope)}`);
      }
    }
    const stats = await vm.stats();
    if (stats.activeScopes !== 0n || stats.tasks !== 0n || stats.ffiCalls !== 0n)
      throw new Error("compiler work retained after settlement");
    await vm.close();
  } finally {
    vm.terminate();
  }
}

export async function exerciseCompiledRPC(MiniGo, values, image, source, address, signal) {
  const compiler = await MiniGo.create(image, { workload: "compiler", signal });
  let prepared;
  try {
    const envelope = await compilerRequest(compiler, values, {}, signal);
    prepared = await compilerRequest(
      compiler,
      values,
      {
        Format: envelope.Format,
        Version: envelope.Version,
        Operation: "prepare",
        Root: "sample",
        Packages: [
          {
            Namespace: "module:sample",
            ModulePath: "sample",
            Files: [{ Path: "main.mgo", Text: source }],
          },
        ],
        EntryPoints: [{ Name: "default", ModulePath: "sample", Function: "Main" }],
      },
      signal,
    );
    if (prepared.Error || prepared.Diagnostics?.length || !prepared.ImageJSON)
      throw new Error(`RPC compilation failed: ${JSON.stringify(prepared)}`);
    await compiler.close();
  } finally {
    compiler.terminate();
  }
  const vm = await MiniGo.create(new TextEncoder().encode(prepared.ImageJSON), {
    rpcUrl: `${address.replace("http", "ws")}/rpc`,
    signal,
  });
  try {
    const execution = vm.start("default", [], { signal });
    const result = await execution.result;
    await execution.settled;
    if (result.roots[0].data.Integer !== 42n) throw new Error("compiled RPC result must be 42");
    const stats = await vm.stats();
    if (stats.activeScopes !== 0n || stats.tasks !== 0n || stats.ffiCalls !== 0n)
      throw new Error("compiled RPC retained work after settlement");
    await vm.close();
  } finally {
    vm.terminate();
  }
}
