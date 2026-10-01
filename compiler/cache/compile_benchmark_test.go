package cache

import (
	"fmt"
	"testing"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

func BenchmarkCompileCacheWrite(b *testing.B) {
	artifact := ir.NewArtifact("bench", "bench")
	for index := range 100 {
		code := &ir.SlotCode{Operands: []ir.SlotOperands{{}}}
		descriptor, err := code.Descriptors.Append(ir.ReturnPayload{})
		if err != nil {
			b.Fatal(err)
		}
		code.Instructions = []ir.SlotInstruction{{Op: ir.OpReturn, Descriptor: descriptor}}
		artifact.Functions = append(artifact.Functions, ir.Function{ID: fmt.Sprintf("fn%d", index), Code: code})
	}
	sealed := mustSealArtifact(b, artifact)
	data, symbols := mustPackageData(b, artifact), mustPackageSymbols(b, artifact)
	action := testCacheAction("bench", "bench", nil, nil)
	for _, backend := range []string{"transient", "serialized"} {
		b.Run(backend, func(b *testing.B) {
			store := New(NewMemoryBackend())
			if backend == "transient" {
				local := NewTransient(TransientConfig{})
				b.Cleanup(local.Close)
				store = local
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := store.StoreCompile(action, sealed, symbols, data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
