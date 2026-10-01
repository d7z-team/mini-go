package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/d7z-team/mini-go/compiler/target"
	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestPrepareActionStoresValidatedOutput(t *testing.T) {
	action, output := testPreparedOutput(t)
	backend := &countingBackend{Backend: NewMemoryBackend()}
	store := New(backend)
	if err := store.StorePrepare(action, output); err != nil {
		t.Fatal(err)
	}
	if backend.putActions != 1 || backend.putOutputs != 1 {
		t.Fatalf("store operations: actions=%d outputs=%d", backend.putActions, backend.putOutputs)
	}
	lookup, err := store.LookupPrepare(action)
	if err != nil || !lookup.Hit || lookup.Image.Hash != output.Image.Hash || len(lookup.TestManifest) != 1 {
		t.Fatalf("LookupPrepare = %#v, %v", lookup, err)
	}

	changed := action
	changed.Entries = []EntryPoint{{Name: "changed", ModulePath: action.Root, Function: "main"}}
	if lookup, err := store.LookupPrepare(changed); err != nil || lookup.Hit {
		t.Fatalf("changed entry lookup = %#v, %v", lookup, err)
	}
	changed = action
	changed.Artifacts = append([]PrepareArtifact(nil), action.Artifacts...)
	changed.Artifacts[0].Hash = hex.EncodeToString(make([]byte, sha256.Size))
	if lookup, err := store.LookupPrepare(changed); err != nil || lookup.Hit {
		t.Fatalf("changed artifact lookup = %#v, %v", lookup, err)
	}
}

func TestLookupPrepareRejectsInvalidState(t *testing.T) {
	action, _ := testPreparedOutput(t)
	backend := NewMemoryBackend()
	actionID, err := action.ID()
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"format":"mini-go-prepare-state","version":1,"image":{},"extra":true}`)
	outputID := OutputIDFor(data)
	if err := backend.PutOutput(outputID, data); err != nil {
		t.Fatal(err)
	}
	if err := backend.PutAction(actionID, Entry{Output: outputID, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	lookup, err := New(backend).LookupPrepare(action)
	if err != nil || lookup.Hit || lookup.Reason != "prepare state invalid" {
		t.Fatalf("LookupPrepare = %#v, %v", lookup, err)
	}
}

func TestLookupPrepareRejectsMismatchedArtifactAndEntryIdentity(t *testing.T) {
	action, output := testPreparedOutput(t)
	tests := []struct {
		name   string
		mutate func(*ir.ExecutionImage)
	}{
		{name: "action artifact hash", mutate: func(image *ir.ExecutionImage) {
			archive := image.Packages[action.Root]
			archive.ArtifactHash = hex.EncodeToString(make([]byte, sha256.Size))
			image.Packages[action.Root] = archive
		}},
		{name: "entry function", mutate: func(image *ir.ExecutionImage) {
			image.Entries[0].FunctionID = "fn.missing"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			image := output.Image
			image.Packages = make(map[string]ir.PackageArchive, len(output.Image.Packages))
			for path, archive := range output.Image.Packages {
				image.Packages[path] = archive
			}
			test.mutate(&image)
			image.Hash = ""
			var err error
			image.Hash, err = ir.HashExecutionImage(image)
			if err != nil {
				t.Fatal(err)
			}
			imageJSON, err := canonicalJSON(image)
			if err != nil {
				t.Fatal(err)
			}
			actionID, err := action.ID()
			if err != nil {
				t.Fatal(err)
			}
			stateJSON, err := canonicalJSON(prepareState{Format: prepareStateFormat, Version: prepareStateVersion, ActionID: actionID.String(), Image: imageJSON})
			if err != nil {
				t.Fatal(err)
			}
			backend := NewMemoryBackend()
			outputID := OutputIDFor(stateJSON)
			if err := backend.PutOutput(outputID, stateJSON); err != nil {
				t.Fatal(err)
			}
			if err := backend.PutAction(actionID, Entry{Output: outputID, Size: int64(len(stateJSON))}); err != nil {
				t.Fatal(err)
			}
			lookup, err := New(backend).LookupPrepare(action)
			if err != nil || lookup.Hit {
				t.Fatalf("LookupPrepare = %#v, %v", lookup, err)
			}
		})
	}
}

func TestLookupPrepareRejectsMismatchedActionBinding(t *testing.T) {
	action, output := testPreparedOutput(t)
	actionID, err := action.ID()
	if err != nil {
		t.Fatal(err)
	}
	other := action
	other.Mode = "other"
	otherID, err := other.ID()
	if err != nil {
		t.Fatal(err)
	}
	imageJSON, err := canonicalJSON(output.Image)
	if err != nil {
		t.Fatal(err)
	}
	stateJSON, err := canonicalJSON(prepareState{
		Format: prepareStateFormat, Version: prepareStateVersion, ActionID: otherID.String(), Image: imageJSON,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := NewMemoryBackend()
	outputID := OutputIDFor(stateJSON)
	if err := backend.PutOutput(outputID, stateJSON); err != nil {
		t.Fatal(err)
	}
	if err := backend.PutAction(actionID, Entry{Output: outputID, Size: int64(len(stateJSON))}); err != nil {
		t.Fatal(err)
	}
	lookup, err := New(backend).LookupPrepare(action)
	if err != nil || lookup.Hit || lookup.Reason != "prepare state action mismatch" {
		t.Fatalf("LookupPrepare = %#v, %v", lookup, err)
	}
}

func TestPrepareActionIdentityIsCanonical(t *testing.T) {
	action, _ := testPreparedOutput(t)
	action.Entries = append(action.Entries, EntryPoint{Name: "answer", ModulePath: action.Root, Function: "answer"})
	action.Artifacts = append(action.Artifacts, PrepareArtifact{ModulePath: "example/dependency", Hash: hex.EncodeToString(make([]byte, sha256.Size))})
	reordered := action
	reordered.Entries = []EntryPoint{action.Entries[1], action.Entries[0]}
	reordered.Artifacts = []PrepareArtifact{action.Artifacts[1], action.Artifacts[0]}
	first, err := action.ID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := reordered.ID()
	if err != nil || first != second {
		t.Fatalf("canonical ids = %s, %s, %v", first, second, err)
	}
	changes := []PrepareAction{action, action, action, action}
	changes[0].Mode = "prepare"
	changes[1].Compiler = "changed"
	changes[2].Contract = "changed"
	changes[3].Target = target.Target{Tags: []string{"debug"}}
	for i, changed := range changes {
		id, err := changed.ID()
		if err != nil || id == first {
			t.Fatalf("prepare identity change %d: id=%s err=%v", i, id, err)
		}
	}
}

func FuzzPrepareActionCapabilityIdentity(f *testing.F) {
	f.Add("console, filesystem,console")
	f.Add("entropy")
	f.Add("")
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 4096 {
			t.Skip()
		}
		action, _ := testPreparedOutput(t)
		parts := strings.Split(text, ",")
		for index := range parts {
			parts[index] = strings.TrimSpace(parts[index])
		}
		action.Capabilities = append([]string(nil), parts...)
		first, err := action.ID()
		if err != nil {
			return
		}
		reordered := action
		reordered.Capabilities = append([]string(nil), parts...)
		for left, right := 0, len(reordered.Capabilities)-1; left < right; left, right = left+1, right-1 {
			reordered.Capabilities[left], reordered.Capabilities[right] = reordered.Capabilities[right], reordered.Capabilities[left]
		}
		second, err := reordered.ID()
		if err != nil || first != second {
			t.Fatalf("reordered capability identity = %s, want %s: %v", second, first, err)
		}
		duplicated := action
		duplicated.Capabilities = append(append([]string(nil), parts...), parts...)
		third, err := duplicated.ID()
		if err != nil || first != third {
			t.Fatalf("duplicate capability identity = %s, want %s: %v", third, first, err)
		}
	})
}

func testPreparedOutput(t *testing.T) (PrepareAction, PreparedOutput) {
	t.Helper()
	artifact := ir.NewArtifact("example/main", "main")
	parser := types.NewParser("example/main", &artifact.TypeTable)
	ref, err := parser.Parse("function() Void")
	if err != nil {
		t.Fatal(err)
	}
	node, ok := artifact.TypeTable.Node(ref)
	if !ok || node.Signature == nil {
		t.Fatal("function signature missing")
	}
	artifact.Functions = []ir.Function{{Code: &ir.SlotCode{}, ID: "fn.main", Signature: *node.Signature}}
	artifactJSON, artifactHash, err := ir.EncodeJSONAndHash(&artifact)
	if err != nil {
		t.Fatal(err)
	}
	buildTarget, err := target.Normalize(target.Target{})
	if err != nil {
		t.Fatal(err)
	}
	entry := EntryPoint{Name: ir.DefaultEntryName, ModulePath: "example/main", Function: "main"}
	image := ir.ExecutionImage{
		Format: ir.ExecutionFormat, Version: ir.ExecutionVersion,
		CompilerID: ir.CompilerIdentity, ContractID: ir.ExecutionContract,
		Target: buildTarget, Root: "example/main", Entries: []ir.Entry{{Name: entry.Name, ModulePath: entry.ModulePath, FunctionID: "fn.main"}},
		Packages: map[string]ir.PackageArchive{"example/main": {Artifact: artifactJSON, ArtifactHash: artifactHash}},
	}
	image.Hash, err = ir.HashExecutionImage(image)
	if err != nil {
		t.Fatal(err)
	}
	action := NewPrepareAction(ir.CompilerIdentity, ir.ExecutionContract, buildTarget, "prepare_test", image.Root, []EntryPoint{entry}, []PrepareArtifact{{ModulePath: image.Root, Hash: artifactHash}}, image.Capabilities)
	return action, PreparedOutput{Image: image, TestManifest: []TestEntry{{Package: image.Root, Name: "TestMain", Index: 0}}}
}
