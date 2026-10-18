import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "vitest";
import { VirtualMachine } from "../src/vm/vm";
import { allRuntimeVectors, runtimeVector, sealImage } from "./support/runtime-vectors.js";
import { executeVM, initializeVM } from "./support/vm.js";

const opcodeOf = (instruction) => instruction[0] & 0xffff;

test.each(allRuntimeVectors)("direct VM executes $name O$optimization", async (vector) => {
  const vm = await VirtualMachine.create(vector.bytes, {});
  initializeVM(vm, `${vector.name} O${vector.optimization}`);
  const execution = vm.start("default", []);
  let state;
  for (let count = 0; count < 100_000; count++) {
    state = vm.pump(4096);
    if (state.executions.find((item) => item.id === execution)?.settled) break;
    assert.equal(vm.actions().length, 0, `${vector.name} O${vector.optimization}: host action`);
    if (!state.running && state.delay !== undefined && state.delay > 0)
      await new Promise((resolve) => setTimeout(resolve, Math.min(state.delay, 10)));
  }
  let result;
  try {
    result = vm.result(execution);
  } catch (error) {
    throw new Error(`${vector.name} O${vector.optimization}: execution failed`, { cause: error });
  }
  assert.equal(
    result.roots[0].data.Integer,
    BigInt(vector.result_integer),
    `${vector.name} O${vector.optimization}`,
  );
  vm.release(execution);
  vm.close();
  vm.pump(1);
  assert.equal(vm.stats().occupiedBytes, 0n, `${vector.name} O${vector.optimization}: release`);
});

test("current guest compiler image passes verification and can close during initialization", async () => {
  const image = await readFile(
    new URL("../../runtime-rust/assets/compiler.json.gz", import.meta.url),
  );
  const vm = await VirtualMachine.create(image, {});
  vm.close();
  for (let count = 0; count < 100 && vm.stats().state !== "Closed"; count++) vm.pump(4096);
  assert.equal(vm.stats().state, "Closed");
  assert.equal(vm.stats().occupiedBytes, 0n);
});

test.each([1, 511, 512, 513])("pump(%i) preserves the exact bytecode budget", async (budget) => {
  const vm = await VirtualMachine.create(runtimeVector("parallel_cpu", 2).bytes, {});
  initializeVM(vm, `budget ${budget}`);
  const execution = vm.start("default", []);
  const before = vm.stats().steps;
  const state = vm.pump(budget);
  assert.equal(vm.stats().steps - before, BigInt(budget));
  assert.equal(state.executions.find((item) => item.id === execution)?.settled, false);
  vm.cancel(execution);
  vm.release(execution);
  vm.close();
  vm.pump(1);
  assert.equal(vm.stats().occupiedBytes, 0n);
});

test("fused updates charge every logical step before committing", async () => {
  const image = JSON.parse(new TextDecoder().decode(runtimeVector("arithmetic").bytes));
  const artifact = Object.values(image.packages)[0].artifact;
  const integer = { kind: 3, primitive: 3, named: { module_path: "", decl_id: "" } };
  artifact.constants = [
    { id: "one", type: integer, value: 1 },
    { id: "end", type: integer, value: 3 },
  ];
  const fn = artifact.functions.find((candidate) => candidate.id === "fn.Main");
  fn.locals = [{ id: "counter", type: integer }];
  fn.code = {
    instructions: [
      [14, 0, 0, 1],
      [73 | (3 << 16), 0, 1, 2],
      [17 | (3 << 16), 0, 2, 1],
    ],
    blocks: [
      { first: 0, count: 1, fallthrough: 1, cost: 2 },
      { first: 1, count: 1, fallthrough: 0xffff_ffff, cost: 1 },
    ],
    descriptors: {
      label: [{ label: "loop" }],
      updatecomparebranch: [
        { update: "+", comparison: "<", type: integer, label: "loop", when: true },
      ],
      return: [{ result_count: 1 }],
    },
    operands: [
      {},
      {
        inputs: [
          [2, 0],
          [1, 0],
          [1, 1],
        ],
      },
      { inputs: [[2, 0]] },
    ],
  };

  const vm = await VirtualMachine.create(sealImage(image), {});
  initializeVM(vm, "fused update accounting");
  const execution = vm.start("default", []);
  const before = vm.stats().steps;
  for (let step = 1; step <= 7; step++) {
    const state = vm.pump(1);
    assert.equal(vm.stats().steps - before, BigInt(step));
    assert.equal(state.executions.find((candidate) => candidate.id === execution)?.settled, false);
  }
  const settled = vm.pump(1);
  assert.equal(vm.stats().steps - before, 7n);
  assert.equal(settled.executions.find((candidate) => candidate.id === execution)?.settled, true);
  assert.equal(vm.result(execution).roots[0].data.Integer, 3n);
  vm.release(execution);
  vm.close();
  vm.pump(1);
  assert.equal(vm.stats().occupiedBytes, 0n);

  const limited = await VirtualMachine.create(sealImage(image), { limits: { maxSteps: 1 } });
  initializeVM(limited, "fused update limit");
  assert.throws(() => executeVM(limited, limited.start("default", [])), { code: "step_limit" });
  assert.equal(limited.stats().steps, 1n);
  limited.close();
  limited.pump(1);
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

test("typed min/max validates direction, arity and homogeneous ordered operands", async () => {
  const source = JSON.parse(new TextDecoder().decode(runtimeVector("arithmetic").bytes));
  for (const [name, left, right, output, operator, count, valid] of [
    ["integer", 3, 3, 3, "min", 2, true],
    ["direction", 3, 3, 3, "sum", 2, false],
    ["empty", 3, 3, 3, "min", 0, false],
    ["arity", 3, 3, 3, "min", 3, false],
    ["boolean", 1, 1, 1, "min", 2, false],
    ["complex", 16, 16, 16, "min", 2, false],
    ["input type", 3, 15, 3, "min", 2, false],
    ["output type", 3, 3, 15, "min", 2, false],
  ]) {
    const image = structuredClone(source);
    const artifact = Object.values(image.packages)[0].artifact;
    const fn = artifact.functions.find((item) => item.id === "fn.Main");
    const type = (primitive) => ({ kind: 3, primitive, named: { module_path: "", decl_id: "" } });
    fn.code = {
      types: [type(left), type(right), type(output)],
      instructions: [
        [4, 0, 0],
        [4, 1, 1],
        [72, 0, 2],
        [17, 0, 3],
      ],
      descriptors: {
        type: [{ type: type(left) }, { type: type(right) }],
        minmax: [{ type: type(left), operator, arg_count: count }],
        return: [{ result_count: 1 }],
      },
      operands: [
        { outputs: [0] },
        { outputs: [1] },
        {
          inputs: [
            [0, 0],
            [0, 1],
          ],
          outputs: [2],
        },
        { inputs: [[0, 2]] },
      ],
    };
    if (valid) {
      const vm = await VirtualMachine.create(sealImage(image), {});
      initializeVM(vm, name);
      assert.equal(executeVM(vm, vm.start("default", [])).roots[0].data.Integer, 0n);
      vm.close();
      vm.pump(1);
    } else {
      await assert.rejects(
        VirtualMachine.create(sealImage(image), {}),
        { code: "invalid_operand" },
        name,
      );
    }
  }
});

test("program loading rejects statically invalid scalar operators", async () => {
  const image = JSON.parse(new TextDecoder().decode(runtimeVector("arithmetic").bytes));
  const artifact = Object.values(image.packages)[0].artifact;
  const fn = artifact.functions.find((item) => item.id === "fn.Main");
  const bool = { kind: 3, primitive: 1, named: { module_path: "", decl_id: "" } };
  fn.signature = { results: [bool] };
  fn.locals = [
    { id: "local.left", type: bool },
    { id: "local.right", type: bool },
  ];
  fn.code = {
    instructions: [
      [4, 0, 0],
      [4, 0, 1],
      [7, 0, 2],
      [17, 0, 3],
    ],
    descriptors: {
      type: [{ type: bool }],
      operator: [{ operator: "+" }],
      return: [{ result_count: 1 }],
    },
    operands: [
      { outputs: [0x80000000] },
      { outputs: [0x80000001] },
      {
        inputs: [
          [2, 0],
          [2, 1],
        ],
        outputs: [0x80000000],
      },
      { inputs: [[2, 0]] },
    ],
  };
  await assert.rejects(
    VirtualMachine.create(sealImage(image), {}),
    /operand types do not support operator/,
  );
});

test("label instructions retain their zero-operand contract after preparation", async () => {
  const image = JSON.parse(new TextDecoder().decode(runtimeVector("arithmetic").bytes));
  const artifact = Object.values(image.packages)[0].artifact;
  const functionWithLabel = artifact.functions.find((fn) =>
    fn.code?.instructions?.some((instruction) => opcodeOf(instruction) === 14),
  );
  assert.ok(functionWithLabel);
  const instruction = functionWithLabel.code.instructions.find(
    (candidate) => opcodeOf(candidate) === 14,
  );
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
  initializeVM(validIndexVM, "index_convert");
  const validIndexExecution = validIndexVM.start("default", []);
  assert.equal(executeVM(validIndexVM, validIndexExecution).roots[0].data.Integer, 109n);
  validIndexVM.close();
  validIndexVM.pump(1);

  const validLengthVM = await VirtualMachine.create(runtimeVector("len_compare_branch").bytes, {});
  initializeVM(validLengthVM, "len_compare_branch");
  const validLengthExecution = validLengthVM.start("default", []);
  assert.equal(executeVM(validLengthVM, validLengthExecution).roots[0].data.Integer, 7n);
  validLengthVM.close();
  validLengthVM.pump(1);

  const indexImage = JSON.parse(new TextDecoder().decode(runtimeVector("index_convert").bytes));
  const indexArtifact = Object.values(indexImage.packages)[0].artifact;
  const indexFunction = indexArtifact.functions.find((fn) => fn.id === "fn.Main");
  const indexInstruction = indexFunction.code.instructions.find(
    (instruction) => opcodeOf(instruction) === 70,
  );
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
  const lengthInstruction = lengthFunction.code.instructions.find(
    (instruction) => opcodeOf(instruction) === 71,
  );
  assert.ok(lengthInstruction);
  const lengthOperands = lengthFunction.code.operands[lengthInstruction[2]];
  lengthOperands.inputs[0] = [1, 4];
  await assert.rejects(
    (async () => {
      const vm = await VirtualMachine.create(sealImage(lengthImage), {});
      initializeVM(vm, "invalid len_compare_branch object");
      executeVM(vm, vm.start("default", []));
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
    initializeVM(vm, "integer length threshold");
    assert.equal(executeVM(vm, vm.start("default", [])).roots[0].data.Integer, expected);
    vm.close();
  }
});

test("map iteration publishes direct key/value locals with an ordinary ok slot", async () => {
  const vector = runtimeVector("map_range");
  const image = JSON.parse(new TextDecoder().decode(vector.bytes));
  const artifact = Object.values(image.packages)[0].artifact;
  const main = artifact.functions.find((candidate) => candidate.id === "fn.Main");
  const next = main.code.instructions.find((instruction) => opcodeOf(instruction) === 44);
  assert.ok(next, "map_iter_next instruction");
  const operands = main.code.operands[next[2]];
  assert.equal(operands.outputs.length, 3);
  assert.ok(operands.outputs[0] >= 0x80000000);
  assert.ok(operands.outputs[1] >= 0x80000000);
  assert.ok(operands.outputs[2] < 0x80000000);

  const vm = await VirtualMachine.create(vector.bytes, {});
  initializeVM(vm, "mixed map iterator outputs");
  assert.equal(executeVM(vm, vm.start("default", [])).roots[0].data.Integer, 35n);
  vm.close();
});

test("reused local storage clears a slice before len observes it", async () => {
  const image = JSON.parse(new TextDecoder().decode(runtimeVector("slice_copy").bytes));
  const artifact = Object.values(image.packages)[0].artifact;
  const main = artifact.functions.find((candidate) => candidate.id === "fn.Main");
  const slice = main.locals.find((local) => local.type.kind === 5).type;
  const integer = { kind: 3, primitive: 3, named: { module_path: "", decl_id: "" } };
  main.signature = { results: [integer] };
  main.locals = [{ id: "local.slice", type: slice }];
  artifact.constants = [{ id: "one", type: integer, value: 1 }];
  main.code = {
    types: [integer, slice, slice, integer],
    descriptors: {
      const: [{ constant: "one" }],
      makeslice: [{ has_capacity: true, type: slice }],
      type: [{ type: slice }],
      local: [{ local: "local.slice" }],
      return: [{ result_count: 1 }],
    },
    instructions: [
      [3, 0, 0],
      [29, 0, 1],
      [9, 0, 2],
      [4, 0, 3],
      [9, 0, 4],
      [36, 0, 5],
      [17, 0, 6],
    ],
    operands: [
      { outputs: [0] },
      {
        inputs: [
          [0, 0],
          [0, 0],
        ],
        outputs: [1],
      },
      { inputs: [[0, 1]] },
      { outputs: [2] },
      { inputs: [[0, 2]] },
      { inputs: [[2, 0]], outputs: [3] },
      { inputs: [[0, 3]] },
    ],
  };
  const vm = await VirtualMachine.create(sealImage(image), {});
  initializeVM(vm, "reused nil slice");
  const result = executeVM(vm, vm.start("default", [])).roots[0].data.Integer;
  assert.equal(result, 0n);
  vm.close();
  vm.pump(1);
});

test("direct outputs require private locals while ordinary declarations can be rebound", async () => {
  for (const [name, optimization, functionID, localID, opcode] of [
    ["direct_call_results", 0, "fn.deferred", "local.value", 22],
    ["direct_call_results", 0, "fn.deferred", "local.result", 22],
    ["slot_locals", 2, "fn.pointers", "local.x", 7],
  ]) {
    const image = JSON.parse(new TextDecoder().decode(runtimeVector(name, optimization).bytes));
    const artifact = Object.values(image.packages)[0].artifact;
    const fn = artifact.functions.find((candidate) => candidate.id === functionID);
    const local = fn.locals.findIndex((candidate) => candidate.id === localID);
    assert.ok(local >= 0);
    const producer = fn.code.instructions.find((instruction) => opcodeOf(instruction) === opcode);
    assert.ok(producer, `${name}: scalar producer`);
    fn.code.operands[producer[2]].outputs = [0x80000000 + local];
    await assert.rejects(
      VirtualMachine.create(sealImage(image), {}),
      /direct local output requires private local storage/,
      name,
    );
  }

  const inputImage = JSON.parse(
    new TextDecoder().decode(runtimeVector("direct_call_results", 0).bytes),
  );
  const inputArtifact = Object.values(inputImage.packages)[0].artifact;
  const inputFunction = inputArtifact.functions.find((candidate) => candidate.id === "fn.deferred");
  const resultIndex = inputFunction.locals.findIndex(
    (candidate) => candidate.id === "local.result",
  );
  assert.ok(resultIndex >= 0);
  inputFunction.code.operands[1].inputs[0] = [2, resultIndex];
  await assert.rejects(
    VirtualMachine.create(sealImage(inputImage), {}),
    /direct local input requires private local storage/,
    "named result locals remain cells for direct inputs",
  );

  const vector = runtimeVector("direct_call_results");
  const vm = await VirtualMachine.create(vector.bytes, {});
  initializeVM(vm, "private local rebinding");
  const execution = vm.start("default", []);
  assert.equal(executeVM(vm, execution).roots[0].data.Integer, BigInt(vector.result_integer));
  vm.release(execution);
  vm.close();
});

test("direct calls in a running call tree use its captured revision", async () => {
  const vector = runtimeVector("direct_call_results", 0);
  const image = JSON.parse(new TextDecoder().decode(vector.bytes));
  const artifact = Object.values(image.packages)[0].artifact;
  const add = artifact.functions.find((fn) => fn.id === "fn.add");
  const original = sealImage(image);
  const binary = add.code.instructions.find((instruction) => opcodeOf(instruction) === 7);
  const constant = add.code.operands[binary[2]].inputs.find(([kind]) => kind === 1)[1];
  artifact.constants[constant].value += 10;
  const replacement = sealImage(image);

  const probe = await VirtualMachine.create(replacement, {});
  initializeVM(probe, "direct call replacement");
  const expected = executeVM(probe, probe.start("default", [])).roots[0].data.Integer;
  probe.close();

  const vm = await VirtualMachine.create(original, {});
  initializeVM(vm, "direct call original");
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
  assert.equal(executeVM(vm, execution).roots[0].data.Integer, BigInt(vector.result_integer));
  const next = vm.start("default", []);
  assert.equal(executeVM(vm, next).roots[0].data.Integer, expected);
  vm.release(execution);
  vm.release(next);
  vm.close();
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
  initializeVM(probe, "replacement probe");
  const replacementResult = executeVM(probe, probe.start("default", [])).roots[0].data.Integer;
  probe.close();
  assert.notEqual(replacementResult, BigInt(vector.result_integer));

  const vm = await VirtualMachine.create(vector.bytes, { profile: true });
  initializeVM(vm, "original program");
  const oldExecution = vm.start("default", []);
  for (let count = 0; count < 10_000; count++) {
    vm.pump(1);
    if ((vm.stats().profile?.opcodes.make_closure ?? 0n) > 0n) break;
  }
  assert.ok((vm.stats().profile?.opcodes.make_closure ?? 0n) > 0n, "old closure was created");
  await vm.patch(replacementBytes);
  const newExecution = vm.start("default", []);
  assert.equal(executeVM(vm, oldExecution).roots[0].data.Integer, BigInt(vector.result_integer));
  assert.equal(executeVM(vm, newExecution).roots[0].data.Integer, replacementResult);
  vm.release(oldExecution);
  vm.release(newExecution);

  const extended = JSON.parse(new TextDecoder().decode(replacementBytes));
  const extendedArtifact = Object.values(extended.packages)[0].artifact;
  extendedArtifact.globals ??= [];
  extendedArtifact.globals.push({
    id: "global.breaking",
    type: { kind: 3, primitive: 3 },
  });
  await vm.patch(sealImage(extended));
  assert.equal(vm.stats().generation, 3n);
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
    initializeVM(counted, "count-only profile");
    executeVM(counted, counted.start("default", []));
    const countedStats = counted.stats();
    assert.equal(
      Object.values(countedStats.profile?.opcodes ?? {}).reduce((sum, count) => sum + count, 0n),
      countedStats.profile?.dispatches,
      "opcode profiling must count every dispatched instruction exactly once",
    );
    assert.equal(
      Object.values(countedStats.profile?.functions ?? {}).reduce(
        (sum, profile) => sum + profile.steps,
        0n,
      ),
      countedStats.steps,
      "function profiling must preserve total logical fuel",
    );
    assert.ok(
      Object.values(countedStats.profile?.functions ?? {}).every(({ nanos }) => nanos === 0n),
      "count-only profiling must not sample per-step time",
    );
    const snapshot = structuredClone(countedStats.profile);
    executeVM(counted, counted.start("default", []));
    assert.deepEqual(countedStats.profile, snapshot, "published profile is an owned snapshot");
    const [name, functionStats] = Object.entries(countedStats.profile.functions)[0];
    functionStats.steps = -1n;
    assert.ok(counted.stats().profile.functions[name].steps > 0n);
    counted.close();
    counted.pump(1);

    const timed = await VirtualMachine.create(vector.bytes, {
      profile: true,
      profileTiming: true,
    });
    initializeVM(timed, "timed profile");
    executeVM(timed, timed.start("default", []));
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
  initializeVM(vm, "step counter promotion");
  const before = vm.stats().steps;
  const first = vm.start("default", []);
  executeVM(vm, first);
  const executionSteps = vm.stats().steps - before;
  vm.release(first);
  vm.steps = Number.MAX_SAFE_INTEGER - 1;
  vm.stepsOverflow = undefined;
  executeVM(vm, vm.start("default", []));
  assert.equal(vm.stats().steps, BigInt(Number.MAX_SAFE_INTEGER - 1) + executionSteps);
  vm.close();
  vm.pump(1);
});

test("number and bigint step budgets stop at the exact instruction boundary", async () => {
  for (const limit of [3, 3n, BigInt(Number.MAX_SAFE_INTEGER) + 1n]) {
    const vm = await VirtualMachine.create(runtimeVector("arithmetic").bytes, {
      limits: { maxSteps: limit },
    });
    initializeVM(vm, "exact step budget");
    vm.steps =
      typeof limit === "bigint" && limit > BigInt(Number.MAX_SAFE_INTEGER)
        ? Number.MAX_SAFE_INTEGER - 1
        : 0;
    const execution = vm.start("default", []);
    assert.throws(() => executeVM(vm, execution), { code: "step_limit" });
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
  initializeVM(vm, "bounded frame cache");
  const canceled = vm.start("default", []);
  vm.pump(1);
  vm.cancel(canceled);
  for (
    let count = 0;
    count < 100 && !vm.pump(64).executions.find((item) => item.id === canceled)?.settled;
    count++
  ) {}
  vm.release(canceled);
  executeVM(vm, vm.start("default", []));
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
  assert.equal(vm.stats().occupiedBytes, 0n, "closing a cached VM releases guest accounting");

  const active = await VirtualMachine.create(runtimeVector("recursive_call").bytes, {
    limits: { maxFrameCacheBytes: 80 },
  });
  initializeVM(active, "active frame cache close");
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
    candidate.code?.instructions?.some((instruction) => opcodeOf(instruction) === 14),
  );
  assert.ok(fn);
  const labelPC = fn.code.instructions.findIndex((instruction) => opcodeOf(instruction) === 14);
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
  initializeVM(vm, "debug symbols");
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
