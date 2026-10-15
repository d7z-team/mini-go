import http from "node:http";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { build } from "vite";
import { chromium } from "playwright";

const packageRoot = new URL("../../", import.meta.url);

// Register cleanup before launch so cancellation also covers initialization.
export async function openBrowserPage(t, url, browserType = chromium) {
  let browser;
  t.onTestFinished(async () => {
    await browser?.close();
  });
  t.signal.addEventListener("abort", () => void browser?.close(), { once: true });
  browser = await browserType.launch({ headless: true, timeout: 300_000 });
  if (t.signal.aborted) {
    await browser.close();
    t.signal.throwIfAborted();
  }
  const page = await browser.newPage();
  page.setDefaultTimeout(300_000);
  await page.goto(url);
  return page;
}

// The local asset server remains test-owned even when browser launch fails.
export async function createBrowserPage(t, routes = {}, browserType = chromium) {
  const assets = new URL("../../dist/", import.meta.url);
  const server = http.createServer(async (request, response) => {
    try {
      const name = new URL(request.url, "http://localhost").pathname;
      let source = routes[name];
      if (source === undefined) {
        if (name === "/") {
          response.setHeader("Content-Type", "text/html");
          response.end("<!doctype html>");
          return;
        }
        source = new URL(`.${name}`, assets);
        if (!source.pathname.startsWith(assets.pathname)) throw new Error("invalid asset path");
      }
      response.setHeader(
        "Content-Type",
        name === "/"
          ? "text/html"
          : name.endsWith(".js")
            ? "text/javascript"
            : name.endsWith(".wasm")
              ? "application/wasm"
              : "application/octet-stream",
      );
      response.end(source instanceof URL ? await readFile(source) : source);
    } catch (error) {
      response.statusCode = 404;
      response.end(String(error));
    }
  });
  t.onTestFinished(async () => {
    server.closeAllConnections();
    if (server.listening) await new Promise((resolve) => server.close(resolve));
  });
  t.signal.addEventListener(
    "abort",
    () => {
      server.closeAllConnections();
      server.close();
    },
    { once: true },
  );
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return openBrowserPage(t, `http://127.0.0.1:${server.address().port}`, browserType);
}

export async function readBrowserScenario(page) {
  await page.waitForFunction(() => "__miniGoResult" in globalThis || "__miniGoError" in globalThis);
  return page.evaluate(() => {
    if ("__miniGoError" in globalThis) throw new Error(globalThis.__miniGoError);
    return globalThis.__miniGoResult;
  });
}

export async function buildBrowserScenario(entry) {
  const output = await build({
    root: packageRoot.pathname,
    configFile: false,
    logLevel: "error",
    resolve: {
      alias: {
        "@d7z-team/mini-go/rpc": path.resolve(packageRoot.pathname, "dist/browser/rpc.js"),
        "@d7z-team/mini-go/tools": path.resolve(packageRoot.pathname, "dist/browser/tools.js"),
        "@d7z-team/mini-go": path.resolve(packageRoot.pathname, "dist/browser/index.js"),
      },
    },
    build: {
      write: false,
      target: "es2022",
      rolldownOptions: {
        input: entry.pathname,
        output: {
          format: "es",
          entryFileNames: "scenario.js",
          chunkFileNames: "chunks/[name]-[hash].js",
          assetFileNames: "assets/[name]-[hash][extname]",
        },
      },
    },
  });
  const routes = {};
  const bundles = Array.isArray(output) ? output : [output];
  for (const bundle of bundles) {
    for (const asset of bundle.output ?? bundle) {
      routes[`/${asset.fileName}`] = asset.type === "asset" ? asset.source : asset.code;
    }
  }
  return routes;
}
