import test from "node:test";
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import http from "node:http";
import { chromium } from "playwright";

const exec = promisify(execFile);
const packageRoot = fileURLToPath(new URL("../", import.meta.url));

test(
  "packed package runs outside the repository in Node and browser",
  { timeout: 60_000 },
  async (t) => {
    const directory = await mkdtemp(path.join(tmpdir(), "mini-go-consumer-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    // The test consumes the completed build without racing other tests' assets.
    const packed = await exec(
      "npm",
      [
        "pack",
        "--ignore-scripts",
        "--json",
        "--cache",
        path.join(directory, ".npm-cache"),
        "--pack-destination",
        directory,
      ],
      { cwd: packageRoot },
    );
    const [{ filename, name: packageName }] = Object.values(JSON.parse(packed.stdout));
    await writeFile(
      path.join(directory, "package.json"),
      JSON.stringify({ private: true, type: "module" }),
    );
    await exec(
      "npm",
      [
        "install",
        "--offline",
        "--no-audit",
        "--no-fund",
        "--cache",
        path.join(directory, ".npm-cache"),
        path.join(directory, filename),
      ],
      { cwd: directory },
    );
    const installed = path.join(directory, "node_modules", packageName);
    const image = await readFile(path.join(process.env.MINIGO_WASM_FIXTURES, "answer.json"));
    await writeFile(path.join(directory, "image.json"), image);
    await writeFile(
      path.join(directory, "consumer.mjs"),
      `
    import { readFile } from 'node:fs/promises';
    import { MiniGo, RPC as RootRPC } from '${packageName}';
    import { RPC } from '${packageName}/rpc';
    import { createLanguageService } from '${packageName}/tools';
    if (RPC !== RootRPC || typeof RPC.connect !== 'function') throw new Error('RPC export mismatch');
    const language = await createLanguageService();
    await language.dispose();
    const vm = await MiniGo.create(await readFile(new URL('./image.json', import.meta.url)));
    try {
      const call = vm.start('default');
      if ((await call.result).roots[0].data.Integer !== 42n) throw new Error('package execution mismatch');
      await call.settled;
    } finally { await vm.close(); }
  `,
    );
    // A successful process exit also verifies worker/timer cleanup.
    await exec(process.execPath, ["consumer.mjs"], { cwd: directory, timeout: 15_000 });
    await mkdir(path.join(directory, "generated"));
    await cp(
      path.resolve(packageRoot, "../../../testdata/rpc/generated/typescript/types.ts"),
      path.join(directory, "generated/types.ts"),
    );
    await cp(
      path.resolve(packageRoot, "../../../testdata/rpc/generated/typescript/service.ts"),
      path.join(directory, "generated/service.ts"),
    );
    await writeFile(
      path.join(directory, "consumer.mts"),
      `
    import { MiniGo, RPC as RootRPC, values, type Snapshot, type WorkerProvider } from '${packageName}';
    import { RPC, type RPCConnection } from '${packageName}/rpc';
    import { LaboratoryClient, createLaboratoryProvider } from './generated/service.js';
    import { MiniGo as BrowserMiniGo } from '${packageName}/browser';
    import { MiniGo as NodeMiniGo } from '${packageName}/node';
    import { createLanguageService, DebugSession, type WorkspaceInput, type SourceTree, type SourcePackages } from '${packageName}/tools';
    const provider: WorkerProvider = { call: ({payload}) => ({payload, consumed: async () => {}}) };
    async function run(image: Uint8Array): Promise<Snapshot> {
      const vm: MiniGo = await MiniGo.create(image);
      try { return await vm.start('default', [values.int(42n)]).result; } finally { await vm.close(); }
    }
    const workspace: WorkspaceInput={Root:'sample',Packages:[]};
    async function sources() {
      const service = await createLanguageService();
      const trees: SourceTree[] = [{ModulePath:'app', Files:[]}];
      try { const result: SourcePackages = await service.sources(trees); return result.Packages; }
      finally { await service.dispose(); }
    }
    async function bind(connection: RPCConnection) { return LaboratoryClient.bind(connection); }
    const generatedProvider = createLaboratoryProvider({
      echo: (_context, value) => value,
      tree: (_context, value) => value,
      open: () => [null, {label: 'none', mode: 0}],
      read: () => 0n,
      wait: () => 0n,
    });
    void [provider, run, BrowserMiniGo, NodeMiniGo, RootRPC, RPC, bind, generatedProvider, createLanguageService, DebugSession, workspace, sources];
  `,
    );
    await exec(
      process.execPath,
      [
        path.join(packageRoot, "node_modules/typescript/bin/tsc"),
        "--noEmit",
        "--strict",
        "--module",
        "NodeNext",
        "--target",
        "ES2022",
        "consumer.mts",
      ],
      { cwd: directory },
    );
    let browser;
    const server = http.createServer(async (request, response) => {
      try {
        const pathname = new URL(request.url, "http://localhost").pathname;
        if (pathname === "/") {
          response.setHeader("Content-Type", "text/html");
          response.end("<!doctype html><title>External package consumer</title>");
          return;
        }
        if (pathname === "/image") {
          response.end(image);
          return;
        }
        const filename = path.resolve(installed, "." + decodeURIComponent(pathname));
        if (!filename.startsWith(installed + path.sep)) throw new Error("invalid path");
        response.setHeader(
          "Content-Type",
          filename.endsWith(".wasm") ? "application/wasm" : "text/javascript",
        );
        response.end(await readFile(filename));
      } catch (error) {
        response.statusCode = 404;
        response.end(String(error));
      }
    });
    t.signal.addEventListener("abort", () => {
      void browser?.close();
      server.closeAllConnections();
      server.close();
    });
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    try {
      browser = await chromium.launch({ headless: true });
      const page = await browser.newPage();
      await page.goto(`http://127.0.0.1:${server.address().port}`);
      assert.equal(
        await page.evaluate(async () => {
          const { MiniGo, RPC: RootRPC } = await import("/dist/browser.js");
          const { RPC } = await import("/dist/browser-rpc.js");
          if (RPC !== RootRPC || typeof RPC.connect !== "function")
            throw new Error("browser RPC export mismatch");
          const vm = await MiniGo.create(await (await fetch("/image")).arrayBuffer(), {
            workerUrl: new URL("/dist/browser-worker.js", location.href),
            wasmUrl: new URL("/dist/wasm/mini_go_wasm_bg.wasm", location.href),
          });
          try {
            const call = vm.start("default");
            const value = await call.result;
            await call.settled;
            return String(value.roots[0].data.Integer);
          } finally {
            await vm.close();
          }
        }),
        "42",
      );
    } finally {
      await browser?.close();
      await new Promise((resolve) => server.close(resolve));
    }
  },
);
