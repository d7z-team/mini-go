import assert from "node:assert/strict";
import { test } from "vitest";
import { prepareFrameReferences } from "../src/vm/reference-plan";

function instruction(name, metadata = {}, inputs = [], outputs = []) {
  return {
    name,
    operands: { inputs, outputs, release: [], releaseBefore: [] },
    ...metadata,
  };
}

function prepare(instructions, localReferences, temporaryReferences, metadata = {}) {
  return prepareFrameReferences(
    {
      id: "roots",
      localTypes: Array(localReferences.length),
      slotTypes: Array(temporaryReferences.length),
      upvalueTypes: [],
      cellLocals: new Set(),
      resultLocals: [],
      instructions,
      ...metadata,
    },
    localReferences,
    temporaryReferences,
  );
}

function roots(plan, pc) {
  const result = [];
  for (const [word, bitmap] of plan.maps[plan.atPC[pc]].entries())
    for (let bit = 0; bit < 32; bit++) if (bitmap & (1 << bit)) result.push(word * 32 + bit);
  return result;
}

test("reference maps transfer hidden local reads and temporary lifetimes", () => {
  const plan = prepare(
    [
      instruction("load_local", { local: 0 }, [], [0]),
      instruction("store_local", { local: 1 }, [[0, 0]]),
      instruction("load_local", { local: 1 }, [], [1]),
      instruction("return", {}, [[0, 1]]),
    ],
    [true, true, false],
    [true, true],
  );
  assert.deepEqual(
    [0, 1, 2, 3, 4].map((pc) => roots(plan, pc)),
    [[0], [3], [1], [4], []],
  );
});

test("type dispatch definitions apply only to the selected successor", () => {
  for (const defaultLocal of [1, 2]) {
    const plan = prepare(
      [
        instruction("type_dispatch", {
          typeDispatch: {
            subject: 0,
            defaultTarget: 3,
            cases: [{ target: 1, binding: 1, original: false }],
          },
        }),
        instruction("load_local", { local: 1 }, [], [0]),
        instruction("return", {}, [[0, 0]]),
        instruction("load_local", { local: defaultLocal }, [], [0]),
        instruction("return", {}, [[0, 0]]),
      ],
      [true, true, true],
      [true],
    );
    assert.deepEqual(roots(plan, 0), [0, defaultLocal]);
    assert.deepEqual(roots(plan, 1), [1]);
  }
});

test("select retains an unchosen receive value and reads hidden send/channel locals", () => {
  const plan = prepare(
    [
      instruction("select", {
        selectIndex: 5,
        selectCases: [
          { channel: 0, value: 3, ok: 4 },
          { channel: 1, send: 2 },
        ],
      }),
      instruction("load_local", { local: 3 }, [], [0]),
      instruction("return", {}, [[0, 0]]),
    ],
    [true, true, true, true, false, false],
    [true],
  );
  assert.deepEqual(roots(plan, 0), [0, 1, 2, 3]);
  assert.deepEqual(roots(plan, 1), [3]);
  assert.deepEqual(roots(plan, 2), [6]);
});

test("rebind replaces a cell while ordinary stores retain its identity", () => {
  const plan = prepare(
    [
      instruction("store_local", { local: 0, rebind: true }, [[1, 0]]),
      instruction(
        "make_closure",
        { captures: [{ kind: "local", index: 0, path: [], targetType: {} }] },
        [],
        [0],
      ),
      instruction("store_local", { local: 0 }, [[1, 1]]),
      instruction("return", {}, [[0, 0]]),
    ],
    [false],
    [true],
    { cellLocals: new Set([0]) },
  );
  assert.deepEqual(
    [0, 1, 2, 3].map((pc) => roots(plan, pc)),
    [[], [0], [0, 1], [1]],
  );
});

test("loop roots and named results survive the implicit recovering-defer exit", () => {
  const plan = prepare(
    [
      instruction("load_local", { local: 1 }, [], [1]),
      instruction("store_upvalue", { upvalue: 0 }, [[1, 0]]),
      instruction("jump_if", { target: 0 }, [[0, 0]]),
      instruction("panic", {}, [[0, 1]]),
    ],
    [true, true],
    [false, true],
    { resultLocals: [0], upvalueTypes: Array(1) },
  );
  assert.deepEqual(
    [0, 1, 2, 3, 4].map((pc) => roots(plan, pc)),
    [[0, 1, 4], [0, 1, 3, 4], [0, 1, 3, 4], [0, 3], [0]],
  );
});

test("release is valid only after the last reachable reference use", () => {
  for (const [pc, before, valid] of [
    [0, true, true],
    [1, true, false],
    [0, false, false],
    [1, false, true],
  ]) {
    const instructions = [instruction("zero", {}, [], [0]), instruction("return", {}, [[0, 0]])];
    instructions[pc].operands[before ? "releaseBefore" : "release"] = [0];
    const run = () => prepare(instructions, [], [true]);
    if (valid) assert.doesNotThrow(run);
    else assert.throws(run, /release discards live reference/);
  }
});

test("reference bitmaps cover the sign bit and multiple words", () => {
  const localReferences = Array(66).fill(false);
  for (const index of [0, 31, 32, 63, 65]) localReferences[index] = true;
  const plan = prepare(
    [
      instruction(
        "call_intrinsic",
        {},
        [0, 31, 32, 63, 65].map((index) => [2, index]),
      ),
      instruction("return"),
    ],
    localReferences,
    [],
  );
  assert.deepEqual(roots(plan, 0), [0, 31, 32, 63, 65]);
  assert.deepEqual(roots(plan, 1), []);
});
