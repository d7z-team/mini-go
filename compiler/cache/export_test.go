package cache

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestArtifactProjectionJSONRoundTrip(t *testing.T) {
	artifact := ir.NewArtifact("example/lib", "lib")
	data, err := FromArtifact(artifact)
	if err != nil {
		t.Fatalf("FromArtifact failed: %v", err)
	}
	encoded, err := EncodeJSON(data)
	if err != nil {
		t.Fatalf("EncodeJSON failed: %v", err)
	}
	decoded, err := DecodeJSON(encoded)
	if err != nil {
		t.Fatalf("DecodeJSON failed: %v", err)
	}
	if decoded.ModulePath != artifact.Module.Path || decoded.ArtifactHash != data.ArtifactHash || decoded.ExportHash != data.ExportHash {
		t.Fatalf("round-trip changed export data: %#v", decoded)
	}

	decoded.Package = "changed"
	if err := decoded.Validate(); err == nil {
		t.Fatal("changed export data passed hash validation")
	}
}

func TestExportHashExcludesRuntimeArtifactIdentity(t *testing.T) {
	first := ir.NewArtifact("example/lib", "lib")
	first.Functions = []ir.Function{{ID: "fn.Value", Code: &ir.SlotCode{}}}
	first.Exports = []ir.Export{{Name: "Value", Kind: "function", ID: "fn.Value"}}
	second := first
	second.Functions = append([]ir.Function(nil), first.Functions...)
	code := &ir.SlotCode{Operands: []ir.SlotOperands{{}}}
	descriptor, err := code.Descriptors.Append(ir.ReturnPayload{})
	if err != nil {
		t.Fatal(err)
	}
	code.Instructions = []ir.SlotInstruction{{Op: ir.OpReturn, Descriptor: descriptor}}
	second.Functions[0].Code = code
	firstData, err := FromArtifact(first)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := FromArtifact(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstData.ArtifactHash == secondData.ArtifactHash {
		t.Fatal("private function implementation did not change artifact hash")
	}
	if firstData.ExportHash != secondData.ExportHash {
		t.Fatalf("private function implementation changed export hash: %s != %s", firstData.ExportHash, secondData.ExportHash)
	}
}
