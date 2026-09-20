package runtime

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestMapStorageConcurrentPublicationKeepsLimitAndIdentity(t *testing.T) {
	for _, workers := range []int{1, 2, 4} {
		data := newVMMap(0)
		start := make(chan struct{})
		var group sync.WaitGroup
		var published atomic.Int64
		for worker := range workers {
			group.Go(func() {
				<-start
				for index := worker; index < 64; index += workers {
					key := vmMapKey{Kind: vmMapKeyInt, Int64: int64(index)}
					value := newVMValue("Int", int64(index))
					if data.storeWithinLimit(key, vmMapEntry{Key: value, Value: value}, 16) == nil {
						published.Add(1)
					}
					for key, entry := range data.snapshot() {
						if entry.Key.materializedData() != key.Int64 || entry.Value.materializedData() != key.Int64 {
							t.Errorf("inconsistent entry for %v", key)
						}
					}
				}
			})
		}
		close(start)
		group.Wait()
		if published.Load() != 16 || data.length() != 16 {
			t.Fatalf("workers=%d: published=%d size=%d", workers, published.Load(), data.length())
		}
		before := data.snapshot()
		for key, entry := range before {
			data.storeEntry(key, vmMapEntry{Key: entry.Key, Value: newVMValue("Int", int64(100))})
			updated, ok := data.loadIdentity(entry.identity)
			if !ok || updated.Value.materializedData() != int64(100) {
				t.Fatal("replacement changed entry identity")
			}
			data.deleteEntry(key)
			data.storeEntry(key, entry)
			if _, ok := data.loadIdentity(entry.identity); ok {
				t.Fatal("deleted identity resolved to a reinserted entry")
			}
		}
		data.clear()
		if data.length() != 0 || len(data.entryIdentities()) != 0 {
			t.Fatal("clear retained entries or identities")
		}
		if len(before) != 16 {
			t.Fatal("snapshot changed after map mutation")
		}
	}
}
