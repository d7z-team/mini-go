import { test } from "vitest";
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { promisify } from "node:util";
import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import http from "node:http";
import { chromium } from "playwright";
import { build, createServer, preview } from "vite";
import { startPeer } from "./support/peer";

const exec = promisify(execFile);
const packageRoot = path.resolve(import.meta.dirname, "..");

test(
  "installed package runs in Node, browsers and Vite consumers",
  { timeout: 600_000 },
  async (t) => {
    const directory = await mkdtemp(path.join(tmpdir(), "mini-go-consumer-"));
    let browser;
    let dev;
    let production;
    let server;
    // Register ownership before initialization; failure and cancellation clean up the same resources.
    const closeServers = async () => {
      await browser?.close();
      await dev?.close();
      await production?.close();
      if (server?.listening) {
        server.closeAllConnections();
        await new Promise((resolve) => server.close(resolve));
      }
    };
    t.signal.addEventListener("abort", () => void closeServers(), { once: true });
    t.onTestFinished(async () => {
      await closeServers();
      await rm(directory, { recursive: true, force: true });
    });

    let tarball = process.env.MINIGO_PACKAGE_TARBALL;
    if (!tarball) {
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
      tarball = path.join(directory, Object.values(JSON.parse(packed.stdout))[0].filename);
    }
    await cp(new URL("./fixtures/package/", import.meta.url), directory, { recursive: true });
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
        tarball,
      ],
      { cwd: directory, signal: t.signal },
    );
    const installed = path.join(directory, "node_modules/@d7z-team/mini-go");
    const hash = (bytes) => createHash("sha256").update(bytes).digest("hex");
    assert.equal(
      hash(await readFile(path.join(installed, "dist/assets/mini-go-compiler.wasm"))),
      hash(await readFile(path.join(packageRoot, ".build/compiler/mini-go-compiler.wasm"))),
      "bundling must preserve compiler WASM bytes",
    );
    await mkdir(path.join(directory, "public"));
    // This external process must terminate with no live Worker or timer after compiling and running.
    await exec(process.execPath, ["node.mjs"], {
      cwd: directory,
      timeout: 300_000,
      signal: t.signal,
    });
    await cp(
      path.resolve(packageRoot, "../../testdata/rpc/generated/typescript"),
      path.join(directory, "generated"),
      { recursive: true },
    );
    const tsc = path.join(packageRoot, "node_modules/typescript/bin/tsc");
    for (const [resolution, files] of [
      ["NodeNext", ["types.ts"]],
      ["Bundler", ["types.ts", "bindings.ts", "main.ts"]],
    ]) {
      await exec(
        process.execPath,
        [
          tsc,
          "--noEmit",
          "--strict",
          "--target",
          "ES2022",
          "--module",
          resolution === "Bundler" ? "ESNext" : "NodeNext",
          "--moduleResolution",
          resolution,
          ...files,
        ],
        { cwd: directory, signal: t.signal },
      );
    }

    const address = await startPeer(t);
    browser = await chromium.launch({ headless: true, timeout: 300_000 });
    t.signal.throwIfAborted();
    const visit = async (url) => {
      const page = await browser.newPage();
      page.setDefaultTimeout(300_000);
      const failures = [];
      page.on("pageerror", (error) => failures.push(error.message));
      try {
        await page.goto(url);
        await page.waitForFunction(
          () =>
            document.body.dataset.result !== undefined || document.body.dataset.error !== undefined,
        );
        const outcome = await page.evaluate(() => ({
          result: document.body.dataset.result,
          error: document.body.dataset.error,
        }));
        assert.deepEqual(failures, []);
        assert.equal(outcome.error, undefined);
        return JSON.parse(outcome.result);
      } finally {
        await page.close();
      }
    };

    // Direct browser consumption also resolves assets below a non-root path.
    server = http.createServer(async (request, response) => {
      try {
        const pathname = decodeURIComponent(new URL(request.url, "http://localhost").pathname);
        if (!pathname.startsWith("/nested/")) throw new Error("unknown route");
        const filename = path.resolve(directory, pathname.slice("/nested/".length));
        if (!filename.startsWith(directory + path.sep)) throw new Error("invalid path");
        response.setHeader(
          "Content-Type",
          filename.endsWith(".wasm")
            ? "application/wasm"
            : filename.endsWith(".js") || filename.endsWith(".mjs")
              ? "text/javascript"
              : filename.endsWith(".html")
                ? "text/html"
                : "application/octet-stream",
        );
        response.end(await readFile(filename));
      } catch (error) {
        response.statusCode = 404;
        response.end(String(error));
      }
    });
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const base = `http://127.0.0.1:${server.address().port}/nested/`;
    assert.equal(await visit(`${base}plain.html`), "41");

    dev = await createServer({
      configFile: false,
      root: directory,
      logLevel: "error",
      server: { host: "127.0.0.1", port: 0, proxy: { "/rpc": { target: address, ws: true } } },
    });
    await dev.listen();
    const expected = { before: "41", after: "43", generation: "2", rpc: "42" };
    assert.deepEqual(await visit(dev.resolvedUrls.local[0]), expected);
    await dev.close();
    dev = undefined;
    await build({
      configFile: false,
      root: directory,
      base: "./",
      logLevel: "error",
      build: { target: "es2022" },
    });
    production = await preview({
      configFile: false,
      root: directory,
      base: "/nested/",
      logLevel: "error",
      preview: { host: "127.0.0.1", port: 0, proxy: { "/rpc": { target: address, ws: true } } },
    });
    assert.deepEqual(await visit(production.resolvedUrls.local[0]), expected);
  },
);
