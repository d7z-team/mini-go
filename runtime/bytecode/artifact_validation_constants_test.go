package bytecode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
)

func TestValidateArtifactRejectsUnknownConstantReference(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{
		ID:        "fn.main",
		Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{"Int"}, []Instruction{{
			Op:      OpConst,
			Payload: ConstPayload{Constant: "c.missing"},
		}}, [][2][]uint32{{nil, {0}}}),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "unknown constant id") {
		t.Fatalf("expected unknown constant id error, got %v", err)
	}
}

func TestValidateArtifactRejectsInvalidConstantMetadata(t *testing.T) {
	tests := []struct {
		name     string
		constant Constant
		want     string
	}{
		{
			name:     "missing type",
			constant: Constant{ID: "const.Bad", Value: json.RawMessage(`1`)},
			want:     "invalid type reference",
		},
		{
			name:     "missing value",
			constant: Constant{ID: "const.Bad", Type: testType("Int64")},
			want:     "missing constant value",
		},
		{
			name:     "invalid json",
			constant: Constant{ID: "const.Bad", Type: testType("Int64"), Value: json.RawMessage(`{`)},
			want:     "invalid json constant value",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			artifact := NewArtifact("example/module", "main")
			artifact.Constants = []Constant{tc.constant}

			err := testValidateArtifact(&artifact)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateArtifactRejectsConstantValueShapeMismatch(t *testing.T) {
	tests := []struct {
		name     string
		constant Constant
		want     string
	}{
		{
			name:     "bool string",
			constant: Constant{ID: "const.Bad", Type: testType("Bool"), Value: json.RawMessage(`"true"`)},
			want:     "Bool constant requires JSON boolean",
		},
		{
			name:     "string number",
			constant: Constant{ID: "const.Bad", Type: testType("String"), Value: json.RawMessage(`1`)},
			want:     "String constant requires JSON string",
		},
		{
			name:     "byte slice number",
			constant: Constant{ID: "const.Bad", Type: testType("Slice<Uint8>"), Value: json.RawMessage(`1`)},
			want:     "byte slice constant requires canonical base64 JSON string",
		},
		{
			name:     "byte slice invalid base64",
			constant: Constant{ID: "const.Bad", Type: testType("Slice<Uint8>"), Value: json.RawMessage(`"%%%"`)},
			want:     "byte slice constant requires canonical base64 JSON string",
		},
		{
			name:     "byte slice noncanonical base64",
			constant: Constant{ID: "const.Bad", Type: testType("Slice<Uint8>"), Value: json.RawMessage(`"AA"`)},
			want:     "byte slice constant requires canonical base64 JSON string",
		},
		{
			name:     "signed fractional",
			constant: Constant{ID: "const.Bad", Type: testType("Int64"), Value: json.RawMessage(`"1.5"`)},
			want:     "signed integer constant requires integral value",
		},
		{
			name:     "signed overflow",
			constant: Constant{ID: "const.Bad", Type: testType("Int8"), Value: json.RawMessage(`128`)},
			want:     "signed integer constant out of range",
		},
		{
			name:     "unsigned negative",
			constant: Constant{ID: "const.Bad", Type: testType("Uint64"), Value: json.RawMessage(`"-1"`)},
			want:     "unsigned integer constant requires non-negative integral value",
		},
		{
			name:     "unsigned overflow",
			constant: Constant{ID: "const.Bad", Type: testType("Uint8"), Value: json.RawMessage(`256`)},
			want:     "unsigned integer constant out of range",
		},
		{
			name:     "float nonfinite",
			constant: Constant{ID: "const.Bad", Type: testType("Float64"), Value: json.RawMessage(`"1e1000"`)},
			want:     "float constant requires finite JSON number",
		},
		{
			name:     "float32 overflow",
			constant: Constant{ID: "const.Bad", Type: testType("Float32"), Value: json.RawMessage(`1e100`)},
			want:     "float constant out of range for Float32",
		},
		{
			name:     "complex missing imag",
			constant: Constant{ID: "const.Bad", Type: testType("Complex128"), Value: json.RawMessage(`{"real":1}`)},
			want:     "complex constant requires finite real and imaginary components",
		},
		{
			name:     "complex scalar text",
			constant: Constant{ID: "const.Bad", Type: testType("Complex128"), Value: json.RawMessage(`"1+2i"`)},
			want:     "complex constant requires finite real and imaginary components",
		},
		{
			name:     "complex extra field",
			constant: Constant{ID: "const.Bad", Type: testType("Complex128"), Value: json.RawMessage(`{"real":1,"imag":2,"extra":3}`)},
			want:     "complex constant requires finite real and imaginary components",
		},
		{
			name:     "complex quoted component",
			constant: Constant{ID: "const.Bad", Type: testType("Complex128"), Value: json.RawMessage(`{"real":"1","imag":2}`)},
			want:     "complex constant requires finite real and imaginary components",
		},
		{
			name:     "complex64 overflow",
			constant: Constant{ID: "const.Bad", Type: testType("Complex64"), Value: json.RawMessage(`{"real":1e100,"imag":0}`)},
			want:     "complex constant out of range for Complex64",
		},
		{
			name:     "array non-null",
			constant: Constant{ID: "const.Bad", Type: testType("Array<1, Int64>"), Value: json.RawMessage(`[1]`)},
			want:     "constant value is unsupported for non-scalar type",
		},
		{
			name:     "non nilable null",
			constant: Constant{ID: "const.Bad", Type: testType("Int64"), Value: json.RawMessage(`null`)},
			want:     "null constant requires nil-able type",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			artifact := NewArtifact("example/module", "main")
			artifact.Constants = []Constant{tc.constant}

			err := testValidateArtifact(&artifact)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateArtifactAcceptsConstantValueShapes(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Constants = []Constant{
		{ID: "const.Bool", Type: testType("Bool"), Value: json.RawMessage(`true`)},
		{ID: "const.String", Type: testType("String"), Value: json.RawMessage(`"ok"`)},
		{ID: "const.Int", Type: testType("Int8"), Value: json.RawMessage(`-128`)},
		{ID: "const.Uint", Type: testType("Uint8"), Value: json.RawMessage(`"255"`)},
		{ID: "const.Float", Type: testType("Float32"), Value: json.RawMessage(`1.5`)},
		{ID: "const.ComplexObject", Type: testType("Complex128"), Value: json.RawMessage(`{"real":1,"imag":2}`)},
		{ID: "const.Complex64", Type: testType("Complex64"), Value: json.RawMessage(`{"real":1,"imag":2}`)},
		{ID: "const.PointerNil", Type: testType("Ptr<Int64>"), Value: json.RawMessage(`null`)},
		{ID: "const.SliceNil", Type: testType("Slice<Int64>"), Value: json.RawMessage(`null`)},
		{ID: "const.AnyValue", Type: types.AnyType(), Value: json.RawMessage(`{"kind":"opaque"}`)},
		{ID: "const.UntypedBig", Type: testType("Int"), Value: json.RawMessage(`"18446744073709551615"`), Untyped: true},
	}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}

func TestValidateArtifactAllowsExternalNamedConstantShape(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Constants = []Constant{{
		ID: "const.External",
		Type: types.TypeRef{Kind: types.Named, Named: types.TypeKey{
			ModulePath: "example/dep",
			DeclID:     "Value",
		}},
		Value: json.RawMessage(`{"shape":"validated-by-dependency-artifact"}`),
	}}

	if err := testValidateArtifact(&artifact); err != nil {
		t.Fatalf("ValidateArtifact failed: %v", err)
	}
}

func TestValidateArtifactRejectsUnresolvedLocalNamedConstantShape(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Constants = []Constant{{
		ID: "const.Local",
		Type: types.TypeRef{Kind: types.Named, Named: types.TypeKey{
			ModulePath: "example/module",
			DeclID:     "Missing",
		}},
		Value: json.RawMessage(`1`),
	}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "constant type has unresolved local named type") {
		t.Fatalf("expected unresolved local named type error, got %v", err)
	}
}

func TestValidateArtifactRejectsInvalidConstExportMetadata(t *testing.T) {
	tests := []struct {
		name   string
		export Export
		want   string
	}{
		{
			name:   "missing type",
			export: Export{Name: "Answer", Kind: "const", ID: "const.Answer"},
			want:   "missing const export type",
		},
		{
			name:   "type mismatch",
			export: Export{Name: "Answer", Kind: "const", ID: "const.Answer", Type: testType("String")},
			want:   "const export type does not match constant type",
		},
		{
			name:   "untyped mismatch",
			export: Export{Name: "Answer", Kind: "const", ID: "const.Answer", Type: testType("Int64"), Untyped: true},
			want:   "const export untyped metadata does not match constant",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			artifact := NewArtifact("example/module", "main")
			artifact.Constants = []Constant{{ID: "const.Answer", Type: testType("Int64"), Value: json.RawMessage(`42`)}}
			artifact.Exports = []Export{tc.export}

			err := testValidateArtifact(&artifact)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateArtifactRejectsUntypedNonConstExport(t *testing.T) {
	artifact := NewArtifact("example/module", "main")
	artifact.Functions = []Function{{Code: &SlotCode{}, ID: "fn.Main", Signature: testSignature("function() Void")}}
	artifact.Exports = []Export{{Name: "Main", Kind: "function", ID: "fn.Main", Untyped: true}}

	err := testValidateArtifact(&artifact)
	if err == nil || !strings.Contains(err.Error(), "only const exports may be untyped") {
		t.Fatalf("expected untyped non-const export error, got %v", err)
	}
}
