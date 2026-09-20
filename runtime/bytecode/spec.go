package bytecode

import "encoding/json"

type Spec struct {
	Format                  string        `json:"format"`
	Version                 int           `json:"version"`
	OpcodeSet               string        `json:"opcode_set"`
	SymbolsFormat           string        `json:"symbols_format"`
	SymbolsVersion          int           `json:"symbols_version"`
	SymbolsContract         string        `json:"symbols_contract"`
	ArtifactFields          []FieldSpec   `json:"artifact_fields"`
	ModuleFields            []FieldSpec   `json:"module_fields"`
	TypeRefFields           []FieldSpec   `json:"type_ref_fields"`
	TypeNodeFields          []FieldSpec   `json:"type_node_fields"`
	FunctionTypeFields      []FieldSpec   `json:"function_type_fields"`
	ConstantFields          []FieldSpec   `json:"constant_fields"`
	GlobalFields            []FieldSpec   `json:"global_fields"`
	FunctionFields          []FieldSpec   `json:"function_fields"`
	LocalFields             []FieldSpec   `json:"local_fields"`
	UpvalueFields           []FieldSpec   `json:"upvalue_fields"`
	InstructionFields       []FieldSpec   `json:"instruction_fields"`
	ExportFields            []FieldSpec   `json:"export_fields"`
	RequirementFields       []FieldSpec   `json:"requirement_fields"`
	ProgramSymbolFields     []FieldSpec   `json:"program_symbol_fields"`
	PackageSymbolFields     []FieldSpec   `json:"package_symbol_fields"`
	GlobalSymbolFields      []FieldSpec   `json:"global_symbol_fields"`
	FunctionSymbolFields    []FieldSpec   `json:"function_symbol_fields"`
	LocalSymbolFields       []FieldSpec   `json:"local_symbol_fields"`
	UpvalueSymbolFields     []FieldSpec   `json:"upvalue_symbol_fields"`
	InstructionSymbolFields []FieldSpec   `json:"instruction_symbol_fields"`
	DebugScopeFields        []FieldSpec   `json:"debug_scope_fields"`
	PCRangeFields           []FieldSpec   `json:"pc_range_fields"`
	SourceFileFields        []FieldSpec   `json:"source_file_fields"`
	LocationFields          []FieldSpec   `json:"location_fields"`
	ValidationLimits        []FieldSpec   `json:"validation_limits"`
	Payloads                []PayloadSpec `json:"payloads"`
	Opcodes                 []OpcodeSpec  `json:"opcodes"`
	TypeRules               []string      `json:"type_rules"`
	HashRule                string        `json:"hash_rule"`
	RequirementRule         string        `json:"requirement_rule"`
	SymbolRule              string        `json:"symbol_rule"`
}

type FieldSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required,omitempty"`
	Rule     string `json:"rule,omitempty"`
}

type PayloadSpec struct {
	Name   string      `json:"name"`
	Fields []FieldSpec `json:"fields,omitempty"`
}

type OpcodeSpec struct {
	Op       string `json:"op"`
	Category string `json:"category"`
	Payload  string `json:"payload,omitempty"`
	Stack    string `json:"stack"`
	Terminal bool   `json:"terminal,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

func CurrentSpec() Spec {
	return Spec{
		Format:                  Format,
		Version:                 CurrentVersion,
		OpcodeSet:               OpcodeSet,
		SymbolsFormat:           SymbolsFormat,
		SymbolsVersion:          SymbolsVersion,
		SymbolsContract:         SymbolsContract,
		ArtifactFields:          artifactFieldSpecs(),
		ModuleFields:            moduleFieldSpecs(),
		TypeRefFields:           typeRefFieldSpecs(),
		TypeNodeFields:          typeNodeFieldSpecs(),
		FunctionTypeFields:      functionTypeFieldSpecs(),
		ConstantFields:          constantFieldSpecs(),
		GlobalFields:            globalFieldSpecs(),
		FunctionFields:          functionFieldSpecs(),
		LocalFields:             localFieldSpecs(),
		UpvalueFields:           upvalueFieldSpecs(),
		InstructionFields:       instructionFieldSpecs(),
		ExportFields:            exportFieldSpecs(),
		RequirementFields:       requirementFieldSpecs(),
		ProgramSymbolFields:     programSymbolFieldSpecs(),
		PackageSymbolFields:     packageSymbolFieldSpecs(),
		GlobalSymbolFields:      globalSymbolFieldSpecs(),
		FunctionSymbolFields:    functionSymbolFieldSpecs(),
		LocalSymbolFields:       localSymbolFieldSpecs(),
		UpvalueSymbolFields:     upvalueSymbolFieldSpecs(),
		InstructionSymbolFields: instructionSymbolFieldSpecs(),
		DebugScopeFields:        debugScopeFieldSpecs(),
		PCRangeFields:           pcRangeFieldSpecs(),
		SourceFileFields:        sourceFileFieldSpecs(),
		LocationFields:          locationFieldSpecs(),
		ValidationLimits:        validationLimitFieldSpecs(),
		Payloads:                PayloadSpecs(),
		Opcodes:                 OpcodeSpecs(),
		TypeRules:               typeRules(),
		HashRule:                "sha256 over EncodeJSON canonical artifact bytes; canonical JSON is validated, emitted with stable struct field order and no trailing newline",
		RequirementRule:         "artifact requirements declare source modules by exact module_path, hash, and exports; loader rejects missing or mismatched requirements before execution",
		SymbolRule:              "ProgramSymbols is an optional immutable sidecar bound to one ProgramHash and exact package CodeHash values; it never changes executable identity",
	}
}

func EncodeSpecJSON() ([]byte, error) {
	data, err := json.MarshalIndent(CurrentSpec(), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func OpcodeSpecs() []OpcodeSpec {
	return append([]OpcodeSpec(nil), opcodeSpecs...)
}

func OpcodeSpecFor(op string) (OpcodeSpec, bool) {
	spec, ok := opcodeSpecByOp[op]
	return spec, ok
}

func PayloadSpecs() []PayloadSpec {
	return append([]PayloadSpec(nil), payloadSpecs...)
}

func PayloadSpecFor(name string) (PayloadSpec, bool) {
	spec, ok := payloadSpecByName[name]
	return spec, ok
}

func AllOpcodes() []string {
	out := make([]string, 0, len(opcodeSpecs))
	for _, spec := range opcodeSpecs {
		out = append(out, spec.Op)
	}
	return out
}

func artifactFieldSpecs() []FieldSpec {
	return []FieldSpec{
		{Name: "format", Type: "string", Required: true, Rule: `must be "mini-go-ir"`},
		{Name: "version", Type: "int", Required: true, Rule: "must match current IR version"},
		{Name: "opcode_set", Type: "string", Required: true, Rule: `must be "` + OpcodeSet + `"`},
		{Name: "module", Type: "Module", Required: true},
		{Name: "type_table", Type: "TypeTable", Required: true, Rule: "structured TypeRef graph; no canonical type text"},
		{Name: "constants", Type: "[]Constant"},
		{Name: "globals", Type: "[]Global"},
		{Name: "functions", Type: "[]Function"},
		{Name: "exports", Type: "[]Export"},
		{Name: "requirements", Type: "[]Requirement"},
	}
}

func moduleFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "path", Type: "string", Required: true}, {Name: "package", Type: "string", Required: true}}
}

func typeRefFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "kind", Type: "TypeKind", Required: true}, {Name: "primitive", Type: "PrimitiveKind"}, {Name: "named", Type: "{module_path,decl_id}", Rule: "stable named identity"}, {Name: "node", Type: "type-node-id", Rule: "references TypeTable.nodes"}}
}

func typeNodeFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "type-node-id", Required: true}, {Name: "kind", Type: "TypeKind", Required: true}, {Name: "name", Type: "string"}, {Name: "primitive", Type: "PrimitiveKind"}, {Name: "identity", Type: "{module_path,decl_id}"}, {Name: "alias", Type: "bool"}, {Name: "alias_target", Type: "TypeRef"}, {Name: "underlying", Type: "TypeRef"}, {Name: "elem", Type: "TypeRef"}, {Name: "key", Type: "TypeRef"}, {Name: "length", Type: "int64", Rule: "arrays require a non-negative length"}, {Name: "direction", Type: "ChannelDir"}, {Name: "signature", Type: "FunctionSignature"}, {Name: "tuple", Type: "[]TypeRef"}, {Name: "fields", Type: "[]Field"}, {Name: "methods", Type: "[]Method"}, {Name: "terms", Type: "[]TypeTerm"}, {Name: "type_set", Type: "bool"}, {Name: "constraint", Type: "TypeRef", Rule: "compiler-only"}, {Name: "base", Type: "TypeRef", Rule: "compiler-only"}, {Name: "type_args", Type: "[]TypeRef", Rule: "compiler-only"}}
}

func functionTypeFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "params", Type: "[]{type:TypeRef}"}, {Name: "results", Type: "[]TypeRef"}, {Name: "variadic", Type: "bool", Rule: "requires a final slice parameter"}}
}

func constantFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "string", Required: true}, {Name: "type", Type: "TypeRef", Required: true}, {Name: "value", Type: "json", Required: true, Rule: "must decode according to type; byte slice constants use canonical base64 strings; exact untyped numbers use canonical text"}, {Name: "untyped", Type: "bool"}}
}

func globalFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "string", Required: true}, {Name: "type", Type: "TypeRef", Required: true}}
}

func functionFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "string", Required: true}, {Name: "revision_local", Type: "bool"}, {Name: "signature", Type: "FunctionSignature", Required: true}, {Name: "locals", Type: "[]Local"}, {Name: "result_locals", Type: "[]local-id", Rule: "must match named result order"}, {Name: "upvalues", Type: "[]Upvalue"}, {Name: "max_stack", Type: "int"}, {Name: "instructions", Type: "[]Instruction"}}
}

func localFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "local-id", Required: true}, {Name: "type", Type: "TypeRef", Required: true}}
}

func debugScopeFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "scope-id", Required: true}, {Name: "parent", Type: "scope-id"}, {Name: "ranges", Type: "[]PCRange"}}
}

func pcRangeFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "start", Type: "int", Required: true}, {Name: "end", Type: "int", Required: true, Rule: "half-open final instruction range [start,end)"}}
}

func upvalueFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "upvalue-id", Required: true}, {Name: "type", Type: "TypeRef", Required: true}}
}

func instructionFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "op", Type: "opcode", Required: true}, {Name: "payload", Type: "opcode-payload"}}
}

func exportFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "name", Type: "string", Required: true}, {Name: "kind", Type: "string", Required: true}, {Name: "id", Type: "string", Required: true}, {Name: "type", Type: "TypeRef", Rule: "required for constant exports"}, {Name: "untyped", Type: "bool", Rule: "only valid for constant exports"}}
}

func requirementFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "kind", Type: "string", Required: true}, {Name: "module_path", Type: "string", Required: true}, {Name: "hash", Type: "string"}, {Name: "exports", Type: "[]string"}}
}

func programSymbolFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "format", Type: "string", Required: true}, {Name: "version", Type: "int", Required: true}, {Name: "compiler_id", Type: "string", Required: true}, {Name: "contract_id", Type: "string", Required: true}, {Name: "program_hash", Type: "sha256-string", Required: true}, {Name: "optimization", Type: "uint8"}, {Name: "packages", Type: "map[module-path]PackageSymbols", Required: true}, {Name: "hash", Type: "sha256-string", Required: true}}
}

func packageSymbolFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "module_path", Type: "string", Required: true}, {Name: "code_hash", Type: "sha256-string", Required: true}, {Name: "source_hash", Type: "sha256-string"}, {Name: "files", Type: "[]SourceFile"}, {Name: "globals", Type: "[]GlobalSymbol"}, {Name: "functions", Type: "[]FunctionSymbols"}}
}

func globalSymbolFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "global-id", Required: true}, {Name: "name", Type: "string", Required: true}}
}

func functionSymbolFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "function-id", Required: true}, {Name: "name", Type: "string", Required: true}, {Name: "generated", Type: "bool"}, {Name: "declaration", Type: "Location"}, {Name: "locals", Type: "[]LocalSymbol"}, {Name: "upvalues", Type: "[]UpvalueSymbol"}, {Name: "scopes", Type: "[]DebugScope"}, {Name: "locations", Type: "[]InstructionSymbol"}}
}

func localSymbolFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "local-id", Required: true}, {Name: "name", Type: "string"}, {Name: "scope", Type: "scope-id"}, {Name: "generated", Type: "bool"}, {Name: "declaration", Type: "Location"}}
}

func upvalueSymbolFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "upvalue-id", Required: true}, {Name: "name", Type: "string"}}
}

func instructionSymbolFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "pc", Type: "int", Required: true}, {Name: "points", Type: "[]Location", Required: true, Rule: "ordered unique source locations; first is primary"}}
}

func sourceFileFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "id", Type: "string", Required: true}, {Name: "path", Type: "string", Required: true}, {Name: "hash", Type: "sha256-string"}}
}

func locationFieldSpecs() []FieldSpec {
	return []FieldSpec{{Name: "file", Type: "debug-file-id-or-path", Required: true}, {Name: "line", Type: "int", Required: true, Rule: "positive"}, {Name: "column", Type: "int", Required: true, Rule: "non-negative"}}
}

func validationLimitFieldSpecs() []FieldSpec {
	return append([]FieldSpec(nil), validationLimitSpecs...)
}

func typeRules() []string {
	return []string{
		"TypeRef.kind selects either an inline void/any/primitive/named value or a TypeTable node",
		"named identity is the pair (module_path, decl_id)",
		"slice and array are distinct kinds; array length is a non-negative part of type identity",
		"function variadic metadata is retained in FunctionSignature",
		"type parameters and instances are compiler-only and are rejected by runtime validation",
	}
}

var payloadSpecByName = makePayloadSpecMap(payloadSpecs)

func makePayloadSpecMap(specs []PayloadSpec) map[string]PayloadSpec {
	out := make(map[string]PayloadSpec, len(specs))
	for _, spec := range specs {
		out[spec.Name] = spec
	}
	return out
}

var opcodeSpecByOp = makeOpcodeSpecMap(opcodeSpecs)

func makeOpcodeSpecMap(specs []OpcodeSpec) map[string]OpcodeSpec {
	out := make(map[string]OpcodeSpec, len(specs))
	for _, spec := range specs {
		out[spec.Op] = spec
	}
	return out
}
