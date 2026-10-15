import { createRuntime } from "/browser/index.js";

const load = async (path) => new Uint8Array(await (await fetch(path)).arrayBuffer());

try {
  const vm = await createRuntime(await load("/initial"));
  try {
    const first = vm.start("default");
    const before = (await first.result).roots[0].data.Integer;
    await first.settled;
    await vm.patch(await load("/replacement"));
    const second = vm.start("default");
    const after = (await second.result).roots[0].data.Integer;
    await second.settled;
    const stats = await vm.stats();
    globalThis.__miniGoResult = {
      before: before.toString(),
      after: after.toString(),
      generation: stats.generation.toString(),
      memory: stats.totalGuestAllocatedBytes.toString(),
    };
  } finally {
    await vm.close();
  }
} catch (error) {
  globalThis.__miniGoError = String(error?.stack ?? error);
}
