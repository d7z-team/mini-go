// Package compilerentry exposes the compiler's typed guest-callable bootstrap ABI.
package compilerentry

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

type File struct {
	Path       string
	Text       string
	OriginPath string
	URI        string
}

type Package struct {
	Editable         *bool
	Namespace        string
	PackagePath      string
	ModulePath       string
	Files            []File
	TestFiles        []File
	Resources        []Resource
	SelectionTarget  target.Target
	SourceCandidates []workspace.SourceCandidate
}

type Resource struct {
	Path       string
	SourcePath string
	Data       []byte
}

type Request struct {
	Format       string
	Version      int
	Operation    Operation
	Root         string
	Tags         []string
	Packages     []Package
	EntryPoints  []compiler.EntryPoint
	Optimization compiler.OptimizationLevel
	Symbols      bool
}

type Response struct {
	Format      string
	Version     int
	Image       *ir.ExecutionImage
	Symbols     *ir.ProgramSymbols
	Diagnostics []source.Diagnostic
	Tests       []cache.TestEntry
	CompilerID  string
	Stats       compiler.Stats
	CacheStats  cache.TransientStats
	Error       string
}

const (
	ServiceFormat  = "mini-go-compiler-service"
	ServiceVersion = 11

	OperationCheck       Operation = "check"
	OperationPrepare     Operation = "prepare"
	OperationPrepareTest Operation = "prepare_test"
)

type Operation string

type Service struct {
	mu         sync.Mutex
	cache      *cache.TransientCache
	analysis   *compiler.AnalysisResult
	library    workspace.SourceSet
	compilerID string
	closed     bool
}

func NewService(config cache.TransientConfig) *Service {
	return &Service{cache: cache.NewTransient(config), compilerID: compiler.Identity()}
}

var defaultService = NewService(cache.TransientConfig{MaxEntries: 128, MaxBytes: 256 << 20})

// Compile is the portable compiler service ABI exposed by the bootstrap image.
func Compile(input []byte) []byte {
	return defaultService.Compile(input)
}

func (s *Service) Compile(input []byte) []byte {
	request, requestError := decodeRequest(input)
	response := Response{Format: ServiceFormat, Version: ServiceVersion, CompilerID: compiler.Identity(), Error: requestError}
	if requestError == "" {
		response = s.Execute(request)
	}
	output, err := encodeResponse(response)
	if err != nil {
		return []byte(`{"Format":"mini-go-compiler-service","Version":11,"Error":"encode compiler response"}`)
	}
	return output
}

func decodeRequest(input []byte) (Request, string) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Request{}, "invalid compiler request JSON"
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Request{}, "compiler request contains trailing JSON"
	}
	if request.Format != ServiceFormat || request.Version != ServiceVersion {
		return Request{}, "unsupported compiler request format or version"
	}
	return request, ""
}

func Execute(request Request) Response {
	return defaultService.Execute(request)
}

func (s *Service) Execute(request Request) Response {
	if s == nil {
		return Response{Format: ServiceFormat, Version: ServiceVersion, CompilerID: compiler.Identity(), Error: "nil compiler service"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Response{Format: ServiceFormat, Version: ServiceVersion, CompilerID: compiler.Identity(), Error: "compiler service is closed"}
	}
	compilerID := compiler.Identity()
	if s.library == nil || s.compilerID != compilerID {
		library, err := newSourceSet(embeddedCorePackages)
		if err != nil {
			return Response{Format: ServiceFormat, Version: ServiceVersion, CompilerID: compilerID, Error: err.Error()}
		}
		s.library = library
	}
	if s.compilerID != compilerID {
		s.cache.Clear()
		s.analysis = nil
		s.compilerID = compilerID
	}
	before := s.cache.Stats()
	response := s.executeRequest(request)
	response.Format = ServiceFormat
	response.Version = ServiceVersion
	response.CompilerID = compilerID
	response.CacheStats = transientStatsDelta(before, s.cache.Stats())
	return response
}

func (s *Service) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cache.Close()
		s.analysis = nil
		s.library = nil
	}
	s.mu.Unlock()
}

func transientStatsDelta(before, after cache.TransientStats) cache.TransientStats {
	after.Hits -= before.Hits
	after.Misses -= before.Misses
	after.Stores -= before.Stores
	after.Evictions -= before.Evictions
	after.Clears -= before.Clears
	return after
}

func (s *Service) executeRequest(request Request) Response {
	sources, err := newSourceSet(request.Packages)
	if err != nil {
		return Response{Error: err.Error()}
	}
	allSources, err := workspace.MergeSourceSets(sources, s.library)
	if err != nil {
		return Response{Error: err.Error()}
	}
	if request.Operation == OperationCheck {
		analyzed, err := compiler.Analyze(compiler.AnalysisRequest{
			Request: compiler.Request{Root: request.Root, Sources: allSources, Target: target.Target{Tags: append([]string(nil), request.Tags...)}},
			Roots:   []string{request.Root}, Previous: s.analysis,
		})
		if err != nil {
			return Response{Error: err.Error()}
		}
		// A service owns only its most recent bounded analysis. Source and AST
		// limits prevent a large workspace from pinning its semantic graph.
		sourceBytes, nodes := 0, 0
		for _, pkg := range analyzed.Packages {
			for _, document := range pkg.Documents {
				sourceBytes += len(document.File.Text)
				nodes += document.NodeCount
			}
		}
		if len(analyzed.Packages) <= 64 && sourceBytes <= 1<<20 && nodes <= 100_000 {
			s.analysis = &analyzed
		} else {
			s.analysis = nil
		}
		return Response{Diagnostics: analyzed.Diagnostics, Stats: analyzed.Stats}
	}
	frontend, err := compiler.New(compiler.Options{
		Sources:          allSources,
		Target:           target.Target{Tags: append([]string(nil), request.Tags...)},
		Cache:            s.cache,
		Optimization:     request.Optimization,
		Symbols:          request.Symbols,
		PreviousAnalysis: s.analysis,
	})
	if err != nil {
		return Response{Error: err.Error()}
	}
	defer frontend.Close()
	switch request.Operation {
	case OperationPrepareTest:
		prepared, err := frontend.PrepareTests([]string{request.Root})
		if err != nil {
			return Response{Error: err.Error()}
		}
		result := prepared[request.Root]
		return Response{Image: result.Image, Symbols: result.Symbols, Diagnostics: result.Checked.Diagnostics, Tests: result.TestManifest, Stats: result.Checked.Stats}
	case OperationPrepare:
		prepared, err := frontend.Prepare(request.Root, request.EntryPoints)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{Image: prepared.Image, Symbols: prepared.Symbols, Diagnostics: prepared.Checked.Diagnostics, Stats: prepared.Checked.Stats}
	default:
		return Response{Error: "unsupported compiler operation " + string(request.Operation)}
	}
}
