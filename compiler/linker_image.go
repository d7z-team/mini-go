package compiler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/target"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type linkRequest struct {
	// validatedBodies is set only by Prepare immediately after owned compilation.
	validatedBodies  bool
	Context          context.Context
	CompilerID       string
	ContractID       string
	Target           target.Target
	Root             string
	Entries          []entrySelection
	Artifacts        map[string]ir.Artifact
	Symbols          map[string]ir.PackageSymbols
	Order            []string
	HostCapabilities map[string][]string
	Optimization     OptimizationLevel
	IncludeSymbols   bool
}

// Prepare computes the executable function closure and seals an ExecutionImage.
func linkExecutionImage(request linkRequest) (ir.ExecutionImage, error) {
	image, _, err := linkProgram(request)
	return image, err
}

func linkProgram(request linkRequest) (ir.ExecutionImage, *ir.ProgramSymbols, error) {
	request.Context = requestContext(request.Context)
	if err := request.Context.Err(); err != nil {
		return ir.ExecutionImage{}, nil, err
	}
	normalizedTarget, err := target.Normalize(request.Target)
	if err != nil {
		return ir.ExecutionImage{}, nil, err
	}
	root := strings.TrimSpace(request.Root)
	if root == "" {
		return ir.ExecutionImage{}, nil, errors.New("linker requires root module")
	}
	if _, ok := request.Artifacts[root]; !ok {
		return ir.ExecutionImage{}, nil, fmt.Errorf("linker root module %q is missing", root)
	}
	selections := append([]entrySelection(nil), request.Entries...)
	sort.Slice(selections, func(i, j int) bool {
		if selections[i].Name != selections[j].Name {
			return selections[i].Name < selections[j].Name
		}
		if selections[i].ModulePath != selections[j].ModulePath {
			return selections[i].ModulePath < selections[j].ModulePath
		}
		return selections[i].Function < selections[j].Function
	})
	entries := make([]ir.Entry, 0, len(selections))
	for i, entry := range selections {
		entry.Name = strings.TrimSpace(entry.Name)
		entry.ModulePath = strings.TrimSpace(entry.ModulePath)
		entry.Function = strings.TrimSpace(entry.Function)
		if entry.Name == "" || entry.ModulePath == "" || entry.Function == "" {
			return ir.ExecutionImage{}, nil, fmt.Errorf("entry %d is incomplete", i)
		}
		if entry.ModulePath != root {
			return ir.ExecutionImage{}, nil, fmt.Errorf("entry %q must belong to root module %q", entry.Name, root)
		}
		functionID, ok := resolveSymbolFunction(request.Symbols[entry.ModulePath], entry.Function)
		if !ok {
			return ir.ExecutionImage{}, nil, fmt.Errorf("entry %q references unknown function %s.%s", entry.Name, entry.ModulePath, entry.Function)
		}
		if i != 0 && selections[i-1].Name == entry.Name {
			return ir.ExecutionImage{}, nil, fmt.Errorf("duplicate entry name %q", entry.Name)
		}
		entries = append(entries, ir.Entry{Name: entry.Name, ModulePath: entry.ModulePath, FunctionID: functionID})
	}

	modules, err := retainReachableModules(request.Context, request.Artifacts, root)
	if err != nil {
		return ir.ExecutionImage{}, nil, err
	}
	if err := validateLinkedReferences(request.Context, modules, !request.validatedBodies); err != nil {
		return ir.ExecutionImage{}, nil, err
	}
	artifacts, err := retainReachableCode(request.Context, modules, entries)
	if err != nil {
		return ir.ExecutionImage{}, nil, err
	}
	if err := validateLinkedReferences(request.Context, artifacts, false); err != nil {
		return ir.ExecutionImage{}, nil, err
	}
	order := dependencyOrder(request.Order, artifacts)
	capabilitySet := make(map[string]struct{})
	for _, modulePath := range order {
		if err := request.Context.Err(); err != nil {
			return ir.ExecutionImage{}, nil, err
		}
		for _, capability := range request.HostCapabilities[modulePath] {
			capability = strings.TrimSpace(capability)
			if capability != "" {
				capabilitySet[capability] = struct{}{}
			}
		}
	}
	capabilities := make([]string, 0, len(capabilitySet))
	for capability := range capabilitySet {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	packages := make(map[string]ir.PackageArchive, len(order))
	hashes := make(map[string]string, len(order))
	for _, modulePath := range order {
		artifact := artifacts[modulePath]
		for i := range artifact.Requirements {
			requirement := &artifact.Requirements[i]
			if requirement.Kind != ir.RequirementSource {
				continue
			}
			hash, ok := hashes[requirement.ModulePath]
			if !ok {
				return ir.ExecutionImage{}, nil, fmt.Errorf("package %q requires unavailable module %q", modulePath, requirement.ModulePath)
			}
			requirement.Hash = hash
		}
		archive, err := ir.NewPackageArchive(&artifact)
		if err != nil {
			return ir.ExecutionImage{}, nil, err
		}
		packages[modulePath] = archive
		hashes[modulePath] = archive.ArtifactHash
	}
	image := ir.ExecutionImage{
		Format: ir.ExecutionFormat, Version: ir.ExecutionVersion,
		CompilerID: strings.TrimSpace(request.CompilerID), ContractID: strings.TrimSpace(request.ContractID),
		Target:  normalizedTarget,
		Root:    root,
		Entries: entries, Capabilities: capabilities, Packages: packages,
	}
	if image.CompilerID == "" || image.ContractID == "" {
		return ir.ExecutionImage{}, nil, errors.New("linker requires compiler and contract identities")
	}
	image.Hash, err = ir.HashExecutionImage(image)
	if err != nil {
		return ir.ExecutionImage{}, nil, err
	}
	if !request.IncludeSymbols {
		return image, nil, nil
	}
	symbols, err := linkProgramSymbols(request.Context, image, artifacts, request.Symbols, request.Optimization)
	return image, symbols, err
}

func linkProgramSymbols(ctx context.Context, image ir.ExecutionImage, artifacts map[string]ir.Artifact, source map[string]ir.PackageSymbols, optimization OptimizationLevel) (*ir.ProgramSymbols, error) {
	symbols := ir.ProgramSymbols{
		Format: ir.SymbolsFormat, Version: ir.SymbolsVersion, CompilerID: image.CompilerID,
		ContractID: ir.SymbolsContract, ProgramHash: image.Hash, Optimization: uint8(optimization),
		Packages: make(map[string]ir.PackageSymbols, len(image.Packages)),
	}
	for modulePath, archive := range image.Packages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		artifact, ok := artifacts[modulePath]
		if !ok {
			var err error
			artifact, err = ir.DecodeJSON(archive.Artifact)
			if err != nil {
				return nil, fmt.Errorf("decode linked package %s: %w", modulePath, err)
			}
		}
		pkg := retainPackageSymbols(source[modulePath], artifact)
		pkg.CodeHash = archive.ArtifactHash
		symbols.Packages[modulePath] = pkg
	}
	var err error
	symbols.Hash, err = ir.HashProgramSymbols(symbols)
	if err != nil {
		return nil, err
	}
	if err := ir.ValidateProgramSymbols(&image, &symbols); err != nil {
		return nil, err
	}
	return &symbols, nil
}

func resolveSymbolFunction(symbols ir.PackageSymbols, name string) (string, bool) {
	for _, function := range symbols.Functions {
		if function.Name == name {
			return function.ID, true
		}
	}
	return "", false
}

func retainPackageSymbols(symbols ir.PackageSymbols, artifact ir.Artifact) ir.PackageSymbols {
	functions := make(map[string]struct{}, len(artifact.Functions))
	for _, function := range artifact.Functions {
		functions[function.ID] = struct{}{}
	}
	globals := make(map[string]struct{}, len(artifact.Globals))
	for _, global := range artifact.Globals {
		globals[global.ID] = struct{}{}
	}
	out := symbols
	out.Functions = make([]ir.FunctionSymbols, 0, len(artifact.Functions))
	for _, function := range symbols.Functions {
		if _, ok := functions[function.ID]; ok {
			out.Functions = append(out.Functions, function)
		}
	}
	out.Globals = make([]ir.GlobalSymbol, 0, len(artifact.Globals))
	for _, global := range symbols.Globals {
		if _, ok := globals[global.ID]; ok {
			out.Globals = append(out.Globals, global)
		}
	}
	return out
}

func retainReachableModules(ctx context.Context, source map[string]ir.Artifact, root string) (map[string]ir.Artifact, error) {
	retained := make(map[string]ir.Artifact)
	queue := []string{root}
	for next := 0; next < len(queue); next++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		modulePath := queue[next]
		if _, ok := retained[modulePath]; ok {
			continue
		}
		artifact, ok := source[modulePath]
		if !ok {
			return nil, fmt.Errorf("linked package %q is missing", modulePath)
		}
		retained[modulePath] = artifact
		for _, requirement := range artifact.Requirements {
			if requirement.Kind == ir.RequirementSource {
				queue = append(queue, requirement.ModulePath)
			}
		}
	}
	return retained, nil
}
