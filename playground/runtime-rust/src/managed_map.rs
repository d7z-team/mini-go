//! Numeric managed storage for guest maps.
//!
//! A map is a small header which points at a directory of 64-entry slabs and
//! at a numeric bucket array.  Entries never move when a bucket array grows;
//! their `(index, generation)` pair is therefore a stable iterator identity.
//! Key and value bytes are only an encoding boundary.  The persistent object
//! graph is the managed page graph described by the registered layouts.

use crate::{
    error::RuntimeError,
    heap::{Handle, PreparedPayload, Trace},
    managed_storage::{StorageLayout, StorageRef, StorageWorkerPool},
    value::{Data, Value},
};
use std::collections::HashMap;

const ENTRIES_PER_SLAB: u32 = 64;
const ENTRY_METADATA: u32 = 16;
const OCCUPIED: u32 = 0;
const GENERATION: u32 = 4;
const HASH: u32 = 8;
const NEXT: u32 = 12;
const DIRECTORY_REF: u32 = 0;
const BUCKETS_REF: u32 = 8;
const SIZE: u32 = 16;
const MAX_INDEX: u32 = 20;
const CAPACITY: u32 = 24;
const FREE_HEAD: u32 = 28;
const BUCKET_COUNT: u32 = 32;
const DIRECTORY_COUNT: u32 = 36;
const HEADER_BYTES: u32 = 40;
const MAX_GENERATION: u32 = u32::MAX;

// The map header has a physical 40-byte layout (the two counters are kept
// in the record), while its logical contract is the record header plus the
// declared map identity payload.  Keep these values separate: collection
// walks the physical layout, and accounting charges the logical contract.
const MAP_HEADER_LOGICAL: u64 = crate::contract_generated::STORAGE_RECORD_HEADER
    + crate::contract_generated::STORAGE_MAP_HEADER;

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash)]
pub(crate) struct MapEntryIdentity {
    pub index: u32,
    pub generation: u32,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct MapValueBytes {
    pub bytes: Vec<u8>,
    /// References are offsets within `bytes` and are written through the
    /// managed heap reference API.  The offset list is supplied by the
    /// prepared value codec, so this module never scans an opaque payload.
    pub references: Vec<(u32, StorageRef)>,
}

impl MapValueBytes {
    /// Adapts one prepared VM payload to an entry lane. The map retains only
    /// numeric bytes and reference lanes, never the source `Value` graph.
    pub(crate) fn from_prepared(payload: PreparedPayload) -> Self {
        Self {
            bytes: payload.bytes,
            references: payload.references,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct ManagedMapEntry {
    pub identity: MapEntryIdentity,
    pub key: Vec<u8>,
    pub key_references: Vec<(u32, StorageRef)>,
    pub value: Vec<u8>,
    pub value_references: Vec<(u32, StorageRef)>,
}

#[derive(Clone, Debug)]
pub(crate) struct MapValueLayout {
    pub stride: u32,
    pub references: Vec<u32>,
    pub dynamics: Vec<u32>,
    pub logical: u64,
}

#[derive(Clone, Copy, Debug)]
struct LayoutIds {
    header: u32,
    directory: u32,
    bucket: u32,
    entry: u32,
}

pub(crate) struct ManagedMap {
    pool: StorageWorkerPool,
    handle: StorageRef,
    ids: LayoutIds,
    layout_leases: Vec<crate::managed_storage::StorageLayoutLease>,
    key: MapValueLayout,
    value: MapValueLayout,
    key_offset: u32,
    value_offset: u32,
    entry_stride: u32,
    closed: bool,
    // The guest header is an Any storage record, so keep the map's structural
    // identity on the same owner record as its numeric storage.
    typ: Option<crate::types::TypeIdentity>,
}

/// Instance-owned managed map registry. The registry contains only map
/// control objects and guest identity; payload bytes, entry slabs, and child
/// edges remain in `StorageHeap`. Keeping this owner separate from `Value`
/// avoids resurrecting a second host `Vec<Value>` map graph.
#[derive(Default)]
pub(crate) struct ManagedMapOwner {
    maps: HashMap<Handle, ManagedMap>,
}

/// Hashes the guest value semantics used by map equality. Map storage keeps
/// prepared bytes and reference lanes, but those are allocation details: a
/// copied string, slice, or aggregate must hash exactly like the original
/// value. All production callers therefore hash the evaluated key before
/// encoding it into a map entry and use the decoded value for collision
/// equality.
pub(crate) fn hash_value(value: &Value) -> Result<u32, RuntimeError> {
    fn feed(hash: &mut u32, bytes: &[u8]) {
        for byte in bytes {
            *hash ^= u32::from(*byte);
            *hash = hash.wrapping_mul(0x0100_0193);
        }
    }
    fn tag(hash: &mut u32, value: u8) {
        feed(hash, &[value]);
    }
    fn number(hash: &mut u32, value: u64) {
        feed(hash, &value.to_le_bytes());
    }
    fn float(hash: &mut u32, value: f64) {
        let bits = if value == 0.0 {
            0
        } else if value.is_nan() {
            f64::NAN.to_bits()
        } else {
            value.to_bits()
        };
        number(hash, bits);
    }
    fn visit(hash: &mut u32, value: &Value, depth: usize) -> Result<(), RuntimeError> {
        if depth > 256 {
            tag(hash, 0xff);
            return Ok(());
        }
        match &value.data {
            Data::Uninitialized => tag(hash, 0),
            Data::Nil => tag(hash, 1),
            Data::Bool(value) => {
                tag(hash, 2);
                tag(hash, u8::from(*value));
            }
            Data::Integer(value) => {
                tag(hash, 3);
                number(hash, *value as u64);
            }
            Data::Unsigned(value) => {
                tag(hash, 4);
                number(hash, *value);
            }
            Data::Float(value) => {
                tag(hash, 5);
                float(hash, *value);
            }
            Data::Complex { real, imag } => {
                tag(hash, 6);
                float(hash, *real);
                float(hash, *imag);
            }
            Data::String(value) => {
                tag(hash, 7);
                feed(hash, value);
            }
            Data::OpaqueJSON(value) => {
                tag(hash, 8);
                feed(hash, value);
            }
            Data::Bytes(value) => {
                tag(hash, 9);
                feed(hash, value);
            }
            Data::Array(values) => {
                tag(hash, 10);
                number(hash, values.len() as u64);
                for value in values.iter() {
                    visit(hash, value, depth + 1)?;
                }
            }
            Data::Struct(fields) => {
                tag(hash, 11);
                number(
                    hash,
                    fields
                        .iter()
                        .filter(|(name, _)| name.as_str() != "_")
                        .count() as u64,
                );
                for (name, value) in fields {
                    if name.as_str() != "_" {
                        tag(hash, 0x31);
                        feed(hash, name.as_bytes());
                        visit(hash, value, depth + 1)?;
                    }
                }
            }
            Data::Interface(value) => {
                // `operators::equal` unwraps interfaces before comparing the
                // concrete value. Do the same here; hashing the wrapper or
                // its storage identity would make an interface key differ
                // from an equal direct key.
                visit(hash, value, depth)?;
            }
            Data::DynamicFunction(value) => {
                // Dynamic functions are not comparable map keys. Keep a
                // deterministic semantic shape for diagnostics and callers
                // that hash before the comparability check.
                tag(hash, 12);
                visit(hash, value, depth + 1)?;
            }
            Data::Pointer(address) => {
                tag(hash, 13);
                use std::hash::{Hash, Hasher};
                let mut state = std::collections::hash_map::DefaultHasher::new();
                address.hash(&mut state);
                number(hash, state.finish());
            }
            Data::ResourceRef(handle) | Data::Map(handle) => {
                tag(hash, 14);
                number(hash, handle.slot as u64);
                number(hash, handle.generation as u64);
            }
            Data::Slice(slice) => {
                tag(hash, 15);
                use std::hash::{Hash, Hasher};
                let mut state = std::collections::hash_map::DefaultHasher::new();
                slice.storage.hash(&mut state);
                number(hash, state.finish());
                number(hash, slice.start as u64);
                number(hash, slice.length as u64);
            }
            Data::Resource(resource) => {
                tag(hash, 16);
                let mut handles = Vec::new();
                resource.trace(&mut |handle| handles.push(handle));
                for handle in handles {
                    number(hash, handle.slot as u64);
                    number(hash, handle.generation as u64);
                }
            }
            Data::Function(function) | Data::Method { function, .. } => {
                tag(hash, 17);
                number(hash, function.index.unwrap_or(usize::MAX) as u64);
                number(hash, function.revision_generation.unwrap_or_default());
                if let Some(anchor) = function.revision_anchor {
                    number(hash, anchor.slot as u64);
                    number(hash, anchor.generation as u64);
                }
            }
            Data::Aggregate(value) => {
                tag(hash, if value.is_array() { 10 } else { 11 });
                let count = (0..value.len())
                    .filter(|index| value.field_name(*index) != Some("_"))
                    .count();
                number(hash, count as u64);
                for index in 0..value.len() {
                    if let Some(name) = value.field_name(index) {
                        if name == "_" {
                            continue;
                        }
                        tag(hash, 0x31);
                        feed(hash, name.as_bytes());
                    }
                    visit(hash, &value.get(index)?, depth + 1)?;
                }
            }
        }
        Ok(())
    }

    let mut hash = 0x811c_9dc5_u32;
    visit(&mut hash, value, 0)?;
    Ok(hash)
}

impl ManagedMapOwner {
    pub(crate) fn insert(&mut self, guest: Handle, map: ManagedMap) -> Result<(), RuntimeError> {
        if self.maps.contains_key(&guest) {
            return Err(map_error("managed map guest handle is already registered"));
        }
        self.maps.insert(guest, map);
        Ok(())
    }

    pub(crate) fn set_type(
        &mut self,
        guest: Handle,
        typ: crate::types::TypeIdentity,
    ) -> Result<(), RuntimeError> {
        let map = self
            .maps
            .get_mut(&guest)
            .ok_or_else(|| map_error("managed map guest handle is not registered"))?;
        map.typ = Some(typ);
        Ok(())
    }

    pub(crate) fn get(&self, guest: Handle) -> Option<&ManagedMap> {
        self.maps.get(&guest)
    }

    pub(crate) fn type_of(&self, guest: Handle) -> Option<crate::types::TypeIdentity> {
        self.maps.get(&guest).and_then(|map| map.typ.clone())
    }

    pub(crate) fn get_mut(&mut self, guest: Handle) -> Option<&mut ManagedMap> {
        self.maps.get_mut(&guest)
    }

    pub(crate) fn remove(&mut self, guest: Handle) -> Option<ManagedMap> {
        self.maps.remove(&guest)
    }

    pub(crate) fn guests(&self) -> impl Iterator<Item = Handle> + '_ {
        self.maps.keys().copied()
    }

    pub(crate) fn dynamic_type_roots(
        &self,
    ) -> impl Iterator<Item = crate::types::TypeIdentity> + '_ {
        self.maps.values().filter_map(|map| map.typ.clone())
    }
}

impl ManagedMap {
    #[cfg(test)]
    pub(crate) fn new(
        pool: StorageWorkerPool,
        key: MapValueLayout,
        value: MapValueLayout,
    ) -> Result<Self, RuntimeError> {
        Self::build(pool, key, value, None)
    }

    /// Attach the numeric map metadata to a preallocated heap record. The
    /// stable slot is rewritten to the map header layout after all map layouts
    /// register successfully, avoiding a second owning header allocation when
    /// a VM map handle already exists.
    pub(crate) fn attach(
        pool: StorageWorkerPool,
        handle: StorageRef,
        key: MapValueLayout,
        value: MapValueLayout,
    ) -> Result<Self, RuntimeError> {
        let map = Self::build(pool, key, value, Some(handle))?;
        // A guest map anchor is allocated as an ordinary typed record before
        // its owner is known. Publish the map header layout in-place only
        // after every map layout registration succeeded; the slot/generation
        // ticket remains stable for Value::Map references.
        map.pool.rewrite(
            handle,
            map.ids.header,
            1,
            MAP_HEADER_LOGICAL,
            &vec![0; HEADER_BYTES as usize],
        )?;
        Ok(map)
    }

    fn build(
        pool: StorageWorkerPool,
        key: MapValueLayout,
        value: MapValueLayout,
        existing: Option<StorageRef>,
    ) -> Result<Self, RuntimeError> {
        validate_value_layout(&key)?;
        validate_value_layout(&value)?;
        let key_offset = ENTRY_METADATA;
        let value_offset = align8(
            key_offset
                .checked_add(key.stride)
                .ok_or_else(|| map_error("map key layout overflows"))?,
        )?;
        let entry_stride = align8(
            value_offset
                .checked_add(value.stride)
                .ok_or_else(|| map_error("map value layout overflows"))?,
        )?;
        let mut references = key
            .references
            .iter()
            .map(|offset| key_offset + *offset)
            .collect::<Vec<_>>();
        references.extend(value.references.iter().map(|offset| value_offset + *offset));
        let mut dynamics = key
            .dynamics
            .iter()
            .map(|offset| key_offset + *offset)
            .collect::<Vec<_>>();
        dynamics.extend(value.dynamics.iter().map(|offset| value_offset + *offset));
        let ids = LayoutIds {
            header: pool.register_layout(StorageLayout {
                stride: HEADER_BYTES,
                references: vec![DIRECTORY_REF, BUCKETS_REF],
                dynamics: Vec::new(),
            })?,
            directory: pool.register_layout(StorageLayout {
                stride: 8,
                references: vec![0],
                dynamics: Vec::new(),
            })?,
            bucket: pool.register_layout(StorageLayout {
                stride: 4,
                references: Vec::new(),
                dynamics: Vec::new(),
            })?,
            entry: pool.register_layout(StorageLayout {
                stride: entry_stride,
                references,
                dynamics,
            })?,
        };
        let layout_leases = [ids.header, ids.directory, ids.bucket, ids.entry]
            .into_iter()
            .map(|layout| pool.pin_layout(layout))
            .collect::<Result<Vec<_>, _>>()?;
        let handle = match existing {
            Some(handle) => handle,
            None => allocate(&pool, ids.header, 1, MAP_HEADER_LOGICAL)?,
        };
        Ok(Self {
            pool,
            handle,
            ids,
            layout_leases,
            key,
            value,
            key_offset,
            value_offset,
            entry_stride,
            closed: false,
            typ: None,
        })
    }

    #[cfg(test)]
    pub(crate) fn handle(&self) -> StorageRef {
        self.handle
    }

    pub(crate) fn len(&self) -> Result<u32, RuntimeError> {
        self.word(self.handle, SIZE)
    }

    pub(crate) fn capacity(&self) -> Result<u32, RuntimeError> {
        self.word(self.handle, CAPACITY)
    }

    pub(crate) fn snapshot(&self) -> Result<Vec<MapEntryIdentity>, RuntimeError> {
        let max = self.word(self.handle, MAX_INDEX)?;
        let mut result = Vec::with_capacity(self.len()?.min(max) as usize);
        for index in 1..=max {
            if let Some(entry) = self.entry_identity(index)? {
                result.push(entry);
            }
        }
        Ok(result)
    }

    pub(crate) fn entry(
        &self,
        identity: MapEntryIdentity,
    ) -> Result<Option<ManagedMapEntry>, RuntimeError> {
        let Some((slab, offset)) = self.location(identity.index)? else {
            return Ok(None);
        };
        if self.word(slab, offset + OCCUPIED)? == 0
            || self.word(slab, offset + GENERATION)? != identity.generation
        {
            return Ok(None);
        }
        let key = self.read_value(slab, offset + self.key_offset, &self.key)?;
        let value = self.read_value(slab, offset + self.value_offset, &self.value)?;
        Ok(Some(ManagedMapEntry {
            identity,
            key: key.bytes,
            key_references: key.references,
            value: value.bytes,
            value_references: value.references,
        }))
    }

    /// Locate an entry while retaining the prepared lane's child references.
    /// Boxed/dynamic map keys cannot be compared from bytes alone; consumers
    /// use this form to decode each candidate through the owning heap.
    pub(crate) fn find_lane<F>(
        &self,
        hash: u32,
        mut matches: F,
    ) -> Result<Option<MapEntryIdentity>, RuntimeError>
    where
        F: FnMut(&MapValueBytes) -> bool,
    {
        let Some(buckets) = self.reference(self.handle, BUCKETS_REF)? else {
            return Ok(None);
        };
        let count = self.word(self.handle, BUCKET_COUNT)?;
        if count == 0 {
            return Ok(None);
        }
        let mut index = self.word(buckets, (hash % count) * 4)?;
        while index != 0 {
            let Some((slab, offset)) = self.location(index)? else {
                return Err(stale_error());
            };
            let next = self.word(slab, offset + NEXT)?;
            if self.word(slab, offset + OCCUPIED)? != 0
                && self.word(slab, offset + HASH)? == hash
                && matches(&self.read_value(slab, offset + self.key_offset, &self.key)?)
            {
                return Ok(Some(MapEntryIdentity {
                    index,
                    generation: self.word(slab, offset + GENERATION)?,
                }));
            }
            index = next;
        }
        Ok(None)
    }

    /// Insert or update an entry using a byte-only equality predicate.
    #[cfg(test)]
    pub(crate) fn set<F>(
        &mut self,
        hash: u32,
        key: MapValueBytes,
        value: MapValueBytes,
        mut matches: F,
    ) -> Result<MapEntryIdentity, RuntimeError>
    where
        F: FnMut(&[u8]) -> bool,
    {
        self.set_lane(hash, key, value, |candidate| matches(&candidate.bytes))
    }

    /// Insert/update using the complete prepared key lane. This is the map
    /// entry point for dynamic or boxed keys whose equality needs child
    /// storage references.
    pub(crate) fn set_lane<F>(
        &mut self,
        hash: u32,
        key: MapValueBytes,
        value: MapValueBytes,
        mut matches: F,
    ) -> Result<MapEntryIdentity, RuntimeError>
    where
        F: FnMut(&MapValueBytes) -> bool,
    {
        if key.bytes.len() as u32 != self.key.stride
            || value.bytes.len() as u32 != self.value.stride
        {
            return Err(map_error("map value does not match prepared layout"));
        }
        if let Some(identity) = self.find_lane(hash, |candidate| matches(candidate))? {
            let (slab, offset) = self.location(identity.index)?.ok_or_else(stale_error)?;
            self.write_value(slab, offset + self.value_offset, &value, &self.value)?;
            return Ok(identity);
        }
        // Pick an entry without mutating the free list.  Capacity growth can
        // fail under the guest/resident budget; consuming a reusable entry
        // before that admission would make an otherwise failed write visible
        // through the free-list metadata.
        let free_head = self.word(self.handle, FREE_HEAD)?;
        let index = if free_head != 0 {
            free_head
        } else {
            self.word(self.handle, MAX_INDEX)?
                .checked_add(1)
                .ok_or_else(|| map_error("map entry index exhausted"))?
        };
        self.ensure_capacity(index)?;
        if free_head != 0 {
            let (free_slab, free_offset) = self.location(free_head)?.ok_or_else(stale_error)?;
            self.write_word(
                self.handle,
                FREE_HEAD,
                self.word(free_slab, free_offset + NEXT)?,
            )?;
        }
        let (slab, offset) = self.location(index)?.ok_or_else(stale_error)?;
        let generation = self
            .word(slab, offset + GENERATION)?
            .checked_add(1)
            .ok_or_else(|| map_error("map entry generation exhausted"))?;
        if generation == 0 {
            return Err(map_error("map entry generation exhausted"));
        }
        let buckets = self
            .reference(self.handle, BUCKETS_REF)?
            .ok_or_else(stale_error)?;
        let count = self.word(self.handle, BUCKET_COUNT)?;
        let bucket = hash % count;
        let head = self.word(buckets, bucket * 4)?;
        self.write_value(slab, offset + self.key_offset, &key, &self.key)?;
        self.write_value(slab, offset + self.value_offset, &value, &self.value)?;
        self.write_word(slab, offset + GENERATION, generation)?;
        self.write_word(slab, offset + HASH, hash)?;
        self.write_word(slab, offset + NEXT, head)?;
        self.write_word(slab, offset + OCCUPIED, 1)?;
        self.write_word(buckets, bucket * 4, index)?;
        self.write_word(self.handle, SIZE, self.word(self.handle, SIZE)? + 1)?;
        self.write_word(
            self.handle,
            MAX_INDEX,
            self.word(self.handle, MAX_INDEX)?.max(index),
        )?;
        Ok(MapEntryIdentity { index, generation })
    }

    #[cfg(test)]
    pub(crate) fn delete<F>(&mut self, hash: u32, mut matches: F) -> Result<bool, RuntimeError>
    where
        F: FnMut(&[u8]) -> bool,
    {
        self.delete_lane(hash, |candidate| matches(&candidate.bytes))
    }

    pub(crate) fn delete_lane<F>(&mut self, hash: u32, mut matches: F) -> Result<bool, RuntimeError>
    where
        F: FnMut(&MapValueBytes) -> bool,
    {
        let Some(buckets) = self.reference(self.handle, BUCKETS_REF)? else {
            return Ok(false);
        };
        let count = self.word(self.handle, BUCKET_COUNT)?;
        if count == 0 {
            return Ok(false);
        }
        let bucket = hash % count;
        let mut index = self.word(buckets, bucket * 4)?;
        let mut previous = 0;
        while index != 0 {
            let (slab, offset) = self.location(index)?.ok_or_else(stale_error)?;
            let next = self.word(slab, offset + NEXT)?;
            if self.word(slab, offset + OCCUPIED)? != 0
                && self.word(slab, offset + HASH)? == hash
                && matches(&self.read_value(slab, offset + self.key_offset, &self.key)?)
            {
                if previous == 0 {
                    self.write_word(buckets, bucket * 4, next)?;
                } else {
                    let (before, before_offset) =
                        self.location(previous)?.ok_or_else(stale_error)?;
                    self.write_word(before, before_offset + NEXT, next)?;
                }
                let generation = self.word(slab, offset + GENERATION)?;
                self.clear_range(slab, offset, self.entry_stride)?;
                self.write_word(slab, offset + GENERATION, generation)?;
                if generation != MAX_GENERATION {
                    let free = self.word(self.handle, FREE_HEAD)?;
                    self.write_word(slab, offset + NEXT, free)?;
                    self.write_word(self.handle, FREE_HEAD, index)?;
                }
                self.write_word(
                    self.handle,
                    SIZE,
                    self.word(self.handle, SIZE)?.saturating_sub(1),
                )?;
                return Ok(true);
            }
            previous = index;
            index = next;
        }
        Ok(false)
    }

    pub(crate) fn clear(&mut self) -> Result<(), RuntimeError> {
        let Some(buckets) = self.reference(self.handle, BUCKETS_REF)? else {
            return Ok(());
        };
        self.clear_range(buckets, 0, self.word(self.handle, BUCKET_COUNT)? * 4)?;
        let mut free = 0;
        let max = self.word(self.handle, MAX_INDEX)?;
        for index in 1..=max {
            let Some((slab, offset)) = self.location(index)? else {
                return Err(stale_error());
            };
            let generation = self.word(slab, offset + GENERATION)?;
            self.clear_range(slab, offset, self.entry_stride)?;
            self.write_word(slab, offset + GENERATION, generation)?;
            if generation != MAX_GENERATION {
                self.write_word(slab, offset + NEXT, free)?;
                free = index;
            }
        }
        self.write_word(self.handle, SIZE, 0)?;
        self.write_word(self.handle, FREE_HEAD, free)
    }

    pub(crate) fn close(&mut self) -> Result<(), RuntimeError> {
        if self.closed {
            return Ok(());
        }
        {
            let mut heap = self.pool.lock()?;
            heap.release(self.handle)?;
        }
        self.layout_leases.clear();
        self.closed = true;
        Ok(())
    }

    fn ensure_capacity(&mut self, index: u32) -> Result<(), RuntimeError> {
        let capacity = self.word(self.handle, CAPACITY)?;
        if index <= capacity {
            return Ok(());
        }
        let new_capacity = capacity
            .checked_add(ENTRIES_PER_SLAB)
            .ok_or_else(|| map_error("map storage capacity exhausted"))?;
        let slab_count = (capacity / ENTRIES_PER_SLAB) as usize;
        let new_slab = allocate(
            &self.pool,
            self.ids.entry,
            ENTRIES_PER_SLAB,
            crate::contract_generated::STORAGE_RECORD_HEADER
                .checked_add(
                    (u64::from(ENTRY_METADATA) + self.key.logical + self.value.logical)
                        .checked_mul(u64::from(ENTRIES_PER_SLAB))
                        .ok_or_else(|| map_error("map entry slab size overflows"))?,
                )
                .ok_or_else(|| map_error("map entry slab size overflows"))?,
        )?;
        let mut pending = vec![new_slab];
        let old_directory = self.reference(self.handle, DIRECTORY_REF)?;
        let directory_capacity = self.word(self.handle, DIRECTORY_COUNT)?;
        let (directory, directory_count) = if directory_capacity > slab_count as u32 {
            (old_directory.ok_or_else(stale_error)?, directory_capacity)
        } else {
            let count = (slab_count.max(1) * 2).max(8) as u32;
            let logical = crate::contract_generated::STORAGE_RECORD_HEADER
                .checked_add(
                    u64::from(count)
                        .checked_mul(crate::contract_generated::STORAGE_REFERENCE)
                        .ok_or_else(|| map_error("map directory size overflows"))?,
                )
                .ok_or_else(|| map_error("map directory size overflows"))?;
            let next = match allocate(&self.pool, self.ids.directory, count, logical) {
                Ok(next) => next,
                Err(error) => {
                    release_all(&self.pool, &pending);
                    return Err(error);
                }
            };
            pending.push(next);
            if let Some(old) = old_directory
                && let Err(error) = self.copy(old, next, slab_count as u32 * 8)
            {
                release_all(&self.pool, &pending);
                return Err(error);
            }
            (next, count)
        };
        let old_buckets = self.reference(self.handle, BUCKETS_REF)?;
        let old_bucket_count = self.word(self.handle, BUCKET_COUNT)?;
        let bucket_count = if old_bucket_count
            < new_capacity
                .checked_mul(2)
                .ok_or_else(|| map_error("map bucket count exhausted"))?
        {
            new_capacity
                .checked_mul(2)
                .and_then(u32::checked_next_power_of_two)
                .ok_or_else(|| map_error("map bucket count exhausted"))?
                .max(8)
        } else {
            old_bucket_count
        };
        let buckets = if bucket_count == old_bucket_count {
            old_buckets.ok_or_else(stale_error)?
        } else {
            let next = match allocate(
                &self.pool,
                self.ids.bucket,
                bucket_count,
                crate::contract_generated::STORAGE_RECORD_HEADER
                    .checked_add(
                        u64::from(bucket_count)
                            .checked_mul(crate::contract_generated::STORAGE_MAP_BUCKET)
                            .ok_or_else(|| map_error("map bucket size overflows"))?,
                    )
                    .ok_or_else(|| map_error("map bucket size overflows"))?,
            ) {
                Ok(next) => next,
                Err(error) => {
                    release_all(&self.pool, &pending);
                    return Err(error);
                }
            };
            pending.push(next);
            next
        };
        // Every allocation above has succeeded. Publish directory/slab and
        // buckets only after the new pages are initialized and rehashed.
        self.write_reference(directory, slab_count as u32 * 8, new_slab)?;
        if buckets != old_buckets.unwrap_or(StorageRef::NULL) {
            let max = self.word(self.handle, MAX_INDEX)?;
            for current in 1..=max {
                let Some((slab, offset)) = self.location(current)? else {
                    return Err(stale_error());
                };
                if self.word(slab, offset + OCCUPIED)? == 0 {
                    continue;
                }
                let bucket = self.word(slab, offset + HASH)? % bucket_count;
                let head = self.word(buckets, bucket * 4)?;
                self.write_word(slab, offset + NEXT, head)?;
                self.write_word(buckets, bucket * 4, current)?;
            }
        }
        self.write_reference(self.handle, DIRECTORY_REF, directory)?;
        self.write_reference(self.handle, BUCKETS_REF, buckets)?;
        self.write_word(self.handle, DIRECTORY_COUNT, directory_count)?;
        self.write_word(self.handle, BUCKET_COUNT, bucket_count)?;
        self.write_word(self.handle, CAPACITY, new_capacity)?;
        self.write_word(
            self.handle,
            MAX_INDEX,
            self.word(self.handle, MAX_INDEX)?.max(index),
        )
    }

    fn location(&self, index: u32) -> Result<Option<(StorageRef, u32)>, RuntimeError> {
        if index == 0 || index > self.word(self.handle, CAPACITY)? {
            return Ok(None);
        }
        let directory = self
            .reference(self.handle, DIRECTORY_REF)?
            .ok_or_else(stale_error)?;
        let slab = self.reference(directory, (index - 1) / ENTRIES_PER_SLAB * 8)?;
        Ok(slab.map(|handle| (handle, ((index - 1) % ENTRIES_PER_SLAB) * self.entry_stride)))
    }

    fn entry_identity(&self, index: u32) -> Result<Option<MapEntryIdentity>, RuntimeError> {
        let Some((slab, offset)) = self.location(index)? else {
            return Ok(None);
        };
        if self.word(slab, offset + OCCUPIED)? == 0 {
            return Ok(None);
        }
        Ok(Some(MapEntryIdentity {
            index,
            generation: self.word(slab, offset + GENERATION)?,
        }))
    }

    fn read_value(
        &self,
        handle: StorageRef,
        offset: u32,
        layout: &MapValueLayout,
    ) -> Result<MapValueBytes, RuntimeError> {
        let heap = self.pool.read_lock()?;
        let bytes = heap.read(handle, offset, layout.stride)?;
        let mut references = Vec::with_capacity(layout.references.len() + layout.dynamics.len());
        for relative in &layout.references {
            let child = heap.read_reference(handle, offset + *relative)?;
            if child != StorageRef::NULL {
                references.push((*relative, child));
            }
        }
        for dynamic in &layout.dynamics {
            let relative = dynamic
                .checked_add(8)
                .ok_or_else(|| map_error("map dynamic lane reference offset overflows"))?;
            let child = heap.read_reference(handle, offset + relative)?;
            if child != StorageRef::NULL {
                references.push((relative, child));
            }
        }
        Ok(MapValueBytes { bytes, references })
    }

    fn write_value(
        &self,
        handle: StorageRef,
        offset: u32,
        value: &MapValueBytes,
        layout: &MapValueLayout,
    ) -> Result<(), RuntimeError> {
        if value.bytes.len() as u32 != layout.stride {
            return Err(map_error("map value does not match prepared layout"));
        }
        let mut heap = self.pool.lock()?;
        for (relative, child) in &value.references {
            if !layout.references.contains(relative)
                && !layout
                    .dynamics
                    .iter()
                    .any(|dynamic| dynamic.checked_add(8) == Some(*relative))
            {
                return Err(map_error("map value reference is outside prepared layout"));
            }
            if *child != StorageRef::NULL {
                heap.get(*child)?;
            }
        }
        heap.write(handle, offset, &value.bytes)?;
        for (relative, child) in &value.references {
            heap.write_reference(handle, offset + *relative, *child)?;
        }
        Ok(())
    }

    fn write_word(&self, handle: StorageRef, offset: u32, value: u32) -> Result<(), RuntimeError> {
        self.pool
            .lock()?
            .write(handle, offset, &value.to_le_bytes())
    }

    fn write_reference(
        &self,
        handle: StorageRef,
        offset: u32,
        value: StorageRef,
    ) -> Result<(), RuntimeError> {
        self.pool.lock()?.write_reference(handle, offset, value)
    }

    fn reference(
        &self,
        handle: StorageRef,
        offset: u32,
    ) -> Result<Option<StorageRef>, RuntimeError> {
        let value = self.pool.read_lock()?.read_reference(handle, offset)?;
        Ok((value != StorageRef::NULL).then_some(value))
    }

    fn word(&self, handle: StorageRef, offset: u32) -> Result<u32, RuntimeError> {
        let bytes = self.pool.read_lock()?.read(handle, offset, 4)?;
        Ok(u32::from_le_bytes(bytes.try_into().unwrap()))
    }

    fn clear_range(
        &self,
        handle: StorageRef,
        offset: u32,
        length: u32,
    ) -> Result<(), RuntimeError> {
        self.pool
            .lock()?
            .write(handle, offset, &vec![0; length as usize])
    }

    fn copy(
        &self,
        source: StorageRef,
        target: StorageRef,
        length: u32,
    ) -> Result<(), RuntimeError> {
        let bytes = self.pool.read_lock()?.read(source, 0, length)?;
        self.pool.lock()?.write(target, 0, &bytes)
    }
}

impl Drop for ManagedMap {
    fn drop(&mut self) {
        let _ = self.close();
    }
}

fn allocate(
    pool: &StorageWorkerPool,
    layout: u32,
    count: u32,
    logical: u64,
) -> Result<StorageRef, RuntimeError> {
    let mut heap = pool.lock()?;
    let mut reservation = heap.reserve(layout, count, logical)?;
    reservation.bytes()?.fill(0);
    Ok(reservation.commit())
}

fn release_all(pool: &StorageWorkerPool, handles: &[StorageRef]) {
    if let Ok(mut heap) = pool.lock() {
        for handle in handles {
            let _ = heap.release(*handle);
        }
    }
}

/// Validate map lanes before registering the entry layout.  The storage
/// registry performs the same checks for the combined entry layout, but doing
/// it here keeps a malformed key/value descriptor from partially registering
/// the map's header, directory, or bucket layouts first.
fn validate_value_layout(layout: &MapValueLayout) -> Result<(), RuntimeError> {
    for offset in &layout.references {
        if offset.checked_add(8).is_none_or(|end| end > layout.stride) {
            return Err(map_error("map value reference is outside its layout"));
        }
    }
    for offset in &layout.dynamics {
        if offset.checked_add(24).is_none_or(|end| end > layout.stride) {
            return Err(map_error("map value dynamic lane is outside its layout"));
        }
    }
    if layout.references.windows(2).any(|pair| pair[0] >= pair[1])
        || layout.dynamics.windows(2).any(|pair| pair[0] >= pair[1])
    {
        return Err(map_error(
            "map value layout offsets must be sorted and unique",
        ));
    }
    if layout.dynamics.iter().any(|dynamic| {
        layout.references.iter().any(|reference| {
            *reference < dynamic.saturating_add(24) && *dynamic < reference.saturating_add(8)
        })
    }) {
        return Err(map_error("map value layout lanes overlap"));
    }
    Ok(())
}

fn align8(value: u32) -> Result<u32, RuntimeError> {
    value
        .checked_add(7)
        .map(|value| value & !7)
        .ok_or_else(|| map_error("map layout alignment overflows"))
}

fn map_error(message: &str) -> RuntimeError {
    RuntimeError::new("invalid_map", "managed map", message)
}

fn stale_error() -> RuntimeError {
    RuntimeError::new(
        "stale_reference",
        "managed map",
        "map storage reference is stale",
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::contract_generated as wire;
    use crate::managed_storage::StorageHeap;
    use crate::types::TypeIdentity;
    use crate::{heap::Heap, value::Value};

    fn map(max_guest_bytes: Option<u64>) -> ManagedMap {
        let pool = StorageHeap::new(max_guest_bytes, None, 4 << 20).into_worker_pool();
        ManagedMap::new(
            pool,
            MapValueLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
                logical: 8,
            },
            MapValueLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
                logical: 8,
            },
        )
        .unwrap()
    }

    #[test]
    fn managed_map_allocation_costs_include_record_headers() {
        let mut map = map(None);
        let vectors: Vec<serde_json::Value> =
            serde_json::from_str(include_str!("../../../testdata/runtime/storage_cost.json"))
                .unwrap();
        let vector = |name: &str| {
            vectors
                .iter()
                .find(|vector| vector["name"] == name)
                .unwrap()
        };
        // The physical header is 40 bytes, while the shared logical contract
        // charges record header (16) plus map identity payload (32).
        assert_eq!(
            map.pool.stats().unwrap().occupied_bytes,
            vector("empty_map")["bytes"].as_u64().unwrap()
        );

        map.set(1, bytes(11), bytes(101), |_| false).unwrap();
        // First growth publishes one entry slab, one directory and one
        // bucket page. Each independent storage object carries one record
        // header in addition to its payload.
        let slab = 16 + 64 * (16 + 8 + 8);
        let directory = 16 + 8 * 8;
        let buckets = 16 + 128 * 4;
        assert_eq!(
            map.pool.stats().unwrap().occupied_bytes,
            48 + slab + directory + buckets
        );
        assert_eq!(
            map.pool.stats().unwrap().occupied_bytes,
            vector("populated_map")["bytes"].as_u64().unwrap()
        );
    }

    fn bytes(value: u64) -> MapValueBytes {
        MapValueBytes {
            bytes: value.to_le_bytes().to_vec(),
            references: Vec::new(),
        }
    }

    #[test]
    fn rejects_invalid_value_lanes_before_registering_map_storage() {
        let pool = StorageHeap::new(None, None, 4 << 20).into_worker_pool();
        let error = match ManagedMap::new(
            pool.clone(),
            MapValueLayout {
                stride: 8,
                references: vec![4],
                dynamics: Vec::new(),
                logical: 8,
            },
            MapValueLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
                logical: 8,
            },
        ) {
            Ok(_) => panic!("invalid map layout was accepted"),
            Err(error) => error,
        };
        assert_eq!(error.code, "invalid_map");

        let error = match ManagedMap::new(
            pool,
            MapValueLayout {
                stride: 24,
                references: vec![8],
                dynamics: vec![0],
                logical: 24,
            },
            MapValueLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
                logical: 8,
            },
        ) {
            Ok(_) => panic!("overlapping map layout was accepted"),
            Err(error) => error,
        };
        assert_eq!(error.code, "invalid_map");
    }

    #[test]
    fn stable_slab_identity_survives_delete_reinsert_and_iterator_snapshot() {
        let mut map = map(None);
        let first = map
            .set(1, bytes(11), bytes(101), |key| key == 11u64.to_le_bytes())
            .unwrap();
        let second = map
            .set(2, bytes(22), bytes(202), |key| key == 22u64.to_le_bytes())
            .unwrap();
        let snapshot = map.snapshot().unwrap();
        assert!(map.delete(1, |key| key == 11u64.to_le_bytes()).unwrap());
        let replacement = map
            .set(1, bytes(11), bytes(303), |key| key == 11u64.to_le_bytes())
            .unwrap();
        assert_eq!(first.index, replacement.index);
        assert_ne!(first.generation, replacement.generation);
        assert!(map.entry(first).unwrap().is_none());
        assert_eq!(
            map.entry(second).unwrap().unwrap().value,
            202u64.to_le_bytes()
        );
        assert_eq!(
            map.entry(snapshot[1]).unwrap().unwrap().value,
            202u64.to_le_bytes()
        );
    }

    #[test]
    fn growth_rehash_preserves_entries_and_numeric_identities() {
        let mut map = map(None);
        let mut identities = Vec::new();
        for index in 1..=130u64 {
            let key = index.to_le_bytes().to_vec();
            identities.push(
                map.set(
                    index as u32,
                    MapValueBytes {
                        bytes: key.clone(),
                        references: Vec::new(),
                    },
                    bytes(index * 3),
                    |candidate| candidate == key,
                )
                .unwrap(),
            );
        }
        assert_eq!(map.len().unwrap(), 130);
        for (index, identity) in identities.into_iter().enumerate() {
            let entry = map.entry(identity).unwrap().unwrap();
            assert_eq!(
                u64::from_le_bytes(entry.value.try_into().unwrap()),
                (index as u64 + 1) * 3
            );
        }
    }

    #[test]
    fn failed_growth_does_not_publish_partial_map_state() {
        let mut map = map(Some(4_000));
        for index in 1..=64u64 {
            let key = index.to_le_bytes().to_vec();
            map.set(
                index as u32,
                MapValueBytes {
                    bytes: key.clone(),
                    references: Vec::new(),
                },
                bytes(index),
                |candidate| candidate == key,
            )
            .unwrap();
        }
        let before = map.snapshot().unwrap();
        let result = map.set(65, bytes(65), bytes(65), |candidate| {
            candidate == 65u64.to_le_bytes()
        });
        assert!(result.is_err());
        assert_eq!(map.snapshot().unwrap(), before);
        assert_eq!(map.len().unwrap(), 64);
    }

    #[test]
    fn entry_reference_lane_keeps_child_alive_through_major_collection() {
        let pool = StorageHeap::new(None, None, 4 << 20).into_worker_pool();
        let child_layout = pool
            .register_layout(StorageLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
            })
            .unwrap();
        let child = allocate(&pool, child_layout, 1, 8).unwrap();
        let mut map = ManagedMap::new(
            pool.clone(),
            MapValueLayout {
                stride: 8,
                references: vec![0],
                dynamics: Vec::new(),
                logical: 8,
            },
            MapValueLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
                logical: 8,
            },
        )
        .unwrap();
        map.set(
            7,
            MapValueBytes {
                bytes: vec![0; 8],
                references: vec![(0, child)],
            },
            bytes(9),
            |_| true,
        )
        .unwrap();
        let entry = map.entry(map.snapshot().unwrap()[0]).unwrap().unwrap();
        assert_eq!(entry.key_references, vec![(0, child)]);
        pool.collect_major([map.handle()]).unwrap();
        assert!(pool.lock().unwrap().get(child).is_ok());
        assert_eq!(map.len().unwrap(), 1);
    }

    #[test]
    fn dynamic_ref_lane_keeps_box_reference_and_rejects_wrong_offsets() {
        let pool = StorageHeap::new(None, None, 4 << 20).into_worker_pool();
        let child_layout = pool
            .register_layout(StorageLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
            })
            .unwrap();
        let child = allocate(&pool, child_layout, 1, 8).unwrap();
        let mut map = ManagedMap::new(
            pool.clone(),
            MapValueLayout {
                stride: 24,
                references: Vec::new(),
                dynamics: vec![0],
                logical: 24,
            },
            MapValueLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
                logical: 8,
            },
        )
        .unwrap();
        let mut key = vec![0; 24];
        key[4..8].copy_from_slice(&4_u32.to_le_bytes());
        let wrong = map.set(
            1,
            MapValueBytes {
                bytes: key.clone(),
                references: vec![(0, child)],
            },
            bytes(1),
            |_| true,
        );
        assert!(wrong.is_err());
        map.set(
            1,
            MapValueBytes {
                bytes: key,
                references: vec![(8, child)],
            },
            bytes(1),
            |_| true,
        )
        .unwrap();
        assert_eq!(
            map.find_lane(1, |candidate| candidate.references == vec![(8, child)])
                .unwrap(),
            map.snapshot().unwrap().first().copied()
        );
        let entry = map.entry(map.snapshot().unwrap()[0]).unwrap().unwrap();
        assert_eq!(entry.key_references, vec![(8, child)]);
        pool.collect_major([map.handle()]).unwrap();
        assert!(pool.lock().unwrap().get(child).is_ok());
    }

    #[test]
    fn map_header_follows_guest_anchor_lifetime() {
        let heap = Heap::new(64, 1 << 20).unwrap();
        heap.set_type_registry(crate::types::TypeRegistry::new([]).unwrap());
        let pool = heap.storage_pool();
        let anchor = Value::int(0);
        let guest = heap
            .allocate(
                anchor.clone(),
                heap.logical_allocation_bytes(&anchor).unwrap(),
            )
            .unwrap();
        heap.flush_worker_lease().unwrap();
        let map = ManagedMap::attach(
            pool.clone(),
            heap.storage_ref(guest).unwrap(),
            MapValueLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
                logical: 8,
            },
            MapValueLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
                logical: 8,
            },
        )
        .unwrap();
        let header = map.handle();
        heap.collect([guest]).unwrap();
        assert!(pool.lock().unwrap().get(header).is_ok());
        heap.collect(std::iter::empty::<crate::heap::Handle>())
            .unwrap();
        assert!(pool.lock().unwrap().get(header).is_err());
    }

    #[test]
    fn semantic_hash_uses_string_content_and_normalizes_float_zero() {
        assert_eq!(
            hash_value(&Value::string(b"same")),
            hash_value(&Value::string(b"same"))
        );
        assert_ne!(
            hash_value(&Value::string(b"same")),
            hash_value(&Value::string(b"other"))
        );
        let positive = Value {
            typ: TypeIdentity::Primitive(wire::PrimitiveFloat64),
            data: Data::Float(0.0),
        };
        let negative = Value {
            typ: TypeIdentity::Primitive(wire::PrimitiveFloat64),
            data: Data::Float(-0.0),
        };
        assert_eq!(hash_value(&positive), hash_value(&negative));
    }

    #[test]
    fn semantic_hash_tracks_aggregate_interface_and_pointer_identity() {
        use crate::value::{Address, StructStorage};
        use std::collections::BTreeMap;

        let array = Value {
            typ: TypeIdentity::Any,
            data: Data::Array(std::sync::Arc::new(vec![Value::int(1), Value::int(2)])),
        };
        let array_copy = array.clone();
        assert_eq!(hash_value(&array), hash_value(&array_copy));

        let mut first_fields = BTreeMap::new();
        first_fields.insert("N".to_owned(), Value::int(7));
        first_fields.insert("_".to_owned(), Value::string(b"ignored"));
        let mut second_fields = BTreeMap::new();
        second_fields.insert("N".to_owned(), Value::int(7));
        second_fields.insert("_".to_owned(), Value::string(b"changed"));
        let first = Value {
            typ: TypeIdentity::Any,
            data: Data::Struct(StructStorage::from(first_fields)),
        };
        let second = Value {
            typ: TypeIdentity::Any,
            data: Data::Struct(StructStorage::from(second_fields)),
        };
        assert_eq!(hash_value(&first), hash_value(&second));

        let direct = Value::int(42);
        let interface = Value {
            typ: TypeIdentity::Any,
            data: Data::Interface(std::sync::Arc::new(direct.clone())),
        };
        assert_eq!(hash_value(&direct), hash_value(&interface));

        let pointer = Value {
            typ: TypeIdentity::Any,
            data: Data::Pointer(std::sync::Arc::new(Address {
                root: StorageRef {
                    slot: 3,
                    generation: 9,
                },
                path: Vec::new(),
                identity: std::sync::Arc::default(),
            })),
        };
        assert_eq!(hash_value(&pointer), hash_value(&pointer.clone()));
    }
}
