import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { planCI, isDocumentation, requestJSON } from "./changes.mjs";

async function fixture(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "mini-go-ci-changes-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const git = (...args) =>
    execFileSync("git", ["-C", directory, ...args], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
      env: {
        ...process.env,
        GIT_AUTHOR_NAME: "CI Test",
        GIT_COMMITTER_NAME: "CI Test",
        GIT_AUTHOR_EMAIL: "ci@example.invalid",
        GIT_COMMITTER_EMAIL: "ci@example.invalid",
      },
    }).trim();
  git("init", "-q", "-b", "main");
  const state = { directory, git, history: [], requests: [] };
  state.commit = async (files) => {
    for (const [name, text] of Object.entries(files)) {
      const filename = path.join(directory, name);
      await mkdir(path.dirname(filename), { recursive: true });
      if (text === null) await rm(filename);
      else await writeFile(filename, text);
    }
    git("add", "-A");
    git("commit", "-qm", "fixture");
    return git("rev-parse", "HEAD");
  };
  state.success = (sha, override = {}) => ({
    head_sha: sha,
    head_branch: "main",
    event: "push",
    status: "completed",
    conclusion: "success",
    ...override,
  });
  state.plan = (options = {}) =>
    planCI({
      directory,
      repository: "d7z-team/mini-go",
      workflow: "publish.yml",
      branch: "main",
      eventName: "push",
      event: {},
      request: async (url) => {
        state.requests.push(url);
        return { workflow_runs: state.history };
      },
      ...options,
    });
  state.base = await state.commit({
    "main.go": "package main\n",
    "README.md": "guide\n",
  });
  state.history = [state.success(state.base)];
  return state;
}

test("push skips unchanged code and documentation but validates runtime inputs", async (t) => {
  const f = await fixture(t);
  assert.equal((await f.plan()).changed, false);
  const docs = await f.commit({
    "README.md": "updated",
    "docs/reference/api.json": "{}",
  });
  assert.equal((await f.plan()).changed, false);
  f.history = [f.success(docs)];
  await f.commit({ "guide.rst": "guide" });
  assert.equal((await f.plan()).changed, false);
  await f.commit({ "testdata/input.md": "payload" });
  assert.deepEqual((await f.plan()).files, ["testdata/input.md"]);
  const query = new URL(f.requests[0]).searchParams;
  assert.equal(query.get("status"), "success");
  assert.equal(query.get("event"), "push");
});

test("documentation after failed or cancelled code CI cannot certify that code", async (t) => {
  const f = await fixture(t);
  const code = await f.commit({ "main.go": "package changed\n" });
  await f.commit({ "README.md": "next" });
  for (const conclusion of ["failure", "cancelled"]) {
    f.history = [f.success(code, { conclusion }), f.success(f.base)];
    assert.equal((await f.plan()).changed, true);
  }
  f.history = [f.success(code, { event: "pull_request" }), f.success(f.base)];
  assert.equal((await f.plan()).changed, true);
  f.history = [f.success(code)];
  assert.equal((await f.plan()).changed, false);
});

test("PR comparison uses the merge base and does not query CI history", async (t) => {
  const f = await fixture(t);
  f.git("checkout", "-qb", "topic");
  const docs = await f.commit({ "README.md": "PR docs" });
  f.git("checkout", "main");
  const base = await f.commit({ "main.go": "package base_changed\n" });
  f.git("checkout", "topic");
  const options = {
    eventName: "pull_request",
    event: { pull_request: { base: { sha: base }, head: { sha: docs } } },
  };
  assert.equal((await f.plan(options)).changed, false);
  const code = await f.commit({ "main.go": "package topic_changed\n" });
  options.event.pull_request.head.sha = code;
  assert.equal((await f.plan(options)).changed, true);
  assert.equal(f.requests.length, 0);
});

test("scheduled and manual fuzz skip code already covered by a successful run", async (t) => {
  const f = await fixture(t);
  f.history = [f.success(f.base, { event: "schedule" })];
  await f.commit({ "README.md": "next" });
  for (const eventName of ["schedule", "workflow_dispatch"]) {
    assert.equal(
      (await f.plan({ eventName, workflow: "fuzz.yml" })).changed,
      false,
    );
  }
  assert.equal(new URL(f.requests[0]).searchParams.has("event"), false);
  await f.commit({ "compiler/input.go": "package compiler\n" });
  assert.equal(
    (await f.plan({ eventName: "schedule", workflow: "fuzz.yml" })).changed,
    true,
  );
});

test("history is paginated and unrelated or missing commits cannot be baselines", async (t) => {
  const f = await fixture(t);
  await f.commit({ "README.md": "next" });
  const pages = [];
  const plan = await f.plan({
    request: async (url) => {
      const page = Number(new URL(url).searchParams.get("page"));
      pages.push(page);
      return {
        workflow_runs:
          page === 1
            ? Array.from({ length: 100 }, () =>
                f.success(f.base, { head_branch: "other" }),
              )
            : [f.success("0".repeat(40)), f.success(f.base)],
      };
    },
  });
  assert.deepEqual(pages, [1, 2]);
  assert.equal(plan.base, f.base);
  assert.equal(plan.changed, false);
  f.history = [];
  assert.equal((await f.plan()).changed, true);
});

test("source deletion remains a code change when renamed to documentation", async (t) => {
  const f = await fixture(t);
  await f.commit({ "main.go": null, "main.md": "package main\n" });
  assert.deepEqual((await f.plan()).files, ["main.go"]);
});

test("history errors fail explicitly instead of skipping validation", async (t) => {
  const f = await fixture(t);
  await assert.rejects(f.plan({ request: async () => ({}) }), /Invalid/);
  await assert.rejects(
    f.plan({
      request: async () => {
        throw new Error("API unavailable");
      },
    }),
    /API unavailable/,
  );
  await assert.rejects(
    f.plan({ eventName: "pull_request", event: {} }),
    /Invalid PR/,
  );
});

test("documentation classification uses suffixes and payload directories", () => {
  for (const name of [
    "README.md",
    "new/guide.MD",
    "guide.mdx",
    "guide.rst",
    "docs/api.json",
    "testdata/README_zh.md",
  ]) {
    assert.equal(isDocumentation(name), true, name);
  }
  for (const name of [
    "main.go",
    "lib.rs",
    "sdk.ts",
    "stdlib/errors.mgo",
    "api.mrpc",
    "Cargo.lock",
    "package.json",
    "Makefile",
    ".github/workflows/publish.yml",
    "LICENSE",
    "unknown.txt",
    "testdata/input.md",
    "compiler/testdata/input.rst",
    "tests/fixtures/page.mdx",
    "assets/page.md",
  ]) {
    assert.equal(isDocumentation(name), false, name);
  }
});

test("HTTP lookup treats only explicit allowed 404 as a missing package", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch");
  for (const status of [401, 403, 429, 500]) {
    fetch.mock.mockImplementation(
      async () => new Response("failure", { status }),
    );
    await assert.rejects(
      requestJSON("https://example.test", { allowMissing: true }),
      /HTTP/,
    );
  }
  fetch.mock.mockImplementation(
    async () => new Response("missing", { status: 404 }),
  );
  assert.equal(
    await requestJSON("https://example.test", { allowMissing: true }),
    null,
  );
  await assert.rejects(requestJSON("https://example.test"), /HTTP 404/);
  fetch.mock.mockImplementation(async () => new Response("not json"));
  await assert.rejects(requestJSON("https://example.test"), SyntaxError);
});
