package language

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/analysis"
	docgen "github.com/d7z-team/mini-go/compiler/doc"
	"github.com/d7z-team/mini-go/compiler/format"
	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

type Config struct {
	Root                string
	Target              target.Target
	Sources             workspace.SourceSet
	Documents           workspace.SourceSet
	URI                 func(modulePath, path string) DocumentURI
	Limits              Limits
	RenderDocumentation func(docgen.Catalog) func(string, docgen.Comment) string
	FormatSource        func(string, string, string) format.Result
}

type indexedOccurrence struct {
	// The immutable package owns the occurrence array for this snapshot.
	*analysis.Occurrence
	ModulePath string
	Key        string
}

type Snapshot struct {
	workspaceDiagnostics []source.Diagnostic
	generation           uint64
	partial              bool
	documents            map[DocumentURI]Document
	packages             map[string]analysis.Package
	documentation        docgen.Catalog
	diagnostics          map[DocumentURI][]Diagnostic
	definitions          map[string]Location
	occurrences          map[DocumentURI][]indexedOccurrence
	references           map[string][]Location
	resultIDs            map[DocumentURI]string
	sourceHash           map[string]string
	exportHash           map[string]string
	dependencyHash       map[string]string
	checkedPackages      int
	reusedPackages       int
	checked              analysis.WorkspaceResult
	renderComment        func(string, docgen.Comment) string
	editable             map[DocumentURI]bool
}

type Engine struct {
	root                string
	target              target.Target
	limits              Limits
	store               *DocumentStore
	base                map[DocumentURI]string
	editable            map[DocumentURI]bool
	baseSource          workspace.SourceSet
	snapshot            *Snapshot
	renderDocumentation func(docgen.Catalog) func(string, docgen.Comment) string
	formatSource        func(string, string, string) format.Result
}

func NewEngine(config Config) (*Engine, error) {
	return NewEngineContext(context.Background(), config)
}

// NewEngineContext constructs the initial immutable semantic snapshot.
func NewEngineContext(ctx context.Context, config Config) (*Engine, error) {
	return newEngine(ctx, config, true)
}

func newEngine(ctx context.Context, config Config, analyze bool) (*Engine, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.Sources == nil {
		return nil, errors.New("nil LSP source set")
	}
	normalizedTarget, err := target.Normalize(config.Target)
	if err != nil {
		return nil, err
	}
	limits := config.Limits
	if limits.MaxFileBytes <= 0 {
		limits = DefaultLimits()
	}
	uriFor := config.URI
	if uriFor == nil {
		uriFor = func(modulePath, path string) DocumentURI {
			return DocumentURI("mini-go://" + modulePath + "/" + path)
		}
	}
	editableSources := config.Documents
	if editableSources == nil {
		editableSources = config.Sources
	}
	editableFiles := make(map[string]struct{})
	var discoveryDiagnostics []source.Diagnostic
	paths, err := editableSources.PackagePaths()
	if err != nil {
		discoveryDiagnostics = append(discoveryDiagnostics, source.Diagnostic{Code: "workspace.source.read", Severity: source.SeverityError, Message: err.Error()})
	}
	for _, modulePath := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pkg, ok, err := editableSources.Package(modulePath)
		if err != nil {
			discoveryDiagnostics = append(discoveryDiagnostics, source.Diagnostic{ModulePath: modulePath, Code: "workspace.source.read", Severity: source.SeverityError, Message: err.Error()})
			continue
		}
		if !ok {
			continue
		}
		for _, file := range append(append([]source.File(nil), pkg.Files...), pkg.TestFiles...) {
			editableFiles[modulePath+"\x00"+file.Path] = struct{}{}
		}
	}
	engine := &Engine{
		renderDocumentation: config.RenderDocumentation,
		formatSource:        config.FormatSource,
		root:                strings.TrimSpace(config.Root),
		target:              normalizedTarget,
		limits:              limits,
		store:               NewDocumentStore(limits),
		base:                map[DocumentURI]string{},
		editable:            map[DocumentURI]bool{},
		baseSource:          config.Sources,
	}
	if engine.formatSource == nil {
		engine.formatSource = format.Source
	}
	for _, modulePath := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pkg, ok, err := config.Sources.Package(modulePath)
		if err != nil {
			discoveryDiagnostics = append(discoveryDiagnostics, source.Diagnostic{ModulePath: modulePath, Code: "workspace.source.read", Severity: source.SeverityError, Message: err.Error()})
			continue
		}
		if !ok {
			continue
		}
		files := append(append([]source.File(nil), pkg.Files...), pkg.TestFiles...)
		for _, file := range files {
			_, editable := editableFiles[modulePath+"\x00"+file.Path]
			documentURI := DocumentURI("mini-go://" + modulePath + "/" + file.Path)
			if editable {
				documentURI = uriFor(modulePath, file.Path)
			}
			identity := DocumentIdentity{URI: documentURI, ModulePath: modulePath, Path: file.Path}
			if err := engine.store.Seed(identity, file.Text); err != nil {
				return nil, err
			}
			engine.base[identity.URI] = file.Text
			engine.editable[identity.URI] = editable
		}
	}
	if engine.root == "" {
		discoveryDiagnostics = append(discoveryDiagnostics, source.Diagnostic{Code: "workspace.root.missing", Severity: source.SeverityError, Message: "missing LSP workspace root module"})
	}
	if !analyze {
		return engine, nil
	}
	if err := engine.rebuild(ctx); err != nil {
		return nil, err
	}
	engine.snapshot.workspaceDiagnostics = source.NormalizeDiagnostics(append(engine.snapshot.workspaceDiagnostics, discoveryDiagnostics...))
	return engine, nil
}

func (e *Engine) Snapshot() *Snapshot { return e.snapshot }

// Documents returns an owned map of immutable text views.
func (s *Snapshot) Documents() map[DocumentURI]Document { return maps.Clone(s.documents) }
func (s *Snapshot) ResultID(uri DocumentURI) string     { return s.resultIDs[uri] }
func (s *Snapshot) ReuseStats() (checked, reused int)   { return s.checkedPackages, s.reusedPackages }

func (s *Snapshot) WorkspaceDiagnostics() []source.Diagnostic {
	items := append([]source.Diagnostic(nil), s.workspaceDiagnostics...)
	for i := range items {
		items[i].Related = append([]source.RelatedDiagnostic(nil), items[i].Related...)
	}
	return items
}

func (s *Snapshot) Diagnostics(uri DocumentURI) []Diagnostic {
	items := append([]Diagnostic(nil), s.diagnostics[uri]...)
	for i := range items {
		items[i].RelatedInformation = append([]DiagnosticRelatedInformation(nil), items[i].RelatedInformation...)
	}
	return items
}

func (s *Snapshot) IsEditable(uri DocumentURI) bool { return s.editable[uri] }

func (e *Engine) Open(identity DocumentIdentity, version int, text string) error {
	return e.OpenContext(context.Background(), identity, version, text)
}

// OpenContext opens a document and atomically publishes its rebuilt snapshot.
func (e *Engine) OpenContext(ctx context.Context, identity DocumentIdentity, version int, text string) error {
	if err := e.checkEditable(identity); err != nil {
		return err
	}
	if !strings.HasSuffix(identity.Path, ".mgo") {
		return fmt.Errorf("source file %q must use .mgo", identity.Path)
	}
	return e.mutate(ctx, func(store *DocumentStore) error {
		return store.Open(identity, version, text)
	}, func(editable map[DocumentURI]bool) {
		editable[identity.URI] = true
	})
}

func (e *Engine) Change(uri DocumentURI, version int, changes []ContentChange) error {
	return e.ChangeContext(context.Background(), uri, version, changes)
}

// ChangeContext applies document changes and atomically publishes its rebuilt snapshot.
func (e *Engine) ChangeContext(ctx context.Context, uri DocumentURI, version int, changes []ContentChange) error {
	return e.mutate(ctx, func(store *DocumentStore) error {
		return store.Change(uri, version, changes)
	}, nil)
}

func (e *Engine) Close(uri DocumentURI) error {
	return e.CloseContext(context.Background(), uri)
}

// CloseContext closes a document and atomically publishes its rebuilt snapshot.
func (e *Engine) CloseContext(ctx context.Context, uri DocumentURI) error {
	return e.mutate(ctx, func(store *DocumentStore) error {
		if text, ok := e.base[uri]; ok {
			return store.Close(uri, text)
		}
		return store.Delete(uri)
	}, func(editable map[DocumentURI]bool) {
		if _, exists := e.base[uri]; !exists {
			delete(editable, uri)
		}
	})
}

func (e *Engine) rebuild(ctx context.Context) error {
	snapshot, err := e.buildSnapshot(ctx, e.store, e.editable)
	if err != nil {
		return err
	}
	e.snapshot = snapshot
	return nil
}

func (e *Engine) mutate(ctx context.Context, update func(*DocumentStore) error, updateEditable func(map[DocumentURI]bool)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store := e.store.clone()
	if err := update(store); err != nil {
		return err
	}
	editable := maps.Clone(e.editable)
	if updateEditable != nil {
		updateEditable(editable)
	}
	snapshot, err := e.buildSnapshot(ctx, store, editable)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.store, e.editable, e.snapshot = store, editable, snapshot
	return nil
}

func (e *Engine) buildSnapshot(ctx context.Context, store *DocumentStore, editableDocuments map[DocumentURI]bool) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	changes := make([]workspace.SourceChange, 0, len(store.Documents()))
	changedPackages := make(map[string]struct{})
	for _, document := range store.Documents() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		changes = append(changes, workspace.SourceChange{
			ModulePath: document.Identity.ModulePath,
			Path:       document.Identity.Path,
			Text:       document.Text,
		})
		if base, exists := e.base[document.Identity.URI]; !exists || base != document.Text {
			changedPackages[document.Identity.ModulePath] = struct{}{}
		}
	}
	var discoveryDiagnostics []source.Diagnostic
	sources, err := workspace.Overlay(e.baseSource, changes)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		discoveryDiagnostics = append(discoveryDiagnostics, source.Diagnostic{Code: "workspace.source.read", Severity: source.SeverityError, Message: err.Error()})
		empty, emptyErr := workspace.NewMemorySourceSet(nil)
		if emptyErr != nil {
			return nil, emptyErr
		}
		sources, err = workspace.Overlay(empty, changes)
		if err != nil {
			return nil, err
		}
	}
	packages := make(map[string]workspace.SourcePackage, len(changedPackages))
	for modulePath := range changedPackages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pkg, ok, err := sources.Package(modulePath)
		if err != nil {
			return nil, err
		}
		if ok {
			packages[modulePath] = pkg
		}
	}
	var previous *analysis.WorkspaceResult
	if e.snapshot != nil {
		previous = &e.snapshot.checked
	}
	checked, err := analysis.CheckWorkspace(analysis.WorkspaceRequest{
		Context: ctx,
		Root:    e.root,
		Sources: sources,
		Target:  e.target,
		Limits: compiler.Limits{
			MaxFiles:       e.limits.MaxFiles,
			MaxSourceBytes: e.limits.MaxFileBytes,
			MaxDiagnostics: e.limits.MaxDiagnostics,
		},
		Previous: previous,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		discoveryDiagnostics = append(discoveryDiagnostics, source.Diagnostic{Code: "workspace.analysis", Severity: source.SeverityError, Message: err.Error()})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	documentationPackages := make([]string, 0, len(checked.Packages))
	for modulePath := range checked.Packages {
		documentationPackages = append(documentationPackages, modulePath)
	}
	sort.Strings(documentationPackages)
	documentation, docErr := docgen.Build(docgen.BuildRequest{Workspace: checked, Packages: documentationPackages})
	if docErr != nil && !source.HasErrors(checked.Diagnostics) {
		return nil, docErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	generation := uint64(1)
	if e.snapshot != nil {
		generation = e.snapshot.generation + 1
	}
	snapshot := &Snapshot{
		workspaceDiagnostics: source.NormalizeDiagnostics(discoveryDiagnostics),
		generation:           generation,
		documents:            map[DocumentURI]Document{},
		packages:             checked.Packages,
		documentation:        documentation,
		diagnostics:          map[DocumentURI][]Diagnostic{},
		definitions:          map[string]Location{},
		occurrences:          map[DocumentURI][]indexedOccurrence{},
		references:           map[string][]Location{},
		resultIDs:            map[DocumentURI]string{},
		sourceHash:           map[string]string{},
		exportHash:           map[string]string{},
		dependencyHash:       map[string]string{},
		checkedPackages:      checked.Stats.PackagesAnalyzed,
		reusedPackages:       checked.Stats.PackageCacheHits,
		checked:              checked,
		editable:             maps.Clone(editableDocuments),
	}
	snapshot.renderComment = func(_ string, comment docgen.Comment) string { return comment.Text }
	if e.renderDocumentation != nil {
		snapshot.renderComment = e.renderDocumentation(documentation)
	}
	for _, document := range store.Documents() {
		snapshot.documents[document.Identity.URI] = document
	}
	// Dependency documents come from the analyzed closure, not the complete
	// library catalog. Keep them read-only and do not copy them into overlays.
	for modulePath, pkg := range checked.Packages {
		for _, parsed := range pkg.Documents {
			file := parsed.File
			uri := DocumentURI("mini-go://" + modulePath + "/" + file.Path)
			found := false
			for _, document := range snapshot.documents {
				if document.Identity.ModulePath == modulePath && document.Identity.Path == file.Path {
					found = true
					break
				}
			}
			if !found {
				snapshot.documents[uri] = Document{Identity: DocumentIdentity{URI: uri, ModulePath: modulePath, Path: file.Path}, Text: file.Text, Index: NewLineIndex(file.Text)}
			}
		}
	}
	for modulePath, hash := range checked.SourceHashes {
		snapshot.sourceHash[modulePath] = hash
	}
	for modulePath, hash := range checked.ExportHashes {
		snapshot.exportHash[modulePath] = hash
	}
	for modulePath, hash := range checked.DependencyHashes {
		snapshot.dependencyHash[modulePath] = hash
	}
	if len(snapshot.packages) == 0 {
		if len(changedPackages) == 0 {
			changedPackages[e.root] = struct{}{}
		}
		snapshot.packages = e.checkPartialPackages(ctx, packages, changedPackages)
		snapshot.partial = true
	}
	e.index(snapshot)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, diagnostic := range checked.Diagnostics {
		e.addDiagnostic(snapshot, diagnostic)
	}
	for uri, diagnostics := range snapshot.diagnostics {
		if e.limits.MaxDiagnostics > 0 && len(diagnostics) > e.limits.MaxDiagnostics {
			diagnostics = diagnostics[:e.limits.MaxDiagnostics]
			snapshot.workspaceDiagnostics = source.NormalizeDiagnostics(append(snapshot.workspaceDiagnostics, source.Diagnostic{Code: "workspace.diagnostic_limit", Severity: source.SeverityWarning, Message: "diagnostics truncated for " + string(uri)}))
		}
		snapshot.diagnostics[uri] = diagnostics
	}
	for uri, document := range snapshot.documents {
		var diagnosticText strings.Builder
		for _, diagnostic := range snapshot.diagnostics[uri] {
			fmt.Fprintf(&diagnosticText, "%#v\x00", diagnostic)
		}
		snapshot.resultIDs[uri] = fmt.Sprintf("%d:%s:%s", document.Version, source.HashText(document.Text), source.HashText(diagnosticText.String()))
	}
	return snapshot, nil
}

func (e *Engine) checkPartialPackages(ctx context.Context, packages map[string]workspace.SourcePackage, selected map[string]struct{}) map[string]analysis.Package {
	out := map[string]analysis.Package{}
	modulePaths := make([]string, 0, len(selected))
	for modulePath := range selected {
		modulePaths = append(modulePaths, modulePath)
	}
	sort.Strings(modulePaths)
	for _, modulePath := range modulePaths {
		if ctx.Err() != nil {
			return out
		}
		pkg, exists := packages[modulePath]
		if !exists {
			continue
		}
		selected, diagnostics, err := workspace.SelectPackage(pkg, e.target)
		if err != nil || source.HasErrors(diagnostics) {
			continue
		}
		parsed, _, err := workspace.ParsePackageWithLimits(selected, workspace.Limits{
			MaxFiles:       e.limits.MaxFiles,
			MaxSourceBytes: e.limits.MaxFileBytes,
			MaxDiagnostics: e.limits.MaxDiagnostics,
		})
		if err != nil {
			continue
		}
		out[modulePath] = analysis.Index(analysis.CheckProgram(parsed.Program, check.AnalyzeOptions{}))
	}
	return out
}
