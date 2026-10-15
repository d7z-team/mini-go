import { createRuntime } from "@d7z-team/mini-go";
import { createLanguageService } from "@d7z-team/mini-go/tools";
import { connectRPC } from "@d7z-team/mini-go/rpc";
import { LaboratoryClient } from "./generated/service";

async function run() {
  const language = await createLanguageService({ timeoutMs: 300_000 });
  let image;
  try {
    await language.open({
      Root: "sample",
      Packages: [
        {
          Namespace: "module:sample",
          ModulePath: "sample",
          Files: [{ Path: "main.mgo", Text: "package main\nfunc Main() int { return 43 }\n" }],
        },
      ],
    });
    image = (
      await language.prepare({
        EntryPoints: [{ Name: "default", ModulePath: "sample", Function: "Main" }],
      })
    ).Image;
    if (!image) throw new Error("compiler returned no image");
  } finally {
    await language.dispose();
  }
  const vm = await createRuntime(await (await fetch("./image.json")).arrayBuffer());
  let before;
  let after;
  let generation;
  try {
    const initial = vm.start("default");
    const initialData = (await initial.result).roots[0]?.data;
    if (typeof initialData !== "object" || !("Integer" in initialData))
      throw new Error("expected integer");
    before = String(initialData.Integer);
    await initial.settled;
    await vm.patch(image);
    const replacement = vm.start("default");
    const data = (await replacement.result).roots[0]?.data;
    if (typeof data !== "object" || !("Integer" in data)) throw new Error("expected integer");
    after = String(data.Integer);
    await replacement.settled;
    generation = String((await vm.stats()).generation);
  } finally {
    await vm.close();
  }
  const connection = await connectRPC(new URL("/rpc", location.href).href.replace("http", "ws"));
  let rpc;
  try {
    const client = await LaboratoryClient.bind(connection);
    try {
      const [counter] = await client.open(40n);
      if (!counter) throw new Error("RPC returned no counter");
      try {
        rpc = String(await counter.add(2n));
      } finally {
        await counter.close();
      }
    } finally {
      await client.close();
    }
  } finally {
    await connection.close();
  }
  return { before, after, generation, rpc };
}

try {
  document.body.dataset.result = JSON.stringify(await run());
} catch (error) {
  document.body.dataset.error = String(error);
}
