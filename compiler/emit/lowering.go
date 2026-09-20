// Package emit converts typed HIR into validated bytecode artifacts and symbols.
package emit

import (
	"sort"

	"github.com/d7z-team/mini-go/compiler/hir"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func Lower(program hir.Program) (ir.Artifact, error) {
	artifact, _, err := LowerUnvalidatedWithSymbols(program)
	if err != nil {
		return ir.Artifact{}, err
	}
	return artifact, ir.ValidateArtifact(&artifact)
}

// LowerUnvalidatedWithSymbols emits executable code and its optional source
// sidecar before catalog requirements are bound to final package hashes.
func LowerUnvalidatedWithSymbols(program hir.Program) (ir.Artifact, ir.PackageSymbols, error) {
	artifact := ir.NewArtifact(program.ModulePath, program.Package)
	symbols := ir.PackageSymbols{ModulePath: program.ModulePath, SourceHash: program.DebugSourceHash}
	artifact.TypeTable.Nodes = append(artifact.TypeTable.Nodes, program.TypeTable.Nodes...)
	if err := artifact.TypeTable.Reindex(); err != nil {
		return ir.Artifact{}, ir.PackageSymbols{}, err
	}
	for _, file := range program.DebugFiles {
		symbols.Files = append(symbols.Files, ir.SourceFile{ID: file.ID, Path: file.Path, Hash: file.Hash})
	}
	for _, constant := range program.Constants {
		artifact.Constants = append(artifact.Constants, ir.Constant{
			ID:      constant.ID,
			Type:    constant.Type,
			Value:   constant.Value,
			Untyped: constant.Untyped,
		})
	}
	for _, global := range program.Globals {
		artifact.Globals = append(artifact.Globals, ir.Global{
			ID: global.ID, Type: global.Type,
		})
		symbols.Globals = append(symbols.Globals, ir.GlobalSymbol{ID: global.ID, Name: global.Name})
	}
	for _, fn := range program.Functions {
		out := ir.Function{
			ID:            fn.ID,
			RevisionLocal: fn.RevisionLocal,
			Signature:     fn.Signature,
			Locals:        lowerLocals(fn.Locals),
			ResultLocals:  append([]string(nil), fn.ResultLocals...),
			Upvalues:      lowerUpvalues(fn.Upvalues),
		}
		functionSymbols := ir.FunctionSymbols{
			ID: fn.ID, Name: fn.Name, Generated: fn.Generated, Declaration: lowerLocationPtr(fn.Declaration),
			Locals: lowerLocalSymbols(fn.Locals), Upvalues: lowerUpvalueSymbols(fn.Upvalues),
		}
		for _, scope := range fn.DebugScopes {
			functionSymbols.Scopes = append(functionSymbols.Scopes, ir.DebugScope{ID: scope.ID, Parent: scope.Parent})
		}
		for _, stmt := range closeExitedMapIterators(fn.Body) {
			if err := lowerStatement(&artifact, &out, &functionSymbols, stmt); err != nil {
				return ir.Artifact{}, ir.PackageSymbols{}, err
			}
		}
		for _, local := range out.Locals[len(functionSymbols.Locals):] {
			functionSymbols.Locals = append(functionSymbols.Locals, ir.LocalSymbol{ID: local.ID, Generated: true})
		}
		normalizeFunctionSymbolPCs(out, &functionSymbols)
		artifact.Functions = append(artifact.Functions, out)
		symbols.Functions = append(symbols.Functions, functionSymbols)
	}
	for _, export := range program.Exports {
		artifact.Exports = append(artifact.Exports, ir.Export{
			Name:    export.Name,
			Kind:    export.Kind,
			ID:      export.ID,
			Type:    export.Type,
			Untyped: export.Untyped,
		})
	}
	for _, requirement := range program.Requirements {
		artifact.Requirements = append(artifact.Requirements, ir.Requirement{
			Kind:       ir.RequirementKind(requirement.Kind),
			ModulePath: requirement.ModulePath,
			Hash:       requirement.Hash,
			Exports:    append([]string(nil), requirement.Exports...),
		})
	}
	return artifact, symbols, nil
}

func lowerLocals(locals []hir.Local) []ir.Local {
	out := make([]ir.Local, 0, len(locals))
	for _, local := range locals {
		out = append(out, ir.Local{ID: local.ID, Type: local.Type})
	}
	return out
}

func lowerLocationPtr(location *hir.Location) *ir.Location {
	if location == nil {
		return nil
	}
	lowered := lowerLocation(*location)
	return &lowered
}

func lowerUpvalues(upvalues []hir.Upvalue) []ir.Upvalue {
	out := make([]ir.Upvalue, 0, len(upvalues))
	for _, upvalue := range upvalues {
		out = append(out, ir.Upvalue{ID: upvalue.ID, Type: upvalue.Type})
	}
	return out
}

func lowerLocalSymbols(locals []hir.Local) []ir.LocalSymbol {
	out := make([]ir.LocalSymbol, 0, len(locals))
	for _, local := range locals {
		out = append(out, ir.LocalSymbol{ID: local.ID, Name: local.Name, Scope: local.Scope, Generated: local.Generated, Declaration: lowerLocationPtr(local.Declaration)})
	}
	return out
}

func lowerUpvalueSymbols(upvalues []hir.Upvalue) []ir.UpvalueSymbol {
	out := make([]ir.UpvalueSymbol, 0, len(upvalues))
	for _, upvalue := range upvalues {
		out = append(out, ir.UpvalueSymbol{ID: upvalue.ID, Name: upvalue.Name})
	}
	return out
}

func normalizeFunctionSymbolPCs(function ir.Function, symbols *ir.FunctionSymbols) {
	if symbols == nil {
		return
	}
	pcs := make([]int, len(function.Instructions)+1)
	pc := 0
	for index, instruction := range function.Instructions {
		pcs[index] = pc
		if instruction.Op != string(ir.OpLabel) {
			pc++
		}
	}
	pcs[len(function.Instructions)] = pc
	locationsByPC := make(map[int][]ir.Location, len(symbols.Locations))
	for _, location := range symbols.Locations {
		finalPC := pcs[location.PC]
		seen := make(map[ir.Location]struct{}, len(locationsByPC[finalPC])+len(location.Points))
		for _, point := range locationsByPC[finalPC] {
			seen[point] = struct{}{}
		}
		for _, point := range location.Points {
			if _, duplicate := seen[point]; duplicate {
				continue
			}
			locationsByPC[finalPC] = append(locationsByPC[finalPC], point)
			seen[point] = struct{}{}
		}
	}
	symbols.Locations = symbols.Locations[:0]
	for finalPC, points := range locationsByPC {
		if finalPC < pc && len(points) != 0 {
			symbols.Locations = append(symbols.Locations, ir.InstructionSymbol{PC: finalPC, Points: points})
		}
	}
	sort.Slice(symbols.Locations, func(i, j int) bool { return symbols.Locations[i].PC < symbols.Locations[j].PC })
	for scopeIndex := range symbols.Scopes {
		ranges := symbols.Scopes[scopeIndex].Ranges
		normalized := ranges[:0]
		for _, pcRange := range ranges {
			pcRange.Start, pcRange.End = pcs[pcRange.Start], pcs[pcRange.End]
			if pcRange.Start >= pcRange.End {
				continue
			}
			if len(normalized) != 0 && pcRange.Start <= normalized[len(normalized)-1].End {
				if pcRange.End > normalized[len(normalized)-1].End {
					normalized[len(normalized)-1].End = pcRange.End
				}
				continue
			}
			normalized = append(normalized, pcRange)
		}
		symbols.Scopes[scopeIndex].Ranges = normalized
	}
}
