import { createRuntime } from "@d7z-team/mini-go";
import { test } from "vitest";
import { runtimeVector } from "../tests/support/runtime-vectors.js";
import { VirtualMachine } from "../src/vm/vm";

const vector = runtimeVector("parallel_cpu", 2);
const image = vector.bytes;
const expected = JSON.parse(new TextDecoder().decode(image));

function snapshot(value) {
  return JSON.stringify(value, (_, item) => (typeof item === "bigint" ? item.toString() : item));
}

async function runDirect(profile) {
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

async function runWorker(profile) {
  const started = performance.now();
  const runtime = await createRuntime(image, { profile });
  try {
    const call = runtime.start("default");
    const result = await call.result;
    await call.settled;
    return { elapsed: performance.now() - started, stats: await runtime.stats(), result };
  } finally {
    await runtime.close();
  }
}

function report(elapsed, stats) {
  return {
    elapsed_ms: elapsed,
    steps: stats.steps,
    steps_per_second: Number(stats.steps) / (elapsed / 1000),
    guest_allocations: stats.guestAllocations,
    live_guest_bytes: stats.liveGuestBytes,
    total_guest_allocated_bytes: stats.totalGuestAllocatedBytes,
    peak_guest_bytes: stats.peakGuestBytes,
    opcodes: stats.profile?.opcodes,
    functions: stats.profile?.functions,
  };
}

test("runtime accounting diagnostics", async ({ bench }) => {
  const registration = bench(
    "direct VM, profiled VM, and built Worker SDK preserve execution accounting",
    async () => {
      const direct = await runDirect(false);
      const profiled = await runDirect(true);
      const worker = await runWorker(false);
      const profiledWorker = await runWorker(true);
      if (
        direct.stats.steps !== profiled.stats.steps ||
        direct.stats.steps !== worker.stats.steps ||
        direct.stats.steps !== profiledWorker.stats.steps ||
        direct.stats.guestAllocations !== profiled.stats.guestAllocations ||
        direct.stats.guestAllocations !== worker.stats.guestAllocations ||
        direct.stats.guestAllocations !== profiledWorker.stats.guestAllocations ||
        snapshot(direct.result) !== snapshot(profiled.result) ||
        snapshot(direct.result) !== snapshot(worker.result) ||
        snapshot(direct.result) !== snapshot(profiledWorker.result) ||
        direct.result.roots[0]?.data.Integer !== BigInt(vector.result_integer)
      ) {
        throw new Error("benchmark paths produced different results or deterministic counters");
      }
      console.log(
        JSON.stringify(
          {
            workload: "parallel_cpu",
            compiler_id: expected.compiler_id,
            optimization: vector.optimization,
            direct_vm: report(direct.elapsed, direct.stats),
            profiled_direct_vm: report(profiled.elapsed, profiled.stats),
            profiling_overhead_ms: profiled.elapsed - direct.elapsed,
            worker_sdk: report(worker.elapsed, worker.stats),
            profiled_worker_sdk: report(profiledWorker.elapsed, profiledWorker.stats),
            worker_overhead_ms: worker.elapsed - direct.elapsed,
          },
          (_, value) => (typeof value === "bigint" ? value.toString() : value),
          2,
        ),
      );
    },
  );
  await registration.run({ iterations: 1, time: 0, warmup: false });
});
