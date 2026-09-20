package runtime

import (
	"fmt"

	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

// A snapshot identifies entries, not language keys: NaN keys cannot be looked
// up by equality, and a deleted/reinserted key denotes a new entry.
type mapIterator struct {
	object   vmValue
	entries  []uint64
	position int
}

func (f *frame) initMapIterator(local string, object vmValue) error {
	if !f.module.isMapType(object.Type) {
		return fmt.Errorf("map iterator requires map, got %s", object.Type)
	}
	data, ok := object.Data.(*vmMap)
	if !ok && object.Data != nil {
		return fmt.Errorf("invalid map backing for %s", object.Type)
	}
	identities := data.entryIdentities()
	delete(f.mapIterators, local)
	if err := f.module.vm.chargeRuntimeObject(len(identities)*2, 0); err != nil {
		return err
	}
	iterator := &mapIterator{object: object, entries: identities}
	if f.mapIterators == nil {
		f.mapIterators = make(map[string]*mapIterator)
	}
	f.mapIterators[local] = iterator
	return nil
}

func (f *frame) nextMapIterator(local string) error {
	iterator := f.mapIterators[local]
	if iterator == nil {
		return fmt.Errorf("map iterator %q is not initialized", local)
	}
	data, _ := iterator.object.Data.(*vmMap)
	for iterator.position < len(iterator.entries) {
		candidate := iterator.entries[iterator.position]
		iterator.position++
		entry, exists := data.loadIdentity(candidate)
		if !exists {
			continue
		}
		f.push(f.module.cloneValueForStore(entry.Key))
		f.push(f.module.cloneValueForStore(entry.Value))
		f.push(newBoolValue(true))
		return nil
	}
	keyType, valueType, _ := f.module.mapKeyValueTypes(iterator.object.Type)
	f.push(f.module.zeroValue(keyType))
	f.push(f.module.zeroValue(valueType))
	f.push(newBoolValue(false))
	iterator.entries = nil
	return nil
}

func (iterator *mapIterator) logicalBytes() int64 {
	return ir.RuntimeNodeBytes + int64(cap(iterator.entries))*2*ir.RuntimeSlotBytes
}
