package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/identity"
	"github.com/d7z-team/mini-go/compiler/target"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

const (
	Format  = "mini-go-compile-cache"
	Version = 29

	packageActionDomain   = "mini-go/package-action/binary/v1"
	packageManifestDomain = "minigo/package-manifest/v5\x00"
	prepareActionDomain   = "mini-go/prepare-action/binary/v1"
	symbolActionDomain    = "mini-go/symbol-action/binary/v1"
)

// Cache stores compiler-derived package and execution-image state and owns
// per-action process locks used to suppress duplicate work.
type Cache interface {
	LookupCompileManifest(Action) (ManifestLookup, error)
	LookupCompile(Action) (Lookup, error)
	StoreCompile(action Action, artifact CompiledArtifact, symbols ir.PackageSymbols, packageData PackageData) (Manifest, error)
	LookupPrepare(PrepareAction) (PrepareLookup, error)
	StorePrepare(PrepareAction, PreparedOutput) error
	LookupSymbols(SymbolAction) (SymbolLookup, error)
	StoreSymbols(SymbolAction, ir.ProgramSymbols) error
	LockCompile(context.Context, Action) (func(), error)
	LockPrepare(context.Context, PrepareAction) (func(), error)
	LockSymbols(context.Context, SymbolAction) (func(), error)
}

type SourceFile struct {
	Path     string `json:"path"`
	Hash     string `json:"hash"`
	Selected bool   `json:"selected"`
}

type Dependency struct {
	ModulePath string `json:"module_path"`
	ExportHash string `json:"export_hash"`
}

type Action struct {
	LimitsHash       string        `json:"limits_hash"`
	Format           string        `json:"format"`
	Version          int           `json:"version"`
	Compiler         string        `json:"compiler"`
	IRFormat         string        `json:"ir_format"`
	IRVersion        int           `json:"ir_version"`
	OpcodeSet        string        `json:"opcode_set"`
	IntrinsicSchema  string        `json:"intrinsic_schema"`
	Optimization     uint8         `json:"optimization"`
	Target           target.Target `json:"target"`
	PackageID        string        `json:"package_id"`
	ModulePath       string        `json:"module_path"`
	Package          string        `json:"package"`
	SourceFiles      []SourceFile  `json:"source_files,omitempty"`
	ResourceFiles    []SourceFile  `json:"resource_files,omitempty"`
	DependencyHashes []Dependency  `json:"dependency_hashes,omitempty"`
}

type Lookup struct {
	Artifact     ir.Artifact
	Symbols      ir.PackageSymbols
	ExportData   PackageData
	ArtifactHash string
	ExportHash   string
	Hit          bool
	Reason       string
}

type Manifest struct {
	ArtifactHash        string
	ExportHash          string
	RuntimeDependencies []string
}

type ManifestLookup struct {
	Manifest
	Hit    bool
	Reason string
}

type PrepareArtifact struct {
	ModulePath string `json:"module_path"`
	// Hash identifies the package artifact before dependency hashes are bound.
	Hash string `json:"hash"`
}

type EntryPoint struct {
	Name       string `json:"name"`
	ModulePath string `json:"module_path"`
	Function   string `json:"function"`
}

type PrepareAction struct {
	LimitsHash   string            `json:"limits_hash"`
	Format       string            `json:"format"`
	Version      int               `json:"version"`
	Compiler     string            `json:"compiler"`
	Contract     string            `json:"contract"`
	Target       target.Target     `json:"target"`
	Mode         string            `json:"mode"`
	Root         string            `json:"root"`
	Entries      []EntryPoint      `json:"entries"`
	Artifacts    []PrepareArtifact `json:"artifacts"`
	Capabilities []string          `json:"capabilities,omitempty"`
}

type SymbolAction struct {
	Format          string `json:"format"`
	Version         int    `json:"version"`
	Compiler        string `json:"compiler"`
	ProgramHash     string `json:"program_hash"`
	SourceGraphHash string `json:"source_graph_hash"`
	Optimization    uint8  `json:"optimization"`
}

type TestEntry struct {
	Package string `json:"package"`
	Name    string `json:"name"`
	Index   int    `json:"index"`
}

type PreparedOutput struct {
	Image        ir.ExecutionImage
	TestManifest []TestEntry
}

type PrepareLookup struct {
	Image        ir.ExecutionImage
	TestManifest []TestEntry
	Hit          bool
	Reason       string
}

type SymbolLookup struct {
	Symbols ir.ProgramSymbols
	Hit     bool
	Reason  string
}

func NewCompileAction(compiler string, buildTarget target.Target, modulePath, packageName string, sources []SourceFile, dependencies []Dependency) Action {
	modulePath = strings.TrimSpace(modulePath)
	return Action{
		Format: Format, Version: Version, Compiler: strings.TrimSpace(compiler),
		IRFormat: ir.Format, IRVersion: ir.CurrentVersion, OpcodeSet: ir.OpcodeSet, IntrinsicSchema: ir.IntrinsicSchema, Target: buildTarget,
		PackageID: "module:" + modulePath + "::", ModulePath: modulePath, Package: strings.TrimSpace(packageName),
		SourceFiles:      append([]SourceFile(nil), sources...),
		DependencyHashes: append([]Dependency(nil), dependencies...),
	}
}

func (a Action) ID() (ActionID, error) {
	data, err := a.normalized()
	if err != nil {
		return ActionID{}, err
	}
	w := identity.New(packageActionDomain)
	EncodeIdentityAction(w, data)
	sum, err := w.Sum()
	return ActionID(sum), err
}

func (a Action) Key() (string, error) {
	id, err := a.ID()
	return id.String(), err
}

func NewPrepareAction(compiler, contract string, buildTarget target.Target, mode, root string, entries []EntryPoint, artifacts []PrepareArtifact, capabilities []string) PrepareAction {
	return PrepareAction{
		Format: Format, Version: Version, Compiler: strings.TrimSpace(compiler), Contract: strings.TrimSpace(contract),
		Target: buildTarget, Mode: strings.TrimSpace(mode), Root: strings.TrimSpace(root),
		Entries: append([]EntryPoint(nil), entries...), Artifacts: append([]PrepareArtifact(nil), artifacts...),
		Capabilities: append([]string(nil), capabilities...),
	}
}

func (a PrepareAction) ID() (ActionID, error) {
	data, err := a.normalized()
	if err != nil {
		return ActionID{}, err
	}
	w := identity.New(prepareActionDomain)
	EncodeIdentityPrepareAction(w, data)
	sum, err := w.Sum()
	return ActionID(sum), err
}

func (a PrepareAction) Key() (string, error) {
	id, err := a.ID()
	return id.String(), err
}

func (a PrepareAction) MaterialJSON() ([]byte, error) {
	canonical, err := a.normalized()
	if err != nil {
		return nil, err
	}
	return canonicalJSON(canonical)
}

func (a PrepareAction) normalized() (PrepareAction, error) {
	normalizedTarget, err := target.Normalize(a.Target)
	if err != nil {
		return PrepareAction{}, err
	}
	canonical := a
	canonical.Target = normalizedTarget
	canonical.Mode = strings.TrimSpace(canonical.Mode)
	canonical.Root = strings.TrimSpace(canonical.Root)
	canonical.Compiler = strings.TrimSpace(canonical.Compiler)
	canonical.Contract = strings.TrimSpace(canonical.Contract)
	canonical.Entries = append([]EntryPoint(nil), a.Entries...)
	sort.Slice(canonical.Entries, func(i, j int) bool {
		if canonical.Entries[i].Name != canonical.Entries[j].Name {
			return canonical.Entries[i].Name < canonical.Entries[j].Name
		}
		if canonical.Entries[i].ModulePath != canonical.Entries[j].ModulePath {
			return canonical.Entries[i].ModulePath < canonical.Entries[j].ModulePath
		}
		return canonical.Entries[i].Function < canonical.Entries[j].Function
	})
	canonical.Artifacts = append([]PrepareArtifact(nil), a.Artifacts...)
	sort.Slice(canonical.Artifacts, func(i, j int) bool {
		return canonical.Artifacts[i].ModulePath < canonical.Artifacts[j].ModulePath
	})
	canonical.Capabilities = append([]string(nil), a.Capabilities...)
	for index := range canonical.Capabilities {
		canonical.Capabilities[index] = strings.TrimSpace(canonical.Capabilities[index])
	}
	sort.Strings(canonical.Capabilities)
	canonical.Capabilities = slices.Compact(canonical.Capabilities)
	return canonical, nil
}

func NewSymbolAction(compiler, programHash, sourceGraphHash string, optimization uint8) SymbolAction {
	return SymbolAction{
		Format: Format, Version: Version, Compiler: strings.TrimSpace(compiler),
		ProgramHash: strings.TrimSpace(programHash), SourceGraphHash: strings.TrimSpace(sourceGraphHash),
		Optimization: optimization,
	}
}

func (a SymbolAction) ID() (ActionID, error) {
	w := identity.New(symbolActionDomain)
	EncodeIdentitySymbolAction(w, a.normalized())
	sum, err := w.Sum()
	return ActionID(sum), err
}

func (a SymbolAction) Key() (string, error) {
	id, err := a.ID()
	return id.String(), err
}

func (a SymbolAction) MaterialJSON() ([]byte, error) {
	return canonicalJSON(a.normalized())
}

func (a SymbolAction) normalized() SymbolAction {
	canonical := a
	canonical.Compiler = strings.TrimSpace(canonical.Compiler)
	canonical.ProgramHash = strings.TrimSpace(canonical.ProgramHash)
	canonical.SourceGraphHash = strings.TrimSpace(canonical.SourceGraphHash)
	return canonical
}

func (a Action) MaterialJSON() ([]byte, error) {
	canonical, err := a.normalized()
	if err != nil {
		return nil, err
	}
	return canonicalJSON(canonical)
}

func (a Action) normalized() (Action, error) {
	normalizedTarget, err := target.Normalize(a.Target)
	if err != nil {
		return Action{}, err
	}
	canonical := a
	canonical.PackageID = strings.TrimSpace(canonical.PackageID)
	canonical.Target = normalizedTarget
	canonical.SourceFiles = append([]SourceFile(nil), a.SourceFiles...)
	canonical.ResourceFiles = append([]SourceFile(nil), a.ResourceFiles...)
	sort.Slice(canonical.ResourceFiles, func(i, j int) bool {
		return canonical.ResourceFiles[i].Path < canonical.ResourceFiles[j].Path
	})
	canonical.DependencyHashes = append([]Dependency(nil), a.DependencyHashes...)
	sort.Slice(canonical.DependencyHashes, func(i, j int) bool {
		return canonical.DependencyHashes[i].ModulePath < canonical.DependencyHashes[j].ModulePath
	})
	return canonical, nil
}

func hashAction(domain string, material []byte) ActionID {
	hasher := sha256.New()
	hasher.Write([]byte(domain))
	hasher.Write(material)
	var id ActionID
	copy(id[:], hasher.Sum(nil))
	return id
}

func canonicalJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}
