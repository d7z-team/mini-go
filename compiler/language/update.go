package language

import (
	"context"
	"errors"
	"maps"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

// DocumentUpdate is one ordered input operation in an atomic batch.
type DocumentUpdate struct {
	Operation string
	Identity  DocumentIdentity
	Version   int
	Text      string
	Changes   []ContentChange
}

func (e *Engine) OpenDocuments() []DocumentUpdate {
	var result []DocumentUpdate
	for _, document := range e.store.Documents() {
		if document.Open {
			result = append(result, DocumentUpdate{Operation: "open", Identity: document.Identity, Version: document.Version, Text: document.Text})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Identity.URI < result[j].Identity.URI })
	return result
}

// Fork shares immutable analysis facts and owns its mutable document state.
// A candidate may be analyzed without changing the published engine.
func (e *Engine) Fork() *Engine {
	candidate := *e
	candidate.store = e.store.clone()
	candidate.base = maps.Clone(e.base)
	candidate.editable = maps.Clone(e.editable)
	return &candidate
}

// ApplyDocuments accepts input without analyzing it. Edits in a batch preserve
// their order; any invalid operation leaves all input unchanged.
func (e *Engine) ApplyDocuments(updates []DocumentUpdate) error {
	candidate := e.Fork()
	for _, update := range updates {
		uri := update.Identity.URI
		var err error
		switch update.Operation {
		case "open":
			if err := candidate.checkEditable(update.Identity); err != nil {
				return err
			}
			if document, ok := candidate.store.Document(uri); ok && document.Open {
				return errors.New("document is already open")
			}
			err = candidate.store.Open(update.Identity, update.Version, update.Text)
			candidate.editable[uri] = true
		case "change":
			document, ok := candidate.store.Document(uri)
			if !ok || !document.Open {
				return ErrDocumentNotFound
			}
			err = candidate.store.Change(uri, update.Version, update.Changes)
		case "close":
			document, ok := candidate.store.Document(uri)
			if !ok || !document.Open {
				return ErrDocumentNotFound
			}
			if text, ok := candidate.base[uri]; ok {
				err = candidate.store.Close(uri, text)
			} else {
				err = candidate.store.Delete(uri)
				delete(candidate.editable, uri)
			}
		default:
			return errors.New("invalid document operation")
		}
		if err != nil {
			return err
		}
	}
	e.store, e.editable = candidate.store, candidate.editable
	return nil
}

func (e *Engine) checkEditable(identity DocumentIdentity) error {
	for uri, document := range e.store.documents {
		if document.Identity.ModulePath == identity.ModulePath && e.editable[uri] {
			return nil
		}
	}
	_, found, err := e.baseSource.Package(identity.ModulePath)
	if err != nil {
		return err
	}
	if !found && (identity.ModulePath == e.root || strings.HasPrefix(identity.ModulePath, e.root+"/")) {
		return nil
	}
	return errors.New("document belongs to a read-only dependency")
}

func (e *Engine) Analyze(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return e.rebuild(ctx)
}

// Sources owns a complete overlay view, including test files and resources.
func (e *Engine) Sources() (workspace.SourceSet, error) {
	documents := e.store.Documents()
	changes := make([]workspace.SourceChange, 0, len(documents))
	for _, document := range documents {
		changes = append(changes, workspace.SourceChange{ModulePath: document.Identity.ModulePath, Path: document.Identity.Path, Text: document.Text})
	}
	return workspace.Overlay(e.baseSource, changes)
}

// Prepare builds the supplied source revision under this engine's limits.
// The compiler reuses immutable analysis only when the complete graph, target
// and limits match; changed or partially analyzed inputs take the normal path.
func (e *Engine) Prepare(request compiler.Request) (compiler.PrepareResult, error) {
	if request.Limits.MaxFiles <= 0 || request.Limits.MaxFiles > e.limits.MaxFiles {
		request.Limits.MaxFiles = e.limits.MaxFiles
	}
	if request.Limits.MaxSourceBytes <= 0 || request.Limits.MaxSourceBytes > e.limits.MaxFileBytes {
		request.Limits.MaxSourceBytes = e.limits.MaxFileBytes
	}
	if request.Limits.MaxDiagnostics <= 0 || request.Limits.MaxDiagnostics > e.limits.MaxDiagnostics {
		request.Limits.MaxDiagnostics = e.limits.MaxDiagnostics
	}
	if e.snapshot != nil && !e.snapshot.partial {
		request.PreviousAnalysis = &e.snapshot.checked.AnalysisResult
	}
	return compiler.Prepare(request)
}

// ReplaceWorkspace prepares a new base while retaining open buffers. The
// previous facts are the incremental baseline; analysis is a separate operation.
func (e *Engine) ReplaceWorkspace(ctx context.Context, config Config) (*Engine, error) {
	if config.RenderDocumentation == nil {
		config.RenderDocumentation = e.renderDocumentation
	}
	if config.FormatSource == nil {
		config.FormatSource = e.formatSource
	}
	candidate, err := newEngine(ctx, config, false)
	if err != nil {
		return nil, err
	}
	for _, document := range e.store.Documents() {
		if document.Open {
			if err := candidate.store.Open(document.Identity, document.Version, document.Text); err != nil {
				return nil, err
			}
			candidate.editable[document.Identity.URI] = true
		}
	}
	candidate.snapshot = e.snapshot
	return candidate, nil
}
