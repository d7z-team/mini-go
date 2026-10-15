package router

import (
	"context"
	"fmt"
	"testing"

	"github.com/d7z-team/mini-go/rpc"
)

func testMRPCBundle(t *testing.T, importPath string) MRPCBundle {
	t.Helper()
	bundle, err := NewMRPCBundle(importPath, []MRPCFile{{Path: "service.mrpc", Text: "syntax = \"mrpc/v2\";\npackage service;\nservice Service { Value() (value string); }\n"}})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestContractRepositoryResolvesExactImmutableBundle(t *testing.T) {
	repository := NewContractRepository()
	first := testMRPCBundle(t, "example/service")
	second, err := NewMRPCBundle("example/service", []MRPCFile{{Path: "service.mrpc", Text: "syntax = \"mrpc/v2\";\npackage service;\nservice Service { Other() (value string); }\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Publish(first, second); err != nil {
		t.Fatal(err)
	}
	expectedBundles := []MRPCBundle{cloneMRPCBundle(first), cloneMRPCBundle(second)}
	first.Files[0].Text = "caller mutation"
	second.Files[0].Hash = "caller mutation"
	for _, expected := range expectedBundles {
		resolved, err := repository.ResolveContract(context.Background(), ContractReference{ImportPath: expected.ImportPath, Hash: expected.Hash})
		if err != nil {
			t.Fatal(err)
		}
		if !equalMRPCBundle(resolved, expected) {
			t.Fatal("caller mutation changed published bundle")
		}
		resolved.Files[0].Text = "mutated"
		again, err := repository.ResolveContract(context.Background(), ContractReference{ImportPath: expected.ImportPath, Hash: expected.Hash})
		if err != nil {
			t.Fatal(err)
		}
		if again.Files[0].Text != expected.Files[0].Text {
			t.Fatal("resolved bundle mutated repository state")
		}
	}
	if _, err := repository.ResolveContract(context.Background(), ContractReference{ImportPath: first.ImportPath, Hash: contractHash}); err != nil {
		code, _ := rpc.CodeOf(err)
		if code != rpc.CodeNotFound {
			t.Fatalf("unknown bundle error = %v", err)
		}
	} else {
		t.Fatal("unknown bundle resolved")
	}
}

func TestContractRepositoryRejectsMalformedBundleWithoutPartialPublish(t *testing.T) {
	repository := NewContractRepository()
	valid := testMRPCBundle(t, "example/valid")
	invalid := testMRPCBundle(t, "example/invalid")
	invalid.Files[0].Hash = contractHash
	if err := repository.Publish(valid, invalid); err == nil {
		t.Fatal("malformed bundle was published")
	}
	if _, err := repository.ResolveContract(context.Background(), ContractReference{ImportPath: valid.ImportPath, Hash: valid.Hash}); err != nil {
		code, _ := rpc.CodeOf(err)
		if code != rpc.CodeNotFound {
			t.Fatalf("partial publication remained: %v", err)
		}
	} else {
		t.Fatal("partial publication remained")
	}
}

func FuzzContractRepositoryPublication(f *testing.F) {
	f.Add("example/service", "service.mrpc", "syntax = \"mrpc/v2\";\npackage service;\nservice Service { Value() (value string); }\n")
	f.Add("../invalid", "service.mrpc", "")
	f.Fuzz(func(t *testing.T, importPath, filePath, text string) {
		if len(importPath)+len(filePath)+len(text) > 1<<20 {
			t.Skip()
		}
		bundle, err := NewMRPCBundle(importPath, []MRPCFile{{Path: filePath, Text: text}})
		if err != nil {
			return
		}
		repository := NewContractRepository()
		if err := repository.Publish(bundle); err != nil {
			t.Fatal(err)
		}
		resolved, err := repository.ResolveContract(context.Background(), ContractReference{ImportPath: bundle.ImportPath, Hash: bundle.Hash})
		if err != nil {
			t.Fatal(err)
		}
		if !equalMRPCBundle(resolved, bundle) {
			t.Fatal("published bundle did not round trip")
		}
	})
}

func FuzzContractRepositoryBatchTransactions(f *testing.F) {
	f.Add([]byte{0, 4, 1, 5, 2, 3, 0})
	f.Add([]byte{5, 5, 4, 0, 1})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 256 {
			t.Skip()
		}
		repository := NewContractRepository()
		baseline := testMRPCBundle(t, "fuzz/batch/baseline")
		if err := repository.Publish(baseline); err != nil {
			t.Fatal(err)
		}

		for index, operation := range operations {
			first, err := NewMRPCBundle(fmt.Sprintf("fuzz/batch/%d/first", index), []MRPCFile{
				{Path: "service.mrpc", Text: fmt.Sprintf("package batch\n// %d\n", operation)},
			})
			if err != nil {
				t.Fatal(err)
			}
			batch := []MRPCBundle{first}
			switch operation % 6 {
			case 1:
				batch[0].Files[0].Hash = contractHash
			case 2:
				batch[0].Hash = contractHash
			case 3:
				batch[0].Files = append(batch[0].Files, batch[0].Files[0])
			case 4:
				second, bundleErr := NewMRPCBundle(fmt.Sprintf("fuzz/batch/%d/second", index), []MRPCFile{
					{Path: "types.mrpc", Text: fmt.Sprintf("package batch\n// second %d\n", operation)},
				})
				if bundleErr != nil {
					t.Fatal(bundleErr)
				}
				batch = append(batch, second)
			case 5:
				second, bundleErr := NewMRPCBundle(fmt.Sprintf("fuzz/batch/%d/invalid", index), []MRPCFile{
					{Path: "service.mrpc", Text: "package batch\n"},
				})
				if bundleErr != nil {
					t.Fatal(bundleErr)
				}
				second.Files[0].Hash = contractHash
				batch = append(batch, second)
			}

			before := contractRepositorySnapshot(repository)
			publishErr := repository.Publish(batch...)
			if publishErr != nil {
				if !equalContractRepositorySnapshot(before, contractRepositorySnapshot(repository)) {
					t.Fatal("failed batch publication changed repository state")
				}
				continue
			}
			for _, bundle := range batch {
				resolved, resolveErr := repository.ResolveContract(context.Background(), ContractReference{
					ImportPath: bundle.ImportPath,
					Hash:       bundle.Hash,
				})
				if resolveErr != nil || !equalMRPCBundle(resolved, bundle) {
					t.Fatalf("published bundle did not round trip: %#v, %v", resolved, resolveErr)
				}
				resolved.Files[0].Text = "mutated"
				again, resolveErr := repository.ResolveContract(context.Background(), ContractReference{
					ImportPath: bundle.ImportPath,
					Hash:       bundle.Hash,
				})
				if resolveErr != nil || !equalMRPCBundle(again, bundle) {
					t.Fatal("resolved bundle mutated repository state")
				}
			}
		}
		resolved, err := repository.ResolveContract(context.Background(), ContractReference{
			ImportPath: baseline.ImportPath,
			Hash:       baseline.Hash,
		})
		if err != nil || !equalMRPCBundle(resolved, baseline) {
			t.Fatal("baseline contract was corrupted")
		}
	})
}

func contractRepositorySnapshot(repository *ContractRepository) map[ContractReference]MRPCBundle {
	repository.mu.RLock()
	defer repository.mu.RUnlock()
	snapshot := make(map[ContractReference]MRPCBundle, len(repository.bundles))
	for reference, bundle := range repository.bundles {
		snapshot[reference] = cloneMRPCBundle(bundle)
	}
	return snapshot
}

func equalContractRepositorySnapshot(left, right map[ContractReference]MRPCBundle) bool {
	if len(left) != len(right) {
		return false
	}
	for reference, bundle := range left {
		other, ok := right[reference]
		if !ok || !equalMRPCBundle(bundle, other) {
			return false
		}
	}
	return true
}
