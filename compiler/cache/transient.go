package cache

import (
	"context"

	"github.com/d7z-team/mini-go/compiler/ast"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// TransientConfig bounds structured compiler state owned by a live service.
type TransientConfig struct {
	MaxEntries int
	MaxBytes   int64
}

const maximumEstimate = int64(1<<63 - 1)

// TransientStats reports observable cache behavior and current ownership.
type TransientStats struct {
	Hits, Misses, Stores, Evictions, Clears uint64
	Entries                                 int
	// Bytes is the estimated memory owned by cached structured values.
	Bytes int64
}

type transientCompileEntry struct {
	lookup   Lookup
	manifest Manifest
	size     int64
}

type transientPrepareEntry struct {
	lookup  PrepareLookup
	symbols *ir.ProgramSymbols
	size    int64
}

type transientOrderEntry struct {
	key       string
	isPrepare bool
}

// TransientCache keeps validated structured values without persistent JSON
// round trips. It is bounded and independent from disk cache storage.
type TransientCache struct {
	mu      cacheMutex
	config  TransientConfig
	compile map[string]transientCompileEntry
	prepare map[string]transientPrepareEntry
	order   []transientOrderEntry
	locks   actionLocks
	bytes   uint64
	stats   TransientStats
}

func NewTransient(config TransientConfig) *TransientCache {
	if config.MaxEntries <= 0 {
		config.MaxEntries = 128
	}
	if config.MaxBytes <= 0 {
		config.MaxBytes = 256 << 20
	}
	return &TransientCache{
		config: config, compile: make(map[string]transientCompileEntry),
		prepare: make(map[string]transientPrepareEntry),
	}
}

func (c *TransientCache) LookupCompileManifest(action Action) (ManifestLookup, error) {
	key, err := action.Key()
	if err != nil {
		return ManifestLookup{}, err
	}
	c.mu.Lock()
	entry, ok := c.compile[key]
	if ok {
		c.stats.Hits++
	} else {
		c.stats.Misses++
	}
	c.mu.Unlock()
	if !ok {
		return ManifestLookup{Reason: "transient action missing"}, nil
	}
	manifest := entry.manifest
	manifest.RuntimeDependencies = append([]string(nil), manifest.RuntimeDependencies...)
	return ManifestLookup{Manifest: manifest, Hit: true, Reason: "transient hit"}, nil
}

func (c *TransientCache) LookupCompile(action Action) (Lookup, error) {
	key, err := action.Key()
	if err != nil {
		return Lookup{}, err
	}
	c.mu.Lock()
	entry, ok := c.compile[key]
	if ok {
		c.stats.Hits++
	} else {
		c.stats.Misses++
	}
	c.mu.Unlock()
	if !ok {
		return Lookup{Reason: "transient action missing"}, nil
	}
	lookup := entry.lookup
	lookup.Artifact = ir.CloneArtifact(lookup.Artifact)
	lookup.Symbols = ir.ClonePackageSymbols(lookup.Symbols)
	lookup.ExportData = clonePackageData(lookup.ExportData)
	lookup.Hit = true
	lookup.Reason = "transient hit"
	return lookup, nil
}

func (c *TransientCache) StoreCompile(action Action, sealed CompiledArtifact, symbols ir.PackageSymbols, data PackageData) (Manifest, error) {
	if err := validateCompileEntry(action, sealed, symbols, data); err != nil {
		return Manifest{}, err
	}
	artifact, artifactHash := sealed.artifact, sealed.hash
	key, err := action.Key()
	if err != nil {
		return Manifest{}, err
	}
	manifest := NewManifest(artifact, artifactHash, data.ExportHash)
	size := cacheEntryBytes(key, estimateArtifactBytes(artifact), estimatePackageSymbolsBytes(symbols), estimatePackageDataBytes(data))
	for _, dependency := range manifest.RuntimeDependencies {
		size = cacheEntryBytes(dependency, size)
	}
	if size == maximumEstimate || size > c.config.MaxBytes {
		return manifest, nil
	}
	entry := transientCompileEntry{
		lookup:   Lookup{Artifact: artifact, Symbols: ir.ClonePackageSymbols(symbols), ExportData: clonePackageData(data), ArtifactHash: artifactHash, ExportHash: data.ExportHash},
		manifest: manifest, size: size,
	}
	c.mu.Lock()
	if previous, ok := c.compile[key]; ok {
		c.bytes -= uint64(previous.size)
	} else {
		c.order = append(c.order, transientOrderEntry{key: key})
	}
	c.compile[key] = entry
	c.bytes += uint64(entry.size)
	c.stats.Stores++
	c.evictLocked()
	c.mu.Unlock()
	manifest.RuntimeDependencies = append([]string(nil), manifest.RuntimeDependencies...)
	return manifest, nil
}

func (c *TransientCache) LookupPrepare(action PrepareAction) (PrepareLookup, error) {
	key, err := action.Key()
	if err != nil {
		return PrepareLookup{}, err
	}
	c.mu.Lock()
	entry, ok := c.prepare[key]
	if ok {
		c.stats.Hits++
	} else {
		c.stats.Misses++
	}
	c.mu.Unlock()
	if !ok {
		return PrepareLookup{Reason: "transient action missing"}, nil
	}
	lookup := entry.lookup
	lookup.Image = ir.CloneExecutionImage(lookup.Image)
	lookup.TestManifest = append([]TestEntry(nil), lookup.TestManifest...)
	lookup.Hit = true
	lookup.Reason = "transient hit"
	return lookup, nil
}

func (c *TransientCache) StorePrepare(action PrepareAction, output PreparedOutput) error {
	if err := validatePreparedOutput(action, output, true); err != nil {
		return err
	}
	key, err := action.Key()
	if err != nil {
		return err
	}
	size := cacheEntryBytes(key, estimateExecutionImageBytes(output.Image))
	for _, entry := range output.TestManifest {
		size = cacheEntryBytes(entry.Package+entry.Name, size, 64)
	}
	if size == maximumEstimate || size > c.config.MaxBytes {
		return nil
	}
	entry := transientPrepareEntry{
		lookup: PrepareLookup{Image: ir.CloneExecutionImage(output.Image), TestManifest: append([]TestEntry(nil), output.TestManifest...)},
		size:   size,
	}
	c.storePrepareEntry(key, entry)
	return nil
}

func (c *TransientCache) LookupSymbols(action SymbolAction) (SymbolLookup, error) {
	key, err := action.Key()
	if err != nil {
		return SymbolLookup{}, err
	}
	key = "symbols\x00" + key
	c.mu.Lock()
	entry, ok := c.prepare[key]
	if ok {
		c.stats.Hits++
	} else {
		c.stats.Misses++
	}
	c.mu.Unlock()
	if !ok || entry.symbols == nil {
		return SymbolLookup{Reason: "transient action missing"}, nil
	}
	return SymbolLookup{Symbols: ir.CloneProgramSymbols(*entry.symbols), Hit: true, Reason: "transient hit"}, nil
}

func (c *TransientCache) StoreSymbols(action SymbolAction, symbols ir.ProgramSymbols) error {
	if err := validateSymbolAction(action, symbols); err != nil {
		return err
	}
	key, err := action.Key()
	if err != nil {
		return err
	}
	key = "symbols\x00" + key
	size := cacheEntryBytes(key, estimateProgramSymbolsBytes(symbols))
	if size == maximumEstimate || size > c.config.MaxBytes {
		return nil
	}
	owned := ir.CloneProgramSymbols(symbols)
	entry := transientPrepareEntry{symbols: &owned, size: size}
	c.storePrepareEntry(key, entry)
	return nil
}

func (c *TransientCache) storePrepareEntry(key string, entry transientPrepareEntry) {
	c.mu.Lock()
	if previous, ok := c.prepare[key]; ok {
		c.bytes -= uint64(previous.size)
	} else {
		c.order = append(c.order, transientOrderEntry{key: key, isPrepare: true})
	}
	c.prepare[key] = entry
	c.bytes += uint64(entry.size)
	c.stats.Stores++
	c.evictLocked()
	c.mu.Unlock()
}

func (c *TransientCache) LockCompile(ctx context.Context, action Action) (func(), error) {
	key, err := action.Key()
	if err != nil {
		return nil, err
	}
	return c.locks.acquire(ctx, "compile\x00"+key)
}

func (c *TransientCache) LockPrepare(ctx context.Context, action PrepareAction) (func(), error) {
	key, err := action.Key()
	if err != nil {
		return nil, err
	}
	return c.locks.acquire(ctx, "prepare\x00"+key)
}

func (c *TransientCache) LockSymbols(ctx context.Context, action SymbolAction) (func(), error) {
	key, err := action.Key()
	if err != nil {
		return nil, err
	}
	return c.locks.acquire(ctx, "symbols\x00"+key)
}

func (c *TransientCache) evictLocked() {
	for len(c.compile)+len(c.prepare) > c.config.MaxEntries || c.bytes > uint64(c.config.MaxBytes) {
		oldest := c.order[0]
		c.order[0] = transientOrderEntry{}
		c.order = c.order[1:]
		if oldest.isPrepare {
			if entry, ok := c.prepare[oldest.key]; ok {
				delete(c.prepare, oldest.key)
				c.bytes -= uint64(entry.size)
				c.stats.Evictions++
			}
		} else if entry, ok := c.compile[oldest.key]; ok {
			delete(c.compile, oldest.key)
			c.bytes -= uint64(entry.size)
			c.stats.Evictions++
		}
	}
}

func (c *TransientCache) Stats() TransientStats {
	c.mu.Lock()
	stats := c.stats
	stats.Entries = len(c.compile) + len(c.prepare)
	stats.Bytes = int64(c.bytes)
	c.mu.Unlock()
	return stats
}

func (c *TransientCache) Clear() {
	c.mu.Lock()
	c.compile = make(map[string]transientCompileEntry)
	c.prepare = make(map[string]transientPrepareEntry)
	c.order = nil
	c.bytes = 0
	c.stats.Clears++
	c.mu.Unlock()
}

func (c *TransientCache) Close() { c.Clear() }

func clonePackageData(data PackageData) PackageData {
	out := data
	out.TypeTable = types.CloneTable(data.TypeTable)
	out.Constants = make([]ir.Constant, len(data.Constants))
	for index, constant := range data.Constants {
		out.Constants[index] = constant
		out.Constants[index].Value = append([]byte(nil), constant.Value...)
	}
	out.Exports = append([]ir.Export(nil), data.Exports...)
	out.Requirements = make([]ir.Requirement, len(data.Requirements))
	for index, requirement := range data.Requirements {
		out.Requirements[index] = requirement
		out.Requirements[index].Exports = append([]string(nil), requirement.Exports...)
	}
	out.SourceFiles = append([]ir.SourceFile(nil), data.SourceFiles...)
	out.GenericTemplates = make([]GenericTemplate, len(data.GenericTemplates))
	for index, template := range data.GenericTemplates {
		out.GenericTemplates[index] = template
		out.GenericTemplates[index].Decl = ast.CloneDecl(template.Decl)
		out.GenericTemplates[index].References = append([]GenericReference(nil), template.References...)
	}
	return out
}

func estimateArtifactBytes(artifact ir.Artifact) int64 {
	size := estimatePackageDataBytes(PackageData{
		ModulePath: artifact.Module.Path, Package: artifact.Module.Package,
		TypeTable: artifact.TypeTable, Constants: artifact.Constants, Exports: artifact.Exports, Requirements: artifact.Requirements,
	})
	size = cacheEntryBytes(artifact.Format, size, int64(len(artifact.OpcodeSet)))
	for _, global := range artifact.Globals {
		size = cacheEntryBytes(global.ID, size, estimateTypeRefBytes(global.Type))
	}
	for _, function := range artifact.Functions {
		size = cacheEntryBytes(function.ID, size, estimateSignatureBytes(function.Signature))
		for _, local := range function.Locals {
			size = cacheEntryBytes(local.ID, size, estimateTypeRefBytes(local.Type))
		}
		for _, upvalue := range function.Upvalues {
			size = cacheEntryBytes(upvalue.ID, size, estimateTypeRefBytes(upvalue.Type))
		}
		for _, local := range function.ResultLocals {
			size = cacheEntryBytes(local, size)
		}
		if code := function.Code; code != nil {
			size += int64(code.Descriptors.Bytes()) + int64(len(code.Instructions))*12
			for _, typ := range code.Types {
				size += estimateTypeRefBytes(typ)
			}
			for _, operands := range code.Operands {
				size += int64(len(operands.Inputs))*8 + int64(len(operands.Outputs)+len(operands.Release)+len(operands.ReleaseBefore))*4
			}
		}
	}
	return size
}

func estimatePackageDataBytes(data PackageData) int64 {
	size := cacheEntryBytes(data.ModulePath, int64(len(data.Format)+len(data.Package)+len(data.ArtifactHash)+len(data.ExportHash)))
	for _, node := range data.TypeTable.Nodes {
		size = cacheEntryBytes(string(node.ID), size, 512, int64(len(node.Name)+len(node.Identity.ModulePath)+len(node.Identity.DeclID)))
		for _, ref := range []types.TypeRef{node.AliasTarget, node.Underlying, node.Elem, node.Key, node.Constraint, node.Base} {
			size = cacheEntryBytes("", size, estimateTypeRefBytes(ref))
		}
		for _, refs := range [][]types.TypeRef{node.Tuple, node.TypeArgs} {
			for _, ref := range refs {
				size = cacheEntryBytes("", size, estimateTypeRefBytes(ref))
			}
		}
		for _, field := range node.Fields {
			size = cacheEntryBytes(field.Name, size, int64(len(field.Tag)), estimateTypeRefBytes(field.Type))
		}
		for _, method := range node.Methods {
			size = cacheEntryBytes(method.Name, size, int64(len(method.FunctionID)+len(method.ModulePath)), estimateTypeRefBytes(method.Receiver), estimateSignatureBytes(method.Signature))
		}
		for _, term := range node.Terms {
			size = cacheEntryBytes("", size, estimateTypeRefBytes(term.Type))
		}
		if node.Signature != nil {
			size = cacheEntryBytes("", size, estimateSignatureBytes(*node.Signature))
		}
	}
	for _, constant := range data.Constants {
		size = cacheEntryBytes(constant.ID, size, int64(len(constant.Value)), estimateTypeRefBytes(constant.Type))
	}
	for _, export := range data.Exports {
		size = cacheEntryBytes(export.Name, size, int64(len(export.ID)+len(export.Kind)), estimateTypeRefBytes(export.Type))
	}
	for _, requirement := range data.Requirements {
		size = cacheEntryBytes(requirement.ModulePath, size, int64(len(requirement.Hash)+len(requirement.Kind)))
		for _, name := range requirement.Exports {
			size = cacheEntryBytes(name, size)
		}
	}
	for _, file := range data.SourceFiles {
		size = cacheEntryBytes(file.Path, size, int64(len(file.ID)+len(file.Hash)))
	}
	for _, template := range data.GenericTemplates {
		size = cacheEntryBytes(template.Name, size, int64(len(template.DeclID)+len(template.Kind)+len(template.Type)), ast.EstimatedDeclBytes(template.Decl))
		for _, ref := range template.References {
			size = cacheEntryBytes(ref.Name, size, int64(len(ref.Span.Start.File)+len(ref.Span.End.File)), 64)
		}
	}
	return size
}

// A conservative structural estimate: values shared between fields are charged
// independently. Saturation rejects an entry instead of wrapping its budget.
func cacheEntryBytes(text string, parts ...int64) int64 {
	size := int64(len(text))
	if size > maximumEstimate-64 {
		return maximumEstimate
	}
	size += 64
	for _, part := range parts {
		if part < 0 || part > maximumEstimate-size {
			return maximumEstimate
		}
		size += part
	}
	return size
}

func estimateTypeRefBytes(ref types.TypeRef) int64 {
	return cacheEntryBytes(string(ref.Node), int64(len(ref.Named.ModulePath)+len(ref.Named.DeclID)))
}

func estimateSignatureBytes(signature types.FunctionSignature) int64 {
	var size int64
	for _, param := range signature.Params {
		size = cacheEntryBytes("", size, estimateTypeRefBytes(param.Type))
	}
	for _, result := range signature.Results {
		size = cacheEntryBytes("", size, estimateTypeRefBytes(result))
	}
	return size
}

func estimateExecutionImageBytes(image ir.ExecutionImage) int64 {
	size := cacheEntryBytes(image.CompilerID, int64(len(image.Root)+len(image.Hash)+len(image.Format)+len(image.ContractID)))
	for _, tag := range image.Target.Tags {
		size = cacheEntryBytes(tag, size)
	}
	for _, capability := range image.Capabilities {
		size = cacheEntryBytes(capability, size)
	}
	for modulePath, archive := range image.Packages {
		size = cacheEntryBytes(modulePath, size, int64(len(archive.Artifact)), int64(len(archive.ArtifactHash)), archive.ValidationBytes())
	}
	for _, entry := range image.Entries {
		size = cacheEntryBytes(entry.Name, size, int64(len(entry.ModulePath)+len(entry.FunctionID)))
	}
	return size
}

func estimateProgramSymbolsBytes(symbols ir.ProgramSymbols) int64 {
	size := cacheEntryBytes(symbols.ProgramHash, int64(len(symbols.CompilerID)+len(symbols.Hash)+len(symbols.Format)+len(symbols.ContractID)))
	for modulePath, symbols := range symbols.Packages {
		size = cacheEntryBytes(modulePath, size, estimatePackageSymbolsBytes(symbols))
	}
	return size
}

func estimatePackageSymbolsBytes(symbols ir.PackageSymbols) int64 {
	size := cacheEntryBytes(symbols.ModulePath, int64(len(symbols.SourceHash)+len(symbols.CodeHash)))
	for _, file := range symbols.Files {
		size = cacheEntryBytes(file.ID, size, int64(len(file.Path)+len(file.Hash)))
	}
	for _, global := range symbols.Globals {
		size = cacheEntryBytes(global.ID, size, int64(len(global.Name)))
	}
	for _, function := range symbols.Functions {
		size = cacheEntryBytes(function.ID, size, int64(len(function.Name)), 128)
		if function.Declaration != nil {
			size = cacheEntryBytes(function.Declaration.File, size)
		}
		for _, local := range function.Locals {
			size = cacheEntryBytes(local.ID, size, int64(len(local.Name)))
			if local.Declaration != nil {
				size = cacheEntryBytes(local.Declaration.File, size)
			}
		}
		for _, upvalue := range function.Upvalues {
			size = cacheEntryBytes(upvalue.ID, size, int64(len(upvalue.Name)))
		}
		for _, scope := range function.Scopes {
			size = cacheEntryBytes("", size, int64(len(scope.Ranges))*16)
		}
		for _, location := range function.Locations {
			size = cacheEntryBytes("", size)
			for _, point := range location.Points {
				size = cacheEntryBytes(point.File, size)
			}
		}
	}
	return size
}
