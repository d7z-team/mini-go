package bytecode

const (
	RuntimeNodeBytes     = 128
	RuntimeSlotBytes     = 16
	RuntimeMapEntryBytes = 32
	RuntimeByteBytes     = 1
)

var payloadSpecs = []PayloadSpec{
	{Name: "select", Fields: []FieldSpec{{Name: "index", Type: "local-id", Required: true}, {Name: "default", Type: "bool"}, {Name: "cases", Type: "[]select-case", Rule: "evaluated channel local and either send local or receive value/ok locals"}}},
	{Name: "const", Fields: []FieldSpec{{Name: "constant", Type: "constant-id", Required: true}}},
	{Name: "local", Fields: []FieldSpec{{Name: "local", Type: "local-id", Required: true}, {Name: "rebind", Type: "bool", Rule: "store_local only: replace the runtime local before a declaration binding is stored"}}},
	{Name: "upvalue", Fields: []FieldSpec{{Name: "upvalue", Type: "upvalue-id", Required: true}}},
	{Name: "global", Fields: []FieldSpec{{Name: "global", Type: "global-id", Required: true}}},
	{Name: "address", Fields: []FieldSpec{{Name: "kind", Type: "enum(local,upvalue,global,export)", Required: true}, {Name: "local", Type: "local-id"}, {Name: "upvalue", Type: "upvalue-id"}, {Name: "global", Type: "global-id"}, {Name: "module_path", Type: "module-path", Rule: "required for export addresses; mutually exclusive with local, upvalue and global ids"}, {Name: "export", Type: "export-name", Rule: "required for export addresses; must resolve to a global variable"}, {Name: "path", Type: "[]address-segment(field,index,indirect)"}}},
	{Name: "type", Fields: []FieldSpec{{Name: "type", Type: "canonical-type", Required: true}, {Name: "variadic", Type: "bool", Rule: "when true, type must be a function signature with a final Slice<T> parameter"}}},
	{Name: "operator", Fields: []FieldSpec{{Name: "operator", Type: "string", Required: true}}},
	{Name: "make_sequence", Fields: []FieldSpec{{Name: "type", Type: "canonical-type", Required: true}, {Name: "element_count", Type: "int", Required: true, Rule: "non-negative"}}},
	{Name: "make_map", Fields: []FieldSpec{{Name: "type", Type: "canonical-type", Required: true}, {Name: "entry_count", Type: "int", Required: true, Rule: "non-negative key/value pair count"}, {Name: "has_capacity", Type: "bool", Rule: "when true, pop a capacity hint before key/value pairs"}}},
	{Name: "make_struct", Fields: []FieldSpec{{Name: "type", Type: "canonical-type", Required: true}, {Name: "fields", Type: "[]field-name"}}},
	{Name: "make_slice", Fields: []FieldSpec{{Name: "type", Type: "canonical-type", Required: true}, {Name: "has_capacity", Type: "bool"}}},
	{Name: "make_waitable", Fields: []FieldSpec{{Name: "type", Type: "canonical-type", Required: true}}},
	{Name: "count", Fields: []FieldSpec{{Name: "count", Type: "int", Required: true, Rule: "non-negative"}, {Name: "expand", Type: "bool"}}},
	{Name: "field", Fields: []FieldSpec{{Name: "field", Type: "field-name", Required: true}}},
	{Name: "export", Fields: []FieldSpec{{Name: "module_path", Type: "module-path", Required: true}, {Name: "export", Type: "export-name", Required: true}}},
	{Name: "init_module", Fields: []FieldSpec{{Name: "module_path", Type: "module-path", Required: true}}},
	{Name: "label", Fields: []FieldSpec{{Name: "label", Type: "label", Required: true}}},
	{Name: "jump", Fields: []FieldSpec{{Name: "label", Type: "label", Required: true}}},
	{Name: "call", Fields: []FieldSpec{{Name: "module_path", Type: "module-path", Rule: "optional exact target module; omitted for the current module"}, {Name: "function", Type: "function-id"}, {Name: "arg_count", Type: "int", Required: true, Rule: "non-negative"}, {Name: "result_count", Type: "int", Rule: "non-negative"}}},
	{Name: "call_interface", Fields: []FieldSpec{{Name: "interface_type", Type: "canonical-type", Required: true}, {Name: "method", Type: "method-name", Required: true}, {Name: "arg_count", Type: "int", Required: true, Rule: "non-negative"}, {Name: "result_count", Type: "int", Rule: "non-negative"}}},
	{Name: "closure", Fields: []FieldSpec{{Name: "module_path", Type: "module-path", Rule: "optional exact target module; omitted for the current module"}, {Name: "function", Type: "function-id", Required: true}, {Name: "captures", Type: "[]address", Rule: "local, upvalue or global captures in the creating frame"}}},
	{Name: "return", Fields: []FieldSpec{{Name: "result_count", Type: "int", Required: true, Rule: "non-negative"}}},
	{Name: "defer", Fields: []FieldSpec{{Name: "owner_depth", Type: "int", Rule: "optional non-negative caller depth; omitted or zero registers on the current frame"}}},
	{Name: "call_ffi", Fields: []FieldSpec{{Name: "arg_count", Type: "int", Required: true, Rule: "exactly two: route and payload"}, {Name: "result_count", Type: "int", Required: true, Rule: "exactly three: payload, message, status"}}},
	{Name: "call_intrinsic", Fields: []FieldSpec{{Name: "id", Type: "intrinsic-id", Required: true}, {Name: "arg_count", Type: "int", Required: true, Rule: "non-negative"}, {Name: "result_count", Type: "int", Rule: "non-negative"}}},
}

var opcodeSpecs = []OpcodeSpec{
	{Op: "select", Category: "waitable", Payload: "select", Stack: "no change", Notes: "commit exactly one communication, storing index and receive results in locals; may suspend"},
	{Op: "const", Category: "stack_value", Payload: "const", Stack: "push constant"},
	{Op: "zero", Category: "stack_value", Payload: "type", Stack: "push zero value"},
	{Op: "pop", Category: "stack_value", Stack: "pop 1"},
	{Op: "unary", Category: "stack_value", Payload: "operator", Stack: "pop 1, push 1"},
	{Op: "binary", Category: "stack_value", Payload: "operator", Stack: "pop 2, push 1"},
	{Op: "load_local", Category: "stack_value", Payload: "local", Stack: "push local"},
	{Op: "store_local", Category: "stack_value", Payload: "local", Stack: "pop value"},
	{Op: "load_upvalue", Category: "stack_value", Payload: "upvalue", Stack: "push upvalue"},
	{Op: "store_upvalue", Category: "stack_value", Payload: "upvalue", Stack: "pop value"},
	{Op: "load_global", Category: "stack_value", Payload: "global", Stack: "push global"},
	{Op: "store_global", Category: "stack_value", Payload: "global", Stack: "pop value"},
	{Op: "label", Category: "control", Payload: "label", Stack: "no change"},
	{Op: "jump", Category: "control", Payload: "jump", Stack: "no change", Terminal: true},
	{Op: "jump_if", Category: "control", Payload: "jump", Stack: "pop condition"},
	{Op: "return", Category: "control", Payload: "return", Stack: "pop count", Terminal: true},
	{Op: "panic", Category: "control", Stack: "pop panic value", Terminal: true},
	{Op: "recover", Category: "control", Stack: "push recovered panic value"},
	{Op: "defer_push", Category: "control", Payload: "defer", Stack: "pop function", Notes: "owner_depth lets compiler-lowered helper frames register a deferred call on an enclosing source frame"},
	{Op: "call_value", Category: "call", Payload: "call", Stack: "pop callee and args, push results"},
	{Op: "call_direct", Category: "call", Payload: "call", Stack: "pop args, push results"},
	{Op: "tail_call_direct", Category: "call", Payload: "call", Stack: "pop args and complete through target", Terminal: true},
	{Op: "call_interface", Category: "call", Payload: "call_interface", Stack: "pop interface receiver and args, push results", Notes: "dynamic dispatch by canonical interface method metadata; not Go source selector syntax"},
	{Op: "make_closure", Category: "call", Payload: "closure", Stack: "push closure"},
	{Op: "make_sequence", Category: "composite", Payload: "make_sequence", Stack: "pop elements, push sequence"},
	{Op: "make_map", Category: "composite", Payload: "make_map", Stack: "pop key/value pairs, push map"},
	{Op: "make_struct", Category: "composite", Payload: "make_struct", Stack: "pop fields, push struct"},
	{Op: "make_slice", Category: "composite", Payload: "make_slice", Stack: "pop len[/cap], push slice"},
	{Op: "make_waitable", Category: "resource", Payload: "make_waitable", Stack: "pop capacity, push waitable resource", Notes: "compiler-defined communication resource; backend provides the waitable protocol"},
	{Op: "load_index", Category: "composite", Stack: "pop object and index, push value"},
	{Op: "load_index_ok", Category: "composite", Stack: "pop map and key, push value and ok"},
	{Op: "string_rune_at", Category: "composite", Stack: "pop string and index, push rune"},
	{Op: "string_next_rune_index", Category: "composite", Stack: "pop string and index, push next index"},
	{Op: "slice", Category: "composite", Stack: "pop object/low/high/max, push slice"},
	{Op: "len", Category: "composite", Stack: "pop object, push len"},
	{Op: "cap", Category: "composite", Stack: "pop object, push cap"},
	{Op: "append", Category: "composite", Payload: "count", Stack: "pop slice and values, push slice"},
	{Op: "delete", Category: "composite", Stack: "pop map and key"},
	{Op: "clear", Category: "composite", Stack: "pop container"},
	{Op: "copy", Category: "composite", Stack: "pop dst/src, push copied count"},
	{Op: "map_keys", Category: "composite", Stack: "pop map, push array of keys"},
	{Op: "map_iter_init", Category: "composite", Payload: "local", Stack: "pop map; replace frame-owned iterator identified by local"},
	{Op: "map_iter_next", Category: "composite", Payload: "local", Stack: "push key, value, ok; skip deleted entries and observe current values"},
	{Op: "map_iter_close", Category: "composite", Payload: "local", Stack: "no change; release frame-owned iterator"},
	{Op: "load_field", Category: "composite", Payload: "field", Stack: "pop object, push field"},
	{Op: "store_index", Category: "composite", Stack: "pop object/index/value"},
	{Op: "store_field", Category: "composite", Payload: "field", Stack: "pop object/value"},
	{Op: "type_assert", Category: "type_pointer", Payload: "type", Stack: "pop value, push asserted value"},
	{Op: "type_assert_ok", Category: "type_pointer", Payload: "type", Stack: "pop value, push value and ok"},
	{Op: "convert", Category: "type_pointer", Payload: "type", Stack: "pop value, push converted value"},
	{Op: "address_of", Category: "type_pointer", Payload: "address", Stack: "push pointer", Notes: "export addresses initialize the target module through the scheduler and refer to its global slot"},
	{Op: "load_indirect", Category: "type_pointer", Stack: "pop pointer, push value"},
	{Op: "store_indirect", Category: "type_pointer", Stack: "pop pointer/value"},
	{Op: "waitable_send", Category: "waitable", Stack: "pop waitable/value", Notes: "compiler-defined send capability; may block through the generic scheduler"},
	{Op: "waitable_recv", Category: "waitable", Stack: "pop waitable, push value", Notes: "compiler-defined receive capability"},
	{Op: "waitable_recv_ok", Category: "waitable", Stack: "pop waitable, push value/ok", Notes: "compiler-defined receive capability with completion state"},
	{Op: "waitable_can_recv", Category: "waitable", Stack: "pop waitable, push ready", Notes: "receive readiness probe"},
	{Op: "waitable_try_recv", Category: "waitable", Stack: "pop waitable, push value/ready", Notes: "non-blocking receive attempt"},
	{Op: "waitable_try_send", Category: "waitable", Stack: "pop waitable/value, push ready", Notes: "non-blocking send attempt"},
	{Op: "waitable_can_send", Category: "waitable", Stack: "pop waitable, push ready", Notes: "send readiness probe"},
	{Op: "waitable_close", Category: "waitable", Stack: "pop waitable", Notes: "compiler-defined resource close operation"},
	{Op: "init_module", Category: "module", Payload: "init_module", Stack: "no change"},
	{Op: "load_export", Category: "module", Payload: "export", Stack: "push module export"},
	{Op: "spawn", Category: "scheduler", Payload: "call", Stack: "pop callee and args"},
	{Op: "call_ffi", Category: "host", Payload: "call_ffi", Stack: "pop route/payload, push payload/message/status", Notes: "opaque asynchronous host boundary; status: 0 success, 1 route unavailable, 2 failure"},
	{Op: "call_intrinsic", Category: "runtime", Payload: "call_intrinsic", Stack: "pop args, push results"},
}

var knownOpcodes = func() map[string]struct{} {
	opcodes := make(map[string]struct{}, len(opcodeSpecs))
	for _, spec := range opcodeSpecs {
		opcodes[spec.Op] = struct{}{}
	}
	return opcodes
}()

var defaultValidationLimits = ValidationLimits{
	MaxTypes:               100000,
	MaxConstants:           100000,
	MaxGlobals:             100000,
	MaxFunctions:           100000,
	MaxExports:             100000,
	MaxRequirements:        10000,
	MaxInstructions:        10000000,
	MaxLocalsPerFunction:   100000,
	MaxUpvaluesPerFunction: 100000,
	MaxPayloadBytes:        67108864,
	MaxConstantBytes:       67108864,
}

var validationLimitSpecs = []FieldSpec{
	{Name: "MaxTypes", Type: "int", Rule: "maximum type declarations; non-positive disables this limit"},
	{Name: "MaxConstants", Type: "int", Rule: "maximum constants; non-positive disables this limit"},
	{Name: "MaxGlobals", Type: "int", Rule: "maximum globals; non-positive disables this limit"},
	{Name: "MaxFunctions", Type: "int", Rule: "maximum functions; non-positive disables this limit"},
	{Name: "MaxExports", Type: "int", Rule: "maximum exports; non-positive disables this limit"},
	{Name: "MaxRequirements", Type: "int", Rule: "maximum module requirements; non-positive disables this limit"},
	{Name: "MaxInstructions", Type: "int", Rule: "maximum total instructions; non-positive disables this limit"},
	{Name: "MaxLocalsPerFunction", Type: "int", Rule: "maximum locals per function; non-positive disables this limit"},
	{Name: "MaxUpvaluesPerFunction", Type: "int", Rule: "maximum upvalues per function; non-positive disables this limit"},
	{Name: "MaxPayloadBytes", Type: "int", Rule: "maximum single payload bytes and total instruction payload bytes; non-positive disables this limit"},
	{Name: "MaxConstantBytes", Type: "int", Rule: "maximum single constant bytes and total constant bytes; non-positive disables this limit"},
}
