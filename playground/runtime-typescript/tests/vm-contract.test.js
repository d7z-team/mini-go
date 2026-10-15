import assert from "node:assert/strict";
import { test } from "vitest";
import { GuestMemory } from "../src/vm/guest-memory";
import { VirtualMachine } from "../src/vm/vm";
import { allRuntimeVectors, runtimeVector, sealImage } from "./support/runtime-vectors.js";

function execute(vm, id) {
  for (let count = 0; count < 100_000; count++) {
    const state = vm.pump(4096);
    if (state.executions.find((item) => item.id === id)?.settled) return vm.result(id);
  }
  assert.fail("execution did not settle within its deterministic pump budget");
}

function initialize(vm, label) {
  for (let count = 0; count < 100_000; count++) {
    const state = vm.pump(4096);
    if (state.error) assert.fail(`${label}: initialization failed: ${state.error}`);
    if (state.ready) return;
    assert.equal(vm.actions().length, 0, `${label}: initialization host action`);
  }
  assert.fail(`${label}: initialization did not settle within its deterministic pump budget`);
}

test("direct VM executes every shared optimization corpus", async () => {
  for (const vector of allRuntimeVectors) {
    const vm = await VirtualMachine.create(vector.bytes, {});
    initialize(vm, `${vector.name} O${vector.optimization}`);
    const execution = vm.start("default", []);
    let state;
    for (let count = 0; count < 100_000; count++) {
      state = vm.pump(4096);
      if (state.executions.find((item) => item.id === execution)?.settled) break;
      assert.equal(vm.actions().length, 0, `${vector.name} O${vector.optimization}: host action`);
    }
    assert.equal(
      vm.result(execution).roots[0].data.Integer,
      BigInt(vector.result_integer),
      `${vector.name} O${vector.optimization}`,
    );
    vm.release(execution);
    vm.close();
    vm.pump(1);
    assert.equal(vm.stats().liveGuestBytes, 0n, `${vector.name} O${vector.optimization}: release`);
  }
});

test("execution image decoder enforces the current object schema", async () => {
  const source = new TextDecoder().decode(runtimeVector("arithmetic").bytes);
  for (const [name, input, pattern] of [
    ["duplicate", source.replace("{", '{"format":"duplicate",'), /duplicate object key format/],
    ["case", source.replace('"format"', '"Format"'), /unknown field Format/],
    ["unknown", source.replace("{", '{"unknown":true,'), /unknown field unknown/],
    ["null", source.replace(/"version":[0-9]+/, '"version":null'), /expected integer/],
  ]) {
    await assert.rejects(VirtualMachine.create(new TextEncoder().encode(input), {}), pattern, name);
  }
});

test("label instructions retain their zero-operand contract after preparation", async () => {
  const image = JSON.parse(new TextDecoder().decode(runtimeVector("arithmetic").bytes));
  const artifact = Object.values(image.packages)[0].artifact;
  const functionWithLabel = artifact.functions.find((fn) =>
    fn.code?.instructions?.some(([opcode]) => opcode === 14),
  );
  assert.ok(functionWithLabel);
  const instruction = functionWithLabel.code.instructions.find(([opcode]) => opcode === 14);
  assert.ok(instruction);
  instruction[2] = functionWithLabel.code.operands.length;
  functionWithLabel.code.operands.push({ inputs: [[0, 0]] });
  await assert.rejects(
    VirtualMachine.create(sealImage(image), {}),
    /operand arity mismatch/,
    "a label must not silently discard operands when labels are stripped",
  );
});

test("fused index and length opcodes keep their operand contracts", async () => {
  const validIndexVM = await VirtualMachine.create(runtimeVector("index_convert").bytes, {});
  initialize(validIndexVM, "index_convert");
  const validIndexExecution = validIndexVM.start("default", []);
  assert.equal(execute(validIndexVM, validIndexExecution).roots[0].data.Integer, 109n);
  validIndexVM.close();
  validIndexVM.pump(1);

  const validLengthVM = await VirtualMachine.create(runtimeVector("len_compare_branch").bytes, {});
  initialize(validLengthVM, "len_compare_branch");
  const validLengthExecution = validLengthVM.start("default", []);
  assert.equal(execute(validLengthVM, validLengthExecution).roots[0].data.Integer, 7n);
  validLengthVM.close();
  validLengthVM.pump(1);

  const indexImage = JSON.parse(new TextDecoder().decode(runtimeVector("index_convert").bytes));
  const indexArtifact = Object.values(indexImage.packages)[0].artifact;
  const indexFunction = indexArtifact.functions.find((fn) => fn.id === "fn.Main");
  const indexInstruction = indexFunction.code.instructions.find(([opcode]) => opcode === 70);
  assert.ok(indexInstruction);
  const indexType = indexFunction.code.descriptors.type[indexInstruction[1]];
  indexType.type = {
    kind: 3,
    primitive: 15,
    named: { module_path: "", decl_id: "" },
  };
  await assert.rejects(
    VirtualMachine.create(sealImage(indexImage), {}),
    /index conversion target must be an integer/,
  );

  const lengthImage = JSON.parse(
    new TextDecoder().decode(runtimeVector("len_compare_branch").bytes),
  );
  const lengthArtifact = Object.values(lengthImage.packages)[0].artifact;
  const lengthFunction = lengthArtifact.functions.find((fn) => fn.id === "fn.Main");
  const lengthInstruction = lengthFunction.code.instructions.find(([opcode]) => opcode === 71);
  assert.ok(lengthInstruction);
  const lengthOperands = lengthFunction.code.operands[lengthInstruction[2]];
  lengthOperands.inputs[0] = [1, 4];
  await assert.rejects(
    (async () => {
      const vm = await VirtualMachine.create(sealImage(lengthImage), {});
      initialize(vm, "invalid len_compare_branch object");
      execute(vm, vm.start("default", []));
    })(),
    /length comparison requires a length-bearing object/,
  );
});

test("fused length branches compare integer thresholds without narrowing", async () => {
  const integer = { kind: 3, primitive: 3, named: { module_path: "", decl_id: "" } };
  const string = { ...integer, primitive: 2 };
  for (const [primitive, threshold, expected] of [
    [4, -1, 0n],
    [6, 3, 0n],
    [9, 4, 1n],
    [12, Number.MAX_SAFE_INTEGER, 1n],
  ]) {
    const image = JSON.parse(new TextDecoder().decode(runtimeVector("len_compare_branch").bytes));
    const artifact = Object.values(image.packages)[0].artifact;
    artifact.constants = [
      { id: "text", type: string, value: "abc" },
      { id: "limit", type: { ...integer, primitive }, value: threshold },
      { id: "false", type: integer, value: 0 },
      { id: "true", type: integer, value: 1 },
    ];
    const fn = artifact.functions.find((item) => item.id === "fn.Main");
    fn.code = {
      instructions: [
        [71, 0, 0],
        [17, 0, 1],
        [14, 0, 2],
        [17, 0, 3],
      ],
      descriptors: {
        comparebranch: [{ operator: "<", type: integer, label: "done", when: true }],
        label: [{ label: "done" }],
        return: [{ result_count: 1 }],
      },
      operands: [
        {
          inputs: [
            [1, 0],
            [1, 1],
          ],
        },
        { inputs: [[1, 2]] },
        {},
        { inputs: [[1, 3]] },
      ],
    };
    const vm = await VirtualMachine.create(sealImage(image), {});
    initialize(vm, "integer length threshold");
    assert.equal(execute(vm, vm.start("default", [])).roots[0].data.Integer, expected);
    vm.close();
  }
});

test("direct outputs require private locals while ordinary declarations can be rebound", async () => {
  for (const [name, functionID, localID, opcode] of [
    ["direct_call_results", "fn.deferred", "local.value", 22],
    ["slot_locals", "fn.pointers", "local.x", 7],
  ]) {
    const image = JSON.parse(new TextDecoder().decode(runtimeVector(name).bytes));
    const artifact = Object.values(image.packages)[0].artifact;
    const fn = artifact.functions.find((candidate) => candidate.id === functionID);
    const local = fn.locals.findIndex((candidate) => candidate.id === localID);
    assert.ok(local >= 0);
    const producer = fn.code.instructions.find(([candidate]) => candidate === opcode);
    assert.ok(producer, `${name}: scalar producer`);
    fn.code.operands[producer[2]].outputs = [0x80000000 + local];
    await assert.rejects(
      VirtualMachine.create(sealImage(image), {}),
      /direct local output requires private local storage/,
      name,
    );
  }

  const vector = runtimeVector("direct_call_results");
  const vm = await VirtualMachine.create(vector.bytes, {});
  initialize(vm, "private local rebinding");
  const execution = vm.start("default", []);
  assert.equal(execute(vm, execution).roots[0].data.Integer, BigInt(vector.result_integer));
  vm.release(execution);
  vm.close();
});

test("direct call results preserve ordinary and revision-local dispatch across a patch", async () => {
  for (const revisionLocal of [false, true]) {
    const image = JSON.parse(new TextDecoder().decode(runtimeVector("direct_call_results").bytes));
    const artifact = Object.values(image.packages)[0].artifact;
    const add = artifact.functions.find((fn) => fn.id === "fn.add");
    add.revision_local = revisionLocal;
    const original = sealImage(image);
    const binary = add.code.instructions.find(([opcode]) => opcode === 7);
    const constant = add.code.operands[binary[2]].inputs.find(([kind]) => kind === 1)[1];
    artifact.constants[constant].value += 10;
    const replacement = sealImage(image);

    const probe = await VirtualMachine.create(replacement, {});
    initialize(probe, "direct call replacement");
    const expected = execute(probe, probe.start("default", [])).roots[0].data.Integer;
    probe.close();

    const vm = await VirtualMachine.create(original, {});
    initialize(vm, "direct call original");
    const execution = vm.start("default", []);
    let waiting;
    for (let count = 0; count < 100 && !waiting; count++) {
      vm.pump(1);
      waiting = [...vm.tasks.values()].some((task) => {
        const frame = task.frames.at(-1);
        return (
          frame?.fn.id === "fn.normal" && frame.fn.instructions[frame.pc]?.name === "call_direct"
        );
      });
    }
    assert.ok(waiting, "pause the old caller before its direct local result call");
    await vm.patch(replacement);
    assert.equal(
      execute(vm, execution).roots[0].data.Integer,
      expected - (revisionLocal ? 10n : 0n),
      `revision_local=${revisionLocal}`,
    );
    vm.release(execution);
    vm.close();
  }
});

test("hot patch keeps active frames on their revision and routes new calls to the new revision", async () => {
  const vector = runtimeVector("closure");
  const replacement = JSON.parse(new TextDecoder().decode(vector.bytes));
  const artifact = Object.values(replacement.packages)[0].artifact;
  const constant = artifact.constants.find((item) => typeof item.value === "number");
  assert.ok(constant);
  constant.value += 2;
  const replacementBytes = sealImage(replacement);

  const probe = await VirtualMachine.create(replacementBytes, {});
  initialize(probe, "replacement probe");
  const replacementResult = execute(probe, probe.start("default", [])).roots[0].data.Integer;
  probe.close();
  assert.notEqual(replacementResult, BigInt(vector.result_integer));

  const vm = await VirtualMachine.create(vector.bytes, { profile: true });
  initialize(vm, "original program");
  const oldExecution = vm.start("default", []);
  for (let count = 0; count < 10_000; count++) {
    vm.pump(1);
    if ((vm.stats().profile?.opcodes.make_closure ?? 0n) > 0n) break;
  }
  assert.ok((vm.stats().profile?.opcodes.make_closure ?? 0n) > 0n, "old closure was created");
  await vm.patch(replacementBytes);
  const newExecution = vm.start("default", []);
  assert.equal(execute(vm, oldExecution).roots[0].data.Integer, BigInt(vector.result_integer));
  assert.equal(execute(vm, newExecution).roots[0].data.Integer, replacementResult);
  vm.release(oldExecution);
  vm.release(newExecution);

  const rejected = JSON.parse(new TextDecoder().decode(replacementBytes));
  const rejectedArtifact = Object.values(rejected.packages)[0].artifact;
  rejectedArtifact.globals ??= [];
  rejectedArtifact.globals.push({
    id: "global.breaking",
    type: { kind: 3, primitive: 3 },
  });
  await assert.rejects(vm.patch(sealImage(rejected)), { code: "global_shape_changed" });
  assert.equal(vm.stats().generation, 2n);
  vm.close();
});

test("profile counts avoid clock reads unless timing is explicitly enabled", async () => {
  const vector = runtimeVector("arithmetic");
  const originalNow = performance.now;
  let clock = 0;
  performance.now = () => {
    return ++clock;
  };
  try {
    const counted = await VirtualMachine.create(vector.bytes, { profile: true });
    initialize(counted, "count-only profile");
    execute(counted, counted.start("default", []));
    const countedStats = counted.stats();
    assert.equal(
      Object.values(countedStats.profile?.opcodes ?? {}).reduce((sum, count) => sum + count, 0n),
      countedStats.steps,
      "opcode profiling must count every dispatched instruction exactly once",
    );
    assert.equal(
      Object.values(countedStats.profile?.functions ?? {}).reduce(
        (sum, profile) => sum + profile.steps,
        0n,
      ),
      countedStats.steps,
      "function profiling must count every dispatched instruction exactly once",
    );
    assert.ok(
      Object.values(countedStats.profile?.functions ?? {}).every(({ nanos }) => nanos === 0n),
      "count-only profiling must not sample per-step time",
    );
    counted.close();
    counted.pump(1);

    const timed = await VirtualMachine.create(vector.bytes, {
      profile: true,
      profileTiming: true,
    });
    initialize(timed, "timed profile");
    execute(timed, timed.start("default", []));
    assert.ok(
      Object.values(timed.stats().profile?.functions ?? {}).some(({ nanos }) => nanos > 0n),
      "timed profiling must sample the clock",
    );
    timed.close();
    timed.pump(1);
  } finally {
    performance.now = originalNow;
  }
});

test("step counter promotes after the safe Number range without losing exactness", async () => {
  const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {
    limits: { maxSteps: 0 },
  });
  initialize(vm, "step counter promotion");
  const before = vm.stats().steps;
  const first = vm.start("default", []);
  execute(vm, first);
  const executionSteps = vm.stats().steps - before;
  vm.release(first);
  vm.steps = Number.MAX_SAFE_INTEGER - 1;
  vm.stepsOverflow = undefined;
  execute(vm, vm.start("default", []));
  assert.equal(vm.stats().steps, BigInt(Number.MAX_SAFE_INTEGER - 1) + executionSteps);
  vm.close();
  vm.pump(1);
});

test("number and bigint step budgets stop at the exact instruction boundary", async () => {
  for (const limit of [3, 3n, BigInt(Number.MAX_SAFE_INTEGER) + 1n]) {
    const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {
      limits: { maxSteps: limit },
    });
    initialize(vm, "exact step budget");
    vm.steps =
      typeof limit === "bigint" && limit > BigInt(Number.MAX_SAFE_INTEGER)
        ? Number.MAX_SAFE_INTEGER - 1
        : 0;
    const execution = vm.start("default", []);
    assert.throws(() => execute(vm, execution), { code: "step_limit" });
    assert.equal(vm.stats().steps, BigInt(limit));
    vm.pump(1);
    assert.equal(vm.stats().steps, BigInt(limit));
    vm.close();
    vm.pump(1);
  }
});

test("frame cache charges entry and scratch capacity against its bound", async () => {
  const vm = await VirtualMachine.create(runtimeVector("recursive_call").bytes, {
    limits: { maxFrameCacheBytes: 80 },
  });
  initialize(vm, "bounded frame cache");
  const canceled = vm.start("default", []);
  vm.pump(1);
  vm.cancel(canceled);
  for (
    let count = 0;
    count < 100 && !vm.pump(64).executions.find((item) => item.id === canceled)?.settled;
    count++
  ) {}
  vm.release(canceled);
  execute(vm, vm.start("default", []));
  assert.ok(vm.frameCacheBytes <= 80, "cached storage must stay within its configured bound");
  assert.ok(vm.frameCache.length <= 1, "a zero-local frame still consumes cache budget");
  for (const storage of vm.frameCache) {
    assert.equal(storage.locals.length, 0, "cached locals must not retain values");
    assert.ok(
      storage.slots.every((value) => value === undefined),
      "cached slots must be clear",
    );
  }
  vm.close();
  vm.pump(1);
  assert.equal(vm.stats().liveGuestBytes, 0n, "closing a cached VM releases guest accounting");

  const active = await VirtualMachine.create(runtimeVector("recursive_call").bytes, {
    limits: { maxFrameCacheBytes: 80 },
  });
  initialize(active, "active frame cache close");
  active.start("default", []);
  active.pump(1);
  active.close();
  active.pump(1);
  assert.equal(active.frameCache.length, 0, "closing must prevent frame cache repopulation");
  assert.equal(active.frameCacheBytes, 0, "closing must release the frame cache budget");
});

test("debugger maps stripped label locations back to wire PCs", async () => {
  const vector = runtimeVector("arithmetic");
  const image = JSON.parse(new TextDecoder().decode(vector.bytes));
  const modulePath = Object.keys(image.packages)[0];
  const artifact = image.packages[modulePath].artifact;
  const fn = artifact.functions.find((candidate) =>
    candidate.code?.instructions?.some(([opcode]) => opcode === 14),
  );
  assert.ok(fn);
  const labelPC = fn.code.instructions.findIndex(([opcode]) => opcode === 14);
  assert.ok(labelPC >= 0);
  const line = 73;
  const symbols = {
    format: "mini-go-program-symbols",
    version: 1,
    program_hash: image.hash,
    packages: {
      [modulePath]: {
        files: [{ id: "source", path: "main.mgo" }],
        functions: [
          {
            id: fn.id,
            name: "Main",
            locations: [{ pc: labelPC, points: [{ file: "source", line, column: 1 }] }],
          },
        ],
      },
    },
  };
  const vm = await VirtualMachine.create(vector.bytes, {
    symbols: new TextEncoder().encode(JSON.stringify(symbols)),
  });
  initialize(vm, "debug symbols");
  vm.debugOpen({ "main.mgo": { module: modulePath, path: "main.mgo", text: "" } }, "default");
  const configured = vm.debugRequest("setBreakpoints", {
    source: { path: "main.mgo" },
    breakpoints: [{ line }],
  });
  assert.deepEqual(configured.breakpoints, [
    { line, verified: true, source: { path: "main.mgo" } },
  ]);
  vm.debugRequest("configurationDone", {});

  let stopped;
  for (let count = 0; count < 32 && !stopped; count++) {
    vm.pump(1);
    stopped = vm.debugEvents().find((event) => event.event === "stopped");
  }
  assert.ok(stopped, "the breakpoint on the stripped label should stop execution");
  const stack = vm.stack();
  assert.equal(stack.length, 1);
  assert.equal(
    stack[0].pc,
    BigInt(labelPC + 1),
    "stack PC must use the executable instruction wire PC",
  );
  assert.deepEqual(stack[0].locations, [{ file: "main.mgo", line: BigInt(line), column: 1n }]);

  const threadID = stopped.body.threadId;
  const trace = vm.debugRequest("stackTrace", { threadId: threadID, levels: 1 });
  assert.equal(trace.stackFrames[0].line, line);
  assert.equal(trace.stackFrames[0].source.path, "main.mgo");
  vm.close();
  vm.pump(1);
});

test("guest memory publishes deterministic lifecycle counters", () => {
  const memory = new GuestMemory({ maxAllocatedBytes: 550n, maxHeapBytes: 300n });
  assert.equal(
    memory.publish({ liveHeapBytes: 128n, liveHeapObjects: 1n, liveGuestBytes: 256n }, true),
    0n,
  );
  assert.deepEqual(memory.stats(), {
    liveHeapBytes: 128n,
    liveHeapObjects: 1n,
    peakHeapBytes: 128n,
    totalHeapAllocatedBytes: 128n,
    liveGuestBytes: 256n,
    peakGuestBytes: 256n,
    totalGuestAllocatedBytes: 256n,
    guestAllocatedSinceCensusBytes: 256n,
    guestAllocations: 1n,
  });
  memory.reserve(32n);
  assert.throws(() => memory.reserve(300n), { code: "allocation_limit" });
  assert.equal(
    memory.publish({ liveHeapBytes: 64n, liveHeapObjects: 1n, liveGuestBytes: 200n }, true),
    64n,
  );
  assert.equal(memory.stats().totalGuestAllocatedBytes, 288n);
  memory.release();
  assert.equal(memory.stats().liveGuestBytes, 0n);
  assert.equal(memory.stats().peakGuestBytes, 256n);
});
