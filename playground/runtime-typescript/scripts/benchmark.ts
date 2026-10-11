import { readFile } from "node:fs/promises";
import { gunzipSync } from "node:zlib";
import { createRuntime } from "../dist/index.node.js";
import { VirtualMachine } from "../dist/vm/vm.js";

const source = gunzipSync(
  await readFile(new URL("../../../testdata/runtime/execution.json.gz", import.meta.url)),
).toString("utf8");
const marker = /\{"name":"parallel_cpu","optimization":2,"image":/g.exec(source);
if (!marker) throw new Error("parallel_cpu O2 runtime vector is unavailable");
const start = marker.index + marker[0].length;
let depth = 0;
let quoted = false;
let escaped = false;
let end = start;
for (; end < source.length; end++) {
  const character = source[end];
  if (quoted) {
    if (escaped) escaped = false;
    else if (character === "\\") escaped = true;
    else if (character === '"') quoted = false;
  } else if (character === '"') quoted = true;
  else if (character === "{" || character === "[") depth++;
  else if (character === "}" || character === "]") {
    depth--;
    if (depth === 0) {
      end++;
      break;
    }
  }
}
const image = new TextEncoder().encode(source.slice(start, end));

async function runDirect(profile: boolean) {
  const started = performance.now();
  const vm = await VirtualMachine.create(image, { profile });
  const execution = vm.start("default", []);
  for (;;) {
    const state = vm.pump(4096);
    if (state.executions.find((item) => item.id === execution)?.settled) break;
  }
  const result = vm.result(execution);
  const elapsed = performance.now() - started;
  const stats = vm.stats();
  vm.release(execution);
  vm.close();
  return { elapsed, stats, result };
}

const direct = await runDirect(false);
const profiled = await runDirect(true);

const workerStarted = performance.now();
const worker = await createRuntime(image);
const call = worker.start("default");
const workerResult = await call.result;
await call.settled;
const workerStats = await worker.stats();
const workerElapsed = performance.now() - workerStarted;
await worker.close();

const snapshotText = (value: unknown) =>
  JSON.stringify(value, (_, item) => (typeof item === "bigint" ? item.toString() : item));
if (
  direct.stats.steps !== profiled.stats.steps ||
  direct.stats.steps !== workerStats.steps ||
  direct.stats.guestAllocations !== profiled.stats.guestAllocations ||
  direct.stats.guestAllocations !== workerStats.guestAllocations ||
  snapshotText(direct.result) !== snapshotText(profiled.result) ||
  snapshotText(direct.result) !== snapshotText(workerResult)
)
  throw new Error("benchmark paths produced different results or deterministic counters");

const report = (elapsed: number, stats: typeof direct.stats) => ({
  elapsed_ms: elapsed,
  steps: stats.steps,
  steps_per_second: Number(stats.steps) / (elapsed / 1000),
  guest_allocations: stats.guestAllocations,
  live_guest_bytes: stats.liveGuestBytes,
  total_guest_allocated_bytes: stats.totalGuestAllocatedBytes,
  peak_guest_bytes: stats.peakGuestBytes,
  opcodes: stats.profile?.opcodes,
  functions: stats.profile?.functions,
});
console.log(
  JSON.stringify(
    {
      workload: "parallel_cpu",
      compiler_id: JSON.parse(source.slice(start, end)).compiler_id,
      optimization: 2,
      direct_vm: report(direct.elapsed, direct.stats),
      profiled_direct_vm: report(profiled.elapsed, profiled.stats),
      profiling_overhead_ms: profiled.elapsed - direct.elapsed,
      worker_sdk: report(workerElapsed, workerStats),
      worker_overhead_ms: workerElapsed - direct.elapsed,
    },
    (_, value) => (typeof value === "bigint" ? value.toString() : value),
    2,
  ),
);
