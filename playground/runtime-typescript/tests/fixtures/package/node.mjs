import assert from "node:assert/strict";
import { writeFile } from "node:fs/promises";
import { createRuntime } from "@d7z-team/mini-go";
import { createLanguageService } from "@d7z-team/mini-go/tools";

const language = await createLanguageService({ timeoutMs: 300_000 });
let image;
try {
  await language.open({
    Root: "sample",
    Packages: [
      {
        Namespace: "module:sample",
        ModulePath: "sample",
        Files: [{ Path: "main.mgo", Text: "package main\nfunc Main() int { return 41 }\n" }],
      },
    ],
  });
  image = (
    await language.prepare({
      EntryPoints: [{ Name: "default", ModulePath: "sample", Function: "Main" }],
    })
  ).Image;
  assert.ok(image instanceof Uint8Array);
  await writeFile(new URL("./public/image.json", import.meta.url), image);
} finally {
  await language.dispose();
}
const vm = await createRuntime(image);
try {
  const call = vm.start("default");
  assert.equal((await call.result).roots[0].data.Integer, 41n);
  await call.settled;
} finally {
  await vm.close();
}
