package runtime

import (
	"fmt"
	"sort"
	"sync"
)

type vmMap struct {
	mu                 sync.Mutex
	entries            map[vmMapKey]vmMapEntry
	nextNonReflexiveID uint64
	nextEntryID        uint64
	entryKeys          map[uint64]vmMapKey
}

type vmMapKey struct {
	Kind         uint8
	TypeIdentity string
	Bool         bool
	Int64        int64
	Uint64       uint64
	Text         string
	NonReflexive bool
	Sequence     uint64
}

const (
	vmMapKeyGeneric uint8 = iota
	vmMapKeyBool
	vmMapKeyString
	vmMapKeyInt
	vmMapKeyUint
)

func (key vmMapKey) String() string {
	if key.NonReflexive {
		return fmt.Sprintf("%s:nonreflexive:%d", key.Text, key.Sequence)
	}
	switch key.Kind {
	case vmMapKeyBool:
		return fmt.Sprintf("%s:bool:%t", key.TypeIdentity, key.Bool)
	case vmMapKeyString:
		return fmt.Sprintf("%s:string:%s", key.TypeIdentity, key.Text)
	case vmMapKeyInt:
		return fmt.Sprintf("%s:int:%d", key.TypeIdentity, key.Int64)
	case vmMapKeyUint:
		return fmt.Sprintf("%s:uint:%d", key.TypeIdentity, key.Uint64)
	default:
		return key.Text
	}
}

type vmMapEntry struct {
	identity uint64
	Key      vmValue
	Value    vmValue
}

func (data *vmMap) storeEntry(key vmMapKey, entry vmMapEntry) {
	data.mu.Lock()
	defer data.mu.Unlock()
	data.storeEntryLocked(key, entry)
}

func (data *vmMap) storeEntryLocked(key vmMapKey, entry vmMapEntry) {
	entry.identity = data.entries[key].identity
	if entry.identity == 0 {
		data.nextEntryID++
		entry.identity = data.nextEntryID
		if data.entryKeys == nil {
			data.entryKeys = make(map[uint64]vmMapKey)
		}
		data.entryKeys[entry.identity] = key
	}
	data.entries[key] = entry
}

func newVMMap(size int) *vmMap {
	return &vmMap{entries: make(map[vmMapKey]vmMapEntry, size)}
}

func (data *vmMap) keyForStore(key vmMapKey) vmMapKey {
	if data == nil || !key.NonReflexive {
		return key
	}
	data.mu.Lock()
	defer data.mu.Unlock()
	data.nextNonReflexiveID++
	key.Sequence = data.nextNonReflexiveID
	return key
}

func sortedVMMapKeys(entries map[vmMapKey]vmMapEntry) []vmMapKey {
	if len(entries) == 0 {
		return nil
	}
	keys := make([]vmMapKey, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

func (data *vmMap) snapshot() map[vmMapKey]vmMapEntry {
	if data == nil {
		return nil
	}
	data.mu.Lock()
	defer data.mu.Unlock()
	entries := make(map[vmMapKey]vmMapEntry, len(data.entries))
	for key, entry := range data.entries {
		entries[key] = entry
	}
	return entries
}

func (data *vmMap) length() int {
	if data == nil {
		return 0
	}
	data.mu.Lock()
	defer data.mu.Unlock()
	return len(data.entries)
}

func (data *vmMap) loadEntry(key vmMapKey) (vmMapEntry, bool) {
	if data == nil {
		return vmMapEntry{}, false
	}
	data.mu.Lock()
	defer data.mu.Unlock()
	entry, exists := data.entries[key]
	return entry, exists
}

func (data *vmMap) deleteEntry(key vmMapKey) {
	data.mu.Lock()
	defer data.mu.Unlock()
	delete(data.entryKeys, data.entries[key].identity)
	delete(data.entries, key)
}

func (data *vmMap) clear() {
	data.mu.Lock()
	defer data.mu.Unlock()
	clear(data.entries)
	clear(data.entryKeys)
}

func (data *vmMap) entryIdentities() []uint64 {
	if data == nil {
		return nil
	}
	data.mu.Lock()
	defer data.mu.Unlock()
	identities := make([]uint64, 0, len(data.entries))
	for _, entry := range data.entries {
		identities = append(identities, entry.identity)
	}
	return identities
}

func (data *vmMap) loadIdentity(identity uint64) (vmMapEntry, bool) {
	data.mu.Lock()
	defer data.mu.Unlock()
	key, exists := data.entryKeys[identity]
	return data.entries[key], exists
}

func (data *vmMap) storeWithinLimit(key vmMapKey, entry vmMapEntry, limit int) error {
	data.mu.Lock()
	defer data.mu.Unlock()
	if _, exists := data.entries[key]; !exists && limit > 0 && len(data.entries) >= limit {
		return ResourceLimitError{Code: "execution.collection_limit", Message: fmt.Sprintf("execution collection element limit exceeded: max %d", limit)}
	}
	data.storeEntryLocked(key, entry)
	return nil
}
