package compiler

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileSourceEmitsExactUntypedNumericMetadata(t *testing.T) {
	result, err := compileTestSource("example/constants", "constants.mgo", `package constants
const Huge = 0xffffffffffffffffffffffffffffffff
const Third = 1.0 / 3.0
const Value = complex(Third, -Third)
`)
	if err != nil {
		t.Fatalf("compileTestSource failed: %v", err)
	}
	if !result.OK() {
		t.Fatalf("compileTestSource diagnostics: %#v", result.Diagnostics)
	}
	constants := map[string]string{}
	for _, constant := range result.Artifact.Constants {
		constants[constant.ID] = string(constant.Value)
		if !constant.Untyped {
			t.Fatalf("constant %s lost untyped metadata", constant.ID)
		}
	}
	for _, export := range result.Artifact.Exports {
		if export.Kind != "const" {
			continue
		}
		value := constants[export.ID]
		switch export.Name {
		case "Huge":
			if value != `"340282366920938463463374607431768211455"` {
				t.Fatalf("Huge wire = %s", value)
			}
		case "Third":
			if value != `"1/3"` {
				t.Fatalf("Third wire = %s", value)
			}
		case "Value":
			if value != `{"real":"1/3","imag":"-1/3"}` {
				t.Fatalf("Value wire = %s", value)
			}
		}
	}
}

func TestCompileSourceKeepsExactMetadataOutOfRuntimeInstructions(t *testing.T) {
	result, err := compileTestSource("example/constants", "constants.mgo", `package constants
const Third = 1.0 / 3.0

func Value() float64 {
	return Third
}
`)
	if err != nil {
		t.Fatalf("compileTestSource failed: %v", err)
	}
	if !result.OK() {
		t.Fatalf("compileTestSource diagnostics: %#v", result.Diagnostics)
	}
	untyped := map[string]bool{}
	for _, constant := range result.Artifact.Constants {
		untyped[constant.ID] = constant.Untyped
	}
	for _, function := range result.Artifact.Functions {
		for _, instruction := range functionOperations(t, function) {
			if instruction.Op != ir.OpConst {
				continue
			}
			var payload ir.ConstPayload
			if err := ir.ReadInstructionPayload(instruction.Payload, &payload); err != nil {
				t.Fatalf("decode const payload: %v", err)
			}
			if untyped[payload.Constant] {
				t.Fatalf("function %s references untyped constant %s", function.ID, payload.Constant)
			}
		}
	}
}

func TestCompileSourceDiagnosesExactNumericTargetOverflow(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "float64 overflow", source: "package main\nconst Bad float64 = 1e1000\n"},
		{name: "default integer overflow", source: "package main\nvar Bad = 0xffffffffffffffffffffffffffffffff\n"},
		{name: "fraction to integer", source: "package main\nconst Bad int64 = 7.0/2.0\n"},
		{name: "typed integer operation overflow", source: "package main\nconst Base int8 = 100\nconst Bad = Base + 100\n"},
		{name: "typed float operation overflow", source: "package main\nconst Base float32 = 3e38\nconst Bad = Base * 2\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := compileTestSource("example/main", "main.mgo", test.source)
			if err != nil {
				t.Fatalf("compileTestSource failed: %v", err)
			}
			requireCompileDiagnostic(t, result.Diagnostics, "semantic.const.representable")
		})
	}
}
