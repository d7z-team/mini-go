package compiler

import (
	"strings"
	"testing"
)

func TestCompileMutexMethodsUseRuntimeSynchronization(t *testing.T) {
	result, err := compileTestPackage(SourcePackage{ModulePath: "sync", Files: []SourceFile{{Path: "mutex.mgo", Text: `package sync
type Mutex struct { state chan bool }
func (m *Mutex) Lock() { runtimeMutexLock(&m.state) }
func (m *Mutex) TryLock() bool { return runtimeMutexTryLock(&m.state) }
func (m *Mutex) Unlock() { runtimeMutexUnlock(&m.state) }
`}, {Path: "intrinsic.mgo", Text: `package sync
func runtimeMutexLock(state *chan bool) { panic("intrinsic") }
func runtimeMutexTryLock(state *chan bool) bool { panic("intrinsic") }
func runtimeMutexUnlock(state *chan bool) { panic("intrinsic") }
`}}})
	if err != nil || !result.OK() {
		t.Fatalf("compile: %v %#v", err, result.Diagnostics)
	}
	assembly, err := result.Disassemble()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sync.mutex_lock", "sync.mutex_try_lock", "sync.mutex_unlock"} {
		if !strings.Contains(assembly, id) {
			t.Fatalf("missing %s lowering:\n%s", id, assembly)
		}
	}
}

func TestCompileRejectsIntrinsicFunctionEscape(t *testing.T) {
	result, err := compileTestSource("reflect", "intrinsic.mgo", `package reflect
func runtimeTypeOf(value any) any { return value }
var escaped = runtimeTypeOf
func Main() {}
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == "hirgen.intrinsic.escape" {
			return
		}
	}
	t.Fatalf("missing intrinsic escape diagnostic: %#v", result.Diagnostics)
}
