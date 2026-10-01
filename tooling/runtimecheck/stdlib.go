package runtimecheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/cache"
	"github.com/d7z-team/mini-go/compiler/workspace"
	"github.com/d7z-team/mini-go/runtime"
	"github.com/d7z-team/mini-go/runtime/bytecode"
	"github.com/d7z-team/mini-go/stdlib"
)

// StdlibVector contains one current package's complete _test.mgo program.
type StdlibVector struct {
	Name  string                   `json:"name"`
	Image *bytecode.ExecutionImage `json:"image"`
	Tests []string                 `json:"tests"`
}

// GenerateStdlibVectors compiles every standard package with test sources and
// verifies its report against actual Go host providers before publication.
func GenerateStdlibVectors() ([]byte, error) {
	sources, err := workspace.StandardLibrary(stdlib.Open())
	if err != nil {
		return nil, err
	}
	paths, err := sources.PackagePaths()
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, path := range paths {
		pkg, ok, err := sources.Package(path)
		if err != nil {
			return nil, err
		}
		if ok && len(pkg.TestFiles) != 0 {
			roots = append(roots, path)
		}
	}
	cacheRoot, err := cache.ResolveDiskRoot("")
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	output.WriteByte('[')
	for index, root := range roots {
		compilerSession, err := compiler.New(compiler.Options{Sources: sources, Cache: cache.New(cache.NewDiskBackend(cacheRoot))})
		if err != nil {
			return nil, err
		}
		prepared, err := compilerSession.PrepareTests([]string{root})
		if err = errors.Join(err, compilerSession.Close()); err != nil {
			return nil, err
		}
		item := prepared[root]
		if !item.Checked.OK() || item.Image == nil {
			return nil, fmt.Errorf("stdlib %s: %v", root, item.Checked.Diagnostics)
		}
		program, err := runtime.LoadExecutionImage(*item.Image)
		if err != nil {
			return nil, err
		}
		host, err := newStandardHost()
		if err != nil {
			return nil, err
		}
		clock := runtime.NewManualClock(time.Unix(1_700_000_000, 123_456_789))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		instance, err := program.Instantiate(ctx, runtime.InstanceOptions{Clock: clock, FFI: host, Entropy: bytes.NewReader(bytes.Repeat([]byte{1, 2, 3, 4}, 1024))})
		if err != nil {
			cancel()
			host.Close()
			return nil, err
		}
		execution, runErr := instance.StartEntry()
		var report runtime.RunResult
		if runErr == nil {
			for {
				if err := ctx.Err(); err != nil {
					execution.Cancel()
					runErr = err
					break
				}
				state, _, pollErr := execution.PollSteps(4096)
				if pollErr != nil && !errors.Is(pollErr, runtime.ErrBusy) {
					runErr = pollErr
					break
				}
				if state == runtime.ExecutionCompleted || state == runtime.ExecutionFailed || state == runtime.ExecutionCanceled {
					report, runErr = execution.Result()
					break
				}
				if state == runtime.ExecutionPaused {
					runErr = errors.New("stdlib execution paused")
					break
				}
				if state == runtime.ExecutionPending && clock.AdvanceToNext() {
					continue
				}
				if state == runtime.ExecutionPending || errors.Is(pollErr, runtime.ErrBusy) {
					time.Sleep(100 * time.Microsecond)
				}
			}
		}
		closeErr := instance.Close()
		hostErr := host.Close()
		cancel()
		if err := errors.Join(runErr, closeErr, hostErr); err != nil {
			return nil, fmt.Errorf("stdlib %s: %w", root, err)
		}
		if len(report.Values) != 1 {
			return nil, fmt.Errorf("stdlib %s: invalid report count", root)
		}
		fields, _ := report.Values[0].Fields()
		passed := false
		for _, field := range fields {
			if field.Name == "Passed" {
				passed, _ = field.Value.Bool()
			}
		}
		if !passed {
			return nil, fmt.Errorf("stdlib %s: test report failed: %#v", root, fields)
		}
		vector := StdlibVector{Name: root, Image: item.Image}
		for _, test := range item.TestManifest {
			vector.Tests = append(vector.Tests, test.Name)
		}
		encoded, err := encodeJSON(vector)
		if err != nil {
			return nil, err
		}
		if index != 0 {
			output.WriteByte(',')
		}
		output.Write(bytes.TrimSuffix(encoded, []byte{'\n'}))
	}
	output.WriteString("]\n")
	return output.Bytes(), nil
}
