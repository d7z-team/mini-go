package stdlib_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/d7z-team/mini-go/compiler"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
	minigoruntime "github.com/d7z-team/mini-go/runtime"
)

func TestTimeFieldLayoutChangeRejectsPatchAndPreservesState(t *testing.T) {
	const root = "minigo.test/time-layout-patch"
	application, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{ModulePath: root, Files: []source.File{{Path: "main.mgo", Text: `package main
import "time"
var retained = time.Unix(123, 456)
func Main() int64 { return retained.Unix() }
`}}}})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := workspace.MergeSourceSets(application, testStandardLibrary(t))
	if err != nil {
		t.Fatal(err)
	}
	pkg, _, err := sources.Package("time")
	if err != nil {
		t.Fatal(err)
	}
	var changes []workspace.SourceChange
	for _, file := range pkg.Files {
		if strings.Contains(file.Text, "absSec") {
			changes = append(changes, workspace.SourceChange{ModulePath: "time", Path: file.Path, Text: strings.ReplaceAll(file.Text, "absSec", "sec")})
		}
	}
	renamedFields, err := workspace.Overlay(sources, changes)
	if err != nil {
		t.Fatal(err)
	}
	var programs []*minigoruntime.Program
	for _, input := range []workspace.SourceSet{renamedFields, sources} {
		prepared, err := compiler.Prepare(compiler.Request{Root: root, Sources: input, EntryPoints: []compiler.EntryPoint{{Name: "main", ModulePath: root, Function: "Main"}}})
		if err != nil || prepared.Image == nil || !prepared.Checked.OK() {
			t.Fatalf("prepare layout: %v %+v", err, prepared.Checked.Diagnostics)
		}
		program, err := minigoruntime.LoadExecutionImage(*prepared.Image)
		if err != nil {
			t.Fatal(err)
		}
		programs = append(programs, program)
	}
	instance, err := programs[0].Instantiate(context.Background(), minigoruntime.InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	if _, err := instance.Call(context.Background(), "main"); err != nil {
		t.Fatal(err)
	}
	_, err = instance.PreparePatch(context.Background(), programs[1])
	var patchError minigoruntime.PatchError
	if !errors.As(err, &patchError) {
		t.Fatalf("incompatible time layout accepted: %v", err)
	}
	result, err := instance.Call(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := result.Values[0].Int64(); !ok || value != 123 {
		t.Fatal("failed patch changed retained time")
	}
}

func TestTimerPendingAcrossPatchUsesCurrentNamedFunction(t *testing.T) {
	const root = "minigo.test/time-patch"
	const sourceText = `package main

import "time"

func Version() int { return VERSION }

func Wait() int {
	time.Sleep(time.Second)
	return Version()
}
`
	entries := []compiler.EntryPoint{{Name: "wait", ModulePath: root, Function: "Wait"}}
	oldProgram := prepareStdlibProgram(t, root, strings.ReplaceAll(sourceText, "VERSION", "1"), entries)
	newProgram := prepareStdlibProgram(t, root, strings.ReplaceAll(sourceText, "VERSION", "2"), entries)
	clock := minigoruntime.NewManualClock(time.Unix(1_700_000_000, 0))
	instance, err := oldProgram.Instantiate(context.Background(), minigoruntime.InstanceOptions{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := instance.Close(); err != nil {
			t.Errorf("close instance: %v", err)
		}
	})
	execution, err := instance.Start("wait")
	if err != nil {
		t.Fatal(err)
	}
	for execution.State() == minigoruntime.ExecutionRunning {
		if _, err := execution.Poll(); err != nil {
			t.Fatal(err)
		}
	}
	if execution.State() != minigoruntime.ExecutionPending {
		t.Fatalf("sleeping execution state = %s", execution.State())
	}
	plan, err := instance.PreparePatch(context.Background(), newProgram)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.ApplyPatch(plan); err != nil {
		t.Fatal(err)
	}
	if err := clock.Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	result, err := execution.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	value, ok := result.Values[0].Int64()
	if !ok || value != 2 {
		t.Fatalf("patched timer result = %#v", result.Values)
	}
	retained, err := instance.RetainedRevisions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != 1 || retained[0].Generation != 2 {
		t.Fatalf("completed timer retained old revision: %#v", retained)
	}

	cancelClock := minigoruntime.NewManualClock(time.Unix(1_800_000_000, 0))
	cancelInstance, err := oldProgram.Instantiate(context.Background(), minigoruntime.InstanceOptions{Clock: cancelClock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cancelInstance.Close(); err != nil {
			t.Errorf("close canceled instance: %v", err)
		}
	})
	cancelExecution, err := cancelInstance.Start("wait")
	if err != nil {
		t.Fatal(err)
	}
	for cancelExecution.State() == minigoruntime.ExecutionRunning {
		if _, err := cancelExecution.Poll(); err != nil {
			t.Fatal(err)
		}
	}
	if cancelExecution.State() != minigoruntime.ExecutionPending {
		t.Fatalf("cancel execution state = %s", cancelExecution.State())
	}
	cancelExecution.Cancel()
	if cancelClock.AdvanceToNext() {
		t.Fatal("execution cancellation retained its clock alarm")
	}
}
