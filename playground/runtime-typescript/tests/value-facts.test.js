import assert from "node:assert/strict";
import { test } from "vitest";
import { Kind, PrimitiveKind } from "../src/vm/contract.generated";
import { TypeRegistry, primitive } from "../src/vm/value";

test("type registry interns primitive runtime types by direct index", () => {
  const registry = new TypeRegistry("facts-test", undefined);
  const first = primitive(registry, PrimitiveKind.PrimitiveInt);
  const second = primitive(registry, PrimitiveKind.PrimitiveInt);
  assert.equal(first, second);
  assert.notEqual(first, primitive(registry, PrimitiveKind.PrimitiveString));
});

test("type facts cache identifies flat fields and reference-bearing types", () => {
  const registry = new TypeRegistry("facts-test", undefined);
  const int = primitive(registry, PrimitiveKind.PrimitiveInt);
  const string = primitive(registry, PrimitiveKind.PrimitiveString);
  const flat = registry.runtime({
    kind: Kind.Struct,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "flat",
  });
  flat.dynamic = {
    kind: Kind.Struct,
    fields: [
      { name: "number", type: int, tag: "", embedded: false },
      { name: "text", type: string, tag: "", embedded: false },
    ],
  };
  const pointer = registry.runtime({
    kind: Kind.Pointer,
    primitive: 0,
    named: { module_path: "", decl_id: "" },
    node: "pointer",
  });
  pointer.dynamic = { kind: Kind.Pointer, elem: int };
  assert.deepEqual(registry.facts(flat), {
    kind: Kind.Struct,
    primitive: 0,
    containsReferences: false,
    flat: true,
    depth: 1,
    fieldCount: 2,
  });
  assert.equal(registry.facts(flat), registry.facts(flat));
  assert.equal(registry.facts(pointer).containsReferences, true);
  assert.equal(registry.facts(pointer).flat, false);
});
