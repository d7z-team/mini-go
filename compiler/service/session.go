// Package service owns native and guest compiler tool sessions.
package service

import (
	"context"
	"errors"
	"strconv"
	"sync"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/language"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/workspace"
)

var (
	ErrClosed = errors.New("tool session closed")
	ErrStale  = errors.New("stale workspace revision or snapshot")
)

type Analysis struct {
	Revision             string
	Snapshot             string
	CheckedPackages      int
	ReusedPackages       int
	Diagnostics          map[language.DocumentURI]language.DiagnosticReport
	WorkspaceDiagnostics []source.Diagnostic
}

type BuildOptions struct {
	Revision     string
	EntryPoints  []compiler.EntryPoint
	Optimization compiler.OptimizationLevel
	Symbols      bool
}

type BuildResult struct {
	Revision string
	Target   target.Target
	Result   compiler.PrepareResult
	Sources  map[string]BuildSource
}

// BuildSource binds debugger content to the exact prepared input revision.
type BuildSource struct {
	Module string `json:"module"`
	Path   string `json:"path"`
	Text   string `json:"text"`
}

// Session has one input owner and one published immutable engine. Long analysis
// and build operations run outside the lock and publish only their own revision.
type Session struct {
	mu               sync.Mutex
	config           language.Config
	input            *language.Engine
	published        *language.Engine
	revision         uint64
	snapshot         uint64
	analyzedRevision uint64
	cache            *cache.TransientCache
	closed           bool
	lifetime         context.Context
	cancel           context.CancelFunc
	active           sync.WaitGroup
	done             chan struct{}
}

func New(ctx context.Context, config language.Config) (*Session, error) {
	buildTarget, err := target.Normalize(config.Target)
	if err != nil {
		return nil, err
	}
	config.Target = buildTarget
	engine, err := language.NewEngineContext(ctx, config)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &Session{
		config: config, input: engine, published: engine,
		revision: 1, snapshot: 1, analyzedRevision: 1,
		cache:    cache.NewTransient(cache.TransientConfig{MaxEntries: 128, MaxBytes: 128 << 20}),
		lifetime: lifetime, cancel: cancel, done: make(chan struct{}),
	}, nil
}

func (s *Session) Update(ctx context.Context, config *language.Config, changes []language.DocumentUpdate) (string, error) {
	ctx, finish, beginErr := s.beginRequest(ctx)
	if beginErr != nil {
		return "", beginErr
	}
	defer finish()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", ErrClosed
	}
	revision := s.revision
	candidate := s.input.Fork()
	s.mu.Unlock()
	var err error
	if config != nil {
		owned := *config
		owned.Target, err = target.Normalize(config.Target)
		if err != nil {
			return "", err
		}
		config = &owned
		candidate, err = candidate.ReplaceWorkspace(ctx, *config)
	}
	if err == nil {
		err = candidate.ApplyDocuments(changes)
	}
	if err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", ErrClosed
	}
	if s.revision != revision {
		return "", ErrStale
	}
	if s.revision == ^uint64(0) {
		return "", errors.New("revision exhausted")
	}
	s.revision++
	s.input = candidate
	if config != nil {
		s.config = *config
	}
	return strconv.FormatUint(s.revision, 10), nil
}

func (s *Session) Analyze(ctx context.Context, revision string) (Analysis, error) {
	ctx, finish, beginErr := s.beginRequest(ctx)
	if beginErr != nil {
		return Analysis{}, beginErr
	}
	defer finish()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Analysis{}, ErrClosed
	}
	current := s.revision
	if revision != strconv.FormatUint(current, 10) {
		s.mu.Unlock()
		return Analysis{}, ErrStale
	}
	already := s.analyzedRevision == current
	var candidate *language.Engine
	if !already {
		candidate = s.input.Fork()
	}
	s.mu.Unlock()
	if !already {
		if err := candidate.Analyze(ctx); err != nil {
			return Analysis{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Analysis{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Analysis{}, ErrClosed
	}
	if current != s.revision {
		return Analysis{}, ErrStale
	}
	if s.analyzedRevision != current {
		if s.snapshot == ^uint64(0) {
			return Analysis{}, errors.New("snapshot exhausted")
		}
		s.snapshot++
		s.input, s.published = candidate, candidate
		s.analyzedRevision = current
	}
	snapshot := s.published.Snapshot()
	checked, reused := snapshot.ReuseStats()
	result := Analysis{
		Revision: revision, Snapshot: strconv.FormatUint(s.snapshot, 10),
		CheckedPackages: checked, ReusedPackages: reused,
		Diagnostics:          make(map[language.DocumentURI]language.DiagnosticReport),
		WorkspaceDiagnostics: snapshot.WorkspaceDiagnostics(),
	}
	for uri := range snapshot.Documents() {
		if snapshot.IsEditable(uri) {
			result.Diagnostics[uri] = s.published.Diagnostics(uri, "")
		}
	}
	return result, nil
}

func (s *Session) Build(ctx context.Context, options BuildOptions) (BuildResult, error) {
	ctx, finish, beginErr := s.beginRequest(ctx)
	if beginErr != nil {
		return BuildResult{}, beginErr
	}
	defer finish()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return BuildResult{}, ErrClosed
	}
	if options.Revision != strconv.FormatUint(s.revision, 10) {
		s.mu.Unlock()
		return BuildResult{}, ErrStale
	}
	engine, config, buildCache := s.input.Fork(), s.config, s.cache
	s.mu.Unlock()
	sources, err := engine.Sources()
	if err != nil {
		return BuildResult{}, err
	}
	result, err := engine.Prepare(compiler.Request{
		Context: ctx, Root: config.Root, Target: config.Target, Sources: sources,
		Cache: buildCache, EntryPoints: options.EntryPoints,
		Optimization: options.Optimization, Symbols: options.Symbols,
	})
	build := BuildResult{
		Revision: options.Revision,
		Target:   target.Target{Tags: append([]string(nil), config.Target.Tags...)},
		Result:   result, Sources: make(map[string]BuildSource),
	}
	for _, module := range result.Checked.Order {
		pkg, found, sourceErr := sources.Package(module)
		if sourceErr != nil {
			return BuildResult{}, sourceErr
		}
		if !found {
			continue
		}
		pkg, _, sourceErr = workspace.SelectPackage(pkg, config.Target)
		if sourceErr != nil {
			return BuildResult{}, sourceErr
		}
		for _, file := range pkg.Files {
			uri := "mini-go://" + module + "/" + file.Path
			if config.URI != nil {
				uri = string(config.URI(module, file.Path))
			}
			build.Sources[uri] = BuildSource{Module: module, Path: file.Path, Text: file.Text}
		}
	}
	return build, err
}

func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.done
		return
	}
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	s.active.Wait()
	s.mu.Lock()
	s.input = nil
	s.published = nil
	s.cache.Close()
	close(s.done)
	s.mu.Unlock()
}

func (s *Session) beginRequest(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	request, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.lifetime, cancel)
	s.active.Add(1)
	return request, func() { stop(); cancel(); s.active.Done() }, nil
}

// OpenDocuments returns owned final buffer contents for bounded host recovery.
func (s *Session) OpenDocuments() ([]language.DocumentUpdate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	return s.input.OpenDocuments(), nil
}
