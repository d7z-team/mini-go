package bytecode

import (
	"encoding/json"

	"github.com/d7z-team/mini-go/compiler/types"
)

// CloneArtifact returns an owned artifact suitable for transient caching.
func CloneArtifact(artifact Artifact) Artifact {
	out := artifact
	out.TypeTable = types.CloneTable(artifact.TypeTable)
	out.Constants = make([]Constant, len(artifact.Constants))
	for index, constant := range artifact.Constants {
		out.Constants[index] = constant
		out.Constants[index].Value = append(json.RawMessage(nil), constant.Value...)
	}
	out.Globals = append([]Global(nil), artifact.Globals...)
	out.Functions = make([]Function, len(artifact.Functions))
	for index, function := range artifact.Functions {
		out.Functions[index] = function
		out.Functions[index].Signature.Params = append([]types.TypeParam(nil), function.Signature.Params...)
		out.Functions[index].Signature.Results = append([]types.TypeRef(nil), function.Signature.Results...)
		out.Functions[index].Locals = append([]Local(nil), function.Locals...)
		out.Functions[index].ResultLocals = append([]string(nil), function.ResultLocals...)
		out.Functions[index].Upvalues = append([]Upvalue(nil), function.Upvalues...)
		if function.Code != nil {
			code := *function.Code
			code.Types = append([]types.TypeRef(nil), code.Types...)
			code.Instructions = append([]SlotInstruction(nil), code.Instructions...)
			code.Descriptors = code.Descriptors.Clone()
			code.Operands = make([]SlotOperands, len(function.Code.Operands))
			for i, operands := range function.Code.Operands {
				code.Operands[i] = SlotOperands{Inputs: append([]Operand(nil), operands.Inputs...), Outputs: append([]uint32(nil), operands.Outputs...), Release: append([]uint32(nil), operands.Release...), ReleaseBefore: append([]uint32(nil), operands.ReleaseBefore...)}
			}
			out.Functions[index].Code = &code
		}
	}
	out.Exports = append([]Export(nil), artifact.Exports...)
	out.Requirements = make([]Requirement, len(artifact.Requirements))
	for index, requirement := range artifact.Requirements {
		out.Requirements[index] = requirement
		out.Requirements[index].Exports = append([]string(nil), requirement.Exports...)
	}
	return out
}

func ClonePackageSymbols(symbols PackageSymbols) PackageSymbols {
	out := symbols
	out.Files = append([]SourceFile(nil), symbols.Files...)
	out.Globals = append([]GlobalSymbol(nil), symbols.Globals...)
	out.Functions = make([]FunctionSymbols, len(symbols.Functions))
	for i, function := range symbols.Functions {
		out.Functions[i] = function
		if function.Declaration != nil {
			declaration := *function.Declaration
			out.Functions[i].Declaration = &declaration
		}
		out.Functions[i].Locals = append([]LocalSymbol(nil), function.Locals...)
		for j, local := range function.Locals {
			if local.Declaration != nil {
				declaration := *local.Declaration
				out.Functions[i].Locals[j].Declaration = &declaration
			}
		}
		out.Functions[i].Upvalues = append([]UpvalueSymbol(nil), function.Upvalues...)
		out.Functions[i].Scopes = make([]DebugScope, len(function.Scopes))
		for j, scope := range function.Scopes {
			out.Functions[i].Scopes[j] = scope
			out.Functions[i].Scopes[j].Ranges = append([]PCRange(nil), scope.Ranges...)
		}
		out.Functions[i].Locations = make([]InstructionSymbol, len(function.Locations))
		for j, location := range function.Locations {
			out.Functions[i].Locations[j] = location
			out.Functions[i].Locations[j].Points = append([]Location(nil), location.Points...)
		}
	}
	return out
}

func CloneProgramSymbols(symbols ProgramSymbols) ProgramSymbols {
	out := symbols
	out.Packages = make(map[string]PackageSymbols, len(symbols.Packages))
	for modulePath, pkg := range symbols.Packages {
		out.Packages[modulePath] = ClonePackageSymbols(pkg)
	}
	return out
}

// CloneExecutionImage returns an owned image without sharing archive bytes.
func CloneExecutionImage(image ExecutionImage) ExecutionImage {
	out := image
	out.Entries = append([]Entry(nil), image.Entries...)
	out.Capabilities = append([]string(nil), image.Capabilities...)
	out.Packages = make(map[string]PackageArchive, len(image.Packages))
	for modulePath, archive := range image.Packages {
		archive.Artifact = append(json.RawMessage(nil), archive.Artifact...)
		out.Packages[modulePath] = archive
	}
	return out
}
