package bytecode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
)

func TestHashIsStableForSameArtifact(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{ID: "fn.main", Signature: testSignature("function() Void")}}

	left, err := Hash(&artifact)
	if err != nil {
		t.Fatalf("Hash failed: %v", err)
	}
	right, err := Hash(&artifact)
	if err != nil {
		t.Fatalf("Hash failed: %v", err)
	}
	if left != right {
		t.Fatalf("expected stable hash, got %q and %q", left, right)
	}
}

func TestHashMatchesCanonicalJSONSHA256(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{ID: "fn.main", Signature: testSignature("function() Void")}}

	payload, err := CanonicalJSON(&artifact)
	if err != nil {
		t.Fatalf("CanonicalJSON failed: %v", err)
	}
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	got, err := Hash(&artifact)
	if err != nil {
		t.Fatalf("Hash failed: %v", err)
	}
	if got != want {
		t.Fatalf("expected hash %q, got %q", want, got)
	}
	encoded, combinedHash, err := EncodeJSONAndHash(&artifact)
	if err != nil {
		t.Fatalf("EncodeJSONAndHash failed: %v", err)
	}
	if string(encoded) != string(payload) || combinedHash != want {
		t.Fatalf("combined encode/hash mismatch: hash=%q payload=%q", combinedHash, encoded)
	}
}

func TestMarshalPayloadUsesArtifactStringPolicy(t *testing.T) {
	payload, err := MarshalPayload(OperatorPayload{Operator: ">"})
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"operator":">"}` {
		t.Fatalf("payload = %s", payload)
	}
}

func TestExecutionImageHashUsesCanonicalStringPolicy(t *testing.T) {
	image := ExecutionImage{
		Format: ExecutionFormat, Version: ExecutionVersion,
		Packages: map[string]PackageArchive{
			"example/main": {Artifact: json.RawMessage(`{"operator":">"}`)},
		},
	}
	first, err := HashExecutionImage(image)
	if err != nil {
		t.Fatal(err)
	}
	image.Packages["example/main"] = PackageArchive{Artifact: json.RawMessage(`{"operator":"\u003e"}`)}
	second, err := HashExecutionImage(image)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different raw payload encodings produced the same image identity")
	}
}

func TestExecutionImageStreamingHashMatchesCanonicalEncoding(t *testing.T) {
	image := ExecutionImage{
		Format: ExecutionFormat, Version: ExecutionVersion, CompilerID: "compiler", ContractID: ExecutionContract,
		Root: "example/main", Entries: []Entry{{Name: DefaultEntryName, ModulePath: "example/main", FunctionID: "main"}},
		Packages: map[string]PackageArchive{
			"example/main": {
				Artifact:     json.RawMessage(`{"format":"test","text":"<>&"}`),
				ArtifactHash: strings.Repeat("a", 64),
			},
		},
	}
	payload, err := encodeCanonicalValue(image)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)
	got, err := HashExecutionImage(image)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("streaming hash = %s, want %x", got, want)
	}
}

func TestDecodeJSONRoundTrip(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	setTestNamedTypes(&artifact, []testNamedType{{
		Name: "User",
		Type: testType("struct{ID:Int64 `json:\"id\"`}"),
		Fields: []testTypeField{{
			Name: "ID",
			Type: testType("Int64"),
			Tag:  `json:"id"`,
		}},
	}})
	artifact.Constants = []Constant{{ID: "c.answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Int64"),
		Instructions: []Instruction{{
			Op:      string(OpConst),
			Payload: json.RawMessage(`{"constant":"c.answer"}`),
		}, {
			Op:      string(OpReturn),
			Payload: json.RawMessage(`{"result_count":1}`),
		}},
	}}
	artifact.Exports = []Export{{Name: "Main", Kind: "function", ID: "fn.main"}}
	attachTestTypeNodes(&artifact)

	data, err := EncodeJSON(&artifact)
	if err != nil {
		t.Fatalf("EncodeJSON failed: %v", err)
	}
	decoded, err := DecodeJSON(data)
	if err != nil {
		t.Fatalf("DecodeJSON failed: %v", err)
	}
	declarations := decoded.TypeTable.DefinedNamed(decoded.Module.Path)
	shape, _ := decoded.TypeTable.Node(decoded.TypeTable.Underlying(types.Ref(declarations[0])))
	if decoded.Module.Path != artifact.Module.Path || len(declarations) != 1 || len(shape.Fields) != 1 || len(decoded.Functions) != 1 || len(decoded.Exports) != 1 {
		t.Fatalf("unexpected decoded artifact: %#v", decoded)
	}
	if shape.Fields[0].Tag != `json:"id"` {
		t.Fatalf("expected decoded struct field tag, got %#v", shape.Fields)
	}
	leftHash, err := Hash(&artifact)
	if err != nil {
		t.Fatalf("Hash original failed: %v", err)
	}
	rightHash, err := Hash(&decoded)
	if err != nil {
		t.Fatalf("Hash decoded failed: %v", err)
	}
	if leftHash != rightHash {
		t.Fatalf("expected matching hashes, got %q and %q", leftHash, rightHash)
	}
}

func TestCurrentSpecDescribesArtifactAndOpcodes(t *testing.T) {
	spec := CurrentSpec()
	if spec.Format != Format || spec.Version != CurrentVersion || spec.OpcodeSet != OpcodeSet {
		t.Fatalf("unexpected spec identity: %#v", spec)
	}
	typeRules := strings.Join(spec.TypeRules, "\n")
	if !strings.Contains(typeRules, "slice and array") || !fieldSpecPresent(spec.TypeRefFields, "kind") || !fieldSpecPresent(spec.TypeNodeFields, "length") {
		t.Fatalf("expected structured type rules, got %#v", spec)
	}
	data, err := EncodeSpecJSON()
	if err != nil {
		t.Fatalf("EncodeSpecJSON failed: %v", err)
	}
	var decoded Spec
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("spec JSON did not decode: %v", err)
	}
	if decoded.Format != Format || decoded.Version != CurrentVersion || decoded.OpcodeSet != OpcodeSet {
		t.Fatalf("unexpected decoded spec: %#v", decoded)
	}
	if !fieldSpecPresent(decoded.ExportFields, "untyped") {
		t.Fatalf("expected export.untyped in spec, got %#v", decoded.ExportFields)
	}
	if !fieldSpecPresent(decoded.ConstantFields, "untyped") {
		t.Fatalf("expected constant.untyped in spec, got %#v", decoded.ConstantFields)
	}
	if fieldSpecPresent(decoded.ArtifactFields, "debug") || fieldSpecPresent(decoded.FunctionFields, "name") || fieldSpecPresent(decoded.InstructionFields, "source_points") {
		t.Fatalf("executable spec still contains source symbols: %#v", decoded)
	}
	if decoded.SymbolsFormat != SymbolsFormat || decoded.SymbolsVersion != SymbolsVersion || !fieldSpecPresent(decoded.ProgramSymbolFields, "program_hash") || !fieldSpecPresent(decoded.FunctionSymbolFields, "locations") {
		t.Fatalf("program symbol sidecar is missing from spec: %#v", decoded)
	}
}

func TestDecodeJSONRoundTripPreservesUntypedConstExport(t *testing.T) {
	artifact := NewArtifact("example/module", "dep")
	artifact.Constants = []Constant{{ID: "const.Max", Type: testType("Int"), Value: json.RawMessage(`"18446744073709551615"`), Untyped: true}}
	artifact.Exports = []Export{{Name: "Max", Kind: "const", ID: "const.Max", Type: testType("Int"), Untyped: true}}
	attachTestTypeNodes(&artifact)

	data, err := EncodeJSON(&artifact)
	if err != nil {
		t.Fatalf("EncodeJSON failed: %v", err)
	}
	decoded, err := DecodeJSON(data)
	if err != nil {
		t.Fatalf("DecodeJSON failed: %v", err)
	}
	if len(decoded.Constants) != 1 || !decoded.Constants[0].Untyped || len(decoded.Exports) != 1 || !decoded.Exports[0].Untyped {
		t.Fatalf("expected untyped const metadata to round trip, constants=%#v exports=%#v", decoded.Constants, decoded.Exports)
	}
}

func fieldSpecPresent(fields []FieldSpec, name string) bool {
	for _, field := range fields {
		if field.Name == name {
			return true
		}
	}
	return false
}

func TestValidationIssueCodesAreStable(t *testing.T) {
	if code := ValidationCode(testValidateArtifact(nil)); code != "ir.artifact.nil" {
		t.Fatalf("expected nil artifact code, got %q", code)
	}

	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Instructions: []Instruction{{
			Op: "does_not_exist",
		}},
	}}
	err := testValidateArtifact(&artifact)
	if code := ValidationCode(err); code != "ir.opcode.unknown" {
		t.Fatalf("expected unknown opcode code, got %q from %v", code, err)
	}

	artifact = NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Instructions: []Instruction{{
			Op:      string(OpConst),
			Payload: json.RawMessage(`{"constant":"c.absent"}`),
		}},
	}}
	err = testValidateArtifact(&artifact)
	if code := ValidationCode(err); code != "ir.reference.unknown" {
		t.Fatalf("expected unknown reference code, got %q from %v", code, err)
	}

	issue, ok := ValidationIssueFromError(err)
	if !ok {
		t.Fatalf("expected validation issue")
	}
	if issue.Code != "ir.reference.unknown" || issue.Path == "" || issue.Message == "" {
		t.Fatalf("unexpected validation issue: %#v", issue)
	}
}

func TestValidationCodeDoesNotDependOnMessageText(t *testing.T) {
	first := newCodedValidationError(ValidationSchemaMismatch, "functions[0]", errors.New("first diagnostic"))
	second := newCodedValidationError(ValidationSchemaMismatch, "functions[0]", errors.New("different diagnostic"))
	if ValidationCode(first) != ValidationSchemaMismatch || ValidationCode(second) != ValidationSchemaMismatch {
		t.Fatalf("message changed validation code: first=%q second=%q", ValidationCode(first), ValidationCode(second))
	}
}

func TestValidationLimitCode(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{ID: "fn.main", Signature: testSignature("function() Void")}}
	limits := DefaultValidationLimits()
	limits.MaxFunctions = 0
	if err := ValidateArtifactWithLimits(&artifact, limits); err != nil {
		t.Fatalf("zero max functions should disable the limit, got %v", err)
	}
	limits.MaxFunctions = -1
	if err := ValidateArtifactWithLimits(&artifact, limits); err != nil {
		t.Fatalf("negative max functions should disable the limit, got %v", err)
	}
	limits.MaxFunctions = 1
	if err := ValidateArtifactWithLimits(&artifact, limits); err != nil {
		t.Fatalf("single function should fit limit, got %v", err)
	}
	limits.MaxFunctions = 0
	artifact.Functions = append(artifact.Functions, Function{ID: "fn.other", Signature: testSignature("function() Void")})
	if err := ValidateArtifactWithLimits(&artifact, limits); err != nil {
		t.Fatalf("zero max functions should still disable the limit, got %v", err)
	}
	limits.MaxFunctions = 1
	err := ValidateArtifactWithLimits(&artifact, limits)
	if code := ValidationCode(err); code != "ir.limit.exceeded" {
		t.Fatalf("expected limit code, got %q from %v", code, err)
	}
}

func TestDecodeJSONRejectsUnknownArtifactField(t *testing.T) {
	data := []byte(`{
		"format":"mini-go-ir",
		"version":22,
		"opcode_set":"minigo.ir.v10",
		"module":{"path":"example/module","package":"main"},
		"type_table":{"nodes":[]},
		"functions":[{"id":"fn.main","signature":{"params":[],"results":[]}}],
		"extra":true
	}`)

	_, err := DecodeJSON(data)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestDecodeJSONRejectsUnknownPayloadField(t *testing.T) {
	data := []byte(`{
		"format":"mini-go-ir",
		"version":22,
		"opcode_set":"minigo.ir.v10",
		"module":{"path":"example/module","package":"main"},
		"type_table":{"nodes":[]},
		"constants":[{"id":"c.answer","type":{"kind":3,"primitive":7},"value":42}],
		"functions":[{
			"id":"fn.main",
			"signature":{"params":[],"results":[{"kind":3,"primitive":7}]},
			"instructions":[
				{"op":"const","payload":{"constant":"c.answer","extra":true}},
				{"op":"return","payload":{"result_count":1}}
			]
		}]
	}`)

	_, err := DecodeJSON(data)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown payload field error, got %v", err)
	}
}
