import { spawnSync } from "node:child_process";
import { cp, mkdir, readFile, rename, rm } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));
const manifest = path.resolve(root, "../Cargo.toml");
const tool = process.env.WASM_BINDGEN ?? "wasm-bindgen";
const lock = await readFile(path.resolve(root, "../Cargo.lock"), "utf8");
const packageJSON = JSON.parse(await readFile(path.join(root, "package.json"), "utf8")) as {
  name: string;
};
const expected = lock.match(/name = "wasm-bindgen"\nversion = "([^"]+)"/)?.[1];

function run(command: string, args: string[], capture = false): string {
  const result = spawnSync(command, args, {
    cwd: root,
    env: {
      ...process.env,
      ...(process.env.MINIGO_BUILD_STD === "1" ? { RUSTC_BOOTSTRAP: "1" } : {}),
    },
    encoding: "utf8",
    stdio: capture ? ["ignore", "pipe", "inherit"] : "inherit",
  });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`${command} exited ${result.status ?? result.signal}`);
  return result.stdout ?? "";
}

if (!expected || run(tool, ["--version"], true).trim() !== `wasm-bindgen ${expected}`) {
  throw new Error(`Install wasm-bindgen-cli ${expected} to match Cargo.lock`);
}
const args = process.argv.slice(2);
if (args.some((arg) => arg !== "--core")) throw new Error("Usage: npm run build -- [--core]");
run("make", ["-C", path.resolve(root, "../../.."), "runtime-compiler-image"]);
const metadata = JSON.parse(
  run(
    "cargo",
    ["metadata", "--manifest-path", manifest, "--locked", "--no-deps", "--format-version", "1"],
    true,
  ),
) as { target_directory: string };
run("cargo", [
  "build",
  ...(process.env.MINIGO_BUILD_STD === "1" ? ["-Z", "build-std=std,panic_abort"] : []),
  "--locked",
  "--manifest-path",
  manifest,
  "--target",
  "wasm32-unknown-unknown",
  "-p",
  "mini-go-wasm",
  "--release",
  ...(args.includes("--core") ? ["--no-default-features"] : []),
]);
const wasm = path.join(root, "sdk/wasm");
await rm(wasm, { recursive: true, force: true });
run(tool, [
  "--target",
  "web",
  "--out-dir",
  wasm,
  path.join(metadata.target_directory, "wasm32-unknown-unknown/release/mini_go_wasm.wasm"),
]);
const staging = path.join(root, ".build/dist");
await rm(staging, { recursive: true, force: true });
run(process.execPath, [path.join(root, "node_modules/typescript/bin/tsc")]);
run(process.execPath, [
  path.join(root, "node_modules/typescript/bin/tsc"),
  "-p",
  path.join(root, "tsconfig.bindings-build.json"),
]);
await cp(wasm, path.join(staging, "wasm"), { recursive: true });
await mkdir(path.join(staging, "tools"), { recursive: true });
await cp(
  path.resolve(root, "../assets/compiler.json.gz"),
  path.join(staging, "tools/compiler.json.gz"),
);
await mkdir(path.join(root, ".build"), { recursive: true });
// A failed Rust/TypeScript build leaves the previous distribution usable.
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
console.log(`Built ${packageJSON.name} (${args.includes("--core") ? "core" : "RPC"})`);
