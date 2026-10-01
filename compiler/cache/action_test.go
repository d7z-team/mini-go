package cache

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/d7z-team/mini-go/compiler/target"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestCompileActionLockSerializesSameKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := New(NewMemoryBackend())
		action := testCacheAction("example/main", "main", nil, nil)
		releaseFirst, err := store.LockCompile(t.Context(), action)
		if err != nil {
			t.Fatal(err)
		}
		acquired := make(chan func())
		var wait sync.WaitGroup
		wait.Add(1)
		go func() {
			defer wait.Done()
			release, err := store.LockCompile(t.Context(), action)
			if err != nil {
				t.Error(err)
				return
			}
			acquired <- release
		}()
		synctest.Wait()
		select {
		case release := <-acquired:
			release()
			t.Fatal("second caller acquired an active package action")
		default:
		}
		releaseFirst()
		select {
		case release := <-acquired:
			release()
		case <-time.After(time.Second):
			t.Fatal("second caller did not acquire released package action")
		}
		wait.Wait()
	})
}

func TestCompileActionLocksAreIndependentAcrossStores(t *testing.T) {
	first := New(NewMemoryBackend())
	second := New(NewMemoryBackend())
	action := testCacheAction("example/main", "main", nil, nil)
	release, err := first.LockCompile(t.Context(), action)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	acquired := make(chan struct{})
	go func() {
		unlock, err := second.LockCompile(t.Context(), action)
		if err != nil {
			t.Error(err)
			return
		}
		unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("independent store waited for another store's action")
	}
}

func BenchmarkCompileActionLock(b *testing.B) {
	store := New(NewMemoryBackend())
	action := testCacheAction("example/main", "main", nil, nil)
	b.ReportAllocs()
	for b.Loop() {
		release, err := store.LockCompile(context.Background(), action)
		if err != nil {
			b.Fatal(err)
		}
		release()
	}
}

func TestActionIdentityIsDeterministic(t *testing.T) {
	action := testCacheAction("example/main", "main", []SourceFile{{Path: "main.mgo", Hash: "source", Selected: true}}, []Dependency{{ModulePath: "example/lib", ExportHash: "export"}})
	first, err := action.ID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := action.ID()
	if err != nil || first != second {
		t.Fatalf("non-deterministic action id: %s %s, %v", first, second, err)
	}
	changes := []Action{action, action, action, action, action, action, action}
	changes[0].Target = target.Target{Tags: []string{"debug"}}
	changes[1].SourceFiles = []SourceFile{{Path: "main.mgo", Hash: "changed", Selected: true}}
	changes[2].SourceFiles = []SourceFile{{Path: "main.mgo", Hash: "source", Selected: false}}
	changes[3].IntrinsicSchema = "changed"
	changes[4].DependencyHashes = []Dependency{{ModulePath: "example/lib", ExportHash: "changed"}}
	changes[5].PackageID = "std::example/main"
	changes[6].Optimization = action.Optimization + 1
	for i, changed := range changes {
		id, err := changed.ID()
		if err != nil {
			t.Fatal(err)
		}
		if id == first {
			t.Fatalf("semantic action change %d did not invalidate identity", i)
		}
	}
	parsed, err := ParseActionID(first.String())
	if err != nil || parsed != first {
		t.Fatalf("ParseActionID = %s, %v", parsed, err)
	}
}

func TestStoreCompilePublishesStateAndManifest(t *testing.T) {
	backend := &countingBackend{Backend: NewMemoryBackend()}
	store := New(backend)
	input := testCacheAction("example/main", "main", []SourceFile{{Path: "main.mgo", Hash: "source-hash", Selected: true}}, nil)
	artifact := ir.NewArtifact("example/main", "main")
	stored, err := store.StoreCompile(input, mustSealArtifact(t, artifact), mustPackageSymbols(t, artifact), mustPackageData(t, artifact))
	if err != nil {
		t.Fatalf("StoreCompile failed: %v", err)
	}
	if backend.putActions != 2 || backend.putOutputs != 2 {
		t.Fatalf("store operations: actions=%d outputs=%d", backend.putActions, backend.putOutputs)
	}
	manifest, err := store.LookupCompileManifest(input)
	if err != nil || !manifest.Hit || manifest.ArtifactHash != stored.ArtifactHash || manifest.ExportHash != stored.ExportHash {
		t.Fatalf("LookupCompileManifest = %#v, %v", manifest, err)
	}
	backend.resetReads()
	lookup, err := store.LookupCompile(input)
	if err != nil {
		t.Fatalf("LookupCompile failed: %v", err)
	}
	if !lookup.Hit || lookup.Artifact.Module.Path != "example/main" || lookup.ArtifactHash != stored.ArtifactHash || lookup.ExportHash != stored.ExportHash {
		t.Fatalf("unexpected lookup: %#v", lookup)
	}
	if backend.getActions != 1 || backend.getOutputs != 1 {
		t.Fatalf("lookup operations: actions=%d outputs=%d", backend.getActions, backend.getOutputs)
	}
}

func TestLookupTreatsCorruptOutputAsMiss(t *testing.T) {
	backend := &corruptOutputBackend{Backend: NewMemoryBackend()}
	store := New(backend)
	input := testCacheAction("example/main", "main", nil, nil)
	artifact := ir.NewArtifact("example/main", "main")
	if _, err := store.StoreCompile(input, mustSealArtifact(t, artifact), mustPackageSymbols(t, artifact), mustPackageData(t, artifact)); err != nil {
		t.Fatal(err)
	}
	actionID, _ := input.ID()
	entry, found, _ := backend.GetAction(actionID)
	if !found {
		t.Fatal("stored action missing")
	}
	backend.output = entry.Output
	backend.data = []byte(`{"format":"broken"}`)
	lookup, err := store.LookupCompile(input)
	if err != nil || lookup.Hit || lookup.Reason != "output missing or corrupt" {
		t.Fatalf("LookupCompile = %#v, %v", lookup, err)
	}
}

func TestLookupRejectsInvalidPackageState(t *testing.T) {
	backend := NewMemoryBackend()
	store := New(backend)
	input := testCacheAction("example/main", "main", nil, nil)
	actionID, _ := input.ID()
	data := []byte(`{"format":"mini-go-package-state","version":8,"artifact":{},"export_data":{},"extra":true}`)
	outputID := OutputIDFor(data)
	_ = backend.PutOutput(outputID, data)
	_ = backend.PutAction(actionID, Entry{Output: outputID, Size: int64(len(data))})
	lookup, err := store.LookupCompile(input)
	if err != nil || lookup.Hit || lookup.Reason != "package state invalid" {
		t.Fatalf("LookupCompile = %#v, %v", lookup, err)
	}
}

func TestLookupCompileManifestRejectsInvalidState(t *testing.T) {
	backend := NewMemoryBackend()
	store := New(backend)
	input := testCacheAction("example/main", "main", nil, nil)
	actionID, err := packageManifestActionID(input)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"format":"mini-go-package-manifest","version":2,"artifact_hash":"bad","export_hash":"bad"}`)
	outputID := OutputIDFor(data)
	if err := backend.PutOutput(outputID, data); err != nil {
		t.Fatal(err)
	}
	if err := backend.PutAction(actionID, Entry{Output: outputID, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	lookup, err := store.LookupCompileManifest(input)
	if err != nil || lookup.Hit || lookup.Reason != "package manifest invalid" {
		t.Fatalf("LookupCompileManifest = %#v, %v", lookup, err)
	}
}

func TestCompileActionChangesMiss(t *testing.T) {
	store := New(NewMemoryBackend())
	input := testCacheAction("example/main", "main", nil, []Dependency{{ModulePath: "example/lib", ExportHash: "old-export"}})
	artifact := ir.NewArtifact("example/main", "main")
	artifact.Requirements = []ir.Requirement{{Kind: "source", ModulePath: "example/lib"}}
	if _, err := store.StoreCompile(input, mustSealArtifact(t, artifact), mustPackageSymbols(t, artifact), mustPackageData(t, artifact)); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []Action{
		testCacheAction("example/main", "main", nil, []Dependency{{ModulePath: "example/lib", ExportHash: "new-export"}}),
	} {
		lookup, err := store.LookupCompile(changed)
		if err != nil || lookup.Hit {
			t.Fatalf("changed action lookup = %#v, %v", lookup, err)
		}
	}
}

func TestStoreRejectsInvalidPackageState(t *testing.T) {
	store := New(NewMemoryBackend())
	input := testCacheAction("example/main", "main", nil, nil)
	artifact := ir.NewArtifact("example/main", "main")
	wrong := mustPackageData(t, artifact)
	wrong.ModulePath = "example/other"
	if _, err := store.StoreCompile(input, mustSealArtifact(t, artifact), mustPackageSymbols(t, artifact), wrong); err == nil {
		t.Fatal("mismatched export data was stored")
	}
}

func TestStoreAcceptsCompileOnlyDependency(t *testing.T) {
	store := New(NewMemoryBackend())
	input := testCacheAction("example/main", "main", nil, []Dependency{{ModulePath: "example/dependency", ExportHash: "compile-time-export"}})
	artifact := ir.NewArtifact("example/main", "main")
	if _, err := store.StoreCompile(input, mustSealArtifact(t, artifact), mustPackageSymbols(t, artifact), mustPackageData(t, artifact)); err != nil {
		t.Fatalf("StoreCompile rejected compile-only dependency: %v", err)
	}

	artifact.Requirements = []ir.Requirement{{Kind: "source", ModulePath: "example/unknown", Hash: strings.Repeat("0", 64)}}
	if _, err := store.StoreCompile(input, mustSealArtifact(t, artifact), mustPackageSymbols(t, artifact), mustPackageData(t, artifact)); err == nil {
		t.Fatal("StoreCompile accepted a dependency-bound package artifact")
	}
}

func TestMemoryBackendOwnsOutputValues(t *testing.T) {
	backend := NewMemoryBackend()
	input := []byte("value")
	id := OutputIDFor(input)
	if err := backend.PutOutput(id, input); err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	first, found, err := backend.GetOutput(id)
	if err != nil || !found || string(first) != "value" {
		t.Fatalf("first GetOutput = %q, %v, %v", first, found, err)
	}
	first[0] = 'Y'
	second, _, _ := backend.GetOutput(id)
	if string(second) != "value" {
		t.Fatalf("GetOutput returned shared storage: %q", second)
	}
}

func TestStorePublishesActionAfterOutput(t *testing.T) {
	input := testCacheAction("example/main", "main", nil, nil)
	artifact := ir.NewArtifact("example/main", "main")

	outputFailure := &faultBackend{Backend: NewMemoryBackend(), failPutOutput: true}
	store := New(outputFailure)
	if _, err := store.StoreCompile(input, mustSealArtifact(t, artifact), mustPackageSymbols(t, artifact), mustPackageData(t, artifact)); err == nil {
		t.Fatal("StoreCompile succeeded after output failure")
	}
	actionID, _ := input.ID()
	if _, found, _ := outputFailure.GetAction(actionID); found {
		t.Fatal("action was published before its output")
	}

	actionFailure := &faultBackend{Backend: NewMemoryBackend(), failPutAction: true}
	store = New(actionFailure)
	if _, err := store.StoreCompile(input, mustSealArtifact(t, artifact), mustPackageSymbols(t, artifact), mustPackageData(t, artifact)); err == nil {
		t.Fatal("StoreCompile succeeded after action failure")
	}
	if _, found, _ := actionFailure.GetAction(actionID); found {
		t.Fatal("failed action was published")
	}
	if actionFailure.outputWrites != 1 {
		t.Fatalf("action failure wrote %d outputs, want 1", actionFailure.outputWrites)
	}
}

func TestBackendErrorsAreReported(t *testing.T) {
	input := testCacheAction("example/main", "main", nil, nil)
	artifact := ir.NewArtifact("example/main", "main")
	for _, operation := range []string{"get-action", "get-output", "put-output", "put-action"} {
		backend := &faultBackend{Backend: NewMemoryBackend(), operation: operation}
		store := New(backend)
		if operation == "get-output" {
			actionID, _ := input.ID()
			_ = backend.Backend.PutAction(actionID, Entry{Output: OutputIDFor(nil), Size: 0})
		}
		var err error
		if operation == "get-action" || operation == "get-output" {
			_, err = store.LookupCompile(input)
		} else {
			_, err = store.StoreCompile(input, mustSealArtifact(t, artifact), mustPackageSymbols(t, artifact), mustPackageData(t, artifact))
		}
		if err == nil {
			t.Fatalf("%s failure was not reported", operation)
		}
	}
}

type countingBackend struct {
	Backend
	getActions, getOutputs int
	putActions, putOutputs int
}

type corruptOutputBackend struct {
	Backend
	output OutputID
	data   []byte
}

func (b *corruptOutputBackend) GetOutput(id OutputID) ([]byte, bool, error) {
	if id == b.output && b.data != nil {
		return append([]byte(nil), b.data...), true, nil
	}
	return b.Backend.GetOutput(id)
}

func (b *countingBackend) GetAction(id ActionID) (Entry, bool, error) {
	b.getActions++
	return b.Backend.GetAction(id)
}

func (b *countingBackend) GetOutput(id OutputID) ([]byte, bool, error) {
	b.getOutputs++
	return b.Backend.GetOutput(id)
}

func (b *countingBackend) PutAction(id ActionID, entry Entry) error {
	b.putActions++
	return b.Backend.PutAction(id, entry)
}

func (b *countingBackend) PutOutput(id OutputID, data []byte) error {
	b.putOutputs++
	return b.Backend.PutOutput(id, data)
}

func (b *countingBackend) resetReads() { b.getActions, b.getOutputs = 0, 0 }

type faultBackend struct {
	Backend
	operation     string
	failPutOutput bool
	failPutAction bool
	outputWrites  int
}

func (b *faultBackend) GetAction(id ActionID) (Entry, bool, error) {
	if b.operation == "get-action" {
		return Entry{}, false, errors.New("get action failed")
	}
	return b.Backend.GetAction(id)
}

func (b *faultBackend) GetOutput(id OutputID) ([]byte, bool, error) {
	if b.operation == "get-output" {
		return nil, false, errors.New("get output failed")
	}
	return b.Backend.GetOutput(id)
}

func (b *faultBackend) PutOutput(id OutputID, data []byte) error {
	if b.operation == "put-output" || b.failPutOutput {
		return errors.New("put output failed")
	}
	if err := b.Backend.PutOutput(id, data); err != nil {
		return err
	}
	b.outputWrites++
	return nil
}

func (b *faultBackend) PutAction(id ActionID, entry Entry) error {
	if b.operation == "put-action" || b.failPutAction {
		return errors.New("put action failed")
	}
	return b.Backend.PutAction(id, entry)
}
