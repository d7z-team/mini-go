import { access, mkdir, readFile, rename, rm } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "vite";
import { rolldown } from "rolldown";
import { dts } from "rolldown-plugin-dts";

const root = fileURLToPath(new URL("../", import.meta.url));
const packageJSON = JSON.parse(await readFile(path.join(root, "package.json"), "utf8")) as {
  name: string;
};

if (process.argv.length !== 2) throw new Error("Usage: npm run build");

const staging = path.join(root, ".build/dist");
const compilerDirectory = path.join(root, ".build/compiler");
const compiler = path.join(compilerDirectory, "mini-go-compiler.wasm");
const wasmExec = path.join(compilerDirectory, "wasm_exec.js");
try {
  await Promise.all([access(compiler), access(wasmExec)]);
} catch {
  throw new Error(
    "compiler assets are unavailable; run make build-typescript from the repository root",
  );
}

await rm(staging, { recursive: true, force: true });
for (const mode of ["browser", "node"]) {
  await build({ root, mode, configFile: path.join(root, "vite.config.ts") });
}
const declarations = await rolldown({
  cwd: root,
  input: { index: "src/index.ts", rpc: "src/rpc/index.ts", tools: "src/tools.ts" },
  plugins: [dts({ generator: "tsc", emitDtsOnly: true })],
});
try {
  await declarations.write({
    dir: path.join(staging, "types"),
    format: "es",
    entryFileNames: "[name].js",
    chunkFileNames: "[name].d.ts",
  });
} finally {
  await declarations.close();
}

await mkdir(path.join(root, ".build"), { recursive: true });
// Publish the complete JS, assets and declaration graph together.
const backup = path.join(root, ".build/previous");
await rm(backup, { recursive: true, force: true });
try {
  await rename(path.join(root, "dist"), backup);
} catch (error) {
  if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
}
try {
  await rename(staging, path.join(root, "dist"));
} catch (error) {
  await rename(backup, path.join(root, "dist")).catch(() => {});
  throw error;
}
await rm(backup, { recursive: true, force: true });
console.log(`Built ${packageJSON.name}`);
