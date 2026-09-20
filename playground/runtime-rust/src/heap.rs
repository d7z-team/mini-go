//! Shared generational storage with independent directory, payload and quota ownership.

use crate::error::RuntimeError;
use std::sync::{
    Arc, Mutex, Weak,
    atomic::{AtomicU64, Ordering},
};

static NEXT_HEAP: AtomicU64 = AtomicU64::new(1);

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct Handle {
    owner: u64,
    index: usize,
    generation: u64,
}

/// Reports every strong guest reference held by an object.
pub trait Trace {
    fn trace(&self, visit: &mut dyn FnMut(Handle));
    /// True only when edges change through heap updates or replacements.
    /// Interior-mutable values must retain the default.
    fn stable_edges(&self) -> bool {
        false
    }
}

#[derive(Clone)]
struct Slot<T> {
    generation: u64,
    value: Option<Arc<Entry<T>>>,
    next_free: Option<usize>,
    marked: u64,
}

struct Entry<T> {
    payload: Mutex<EntryPayload<T>>,
}

struct EntryPayload<T> {
    live: bool,
    value: Arc<T>,
    bytes: u64,
    edges: Option<Vec<Handle>>,
}

impl<T> Entry<T> {
    fn new(value: T, bytes: u64) -> Arc<Self> {
        Arc::new(Self {
            payload: Mutex::new(EntryPayload {
                live: true,
                value: Arc::new(value),
                bytes,
                edges: None,
            }),
        })
    }

    fn snapshot(&self) -> Arc<T> {
        self.payload.lock().unwrap().value.clone()
    }
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct HeapStats {
    pub collections: u64,
    pub live_objects: usize,
    pub live_bytes: u64,
    pub total_allocated_bytes: u64,
    pub peak_bytes: u64,
}

#[derive(Default)]
struct Accounting {
    stats: HeapStats,
    reserved_bytes: u64,
    reserved_objects: usize,
    external_bytes: u64,
}

impl Accounting {
    fn snapshot(&self) -> HeapStats {
        HeapStats {
            live_bytes: self.stats.live_bytes - self.reserved_bytes,
            live_objects: self.stats.live_objects - self.reserved_objects,
            ..self.stats
        }
    }

    fn publish_growth(&mut self, bytes: u64) {
        self.stats.total_allocated_bytes = self.stats.total_allocated_bytes.saturating_add(bytes);
        self.stats.peak_bytes = self
            .stats
            .peak_bytes
            .max(self.stats.live_bytes - self.reserved_bytes);
    }
}

/// Private task storage participates in the same quota as arena objects, but
/// owns its value directly and needs no arena lookup to read or update a scalar.
pub(crate) struct OwnedValue<T> {
    value: T,
    bytes: u64,
    max_bytes: u64,
    accounting: Arc<Mutex<Accounting>>,
}

impl<T> OwnedValue<T> {
    pub(crate) fn get(&self) -> &T {
        &self.value
    }

    pub(crate) fn replace(&mut self, value: T, bytes: u64) -> Result<(), (RuntimeError, T)> {
        if bytes != self.bytes {
            let mut accounting = self.accounting.lock().unwrap();
            let Some(live) = (accounting.stats.live_bytes - self.bytes)
                .checked_add(bytes)
                .filter(|live| *live <= self.max_bytes)
            else {
                return Err((
                    RuntimeError::new("allocation_limit", "local", "logical byte limit exceeded"),
                    value,
                ));
            };
            accounting.stats.live_bytes = live;
            accounting.publish_growth(bytes.saturating_sub(self.bytes));
            self.bytes = bytes;
        }
        self.value = value;
        Ok(())
    }
}

impl<T> Drop for OwnedValue<T> {
    fn drop(&mut self) {
        let mut accounting = self.accounting.lock().unwrap();
        accounting.stats.live_bytes -= self.bytes;
        accounting.stats.live_objects -= 1;
    }
}

/// Concurrent object access uses stable entries. Collection requires the VM's
/// safepoint; logical bytes follow the contract independently of Rust sizes.
pub struct Heap<T> {
    id: u64,
    directory: Mutex<Directory<T>>,
    max_objects: usize,
    max_bytes: u64,
    accounting: Arc<Mutex<Accounting>>,
    collection: Mutex<()>,
}

struct Directory<T> {
    slots: Vec<Slot<T>>,
    free: Option<usize>,
    occupied: Vec<usize>,
    pending: usize,
    mark_epoch: u64,
    trace_pending: Vec<Handle>,
}

pub(crate) struct HeapSnapshot<T> {
    id: u64,
    slots: Vec<(u64, Option<Arc<T>>)>,
}

impl<T> HeapSnapshot<T> {
    pub(crate) fn get(&self, handle: Handle) -> Result<&T, RuntimeError> {
        self.slots
            .get(handle.index)
            .filter(|(generation, _)| handle.owner == self.id && *generation == handle.generation)
            .and_then(|(_, value)| value.as_deref())
            .ok_or_else(|| {
                RuntimeError::new("stale_reference", "heap", "object handle is not live")
            })
    }
}

impl<T: Trace> Heap<T> {
    fn entry(&self, handle: Handle) -> Result<Arc<Entry<T>>, RuntimeError> {
        self.directory
            .lock()
            .unwrap()
            .slots
            .get(handle.index)
            .filter(|slot| handle.owner == self.id && slot.generation == handle.generation)
            .and_then(|slot| slot.value.clone())
            .ok_or_else(|| {
                RuntimeError::new("stale_reference", "heap", "object handle is not live")
            })
    }

    pub(crate) fn snapshot(&self) -> HeapSnapshot<T> {
        let entries = self
            .directory
            .lock()
            .unwrap()
            .slots
            .iter()
            .map(|slot| (slot.generation, slot.value.clone()))
            .collect::<Vec<_>>();
        HeapSnapshot {
            id: self.id,
            slots: entries
                .into_iter()
                .map(|(generation, entry)| (generation, entry.map(|entry| entry.snapshot())))
                .collect(),
        }
    }

    pub(crate) fn owner_id(&self) -> u64 {
        self.id
    }

    pub fn new(max_objects: usize, max_bytes: u64) -> Result<Self, RuntimeError> {
        let id = NEXT_HEAP
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |next| {
                next.checked_add(1)
            })
            .map_err(|_| {
                RuntimeError::new("allocation_limit", "heap", "heap identity exhausted")
            })?;
        Ok(Self {
            id,
            directory: Mutex::new(Directory {
                slots: Vec::new(),
                free: None,
                occupied: Vec::new(),
                pending: 0,
                mark_epoch: 0,
                trace_pending: Vec::new(),
            }),
            max_objects,
            max_bytes,
            accounting: Arc::default(),
            collection: Mutex::new(()),
        })
    }

    pub fn stats(&self) -> HeapStats {
        self.accounting.lock().unwrap().snapshot()
    }

    pub(crate) fn own(&self, value: T, bytes: u64) -> Result<OwnedValue<T>, (RuntimeError, T)> {
        let mut accounting = self.accounting.lock().unwrap();
        let live = accounting.stats.live_bytes.checked_add(bytes);
        if accounting.stats.live_objects >= self.max_objects
            || live.is_none_or(|live| live > self.max_bytes)
        {
            return Err((
                RuntimeError::new(
                    "allocation_limit",
                    "local",
                    "object or logical byte limit exceeded",
                ),
                value,
            ));
        }
        accounting.stats.live_bytes = live.unwrap();
        accounting.stats.live_objects += 1;
        accounting.publish_growth(bytes);
        Ok(OwnedValue {
            value,
            bytes,
            max_bytes: self.max_bytes,
            accounting: self.accounting.clone(),
        })
    }

    pub(crate) fn replacements_fit(
        &self,
        replacements: &[(Handle, u64)],
    ) -> Result<bool, RuntimeError> {
        let mut old = 0u64;
        let mut new = 0u64;
        for (handle, bytes) in replacements {
            old = old.saturating_add(self.allocation_bytes(*handle)?);
            let Some(total) = new.checked_add(*bytes) else {
                return Ok(false);
            };
            new = total;
        }
        let live = self.accounting.lock().unwrap().stats.live_bytes;
        Ok(live
            .checked_sub(old)
            .and_then(|bytes| bytes.checked_add(new))
            .is_some_and(|bytes| bytes <= self.max_bytes))
    }

    pub(crate) fn set_external_bytes(&self, bytes: u64) -> Result<(), RuntimeError> {
        let mut accounting = self.accounting.lock().unwrap();
        let live = accounting
            .stats
            .live_bytes
            .checked_sub(accounting.external_bytes)
            .and_then(|live| live.checked_add(bytes))
            .filter(|live| *live <= self.max_bytes)
            .ok_or_else(|| {
                RuntimeError::new("allocation_limit", "heap", "logical byte limit exceeded")
            })?;
        let growth = bytes.saturating_sub(accounting.external_bytes);
        accounting.stats.live_bytes = live;
        accounting.publish_growth(growth);
        accounting.external_bytes = bytes;
        Ok(())
    }

    /// Publishes an object after reserving both directory capacity and quota.
    pub fn allocate(&self, value: T, bytes: u64) -> Result<Handle, (RuntimeError, T)> {
        let mut directory = self.directory.lock().unwrap();
        let mut accounting = self.accounting.lock().unwrap();
        let live = accounting.stats.live_bytes.checked_add(bytes);
        if accounting.stats.live_objects >= self.max_objects
            || live.is_none_or(|live| live > self.max_bytes)
        {
            return Err((
                RuntimeError::new(
                    "allocation_limit",
                    "heap",
                    "object or logical byte limit exceeded",
                ),
                value,
            ));
        }
        let reserved = directory.pending + 1;
        if directory.occupied.try_reserve(reserved).is_err() {
            return Err((
                RuntimeError::new("allocation_limit", "heap", "occupied storage exhausted"),
                value,
            ));
        }
        let index = if let Some(index) = directory.free {
            directory.free = directory.slots[index].next_free.take();
            directory.slots[index].value = Some(Entry::new(value, bytes));
            index
        } else {
            if directory.slots.len() >= self.max_objects || directory.slots.try_reserve(1).is_err()
            {
                return Err((
                    RuntimeError::new("allocation_limit", "heap", "object storage exhausted"),
                    value,
                ));
            }
            let index = directory.slots.len();
            directory.slots.push(Slot {
                generation: 1,
                value: Some(Entry::new(value, bytes)),
                next_free: None,
                marked: 0,
            });
            index
        };
        directory.occupied.push(index);
        accounting.stats.live_objects += 1;
        accounting.stats.live_bytes = live.unwrap();
        accounting.publish_growth(bytes);
        Ok(Handle {
            owner: self.id,
            index,
            generation: directory.slots[index].generation,
        })
    }

    /// The returned payload owns its lifetime, but does not root guest handles.
    pub fn get(&self, handle: Handle) -> Result<Arc<T>, RuntimeError> {
        Ok(self.entry(handle)?.snapshot())
    }

    pub(crate) fn read_mutation_base(&self, handle: Handle) -> Result<(Arc<T>, u64), RuntimeError> {
        let entry = self.entry(handle)?;
        let payload = entry.payload.lock().unwrap();
        Ok((payload.value.clone(), payload.bytes))
    }

    pub fn replace(&self, handle: Handle, value: T, bytes: u64) -> Result<(), (RuntimeError, T)> {
        let entry = match self.entry(handle) {
            Ok(entry) => entry,
            Err(error) => return Err((error, value)),
        };
        let mut payload = entry.payload.lock().unwrap();
        if !payload.live {
            return Err((
                RuntimeError::new("stale_reference", "heap", "object handle is not live"),
                value,
            ));
        }
        let mut accounting = self.accounting.lock().unwrap();
        let Some(live) = (accounting.stats.live_bytes - payload.bytes)
            .checked_add(bytes)
            .filter(|live| *live <= self.max_bytes)
        else {
            return Err((
                RuntimeError::new("allocation_limit", "heap", "logical byte limit exceeded"),
                value,
            ));
        };
        accounting.stats.live_bytes = live;
        accounting.publish_growth(bytes.saturating_sub(payload.bytes));
        payload.bytes = bytes;
        payload.edges = None;
        let previous = std::mem::replace(&mut payload.value, Arc::new(value));
        drop(accounting);
        drop(payload);
        drop(previous);
        Ok(())
    }

    pub(crate) fn allocation_bytes(&self, handle: Handle) -> Result<u64, RuntimeError> {
        Ok(self.entry(handle)?.payload.lock().unwrap().bytes)
    }

    /// Reserve growth against the exact snapshot used to prepare a replacement.
    /// None means preparation raced with another writer; no value was published.
    pub(crate) fn prepare_mutation(
        &self,
        handle: Handle,
        expected: &Arc<T>,
        value: T,
        bytes: u64,
    ) -> Result<Option<PreparedMutation<T>>, RuntimeError> {
        let entry = self.entry(handle)?;
        let payload = entry.payload.lock().unwrap();
        if !payload.live || !Arc::ptr_eq(expected, &payload.value) {
            return Ok(None);
        }
        let growth = bytes.saturating_sub(payload.bytes);
        let mut accounting = self.accounting.lock().unwrap();
        let live = accounting
            .stats
            .live_bytes
            .checked_add(growth)
            .filter(|live| *live <= self.max_bytes)
            .ok_or_else(|| {
                RuntimeError::new("allocation_limit", "heap", "logical byte limit exceeded")
            })?;
        accounting.stats.live_bytes = live;
        accounting.reserved_bytes += growth;
        drop(accounting);
        drop(payload);
        Ok(Some(PreparedMutation {
            handle,
            entry,
            expected: Arc::downgrade(expected),
            value: Some(value),
            bytes,
            reserved: growth,
            accounting: self.accounting.clone(),
        }))
    }

    pub(crate) fn update(
        &self,
        handle: Handle,
        expected: &Weak<T>,
        bytes: u64,
        edges_unchanged: bool,
        update: impl FnOnce(&mut T),
    ) -> Result<bool, RuntimeError>
    where
        T: Clone,
    {
        let entry = self.entry(handle)?;
        let mut payload = entry.payload.lock().unwrap();
        if !payload.live || !Weak::ptr_eq(expected, &Arc::downgrade(&payload.value)) {
            return Ok(false);
        }
        let mut accounting = self.accounting.lock().unwrap();
        let live = (accounting.stats.live_bytes - payload.bytes)
            .checked_add(bytes)
            .filter(|bytes| *bytes <= self.max_bytes)
            .ok_or_else(|| {
                RuntimeError::new("allocation_limit", "heap", "logical byte limit exceeded")
            })?;
        accounting.stats.live_bytes = live;
        accounting.publish_growth(bytes.saturating_sub(payload.bytes));
        payload.bytes = bytes;
        if !edges_unchanged {
            payload.edges = None;
        }
        drop(accounting);
        update(Arc::make_mut(&mut payload.value));
        Ok(true)
    }

    /// Collection runs after the caller has stopped mutations and registered
    /// every task's roots. Payload tracing never holds the directory lock.
    pub fn collect(&self, roots: impl IntoIterator<Item = Handle>) -> Result<usize, RuntimeError> {
        self.collect_with_persistent_roots(&[], roots)
    }

    pub(crate) fn collect_with_persistent_roots(
        &self,
        persistent: &[Handle],
        roots: impl IntoIterator<Item = Handle>,
    ) -> Result<usize, RuntimeError> {
        let _collection = self.collection.lock().unwrap();
        let (epoch, mut pending) = {
            let mut directory = self.directory.lock().unwrap();
            directory.mark_epoch = match directory.mark_epoch.checked_add(1) {
                Some(epoch) => epoch,
                None => {
                    for slot in &mut directory.slots {
                        slot.marked = 0;
                    }
                    1
                }
            };
            (
                directory.mark_epoch,
                std::mem::take(&mut directory.trace_pending),
            )
        };
        let marked = (|| {
            for root in persistent.iter().copied().chain(roots) {
                pending.try_reserve(1).map_err(|_| {
                    RuntimeError::new("allocation_limit", "heap", "trace storage exhausted")
                })?;
                pending.push(root);
            }
            while let Some(handle) = pending.pop() {
                let entry = {
                    let mut directory = self.directory.lock().unwrap();
                    let slot = directory
                        .slots
                        .get_mut(handle.index)
                        .filter(|slot| {
                            handle.owner == self.id
                                && slot.generation == handle.generation
                                && slot.value.is_some()
                        })
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "stale_reference",
                                "heap",
                                "object handle is not live",
                            )
                        })?;
                    if slot.marked == epoch {
                        continue;
                    }
                    slot.marked = epoch;
                    slot.value.as_ref().unwrap().clone()
                };
                let (value, mut edges) = {
                    let mut payload = entry.payload.lock().unwrap();
                    (
                        payload.value.clone(),
                        payload.edges.take().unwrap_or_default(),
                    )
                };
                let cacheable = value.stable_edges();
                if edges.is_empty() || !cacheable {
                    edges.clear();
                    value.trace(&mut |child| edges.push(child));
                }
                pending.try_reserve(edges.len()).map_err(|_| {
                    RuntimeError::new("allocation_limit", "heap", "trace storage exhausted")
                })?;
                pending.extend(edges.iter().copied());
                if cacheable {
                    entry.payload.lock().unwrap().edges = Some(edges);
                }
            }
            Ok::<_, RuntimeError>(())
        })();
        pending.clear();
        self.directory.lock().unwrap().trace_pending = pending;
        marked?;
        let mut retired = Vec::new();
        {
            let mut directory = self.directory.lock().unwrap();
            retired.try_reserve(directory.occupied.len()).map_err(|_| {
                RuntimeError::new("allocation_limit", "heap", "sweep storage exhausted")
            })?;
            let mut position = 0;
            while position < directory.occupied.len() {
                let index = directory.occupied[position];
                if directory.slots[index].marked == epoch {
                    position += 1;
                    continue;
                }
                retired.push(directory.slots[index].value.take().unwrap());
                if let Some(generation) = directory.slots[index].generation.checked_add(1) {
                    directory.slots[index].generation = generation;
                    directory.slots[index].next_free = directory.free;
                    directory.free = Some(index);
                }
                directory.occupied.swap_remove(position);
            }
        }
        let released = retired.len();
        let bytes: u64 = retired
            .iter()
            .map(|entry| {
                let mut payload = entry.payload.lock().unwrap();
                payload.live = false;
                payload.bytes
            })
            .sum();
        {
            let mut accounting = self.accounting.lock().unwrap();
            accounting.stats.live_bytes -= bytes;
            accounting.stats.live_objects -= released;
            accounting.stats.collections = accounting.stats.collections.saturating_add(1);
        }
        drop(retired);
        Ok(released)
    }

    pub(crate) fn prepare_allocations(
        &self,
        values: Vec<(T, u64)>,
    ) -> Result<AllocationBatch<'_, T>, RuntimeError> {
        let failure = || {
            RuntimeError::new(
                "allocation_limit",
                "heap",
                "allocation batch exceeds storage limits",
            )
        };
        let count = values.len();
        let bytes = values
            .iter()
            .try_fold(0u64, |total, (_, bytes)| total.checked_add(*bytes))
            .ok_or_else(failure)?;
        let mut directory = self.directory.lock().unwrap();
        let mut accounting = self.accounting.lock().unwrap();
        if count
            > self
                .max_objects
                .saturating_sub(accounting.stats.live_objects)
        {
            return Err(failure());
        }
        let live = accounting
            .stats
            .live_bytes
            .checked_add(bytes)
            .filter(|live| *live <= self.max_bytes)
            .ok_or_else(failure)?;
        let mut handles = Vec::new();
        handles.try_reserve_exact(count).map_err(|_| failure())?;
        let mut free = directory.free;
        let mut additional = 0;
        for _ in 0..count {
            let (index, generation) = if let Some(index) = free {
                let slot = &directory.slots[index];
                free = slot.next_free;
                (index, slot.generation)
            } else {
                let index = directory
                    .slots
                    .len()
                    .checked_add(additional)
                    .filter(|index| *index < self.max_objects)
                    .ok_or_else(failure)?;
                additional += 1;
                (index, 1)
            };
            handles.push(Handle {
                owner: self.id,
                index,
                generation,
            });
        }
        directory
            .slots
            .try_reserve_exact(additional)
            .map_err(|_| failure())?;
        let reserved = directory.pending + count;
        directory
            .occupied
            .try_reserve(reserved)
            .map_err(|_| failure())?;
        for handle in &handles {
            if handle.index == directory.slots.len() {
                directory.slots.push(Slot {
                    generation: handle.generation,
                    value: None,
                    next_free: None,
                    marked: 0,
                });
            } else {
                directory.slots[handle.index].next_free = None;
            }
        }
        directory.free = free;
        directory.pending += count;
        accounting.stats.live_bytes = live;
        accounting.stats.live_objects += count;
        accounting.reserved_bytes += bytes;
        accounting.reserved_objects += count;
        Ok(AllocationBatch {
            heap: self,
            values,
            handles,
            bytes,
            committed: false,
        })
    }
}

/// Owned preparation can be rooted by a parked task without retaining a lock.
pub(crate) struct PreparedMutation<T> {
    handle: Handle,
    entry: Arc<Entry<T>>,
    expected: Weak<T>,
    value: Option<T>,
    bytes: u64,
    reserved: u64,
    accounting: Arc<Mutex<Accounting>>,
}

impl<T: Trace> Trace for PreparedMutation<T> {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        visit(self.handle);
        if let Some(value) = &self.value {
            value.trace(visit);
        }
    }
}

impl<T> PreparedMutation<T> {
    /// A conflict releases the reservation. The caller may recompute the pure
    /// preparation, but must not re-evaluate guest expressions or host calls.
    pub(crate) fn commit(mut self) -> bool {
        let mut payload = self.entry.payload.lock().unwrap();
        if !payload.live || !Weak::ptr_eq(&self.expected, &Arc::downgrade(&payload.value)) {
            return false;
        }
        let mut accounting = self.accounting.lock().unwrap();
        accounting.stats.live_bytes =
            accounting.stats.live_bytes - self.reserved - payload.bytes + self.bytes;
        accounting.reserved_bytes -= self.reserved;
        accounting.publish_growth(self.bytes.saturating_sub(payload.bytes));
        self.reserved = 0;
        payload.bytes = self.bytes;
        payload.edges = None;
        let previous = std::mem::replace(&mut payload.value, Arc::new(self.value.take().unwrap()));
        drop(accounting);
        drop(payload);
        drop(previous);
        true
    }
}

impl<T> Drop for PreparedMutation<T> {
    fn drop(&mut self) {
        if self.reserved != 0 {
            let mut accounting = self.accounting.lock().unwrap();
            accounting.stats.live_bytes -= self.reserved;
            accounting.reserved_bytes -= self.reserved;
        }
    }
}

pub(crate) struct AllocationBatch<'a, T: Trace> {
    heap: &'a Heap<T>,
    values: Vec<(T, u64)>,
    handles: Vec<Handle>,
    bytes: u64,
    committed: bool,
}

impl<T: Trace> AllocationBatch<'_, T> {
    pub(crate) fn handles(&self) -> &[Handle] {
        &self.handles
    }

    pub(crate) fn commit(mut self) {
        let count = self.values.len();
        {
            let mut directory = self.heap.directory.lock().unwrap();
            for ((value, bytes), handle) in std::mem::take(&mut self.values)
                .into_iter()
                .zip(&self.handles)
            {
                directory.slots[handle.index].value = Some(Entry::new(value, bytes));
                directory.occupied.push(handle.index);
            }
            directory.pending -= count;
        }
        let mut accounting = self.heap.accounting.lock().unwrap();
        accounting.reserved_bytes -= self.bytes;
        accounting.reserved_objects -= count;
        accounting.publish_growth(self.bytes);
        self.committed = true;
    }
}

impl<T: Trace> Drop for AllocationBatch<'_, T> {
    fn drop(&mut self) {
        if !self.committed {
            {
                let mut directory = self.heap.directory.lock().unwrap();
                for handle in &self.handles {
                    if let Some(generation) =
                        directory.slots[handle.index].generation.checked_add(1)
                    {
                        directory.slots[handle.index].generation = generation;
                        directory.slots[handle.index].next_free = directory.free;
                        directory.free = Some(handle.index);
                    }
                }
                directory.pending -= self.handles.len();
            }
            let mut accounting = self.heap.accounting.lock().unwrap();
            accounting.stats.live_bytes -= self.bytes;
            accounting.stats.live_objects -= self.values.len();
            accounting.reserved_bytes -= self.bytes;
            accounting.reserved_objects -= self.values.len();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        types::TypeIdentity,
        value::{Address, Data, Value},
    };

    #[test]
    fn prepared_mutations_reserve_growth_and_release_conflicts() {
        let heap = Heap::new(4, 128).unwrap();
        let handle = heap.allocate(Value::int(1), 32).unwrap();
        let snapshot = heap.get(handle).unwrap();
        let first = heap
            .prepare_mutation(handle, &snapshot, Value::int(2), 96)
            .unwrap()
            .unwrap();
        assert_eq!(heap.stats().live_bytes, 32);
        assert!(heap.allocate(Value::int(3), 64).is_err());
        let second = heap
            .prepare_mutation(handle, &snapshot, Value::int(4), 48)
            .unwrap()
            .unwrap();
        assert!(second.commit());
        assert!(!first.commit());
        assert_eq!(heap.get(handle).unwrap().integer().unwrap(), 4);
        assert_eq!(heap.stats().live_bytes, 48);
        assert_eq!(heap.stats().total_allocated_bytes, 48);
        assert_eq!(heap.stats().peak_bytes, 48);
        assert!(
            heap.prepare_mutation(handle, &snapshot, Value::int(5), 48)
                .unwrap()
                .is_none()
        );
        let snapshot = heap.get(handle).unwrap();
        let abandoned = heap
            .prepare_mutation(handle, &snapshot, Value::int(6), 128)
            .unwrap()
            .unwrap();
        drop(abandoned);
        assert!(heap.allocate(Value::int(7), 80).is_ok());
        assert_eq!(heap.stats().live_bytes, 128);
    }

    #[test]
    fn prepared_mutation_version_survives_released_read_snapshots() {
        let heap = Heap::new(2, 128).unwrap();
        let handle = heap.allocate(Value::int(1), 32).unwrap();
        let snapshot = heap.get(handle).unwrap();
        let pending = heap
            .prepare_mutation(handle, &snapshot, Value::int(2), 32)
            .unwrap()
            .unwrap();
        drop(snapshot);
        heap.update(
            handle,
            &Arc::downgrade(&heap.get(handle).unwrap()),
            32,
            true,
            |value| *value = Value::int(3),
        )
        .unwrap();
        assert!(!pending.commit());
        assert_eq!(heap.get(handle).unwrap().integer().unwrap(), 3);
        assert_eq!(heap.stats().total_allocated_bytes, 32);
    }

    #[test]
    fn concurrent_prepared_mutations_preserve_each_committed_increment() {
        let heap = Heap::new(1, 32).unwrap();
        let handle = heap.allocate(Value::int(0), 32).unwrap();
        std::thread::scope(|scope| {
            for _ in 0..4 {
                let heap = &heap;
                scope.spawn(move || {
                    for _ in 0..1000 {
                        loop {
                            let (snapshot, bytes) = heap.read_mutation_base(handle).unwrap();
                            let value = Value::int(snapshot.integer().unwrap() + 1);
                            if let Some(mutation) = heap
                                .prepare_mutation(handle, &snapshot, value, bytes)
                                .unwrap()
                            {
                                drop(snapshot);
                                if mutation.commit() {
                                    break;
                                }
                            }
                        }
                    }
                });
            }
        });
        assert_eq!(heap.get(handle).unwrap().integer().unwrap(), 4000);
        assert_eq!(heap.stats().live_bytes, 32);
        assert_eq!(heap.stats().total_allocated_bytes, 32);
    }

    #[test]
    fn conditional_updates_reject_stale_inputs_before_quota_and_mutation() {
        let heap = Heap::new(1, 32).unwrap();
        let handle = heap.allocate(Value::int(0), 32).unwrap();
        let expected = Arc::downgrade(&heap.get(handle).unwrap());
        heap.replace(handle, Value::int(1), 32).unwrap();
        assert!(
            !heap
                .update(handle, &expected, 128, false, |_| {
                    panic!("a stale preparation must not mutate the object");
                })
                .unwrap()
        );
        std::thread::scope(|scope| {
            for _ in 0..4 {
                let heap = &heap;
                scope.spawn(move || {
                    for _ in 0..1000 {
                        loop {
                            let (snapshot, bytes) = heap.read_mutation_base(handle).unwrap();
                            let expected = Arc::downgrade(&snapshot);
                            let next = snapshot.integer().unwrap() + 1;
                            drop(snapshot);
                            if heap
                                .update(handle, &expected, bytes, true, |value| {
                                    *value = Value::int(next);
                                })
                                .unwrap()
                            {
                                break;
                            }
                        }
                    }
                });
            }
        });
        assert_eq!(heap.get(handle).unwrap().integer().unwrap(), 4001);
        assert_eq!(heap.stats().live_bytes, 32);
        assert_eq!(heap.stats().total_allocated_bytes, 32);
    }

    #[test]
    fn prepared_mutation_roots_preserve_target_and_unpublished_references() {
        let heap = Heap::new(4, 256).unwrap();
        let target = heap.allocate(Value::int(0), 32).unwrap();
        let child = heap.allocate(Value::int(42), 32).unwrap();
        let replacement = Value {
            typ: TypeIdentity::Any,
            data: Data::Pointer(Address {
                identity: Arc::default(),
                root: child,
                path: Vec::new(),
            }),
        };
        let snapshot = heap.get(target).unwrap();
        let mutation = heap
            .prepare_mutation(target, &snapshot, replacement, 64)
            .unwrap()
            .unwrap();
        let mut roots = Vec::new();
        mutation.trace(&mut |root| roots.push(root));
        assert_eq!(heap.collect(roots).unwrap(), 0);
        assert_eq!(heap.stats().live_bytes, 64);
        drop(mutation);
        assert_eq!(heap.collect([target]).unwrap(), 1);
        assert!(heap.get(child).is_err());
        assert_eq!(heap.stats().live_bytes, 32);
    }

    #[test]
    fn collected_target_rejects_late_prepared_commit_and_releases_reservation() {
        let heap = Heap::new(1, 128).unwrap();
        let target = heap.allocate(Value::int(1), 32).unwrap();
        let snapshot = heap.get(target).unwrap();
        let mutation = heap
            .prepare_mutation(target, &snapshot, Value::int(2), 128)
            .unwrap()
            .unwrap();
        assert_eq!(heap.collect([]).unwrap(), 1);
        assert!(!mutation.commit());
        assert_eq!(heap.stats().live_objects, 0);
        assert_eq!(heap.stats().live_bytes, 0);
        let next = heap.allocate(Value::int(3), 128).unwrap();
        assert_eq!(heap.get(next).unwrap().integer().unwrap(), 3);
        assert!(heap.get(target).is_err());
    }

    #[test]
    fn concurrent_directory_allocations_and_replacements_share_one_quota() {
        let heap = Heap::new(8, 256).unwrap();
        let handles = Mutex::new(Vec::new());
        std::thread::scope(|scope| {
            for worker in 0..4 {
                let heap = &heap;
                let handles = &handles;
                scope.spawn(move || {
                    for index in 0..8 {
                        match heap.allocate(Value::int(worker * 8 + index), 32) {
                            Ok(handle) => handles.lock().unwrap().push(handle),
                            Err((error, _)) => assert_eq!(error.code, "allocation_limit"),
                        }
                    }
                });
            }
        });
        let handles = handles.into_inner().unwrap();
        assert_eq!(handles.len(), 8);
        assert_eq!(heap.stats().live_bytes, 256);
        assert_eq!(heap.collect(handles.iter().copied()).unwrap(), 0);
        for handle in &handles {
            heap.replace(*handle, Value::int(1), 16).unwrap();
        }
        let successes = AtomicU64::new(0);
        std::thread::scope(|scope| {
            for handle in &handles {
                let heap = &heap;
                let successes = &successes;
                scope.spawn(move || match heap.replace(*handle, Value::int(2), 80) {
                    Ok(()) => {
                        successes.fetch_add(1, Ordering::Relaxed);
                    }
                    Err((error, _)) => assert_eq!(error.code, "allocation_limit"),
                });
            }
        });
        assert_eq!(successes.load(Ordering::Relaxed), 2);
        assert_eq!(heap.stats().live_bytes, 256);
        assert_eq!(
            handles
                .iter()
                .filter(|handle| heap.get(**handle).unwrap().integer().unwrap() == 2)
                .count(),
            2
        );
        assert_eq!(heap.collect([]).unwrap(), 8);
        assert_eq!(heap.stats().live_bytes, 0);
    }

    #[test]
    fn pending_batches_reserve_capacity_and_revoke_abandoned_handles() {
        let heap = Heap::new(4, 128).unwrap();
        let batch = heap
            .prepare_allocations(vec![(Value::int(1), 32), (Value::int(2), 32)])
            .unwrap();
        let abandoned = batch.handles()[0];
        let first = heap.allocate(Value::int(3), 32).unwrap();
        let second = heap.allocate(Value::int(4), 32).unwrap();
        assert_eq!(
            heap.allocate(Value::int(5), 32).unwrap_err().0.code,
            "allocation_limit"
        );
        assert_eq!(heap.stats().live_bytes, 64);
        drop(batch);
        let replacement = heap
            .prepare_allocations(vec![(Value::int(6), 32), (Value::int(7), 32)])
            .unwrap();
        let mut roots = replacement.handles().to_vec();
        replacement.commit();
        assert_eq!(heap.get(abandoned).unwrap_err().code, "stale_reference");
        roots.extend([first, second]);
        assert_eq!(heap.collect(roots).unwrap(), 0);
        assert_eq!(heap.stats().live_bytes, 128);
        assert_eq!(heap.collect([]).unwrap(), 4);
    }

    #[test]
    fn private_values_share_quota_with_arena_and_pending_publication() {
        let heap = Heap::new(3, 256).unwrap();
        let mut private = heap.own(Value::int(1), 64).unwrap();
        let root = heap.allocate(Value::int(2), 64).unwrap();
        let before = heap.stats();
        let candidate = heap
            .prepare_allocations(vec![(Value::int(3), 128)])
            .unwrap();
        let error = private.replace(Value::int(4), 65).unwrap_err().0;
        assert_eq!(error.code, "allocation_limit");
        assert_eq!(private.get().integer().unwrap(), 1);
        drop(candidate);
        assert_eq!(heap.stats(), before);
        private.replace(Value::int(4), 128).unwrap();
        assert_eq!(heap.stats().live_bytes, 192);
        let candidate = heap.prepare_allocations(vec![(Value::int(5), 64)]).unwrap();
        let published = candidate.handles()[0];
        drop(private);
        candidate.commit();
        assert_eq!(heap.stats().live_bytes, 128);
        assert_eq!(heap.stats().live_objects, 2);
        assert_eq!(heap.get(published).unwrap().integer().unwrap(), 5);
        heap.collect([root]).unwrap();
        assert_eq!(heap.stats().live_bytes, 64);
        assert_eq!(heap.stats().live_objects, 1);
    }

    #[test]
    fn concurrent_private_growth_cannot_exceed_shared_capacity() {
        let heap = Heap::new(4, 256).unwrap();
        let values = (0..4)
            .map(|i| heap.own(Value::int(i), 32).unwrap())
            .collect::<Vec<_>>();
        let start = std::sync::Barrier::new(4);
        let finished = std::sync::Barrier::new(4);
        let successes = std::sync::atomic::AtomicUsize::new(0);
        std::thread::scope(|scope| {
            for mut value in values {
                let (start, finished, successes) = (&start, &finished, &successes);
                scope.spawn(move || {
                    start.wait();
                    if value.replace(Value::int(42), 96).is_ok() {
                        successes.fetch_add(1, Ordering::Relaxed);
                    }
                    finished.wait();
                });
            }
        });
        assert_eq!(successes.load(Ordering::Relaxed), 2);
        assert_eq!(heap.stats().live_bytes, 0);
        assert_eq!(heap.stats().live_objects, 0);
        assert_eq!(heap.stats().peak_bytes, 256);
    }

    #[test]
    fn allocation_batch_preserves_payload_and_publishes_only_on_commit() {
        #[derive(Debug)]
        struct Bytes(Vec<u8>);
        impl Trace for Bytes {
            fn trace(&self, _: &mut dyn FnMut(Handle)) {}
        }
        let heap = Heap::new(4, 1024).unwrap();
        let root = heap.allocate(Bytes(vec![42; 256]), 256).unwrap();
        let payload = heap.get(root).unwrap().0.as_ptr();
        let before = heap.stats();
        assert!(
            heap.prepare_allocations(vec![(Bytes(vec![1]), 1024)])
                .is_err()
        );
        assert_eq!(heap.stats(), before);
        let candidate = heap
            .prepare_allocations(vec![(Bytes(vec![2]), 64)])
            .unwrap();
        let unpublished = candidate.handles()[0];
        drop(candidate);
        assert!(heap.get(unpublished).is_err());
        assert_eq!(heap.stats(), before);
        let candidate = heap
            .prepare_allocations(vec![(Bytes(vec![3]), 64)])
            .unwrap();
        let published = candidate.handles()[0];
        candidate.commit();
        assert_eq!(heap.get(published).unwrap().0, [3]);
        assert_eq!(heap.get(root).unwrap().0.as_ptr(), payload);
        assert_eq!(heap.stats().live_bytes, 320);
        heap.collect([root]).unwrap();
        let candidate = heap
            .prepare_allocations(vec![(Bytes(vec![4]), 64)])
            .unwrap();
        let reused = candidate.handles()[0];
        candidate.commit();
        assert!(heap.get(published).is_err());
        assert_eq!(heap.get(reused).unwrap().0, [4]);
    }

    #[test]
    fn failed_mark_keeps_objects_and_next_epoch_can_recover() {
        #[derive(Clone, Debug)]
        struct Node(Option<Handle>);
        impl Trace for Node {
            fn trace(&self, visit: &mut dyn FnMut(Handle)) {
                if let Some(child) = self.0 {
                    visit(child);
                }
            }
        }
        let heap = Heap::new(4, 128).unwrap();
        let stale = heap.allocate(Node(None), 16).unwrap();
        heap.collect([]).unwrap();
        let root = heap.allocate(Node(Some(stale)), 16).unwrap();
        let other = heap.allocate(Node(None), 16).unwrap();
        let before = heap.stats();
        assert_eq!(heap.collect([root]).unwrap_err().code, "stale_reference");
        assert_eq!(heap.stats(), before);
        assert!(heap.get(other).is_ok());
        heap.update(
            root,
            &Arc::downgrade(&heap.get(root).unwrap()),
            16,
            false,
            |node| node.0 = None,
        )
        .unwrap();
        heap.directory.lock().unwrap().mark_epoch = u64::MAX;
        heap.collect([root]).unwrap();
        assert!(heap.get(other).is_err());
        assert!(heap.get(root).is_ok());
    }

    #[test]
    fn external_metadata_reservation_is_atomic_and_survives_arena_collection() {
        let heap = Heap::new(4, 256).unwrap();
        let root = heap.allocate(Value::int(42), 128).unwrap();
        heap.set_external_bytes(128).unwrap();
        let before = heap.stats();
        assert_eq!(
            heap.set_external_bytes(129).unwrap_err().code,
            "allocation_limit"
        );
        assert_eq!(heap.stats(), before);
        assert_eq!(heap.get(root).unwrap().integer().unwrap(), 42);
        heap.collect([]).unwrap();
        assert_eq!(heap.stats().live_objects, 0);
        assert_eq!(heap.stats().live_bytes, 128);
        heap.set_external_bytes(0).unwrap();
        assert_eq!(heap.stats().live_bytes, 0);
        assert_eq!(heap.stats().total_allocated_bytes, 256);
    }

    #[test]
    fn reachable_graph_tracks_mutation_cycles_and_quota_failure() {
        let heap = Heap::new(8, 512).unwrap();
        let first = heap.allocate(Value::int(1), 32).unwrap();
        let second = heap.allocate(Value::int(2), 32).unwrap();
        let pointer = |root| Value {
            typ: TypeIdentity::Any,
            data: Data::Pointer(Address {
                identity: std::sync::Arc::default(),
                root,
                path: vec![],
            }),
        };
        let root = heap.allocate(pointer(first), 32).unwrap();
        heap.collect_with_persistent_roots(&[root], [second])
            .unwrap();
        let before = heap.stats();
        assert_eq!(
            heap.update(
                root,
                &Arc::downgrade(&heap.get(root).unwrap()),
                1024,
                false,
                |_| panic!("quota must precede mutation")
            )
            .unwrap_err()
            .code,
            "allocation_limit"
        );
        assert_eq!(heap.stats(), before);
        heap.collect_with_persistent_roots(&[root], [second])
            .unwrap();
        assert!(heap.get(first).is_ok());
        let bytes = heap.allocation_bytes(root).unwrap();
        heap.update(
            root,
            &Arc::downgrade(&heap.get(root).unwrap()),
            bytes,
            false,
            |value| *value = pointer(second),
        )
        .unwrap();
        heap.collect_with_persistent_roots(&[root], []).unwrap();
        assert!(heap.get(first).is_err());
        assert!(heap.get(second).is_ok());
        let third = heap.allocate(pointer(root), 32).unwrap();
        heap.update(
            root,
            &Arc::downgrade(&heap.get(root).unwrap()),
            32,
            false,
            |value| *value = pointer(third),
        )
        .unwrap();
        heap.collect_with_persistent_roots(&[root], []).unwrap();
        assert!(heap.get(second).is_err());
        assert!(heap.get(third).is_ok());
        heap.collect_with_persistent_roots(&[], []).unwrap();
        assert_eq!(heap.stats().live_objects, 0);
        assert_eq!(heap.stats().live_bytes, 0);
    }

    #[test]
    fn persistent_roots_with_interior_mutability_are_retraced() {
        #[derive(Clone, Debug)]
        struct Node(bool, std::cell::RefCell<Vec<Handle>>);
        impl Trace for Node {
            fn stable_edges(&self) -> bool {
                self.0
            }
            fn trace(&self, visit: &mut dyn FnMut(Handle)) {
                for handle in self.1.borrow().iter() {
                    visit(*handle);
                }
            }
        }
        let heap = Heap::new(4, 128).unwrap();
        let root = heap.allocate(Node(true, Default::default()), 16).unwrap();
        let child = heap.allocate(Node(true, Default::default()), 16).unwrap();
        heap.collect_with_persistent_roots(&[root], [child])
            .unwrap();
        let bytes = heap.allocation_bytes(root).unwrap();
        heap.update(
            root,
            &Arc::downgrade(&heap.get(root).unwrap()),
            bytes,
            false,
            |node| node.0 = false,
        )
        .unwrap();
        heap.get(root).unwrap().1.borrow_mut().push(child);
        heap.collect_with_persistent_roots(&[root], []).unwrap();
        assert!(heap.get(child).is_ok());
        heap.get(root).unwrap().1.borrow_mut().clear();
        heap.collect_with_persistent_roots(&[root], []).unwrap();
        assert!(heap.get(child).is_err());
    }

    #[test]
    fn shared_entry_readers_keep_coherent_payload_snapshots() {
        #[derive(Debug)]
        struct Pair(u64, u64);
        impl Trace for Pair {
            fn trace(&self, _: &mut dyn FnMut(Handle)) {}
        }
        let heap = Heap::new(2, 128).unwrap();
        let handle = heap.allocate(Pair(0, 0), 64).unwrap();
        let entry = heap.entry(handle).unwrap();
        let initial = heap.get(handle).unwrap();
        let start = std::sync::Barrier::new(5);
        std::thread::scope(|scope| {
            for _ in 0..4 {
                let entry = entry.clone();
                let start = &start;
                scope.spawn(move || {
                    start.wait();
                    for _ in 0..1000 {
                        let value = entry.snapshot();
                        assert_eq!(value.0, value.1);
                    }
                });
            }
            start.wait();
            for value in 1..=1000 {
                heap.replace(handle, Pair(value, value), 64).unwrap();
            }
        });
        assert!(Arc::ptr_eq(&entry, &heap.entry(handle).unwrap()));
        assert_eq!((initial.0, initial.1), (0, 0));
        heap.collect([]).unwrap();
        let replacement = heap.allocate(Pair(7, 7), 64).unwrap();
        assert_eq!(heap.get(handle).unwrap_err().code, "stale_reference");
        assert_eq!(heap.get(replacement).unwrap().0, 7);
        assert_eq!(entry.snapshot().0, 1000);
    }
}
