package compiler

import (
	"context"
	"fmt"
	"sort"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// validateLinkedReferences complements package-local bytecode validation with
// references whose targets belong to another package. It runs before pruning
// and again on the retained closure.
func validateLinkedReferences(ctx context.Context, artifacts map[string]ir.Artifact, validateBodies bool) error {
	exports := make(map[string]map[string]ir.Export, len(artifacts))
	functions := make(map[functionRef]struct{})
	paths := make([]string, 0, len(artifacts))
	for path, artifact := range artifacts {
		paths = append(paths, path)
		exports[path] = make(map[string]ir.Export, len(artifact.Exports))
		for _, export := range artifact.Exports {
			exports[path][export.Name] = export
		}
		for _, function := range artifact.Functions {
			functions[functionRef{path, function.ID}] = struct{}{}
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		artifact := artifacts[path]
		if validateBodies {
			if err := ir.ValidateArtifact(&artifact); err != nil {
				return fmt.Errorf("validate package %q: %w", path, err)
			}
		}
		for _, requirement := range artifact.Requirements {
			if requirement.Kind != ir.RequirementSource {
				continue
			}
			dependency, ok := artifacts[requirement.ModulePath]
			if !ok {
				return fmt.Errorf("package %q requires unavailable module %q", path, requirement.ModulePath)
			}
			for _, name := range requirement.Exports {
				if _, ok := exports[requirement.ModulePath][name]; ok {
					continue
				}
				if _, ok := dependency.TypeTable.Named(types.TypeKey{ModulePath: requirement.ModulePath, DeclID: types.DeclID(name)}); ok {
					continue
				}
				return fmt.Errorf("package %q requires missing export %q from %q", path, name, requirement.ModulePath)
			}
		}
		for _, function := range artifact.Functions {
			operations, err := function.Operations()
			if err != nil {
				return err
			}
			for pc, instruction := range operations {
				if err := ctx.Err(); err != nil {
					return err
				}
				var ref functionRef
				switch instruction.Op {
				case ir.OpCallDirect, ir.OpTailCallDirect:
					var call ir.CallPayload
					if err := ir.ReadInstructionPayload(instruction.Payload, &call); err != nil {
						return err
					}
					ref = functionRef{call.ModulePath, call.Function}
				case ir.OpMakeClosure:
					var closure ir.ClosurePayload
					if err := ir.ReadInstructionPayload(instruction.Payload, &closure); err != nil {
						return err
					}
					ref = functionRef{closure.ModulePath, closure.Function}
				case ir.OpLoadExport:
					var member ir.ExportPayload
					if err := ir.ReadInstructionPayload(instruction.Payload, &member); err != nil {
						return err
					}
					if _, ok := exports[member.ModulePath][member.Export]; !ok {
						return fmt.Errorf("%s.%s instruction %d references missing export %s.%s", path, function.ID, pc, member.ModulePath, member.Export)
					}
				case ir.OpAddressOf:
					var address ir.AddressPayload
					if err := ir.ReadInstructionPayload(instruction.Payload, &address); err != nil {
						return err
					}
					if address.ModulePath != "" {
						if export, ok := exports[address.ModulePath][address.Export]; !ok || export.Kind != "global" {
							return fmt.Errorf("%s.%s instruction %d references invalid global export %s.%s", path, function.ID, pc, address.ModulePath, address.Export)
						}
					}
				}
				if ref.function != "" {
					if ref.modulePath == "" {
						ref.modulePath = path
					}
					if _, ok := functions[ref]; !ok {
						return fmt.Errorf("%s.%s instruction %d references missing function %s.%s", path, function.ID, pc, ref.modulePath, ref.function)
					}
				}
			}
		}
	}
	return nil
}
