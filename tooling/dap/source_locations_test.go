package dap

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/workspace"
	miniruntime "github.com/d7z-team/mini-go/runtime"
	protocol "github.com/google/go-dap"
)

func TestGenericMethodBreakpointUsesDeclarationSource(t *testing.T) {
	program := compileTestProgram(t, "package main\ntype Box struct{}\nfunc (b Box) Echo[T ~int](x T) T {\n value := x + 1\n return value\n}\nfunc main() { _ = Box{}.Echo(41) }\n")
	instance, err := program.Instantiate(t.Context(), miniruntime.InstanceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer instance.Close()
	breakpoints, err := instance.SetBreakpoints("example", "main.mgo", []int{5})
	if err != nil || len(breakpoints) != 1 || !breakpoints[0].Verified || breakpoints[0].Line != 5 {
		t.Fatalf("method breakpoint: %+v %v", breakpoints, err)
	}
	execution, err := instance.StartMain()
	if err != nil {
		t.Fatal(err)
	}
	defer execution.Cancel()
	state, _, err := execution.PollSteps(1000)
	if err != nil || state != miniruntime.ExecutionPaused {
		t.Fatalf("method pause: %v %v", state, err)
	}
	snapshot, err := execution.DebugSnapshot()
	if err != nil || len(snapshot.Frames) == 0 {
		t.Fatalf("method snapshot: %+v %v", snapshot, err)
	}
	var output bytes.Buffer
	session := NewSession(nil, &output, nil)
	session.initialized, session.instance, session.execution = true, instance, execution
	session.target = LaunchTarget{RootPath: t.TempDir(), ModulePath: "example"}
	_, err = session.handle(t.Context(), &protocol.StackTraceRequest{Request: request(1, "stackTrace"), Arguments: protocol.StackTraceArguments{ThreadId: int(snapshot.Frames[0].ThreadID)}})
	if err != nil {
		t.Fatal(err)
	}
	stack := readDAP[*protocol.StackTraceResponse](t, bufio.NewReader(&output))
	frame := stack.Body.StackFrames[0]
	if frame.Line != 5 || frame.Source == nil || !strings.Contains(frame.Name, "Echo") {
		t.Fatalf("method source frame: %+v", frame)
	}
	module, file, err := session.sourceIdentity(*frame.Source)
	if err != nil || module != "example" || file != "main.mgo" {
		t.Fatalf("method source identity: %s %s %v", module, file, err)
	}
}

func TestBreakpointUsesRegisteredLibraryIdentity(t *testing.T) {
	app, library := t.TempDir(), t.TempDir()
	filename := filepath.Join(library, "value.mgo")
	if err := os.WriteFile(filename, []byte("package rules\nfunc Value() int { return 42 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := workspace.LoadSources(t.Context(), app, "app", []workspace.DirectorySource{{Module: "rules", Directory: library}})
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{target: LaunchTarget{RootPath: app, ModulePath: "app", Locations: loaded.Locations}}
	module, file, err := session.sourceIdentity(protocol.Source{Path: filename})
	if err != nil || module != "rules" || file != "value.mgo" {
		t.Fatalf("identity: %q %q %v", module, file, err)
	}
	if _, _, err := session.sourceIdentity(protocol.Source{Path: filepath.Join(library, "missing.mgo")}); err == nil {
		t.Fatal("unregistered breakpoint accepted")
	}
}
