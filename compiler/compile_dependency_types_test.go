package compiler

import (
	"reflect"
	"testing"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileWorkspaceLowersImportedPointerReceiverCall(t *testing.T) {
	result, err := compileTestWorkspace([]SourcePackage{{
		ModulePath: "example/lib",
		Files: []SourceFile{{Path: "lib.mgo", Text: `package lib

type Inst struct { Value int }
type Program struct { Inst []Inst }

func (item *Inst) Equal(value int) bool { return item.Value == value }
func New(value int) *Program { return &Program{Inst: []Inst{{Value: value}}} }
`}},
	}, {
		ModulePath: "example/main",
		Files: []SourceFile{{Path: "main.mgo", Text: `package main

import "example/lib"

type wrapper struct { program *lib.Program }

func Main() bool {
	value := &wrapper{program: lib.New(42)}
	goto Check
Check:
	item := &value.program.Inst[uint32(0)]
	return item.Equal(42)
}
`}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK() {
		t.Fatalf("compile diagnostics = %#v", result.Diagnostics)
	}
	artifact, ok := result.Artifact("example/main")
	if !ok {
		t.Fatal("missing main artifact")
	}
	for _, function := range artifact.Functions {
		if function.ID != "fn.Main" {
			continue
		}
		for _, instruction := range functionOperations(t, function) {
			if instruction.Op != ir.OpCallDirect && instruction.Op != ir.OpTailCallDirect {
				continue
			}
			var call ir.CallPayload
			if err := ir.ReadInstructionPayload(instruction.Payload, &call); err != nil {
				t.Fatal(err)
			}
			if call.ModulePath == "example/lib" && call.Function == "method.Ptr<Inst>.Equal" {
				return
			}
		}
		t.Fatalf("Main did not call imported pointer method: %#v", functionOperations(t, function))
	}
	t.Fatal("missing Main function")
}

func TestCompileWorkspaceExportsTypeMetadataForDependencies(t *testing.T) {
	result, err := compileTestWorkspace([]SourcePackage{{
		ModulePath: "example/main",
		Files: []SourceFile{{
			Path: "main.mgo",
			Text: `
package main

import "example/lib"

type Local interface {
	Add(delta int64) int64
}

func Main() int64 {
	value := lib.New(40)
	var local Local = value
	var imported lib.Adder = value
	if local.Add(2) == 42 && imported.Add(2) == 42 {
		return 42
	}
	return 0
}
`,
		}},
	}, {
		ModulePath: "example/lib",
		Files: []SourceFile{{
			Path: "lib.mgo",
			Text: `
package lib

type Counter struct {
	Value int64
}

type Adder interface {
	Add(delta int64) int64
}

func (c Counter) Add(delta int64) int64 {
	return c.Value + delta
}

func New(value int64) Counter {
	return Counter{Value: value}
}
`,
		}},
	}})
	if err != nil {
		t.Fatalf("compileTestWorkspace failed: %v", err)
	}
	if !result.OK() {
		t.Fatalf("expected no diagnostics, got %#v", result.Diagnostics)
	}
	mainArtifact, ok := result.Artifact("example/main")
	if !ok {
		t.Fatalf("expected main artifact")
	}
	libArtifact, ok := result.Artifact("example/lib")
	if !ok {
		t.Fatalf("expected lib artifact")
	}
	if len(mainArtifact.Requirements) != 1 || mainArtifact.Requirements[0].ModulePath != "example/lib" {
		t.Fatalf("expected lib requirement, got %#v", mainArtifact.Requirements)
	}
	wantExports := []string{"Adder", "Counter", "New"}
	if len(mainArtifact.Requirements[0].Exports) != len(wantExports) {
		t.Fatalf("unexpected dependency exports: got %#v, want %#v", mainArtifact.Requirements[0].Exports, wantExports)
	}
	for i := range wantExports {
		if mainArtifact.Requirements[0].Exports[i] != wantExports[i] {
			t.Fatalf("unexpected dependency exports: got %#v, want %#v", mainArtifact.Requirements[0].Exports, wantExports)
		}
	}
	counter, ok := artifactType(libArtifact, "Counter")
	if !ok {
		t.Fatalf("expected Counter type metadata")
	}
	if len(counter.Methods) != 1 || counter.Methods[0].Name != "Add" || types.FormatWithTable(&libArtifact.TypeTable, counter.Methods[0].Receiver) != "example/lib.Counter" || types.FormatSignature(&libArtifact.TypeTable, counter.Methods[0].Signature) != "function(Int64) Int64" || counter.Methods[0].FunctionID != "method.Counter.Add" {
		t.Fatalf("unexpected Counter methods: %#v", counter.Methods)
	}
	data, err := ir.EncodeJSON(&libArtifact)
	if err != nil {
		t.Fatalf("EncodeJSON lib artifact failed: %v", err)
	}
	decoded, err := ir.DecodeJSON(data)
	if err != nil {
		t.Fatalf("DecodeJSON lib artifact failed: %v", err)
	}
	decodedCounter, ok := artifactType(decoded, "Counter")
	if !ok || len(decodedCounter.Methods) != 1 || !reflect.DeepEqual(decodedCounter.Methods[0], counter.Methods[0]) {
		t.Fatalf("unexpected decoded Counter metadata: %#v", decodedCounter)
	}
}

func TestCompileWorkspaceAssignsImportedPointerReceiverToImportedInterface(t *testing.T) {
	result, err := compileTestWorkspace([]SourcePackage{{
		ModulePath: "example/main",
		Files: []SourceFile{{
			Path: "main.mgo",
			Text: `
package main

import "example/buf"
import "example/io"

func Use(w io.Writer) int {
	n, err := w.Write([]byte("go"))
	if err != nil {
		return 0
	}
	return n
}

func Main() int {
	var b buf.Buffer
	return Use(&b)
}
`,
		}},
	}, {
		ModulePath: "example/io",
		Files: []SourceFile{{
			Path: "io.mgo",
			Text: `
package io

type Writer interface {
	Write([]byte) (int, error)
}
`,
		}},
	}, {
		ModulePath: "example/buf",
		Files: []SourceFile{{
			Path: "buf.mgo",
			Text: `
package buf

type Buffer struct {
	Data []byte
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.Data = append(b.Data, p...)
	return len(p), nil
}
`,
		}},
	}})
	if err != nil {
		t.Fatalf("compileTestWorkspace failed: %v", err)
	}
	if !result.OK() {
		t.Fatalf("expected no diagnostics, got %#v", result.Diagnostics)
	}
}

func TestCompileWorkspaceAllowsNilComparisonForImportedInterface(t *testing.T) {
	result, err := compileTestWorkspace([]SourcePackage{{
		ModulePath: "example/main",
		Files: []SourceFile{{
			Path: "main.mgo",
			Text: `
package main

import "example/io"

func Use(w io.Writer) bool {
	return w == nil
}
`,
		}},
	}, {
		ModulePath: "example/io",
		Files: []SourceFile{{
			Path: "io.mgo",
			Text: `
package io

type Writer interface {
	Write([]byte) (int, error)
}
`,
		}},
	}})
	if err != nil {
		t.Fatalf("compileTestWorkspace failed: %v", err)
	}
	if !result.OK() {
		t.Fatalf("expected imported interface nil comparison to compile, got %#v", result.Diagnostics)
	}
}

func TestCompileWorkspaceRejectsImportedUnexportedMethodAsLocalInterfaceImplementation(t *testing.T) {
	result, err := compileTestWorkspace([]SourcePackage{{
		ModulePath: "example/main",
		Files: []SourceFile{{
			Path: "main.mgo",
			Text: `
package main

import "example/lib"

type localHidden interface {
	hidden() int
}

func Main() int {
	value := lib.New()
	var hidden localHidden = value
	_ = hidden
	return 0
}
`,
		}},
	}, {
		ModulePath: "example/lib",
		Files: []SourceFile{{
			Path: "lib.mgo",
			Text: `
package lib

type Token struct{}

func (t Token) hidden() int {
	return 1
}

func New() Token {
	return Token{}
}
`,
		}},
	}})
	if err != nil {
		t.Fatalf("compileTestWorkspace failed: %v", err)
	}
	if result.OK() {
		t.Fatalf("expected diagnostics")
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == "semantic.assign.type" {
			return
		}
	}
	t.Fatalf("expected assignment diagnostic, got %#v", result.Diagnostics)
}

func TestCompileWorkspaceRejectsImportedUnexportedInterfaceMethodExpression(t *testing.T) {
	result, err := compileTestWorkspace([]SourcePackage{{
		ModulePath: "example/main",
		Files: []SourceFile{{
			Path: "main.mgo",
			Text: `
package main

import "example/lib"

func Main() int {
	_ = lib.Sealed.hidden
	return 0
}
`,
		}},
	}, {
		ModulePath: "example/lib",
		Files: []SourceFile{{
			Path: "lib.mgo",
			Text: `
package lib

type Sealed interface {
	hidden() int
}
`,
		}},
	}})
	if err != nil {
		t.Fatalf("compileTestWorkspace failed: %v", err)
	}
	if result.OK() {
		t.Fatalf("expected diagnostics")
	}
}

func TestCompileWorkspaceRejectsImportedUnexportedConcreteMethodExpression(t *testing.T) {
	result, err := compileTestWorkspace([]SourcePackage{{
		ModulePath: "example/main",
		Files: []SourceFile{{
			Path: "main.mgo",
			Text: `
package main

import "example/lib"

func Main() int {
	_ = lib.Token.hidden
	return 0
}
`,
		}},
	}, {
		ModulePath: "example/lib",
		Files: []SourceFile{{
			Path: "lib.mgo",
			Text: `
package lib

type Token struct{}

func (t Token) hidden() int {
	return 1
}
`,
		}},
	}})
	if err != nil {
		t.Fatalf("compileTestWorkspace failed: %v", err)
	}
	if result.OK() {
		t.Fatalf("expected diagnostics")
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == "semantic.method_expression.method" {
			return
		}
	}
	t.Fatalf("expected method expression diagnostic, got %#v", result.Diagnostics)
}
