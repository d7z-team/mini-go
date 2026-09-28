import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { planRelease, waitForCI } from "./release-plan.mjs";
import { isDocumentation, requestJSON } from "./changes.mjs";

const scripts = fileURLToPath(new URL("../../scripts/", import.meta.url));

function run(command, parameters, cwd) {
  return execFileSync(command, parameters, {
    cwd,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: "Release Test",
      GIT_AUTHOR_EMAIL: "release@example.invalid",
      GIT_COMMITTER_NAME: "Release Test",
      GIT_COMMITTER_EMAIL: "release@example.invalid",
    },
  }).trim();
}

async function releaseFixture(t) {
  const directory = await mkdtemp(path.join(tmpdir(), "mini-go-release-plan-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  run("git", ["init", "-q", "-b", "main"], directory);
  const fixture = { directory, ci: {}, npm: [], crates: [], requests: [] };
  fixture.commit = async (files, removed = []) => {
    for (const [name, data] of Object.entries(files)) {
      await mkdir(path.dirname(path.join(directory, name)), {
        recursive: true,
      });
      await writeFile(path.join(directory, name), data);
    }
    for (const name of removed) await rm(path.join(directory, name));
    run("git", ["add", "-A"], directory);
    run("git", ["commit", "-qm", "fixture"], directory);
    run("git", ["update-ref", "refs/remotes/origin/main", "HEAD"], directory);
    fixture.release = JSON.parse(
      run(
        process.execPath,
        [
          path.join(scripts, "release-version.mjs"),
          "--json",
          "--repository",
          directory,
        ],
        directory,
      ),
    );
    fixture.event = {
      repository: { full_name: "d7z-team/mini-go" },
      ref: "refs/heads/main",
      after: fixture.release.commit,
      deleted: false,
    };
    return fixture.release;
  };
  fixture.request = async (url) => {
    fixture.requests.push(url);
    if (url.endsWith("/git/ref/heads/main"))
      return { object: { sha: fixture.release.commit } };
    if (url.startsWith("https://api.github.com/")) {
      const workflow = /workflows\/([^/]+)\//.exec(url)[1];
      const query = new URL(url).searchParams;
      assert.equal(query.get("head_sha"), fixture.release.commit);
      assert.equal(query.get("branch"), "main");
      assert.equal(query.get("event"), "push");
      const override = fixture.ci[workflow];
      return {
        workflow_runs:
          override === null
            ? []
            : [
                {
                  head_sha: fixture.release.commit,
                  head_branch: "main",
                  event: "push",
                  status: "completed",
                  conclusion: "success",
                  ...override,
                },
              ],
      };
    }
    if (url.startsWith("https://registry.npmjs.org/")) {
      return {
        versions: Object.fromEntries(
          fixture.npm.map((version) => [version, {}]),
        ),
      };
    }
    if (url.startsWith("https://crates.io/")) {
      const page = Number(new URL(url).searchParams.get("seek") ?? 0);
      return {
        versions: fixture.crates[page].map((num) => ({ num })),
        meta: {
          next_page:
            page + 1 < fixture.crates.length ? `?seek=${page + 1}` : null,
        },
      };
    }
    throw new Error(`Unexpected request ${url}`);
  };
  fixture.plan = (options = {}) =>
    planRelease({
      directory,
      repository: "d7z-team/mini-go",
      event: fixture.event,
      eventName: "push",
      request: fixture.request,
      ...options,
    });
  fixture.wait = (options = {}) =>
    waitForCI({
      repository: "d7z-team/mini-go",
      sha: fixture.release.commit,
      request: fixture.request,
      pause: async () => {
        throw new Error("Unexpected CI wait");
      },
      ...options,
    });
  fixture.base = await fixture.commit({
    "main.go": "package main\n",
    "README.md": "guide\n",
  });
  fixture.npm = [fixture.base.version];
  fixture.crates = [[fixture.base.version]];
  return fixture;
}

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

test("release waits for both exact-commit push workflows", async (t) => {
  const fixture = await releaseFixture(t);
  await fixture.commit({ "main.go": "package changed\n" });
  for (const workflow of ["go-test.yml", "runtime-rust.yml"]) {
    for (const state of [
      null,
      { status: "queued" },
      { status: "in_progress" },
    ]) {
      fixture.ci[workflow] = state;
      let polls = 0;
      assert.equal(
        (
          await fixture.wait({
            pause: async (ms) => {
              assert.equal(ms, 30_000);
              polls++;
              delete fixture.ci[workflow];
            },
          })
        ).publish,
        true,
      );
      assert.equal(polls, 1);
    }
    for (const state of [
      { head_sha: "other" },
      { head_branch: "topic" },
      { event: "pull_request" },
    ]) {
      fixture.ci[workflow] = state;
      await assert.rejects(fixture.wait(), /does not match/);
    }
    for (const conclusion of ["failure", "cancelled", "timed_out", "skipped"]) {
      fixture.ci[workflow] = { conclusion };
      assert.equal((await fixture.wait()).publish, false);
    }
    delete fixture.ci[workflow];
  }
  assert.equal((await fixture.wait()).publish, true);
});

test("CI wait stops on a newer main, a bounded deadline, or an API error", async (t) => {
  const fixture = await releaseFixture(t);
  const sha = fixture.release.commit;
  await fixture.commit({ "main.go": "package next\n" });
  assert.match((await fixture.wait({ sha })).reason, /superseded/);
  fixture.ci["go-test.yml"] = null;
  let clock = 0;
  await assert.rejects(
    fixture.wait({
      now: () => clock,
      pause: async () => {
        clock = 50 * 60_000;
      },
    }),
    /Timed out/,
  );
  for (const request of [
    async () => {
      throw new Error("request failed");
    },
    async () => ({}),
  ]) {
    await assert.rejects(fixture.wait({ request }), /request failed|Invalid/);
  }
  await assert.rejects(
    fixture.wait({
      request: async (url) =>
        url.includes("/workflows/") ? {} : fixture.request(url),
    }),
    /Invalid CI/,
  );
});

test("only trusted current main candidates reach registry planning", async (t) => {
  const fixture = await releaseFixture(t);
  const event = structuredClone(fixture.event);
  for (const override of [
    { ref: "refs/heads/topic" },
    { deleted: true },
    { repository: { full_name: "fork/mini-go" } },
  ]) {
    fixture.event = {
      ...event,
      ...override,
    };
    assert.equal((await fixture.plan()).publish, false);
  }
  fixture.event = event;
  assert.equal(
    (await fixture.plan({ eventName: "pull_request" })).publish,
    false,
  );
  assert.equal(
    (await fixture.plan({ repository: "fork/mini-go" })).publish,
    false,
  );
  assert.equal(fixture.requests.length, 0);
  await fixture.commit({ "README.md": "next\n" });
  await assert.rejects(fixture.plan({ event }), /Checkout does not match/);
  run("git", ["checkout", "--detach", fixture.base.commit], fixture.directory);
  fixture.event = event;
  assert.match((await fixture.plan()).reason, /superseded/);
  fixture.event = {
    ...event,
    after: "bad",
  };
  await assert.rejects(fixture.plan(), /Invalid release commit/);
});

test("release planning rejects uncommitted inputs before querying registries", async (t) => {
  const fixture = await releaseFixture(t);
  await writeFile(path.join(fixture.directory, "main.go"), "package dirty\n");
  await assert.rejects(fixture.plan(), /clean worktree/);
  assert.equal(fixture.requests.length, 0);
});

test("planning compares the published tree across documentation commits and reverts", async (t) => {
  const fixture = await releaseFixture(t);
  await fixture.commit({ "new/guide.md": "guide\n", "docs/api.json": "{}" });
  assert.equal((await fixture.plan()).publish, false);
  await fixture.commit({ "main.go": "package changed\n" });
  await fixture.commit({ "README.md": "updated\n" });
  const plan = await fixture.plan();
  assert.equal(plan.publish, true);
  assert.equal(plan.base, fixture.base.commit);
  assert.deepEqual(plan.changed, ["main.go"]);
  await fixture.commit({ "main.go": "package main\n" });
  assert.equal((await fixture.plan()).publish, false);
});

test("source deletion and renaming to a document still require publishing", async (t) => {
  const fixture = await releaseFixture(t);
  await fixture.commit({ "main.md": "package main\n" }, ["main.go"]);
  const plan = await fixture.plan();
  assert.equal(plan.publish, true);
  assert.deepEqual(plan.changed, ["main.go"]);
});

test("registry versions handle first, partial, repeated and paginated publication", async (t) => {
  const fixture = await releaseFixture(t);
  const target = await fixture.commit({ "main.go": "package changed\n" });
  fixture.npm = [];
  fixture.crates = [[]];
  assert.match((await fixture.plan()).reason, /No common/);
  fixture.npm = [target.version];
  assert.match((await fixture.plan()).reason, /partially/);
  fixture.npm = [];
  fixture.crates = [[target.version]];
  assert.match((await fixture.plan()).reason, /partially/);
  fixture.npm = [target.version];
  assert.equal((await fixture.plan()).publish, false);
  assert.equal((await fixture.plan()).publish, false);
  fixture.npm = [fixture.base.version, "0.0.10"];
  fixture.crates = [["0.0.10"], [fixture.base.version]];
  assert.equal((await fixture.plan()).base, fixture.base.commit);
  assert.ok(fixture.requests.some((url) => url.endsWith("?seek=1")));
});

test("published identities must match the candidate history", async (t) => {
  const fixture = await releaseFixture(t);
  await fixture.commit({ "main.go": "package changed\n" });
  for (const version of [
    `0.0.999-git.g${fixture.base.shortCommit}`,
    "0.0.1-git.g0000000",
  ]) {
    fixture.npm = [version];
    fixture.crates = [[version]];
    await assert.rejects(fixture.plan(), /Published commit/);
  }
  const candidate = fixture.release;
  run("git", ["checkout", "--detach", fixture.base.commit], fixture.directory);
  const other = await fixture.commit({ "side.rs": "fn side() {}" });
  fixture.npm = [other.version];
  fixture.crates = [[other.version]];
  run("git", ["checkout", "main"], fixture.directory);
  run(
    "git",
    ["update-ref", "refs/remotes/origin/main", "HEAD"],
    fixture.directory,
  );
  fixture.release = candidate;
  fixture.event.after = candidate.commit;
  await assert.rejects(fixture.plan());
});

test("registry failures cannot become an empty publication baseline", async (t) => {
  const fixture = await releaseFixture(t);
  await fixture.commit({ "main.go": "package changed\n" });
  for (const host of ["registry.npmjs.org", "crates.io"]) {
    await assert.rejects(
      fixture.plan({
        request: async (url) => {
          if (new URL(url).hostname === host) throw new Error("request failed");
          return fixture.request(url);
        },
      }),
      /request failed/,
    );
    await assert.rejects(
      fixture.plan({
        request: async (url) => {
          return new URL(url).hostname === host ? {} : fixture.request(url);
        },
      }),
      /Invalid/,
    );
  }
  await assert.rejects(
    fixture.plan({
      request: async (url) => {
        return url.startsWith("https://crates.io/")
          ? { versions: [], meta: { next_page: "?per_page=100" } }
          : fixture.request(url);
      },
    }),
    /Repeated/,
  );
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
