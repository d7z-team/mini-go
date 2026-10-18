// Package runtime loads bytecode programs and executes them in the Mini-Go VM.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/target"
	irtypes "github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type Program struct {
	code    *programCode
	symbols *symbolIndex
}

// ProgramIdentity identifies immutable code and optional symbols, not a deployment.
type ProgramIdentity struct {
	Hash        string
	SymbolsHash string
	CompilerID  string
}

func (p *Program) Identity() ProgramIdentity {
	identity := ProgramIdentity{Hash: p.Hash(), SymbolsHash: p.SymbolsHash()}
	if p != nil && p.code != nil {
		identity.CompilerID = p.code.image.CompilerID
	}
	return identity
}

type programCode struct {
	image         ir.ExecutionImage
	root          *executable
	modules       map[string]*executable
	moduleOrder   []string
	entries       map[string]string
	packageHashes map[string]string
}

func (p *Program) Hash() string {
	if p == nil || p.code == nil {
		return ""
	}
	return p.code.image.Hash
}

// WithSymbols returns a program view with source-level symbols for this exact
// execution image. The executable code is shared and remains immutable.
func (p *Program) WithSymbols(symbols ir.ProgramSymbols) (*Program, error) {
	if p == nil || p.code == nil {
		return nil, errors.New("nil program")
	}
	index, err := newSymbolIndex(p.code, &symbols)
	if err != nil {
		return nil, fmt.Errorf("attach program symbols: %w", err)
	}
	return &Program{code: p.code, symbols: index}, nil
}

// WithoutSymbols returns a program view that shares the same immutable code
// without source-level symbols.
func (p *Program) WithoutSymbols() *Program {
	if p == nil {
		return nil
	}
	return &Program{code: p.code}
}

func (p *Program) SymbolsHash() string {
	if p == nil || p.symbols == nil {
		return ""
	}
	return p.symbols.hash
}

func (p *Program) Target() target.Target {
	if p == nil || p.code == nil {
		return target.Target{}
	}
	return target.Target{Tags: append([]string(nil), p.code.image.Target.Tags...)}
}

// PackagePaths returns the linked package paths in sorted order.
func (p *Program) PackagePaths() []string {
	if p == nil || p.code == nil {
		return nil
	}
	return append([]string(nil), p.code.moduleOrder...)
}

// RequiredHostCapabilities returns the explicitly required host services of the
// linked package closure. Optional standard-library services are assembly hints.
func (p *Program) RequiredHostCapabilities() []string {
	if p == nil || p.code == nil {
		return nil
	}
	return append([]string(nil), p.code.image.Capabilities...)
}

type ProgramExport struct {
	Name string
	Kind string
	Type string
}

func (p *Program) Exports() []ProgramExport {
	if p == nil || p.code == nil || p.code.root == nil {
		return nil
	}
	out := make([]ProgramExport, len(p.code.root.Artifact.Exports))
	for i, export := range p.code.root.Artifact.Exports {
		out[i] = ProgramExport{Name: export.Name, Kind: export.Kind, Type: irtypes.FormatWithTable(&p.code.root.Artifact.TypeTable, export.Type)}
	}
	return out
}

func LoadExecutionImage(image ir.ExecutionImage) (*Program, error) {
	return LoadExecutionImageWithOptions(image, LoadOptions{})
}

func LoadExecutionImageWithOptions(image ir.ExecutionImage, options LoadOptions) (*Program, error) {
	options = normalizeLoadOptions(options)
	if image.Format != ir.ExecutionFormat || image.Version != ir.ExecutionVersion {
		return nil, fmt.Errorf("unsupported execution image %q version %d", image.Format, image.Version)
	}
	if image.ContractID != ir.ExecutionContract || image.CompilerID != ir.CompilerIdentity {
		return nil, errors.New("execution image identity mismatch")
	}
	if err := target.Validate(image.Target); err != nil {
		return nil, fmt.Errorf("invalid execution target: %w", err)
	}
	for index, capability := range image.Capabilities {
		if strings.TrimSpace(capability) == "" || index > 0 && image.Capabilities[index-1] >= capability {
			return nil, errors.New("execution image capabilities are not sorted unique non-empty names")
		}
	}
	rootPath := strings.TrimSpace(image.Root)
	if rootPath == "" {
		return nil, errors.New("execution image requires root module")
	}
	rootArchive, ok := image.Packages[rootPath]
	if !ok {
		return nil, fmt.Errorf("execution image is missing root package %q", rootPath)
	}
	if len(image.Packages)-1 > options.MaxDependencies {
		return nil, fmt.Errorf("program dependency limit exceeded: max %d", options.MaxDependencies)
	}
	totalArtifactBytes := 0
	for _, archive := range image.Packages {
		totalArtifactBytes += len(archive.Artifact)
		if totalArtifactBytes > options.MaxArtifactBytes {
			return nil, fmt.Errorf("program artifact byte limit exceeded: max %d", options.MaxArtifactBytes)
		}
	}
	imageHash, err := ir.HashExecutionImage(image)
	if err != nil {
		return nil, err
	}
	if imageHash != image.Hash {
		return nil, errors.New("execution image hash mismatch")
	}
	root, err := newLoader().loadJSON(rootArchive.Artifact)
	if err != nil {
		return nil, fmt.Errorf("load root package %s: %w", rootPath, err)
	}
	if root.Hash != rootArchive.ArtifactHash || root.Artifact.Module.Path != rootPath {
		return nil, errors.New("root package identity mismatch")
	}
	if err := validateArtifactLoad(&root.Artifact, options); err != nil {
		return nil, fmt.Errorf("load root package %s: %w", rootPath, err)
	}
	if len(image.Entries) == 0 {
		return nil, errors.New("execution image requires at least one entry")
	}
	entries := make(map[string]string, len(image.Entries))
	for _, entry := range image.Entries {
		if entry.ModulePath != rootPath || strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.FunctionID) == "" {
			return nil, errors.New("execution image contains invalid entry")
		}
		if _, exists := entries[entry.Name]; exists {
			return nil, fmt.Errorf("execution image contains duplicate entry %q", entry.Name)
		}
		if _, ok := root.Functions[entry.FunctionID]; !ok {
			return nil, fmt.Errorf("execution image entry %q references unknown function id %q", entry.Name, entry.FunctionID)
		}
		entries[entry.Name] = entry.FunctionID
	}
	modules := make(map[string]*executable, len(image.Packages))
	modules[rootPath] = root
	for modulePath, archive := range image.Packages {
		if modulePath == rootPath {
			continue
		}
		dependency, err := newLoader().loadJSON(archive.Artifact)
		if err != nil {
			return nil, fmt.Errorf("load package %s: %w", modulePath, err)
		}
		if dependency.Hash != archive.ArtifactHash || dependency.Artifact.Module.Path != modulePath {
			return nil, fmt.Errorf("package %s failed identity validation", modulePath)
		}
		if err := validateArtifactLoad(&dependency.Artifact, options); err != nil {
			return nil, fmt.Errorf("load package %s: %w", modulePath, err)
		}
		modules[modulePath] = dependency
	}
	moduleOrder, err := prepareModuleReferences(modules)
	if err != nil {
		return nil, err
	}
	ownedImage := image
	ownedImage.Target.Tags = append([]string(nil), image.Target.Tags...)
	ownedImage.Entries = append([]ir.Entry(nil), image.Entries...)
	ownedImage.Capabilities = append([]string(nil), image.Capabilities...)
	ownedImage.Packages = make(map[string]ir.PackageArchive, len(image.Packages))
	packageHashes := make(map[string]string, len(image.Packages))
	for path, archive := range image.Packages {
		packageHashes[path] = archive.ArtifactHash
		ownedImage.Packages[path] = ir.PackageArchive{ArtifactHash: archive.ArtifactHash}
	}
	return &Program{code: &programCode{
		image: ownedImage, root: root, modules: modules, moduleOrder: moduleOrder,
		entries: entries, packageHashes: packageHashes,
	}}, nil
}

func prepareModuleReferences(modules map[string]*executable) ([]string, error) {
	moduleOrder := make([]string, 0, len(modules))
	for modulePath := range modules {
		moduleOrder = append(moduleOrder, modulePath)
	}
	sort.Strings(moduleOrder)
	moduleIndexes := make(map[string]int, len(moduleOrder))
	for index, modulePath := range moduleOrder {
		moduleIndexes[modulePath] = index
	}
	for _, modulePath := range moduleOrder {
		executable := modules[modulePath]
		for functionID, function := range executable.Functions {
			for pc := range function.Instructions {
				instruction := &function.Instructions[pc]
				if instruction.op == preparedAddressOf && instruction.address.Kind == "export" {
					address := instruction.address
					target := modules[address.ModulePath]
					if target == nil {
						return nil, fmt.Errorf("link %s.%s instruction %d: unknown module %q", modulePath, functionID, pc, address.ModulePath)
					}
					export, ok := target.Exports[address.Export]
					if !ok || export.Kind != "global" {
						return nil, fmt.Errorf("link %s.%s instruction %d: address target %s.%s is not an exported variable", modulePath, functionID, pc, address.ModulePath, address.Export)
					}
					continue
				}
				if instruction.op != preparedCallDirect && instruction.op != preparedTailCallDirect {
					continue
				}
				call := instruction.call
				targetPath := strings.TrimSpace(call.ModulePath)
				if targetPath == "" {
					targetPath = modulePath
				}
				targetModule, ok := modules[targetPath]
				if !ok {
					return nil, fmt.Errorf("link %s.%s instruction %d: unknown module %q", modulePath, functionID, pc, targetPath)
				}
				target, ok := targetModule.Functions[call.Function]
				if !ok {
					return nil, fmt.Errorf("link %s.%s instruction %d: unknown function %q in module %q", modulePath, functionID, pc, call.Function, targetPath)
				}
				if call.ArgCount != len(target.Decl.Signature.Params) || call.ResultCount != len(target.Decl.Signature.Results) {
					return nil, fmt.Errorf("link %s.%s instruction %d: call shape (%d, %d) does not match %s.%s (%d, %d)",
						modulePath, functionID, pc, call.ArgCount, call.ResultCount, targetPath, call.Function,
						len(target.Decl.Signature.Params), len(target.Decl.Signature.Results))
				}
				instruction.callModuleIndex = moduleIndexes[targetPath]
				instruction.callFunctionIndex = targetModule.FunctionIndexes[call.Function]
			}
			executable.Functions[functionID] = function
		}
	}
	return moduleOrder, nil
}

func (p *Program) Run() (RunResult, error) {
	return p.RunWithOptions(InstanceOptions{})
}

// RunWithOptions executes the image entry with host-provided runtime options.
func (p *Program) RunWithOptions(options InstanceOptions) (result RunResult, err error) {
	instance, err := p.Instantiate(context.Background(), options)
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, instance.Close()) }()
	return instance.CallMain(context.Background())
}

func (p *Program) RunEntry(name string, args ...HostValue) (result RunResult, err error) {
	instance, err := p.Instantiate(context.Background(), InstanceOptions{})
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, instance.Close()) }()
	return instance.Call(context.Background(), name, args...)
}

func (p *Program) newVM(options InstanceOptions) (*vm, error) {
	if p == nil || p.code == nil || p.code.root == nil {
		return nil, errors.New("nil program")
	}
	registry := newModuleRegistry()
	for path, module := range p.code.modules {
		if path == p.code.image.Root {
			continue
		}
		if err := registry.addExecutable(module); err != nil {
			return nil, err
		}
	}
	if options.modules != nil {
		return nil, errors.New("program options must not provide a module registry")
	}
	options.modules = registry
	vm, err := newVMWithOptions(p.code.root, options)
	if err != nil {
		return nil, err
	}
	if err := vm.installInitialRevision(p); err != nil {
		vm.closeRevisions()
		return nil, err
	}
	return vm, nil
}
