import * as tools from "/browser/tools.js";
import { exerciseCompilerWorkload } from "/workloads.js";

try {
  const workload = await (await fetch("/workload.json")).json();
  globalThis.__miniGoResult = await exerciseCompilerWorkload(tools, workload);
} catch (error) {
  globalThis.__miniGoError = String(error?.stack ?? error);
}
