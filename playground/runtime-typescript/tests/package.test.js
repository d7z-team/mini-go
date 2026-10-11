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
  { timeout: 600_000 },
  async (t) => {
    const directory = await mkdtemp(path.join(tmpdir(), "mini-go-consumer-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    // The test consumes the completed build without racing other tests' assets.
    let filename;
    let packageName;
    if (process.env.MINIGO_PACKAGE_TARBALL) {
      filename = path.basename(process.env.MINIGO_PACKAGE_TARBALL);
      await cp(process.env.MINIGO_PACKAGE_TARBALL, path.join(directory, filename));
      packageName = "@d7z-team/mini-go";
    } else {
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
        { cwd: packageRoot, signal: t.signal },
      );
      [{ filename, name: packageName }] = Object.values(JSON.parse(packed.stdout));
    }
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
      { cwd: directory, signal: t.signal },
    );
    const installed = path.join(directory, "node_modules", packageName);
    await writeFile(
      path.join(directory, "consumer.mjs"),
      `
    import { readFile, writeFile } from 'node:fs/promises';
    import { createRuntime } from '${packageName}';
    import { connectRPC } from '${packageName}/rpc';
    import { createLanguageService } from '${packageName}/tools';
    if (typeof connectRPC !== 'function') throw new Error('RPC entry point mismatch');
    const language = await createLanguageService({ timeoutMs: 300_000 });
    let image;
    try {
      await language.open({
        Root: 'sample',
        Packages: [{
          Namespace: 'module:sample',
          ModulePath: 'sample',
          Files: [{Path: 'main.mgo', Text: 'package main\\nfunc Main() int { return 42 }\\n'}],
        }],
      });
      const built = await language.prepare({
        EntryPoints: [{Name: 'default', ModulePath: 'sample', Function: 'Main'}],
      });
      image = built.Image;
      if (!(image instanceof Uint8Array)) throw new Error('compiler returned no image');
      await writeFile(new URL('./image.json', import.meta.url), image);
    } finally { await language.dispose(); }
    const vm = await createRuntime(await readFile(new URL('./image.json', import.meta.url)));
    try {
      const call = vm.start('default');
      if ((await call.result).roots[0].data.Integer !== 42n) throw new Error('package execution mismatch');
      await call.settled;
    } finally { await vm.close(); }
  `,
    );
    // A successful process exit also verifies worker/timer cleanup.
    await exec(process.execPath, ["consumer.mjs"], {
      cwd: directory,
      timeout: 300_000,
      signal: t.signal,
    });
    const image = await readFile(path.join(directory, "image.json"));
    await mkdir(path.join(directory, "generated"));
    await cp(
      path.resolve(packageRoot, "../../testdata/rpc/generated/typescript/types.ts"),
      path.join(directory, "generated/types.ts"),
    );
    await cp(
      path.resolve(packageRoot, "../../testdata/rpc/generated/typescript/service.ts"),
      path.join(directory, "generated/service.ts"),
    );
    await writeFile(
      path.join(directory, "consumer.mts"),
      `
    import { createRuntime, values, type Runtime, type Snapshot, type WorkerProvider } from '${packageName}';
    import { connectRPC, type RPCConnection } from '${packageName}/rpc';
    import { LaboratoryClient, createLaboratoryProvider } from './generated/service.js';
    import { createLanguageService, createDebugSession, attachDebugSession, type WorkspaceInput, type SourceTree, type SourcePackages } from '${packageName}/tools';
    const provider: WorkerProvider = { call: ({payload}) => ({payload, consumed: async () => {}}) };
    async function run(image: Uint8Array): Promise<Snapshot> {
      const vm: Runtime = await createRuntime(image);
      try { return await vm.start('default', [values.int(42n)]).result; } finally { await vm.close(); }
    }
    const workspace: WorkspaceInput={Root:'sample',Packages:[]};
    async function sources() {
      const service = await createLanguageService({ timeoutMs: 300_000 });
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
    void [provider, run, connectRPC, bind, generatedProvider, createLanguageService, createDebugSession, attachDebugSession, workspace, sources];
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
      { cwd: directory, signal: t.signal },
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
      browser = await chromium.launch({ headless: true, timeout: 300_000 });
      t.signal.throwIfAborted();
      const page = await browser.newPage();
      page.setDefaultTimeout(300_000);
      await page.goto(`http://127.0.0.1:${server.address().port}`);
      assert.equal(
        await page.evaluate(async () => {
          const { createRuntime } = await import("/dist/index.browser.js");
          const { connectRPC } = await import("/dist/rpc.browser.js");
          if (typeof connectRPC !== "function") throw new Error("browser RPC entry point mismatch");
          const vm = await createRuntime(await (await fetch("/image")).arrayBuffer(), {
            workerUrl: new URL("/dist/workers/runtime.browser.js", location.href),
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
