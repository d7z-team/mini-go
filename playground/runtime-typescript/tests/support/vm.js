import assert from "node:assert/strict";

/** Drive a VM until its initialization handshake completes. */
export function initializeVM(vm, label = "VM") {
  for (let count = 0; count < 100_000; count++) {
    const state = vm.pump(4096);
    if (state.error) assert.fail(`${label}: initialization failed: ${state.error}`);
    if (state.ready) return;
    assert.equal(vm.actions().length, 0, `${label}: initialization host action`);
  }
  assert.fail(`${label}: initialization did not settle within its deterministic pump budget`);
}

/** Drive one execution to settlement and return its snapshot. */
export function executeVM(vm, id) {
  for (let count = 0; count < 100_000; count++) {
    const state = vm.pump(4096);
    if (state.error) throw new Error(state.error);
    if (state.executions.find((execution) => execution.id === id)?.settled) return vm.result(id);
  }
  throw new Error("execution did not settle within its deterministic pump budget");
}
