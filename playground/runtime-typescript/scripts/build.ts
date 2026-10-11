import { spawnSync } from "node:child_process";
import { access, cp, mkdir, readFile, rename, rm } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

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
const result = spawnSync(
  process.execPath,
  [path.join(root, "node_modules/typescript/bin/tsc"), "-p", path.join(root, "tsconfig.json")],
  { cwd: root, stdio: "inherit" },
);
if (result.error) throw result.error;
if (result.status !== 0)
  throw new Error(`TypeScript compiler exited ${result.status ?? result.signal}`);

const compilerAssets = path.join(staging, "compiler");
await mkdir(compilerAssets, { recursive: true });
await Promise.all([
  cp(compiler, path.join(compilerAssets, "mini-go-compiler.wasm")),
  cp(wasmExec, path.join(compilerAssets, "wasm_exec.js")),
]);

await mkdir(path.join(root, ".build"), { recursive: true });
// A failed TypeScript build leaves the previous distribution usable.
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
