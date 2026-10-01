package cache

import (
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func TestSealArtifactValidatesInput(t *testing.T) {
	artifact := ir.NewArtifact("example/sealed", "sealed")
	artifact.Format = "invalid"
	if _, err := SealArtifact(artifact); err == nil {
		t.Fatal("sealed invalid artifact")
	}
}

func TestCompileStoresValidateSnapshotBinding(t *testing.T) {
	artifact := ir.NewArtifact("sample", "sample")
	sealed := mustSealArtifact(t, artifact)
	symbols, data := mustPackageSymbols(t, artifact), mustPackageData(t, artifact)
	action := testCacheAction("sample", "sample", nil, nil)
	for _, backend := range []string{"transient", "serialized"} {
		for _, invalid := range []string{"snapshot", "action", "symbols", "export"} {
			t.Run(backend+"/"+invalid, func(t *testing.T) {
				store := New(NewMemoryBackend())
				if backend == "transient" {
					local := NewTransient(TransientConfig{})
					t.Cleanup(local.Close)
					store = local
				}
				a, value, syms, exports := action, sealed, symbols, data
				switch invalid {
				case "snapshot":
					value = CompiledArtifact{}
				case "action":
					a.ModulePath = "other"
				case "symbols":
					syms.ModulePath = "other"
				case "export":
					exports.ArtifactHash = "invalid"
				}
				if _, err := store.StoreCompile(a, value, syms, exports); err == nil {
					t.Fatal("accepted invalid binding")
				}
				if result, err := store.LookupCompileManifest(action); err != nil || result.Hit {
					t.Fatalf("failed write published state: %+v %v", result, err)
				}
			})
		}
	}
}
