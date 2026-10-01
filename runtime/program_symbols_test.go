package runtime

import (
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/target"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestProgramWithSymbolsValidatesAndOwnsSidecar(t *testing.T) {
	artifact := ir.NewArtifact("test/symbols", "symbols")
	artifact.Functions = []ir.Function{{
		ID: "fn.main", Signature: testSignature("function() Void"),
		Code: testSlotCode([]string{}, []ir.Instruction{{Op: ir.OpReturn, Payload: ir.ReturnPayload{}}}, [][2][]uint32{{nil, nil}}),
	}}
	attachRuntimeTestTypeNodes(&artifact)
	artifactJSON, artifactHash, err := ir.EncodeJSONAndHash(&artifact)
	if err != nil {
		t.Fatal(err)
	}
	buildTarget, err := target.Normalize(target.Target{})
	if err != nil {
		t.Fatal(err)
	}
	image := ir.ExecutionImage{
		Format: ir.ExecutionFormat, Version: ir.ExecutionVersion, CompilerID: ir.CompilerIdentity, ContractID: ir.ExecutionContract,
		Target: buildTarget, Root: artifact.Module.Path,
		Entries:  []ir.Entry{{Name: ir.DefaultEntryName, ModulePath: artifact.Module.Path, FunctionID: "fn.main"}},
		Packages: map[string]ir.PackageArchive{artifact.Module.Path: {Artifact: artifactJSON, ArtifactHash: artifactHash}},
	}
	image.Hash, err = ir.HashExecutionImage(image)
	if err != nil {
		t.Fatal(err)
	}
	program, err := LoadExecutionImage(image)
	if err != nil {
		t.Fatal(err)
	}
	symbols := ir.ProgramSymbols{
		Format: ir.SymbolsFormat, Version: ir.SymbolsVersion, CompilerID: ir.CompilerIdentity, ContractID: ir.SymbolsContract,
		ProgramHash: image.Hash,
		Packages: map[string]ir.PackageSymbols{artifact.Module.Path: {
			ModulePath: artifact.Module.Path, CodeHash: artifactHash,
			Functions: []ir.FunctionSymbols{{ID: "fn.main", Name: "main"}},
		}},
	}
	symbols.Hash, err = ir.HashProgramSymbols(symbols)
	if err != nil {
		t.Fatal(err)
	}
	symbolized, err := program.WithSymbols(symbols)
	if err != nil {
		t.Fatal(err)
	}
	if symbolized.code != program.code || symbolized.Hash() != program.Hash() || symbolized.SymbolsHash() != symbols.Hash || program.SymbolsHash() != "" {
		t.Fatalf("attached program identity: code=%p/%p hash=%s symbols=%s", symbolized.code, program.code, symbolized.Hash(), symbolized.SymbolsHash())
	}
	pkg := symbols.Packages[artifact.Module.Path]
	pkg.Functions[0].Name = "changed"
	symbols.Packages[artifact.Module.Path] = pkg
	if function, _ := symbolized.symbols.function(artifact.Module.Path, "fn.main"); function.Name != "main" {
		t.Fatal("Program.WithSymbols retained caller-owned data")
	}
	invalid := ir.CloneProgramSymbols(symbols)
	invalid.ProgramHash = strings.Repeat("0", 64)
	invalid.Hash, _ = ir.HashProgramSymbols(invalid)
	if _, err := program.WithSymbols(invalid); err == nil {
		t.Fatal("Program.WithSymbols accepted a sidecar for another program")
	}
}
