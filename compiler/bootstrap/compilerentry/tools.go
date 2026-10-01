package compilerentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/language"
	"github.com/d7z-team/mini-go/compiler/service"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
	"github.com/d7z-team/mini-go/runtime/bytecode"
)

const (
	ToolsFormat   = "mini-go-tools"
	ToolsVersion  = 3
	MaxToolsInput = 64 << 20
)

type ToolsRequest struct {
	Format    string
	Version   int
	Operation string
	Session   string
	Revision  string
	Token     string
	Deadline  string
	Epoch     string
	Root      string
	Tags      []string
	Packages  []Package
	Changes   []language.DocumentUpdate
	Query     service.Query
	Build     service.BuildOptions
	Trees     []SourceTree
}

type ToolsResponse struct {
	Format      string
	Version     int
	CompilerID  string
	Session     string
	Revision    string
	Analysis    *service.Analysis
	Value       any
	ImageJSON   string
	SymbolsJSON string
	Sources     map[string]service.BuildSource
	Diagnostics []source.Diagnostic
	Error       *ToolsError
	Recovery    *ToolsRequest
}
type ToolsError struct {
	Code    string
	Message string
}
type ToolService struct {
	mu         sync.Mutex
	session    *service.Session
	generation uint64
	library    workspace.SourceSet
	workspace  ToolsRequest
	epoch      string
}

var portableTools ToolService

// Tools is the portable, stateful language/workspace/build entry point.
func Tools(input []byte) []byte { return portableTools.Call(input) }

func (s *ToolService) Call(input []byte) []byte {
	response := ToolsResponse{Format: ToolsFormat, Version: ToolsVersion, CompilerID: compiler.Identity()}
	var request ToolsRequest
	var err error
	if len(input) > MaxToolsInput {
		err = errors.New("tool input exceeds byte limit")
	} else {
		decoder := json.NewDecoder(bytes.NewReader(input))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&request)
		if err == nil {
			var trailing any
			if decoder.Decode(&trailing) != io.EOF {
				err = errors.New("trailing tool input")
			}
		}
	}
	if err == nil && (request.Format != ToolsFormat || request.Version != ToolsVersion) {
		err = errors.New("unsupported tools format or version")
	}
	if err == nil {
		ctx, finish, contextErr := toolRequestContext(request.Token, request.Deadline)
		if contextErr != nil {
			err = contextErr
		} else {
			response = s.Execute(ctx, request)
			finish()
		}
	}
	if err != nil {
		response.Error = &ToolsError{Code: "invalid_argument", Message: err.Error()}
	}
	output, encodeErr := encodeToolsResponse(response)
	if encodeErr != nil {
		code := "internal"
		if errors.Is(encodeErr, errToolsOutputBudget) {
			code = "budget"
		}
		output, _ = encodeToolsResponse(ToolsResponse{Format: ToolsFormat, Version: ToolsVersion, CompilerID: compiler.Identity(), Error: &ToolsError{Code: code, Message: encodeErr.Error()}})
	}
	return output
}

// Execute accepts a native context. Go integrations normally use service.Session
// directly; this entry also supplies the native reference for wire conformance.
func (s *ToolService) Execute(ctx context.Context, request ToolsRequest) ToolsResponse {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	response := ToolsResponse{Format: ToolsFormat, Version: ToolsVersion, CompilerID: compiler.Identity(), Session: s.sessionID()}
	err := ctx.Err()
	if err == nil {
		err = s.execute(ctx, request, &response)
	}
	if err == nil && (request.Operation == "workspace/open" || request.Operation == "workspace/update" || request.Operation == "document/update") {
		if request.Operation != "document/update" {
			s.workspace = request.workspaceSnapshot()
		}
		recovery := s.workspace.workspaceSnapshot()
		recovery.Changes, err = s.session.OpenDocuments()
		response.Recovery = &recovery
	}
	if err != nil {
		code := "invalid_argument"
		switch {
		case errors.Is(err, context.Canceled):
			code = "canceled"
		case errors.Is(err, context.DeadlineExceeded):
			code = "deadline"
		case errors.Is(err, service.ErrClosed):
			code = "closed"
		case errors.Is(err, service.ErrStale):
			code = "stale"
		}
		response.Error = &ToolsError{Code: code, Message: err.Error()}
	}
	return response
}

func (request ToolsRequest) workspaceSnapshot() ToolsRequest {
	owned := ToolsRequest{Format: ToolsFormat, Version: ToolsVersion, Operation: "workspace/open", Root: request.Root, Tags: append([]string(nil), request.Tags...), Packages: append([]Package(nil), request.Packages...)}
	for i := range owned.Packages {
		pkg := &owned.Packages[i]
		pkg.Files = append([]File(nil), pkg.Files...)
		pkg.TestFiles = append([]File(nil), pkg.TestFiles...)
		pkg.Resources = append([]Resource(nil), pkg.Resources...)
		for j := range pkg.Resources {
			pkg.Resources[j].Data = append([]byte(nil), pkg.Resources[j].Data...)
		}
		pkg.SelectionTarget.Tags = append([]string(nil), pkg.SelectionTarget.Tags...)
		pkg.SourceCandidates = append([]workspace.SourceCandidate(nil), pkg.SourceCandidates...)
		if pkg.Editable != nil {
			editable := *pkg.Editable
			pkg.Editable = &editable
		}
	}
	return owned
}

func (s *ToolService) execute(ctx context.Context, request ToolsRequest, response *ToolsResponse) error {
	if request.Operation == "hello" {
		response.Value = struct {
			Operations    []string
			MaxInputBytes int
		}{[]string{"workspace/open", "workspace/update", "workspace/analyze", "workspace/close", "language/*", "workspace/sources", "build/prepare"}, MaxToolsInput}
		return nil
	}
	if request.Operation == "workspace/sources" {
		packages, err := sourceTreePackages(ctx, request.Trees)
		response.Value = SourcePackages{Packages: packages}
		return err
	}
	if request.Operation == "workspace/open" || request.Operation == "workspace/update" {
		if request.Operation == "workspace/update" && (s.session == nil || request.Session != s.sessionID()) {
			return service.ErrStale
		}
		var modules, standard []Package
		for _, pkg := range request.Packages {
			if pkg.Namespace == "std" {
				standard = append(standard, pkg)
			} else {
				modules = append(modules, pkg)
			}
		}
		sources, err := newSourceSet(modules)
		if err != nil {
			return err
		}
		var editable []Package
		for _, pkg := range modules {
			allowed := pkg.ModulePath == request.Root || strings.HasPrefix(pkg.ModulePath, request.Root+"/")
			if pkg.Editable != nil {
				allowed = *pkg.Editable
			}
			if allowed {
				editable = append(editable, pkg)
			}
		}
		documents, err := newSourceSet(editable)
		if err != nil {
			return err
		}
		if s.library == nil {
			s.library, err = newSourceSet(embeddedCorePackages)
			if err != nil {
				return err
			}
		}
		library := s.library
		if len(standard) != 0 {
			extra, err := newSourceSet(standard)
			if err != nil {
				return err
			}
			library, err = workspace.ComposeStandardSourceSets(s.library, extra)
			if err != nil {
				return err
			}
		}
		all, err := workspace.MergeSourceSets(sources, library)
		if err != nil {
			return err
		}
		config := language.Config{Root: request.Root, Target: target.Target{Tags: request.Tags}, Sources: all, Documents: documents}
		uris := make(map[string]language.DocumentURI)
		for _, pkg := range request.Packages {
			for _, file := range append(append([]File(nil), pkg.Files...), pkg.TestFiles...) {
				if file.URI != "" {
					uris[pkg.ModulePath+"\x00"+file.Path] = language.DocumentURI(file.URI)
				}
			}
		}
		config.URI = func(module, path string) language.DocumentURI {
			if uri := uris[module+"\x00"+path]; uri != "" {
				return uri
			}
			return language.DocumentURI("mini-go://" + module + "/" + path)
		}
		if request.Operation == "workspace/open" {
			candidate, err := service.New(ctx, config)
			if err != nil {
				return err
			}
			if s.generation == ^uint64(0) {
				candidate.Close()
				return errors.New("session generation exhausted")
			}
			if len(request.Changes) != 0 {
				if _, err = candidate.Update(ctx, nil, request.Changes); err != nil {
					candidate.Close()
					return err
				}
			}
			if s.session != nil {
				s.session.Close()
			}
			s.session = candidate
			s.generation++
			s.epoch = request.Epoch
			response.Session = s.sessionID()
			response.Revision = "1"
			if len(request.Changes) != 0 {
				response.Revision = "2"
			}
		} else {
			response.Revision, err = s.session.Update(ctx, &config, request.Changes)
			if err != nil {
				return err
			}
		}
		return nil
	}
	if s.session == nil {
		return service.ErrClosed
	}
	if request.Session != s.sessionID() {
		return service.ErrStale
	}
	switch request.Operation {
	case "document/update":
		revision, err := s.session.Update(ctx, nil, request.Changes)
		response.Revision = revision
		return err
	case "workspace/analyze":
		analysis, err := s.session.Analyze(ctx, request.Revision)
		if err == nil {
			analysis.Snapshot = s.sessionID() + ":" + analysis.Snapshot
			response.Analysis = &analysis
			response.Revision = analysis.Revision
		}
		return err
	case "workspace/close":
		s.session.Close()
		s.session = nil
		return nil
	case "build/prepare":
		result, err := s.session.Build(ctx, request.Build)
		if err == nil && result.Result.Image != nil {
			encoded, encodeErr := bytecode.EncodeExecutionImage(result.Result.Image)
			if encodeErr != nil {
				return encodeErr
			}
			response.ImageJSON = string(encoded)
			if result.Result.Symbols != nil {
				encoded, encodeErr = bytecode.EncodeProgramSymbols(result.Result.Symbols)
				if encodeErr != nil {
					return encodeErr
				}
				response.SymbolsJSON = string(encoded)
			}
		}
		response.Diagnostics = result.Result.Checked.Diagnostics
		response.Revision = result.Revision
		response.Sources = result.Sources
		return err
	default:
		if strings.HasPrefix(request.Operation, "language/") {
			query := request.Query
			prefix := s.sessionID() + ":"
			if !strings.HasPrefix(query.Snapshot, prefix) {
				return service.ErrStale
			}
			query.Snapshot = strings.TrimPrefix(query.Snapshot, prefix)
			query.Operation = strings.TrimPrefix(request.Operation, "language/")
			dataPrefix := request.Query.Snapshot + "|"
			if query.Operation == "completion/resolve" && query.Completion.Data != "" {
				if !strings.HasPrefix(query.Completion.Data, dataPrefix) {
					return service.ErrStale
				}
				query.Completion.Data = strings.TrimPrefix(query.Completion.Data, dataPrefix)
			}
			if query.Operation == "codeAction/resolve" && query.Action.Data != "" {
				if !strings.HasPrefix(query.Action.Data, dataPrefix) {
					return service.ErrStale
				}
				query.Action.Data = strings.TrimPrefix(query.Action.Data, dataPrefix)
			}
			value, err := s.session.Query(ctx, query)
			switch result := value.(type) {
			case language.CompletionList:
				for i := range result.Items {
					if result.Items[i].Data != "" {
						result.Items[i].Data = dataPrefix + result.Items[i].Data
					}
				}
				value = result
			case language.CompletionItem:
				if result.Data != "" {
					result.Data = dataPrefix + result.Data
				}
				value = result
			case []language.CodeAction:
				for i := range result {
					if result[i].Data != "" {
						result[i].Data = dataPrefix + result[i].Data
					}
				}
				value = result
			case language.CodeAction:
				if result.Data != "" {
					result.Data = dataPrefix + result.Data
				}
				value = result
			}
			response.Value = value
			return err
		}
		return errors.New("unknown tools operation")
	}
}

func (s *ToolService) sessionID() string { return s.epoch + "/" + strconv.FormatUint(s.generation, 10) }
