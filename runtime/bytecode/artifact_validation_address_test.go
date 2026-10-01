package bytecode

import (
	"errors"
	"strings"
	"testing"
)

func TestAddressAndCapturePathsReportInvalidSegments(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, test := range []struct {
			segment      AddressPathSegment
			code, suffix string
		}{
			{AddressPathSegment{Kind: "field"}, ValidationFieldMissing, ".field"},
			{AddressPathSegment{Kind: "index"}, ValidationFieldMissing, ".local"},
			{AddressPathSegment{Kind: "index", Local: "missing"}, ValidationReferenceUnknown, ".local"},
			{AddressPathSegment{Kind: "invalid"}, ValidationValueUnsupported, ".kind"},
		} {
			artifact := NewArtifact("app", "main")
			artifact.Globals = []Global{{ID: "g", Type: testType("Int64")}}
			payload := AddressPayload{Kind: "global", Global: "g", Path: []AddressPathSegment{test.segment}}
			instruction := Instruction{Op: OpAddressOf, Payload: payload}
			resultType := "Ptr<Int64>"
			if capture {
				artifact.Functions = []Function{{ID: "child", Signature: testSignature("function() Void"), Code: &SlotCode{}, Upvalues: []Upvalue{{ID: "g", Type: testType("Int64")}}}}
				instruction = Instruction{Op: OpMakeClosure, Payload: ClosurePayload{Function: "child", Captures: []AddressPayload{payload}}}
				resultType = "function() Void"
			}
			artifact.Functions = append(artifact.Functions, Function{ID: "main", Signature: testSignature("function() Void"), Code: testSlotCode([]string{resultType}, []Instruction{instruction, {Op: OpPop}}, [][2][]uint32{{nil, {0}}, {{0}, nil}})})
			err := testValidateArtifact(&artifact)
			var diagnostic ValidationError
			if !errors.As(err, &diagnostic) || diagnostic.Code != test.code || !strings.HasSuffix(diagnostic.Path, ".path[0]"+test.suffix) {
				t.Fatalf("capture=%t segment=%+v: %v", capture, test.segment, err)
			}
		}
	}
}

func TestExportAddressValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload AddressPayload
		want    string
	}{
		{"valid", AddressPayload{Kind: "export", ModulePath: "lib", Export: "Value"}, ""},
		{"missing_module", AddressPayload{Kind: "export", Export: "Value"}, "requires module path"},
		{"missing_export", AddressPayload{Kind: "export", ModulePath: "lib"}, "requires module path"},
		{"mixed_target", AddressPayload{Kind: "export", ModulePath: "lib", Export: "Value", Local: "x"}, "cannot contain local"},
		{"local_with_export", AddressPayload{Kind: "local", Local: "x", Export: "Value"}, "cannot contain export identity"},
		{"unknown_module", AddressPayload{Kind: "export", ModulePath: "other", Export: "Value"}, "unknown module requirement"},
		{"unknown_export", AddressPayload{Kind: "export", ModulePath: "lib", Export: "Other"}, "unknown module export"},
		{"unknown_index", AddressPayload{Kind: "export", ModulePath: "lib", Export: "Value", Path: []AddressPathSegment{{Kind: "index", Local: "missing"}}}, "unknown local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := NewArtifact("app", "main")
			artifact.Requirements = []Requirement{{Kind: "source", ModulePath: "lib", Exports: []string{"Value"}}}
			artifact.Functions = []Function{{ID: "fn.main", Signature: testSignature("function() Ptr<Int64>"), Code: testSlotCode([]string{"Ptr<Int64>"}, []Instruction{
				{Op: OpAddressOf, Payload: test.payload},
				{Op: OpReturn, Payload: ReturnPayload{ResultCount: 1}},
			}, [][2][]uint32{{nil, {0}}, {{0}, nil}})}}
			err := testValidateArtifact(&artifact)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want %q", err, test.want)
			}
		})
	}
}

func TestClosureCaptureAddressValidation(t *testing.T) {
	artifact := NewArtifact("app", "main")
	artifact.Functions = []Function{
		{Code: &SlotCode{}, ID: "fn.child", Signature: testSignature("function() Void"), Upvalues: []Upvalue{{ID: "x", Type: testType("Int64")}}},
		{ID: "fn.main", Signature: testSignature("function() Void"), Code: testSlotCode([]string{"function() Void"}, []Instruction{
			{Op: OpMakeClosure, Payload: ClosurePayload{Function: "fn.child", Captures: []AddressPayload{{Kind: "global", Global: "value", ModulePath: "lib", Export: "Value"}}}},
			{Op: OpPop},
		}, [][2][]uint32{{nil, {0}}, {{0}, nil}})},
	}
	if err := testValidateArtifact(&artifact); err == nil || !strings.Contains(err.Error(), "capture cannot contain export identity") {
		t.Fatalf("capture validation: %v", err)
	}
}
