import { VMError } from "./json";
import type { PreparedFunction } from "./program";

export interface FrameReferences {
  /** Locals, temporaries and upvalue cells, in that order. */
  readonly maps: readonly Uint32Array[];
  /** Executable PC, including the completed-frame boundary. */
  readonly atPC: Uint32Array;
}

type ReferenceFunction = Pick<
  PreparedFunction,
  | "id"
  | "localTypes"
  | "slotTypes"
  | "upvalueTypes"
  | "cellLocals"
  | "resultLocals"
  | "instructions"
>;

/** Derive entry roots after descriptor validation and executable-PC preparation.
 * Runtime-owned operation and continuation values have their own root regions.
 */
export function prepareFrameReferences(
  fn: ReferenceFunction,
  localReferences: readonly boolean[],
  temporaryReferences: readonly boolean[],
): FrameReferences {
  const count = fn.instructions.length;
  const locals = fn.localTypes.length;
  const temporaries = fn.slotTypes.length;
  const storageCount = locals + temporaries + fn.upvalueTypes.length;
  const words = Math.ceil(storageCount / 32);
  const invalid = (message: string): never => {
    throw new VMError(message, "invalid_operand", `${fn.id}.references`);
  };
  if (localReferences.length !== locals || temporaryReferences.length !== temporaries)
    invalid("reference storage does not match function");
  const references = new Uint8Array(storageCount);
  for (let index = 0; index < locals; index++)
    references[index] = localReferences[index] || fn.cellLocals.has(index) ? 1 : 0;
  for (let index = 0; index < temporaries; index++)
    references[locals + index] = temporaryReferences[index] ? 1 : 0;
  references.fill(1, locals + temporaries);

  const uses = new Uint32Array(count * words);
  const defs = new Uint32Array(count * words);
  const incoming = new Uint32Array((count + 1) * words);
  const successors: { pc: number; kill: number }[][] = Array.from({ length: count }, () => []);
  const results = new Uint32Array(words);
  const set = (bitmap: Uint32Array, offset: number, index: number | undefined): void => {
    if (index === undefined) return;
    if (!Number.isInteger(index) || index < 0 || index >= storageCount)
      invalid("reference index is outside frame storage");
    if (references[index]) {
      const word = offset + (index >>> 5);
      bitmap[word] = bitmap[word]! | (1 << (index & 31));
    }
  };
  for (const index of fn.resultLocals) set(results, 0, index);
  incoming.set(results, count * words);

  for (let pc = 0; pc < count; pc++) {
    const instruction = fn.instructions[pc]!;
    const descriptor = instruction.descriptor ?? {};
    const offset = pc * words;
    // A runtime panic may unwind into a recovering defer at any instruction.
    uses.set(results, offset);
    for (const [kind, index] of instruction.operands.inputs) {
      if (kind === 0) set(uses, offset, locals + index);
      else if (kind === 2) set(uses, offset, index);
    }
    for (const output of instruction.operands.outputs) {
      const local = output >= 0x8000_0000;
      const index = local ? output - 0x8000_0000 : locals + output;
      set(defs, offset, index);
      if (local && fn.cellLocals.has(index)) set(uses, offset, index);
    }
    const readAddress = (address: Record<string, unknown>): void => {
      const index = address.$index as number;
      if (address.kind === "local") set(uses, offset, index);
      else if (address.kind === "upvalue") set(uses, offset, locals + temporaries + index);
      for (const segment of (address.path ?? []) as Record<string, unknown>[])
        if (segment.kind === "index") set(uses, offset, segment.$index as number);
    };
    const branch = (target: number, binding?: number): void => {
      if (!Number.isInteger(target) || target < 0 || target > count)
        invalid("reference branch target is outside function");
      let kill = binding ?? -1;
      if (kill >= 0 && fn.cellLocals.has(kill)) {
        set(uses, offset, kill);
        kill = -1;
      }
      successors[pc]!.push({ pc: target, kill });
    };
    let fallsThrough = true;
    switch (instruction.name) {
      case "load_local":
        set(uses, offset, instruction.local);
        break;
      case "store_local": {
        const index = instruction.local!;
        set(defs, offset, index);
        // A rebind replaces the stable cell itself in VM.execute. Ordinary
        // stores retain that cell so its existing managed payload remains a
        // root through the post-store control flow.
        if (fn.cellLocals.has(index) && descriptor.rebind !== true) set(uses, offset, index);
        break;
      }
      case "load_upvalue":
      case "store_upvalue":
        set(uses, offset, locals + temporaries + instruction.upvalue!);
        break;
      case "address_of":
        readAddress(descriptor);
        break;
      case "make_closure":
        for (const capture of instruction.captures ?? []) readAddress(capture);
        break;
      case "type_dispatch":
        set(uses, offset, descriptor.$subject as number);
        branch(descriptor.$target as number, descriptor.$default_local as number | undefined);
        for (const match of (descriptor.cases ?? []) as Record<string, unknown>[])
          branch(match.$target as number, match.$binding as number | undefined);
        fallsThrough = false;
        break;
      case "select":
        set(defs, offset, instruction.selectIndex);
        if (fn.cellLocals.has(instruction.selectIndex!)) set(uses, offset, instruction.selectIndex);
        for (const selected of instruction.selectCases ?? []) {
          set(uses, offset, selected.channel);
          if (selected.send !== undefined) set(uses, offset, selected.send);
          else {
            // Unchosen receive destinations retain their previous values.
            if (fn.cellLocals.has(selected.value!)) set(uses, offset, selected.value);
            if (fn.cellLocals.has(selected.ok!)) set(uses, offset, selected.ok);
          }
        }
        break;
      case "jump":
      case "jump_if":
        branch(instruction.target!);
        fallsThrough = instruction.name !== "jump";
        break;
      case "compare_branch":
      case "len_compare_branch":
        branch(instruction.target!);
        break;
      case "return":
      case "panic":
      case "tail_call_direct":
        fallsThrough = false;
        break;
    }
    if (fallsThrough) branch(pc + 1);
  }

  for (let changed = true; changed; ) {
    changed = false;
    for (let pc = count - 1; pc >= 0; pc--) {
      for (let word = 0; word < words; word++) {
        let outgoing = 0;
        for (const edge of successors[pc]!) {
          let bits = incoming[edge.pc * words + word]!;
          if (edge.kill >= 0 && edge.kill >>> 5 === word) bits &= ~(1 << (edge.kill & 31));
          outgoing |= bits;
        }
        const index = pc * words + word;
        const next = (uses[index]! | (outgoing & ~defs[index]!)) >>> 0;
        if (incoming[index] !== next) {
          incoming[index] = next;
          changed = true;
        }
      }
    }
  }

  for (let pc = 0; pc < count; pc++) {
    const operands = fn.instructions[pc]!.operands;
    for (const slot of operands.releaseBefore) {
      const index = locals + slot;
      if (incoming[pc * words + (index >>> 5)]! & (1 << (index & 31)))
        invalid(`entry release discards live reference slot ${slot} at ${pc}`);
    }
    for (const slot of operands.release) {
      const index = locals + slot;
      for (const edge of successors[pc]!)
        if (incoming[edge.pc * words + (index >>> 5)]! & (1 << (index & 31)))
          invalid(`release discards live reference slot ${slot} at ${pc}`);
    }
  }

  const maps: Uint32Array[] = [];
  const atPC = new Uint32Array(count + 1);
  const interned = new Map<number, number[]>();
  for (let pc = 0; pc <= count; pc++) {
    const bitmap = incoming.subarray(pc * words, (pc + 1) * words);
    let hash = 0x811c9dc5;
    for (const word of bitmap) hash = Math.imul(hash ^ word, 0x01000193);
    const candidates = interned.get(hash) ?? [];
    const found = candidates.find((index) => {
      const candidate = maps[index]!;
      for (let word = 0; word < words; word++) if (candidate[word] !== bitmap[word]) return false;
      return true;
    });
    if (found !== undefined) atPC[pc] = found;
    else {
      atPC[pc] = maps.length;
      candidates.push(maps.length);
      maps.push(bitmap.slice());
      interned.set(hash, candidates);
    }
  }
  return { maps, atPC };
}
