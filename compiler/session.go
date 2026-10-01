// Package compiler checks Mini-Go source and prepares executable bytecode programs.
package compiler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

// Options fixes the immutable source and target context for a build session.
type Options struct {
	Sources          workspace.SourceSet
	Target           target.Target
	Cache            cache.Cache
	CacheVerify      bool
	CacheHash        bool
	TraceCache       func(cache.Event)
	TransientCache   cache.TransientConfig
	Limits           Limits
	HostCapabilities map[string][]string
	Optimization     OptimizationLevel
	Symbols          bool
	PreviousAnalysis *AnalysisResult
}

// Compiler reuses checked package graphs and test source selection across
// related sequential build actions. Returned results are immutable session-owned values.
type Compiler struct {
	options   Options
	workspace *workspace.Loader
	tests     *testSourceSnapshot
	testErr   error
}

// Close releases session-local graph, test snapshot, and transient cache state.
// Caller-provided caches remain owned by the caller.
func (s *Compiler) Close() error {
	if s == nil {
		return nil
	}
	if session, ok := s.options.Cache.(*sessionCache); ok {
		session.Close()
	}
	s.options = Options{}
	s.workspace = nil
	s.tests = nil
	s.testErr = errors.New("compiler session is closed")
	return nil
}

func New(options Options) (*Compiler, error) {
	if options.Sources == nil {
		return nil, errors.New("nil build session source set")
	}
	normalized, err := target.Normalize(options.Target)
	if err != nil {
		return nil, err
	}
	options.Target = normalized
	options.Limits = normalizeCompilerLimits(options.Limits)
	// A caller-owned structured cache already provides bounded memory storage.
	// Only persistent/custom backends need the session-local structured tier.
	if _, structured := options.Cache.(*cache.TransientCache); !structured {
		options.Cache = newSessionCache(options.Cache, options.TransientCache)
	}
	loader, err := workspace.NewLoader(options.Sources, options.Target, options.Limits.workspaceLimits())
	if err != nil {
		return nil, err
	}
	return &Compiler{options: options, workspace: loader}, nil
}

func (s *Compiler) request(ctx context.Context, root string, entries []EntryPoint) (Request, error) {
	if s == nil || s.workspace == nil {
		return Request{}, errors.New("compiler session is closed")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return Request{}, errors.New("build session requires a root module")
	}
	return Request{
		Context: requestContext(ctx),
		Root:    root, Target: s.options.Target, Sources: s.options.Sources,
		EntryPoints: append([]EntryPoint(nil), entries...), Cache: s.options.Cache,
		CacheVerify: s.options.CacheVerify, CacheHash: s.options.CacheHash,
		TraceCache: s.options.TraceCache,
		Limits:     s.options.Limits, HostCapabilities: cloneHostCapabilities(s.options.HostCapabilities),
		Optimization:     s.options.Optimization,
		Symbols:          s.options.Symbols,
		PreviousAnalysis: s.options.PreviousAnalysis,
		workspace:        s.workspace,
	}, nil
}

func cloneHostCapabilities(input map[string][]string) map[string][]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string][]string, len(input))
	for path, values := range input {
		out[path] = append([]string(nil), values...)
	}
	return out
}

func (s *Compiler) Check(root string) (CheckResult, error) {
	return s.CheckContext(context.Background(), root)
}

// CheckContext checks root and stops at stable compiler boundaries when ctx is canceled.
func (s *Compiler) CheckContext(ctx context.Context, root string) (CheckResult, error) {
	request, err := s.request(ctx, root, nil)
	if err != nil {
		return CheckResult{}, err
	}
	return Check(request)
}

// CheckPackages parses and semantically analyzes the union of the requested
// package graphs without lowering or emitting runtime artifacts.
func (s *Compiler) CheckPackages(roots []string) (CheckResult, error) {
	return s.CheckPackagesContext(context.Background(), roots)
}

// CheckPackagesContext checks the requested package graphs with cooperative cancellation.
func (s *Compiler) CheckPackagesContext(ctx context.Context, roots []string) (CheckResult, error) {
	if len(roots) == 0 {
		return CheckResult{}, errors.New("build session requires at least one package root")
	}
	normalized := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			return CheckResult{}, errors.New("package root is empty")
		}
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		normalized = append(normalized, root)
	}
	sort.Strings(normalized)
	request, err := s.request(ctx, normalized[0], nil)
	if err != nil {
		return CheckResult{}, err
	}
	return CheckPackages(request, normalized)
}

func (s *Compiler) Compile(root string) (Result, error) {
	return s.CompileContext(context.Background(), root)
}

// CompileContext compiles root with cooperative cancellation between compiler stages.
func (s *Compiler) CompileContext(ctx context.Context, root string) (Result, error) {
	request, err := s.request(ctx, root, nil)
	if err != nil {
		return Result{}, err
	}
	return Compile(request)
}

func (s *Compiler) Prepare(root string, entries []EntryPoint) (PrepareResult, error) {
	return s.PrepareContext(context.Background(), root, entries)
}

// PrepareContext compiles and links root with cooperative cancellation.
func (s *Compiler) PrepareContext(ctx context.Context, root string, entries []EntryPoint) (PrepareResult, error) {
	request, err := s.request(ctx, root, entries)
	if err != nil {
		return PrepareResult{}, err
	}
	return prepare(request, "prepare", nil)
}

// PrepareTests builds multiple test roots from one selected source snapshot.
func (s *Compiler) PrepareTests(roots []string) (map[string]PrepareResult, error) {
	return s.PrepareTestsContext(context.Background(), roots)
}

// PrepareTestsContext prepares test roots with cooperative cancellation.
func (s *Compiler) PrepareTestsContext(ctx context.Context, roots []string) (map[string]PrepareResult, error) {
	if len(roots) == 0 {
		return nil, errors.New("build session requires at least one test root")
	}
	normalized := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			return nil, errors.New("test root module is empty")
		}
		if _, ok := seen[root]; ok {
			return nil, fmt.Errorf("duplicate test root %q", root)
		}
		seen[root] = struct{}{}
		normalized = append(normalized, root)
	}
	sort.Strings(normalized)

	if s.tests == nil && s.testErr == nil {
		tests, err := loadTestSourceSnapshot(requestContext(ctx), s.options.Sources, s.options.Target, s.options.Limits)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			s.testErr = err
		} else {
			s.tests = tests
		}
	}
	tests, loadErr := s.tests, s.testErr
	if loadErr != nil {
		return nil, loadErr
	}

	results := make(map[string]PrepareResult, len(normalized))
	for _, root := range normalized {
		if err := requestContext(ctx).Err(); err != nil {
			return nil, err
		}
		request, err := s.request(ctx, root, nil)
		if err != nil {
			return nil, err
		}
		prepared, err := prepareTestWorkspace(request, tests)
		if err != nil {
			return nil, err
		}
		results[root] = prepared
	}
	return results, nil
}
