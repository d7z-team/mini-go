package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"math"
	"unsafe"

	"github.com/d7z-team/mini-go/compiler/types"
	ir "github.com/d7z-team/mini-go/runtime/bytecode"
)

const (
	managedMapSlabEntries   = uint32(64)
	managedMapMetadata      = uint32(16)
	managedMapIdentityBytes = uint32(32)
)

// Entry metadata is occupied/generation/hash/next. Bucket heads and next links
// are entry indexes; only the slab directory and typed key/value lanes are GC edges.
type managedMapStorage struct {
	heap     *managedHeap
	identity guestRef
	*preparedMapLayout
	layouts *preparedLayoutRegistry
	owner   *vm
	batch   *managedAllocationBatch
}

type preparedMapLayout struct {
	keyType, valueType       vmType
	keyLayout, valueLayout   preparedValueLayout
	entryStride, valueOffset uint32
	marked                   uint64
}

type managedMapHeader struct {
	directory, buckets             guestRef
	count, created, capacity, free uint32
}

type managedMapSeed struct {
	key   vmMapKey
	entry vmMapEntry
}

// A failed composite construction releases publications without pretending
// that the worker's not-yet-flushed records already belong to the global ledger.
func rollbackManagedPublications(heap *managedHeap, batch *managedAllocationBatch, refs ...guestRef) {
	if heap == nil {
		return
	}
	if batch == nil || batch.finished {
		for _, ref := range refs {
			if ref.valid() {
				_ = heap.release(ref)
			}
		}
		return
	}
	heap.mu.Lock()
	defer heap.mu.Unlock()
	for _, ref := range refs {
		if !ref.valid() {
			continue
		}
		entry := heap.object(ref.slot)
		if entry == nil || entry.generation != ref.generation || entry.state.Load() != managedPublished {
			continue
		}
		workerOwned := false
		for _, reservations := range batch.reservations {
			for index := range reservations {
				if reservations[index].ref == ref {
					workerOwned = true
					break
				}
			}
			if workerOwned {
				break
			}
		}
		if workerOwned {
			batch.committed -= entry.logical
			entry.logical = 0
			entry.state.Store(managedFree)
		} else {
			// Large objects and exhausted leases publish through the owner.
			// Remove their directory and ledger entries together, even when
			// the surrounding composite also used a worker batch.
			heap.counters.reclaimedBytes += entry.logical
			heap.freeObject(ref.slot, true)
		}
	}
}

func (module *moduleInstance) importManagedMap(value vmValue, data *vmMap) (vmValue, error) {
	if module == nil || data == nil {
		return value, nil
	}
	entries := data.snapshot()
	seeds := make([]managedMapSeed, 0, len(entries))
	for key, entry := range entries {
		seeds = append(seeds, managedMapSeed{key: key, entry: entry})
	}
	return module.newManagedMapWithBatch(value.Type, seeds, len(seeds), nil)
}

var managedMapHashSeed = maphash.MakeSeed()

func managedMapKeyHash(key vmMapKey) uint32 {
	var hash maphash.Hash
	hash.SetSeed(managedMapHashSeed)
	// Common scalar keys avoid formatting a temporary string for every lookup.
	_, _ = hash.WriteString(key.TypeIdentity)
	_ = hash.WriteByte(key.Kind)
	var raw [8]byte
	switch key.Kind {
	case vmMapKeyBool:
		if key.Bool {
			raw[0] = 1
		}
		_, _ = hash.Write(raw[:1])
	case vmMapKeyInt:
		binary.LittleEndian.PutUint64(raw[:], uint64(key.Int64))
		_, _ = hash.Write(raw[:])
	case vmMapKeyUint:
		binary.LittleEndian.PutUint64(raw[:], key.Uint64)
		_, _ = hash.Write(raw[:])
	default:
		_, _ = hash.WriteString(key.Text)
	}
	return uint32(hash.Sum64())
}

func alignManagedOffset(offset uint32) uint32 { return (offset + 7) &^ 7 }

func managedMapFieldOffsets(key, value []uint32, valueBase uint32) []uint32 {
	offsets := make([]uint32, 0, len(key)+len(value))
	for _, offset := range key {
		offsets = append(offsets, managedMapMetadata+offset)
	}
	for _, offset := range value {
		offsets = append(offsets, valueBase+offset)
	}
	return offsets
}

func prepareManagedMap(value vmValue) (*managedMapStorage, error) {
	if value.heap == nil || value.layouts == nil || value.owner == nil {
		return nil, errors.New("managed map has no owner")
	}
	registry := value.layouts
	typeLayout, err := registry.layoutFor(value.Type)
	if err != nil {
		return nil, err
	}
	storage := &managedMapStorage{heap: value.heap, identity: value.ref, layouts: registry, owner: value.owner}
	registry.mu.RLock()
	storage.preparedMapLayout = registry.mapLayouts[typeLayout.ID]
	registry.mu.RUnlock()
	if storage.preparedMapLayout != nil {
		return storage, nil
	}
	ownedType, _ := registry.typeByID(typeLayout.ID)
	keyType, valueType, ok := ownedType.MapInfo()
	if !ok {
		return nil, fmt.Errorf("map type %s has no key/value layout", value.Type)
	}
	keyLayout, err := registry.layoutFor(keyType)
	if err != nil {
		return nil, err
	}
	valueLayout, err := registry.layoutFor(valueType)
	if err != nil {
		return nil, err
	}
	if uint64(managedMapMetadata)+uint64(keyLayout.Stride)+uint64(valueLayout.Stride)+14 > math.MaxUint32/uint64(managedMapSlabEntries) {
		return nil, errors.New("managed map entry storage overflows")
	}
	valueOffset := alignManagedOffset(managedMapMetadata + keyLayout.Stride)
	prepared := &preparedMapLayout{
		keyType: keyType, valueType: valueType,
		keyLayout: keyLayout, valueLayout: valueLayout,
		entryStride: alignManagedOffset(valueOffset + valueLayout.Stride), valueOffset: valueOffset,
	}
	resident := uint64(unsafe.Sizeof(preparedMapLayout{}))
	value.heap.mu.Lock()
	registry.mu.Lock()
	if registry.mapLayouts == nil {
		registry.mapLayouts = make(map[uint32]*preparedMapLayout)
	}
	if existing := registry.mapLayouts[typeLayout.ID]; existing != nil {
		prepared = existing
	} else {
		if !value.heap.admitResident(resident) {
			registry.mu.Unlock()
			value.heap.mu.Unlock()
			return nil, errManagedLimit
		}
		registry.mapLayouts[typeLayout.ID] = prepared
		registry.resident[typeLayout.ID] += resident
	}
	registry.mu.Unlock()
	value.heap.mu.Unlock()
	storage.preparedMapLayout = prepared
	return storage, nil
}

func (module *moduleInstance) newManagedMapWithBatch(typ vmType, entries []managedMapSeed, capacity int, batch *managedAllocationBatch) (vmValue, error) {
	if module == nil || module.vm == nil {
		return vmValue{}, errors.New("managed map has no owner")
	}
	if module.vm.heap == nil {
		module.vm.heap = newManagedHeap()
	}
	if module.vm.valueLayouts == nil {
		module.vm.valueLayouts = newPreparedLayoutRegistry()
	}
	value := vmValue{Type: typ, heap: module.vm.heap, owner: module.vm, module: module, layouts: module.vm.valueLayouts}
	storage, err := prepareManagedMap(value)
	if err != nil {
		return vmValue{}, err
	}
	storage.batch = batch
	typeLayout, err := value.layouts.layoutFor(typ)
	if err != nil {
		return vmValue{}, err
	}
	identityLayout, err := value.heap.registerLayout(managedLayout{stride: managedMapIdentityBytes, typeID: typeLayout.ID, references: []uint32{0, 8}})
	if err != nil {
		return vmValue{}, err
	}
	defer value.heap.releaseLayout(identityLayout)
	reservation, err := value.heap.reserveWithBatch(batch, identityLayout, 1, ir.StorageRecordHeaderBytes+ir.StorageMapHeaderBytes)
	if err != nil {
		return vmValue{}, err
	}
	storage.identity = reservation.commit()
	value.ref = storage.identity
	complete := false
	defer func() {
		if !complete {
			storage.rollback()
		}
	}()
	capacity = max(capacity, len(entries))
	if capacity < 0 || uint64(capacity) > math.MaxUint32-uint64(managedMapSlabEntries) {
		return vmValue{}, errManagedLimit
	}
	if capacity > 0 {
		if _, err := storage.ensureCapacity(managedMapHeader{}, uint32(capacity)); err != nil {
			return vmValue{}, err
		}
	}
	for _, entry := range entries {
		if err := storage.storeEntry(entry.key, entry.entry); err != nil {
			return vmValue{}, err
		}
	}
	complete = true
	return value, nil
}

func materializeManagedMap(value vmValue) (*vmMap, bool) {
	if !value.ref.valid() || value.Type.ShapeKind() != types.Map {
		return nil, false
	}
	storage, err := prepareManagedMap(value)
	return &vmMap{managed: storage}, err == nil
}

func (storage *managedMapStorage) currentModule() *moduleInstance {
	return storage.owner.runtimeModule()
}

func (storage *managedMapStorage) readHeader() (managedMapHeader, error) {
	var raw [managedMapIdentityBytes]byte
	if err := storage.heap.read(storage.identity, 0, raw[:]); err != nil {
		return managedMapHeader{}, err
	}
	return managedMapHeader{
		directory: guestRef{binary.LittleEndian.Uint32(raw[0:]), binary.LittleEndian.Uint32(raw[4:])},
		buckets:   guestRef{binary.LittleEndian.Uint32(raw[8:]), binary.LittleEndian.Uint32(raw[12:])},
		count:     binary.LittleEndian.Uint32(raw[16:]),
		created:   binary.LittleEndian.Uint32(raw[20:]),
		capacity:  binary.LittleEndian.Uint32(raw[24:]),
		free:      binary.LittleEndian.Uint32(raw[28:]),
	}, nil
}

func (storage *managedMapStorage) writeHeader(header managedMapHeader) error {
	var raw [managedMapIdentityBytes]byte
	for index, word := range [...]uint32{header.directory.slot, header.directory.generation, header.buckets.slot, header.buckets.generation, header.count, header.created, header.capacity, header.free} {
		binary.LittleEndian.PutUint32(raw[index*4:], word)
	}
	return storage.heap.write(storage.identity, 0, raw[:])
}

func (storage *managedMapStorage) word(ref guestRef, offset uint32) (uint32, error) {
	var raw [4]byte
	err := storage.heap.read(ref, offset, raw[:])
	return binary.LittleEndian.Uint32(raw[:]), err
}

func (storage *managedMapStorage) writeWord(ref guestRef, offset, word uint32) error {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], word)
	return storage.heap.write(ref, offset, raw[:])
}

func (storage *managedMapStorage) location(header managedMapHeader, index uint32) (guestRef, uint32, error) {
	if index == 0 || index > header.capacity {
		return guestRef{}, 0, errors.New("managed map index outside entries")
	}
	slab, err := storage.heap.readReference(header.directory, (index-1)/managedMapSlabEntries*8)
	return slab, (index - 1) % managedMapSlabEntries * storage.entryStride, err
}

func (storage *managedMapStorage) rollback() {
	header, err := storage.readHeader()
	refs := []guestRef{storage.identity}
	if err == nil {
		for slabIndex := uint32(0); slabIndex < header.capacity/managedMapSlabEntries; slabIndex++ {
			slab, readErr := storage.heap.readReference(header.directory, slabIndex*8)
			if readErr == nil {
				refs = append(refs, slab)
			}
		}
		refs = append(refs, header.directory, header.buckets)
	}
	rollbackManagedPublications(storage.heap, storage.batch, refs...)
}

// Capacity preparation allocates before touching existing chains. Once all
// fallible admission succeeds, the owner publishes a directory and bucket table.
func (storage *managedMapStorage) ensureCapacity(header managedMapHeader, index uint32) (managedMapHeader, error) {
	if index <= header.capacity {
		return header, nil
	}
	entryLayout, err := storage.heap.registerLayout(managedLayout{
		stride:     storage.entryStride,
		references: managedMapFieldOffsets(storage.keyLayout.References, storage.valueLayout.References, storage.valueOffset),
		dynamics:   managedMapFieldOffsets(storage.keyLayout.Dynamics, storage.valueLayout.Dynamics, storage.valueOffset),
		types:      managedMapFieldOffsets(storage.keyLayout.TypeOffsets, storage.valueLayout.TypeOffsets, storage.valueOffset),
	})
	if err != nil {
		return header, err
	}
	defer storage.heap.releaseLayout(entryLayout)
	directoryLayout, err := storage.heap.registerLayout(managedLayout{stride: 8, references: []uint32{0}})
	if err != nil {
		return header, err
	}
	defer storage.heap.releaseLayout(directoryLayout)
	bucketLayout, err := storage.heap.registerLayout(managedLayout{stride: 4})
	if err != nil {
		return header, err
	}
	defer storage.heap.releaseLayout(bucketLayout)
	if index > math.MaxUint32-managedMapSlabEntries {
		return header, errManagedLimit
	}
	capacity := (index + managedMapSlabEntries - 1) / managedMapSlabEntries * managedMapSlabEntries
	slabs := capacity / managedMapSlabEntries
	oldSlabs := header.capacity / managedMapSlabEntries
	var published []guestRef
	complete := false
	defer func() {
		if !complete {
			rollbackManagedPublications(storage.heap, storage.batch, published...)
		}
	}()
	allocate := func(layout, count uint32, cost uint64) (guestRef, error) {
		reservation, err := storage.heap.reserveWithBatch(storage.batch, layout, count, cost)
		if err != nil {
			return guestRef{}, err
		}
		ref := reservation.commit()
		published = append(published, ref)
		return ref, nil
	}
	directory := header.directory
	directoryCount := uint32(0)
	if directory.valid() {
		entry, err := storage.heap.resolveObject(directory)
		if err != nil {
			return header, err
		}
		directoryCount = entry.size / 8
	}
	if slabs > directoryCount {
		grownDirectory := uint64(directoryCount) * 2
		if grownDirectory > math.MaxUint32 {
			return header, errManagedLimit
		}
		directoryCount = max(8, max(slabs, uint32(grownDirectory)))
		var err error
		directory, err = allocate(directoryLayout, directoryCount, ir.StorageRecordHeaderBytes+uint64(directoryCount)*ir.StorageReferenceBytes)
		if err != nil {
			return header, err
		}
		if oldSlabs != 0 {
			if err := storage.heap.copy(directory, 0, header.directory, 0, oldSlabs*8); err != nil {
				return header, err
			}
		}
	}
	newSlabs := make([]guestRef, 0, slabs-oldSlabs)
	for count := oldSlabs; count < slabs; count++ {
		slab, err := allocate(entryLayout, managedMapSlabEntries, ir.StorageRecordHeaderBytes+uint64(managedMapSlabEntries)*(ir.StorageMapEntryBytes+storage.keyLayout.Logical+storage.valueLayout.Logical))
		if err != nil {
			return header, err
		}
		newSlabs = append(newSlabs, slab)
	}
	buckets := header.buckets
	bucketCount := uint32(0)
	if buckets.valid() {
		entry, err := storage.heap.resolveObject(buckets)
		if err != nil {
			return header, err
		}
		bucketCount = entry.size / 4
	}
	bucketTarget := uint64(capacity) * 2
	if bucketTarget > math.MaxUint32 {
		return header, errManagedLimit
	}
	if uint64(bucketCount) < bucketTarget {
		bucketCount = 1
		for uint64(bucketCount) < bucketTarget {
			if bucketCount > math.MaxUint32/2 {
				return header, errManagedLimit
			}
			bucketCount *= 2
		}
		var err error
		buckets, err = allocate(bucketLayout, bucketCount, ir.StorageRecordHeaderBytes+uint64(bucketCount)*ir.StorageMapBucketBytes)
		if err != nil {
			return header, err
		}
	}
	if buckets != header.buckets {
		for entryIndex := uint32(1); entryIndex <= header.created; entryIndex++ {
			slab, offset, err := storage.location(header, entryIndex)
			if err != nil {
				return header, err
			}
			var raw [managedMapMetadata]byte
			if err := storage.heap.read(slab, offset, raw[:]); err != nil {
				return header, err
			}
			if binary.LittleEndian.Uint32(raw[:]) == 0 {
				continue
			}
			bucket := binary.LittleEndian.Uint32(raw[8:]) & (bucketCount - 1)
			head, err := storage.word(buckets, bucket*4)
			if err != nil {
				return header, err
			}
			if err := storage.writeWord(slab, offset+12, head); err != nil {
				return header, err
			}
			if err := storage.writeWord(buckets, bucket*4, entryIndex); err != nil {
				return header, err
			}
		}
	}
	for slabIndex, slab := range newSlabs {
		if err := storage.heap.writeReference(directory, (oldSlabs+uint32(slabIndex))*8, slab); err != nil {
			return header, err
		}
	}
	header.directory, header.buckets, header.capacity = directory, buckets, capacity
	if err := storage.writeHeader(header); err != nil {
		return header, err
	}
	complete = true
	return header, nil
}

func (storage *managedMapStorage) readEntry(header managedMapHeader, index, generation uint32) (vmMapEntry, bool, error) {
	if index == 0 || index > header.created {
		return vmMapEntry{}, false, nil
	}
	slab, offset, err := storage.location(header, index)
	if err != nil {
		return vmMapEntry{}, false, err
	}
	var raw [8]byte
	if err := storage.heap.read(slab, offset, raw[:]); err != nil {
		return vmMapEntry{}, false, err
	}
	current := binary.LittleEndian.Uint32(raw[4:])
	if binary.LittleEndian.Uint32(raw[:]) == 0 || generation != 0 && generation != current {
		return vmMapEntry{}, false, nil
	}
	key := decodeManagedInline(storage.heap, slab, offset+managedMapMetadata, storage.keyType, storage.keyLayout, storage.layouts, storage.owner)
	value := decodeManagedInline(storage.heap, slab, offset+storage.valueOffset, storage.valueType, storage.valueLayout, storage.layouts, storage.owner)
	module := storage.currentModule()
	key.module, value.module = module, module
	identity := uint64(current)<<32 | uint64(index)
	return vmMapEntry{identity: identity, Key: key, Value: value}, true, nil
}

func (storage *managedMapStorage) find(header managedMapHeader, key vmMapKey, hash uint32) (uint32, uint32, vmMapEntry, error) {
	if key.NonReflexive || !header.buckets.valid() {
		return 0, 0, vmMapEntry{}, nil
	}
	buckets, err := storage.heap.resolveObject(header.buckets)
	if err != nil {
		return 0, 0, vmMapEntry{}, err
	}
	bucketCount := buckets.size / 4
	if bucketCount == 0 {
		return 0, 0, vmMapEntry{}, errors.New("managed map has an empty bucket table")
	}
	index, err := storage.word(header.buckets, hash%bucketCount*4)
	var previous uint32
	for index != 0 && err == nil {
		slab, offset, locErr := storage.location(header, index)
		if locErr != nil {
			return 0, 0, vmMapEntry{}, locErr
		}
		var raw [managedMapMetadata]byte
		if err := storage.heap.read(slab, offset, raw[:]); err != nil {
			return 0, 0, vmMapEntry{}, err
		}
		if binary.LittleEndian.Uint32(raw[8:]) == hash {
			entry, present, err := storage.readEntry(header, index, 0)
			if err != nil {
				return 0, 0, vmMapEntry{}, err
			}
			if present {
				candidate, err := storage.currentModule().mapKey(entry.Key)
				if err != nil {
					return 0, 0, vmMapEntry{}, err
				}
				if candidate == key {
					return index, previous, entry, nil
				}
			}
		}
		previous, index = index, binary.LittleEndian.Uint32(raw[12:])
	}
	return 0, 0, vmMapEntry{}, err
}

func (storage *managedMapStorage) storeEntry(key vmMapKey, entry vmMapEntry) error {
	return storage.storeEntryWithBatch(key, entry, storage.batch)
}

func (storage *managedMapStorage) acquireMutationBatch() (*managedAllocationBatch, error) {
	credit := managedBatchCredit
	storage.heap.mu.Lock()
	if storage.heap.maxGuestBytes != 0 {
		used := storage.heap.counters.occupiedBytes + storage.heap.counters.reservedBytes
		available := uint64(0)
		if used < storage.heap.maxGuestBytes {
			available = storage.heap.maxGuestBytes - used
		}
		credit = min(credit, available)
	}
	storage.heap.mu.Unlock()
	if credit == 0 {
		return nil, errManagedCollection
	}
	batch, err := storage.heap.acquireBatch(credit, managedBatchHandles)
	if err != nil {
		return nil, err
	}
	batch.transactional = true
	return batch, nil
}

func rollbackMutationBatch(batch *managedAllocationBatch) error {
	if batch == nil {
		return nil
	}
	refs := batch.publishedRefs()
	batch.abortUnfinished()
	rollbackManagedPublications(batch.heap, batch, refs...)
	return batch.flush()
}

func (storage *managedMapStorage) storeEntryWithBatch(key vmMapKey, entry vmMapEntry, batch *managedAllocationBatch) error {
	if batch == nil && storage.batch == nil && managedMapValueNeedsAllocation(entry.Key, entry.Value) {
		local, err := storage.acquireMutationBatch()
		if err != nil {
			return err
		}
		working := *storage
		working.batch = local
		err = working.storeEntryWithBatch(key, entry, local)
		if err != nil {
			_ = rollbackMutationBatch(local)
			return err
		}
		if err := local.flush(); err != nil {
			_ = rollbackMutationBatch(local)
			return err
		}
		return nil
	}
	if batch != storage.batch {
		working := *storage
		working.batch = batch
		return working.storeEntryWithBatch(key, entry, batch)
	}
	header, err := storage.readHeader()
	if err != nil {
		return err
	}
	hash := managedMapKeyHash(key)
	index, _, _, err := storage.find(header, key, hash)
	if err != nil {
		return err
	}
	// Encode while the original table is intact; nested allocation may fail.
	var scratch [128]byte
	encoded := scratch[:min(storage.entryStride, uint32(len(scratch)))]
	if storage.entryStride > uint32(len(scratch)) {
		encoded = make([]byte, storage.entryStride)
	}
	module := storage.currentModule()
	seen := make(map[uintptr]guestRef)
	if err := module.writeInlineValueWithLayout(encoded, int(storage.valueOffset), entry.Value, storage.valueLayout, seen, storage.batch); err != nil {
		return err
	}
	if index != 0 {
		slab, offset, err := storage.location(header, index)
		if err != nil {
			return err
		}
		return storage.heap.write(slab, offset+storage.valueOffset, encoded[storage.valueOffset:storage.valueOffset+storage.valueLayout.Stride])
	}
	if err := module.writeInlineValueWithLayout(encoded, int(managedMapMetadata), entry.Key, storage.keyLayout, seen, storage.batch); err != nil {
		return err
	}
	index = header.free
	if index == 0 {
		if header.created == math.MaxUint32 {
			return errManagedLimit
		}
		index = header.created + 1
	}
	header, err = storage.ensureCapacity(header, index)
	if err != nil {
		return err
	}
	slab, offset, err := storage.location(header, index)
	if err != nil {
		return err
	}
	generation, err := storage.word(slab, offset+4)
	if err != nil {
		return err
	}
	if generation == math.MaxUint32 {
		return errors.New("retired managed map entry in free list")
	}
	nextFree, err := storage.word(slab, offset+12)
	if err != nil {
		return err
	}
	buckets, err := storage.heap.resolveObject(header.buckets)
	if err != nil {
		return err
	}
	bucket := hash % (buckets.size / 4) * 4
	head, err := storage.word(header.buckets, bucket)
	if err != nil {
		return err
	}
	for field, word := range [...]uint32{1, generation + 1, hash, head} {
		binary.LittleEndian.PutUint32(encoded[field*4:], word)
	}
	if err := storage.heap.write(slab, offset, encoded); err != nil {
		return err
	}
	if err := storage.writeWord(header.buckets, bucket, index); err != nil {
		return err
	}
	header.count++
	if header.free != 0 {
		header.free = nextFree
	} else {
		header.created = index
	}
	return storage.writeHeader(header)
}

func (storage *managedMapStorage) deleteEntryWithBatch(key vmMapKey, batch *managedAllocationBatch) error {
	if batch == nil {
		batch = storage.batch
	}
	if batch != storage.batch {
		working := *storage
		working.batch = batch
		return working.deleteEntryWithBatch(key, batch)
	}
	header, err := storage.readHeader()
	if err != nil {
		return err
	}
	hash := managedMapKeyHash(key)
	index, previous, entry, err := storage.find(header, key, hash)
	if err != nil || index == 0 {
		return err
	}
	slab, offset, err := storage.location(header, index)
	if err != nil {
		return err
	}
	next, err := storage.word(slab, offset+12)
	if err != nil {
		return err
	}
	if previous == 0 {
		buckets, err := storage.heap.resolveObject(header.buckets)
		if err != nil {
			return err
		}
		bucketCount := buckets.size / 4
		if bucketCount == 0 {
			return errors.New("managed map has an empty bucket table")
		}
		if err := storage.writeWord(header.buckets, hash%bucketCount*4, next); err != nil {
			return err
		}
	} else {
		before, beforeOffset, err := storage.location(header, previous)
		if err != nil {
			return err
		}
		if err := storage.writeWord(before, beforeOffset+12, next); err != nil {
			return err
		}
	}
	if err := storage.heap.clear(slab, offset, storage.entryStride); err != nil {
		return err
	}
	generation := uint32(entry.identity >> 32)
	if err := storage.writeWord(slab, offset+4, generation); err != nil {
		return err
	}
	if generation != math.MaxUint32 {
		if err := storage.writeWord(slab, offset+12, header.free); err != nil {
			return err
		}
		header.free = index
	}
	header.count--
	return storage.writeHeader(header)
}

func (storage *managedMapStorage) clearWithBatch(batch *managedAllocationBatch) error {
	if batch == nil {
		batch = storage.batch
	}
	if batch != storage.batch {
		working := *storage
		working.batch = batch
		return working.clearWithBatch(batch)
	}
	header, err := storage.readHeader()
	if err != nil {
		return err
	}
	if !header.buckets.valid() {
		return nil
	}
	buckets, err := storage.heap.resolveObject(header.buckets)
	if err != nil {
		return err
	}
	if err := storage.heap.clear(header.buckets, 0, buckets.size); err != nil {
		return err
	}
	header.free = 0
	for index := header.created; index > 0; index-- {
		slab, offset, err := storage.location(header, index)
		if err != nil {
			return err
		}
		generation, err := storage.word(slab, offset+4)
		if err != nil {
			return err
		}
		if err := storage.heap.clear(slab, offset, storage.entryStride); err != nil {
			return err
		}
		if err := storage.writeWord(slab, offset+4, generation); err != nil {
			return err
		}
		if generation != math.MaxUint32 {
			if err := storage.writeWord(slab, offset+12, header.free); err != nil {
				return err
			}
			header.free = index
		}
	}
	header.count = 0
	return storage.writeHeader(header)
}

func (storage *managedMapStorage) length() int {
	header, err := storage.readHeader()
	if err != nil {
		return 0
	}
	return int(header.count)
}

func (storage *managedMapStorage) loadEntry(key vmMapKey) (vmMapEntry, bool) {
	header, err := storage.readHeader()
	if err != nil {
		return vmMapEntry{}, false
	}
	index, _, entry, err := storage.find(header, key, managedMapKeyHash(key))
	return entry, index != 0 && err == nil
}

func (storage *managedMapStorage) loadIdentity(identity uint64) (vmMapEntry, bool) {
	if identity == 0 {
		return vmMapEntry{}, false
	}
	header, err := storage.readHeader()
	if err != nil {
		return vmMapEntry{}, false
	}
	entry, present, err := storage.readEntry(header, uint32(identity), uint32(identity>>32))
	return entry, present && err == nil
}

func (storage *managedMapStorage) entryIdentities() []uint64 {
	header, err := storage.readHeader()
	if err != nil {
		return nil
	}
	identities := make([]uint64, 0, header.count)
	for index := uint32(1); index <= header.created; index++ {
		slab, offset, err := storage.location(header, index)
		if err != nil {
			return nil
		}
		var raw [8]byte
		if err := storage.heap.read(slab, offset, raw[:]); err != nil {
			return nil
		}
		if binary.LittleEndian.Uint32(raw[:]) != 0 {
			identities = append(identities, uint64(binary.LittleEndian.Uint32(raw[4:]))<<32|uint64(index))
		}
	}
	return identities
}

func (storage *managedMapStorage) snapshot() (map[vmMapKey]vmMapEntry, error) {
	header, err := storage.readHeader()
	if err != nil {
		return nil, err
	}
	entries := make(map[vmMapKey]vmMapEntry, header.count)
	for index := uint32(1); index <= header.created; index++ {
		entry, present, err := storage.readEntry(header, index, 0)
		if err != nil {
			return nil, err
		}
		if present {
			key, err := storage.currentModule().mapKey(entry.Key)
			if err != nil {
				return nil, err
			}
			if key.NonReflexive {
				key.Sequence = entry.identity
			}
			entries[key] = entry
		}
	}
	return entries, nil
}
