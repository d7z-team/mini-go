//! Numeric typed page storage for guest payloads and collection.
//!
//! This module owns guest bytes and slot identity.  It deliberately does not
//! store `Value`, `Arc<Value>` or host objects in a payload page.  The API is
//! synchronous at a safepoint; a reservation borrow prevents collection until
//! initialization has either been committed or aborted.

use crate::error::RuntimeError;
use crate::types::TypeIdentity;
use std::cmp::{max, min};
use std::collections::HashSet;
use std::ops::{Deref, DerefMut};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock, RwLockWriteGuard};
use std::time::Instant;

const PAGE_BYTES: u32 = 64 * 1024;
pub(crate) const TLAB_MAX_OBJECT_BYTES: u32 = PAGE_BYTES / 4;
const CARD_BYTES: u32 = 256;
const MAX_BYTES: u64 = i64::MAX as u64;
const MAX_COLLECTOR_WORKERS: usize = 4;
const FREE: u8 = 0;
const RESERVED: u8 = 1;
const PUBLISHED: u8 = 2;
const YOUNG: u8 = 1;
const SPARE: u8 = 2;
const OLD: u8 = 3;
const LARGE: u8 = 4;
pub(crate) const VALUE_REF: u32 = 4;
pub(crate) const VALUE_BOX: u32 = 5;
/// Dynamic lanes use an explicit representation code.  Scalar bits are kept
/// inline in the 16-byte payload area; they never share the reference/box
/// ticket encoding or rely on a value-kind magic number.
pub(crate) const VALUE_BITS: u32 = 1;
pub(crate) const VALUE_TYPED_NIL: u32 = 2;
/// A VALUE_BOX descriptor with this flag stores an untyped aggregate backing.
/// It is decoded as `Any` plus its concrete aggregate data so slice/index
/// consumers retain the backing representation. Ordinary interface boxes use
/// zero and preserve the interface wrapper.
pub(crate) const VALUE_BOX_BACKING: u32 = 1;
pub(crate) const VALUE_REF_MAP: u32 = 1;
pub(crate) const VALUE_REF_RESOURCE: u32 = 2;
pub(crate) const VALUE_REF_OPAQUE: u32 = 3;
pub(crate) const VALUE_REF_BYTES: u32 = 4;

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash)]
pub struct StorageRef {
    pub slot: u32,
    pub generation: u32,
}

impl StorageRef {
    pub const NULL: Self = Self {
        slot: 0,
        generation: 0,
    };

    pub(crate) fn valid(self) -> bool {
        self.slot != 0 && self.generation != 0
    }

    pub(crate) fn identity_for_payload(self) -> u64 {
        u64::from(self.slot) | (u64::from(self.generation) << 32)
    }

    pub(crate) fn from_payload_identity(identity: u64) -> Self {
        Self {
            slot: identity as u32,
            generation: (identity >> 32) as u32,
        }
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct StorageLayout {
    pub stride: u32,
    pub references: Vec<u32>,
    pub dynamics: Vec<u32>,
}

#[derive(Clone, Copy, Default)]
struct LayoutOwner {
    pins: usize,
    marked: u64,
    resident: u64,
}

/// Keeps a prepared container layout available even while its storage is empty.
#[derive(Clone)]
pub(crate) struct StorageLayoutLease(Arc<StorageLayoutPin>);

struct StorageLayoutPin {
    heap: Arc<RwLock<StorageHeap>>,
    layout: u32,
}

impl std::fmt::Debug for StorageLayoutLease {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_tuple("StorageLayoutLease")
            .field(&self.0.layout)
            .finish()
    }
}

impl Drop for StorageLayoutPin {
    fn drop(&mut self) {
        let mut heap = self.heap.write().unwrap_or_else(|error| error.into_inner());
        if heap.state != StorageState::Closed {
            heap.layout_owners[self.layout as usize].pins -= 1;
            heap.stats.resident_page_bytes -=
                (size_of::<StorageLayoutPin>() + 2 * size_of::<usize>()) as u64;
        }
    }
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct StorageStats {
    pub occupied_bytes: u64,
    pub young_bytes: u64,
    pub old_bytes: u64,
    pub reserved_bytes: u64,
    pub last_major_reachable_bytes: u64,
    pub resident_page_bytes: u64,
    pub total_allocated_bytes: u64,
    pub allocations: u64,
    pub minor_collections: u64,
    pub major_collections: u64,
    pub copied_bytes: u64,
    pub promoted_bytes: u64,
    pub promoted_objects: u64,
    pub reclaimed_bytes: u64,
    pub peak_bytes: u64,
    pub scanned_roots: u64,
    pub scanned_cards: u64,
    pub scanned_references: u64,
    pub scanned_objects: u64,
    pub traced_objects: u64,
    pub collection_nanos: u64,
}

/// Physical nursery state used by the owner scheduler when an allocation
/// reports that it needs a refill.  This is deliberately read-only: the
/// collector remains the sole owner of page rotation and promotion.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub(crate) struct StorageNurseryPressure {
    pub limit: u64,
    pub refill_required: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum StorageState {
    Open,
    Closed,
}

#[derive(Debug)]
struct Slot {
    generation: u32,
    state: u8,
    layout: u32,
    count: u32,
    page: u32,
    offset: u32,
    size: u32,
    logical: u64,
    marked: AtomicU64,
    age: u8,
    young: bool,
    occupied_index: usize,
    young_index: usize,
    metadata: RwLock<Option<StorageObjectMeta>>,
}

/// Directory metadata owned by the managed storage entry. The VM keeps no
/// second identity map: codecs publish their type and prepared lane shape
/// here, next to the slot location and generation used by collection.
#[derive(Clone, Debug)]
pub(crate) struct StorageObjectMeta {
    pub logical_bytes: u64,
    pub typ: Option<TypeIdentity>,
    pub initialized: bool,
    pub payload_len: u32,
    pub kind: StoragePayloadKind,
    pub version: u64,
}

/// A self-consistent copy of one published payload.  Metadata and bytes are
/// captured while the heap read guard and page read lock are both held, so a
/// concurrent same-shape rewrite cannot pair the new payload with stale type
/// or length metadata.
#[derive(Clone, Debug)]
pub(crate) struct StorageSnapshot {
    pub(crate) metadata: StorageObjectMeta,
    pub(crate) bytes: Vec<u8>,
}

/// A fixed-size publication prepared by the heap owner. All entries are
/// validated before any bytes are changed, so commit cannot fail halfway
/// through a result batch due to a stale handle or a shape mismatch.
pub(crate) struct StorageRewrite {
    pub(crate) handle: StorageRef,
    pub(crate) layout: u32,
    pub(crate) bytes: Vec<u8>,
    pub(crate) metadata: StorageObjectMeta,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum StoragePayloadKind {
    Typed,
    Record,
    RawBytes,
}

impl Default for Slot {
    fn default() -> Self {
        Self {
            generation: 0,
            state: FREE,
            layout: 0,
            count: 0,
            page: 0,
            offset: 0,
            size: 0,
            logical: 0,
            marked: AtomicU64::new(0),
            age: 0,
            young: false,
            occupied_index: 0,
            young_index: 0,
            metadata: RwLock::new(None),
        }
    }
}

impl Clone for Slot {
    fn clone(&self) -> Self {
        Self {
            generation: self.generation,
            state: self.state,
            layout: self.layout,
            count: self.count,
            page: self.page,
            offset: self.offset,
            size: self.size,
            logical: self.logical,
            marked: AtomicU64::new(self.marked.load(Ordering::Relaxed)),
            age: self.age,
            young: self.young,
            occupied_index: self.occupied_index,
            young_index: self.young_index,
            metadata: RwLock::new(self.metadata.read().unwrap().clone()),
        }
    }
}

impl Slot {
    /// The payload remains immutable for the stopped-mutator collection.
    /// Both serial and parallel markers use this same reference-lane scanner.
    fn visit_references(
        &self,
        layout: &StorageLayout,
        page: &Page,
        start: u32,
        end: u32,
        mut visit: impl FnMut(StorageRef) -> Result<(), RuntimeError>,
    ) -> Result<(), RuntimeError> {
        let first = if start <= self.offset {
            0
        } else {
            min(self.count, (start - self.offset) / layout.stride)
        };
        let last = if end == u32::MAX {
            self.count
        } else {
            min(
                self.count,
                end.saturating_sub(self.offset).div_ceil(layout.stride),
            )
        };
        let bytes = page.bytes.read().unwrap();
        let read = |position: u32| {
            let bytes = &bytes[position as usize..position as usize + 8];
            StorageRef {
                slot: u32::from_le_bytes(bytes[..4].try_into().unwrap()),
                generation: u32::from_le_bytes(bytes[4..].try_into().unwrap()),
            }
        };
        for element in first..last {
            let base = self.offset + element * layout.stride;
            for offset in &layout.references {
                let position = base + offset;
                if position + 8 <= start {
                    continue;
                }
                if position >= end {
                    break;
                }
                visit(read(position))?;
            }
            for offset in &layout.dynamics {
                let position = base + offset;
                // A tag and its ticket can straddle cards; inspect either part.
                if position + 16 <= start {
                    continue;
                }
                if position + 4 >= end {
                    break;
                }
                let representation = u32::from_le_bytes(
                    bytes[position as usize + 4..position as usize + 8]
                        .try_into()
                        .unwrap(),
                );
                if representation == VALUE_REF || representation == VALUE_BOX {
                    visit(read(position + 8))?;
                }
            }
        }
        Ok(())
    }
}

struct Page {
    bytes: RwLock<Vec<u8>>,
    kind: u8,
    leased: bool,
    used: u32,
    partner: u32,
    class: u32,
    entries: Vec<u32>,
    free: Vec<u32>,
    cards: RwLock<Vec<u8>>,
    resident: u64,
}

impl Page {
    fn new(size: u32, kind: u8, class: u32) -> Self {
        let entry_count = if kind == OLD {
            (size / class) as usize
        } else if kind == LARGE {
            1
        } else {
            0
        };
        let card_count = if kind == OLD || kind == LARGE {
            size.div_ceil(CARD_BYTES) as usize
        } else {
            0
        };
        let mut free = Vec::with_capacity(entry_count);
        for index in (0..entry_count).rev() {
            free.push(index as u32);
        }
        Self {
            bytes: RwLock::new(vec![0; size as usize]),
            kind,
            leased: false,
            used: 0,
            partner: 0,
            class,
            entries: vec![0; entry_count],
            free,
            cards: RwLock::new(vec![0; card_count]),
            resident: size as u64 + (entry_count * 8 + card_count) as u64,
        }
    }

    fn vacant() -> Self {
        Self {
            bytes: RwLock::new(Vec::new()),
            kind: FREE,
            leased: false,
            used: 0,
            partner: 0,
            class: 0,
            entries: Vec::new(),
            free: Vec::new(),
            cards: RwLock::new(Vec::new()),
            resident: 0,
        }
    }

    fn len(&self) -> usize {
        self.bytes.read().unwrap().len()
    }

    fn read_range(&self, offset: usize, length: usize) -> Vec<u8> {
        let bytes = self.bytes.read().unwrap();
        bytes[offset..offset + length].to_vec()
    }

    fn read_range_into(&self, offset: usize, destination: &mut [u8]) {
        let bytes = self.bytes.read().unwrap();
        destination.copy_from_slice(&bytes[offset..offset + destination.len()]);
    }

    fn write_range(&self, offset: usize, data: &[u8]) {
        let mut bytes = self.bytes.write().unwrap();
        bytes[offset..offset + data.len()].copy_from_slice(data);
    }

    fn fill_range(&self, offset: usize, length: usize, value: u8) {
        self.bytes.write().unwrap()[offset..offset + length].fill(value);
    }
}

#[derive(Clone, Copy)]
struct Card {
    page: u32,
    index: u32,
}

pub struct StorageReservation<'a> {
    heap: &'a mut StorageHeap,
    handle: StorageRef,
    finished: bool,
}

/// A bounded mutable view of a reservation payload.  The page bytes are
/// protected even while a caller initializes an object; the reservation
/// borrow keeps directory movement impossible until this guard is dropped.
pub struct StorageReservationBytes<'a> {
    guard: RwLockWriteGuard<'a, Vec<u8>>,
    start: usize,
    end: usize,
}

impl Deref for StorageReservationBytes<'_> {
    type Target = [u8];

    fn deref(&self) -> &Self::Target {
        &self.guard[self.start..self.end]
    }
}

impl DerefMut for StorageReservationBytes<'_> {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.guard[self.start..self.end]
    }
}

impl StorageReservation<'_> {
    pub fn bytes(&mut self) -> Result<StorageReservationBytes<'_>, RuntimeError> {
        if self.finished {
            return Err(storage_error("reservation is finished"));
        }
        self.heap.reservation_bytes(self.handle)
    }

    pub fn commit(mut self) -> StorageRef {
        self.heap.finish_reservation(self.handle, true);
        self.finished = true;
        self.handle
    }

    pub fn abort(&mut self) {
        if !self.finished {
            self.heap.finish_reservation(self.handle, false);
            self.finished = true;
        }
    }
}

impl Drop for StorageReservation<'_> {
    fn drop(&mut self) {
        self.abort();
    }
}

/// Shared directory and published pages. Readers may access committed payload
/// concurrently; publication, versioned writes and collection hold the owner
/// write guard. Worker leases own their nursery bytes exclusively until flush.
#[derive(Clone)]
pub struct StorageWorkerPool {
    heap: Arc<RwLock<StorageHeap>>,
}

/// Logical capacity owned by a live runtime control structure. It has no
/// payload edges: those belong to the frame/task's normal root enumerator.
/// Dropping an unpublished lease aborts admission; dropping a committed lease
/// releases capacity when its owner ends. Admission and release update the
/// heap's capacity ledger directly.
pub(crate) struct StorageControlLease {
    heap: Arc<RwLock<StorageHeap>>,
    bytes: u64,
    committed: bool,
}

/// Admitted physical buffers owned by a frame or another runtime structure.
/// The lease stays charged while that owner is cached; guest allocation
/// counters describe separate activation and payload lifetimes.
pub(crate) struct StorageResidentLease {
    heap: Arc<RwLock<StorageHeap>>,
    bytes: u64,
}

impl StorageResidentLease {
    pub(crate) fn ensure_capacity(&mut self, bytes: u64) -> Result<(), RuntimeError> {
        if bytes <= self.bytes {
            return Ok(());
        }
        let mut heap = self
            .heap
            .write()
            .map_err(|_| storage_error("managed storage lock poisoned"))?;
        if heap.state == StorageState::Closed {
            return Err(storage_error("managed storage is closed"));
        }
        heap.admit_resident(bytes - self.bytes)?;
        self.bytes = bytes;
        Ok(())
    }
}

impl Drop for StorageResidentLease {
    fn drop(&mut self) {
        let mut heap = self.heap.write().unwrap_or_else(|error| error.into_inner());
        if heap.state != StorageState::Closed {
            heap.stats.resident_page_bytes -= self.bytes;
        }
    }
}

impl StorageControlLease {
    pub(crate) fn from_transferred(pool: &StorageWorkerPool, bytes: u64) -> Self {
        Self {
            heap: pool.heap.clone(),
            bytes,
            committed: false,
        }
    }

    pub(crate) fn commit(mut self) -> Self {
        {
            let mut heap = self.heap.write().unwrap_or_else(|error| error.into_inner());
            heap.stats.reserved_bytes -= self.bytes;
            heap.stats.occupied_bytes += self.bytes;
            heap.stats.total_allocated_bytes += self.bytes;
            heap.stats.allocations += 1;
            heap.stats.peak_bytes = heap.stats.peak_bytes.max(
                heap.stats
                    .occupied_bytes
                    .saturating_add(heap.stats.reserved_bytes),
            );
        }
        self.committed = true;
        self
    }
}

impl Drop for StorageControlLease {
    fn drop(&mut self) {
        let mut heap = self.heap.write().unwrap_or_else(|error| error.into_inner());
        if self.committed {
            heap.stats.occupied_bytes = heap.stats.occupied_bytes.saturating_sub(self.bytes);
            heap.stats.reclaimed_bytes += self.bytes;
        } else {
            heap.stats.reserved_bytes = heap.stats.reserved_bytes.saturating_sub(self.bytes);
        }
    }
}

pub struct StorageWorkerLease {
    pool: StorageWorkerPool,
    page: u32,
    page_bytes: Vec<u8>,
    initial_used: u32,
    slot_ids: Vec<WorkerSlot>,
    credit: u64,
    reserved: u64,
    committed: u64,
    open: usize,
    used: u32,
    resident: u64,
    objects: Vec<WorkerObject>,
    finished: bool,
}

struct WorkerObject {
    handle: StorageRef,
    layout: u32,
    count: u32,
    offset: u32,
    size: u32,
    logical: u64,
    committed: bool,
    metadata: Option<StorageObjectMeta>,
}

struct WorkerSlot {
    slot: u32,
    generation: u32,
}

pub struct StorageHeap {
    layouts: Vec<StorageLayout>,
    layout_owners: Vec<LayoutOwner>,
    slots: Vec<Slot>,
    pages: Vec<Page>,
    free_pages: Vec<u32>,
    free_slots: Vec<u32>,
    occupied: Vec<u32>,
    young_slots: Vec<u32>,
    young_classes: [usize; 12],
    nursery_pages: Vec<u32>,
    old_available: [Vec<u32>; 14],
    pending_cards: Vec<Card>,
    /// Cards dirtied by published payload writers while the heap is held by
    /// a shared read guard. The collector merges this queue at its write
    /// boundary before scanning cards.
    shared_pending_cards: Mutex<Vec<Card>>,
    queue: Vec<u32>,
    next_slot: u32,
    epoch: u64,
    unpublished: usize,
    nursery_capacity: u64,
    nursery_limit: u64,
    max_guest_bytes: Option<u64>,
    max_resident_bytes: Option<u64>,
    stats: StorageStats,
    state: StorageState,
    active_workers: usize,
    collector_workers: usize,
}

impl Default for StorageHeap {
    fn default() -> Self {
        Self::new(None, None, 4 << 20)
    }
}

impl StorageHeap {
    pub fn new(
        max_guest_bytes: Option<u64>,
        max_resident_bytes: Option<u64>,
        nursery_limit: u64,
    ) -> Self {
        Self {
            layouts: vec![StorageLayout::default()],
            layout_owners: vec![LayoutOwner::default()],
            slots: vec![Slot::default()],
            pages: Vec::new(),
            free_pages: Vec::new(),
            free_slots: Vec::new(),
            occupied: Vec::new(),
            young_slots: Vec::new(),
            young_classes: [0; 12],
            nursery_pages: Vec::new(),
            old_available: std::array::from_fn(|_| Vec::new()),
            pending_cards: Vec::new(),
            shared_pending_cards: Mutex::new(Vec::new()),
            queue: Vec::new(),
            next_slot: 1,
            epoch: 0,
            unpublished: 0,
            nursery_capacity: 0,
            nursery_limit,
            max_guest_bytes,
            max_resident_bytes,
            stats: StorageStats {
                resident_page_bytes: (size_of::<StorageLayout>()
                    + size_of::<LayoutOwner>()
                    + size_of::<Slot>()) as u64,
                ..Default::default()
            },
            state: StorageState::Open,
            active_workers: 0,
            collector_workers: 1,
        }
    }

    pub fn register_layout(&mut self, mut layout: StorageLayout) -> Result<u32, RuntimeError> {
        if self.state == StorageState::Closed {
            return Err(storage_error("managed storage is closed"));
        }
        if layout.stride == 0 {
            return Err(storage_error("layout stride must be nonzero"));
        }
        layout.references.sort_unstable();
        layout.dynamics.sort_unstable();
        for (offset, width) in layout
            .references
            .iter()
            .copied()
            .map(|v| (v, 8))
            .chain(layout.dynamics.iter().copied().map(|v| (v, 24)))
        {
            if offset
                .checked_add(width)
                .is_none_or(|end| end > layout.stride)
            {
                return Err(storage_error("layout offset outside stride"));
            }
        }
        if layout
            .references
            .windows(2)
            .any(|pair| pair[0] + 8 > pair[1])
            || layout
                .dynamics
                .windows(2)
                .any(|pair| pair[0] + 24 > pair[1])
            || layout.dynamics.iter().any(|dynamic| {
                layout
                    .references
                    .iter()
                    .any(|reference| *reference < *dynamic + 24 && *reference + 8 > *dynamic)
            })
        {
            return Err(storage_error("overlapping layout offsets"));
        }
        if let Some(index) = self.layouts.iter().position(|existing| *existing == layout) {
            return Ok(index as u32);
        }
        let resident =
            ((layout.references.capacity() + layout.dynamics.capacity()) * size_of::<u32>()) as u64;
        if let Some(index) = self
            .layouts
            .iter()
            .enumerate()
            .skip(1)
            .find_map(|(index, layout)| (layout.stride == 0).then_some(index))
        {
            self.admit_resident(resident)?;
            self.layouts[index] = layout;
            self.layout_owners[index] = LayoutOwner {
                resident,
                ..Default::default()
            };
            return Ok(index as u32);
        }
        let count = self
            .layouts
            .len()
            .checked_add(1)
            .ok_or_else(storage_error_limit)?;
        let capacity = count
            .checked_next_power_of_two()
            .ok_or_else(storage_error_limit)?;
        let directory_bytes = self.layouts.capacity() * size_of::<StorageLayout>()
            + self.layout_owners.capacity() * size_of::<LayoutOwner>();
        let additional = (capacity.saturating_sub(self.layouts.capacity())
            * size_of::<StorageLayout>()
            + capacity.saturating_sub(self.layout_owners.capacity()) * size_of::<LayoutOwner>())
            as u64;
        self.admit_resident(additional)?;
        let reserve = self
            .layouts
            .try_reserve_exact(capacity.saturating_sub(self.layouts.len()))
            .and_then(|()| {
                self.layout_owners
                    .try_reserve_exact(capacity.saturating_sub(self.layout_owners.len()))
            });
        if reserve.is_err() {
            let retained = self.layouts.capacity() * size_of::<StorageLayout>()
                + self.layout_owners.capacity() * size_of::<LayoutOwner>()
                - directory_bytes;
            self.stats.resident_page_bytes -= additional - retained as u64;
            return Err(storage_error_limit());
        }
        self.admit_resident(resident)?;
        self.layouts.push(layout);
        self.layout_owners.push(LayoutOwner {
            resident,
            ..Default::default()
        });
        Ok((self.layouts.len() - 1) as u32)
    }

    pub fn layout(&self, index: u32) -> Result<&StorageLayout, RuntimeError> {
        let index = index as usize;
        if self
            .layouts
            .get(index)
            .is_none_or(|layout| layout.stride == 0)
        {
            return Err(storage_error("invalid allocation layout"));
        }
        self.layouts
            .get(index)
            .ok_or_else(|| storage_error("invalid allocation layout"))
    }

    pub fn into_worker_pool(self) -> StorageWorkerPool {
        StorageWorkerPool {
            heap: Arc::new(RwLock::new(self)),
        }
    }

    pub fn reserve<'a>(
        &'a mut self,
        layout: u32,
        count: u32,
        logical: u64,
    ) -> Result<StorageReservation<'a>, RuntimeError> {
        if self.state == StorageState::Closed || layout == 0 {
            return Err(storage_error("invalid allocation layout"));
        }
        let shape = self.layout(layout)?;
        let physical = u64::from(shape.stride)
            .checked_mul(u64::from(count))
            .ok_or_else(|| storage_error("allocation size overflow"))?;
        if physical > u64::from(u32::MAX - 7) || logical > MAX_BYTES {
            return Err(storage_error("allocation size overflow"));
        }
        let used = self
            .stats
            .occupied_bytes
            .saturating_add(self.stats.reserved_bytes);
        if logical > MAX_BYTES - used
            || self
                .max_guest_bytes
                .is_some_and(|limit| logical > limit.saturating_sub(used))
        {
            return Err(collection_error());
        }
        let size = max(8, physical.saturating_add(7) & !7) as u32;
        let young = size <= PAGE_BYTES / 4;
        let (page, offset) = if young {
            let page = self.nursery_page(size)?;
            let offset = self.pages[page as usize].used;
            let class = 1u32 << max(3, 32 - size.saturating_sub(1).leading_zeros());
            let class_index = (class.trailing_zeros() as usize).saturating_sub(3);
            self.prepare_promotion_space(class_index, self.young_classes[class_index] + 1);
            (page, offset)
        } else {
            (self.new_page(size, LARGE, 0, false)?, 0)
        };
        let slot = self.take_slot()?;
        let generation = self.slots[slot as usize]
            .generation
            .checked_add(1)
            .ok_or_else(|| storage_error("generation exhausted"))?;
        let entry = &mut self.slots[slot as usize];
        entry.generation = generation;
        entry.state = RESERVED;
        entry.layout = layout;
        entry.count = count;
        entry.page = page;
        entry.offset = offset;
        entry.size = size;
        entry.logical = logical;
        entry.marked.store(0, Ordering::Relaxed);
        entry.age = 0;
        entry.young = young;
        self.pages[page as usize].used = self.pages[page as usize].used.saturating_add(size);
        if !young {
            self.pages[page as usize].entries[0] = slot;
        }
        self.stats.reserved_bytes += logical;
        self.unpublished += 1;
        Ok(StorageReservation {
            heap: self,
            handle: StorageRef { slot, generation },
            finished: false,
        })
    }

    pub fn read(
        &self,
        handle: StorageRef,
        offset: u32,
        length: u32,
    ) -> Result<Vec<u8>, RuntimeError> {
        let slot = self.validate(handle)?;
        self.check_range(slot, offset, length)?;
        let entry = &self.slots[handle.slot as usize];
        Ok(self.pages[entry.page as usize]
            .read_range((entry.offset + offset) as usize, length as usize))
    }

    pub(crate) fn read_into(
        &self,
        handle: StorageRef,
        offset: u32,
        destination: &mut [u8],
    ) -> Result<(), RuntimeError> {
        let length =
            u32::try_from(destination.len()).map_err(|_| storage_error("read size overflow"))?;
        let slot = self.validate(handle)?;
        self.check_range(slot, offset, length)?;
        let entry = &self.slots[handle.slot as usize];
        self.pages[entry.page as usize]
            .read_range_into((entry.offset + offset) as usize, destination);
        Ok(())
    }

    fn read_snapshot_range(
        &self,
        handle: StorageRef,
        offset: u32,
        requested_length: Option<u32>,
    ) -> Result<StorageSnapshot, RuntimeError> {
        let slot = self.validate(handle)? as usize;
        let entry = &self.slots[slot];
        let metadata_guard = entry.metadata.read().unwrap();
        let metadata = metadata_guard.clone().ok_or_else(stale_error)?;
        let length = requested_length.unwrap_or(metadata.payload_len);
        if offset > metadata.payload_len || length > metadata.payload_len.saturating_sub(offset) {
            return Err(storage_error("snapshot range exceeds payload"));
        }
        self.check_range(slot as u32, offset, length)?;
        let absolute = entry
            .offset
            .checked_add(offset)
            .ok_or_else(|| storage_error("snapshot range overflows"))?;
        let end = absolute
            .checked_add(length)
            .ok_or_else(|| storage_error("snapshot range overflows"))?;
        if end as usize > self.pages[entry.page as usize].len() {
            return Err(storage_error("snapshot range exceeds page"));
        }
        let bytes = self.pages[entry.page as usize].read_range(absolute as usize, length as usize);
        let snapshot = StorageSnapshot { metadata, bytes };
        drop(metadata_guard);
        Ok(snapshot)
    }

    /// Copy a prepared range into caller-owned storage while retaining the
    /// metadata guard for the entire operation. The caller receives the same
    /// metadata/bytes pairing as `read_snapshot_range` without allocating a
    /// temporary byte vector.
    /// Validate a prepared payload while holding its metadata read guard, then
    /// copy the requested bytes into the caller buffer. A rejected metadata
    /// shape returns `Ok(false)` without touching the destination. The page
    /// copy only happens after validation and while the same guard is held.
    pub(crate) fn read_snapshot_into_if<F>(
        &self,
        handle: StorageRef,
        offset: u32,
        destination: &mut [u8],
        mut validate: F,
    ) -> Result<bool, RuntimeError>
    where
        F: FnMut(&StorageObjectMeta) -> Result<bool, RuntimeError>,
    {
        let slot = self.validate(handle)? as usize;
        let entry = &self.slots[slot];
        let metadata_guard = entry.metadata.read().unwrap();
        let metadata = metadata_guard.clone().ok_or_else(stale_error)?;
        if !validate(&metadata)? {
            return Ok(false);
        }
        let length =
            u32::try_from(destination.len()).map_err(|_| storage_error("read size overflow"))?;
        if offset > metadata.payload_len || length > metadata.payload_len.saturating_sub(offset) {
            return Err(storage_error("snapshot range exceeds payload"));
        }
        self.check_range(slot as u32, offset, length)?;
        let absolute = entry
            .offset
            .checked_add(offset)
            .ok_or_else(|| storage_error("snapshot range overflows"))?;
        let end = absolute
            .checked_add(length)
            .ok_or_else(|| storage_error("snapshot range overflows"))?;
        if end as usize > self.pages[entry.page as usize].len() {
            return Err(storage_error("snapshot range exceeds page"));
        }
        self.pages[entry.page as usize].read_range_into(absolute as usize, destination);
        Ok(true)
    }

    pub fn write(
        &mut self,
        handle: StorageRef,
        offset: u32,
        bytes: &[u8],
    ) -> Result<(), RuntimeError> {
        let slot = self.validate(handle)?;
        let length =
            u32::try_from(bytes.len()).map_err(|_| storage_error("write size overflow"))?;
        self.check_range(slot, offset, length)?;
        let entry = &self.slots[slot as usize];
        self.pages[entry.page as usize].write_range((entry.offset + offset) as usize, bytes);
        self.dirty_region(slot, offset, length)
    }

    /// Short write for an already published object. The caller holds the
    /// heap read guard, so relocation/collection cannot race the directory
    /// lookup. Page bytes, slot metadata and remembered-card publication are
    /// protected independently, allowing unrelated published pages to write
    /// concurrently without exposing a partial payload to readers.
    fn write_shared(
        &self,
        handle: StorageRef,
        offset: u32,
        bytes: &[u8],
    ) -> Result<(), RuntimeError> {
        let slot = self.validate(handle)?;
        let length =
            u32::try_from(bytes.len()).map_err(|_| storage_error("write size overflow"))?;
        self.check_range(slot, offset, length)?;
        let entry = &self.slots[slot as usize];
        self.pages[entry.page as usize].write_range((entry.offset + offset) as usize, bytes);
        self.mark_card_shared(entry.page, entry.offset + offset, length, entry.young);
        Ok(())
    }

    #[expect(
        clippy::too_many_arguments,
        reason = "one storage transaction owns the payload, accounting, metadata and version check"
    )]
    fn replace_shared(
        &self,
        handle: StorageRef,
        layout: u32,
        count: u32,
        logical: u64,
        bytes: &[u8],
        expected_version: Option<u64>,
        metadata: StorageObjectMeta,
    ) -> Result<Option<bool>, RuntimeError> {
        let slot = self.validate(handle)? as usize;
        let shape = self.layout(layout)?.stride as u64 * u64::from(count);
        let size = max(8, shape.saturating_add(7) & !7);
        let entry = &self.slots[slot];
        if entry.layout != layout
            || entry.count != count
            || entry.logical != logical
            || u64::from(entry.size) != size
            || bytes.len() as u64 > size
        {
            return Ok(None);
        }
        let mut current = entry.metadata.write().unwrap();
        if expected_version.is_some_and(|version| {
            current
                .as_ref()
                .is_none_or(|metadata| metadata.version != version)
        }) {
            return Ok(Some(false));
        }
        {
            let mut page = self.pages[entry.page as usize].bytes.write().unwrap();
            let payload = &mut page[entry.offset as usize..(entry.offset + entry.size) as usize];
            payload.fill(0);
            payload[..bytes.len()].copy_from_slice(bytes);
        }
        self.mark_card_shared(entry.page, entry.offset, entry.size, entry.young);
        *current = Some(metadata);
        Ok(Some(true))
    }

    fn mark_card_shared(&self, page: u32, offset: u32, size: u32, young: bool) {
        if size == 0 || young {
            return;
        }
        let first = offset / CARD_BYTES;
        let last = (offset + size - 1) / CARD_BYTES;
        let page_ref = &self.pages[page as usize];
        let mut cards = page_ref.cards.write().unwrap();
        let mut pending = self.shared_pending_cards.lock().unwrap();
        for card in first..=last {
            if cards[card as usize] == 0 {
                cards[card as usize] = 1;
                pending.push(Card { page, index: card });
            }
        }
    }

    pub(crate) fn clear(
        &mut self,
        handle: StorageRef,
        offset: u32,
        length: u32,
    ) -> Result<(), RuntimeError> {
        let slot = self.validate(handle)?;
        self.check_range(slot, offset, length)?;
        let entry = &self.slots[slot as usize];
        let start = (entry.offset + offset) as usize;
        self.pages[entry.page as usize].fill_range(start, length as usize, 0);
        self.dirty_region(slot, offset, length)
    }

    pub fn read_reference(
        &self,
        handle: StorageRef,
        offset: u32,
    ) -> Result<StorageRef, RuntimeError> {
        let slot = self.validate(handle)?;
        self.check_range(slot, offset, 8)?;
        let entry = &self.slots[slot as usize];
        let data = self.pages[entry.page as usize].read_range((entry.offset + offset) as usize, 8);
        Ok(StorageRef {
            slot: u32::from_le_bytes(data[0..4].try_into().unwrap()),
            generation: u32::from_le_bytes(data[4..8].try_into().unwrap()),
        })
    }

    pub fn write_reference(
        &mut self,
        handle: StorageRef,
        offset: u32,
        child: StorageRef,
    ) -> Result<(), RuntimeError> {
        let slot = self.validate(handle)?;
        self.check_range(slot, offset, 8)?;
        let entry = &self.slots[slot as usize];
        let mut data = [0_u8; 8];
        data[0..4].copy_from_slice(&child.slot.to_le_bytes());
        data[4..8].copy_from_slice(&child.generation.to_le_bytes());
        self.pages[entry.page as usize].write_range((entry.offset + offset) as usize, &data);
        self.dirty_region(handle.slot, offset, 8)
    }

    /// Rewrite an object while preserving its slot and generation. A rewrite
    /// that outgrows the current payload relocates the bytes to a page of the
    /// same generation class and updates the directory atomically from the
    /// caller's point of view.
    pub fn rewrite(
        &mut self,
        handle: StorageRef,
        layout: u32,
        count: u32,
        logical: u64,
        bytes: &[u8],
    ) -> Result<(), RuntimeError> {
        let slot = self.validate(handle)? as usize;
        let stride = self.layout(layout)?.stride;
        let physical = u64::from(stride)
            .checked_mul(u64::from(count))
            .ok_or_else(|| storage_error("allocation size overflow"))?;
        if physical > u64::from(u32::MAX - 7) || logical > MAX_BYTES {
            return Err(storage_error("allocation size overflow"));
        }
        let size = max(8, physical.saturating_add(7) & !7) as u32;
        if bytes.len() as u64 > u64::from(size) || logical > MAX_BYTES {
            return Err(storage_error("allocation size overflow"));
        }
        let old = self.slots[slot].clone();
        let used = if logical >= old.logical {
            self.stats.occupied_bytes.checked_add(logical - old.logical)
        } else {
            self.stats.occupied_bytes.checked_sub(old.logical - logical)
        }
        .ok_or_else(collection_error)?;
        let admitted = used.saturating_add(self.stats.reserved_bytes);
        if admitted > MAX_BYTES || self.max_guest_bytes.is_some_and(|limit| admitted > limit) {
            return Err(collection_error());
        }
        if size <= old.size {
            self.pages[old.page as usize].fill_range(old.offset as usize, old.size as usize, 0);
            self.pages[old.page as usize].write_range(old.offset as usize, bytes);
            self.slots[slot].layout = layout;
            self.slots[slot].count = count;
            self.slots[slot].size = size;
            if old.young {
                let old_class =
                    max(3, 32 - old.size.saturating_sub(1).leading_zeros()) as usize - 3;
                let new_class = max(3, 32 - size.saturating_sub(1).leading_zeros()) as usize - 3;
                self.young_classes[old_class] -= 1;
                self.young_classes[new_class] += 1;
                self.prepare_promotion_space(new_class, self.young_classes[new_class]);
            }
            self.slots[slot].logical = logical;
            self.stats.occupied_bytes = used;
            if old.young {
                self.stats.young_bytes = self
                    .stats
                    .young_bytes
                    .saturating_sub(old.logical)
                    .saturating_add(logical);
            } else {
                self.stats.old_bytes = self
                    .stats
                    .old_bytes
                    .saturating_sub(old.logical)
                    .saturating_add(logical);
            }
            self.stats.total_allocated_bytes += logical.saturating_sub(old.logical);
            self.stats.reclaimed_bytes += old.logical.saturating_sub(logical);
            self.stats.peak_bytes = self.stats.peak_bytes.max(
                self.stats
                    .occupied_bytes
                    .saturating_add(self.stats.reserved_bytes),
            );
            return self.dirty_region(handle.slot, 0, size);
        }

        let (page, offset, old_kind) = if size > TLAB_MAX_OBJECT_BYTES {
            let page = self.new_page(size, LARGE, 0, false)?;
            self.pages[page as usize].entries[0] = handle.slot;
            self.pages[page as usize].used = size;
            (page, 0, LARGE)
        } else if old.young {
            let page = self.nursery_page(size)?;
            let offset = self.pages[page as usize].used;
            self.pages[page as usize].used += size;
            (page, offset, YOUNG)
        } else if self.pages[old.page as usize].kind == LARGE {
            let page = self.new_page(size, LARGE, 0, false)?;
            (page, 0, LARGE)
        } else {
            let class = 1u32 << max(3, 32 - size.saturating_sub(1).leading_zeros());
            let page = match self.take_old_page(size) {
                Some(page) => page,
                None => self.new_page(PAGE_BYTES, OLD, class, false)?,
            };
            let index = self.pages[page as usize]
                .free
                .pop()
                .ok_or_else(collection_error)?;
            self.pages[page as usize].entries[index as usize] = handle.slot;
            self.pages[page as usize].used += self.pages[page as usize].class;
            (page, index * self.pages[page as usize].class, OLD)
        };
        self.pages[page as usize].fill_range(offset as usize, size as usize, 0);
        self.pages[page as usize].write_range(offset as usize, bytes);

        if old.young {
            // Nursery pages are bump allocated, so the old extent remains in
            // from-space until the next collection.
        } else if self.pages[old.page as usize].kind == OLD {
            let index = old.offset / self.pages[old.page as usize].class;
            self.pages[old.page as usize].entries[index as usize] = 0;
            self.pages[old.page as usize].free.push(index);
            self.pages[old.page as usize].used -= self.pages[old.page as usize].class;
        } else {
            self.pages[old.page as usize].entries[0] = 0;
            self.pages[old.page as usize].used = 0;
        }
        if old.young {
            self.remove_young(handle.slot);
        }
        self.slots[slot].layout = layout;
        self.slots[slot].count = count;
        self.slots[slot].page = page;
        self.slots[slot].offset = offset;
        self.slots[slot].size = size;
        self.slots[slot].logical = logical;
        self.slots[slot].young = old_kind == YOUNG;
        if old_kind == YOUNG {
            self.slots[slot].young_index = self.young_slots.len();
            self.young_slots.push(handle.slot);
            let class = max(3, 32 - size.saturating_sub(1).leading_zeros()) as usize - 3;
            self.young_classes[class] += 1;
            self.prepare_promotion_space(class, self.young_classes[class]);
        }
        self.stats.occupied_bytes = used;
        if old.young {
            self.stats.young_bytes = self.stats.young_bytes.saturating_sub(old.logical);
        } else {
            self.stats.old_bytes = self.stats.old_bytes.saturating_sub(old.logical);
        }
        if old_kind == YOUNG {
            self.stats.young_bytes = self.stats.young_bytes.saturating_add(logical);
        } else {
            self.stats.old_bytes = self.stats.old_bytes.saturating_add(logical);
        }
        self.stats.total_allocated_bytes += logical.saturating_sub(old.logical);
        self.stats.reclaimed_bytes += old.logical.saturating_sub(logical);
        self.stats.peak_bytes = self.stats.peak_bytes.max(
            self.stats
                .occupied_bytes
                .saturating_add(self.stats.reserved_bytes),
        );
        self.dirty_region(handle.slot, 0, size)
    }

    pub fn get(&self, handle: StorageRef) -> Result<(u32, u32, u32, bool), RuntimeError> {
        let index = self.validate(handle)? as usize;
        let entry = &self.slots[handle.slot as usize];
        Ok((
            entry.layout,
            entry.page,
            entry.offset,
            !entry.young && index != 0,
        ))
    }

    pub fn release(&mut self, handle: StorageRef) -> Result<(), RuntimeError> {
        self.validate(handle)?;
        self.free_object(handle.slot, true);
        Ok(())
    }

    pub fn stats(&self) -> StorageStats {
        self.stats
    }

    pub(crate) fn nursery_pressure(&self) -> StorageNurseryPressure {
        let refill_required = self.nursery_capacity >= self.nursery_limit
            && !self.nursery_pages.iter().any(|id| {
                let page = &self.pages[*id as usize];
                !page.leased && page.used == 0
            });
        StorageNurseryPressure {
            limit: self.nursery_limit,
            refill_required,
        }
    }

    /// Return the dynamic type IDs encoded by live descriptor lanes.  Dynamic
    /// aggregate payloads can be created by the heap codec itself (without an
    /// Instance runtimeType record), so the collector must retain the type
    /// metadata referenced by these published records before pruning the
    /// registry.
    pub(crate) fn live_dynamic_type_ids(&self) -> HashSet<u32> {
        let mut ids = HashSet::new();
        for slot in &self.occupied {
            let entry = &self.slots[*slot as usize];
            let Some(layout) = self.layouts.get(entry.layout as usize) else {
                continue;
            };
            let page = self.pages[entry.page as usize].bytes.read().unwrap();
            for element in 0..entry.count {
                let base = entry
                    .offset
                    .saturating_add(element.saturating_mul(layout.stride));
                for offset in &layout.dynamics {
                    let position = base.saturating_add(*offset);
                    if position.saturating_add(4) <= entry.offset.saturating_add(entry.size)
                        && position.saturating_add(4) <= page.len() as u32
                    {
                        let id = u32::from_le_bytes(
                            page[position as usize..position as usize + 4]
                                .try_into()
                                .unwrap(),
                        );
                        if id != 0 {
                            ids.insert(id);
                        }
                    }
                }
            }
        }
        ids
    }

    pub(crate) fn live_payload_types(&self) -> Vec<crate::types::TypeIdentity> {
        self.occupied
            .iter()
            .filter_map(|slot| {
                self.slots[*slot as usize]
                    .metadata
                    .read()
                    .unwrap()
                    .as_ref()?
                    .typ
                    .clone()
            })
            .collect()
    }

    pub fn collect_minor<I: IntoIterator<Item = StorageRef>>(
        &mut self,
        roots: I,
    ) -> Result<usize, RuntimeError> {
        self.collect(roots, false)
    }

    pub fn collect_major<I: IntoIterator<Item = StorageRef>>(
        &mut self,
        roots: I,
    ) -> Result<usize, RuntimeError> {
        self.collect(roots, true)
    }

    fn finish_reservation(&mut self, handle: StorageRef, commit: bool) {
        let slot = handle.slot as usize;
        let logical = self.slots[slot].logical;
        self.stats.reserved_bytes -= logical;
        self.unpublished -= 1;
        if !commit {
            self.free_object(handle.slot, false);
            return;
        }
        let index = self.occupied.len();
        self.slots[slot].occupied_index = index;
        self.occupied.push(handle.slot);
        if self.slots[slot].young {
            self.slots[slot].young_index = self.young_slots.len();
            self.young_slots.push(handle.slot);
            let class = max(
                3,
                32 - self.slots[slot].size.saturating_sub(1).leading_zeros(),
            ) as usize
                - 3;
            self.young_classes[class] += 1;
            self.stats.young_bytes += logical;
        } else {
            self.stats.old_bytes += logical;
        }
        self.stats.occupied_bytes += logical;
        self.stats.total_allocated_bytes += logical;
        self.stats.allocations += 1;
        self.stats.peak_bytes = self.stats.peak_bytes.max(
            self.stats
                .occupied_bytes
                .saturating_add(self.stats.reserved_bytes),
        );
        self.slots[slot].state = PUBLISHED;
        let entry = &self.slots[slot];
        // A large allocation is old from its first publication. Its initial
        // payload can already contain nursery references, before any ordinary
        // write has had an opportunity to run the barrier.
        self.mark_card_shared(entry.page, entry.offset, entry.size, entry.young);
    }

    fn reservation_bytes(
        &mut self,
        handle: StorageRef,
    ) -> Result<StorageReservationBytes<'_>, RuntimeError> {
        self.validate_state(handle, RESERVED)?;
        let entry = &self.slots[handle.slot as usize];
        let page = &self.pages[entry.page as usize];
        let guard = page.bytes.write().unwrap();
        Ok(StorageReservationBytes {
            guard,
            start: entry.offset as usize,
            end: (entry.offset + entry.size) as usize,
        })
    }

    fn collect<I: IntoIterator<Item = StorageRef>>(
        &mut self,
        roots: I,
        major: bool,
    ) -> Result<usize, RuntimeError> {
        if self.state == StorageState::Closed || self.unpublished != 0 || self.active_workers != 0 {
            return Err(storage_error("collection requires committed allocations"));
        }
        let started = Instant::now();
        self.epoch = self.epoch.wrapping_add(1);
        if self.epoch == 0 {
            for slot in &self.slots {
                slot.marked.store(0, Ordering::Relaxed);
            }
            for owner in &mut self.layout_owners {
                owner.marked = 0;
            }
            self.epoch = 1;
        }
        // Published mutators enqueue cards while holding only the heap read
        // guard. At the collector write boundary no such writer can remain,
        // so merge their queue before tracing.
        if let Ok(mut shared) = self.shared_pending_cards.lock() {
            self.pending_cards.extend(shared.drain(..));
        }
        self.queue.clear();
        let mut roots_count = 0;
        for root in roots {
            roots_count += 1;
            self.mark(root, major)?;
        }
        self.stats.scanned_roots += roots_count;
        self.drain_queue(major)?;
        let mut reclaimed = 0;
        if !major {
            self.queue.clear();
            for card_index in 0..self.pending_cards.len() {
                let card = self.pending_cards[card_index];
                self.stats.scanned_cards += 1;
                self.scan_card(card, false)?;
            }
            self.drain_queue(false)?;
        } else {
            reclaimed = self.sweep_old_parallel()?;
        }
        reclaimed += self.copy_nursery(major)?;
        if major {
            self.reclaim_empty_pages();
        }
        // Major sweep can turn a card's page into FREE before the card queue
        // is compacted.  Drop those stale tickets (and tickets whose card was
        // cleared by page reuse) before indexing the page below.
        let pages = &self.pages;
        self.pending_cards.retain(|card| {
            pages
                .get(card.page as usize)
                .and_then(|page| page.cards.read().unwrap().get(card.index as usize).copied())
                .is_some_and(|marked| marked != 0)
        });
        let mut kept = 0;
        for card_index in 0..self.pending_cards.len() {
            let card = self.pending_cards[card_index];
            if self.scan_card(card, true)? {
                self.pending_cards[kept] = card;
                kept += 1;
            } else {
                self.pages[card.page as usize].cards.write().unwrap()[card.index as usize] = 0;
            }
        }
        self.pending_cards.truncate(kept);
        if major {
            self.stats.last_major_reachable_bytes = self.stats.occupied_bytes;
            self.stats.major_collections += 1;
            self.release_unused_layout_metadata();
        } else {
            self.stats.minor_collections += 1;
        }
        self.stats.collection_nanos = self
            .stats
            .collection_nanos
            .saturating_add(started.elapsed().as_nanos().min(u128::from(u64::MAX)) as u64);
        Ok(reclaimed)
    }

    fn release_unused_layout_metadata(&mut self) {
        for slot in &self.occupied {
            self.layout_owners[self.slots[*slot as usize].layout as usize].marked = self.epoch;
        }
        for index in 1..self.layouts.len() {
            let owner = &mut self.layout_owners[index];
            if owner.pins == 0 && owner.marked != self.epoch {
                self.layouts[index] = StorageLayout::default();
                self.stats.resident_page_bytes -= owner.resident;
                owner.resident = 0;
            }
        }
    }

    fn mark(&mut self, handle: StorageRef, major: bool) -> Result<(), RuntimeError> {
        if !handle.valid() {
            if handle != StorageRef::NULL {
                return Err(stale_error());
            }
            return Ok(());
        }
        let index = self.validate(handle).map_err(|mut error| {
            error.path = "managed_storage/mark".into();
            error
        })? as usize;
        if !major && !self.slots[index].young
            || self.slots[index].marked.load(Ordering::Relaxed) == self.epoch
        {
            return Ok(());
        }
        self.slots[index]
            .marked
            .store(self.epoch, Ordering::Relaxed);
        self.queue.push(handle.slot);
        Ok(())
    }

    fn drain_queue(&mut self, major: bool) -> Result<(), RuntimeError> {
        let workers = if major {
            self.collector_workers
                .min(MAX_COLLECTOR_WORKERS)
                .min(self.queue.len())
        } else {
            1
        };
        if workers < 2 {
            let mut cursor = 0;
            while cursor < self.queue.len() {
                let slot = self.queue[cursor];
                let entry = &self.slots[slot as usize];
                if entry.state != PUBLISHED || entry.marked.load(Ordering::Relaxed) != self.epoch {
                    return Err(stale_error());
                }
                self.scan_object(slot, 0, u32::MAX, major, true)?;
                cursor += 1;
            }
            return Ok(());
        }
        // The owner has stopped every mutator. Only mark epochs and the
        // pre-admitted queue change while workers read immutable payload pages.
        let slots = &self.slots;
        let layouts = &self.layouts;
        let pages = &self.pages;
        let epoch = self.epoch;
        let queue = Mutex::new((0usize, &mut self.queue));
        let references = std::thread::scope(|scope| -> Result<u64, RuntimeError> {
            let mut threads = Vec::with_capacity(workers);
            for _ in 0..workers {
                let queue = &queue;
                threads.push(scope.spawn(move || -> Result<u64, RuntimeError> {
                    let mut references = 0;
                    loop {
                        let slot = {
                            let mut state = queue.lock().unwrap();
                            if state.0 == state.1.len() {
                                break;
                            }
                            let slot = state.1[state.0];
                            state.0 += 1;
                            slot
                        };
                        let entry = &slots[slot as usize];
                        if entry.state != PUBLISHED || entry.marked.load(Ordering::Relaxed) != epoch
                        {
                            return Err(stale_error());
                        }
                        entry.visit_references(
                            &layouts[entry.layout as usize],
                            &pages[entry.page as usize],
                            0,
                            u32::MAX,
                            |child| {
                                if child == StorageRef::NULL {
                                    return Ok(());
                                }
                                let target = slots
                                    .get(child.slot as usize)
                                    .filter(|target| {
                                        target.state == PUBLISHED
                                            && target.generation == child.generation
                                    })
                                    .ok_or_else(stale_error)?;
                                references += 1;
                                if target.marked.swap(epoch, Ordering::Relaxed) != epoch {
                                    let mut state = queue.lock().unwrap();
                                    if state.1.len() == state.1.capacity() {
                                        return Err(storage_error("collector scratch exhausted"));
                                    }
                                    state.1.push(child.slot);
                                }
                                Ok(())
                            },
                        )?;
                    }
                    Ok(references)
                }));
            }
            let mut references = 0;
            for thread in threads {
                references += thread
                    .join()
                    .map_err(|_| storage_error("collector worker panicked"))??;
            }
            Ok(references)
        })?;
        let processed = queue.lock().unwrap().0 as u64;
        self.stats.scanned_objects = self.stats.scanned_objects.saturating_add(processed);
        self.stats.traced_objects = self.stats.traced_objects.saturating_add(processed);
        self.stats.scanned_references += references;
        Ok(())
    }

    fn scan_object(
        &mut self,
        slot: u32,
        start: u32,
        end: u32,
        major: bool,
        trace: bool,
    ) -> Result<bool, RuntimeError> {
        self.stats.scanned_objects = self.stats.scanned_objects.saturating_add(1);
        if trace {
            self.stats.traced_objects = self.stats.traced_objects.saturating_add(1);
        }
        let entry = &self.slots[slot as usize];
        let slots = &self.slots;
        let queue = &mut self.queue;
        let epoch = self.epoch;
        let mut young = false;
        let mut references = 0;
        entry.visit_references(
            &self.layouts[entry.layout as usize],
            &self.pages[entry.page as usize],
            start,
            end,
            |child| {
                if child == StorageRef::NULL {
                    return Ok(());
                }
                let target = slots
                    .get(child.slot as usize)
                    .filter(|target| {
                        target.state == PUBLISHED && target.generation == child.generation
                    })
                    .ok_or_else(stale_error)?;
                young |= target.young;
                if trace {
                    references += 1;
                    if (major || target.young)
                        && target.marked.swap(epoch, Ordering::Relaxed) != epoch
                    {
                        if queue.len() == queue.capacity() {
                            return Err(storage_error("collector scratch exhausted"));
                        }
                        queue.push(child.slot);
                    }
                }
                Ok(())
            },
        )?;
        self.stats.scanned_references += references;
        Ok(young)
    }

    fn scan_card(&mut self, card: Card, check_only: bool) -> Result<bool, RuntimeError> {
        let start = card.index * CARD_BYTES;
        let end = {
            let page = &self.pages[card.page as usize];
            min(start + CARD_BYTES, page.len() as u32)
        };
        let (first, last) = if self.pages[card.page as usize].class == 0 {
            (
                0,
                min(1, self.pages[card.page as usize].entries.len() as u32),
            )
        } else {
            let page = &self.pages[card.page as usize];
            (
                start / page.class,
                min(page.entries.len() as u32, end.div_ceil(page.class)),
            )
        };
        let mut young = false;
        for index in first..last {
            let slot = self.pages[card.page as usize].entries[index as usize];
            if slot != 0 {
                young |= self.scan_object(slot, start, end, false, !check_only)?;
            }
        }
        Ok(young)
    }

    fn copy_nursery(&mut self, major: bool) -> Result<usize, RuntimeError> {
        let mut reclaimed = 0;
        for page_index in 0..self.nursery_pages.len() {
            let page_id = self.nursery_pages[page_index];
            let partner = self.pages[page_id as usize].partner;
            self.pages[partner as usize].kind = YOUNG;
            self.pages[partner as usize].used = 0;
            self.pages[partner as usize].fill_range(0, self.pages[partner as usize].len(), 0);
        }
        let mut index = 0;
        while index < self.young_slots.len() {
            let slot = self.young_slots[index];
            let entry = &self.slots[slot as usize];
            if entry.marked.load(Ordering::Relaxed) != self.epoch {
                if self.free_object(slot, true).is_some() {
                    reclaimed += 1;
                }
                continue;
            }
            let from = entry.page;
            let (source_offset, size, logical, age) =
                (entry.offset, entry.size, entry.logical, entry.age);
            let target = if major || age >= 1 {
                self.take_old_page(size)
                    .unwrap_or(self.pages[from as usize].partner)
            } else {
                self.pages[from as usize].partner
            };
            let promoted = self.pages[target as usize].kind == OLD;
            let offset = if promoted {
                let page = &mut self.pages[target as usize];
                let entry_index = page.free.pop().ok_or_else(collection_error)?;
                page.entries[entry_index as usize] = slot;
                page.used += page.class;
                entry_index * page.class
            } else {
                let offset = self.pages[target as usize].used;
                self.pages[target as usize].used += size;
                offset
            };
            copy_page_bytes(&mut self.pages, from, source_offset, target, offset, size)?;
            self.slots[slot as usize].page = target;
            self.slots[slot as usize].offset = offset;
            self.slots[slot as usize].age = self.slots[slot as usize].age.saturating_add(1);
            self.slots[slot as usize].young = !promoted;
            self.stats.copied_bytes += u64::from(size);
            if promoted {
                self.remove_young(slot);
                self.stats.young_bytes = self.stats.young_bytes.saturating_sub(logical);
                self.stats.old_bytes = self.stats.old_bytes.saturating_add(logical);
                self.stats.promoted_bytes += u64::from(size);
                self.stats.promoted_objects += 1;
                self.mark_young_references(slot)?;
            } else {
                index += 1;
            }
        }
        for index in 0..self.nursery_pages.len() {
            let from = self.nursery_pages[index];
            let partner = self.pages[from as usize].partner;
            self.nursery_pages[index] = partner;
            self.pages[from as usize].partner = partner;
            self.pages[partner as usize].partner = from;
            self.pages[from as usize].kind = SPARE;
            self.pages[from as usize].used = 0;
            self.pages[from as usize].fill_range(0, self.pages[from as usize].len(), 0xdd);
        }
        Ok(reclaimed)
    }

    fn mark_young_references(&mut self, slot: u32) -> Result<(), RuntimeError> {
        let entry = &self.slots[slot as usize];
        let layout_id = entry.layout as usize;
        let (page, offset, count) = (entry.page, entry.offset, entry.count);
        let stride = self.layouts[layout_id].stride;
        for element in 0..count {
            let base = offset + element * stride;
            for offset_index in 0..self.layouts[layout_id].references.len() {
                let offset = self.layouts[layout_id].references[offset_index];
                let child = self.read_reference_at(page, base + offset)?;
                if child.valid() && self.slots[self.validate(child)? as usize].young {
                    self.mark_card(page, base + offset, 8)?;
                }
            }
            // Dynamic descriptors carry a representation tag followed by a
            // managed ticket at +8. A promoted ring or aggregate can retain
            // a young descriptor box through this lane, so record the card
            // exactly as for a fixed reference lane.
            for offset_index in 0..self.layouts[layout_id].dynamics.len() {
                let offset = self.layouts[layout_id].dynamics[offset_index];
                let position = base + offset;
                let representation = self.read_u32(page, position + 4);
                if representation != VALUE_REF && representation != VALUE_BOX {
                    continue;
                }
                let child = self.read_reference_at(page, position + 8)?;
                if child.valid() && self.slots[self.validate(child)? as usize].young {
                    self.mark_card(page, position + 8, 8)?;
                }
            }
        }
        Ok(())
    }

    fn sweep_old_parallel(&mut self) -> Result<usize, RuntimeError> {
        let mut reclaimed = 0;
        let count = self.occupied.len();
        if count == 0 {
            return Ok(reclaimed);
        }
        if self.queue.capacity() < count {
            let mut index = 0;
            while index < self.occupied.len() {
                let slot = self.occupied[index];
                if !self.slots[slot as usize].young
                    && self.slots[slot as usize].marked.load(Ordering::Relaxed) != self.epoch
                {
                    if self.free_object(slot, true).is_some() {
                        reclaimed += 1;
                    }
                } else {
                    index += 1;
                }
            }
            return Ok(reclaimed);
        }
        self.queue.resize(count, 0);
        self.queue[..count].fill(0);
        let workers = self
            .collector_workers
            .min(MAX_COLLECTOR_WORKERS)
            .min(count.div_ceil(128))
            .max(1);
        if workers == 1 {
            for index in 0..count {
                let slot = self.occupied[index];
                if !self.slots[slot as usize].young
                    && self.slots[slot as usize].marked.load(Ordering::Relaxed) != self.epoch
                {
                    self.queue[index] = slot;
                }
            }
        } else {
            let occupied = &self.occupied;
            let slots = &self.slots;
            let epoch = self.epoch;
            let chunk = count.div_ceil(workers);
            std::thread::scope(|scope| {
                for (queue, objects) in self.queue[..count]
                    .chunks_mut(chunk)
                    .zip(occupied.chunks(chunk))
                {
                    scope.spawn(move || {
                        for (index, slot) in objects.iter().copied().enumerate() {
                            if !slots[slot as usize].young
                                && slots[slot as usize].marked.load(Ordering::Relaxed) != epoch
                            {
                                queue[index] = slot;
                            }
                        }
                    });
                }
            });
        }
        for index in 0..count {
            let slot = self.queue[index];
            if slot != 0 && self.free_object(slot, true).is_some() {
                reclaimed += 1;
            }
        }
        self.queue.clear();
        Ok(reclaimed)
    }

    fn reclaim_empty_pages(&mut self) {
        for id in 0..self.pages.len() {
            if self.pages[id].used != 0
                || (self.pages[id].kind != OLD && self.pages[id].kind != LARGE)
            {
                continue;
            }
            if self.pages[id].kind == OLD {
                let class_index = self.pages[id].class.trailing_zeros().saturating_sub(3) as usize;
                if self.old_available[class_index].len() <= 1 {
                    continue;
                }
                self.old_available[class_index].retain(|page| *page != id as u32);
            }
            self.stats.resident_page_bytes = self
                .stats
                .resident_page_bytes
                .saturating_sub(self.pages[id].resident);
            self.pages[id] = Page::vacant();
            self.free_pages.push(id as u32);
        }
    }

    fn free_object(&mut self, slot: u32, published: bool) -> Option<StorageRef> {
        let entry = &self.slots[slot as usize];
        let handle = StorageRef {
            slot,
            generation: entry.generation,
        };
        let page = &mut self.pages[entry.page as usize];
        if !entry.young {
            if page.kind == OLD {
                let index = entry.offset / page.class;
                page.entries[index as usize] = 0;
                page.free.push(index);
                page.used -= page.class;
                if page.free.len() == 1 {
                    self.old_available[(page.class.trailing_zeros() - 3) as usize].push(entry.page);
                }
            } else {
                page.entries[0] = 0;
                page.used = 0;
            }
        }
        if published {
            self.stats.occupied_bytes -= entry.logical;
            if entry.young {
                self.stats.young_bytes = self.stats.young_bytes.saturating_sub(entry.logical);
            } else {
                self.stats.old_bytes = self.stats.old_bytes.saturating_sub(entry.logical);
            }
            self.stats.reclaimed_bytes += entry.logical;
            let (occupied_index, young) = (entry.occupied_index, entry.young);
            let last = self.occupied.pop().unwrap();
            if last != slot {
                self.occupied[occupied_index] = last;
                self.slots[last as usize].occupied_index = occupied_index;
            }
            if young {
                self.remove_young(slot);
            }
        }
        self.slots[slot as usize].state = FREE;
        self.slots[slot as usize].logical = 0;
        *self.slots[slot as usize].metadata.write().unwrap() = None;
        if self.slots[slot as usize].generation != u32::MAX {
            self.free_slots.push(slot);
        }
        published.then_some(handle)
    }

    fn remove_young(&mut self, slot: u32) {
        let size = self.slots[slot as usize].size;
        let class = max(3, 32 - size.saturating_sub(1).leading_zeros()) as usize - 3;
        self.young_classes[class] -= 1;
        let index = self.slots[slot as usize].young_index;
        let last = self.young_slots.pop().unwrap();
        if last != slot {
            self.young_slots[index] = last;
            self.slots[last as usize].young_index = index;
        }
    }

    fn take_slot(&mut self) -> Result<u32, RuntimeError> {
        if let Some(slot) = self.free_slots.pop() {
            return Ok(slot);
        }
        if self.next_slot == u32::MAX {
            return Err(storage_error("slot limit"));
        }
        let slot = self.next_slot;
        if slot as usize >= self.slots.len() {
            let capacity = self.slots.len().saturating_mul(2).max(64);
            let resident = |heap: &Self| {
                (heap.slots.capacity() * size_of::<Slot>()
                    + (heap.queue.capacity()
                        + heap.free_slots.capacity()
                        + heap.occupied.capacity()
                        + heap.young_slots.capacity())
                        * size_of::<u32>()) as u64
            };
            let before = resident(self);
            let additional =
                (capacity * (size_of::<Slot>() + 4 * size_of::<u32>())) as u64 - before;
            self.admit_resident(additional)?;
            // A directory slot also admits its collector queue and free-list
            // space. Neither tracing nor sweeping grows these buffers.
            let reserve = (|| {
                self.slots
                    .try_reserve_exact(capacity.saturating_sub(self.slots.len()))?;
                self.queue
                    .try_reserve_exact(capacity.saturating_sub(self.queue.len()))?;
                self.free_slots
                    .try_reserve_exact(capacity.saturating_sub(self.free_slots.len()))?;
                self.occupied
                    .try_reserve_exact(capacity.saturating_sub(self.occupied.len()))?;
                self.young_slots
                    .try_reserve_exact(capacity.saturating_sub(self.young_slots.len()))?;
                Ok::<_, std::collections::TryReserveError>(())
            })();
            if reserve.is_err() {
                let retained = resident(self) - before;
                self.stats.resident_page_bytes -= additional - retained;
                return Err(storage_error_limit());
            }
            self.slots.resize(capacity, Slot::default());
        }
        self.next_slot += 1;
        Ok(slot)
    }

    fn nursery_page(&mut self, size: u32) -> Result<u32, RuntimeError> {
        if let Some(page) = self.nursery_pages.iter().copied().find(|id| {
            !self.pages[*id as usize].leased
                && size <= self.pages[*id as usize].len() as u32 - self.pages[*id as usize].used
        }) {
            return Ok(page);
        }
        self.new_nursery_pair()
    }

    fn empty_nursery_page(&mut self) -> Result<u32, RuntimeError> {
        if let Some(page) = self
            .nursery_pages
            .iter()
            .copied()
            .find(|id| !self.pages[*id as usize].leased && self.pages[*id as usize].used == 0)
        {
            return Ok(page);
        }
        self.new_nursery_pair()
    }

    fn new_nursery_pair(&mut self) -> Result<u32, RuntimeError> {
        if self.nursery_capacity + u64::from(PAGE_BYTES) > self.nursery_limit {
            return Err(collection_error());
        }
        self.reserve_page_metadata(2, 0, None)?;
        self.admit_resident(u64::from(PAGE_BYTES) * 2)?;
        let from = self.new_page(PAGE_BYTES, YOUNG, 0, true)?;
        let to = self.new_page(PAGE_BYTES, SPARE, 0, true)?;
        self.pages[from as usize].partner = to;
        self.pages[to as usize].partner = from;
        self.nursery_pages.push(from);
        self.nursery_capacity += u64::from(PAGE_BYTES);
        Ok(from)
    }

    fn new_page(
        &mut self,
        size: u32,
        kind: u8,
        class: u32,
        prepaid: bool,
    ) -> Result<u32, RuntimeError> {
        let entries = if kind == OLD {
            (size / class) as usize
        } else {
            usize::from(kind == LARGE)
        };
        let cards = if kind == OLD || kind == LARGE {
            size.div_ceil(CARD_BYTES) as usize
        } else {
            0
        };
        let bytes = u64::from(size) + (entries * 8 + cards) as u64;
        if !prepaid {
            self.reserve_page_metadata(1, cards, (kind == OLD).then_some(class))?;
            self.admit_resident(bytes)?;
        }
        if !self.free_pages.is_empty() {
            let id = self.free_pages.pop().unwrap();
            self.pages[id as usize] = Page::new(size, kind, class);
            if kind == OLD {
                self.old_available[(class.trailing_zeros() - 3) as usize].push(id);
            }
            return Ok(id);
        }
        let id = self.pages.len() as u32;
        self.pages.push(Page::new(size, kind, class));
        if kind == OLD {
            self.old_available[(class.trailing_zeros() - 3) as usize].push(id);
        }
        Ok(id)
    }

    /// Admit page-directory, free-list and remembered-card capacity before
    /// publishing pages. Collection can then mark cards and return pages
    /// without growing a host vector.
    fn reserve_page_metadata(
        &mut self,
        additional_pages: usize,
        additional_cards: usize,
        old_class: Option<u32>,
    ) -> Result<(), RuntimeError> {
        let pages = self.pages.len() + additional_pages.saturating_sub(self.free_pages.len());
        let pages = self.pages.capacity().max(
            pages
                .checked_next_power_of_two()
                .ok_or_else(storage_error_limit)?,
        );
        let cards = self
            .pages
            .iter()
            .map(|page| page.cards.read().unwrap().len())
            .sum::<usize>()
            + additional_cards;
        let cards = self.pending_cards.capacity().max(
            cards
                .checked_next_power_of_two()
                .ok_or_else(storage_error_limit)?,
        );
        let shared_cards = self
            .shared_pending_cards
            .lock()
            .unwrap()
            .capacity()
            .max(cards);
        let class = old_class.map(|class| (class.trailing_zeros() - 3) as usize);
        let available = old_class.map_or(0, |class| {
            self.pages
                .iter()
                .filter(|page| page.kind == OLD && page.class == class)
                .count()
                + 1
        });
        let resident = |heap: &Self| {
            (heap.pages.capacity() * size_of::<Page>()
                + heap.free_pages.capacity() * size_of::<u32>()
                + heap.nursery_pages.capacity() * size_of::<u32>()
                + heap.pending_cards.capacity() * size_of::<Card>()
                + heap.shared_pending_cards.lock().unwrap().capacity() * size_of::<Card>()
                + heap
                    .old_available
                    .iter()
                    .map(|pages| pages.capacity() * size_of::<u32>())
                    .sum::<usize>()) as u64
        };
        let before = resident(self);
        let additional = ((pages - self.pages.capacity()) * size_of::<Page>()
            + pages.saturating_sub(self.free_pages.capacity()) * size_of::<u32>()
            + pages.saturating_sub(self.nursery_pages.capacity()) * size_of::<u32>()
            + (cards - self.pending_cards.capacity()) * size_of::<Card>()
            + (shared_cards - self.shared_pending_cards.lock().unwrap().capacity())
                * size_of::<Card>()
            + class.map_or(0, |index| {
                available.saturating_sub(self.old_available[index].capacity()) * size_of::<u32>()
            })) as u64;
        self.admit_resident(additional)?;
        let reserve = (|| {
            self.pages
                .try_reserve_exact(pages.saturating_sub(self.pages.len()))?;
            self.free_pages
                .try_reserve_exact(pages.saturating_sub(self.free_pages.len()))?;
            self.nursery_pages
                .try_reserve_exact(pages.saturating_sub(self.nursery_pages.len()))?;
            self.pending_cards
                .try_reserve_exact(cards.saturating_sub(self.pending_cards.len()))?;
            let mut shared_cards_vec = self.shared_pending_cards.lock().unwrap();
            let additional_shared = shared_cards.saturating_sub(shared_cards_vec.len());
            shared_cards_vec.try_reserve_exact(additional_shared)?;
            if let Some(index) = class {
                let pages = &mut self.old_available[index];
                pages.try_reserve_exact(available.saturating_sub(pages.len()))?;
            }
            Ok::<_, std::collections::TryReserveError>(())
        })();
        if reserve.is_err() {
            let retained = resident(self) - before;
            self.stats.resident_page_bytes -= additional - retained;
            return Err(storage_error_limit());
        }
        Ok(())
    }

    fn prepare_promotion_space(&mut self, index: usize, required: usize) {
        let class = 8u32 << index;
        let mut free = self.old_available[index]
            .iter()
            .map(|page| self.pages[*page as usize].free.len())
            .sum::<usize>();
        while free < required {
            if self.new_page(PAGE_BYTES, OLD, class, false).is_err() {
                break; // Pre-admitted to-space can retain every survivor.
            }
            free += (PAGE_BYTES / class) as usize;
        }
    }

    fn take_old_page(&mut self, size: u32) -> Option<u32> {
        let class = 1u32 << max(3, 32 - size.saturating_sub(1).leading_zeros());
        let index = (class.trailing_zeros() as usize).saturating_sub(3);
        let pages = &mut self.old_available[index];
        while let Some(id) = pages.last().copied() {
            if self.pages[id as usize].free.is_empty() {
                pages.pop();
            } else {
                return Some(id);
            }
        }
        None
    }

    fn dirty_region(&mut self, slot: u32, offset: u32, size: u32) -> Result<(), RuntimeError> {
        if size == 0 || self.slots[slot as usize].young {
            return Ok(());
        }
        let entry = &self.slots[slot as usize];
        self.mark_card(entry.page, entry.offset + offset, size)
    }

    fn mark_card(&mut self, page: u32, offset: u32, size: u32) -> Result<(), RuntimeError> {
        if size == 0 {
            return Ok(());
        }
        let first = offset / CARD_BYTES;
        let last = (offset + size - 1) / CARD_BYTES;
        for card in first..=last {
            if self.pages[page as usize].cards.read().unwrap()[card as usize] == 0 {
                self.pages[page as usize].cards.write().unwrap()[card as usize] = 1;
                self.pending_cards.push(Card { page, index: card });
            }
        }
        Ok(())
    }

    fn validate(&self, handle: StorageRef) -> Result<u32, RuntimeError> {
        if !handle.valid() {
            return Err(stale_error());
        }
        self.validate_state(handle, PUBLISHED)
    }

    fn validate_state(&self, handle: StorageRef, state: u8) -> Result<u32, RuntimeError> {
        let entry = self
            .slots
            .get(handle.slot as usize)
            .ok_or_else(stale_error)?;
        if entry.state != state || entry.generation != handle.generation {
            return Err(RuntimeError::new(
                "stale_reference",
                "managed_storage",
                format!(
                    "handle {}:{} has state {} generation {}; expected state {}",
                    handle.slot, handle.generation, entry.state, entry.generation, state
                ),
            ));
        }
        Ok(handle.slot)
    }

    fn check_range(&self, slot: u32, offset: u32, length: u32) -> Result<(), RuntimeError> {
        if u64::from(offset) + u64::from(length) > u64::from(self.slots[slot as usize].size) {
            return Err(storage_error("access outside payload"));
        }
        Ok(())
    }

    fn read_reference_at(&self, page: u32, offset: u32) -> Result<StorageRef, RuntimeError> {
        let data = self.pages[page as usize].read_range(offset as usize, 8);
        Ok(StorageRef {
            slot: u32::from_le_bytes(data[0..4].try_into().unwrap()),
            generation: u32::from_le_bytes(data[4..8].try_into().unwrap()),
        })
    }

    fn read_u32(&self, page: u32, offset: u32) -> u32 {
        u32::from_le_bytes(
            self.pages[page as usize]
                .read_range(offset as usize, 4)
                .try_into()
                .unwrap(),
        )
    }

    fn admit_resident(&mut self, bytes: u64) -> Result<(), RuntimeError> {
        let total = self
            .stats
            .resident_page_bytes
            .checked_add(bytes)
            .ok_or_else(storage_error_limit)?;
        if self.max_resident_bytes.is_some_and(|limit| total > limit) {
            return Err(storage_error_limit());
        }
        self.stats.resident_page_bytes = total;
        Ok(())
    }
}

impl StorageWorkerPool {
    pub(crate) fn reserve_resident(
        &self,
        bytes: u64,
    ) -> Result<StorageResidentLease, RuntimeError> {
        let mut heap = self.lock()?;
        if heap.state == StorageState::Closed {
            return Err(storage_error("managed storage is closed"));
        }
        heap.admit_resident(bytes)?;
        Ok(StorageResidentLease {
            heap: self.heap.clone(),
            bytes,
        })
    }

    pub(crate) fn active_workers(&self) -> Result<usize, RuntimeError> {
        Ok(self.read_lock()?.active_workers)
    }

    pub(crate) fn close(&self) -> Result<(), RuntimeError> {
        let mut heap = self.lock()?;
        if heap.active_workers != 0 || heap.unpublished != 0 {
            return Err(storage_error("collection requires committed allocations"));
        }
        heap.state = StorageState::Closed;
        // Closed heaps are terminal. Drop the backing allocations themselves
        // rather than clearing lengths and retaining their capacities as
        // resident memory after the owner has gone away.
        heap.pages = Vec::new();
        heap.free_pages = Vec::new();
        heap.occupied = Vec::new();
        heap.young_slots = Vec::new();
        heap.young_classes = [0; 12];
        heap.nursery_pages = Vec::new();
        heap.old_available = std::array::from_fn(|_| Vec::new());
        heap.pending_cards = Vec::new();
        if let Ok(mut cards) = heap.shared_pending_cards.lock() {
            cards.clear();
        }
        heap.queue = Vec::new();
        heap.slots = Vec::new();
        heap.free_slots = Vec::new();
        heap.layouts = Vec::new();
        heap.layout_owners = Vec::new();
        heap.nursery_capacity = 0;
        heap.stats.occupied_bytes = 0;
        heap.stats.reserved_bytes = 0;
        heap.stats.young_bytes = 0;
        heap.stats.old_bytes = 0;
        heap.stats.resident_page_bytes = 0;
        Ok(())
    }

    /// Commit one object directly in the shared directory.  Large prepared
    /// payloads cannot fit a nursery TLAB; they use StorageHeap's existing
    /// LARGE page path while retaining the same layout, metadata and quota
    /// accounting as worker-local allocations.
    pub(crate) fn reserve_direct(
        &self,
        layout: u32,
        count: u32,
        logical: u64,
        bytes: &[u8],
        metadata: StorageObjectMeta,
    ) -> Result<StorageRef, RuntimeError> {
        let mut heap = self.lock()?;
        let mut reservation = heap.reserve(layout, count, logical)?;
        {
            let mut payload = reservation.bytes()?;
            if bytes.len() > payload.len() {
                return Err(storage_error("prepared payload exceeds allocation"));
            }
            payload.fill(0);
            payload[..bytes.len()].copy_from_slice(bytes);
        }
        let handle = reservation.commit();
        *heap.slots[handle.slot as usize].metadata.write().unwrap() = Some(metadata);
        Ok(handle)
    }

    pub(crate) fn set_resident_limit(&self, limit: Option<u64>) -> Result<(), RuntimeError> {
        let mut heap = self.lock()?;
        heap.max_resident_bytes = limit;
        Ok(())
    }

    pub(crate) fn reserve_control(&self, bytes: u64) -> Result<StorageControlLease, RuntimeError> {
        let mut heap = self.lock()?;
        if heap.state == StorageState::Closed {
            return Err(storage_error("managed storage is closed"));
        }
        let used = heap
            .stats
            .occupied_bytes
            .saturating_add(heap.stats.reserved_bytes);
        if bytes > MAX_BYTES.saturating_sub(used)
            || heap
                .max_guest_bytes
                .is_some_and(|limit| bytes > limit.saturating_sub(used))
        {
            return Err(collection_error());
        }
        heap.stats.reserved_bytes += bytes;
        Ok(StorageControlLease {
            heap: self.heap.clone(),
            bytes,
            committed: false,
        })
    }

    pub fn lock(&self) -> Result<std::sync::RwLockWriteGuard<'_, StorageHeap>, RuntimeError> {
        self.heap
            .write()
            .map_err(|_| storage_error("managed storage lock poisoned"))
    }

    pub(crate) fn read_lock(
        &self,
    ) -> Result<std::sync::RwLockReadGuard<'_, StorageHeap>, RuntimeError> {
        self.heap
            .read()
            .map_err(|_| storage_error("managed storage lock poisoned"))
    }

    pub fn register_layout(&self, layout: StorageLayout) -> Result<u32, RuntimeError> {
        let mut heap = self.lock()?;
        heap.register_layout(layout)
    }

    pub(crate) fn pin_layout(&self, layout: u32) -> Result<StorageLayoutLease, RuntimeError> {
        let mut heap = self.lock()?;
        heap.layout(layout)?;
        heap.admit_resident((size_of::<StorageLayoutPin>() + 2 * size_of::<usize>()) as u64)?;
        heap.layout_owners[layout as usize].pins += 1;
        Ok(StorageLayoutLease(Arc::new(StorageLayoutPin {
            heap: self.heap.clone(),
            layout,
        })))
    }

    /// Read a prepared layout without exposing the heap's mutable directory.
    /// Channel and collection owners use this to calculate element offsets
    /// after registering a ring layout.
    pub fn layout(&self, index: u32) -> Result<StorageLayout, RuntimeError> {
        let heap = self.read_lock()?;
        Ok(heap.layout(index)?.clone())
    }

    /// Returns the committed byte width of a storage ticket. Typed container
    /// decoders use this for indirect dynamic lanes whose payload is owned by
    /// the same managed heap.
    pub fn size(&self, handle: StorageRef) -> Result<u32, RuntimeError> {
        let heap = self.read_lock()?;
        let size = heap
            .slots
            .get(handle.slot as usize)
            .filter(|slot| slot.generation == handle.generation && slot.state == PUBLISHED)
            .map(|slot| slot.size);
        size.ok_or_else(|| storage_error("stale storage reference"))
    }

    pub(crate) fn live_objects(&self) -> Result<usize, RuntimeError> {
        Ok(self.read_lock()?.occupied.len())
    }

    /// Rewrite a stable storage ticket while preserving its slot/generation.
    /// Owners use this when a guest handle changes representation in place,
    /// such as attaching a managed map header to an already allocated map
    /// anchor.  The caller must have completed all admission checks before
    /// invoking the rewrite; the underlying heap keeps the ticket atomic.
    pub fn rewrite(
        &self,
        handle: StorageRef,
        layout: u32,
        count: u32,
        logical: u64,
        bytes: &[u8],
    ) -> Result<(), RuntimeError> {
        self.lock()?.rewrite(handle, layout, count, logical, bytes)
    }

    pub(crate) fn rewrite_with_metadata(
        &self,
        handle: StorageRef,
        layout: u32,
        count: u32,
        logical: u64,
        bytes: &[u8],
        metadata: StorageObjectMeta,
    ) -> Result<(), RuntimeError> {
        {
            let heap = self.read_lock()?;
            if let Some(result) = heap.replace_shared(
                handle,
                layout,
                count,
                logical,
                bytes,
                None,
                metadata.clone(),
            )? {
                if result {
                    return Ok(());
                }
                return Err(stale_error());
            }
        }
        let mut heap = self.lock()?;
        heap.rewrite(handle, layout, count, logical, bytes)?;
        let slot = heap
            .slots
            .get_mut(handle.slot as usize)
            .filter(|slot| slot.generation == handle.generation && slot.state == PUBLISHED)
            .ok_or_else(stale_error)?;
        *slot.metadata.write().unwrap() = Some(metadata);
        Ok(())
    }

    /// Commit a batch of fixed-size prepared destinations as one owner
    /// transaction. Validation is completed while holding the write guard;
    /// the subsequent writes only touch existing pages and metadata, so the
    /// commit phase has no fallible allocation, relocation or directory
    /// growth path.
    pub(crate) fn rewrite_fixed_batch(
        &self,
        writes: &[StorageRewrite],
    ) -> Result<(), RuntimeError> {
        let mut heap = self.lock()?;
        for write in writes {
            let slot = heap.validate(write.handle)? as usize;
            let entry = &heap.slots[slot];
            let stride = heap.layout(write.layout)?.stride;
            if stride == 0
                || u64::from(stride) > u64::from(entry.size)
                || write.bytes.len() > entry.size as usize
                || write.metadata.logical_bytes != entry.logical
                || u64::from(write.metadata.payload_len) > write.bytes.len() as u64
            {
                return Err(RuntimeError::new(
                    "invalid_result",
                    "managed_storage",
                    "fixed rewrite does not fit its destination",
                ));
            }
        }
        for write in writes {
            let slot = heap.validate(write.handle)? as usize;
            let entry = heap.slots[slot].clone();
            heap.pages[entry.page as usize].fill_range(
                entry.offset as usize,
                entry.size as usize,
                0,
            );
            heap.write(write.handle, 0, &write.bytes)?;
            heap.slots[slot].layout = write.layout;
            heap.slots[slot].count = 1;
            *heap.slots[slot].metadata.write().unwrap() = Some(write.metadata.clone());
        }
        Ok(())
    }

    /// Conditionally rewrite one directory entry.  The version check and the
    /// byte/metadata replacement share the storage lock, so an optimistic
    /// prepared update cannot overwrite a concurrent writer after it has
    /// decoded its old value.
    #[expect(
        clippy::too_many_arguments,
        reason = "one storage transaction owns the payload, accounting, metadata and version check"
    )]
    pub(crate) fn rewrite_with_metadata_if_version(
        &self,
        handle: StorageRef,
        layout: u32,
        count: u32,
        logical: u64,
        bytes: &[u8],
        expected_version: u64,
        metadata: StorageObjectMeta,
    ) -> Result<bool, RuntimeError> {
        {
            let heap = self.read_lock()?;
            if let Some(result) = heap.replace_shared(
                handle,
                layout,
                count,
                logical,
                bytes,
                Some(expected_version),
                metadata.clone(),
            )? {
                return Ok(result);
            }
        }
        let mut heap = self.lock()?;
        let slot_index = heap.validate(handle)? as usize;
        if heap.slots[slot_index]
            .metadata
            .read()
            .unwrap()
            .as_ref()
            .is_none_or(|current| current.version != expected_version)
        {
            return Ok(false);
        }
        heap.rewrite(handle, layout, count, logical, bytes)?;
        let slot = heap
            .slots
            .get_mut(slot_index)
            .filter(|slot| slot.generation == handle.generation && slot.state == PUBLISHED)
            .ok_or_else(stale_error)?;
        *slot.metadata.write().unwrap() = Some(metadata);
        Ok(true)
    }

    /// Versioned in-place update for a published object. Same-shape writes
    /// retain the directory entry and use only the heap read guard plus the
    /// page/metadata locks; relocation is intentionally kept on the owner
    /// write path below.
    pub(crate) fn write_range_if_version_shared(
        &self,
        handle: StorageRef,
        offset: u32,
        bytes: &[u8],
        expected_version: u64,
    ) -> Result<bool, RuntimeError> {
        let heap = self.read_lock()?;
        let slot = heap.validate(handle)? as usize;
        let entry = &heap.slots[slot];
        let mut metadata = entry.metadata.write().unwrap();
        let Some(metadata_value) = metadata.as_mut() else {
            return Err(stale_error());
        };
        if metadata_value.version != expected_version {
            return Ok(false);
        }
        heap.write_shared(handle, offset, bytes)?;
        metadata_value.initialized = true;
        metadata_value.version = expected_version.wrapping_add(1).max(1);
        Ok(true)
    }

    /// Copy a prepared byte range between published objects while holding the
    /// directory read guard. Same-page ranges use `copy_within` so overlapping
    /// slices have memmove semantics; different pages copy directly between
    /// ordered page locks without constructing a temporary payload. The
    /// destination version and remembered-card barrier remain one transaction.
    pub(crate) fn copy_range_if_version_shared(
        &self,
        source: StorageRef,
        source_offset: u32,
        destination: StorageRef,
        destination_offset: u32,
        length: u32,
        expected_version: u64,
    ) -> Result<bool, RuntimeError> {
        let heap = self.read_lock()?;
        let source_index = heap.validate(source)? as usize;
        let destination_index = heap.validate(destination)? as usize;
        heap.check_range(source_index as u32, source_offset, length)?;
        heap.check_range(destination_index as u32, destination_offset, length)?;
        let source_entry = &heap.slots[source_index];
        let destination_entry = &heap.slots[destination_index];
        let mut metadata = destination_entry.metadata.write().unwrap();
        let Some(metadata_value) = metadata.as_mut() else {
            return Err(stale_error());
        };
        if metadata_value.version != expected_version {
            return Ok(false);
        }
        let source_page = source_entry.page;
        let destination_page = destination_entry.page;
        let source_start = source_entry.offset + source_offset;
        let destination_start = destination_entry.offset + destination_offset;
        if source_page == destination_page {
            let mut page = heap.pages[source_page as usize].bytes.write().unwrap();
            page.copy_within(
                source_start as usize..(source_start + length) as usize,
                destination_start as usize,
            );
        } else if source_page < destination_page {
            let source_bytes = heap.pages[source_page as usize].bytes.read().unwrap();
            let mut destination_bytes =
                heap.pages[destination_page as usize].bytes.write().unwrap();
            destination_bytes[destination_start as usize..(destination_start + length) as usize]
                .copy_from_slice(
                    &source_bytes[source_start as usize..(source_start + length) as usize],
                );
        } else {
            let mut destination_bytes =
                heap.pages[destination_page as usize].bytes.write().unwrap();
            let source_bytes = heap.pages[source_page as usize].bytes.read().unwrap();
            destination_bytes[destination_start as usize..(destination_start + length) as usize]
                .copy_from_slice(
                    &source_bytes[source_start as usize..(source_start + length) as usize],
                );
        }
        heap.mark_card_shared(
            destination_entry.page,
            destination_entry.offset + destination_offset,
            length,
            destination_entry.young,
        );
        metadata_value.initialized = true;
        metadata_value.version = expected_version.wrapping_add(1).max(1);
        Ok(true)
    }

    /// Update a typed subrange without reconstructing the containing object.
    /// Version validation, the write barrier and metadata publication happen
    /// under the same storage lock.
    pub(crate) fn write_range_if_version(
        &self,
        handle: StorageRef,
        offset: u32,
        bytes: &[u8],
        expected_version: u64,
    ) -> Result<bool, RuntimeError> {
        self.write_range_if_version_shared(handle, offset, bytes, expected_version)
    }

    /// Acquire a worker lease. The lease owns one nursery page
    /// until it is flushed, so no worker can publish into another worker's
    /// partially initialized page.
    pub fn acquire(&self, credit: u64) -> Result<StorageWorkerLease, RuntimeError> {
        self.acquire_inner(credit)
    }

    fn acquire_inner(&self, credit: u64) -> Result<StorageWorkerLease, RuntimeError> {
        if credit == 0 {
            return Err(storage_error("invalid allocation batch"));
        }
        let mut heap = self.lock()?;
        if heap.state == StorageState::Closed {
            return Err(storage_error("managed storage is closed"));
        }
        let used = heap
            .stats
            .occupied_bytes
            .saturating_add(heap.stats.reserved_bytes);
        let available = MAX_BYTES.saturating_sub(used).min(
            heap.max_guest_bytes
                .map_or(u64::MAX, |limit| limit.saturating_sub(used)),
        );
        let credit = credit.min(available);
        if credit == 0 {
            return Err(collection_error());
        }
        // A lease takes ownership of the page byte buffer. It therefore only
        // uses an empty nursery page; existing objects remain readable through
        // the shared storage reader while this page is leased.
        let page = heap.empty_nursery_page()?;
        heap.pages[page as usize].leased = true;
        let initial_used = heap.pages[page as usize].used;
        let page_bytes = {
            let mut bytes = heap.pages[page as usize].bytes.write().unwrap();
            std::mem::take(&mut *bytes)
        };
        const TLAB_SLOTS: usize = 128;
        let slot_capacity = TLAB_SLOTS;
        let resident = (slot_capacity as u64).saturating_mul(
            (std::mem::size_of::<WorkerSlot>() + std::mem::size_of::<WorkerObject>()) as u64,
        );
        if heap.admit_resident(resident).is_err() {
            *heap.pages[page as usize].bytes.write().unwrap() = page_bytes;
            heap.pages[page as usize].leased = false;
            return Err(storage_error_limit());
        }
        let mut slot_ids: Vec<WorkerSlot> = Vec::with_capacity(slot_capacity);
        for _ in 0..slot_capacity {
            match heap.take_slot() {
                Ok(slot) => {
                    let generation = match heap.slots[slot as usize].generation.checked_add(1) {
                        Some(generation) => generation,
                        None => {
                            heap.free_slots
                                .extend(slot_ids.drain(..).map(|ticket| ticket.slot));
                            heap.stats.resident_page_bytes =
                                heap.stats.resident_page_bytes.saturating_sub(resident);
                            heap.free_slots.push(slot);
                            *heap.pages[page as usize].bytes.write().unwrap() = page_bytes;
                            heap.pages[page as usize].leased = false;
                            return Err(storage_error("generation exhausted"));
                        }
                    };
                    slot_ids.push(WorkerSlot { slot, generation });
                }
                Err(error) => {
                    heap.free_slots
                        .extend(slot_ids.drain(..).map(|ticket| ticket.slot));
                    heap.stats.resident_page_bytes =
                        heap.stats.resident_page_bytes.saturating_sub(resident);
                    *heap.pages[page as usize].bytes.write().unwrap() = page_bytes;
                    heap.pages[page as usize].leased = false;
                    return Err(error);
                }
            }
        }
        heap.active_workers += 1;
        // Reserve the complete TLAB credit once.  Individual bump-pointer
        // allocations stay worker-local and no longer contend on the global
        // quota ledger for every object.
        heap.stats.reserved_bytes += credit;
        Ok(StorageWorkerLease {
            pool: self.clone(),
            page,
            page_bytes,
            initial_used,
            slot_ids,
            credit,
            reserved: 0,
            committed: 0,
            open: 0,
            used: 0,
            resident,
            objects: Vec::with_capacity(slot_capacity),
            finished: false,
        })
    }

    pub fn stats(&self) -> Result<StorageStats, RuntimeError> {
        let heap = self.read_lock()?;
        Ok(heap.stats())
    }

    pub(crate) fn nursery_pressure(&self) -> Result<StorageNurseryPressure, RuntimeError> {
        Ok(self.read_lock()?.nursery_pressure())
    }

    pub(crate) fn live_dynamic_type_ids(&self) -> Result<HashSet<u32>, RuntimeError> {
        Ok(self.read_lock()?.live_dynamic_type_ids())
    }

    pub(crate) fn live_payload_types(
        &self,
    ) -> Result<Vec<crate::types::TypeIdentity>, RuntimeError> {
        Ok(self.read_lock()?.live_payload_types())
    }

    pub(crate) fn metadata(&self, handle: StorageRef) -> Result<StorageObjectMeta, RuntimeError> {
        let heap = self.read_lock()?;
        let slot = heap
            .slots
            .get(handle.slot as usize)
            .filter(|slot| slot.generation == handle.generation && slot.state == PUBLISHED)
            .ok_or_else(stale_error)?;
        slot.metadata
            .read()
            .unwrap()
            .clone()
            .ok_or_else(stale_error)
    }

    /// Read metadata and the initialized payload as one published snapshot.
    /// The returned bytes are owned; no page slice escapes the protection
    /// region.  Heap decoders use this when metadata determines how to parse
    /// the payload.
    pub(crate) fn read_snapshot(
        &self,
        handle: StorageRef,
    ) -> Result<StorageSnapshot, RuntimeError> {
        let heap = self.read_lock()?;
        let slot_index = heap.validate(handle)? as usize;
        let slot = &heap.slots[slot_index];
        let metadata_guard = slot.metadata.read().unwrap();
        let metadata = metadata_guard.clone().ok_or_else(stale_error)?;
        let end = slot
            .offset
            .checked_add(metadata.payload_len)
            .ok_or_else(|| storage_error("payload range overflows"))?;
        if metadata.payload_len > slot.size || end as usize > heap.pages[slot.page as usize].len() {
            return Err(storage_error("payload metadata exceeds storage"));
        }
        let payload_len = metadata.payload_len as usize;
        let snapshot = StorageSnapshot {
            metadata,
            bytes: heap.pages[slot.page as usize].read_range(slot.offset as usize, payload_len),
        };
        drop(metadata_guard);
        Ok(snapshot)
    }

    /// Read one requested payload range while holding the metadata read lock
    /// through the page copy. This avoids copying a full backing for callers
    /// that only need a byte or a fixed descriptor.
    pub(crate) fn read_snapshot_range(
        &self,
        handle: StorageRef,
        offset: u32,
        length: u32,
    ) -> Result<StorageSnapshot, RuntimeError> {
        let heap = self.read_lock()?;
        heap.read_snapshot_range(handle, offset, Some(length))
    }

    pub fn collect_minor<I: IntoIterator<Item = StorageRef>>(
        &self,
        roots: I,
    ) -> Result<usize, RuntimeError> {
        let mut heap = self.lock()?;
        let released = heap.collect_minor(roots)?;
        Ok(released)
    }

    pub fn collect_major<I: IntoIterator<Item = StorageRef>>(
        &self,
        roots: I,
    ) -> Result<usize, RuntimeError> {
        let mut heap = self.lock()?;
        let released = heap.collect_major(roots)?;
        Ok(released)
    }
}

impl StorageWorkerLease {
    pub(crate) fn read(
        &self,
        handle: StorageRef,
        offset: u32,
        length: u32,
    ) -> Result<Vec<u8>, RuntimeError> {
        let object = self
            .objects
            .iter()
            .find(|object| object.handle == handle && object.committed)
            .ok_or_else(stale_error)?;
        if u64::from(offset) + u64::from(length) > u64::from(object.size) {
            return Err(storage_error("access outside payload"));
        }
        Ok(self.page_bytes
            [(object.offset + offset) as usize..(object.offset + offset + length) as usize]
            .to_vec())
    }

    /// Read into a caller-owned buffer and report whether this lease owns the
    /// handle. A single object lookup serves both the ownership check and the
    /// copy, avoiding `contains` followed by a second linear search.
    pub(crate) fn read_into_if_present(
        &self,
        handle: StorageRef,
        offset: u32,
        destination: &mut [u8],
    ) -> Option<Result<(), RuntimeError>> {
        self.objects
            .iter()
            .find(|object| object.handle == handle && object.committed)
            .map(|object| {
                let length = u32::try_from(destination.len())
                    .map_err(|_| storage_error("read size overflow"))?;
                if u64::from(offset) + u64::from(length) > u64::from(object.size) {
                    return Err(storage_error("access outside payload"));
                }
                let start = (object.offset + offset) as usize;
                destination.copy_from_slice(&self.page_bytes[start..start + destination.len()]);
                Ok(())
            })
    }

    pub(crate) fn read_snapshot_into_if<F>(
        &self,
        handle: StorageRef,
        offset: u32,
        destination: &mut [u8],
        mut validate: F,
    ) -> Option<Result<bool, RuntimeError>>
    where
        F: FnMut(&StorageObjectMeta) -> Result<bool, RuntimeError>,
    {
        self.objects
            .iter()
            .find(|object| object.handle == handle && object.committed)
            .map(|object| {
                let metadata = object.metadata.clone().ok_or_else(stale_error)?;
                if !validate(&metadata)? {
                    return Ok(false);
                }
                let length = u32::try_from(destination.len())
                    .map_err(|_| storage_error("read size overflow"))?;
                if offset > metadata.payload_len
                    || length > metadata.payload_len.saturating_sub(offset)
                {
                    return Err(storage_error("snapshot range exceeds payload"));
                }
                let start = (object.offset + offset) as usize;
                destination.copy_from_slice(&self.page_bytes[start..start + destination.len()]);
                Ok(true)
            })
    }

    pub fn contains(&self, handle: StorageRef) -> bool {
        self.objects
            .iter()
            .any(|object| object.handle == handle && object.committed)
    }

    pub fn size(&self, handle: StorageRef) -> Option<u32> {
        self.objects
            .iter()
            .find(|object| object.handle == handle && object.committed)
            .map(|object| object.size)
    }

    pub(crate) fn committed_objects(&self) -> usize {
        self.objects
            .iter()
            .filter(|object| object.committed)
            .count()
    }

    pub(crate) fn committed_bytes(&self) -> u64 {
        self.objects
            .iter()
            .filter(|object| object.committed)
            .map(|object| object.logical)
            .sum()
    }

    pub(crate) fn unused_credit(&self) -> u64 {
        // `reserved` includes both open and privately committed objects.
        // Commit changes publication state, not the space taken from TLAB.
        self.credit.saturating_sub(self.reserved)
    }

    pub(crate) fn transfer_unused_credit(&mut self, bytes: u64) -> bool {
        if self.unused_credit() < bytes {
            return false;
        }
        self.credit = self.credit.saturating_sub(bytes);
        true
    }

    pub(crate) fn objects(&self) -> impl Iterator<Item = (StorageRef, bool)> + '_ {
        self.objects
            .iter()
            .map(|object| (object.handle, object.committed))
    }

    /// Roll back objects allocated after a prepared-value transaction began.
    /// Their slots remain in this lease's private ticket batch and are
    /// returned on flush/drop; logical reservation and committed accounting
    /// are removed immediately so a failed parent allocation cannot leak
    /// backing ownership into the guest ledger.
    pub(crate) fn rollback_since(&mut self, existing: &[StorageRef]) {
        // Prepared dynamic values may allocate one or more backing boxes
        // before their parent record is admitted.  Those allocations are
        // always appended to this lease, so roll back the suffix as one
        // transaction, including its physical bump-pointer bytes and slot
        // tickets.  Merely clearing logical metadata leaves the page cursor
        // consumed and makes a retry fail with a false collection_required.
        let boundary = self
            .objects
            .iter()
            .rposition(|object| existing.contains(&object.handle))
            .map_or(0, |index| index + 1);
        let removed = self.objects.split_off(boundary);
        for object in &removed {
            if object.committed {
                self.committed = self.committed.saturating_sub(object.logical);
            } else {
                self.open = self.open.saturating_sub(1);
            }
            self.reserved = self.reserved.saturating_sub(object.logical);
            self.used = self.used.saturating_sub(object.size);
            self.slot_ids.push(WorkerSlot {
                slot: object.handle.slot,
                generation: object.handle.generation,
            });
        }
    }

    pub(crate) fn metadata(&self, handle: StorageRef) -> Option<StorageObjectMeta> {
        self.objects
            .iter()
            .find(|object| object.handle == handle && object.committed)
            .and_then(|object| object.metadata.clone())
    }

    #[cfg(test)]
    pub fn reserve(
        &mut self,
        layout: u32,
        count: u32,
        logical: u64,
    ) -> Result<StorageRef, RuntimeError> {
        let stride = self.pool.layout(layout)?.stride;
        self.reserve_prepared(layout, stride, count, logical)
    }

    pub(crate) fn reserve_prepared(
        &mut self,
        layout: u32,
        stride: u32,
        count: u32,
        logical: u64,
    ) -> Result<StorageRef, RuntimeError> {
        if self.finished || layout == 0 {
            return Err(storage_error("invalid allocation batch"));
        }
        if logical > self.credit.saturating_sub(self.reserved) {
            return Err(collection_error());
        }
        let physical = u64::from(stride)
            .checked_mul(u64::from(count))
            .ok_or_else(|| storage_error("allocation size overflow"))?;
        let size = max(8, physical.saturating_add(7) & !7);
        if size > u64::from(u32::MAX) {
            return Err(storage_error("allocation size overflow"));
        }
        let size = size as u32;
        if size > PAGE_BYTES / 4
            || size > self.page_bytes.len() as u32 - self.initial_used - self.used
        {
            return Err(collection_error());
        }
        let ticket = self.slot_ids.pop().ok_or_else(collection_error)?;
        let slot = ticket.slot;
        let generation = ticket.generation;
        let offset = self.initial_used + self.used;
        self.page_bytes[offset as usize..(offset + size) as usize].fill(0);
        self.used += size;
        self.reserved += logical;
        self.open += 1;
        let handle = StorageRef { slot, generation };
        self.objects.push(WorkerObject {
            handle,
            layout,
            count,
            offset,
            size,
            logical,
            committed: false,
            metadata: None,
        });
        Ok(handle)
    }

    pub(crate) fn set_metadata(
        &mut self,
        handle: StorageRef,
        metadata: StorageObjectMeta,
    ) -> Result<(), RuntimeError> {
        let object = self
            .objects
            .iter_mut()
            .find(|object| object.handle == handle && !object.committed)
            .ok_or_else(stale_error)?;
        object.metadata = Some(metadata);
        Ok(())
    }

    pub fn write(
        &mut self,
        handle: StorageRef,
        offset: u32,
        bytes: &[u8],
    ) -> Result<(), RuntimeError> {
        // The page remains exclusively owned by this lease until flush.
        // Shared readers cannot resolve its tickets yet, so &mut self is the
        // complete write exclusion and no shared stripe is needed here.
        if self.finished {
            return Err(storage_error("allocation batch is finished"));
        }
        let (object_offset, object_size) = self
            .objects
            .iter()
            .find(|object| object.handle == handle)
            .map(|object| (object.offset, object.size))
            .ok_or_else(stale_error)?;
        if u64::from(offset) + bytes.len() as u64 > u64::from(object_size) {
            return Err(storage_error("access outside payload"));
        }
        self.page_bytes[(object_offset + offset) as usize
            ..(object_offset + offset + bytes.len() as u32) as usize]
            .copy_from_slice(bytes);
        Ok(())
    }

    /// Atomically update a prepared sub-lane owned by this lease.  Leased
    /// objects are still private to the worker, so the lease metadata is the
    /// version authority until the page is published.  Keeping this path on
    /// the lease avoids flushing a nursery page merely to perform a scalar
    /// field write.
    pub(crate) fn write_range_if_version_if_present(
        &mut self,
        handle: StorageRef,
        offset: u32,
        bytes: &[u8],
        expected_version: u64,
    ) -> Option<Result<bool, RuntimeError>> {
        self.objects
            .iter()
            .position(|object| object.handle == handle && object.committed)
            .map(|index| {
                if self.finished {
                    return Err(storage_error("allocation batch is finished"));
                }
                let (object_offset, object_size, version) = {
                    let object = &self.objects[index];
                    let version = object.metadata.as_ref().ok_or_else(stale_error)?.version;
                    (object.offset, object.size, version)
                };
                if version != expected_version {
                    return Ok(false);
                }
                if u64::from(offset) + bytes.len() as u64 > u64::from(object_size) {
                    return Err(storage_error("access outside payload"));
                }
                self.page_bytes[(object_offset + offset) as usize
                    ..(object_offset + offset + bytes.len() as u32) as usize]
                    .copy_from_slice(bytes);
                let metadata = self.objects[index]
                    .metadata
                    .as_mut()
                    .ok_or_else(stale_error)?;
                metadata.initialized = true;
                metadata.version = expected_version.wrapping_add(1).max(1);
                Ok(true)
            })
    }

    pub fn commit(&mut self, handle: StorageRef) -> Result<(), RuntimeError> {
        if self.finished {
            return Err(storage_error("allocation batch is finished"));
        }
        let object = self
            .objects
            .iter_mut()
            .find(|object| object.handle == handle && !object.committed)
            .ok_or_else(stale_error)?;
        object.committed = true;
        self.committed += object.logical;
        self.open -= 1;
        Ok(())
    }

    pub fn abort(&mut self, handle: StorageRef) -> Result<(), RuntimeError> {
        if self.finished {
            return Err(storage_error("allocation batch is finished"));
        }
        let object = self
            .objects
            .iter_mut()
            .find(|object| object.handle == handle && !object.committed)
            .ok_or_else(stale_error)?;
        self.reserved = self.reserved.saturating_sub(object.logical);
        object.logical = 0;
        self.open -= 1;
        Ok(())
    }

    pub fn flush(mut self) -> Result<(), RuntimeError> {
        if self.finished {
            return Ok(());
        }
        if self.open != 0 {
            return Err(storage_error(
                "allocation batch has unfinished reservations",
            ));
        }
        let mut heap = self.pool.lock()?;
        if heap.state == StorageState::Closed || self.page as usize >= heap.pages.len() {
            self.finished = true;
            self.page_bytes.clear();
            self.slot_ids.clear();
            self.objects.clear();
            return Err(storage_error("managed storage is closed"));
        }
        // Refill is a mutator boundary, so optional promotion storage can be
        // admitted here without allocating inside the collector. Worker
        // allocations bypass StorageHeap::reserve and need the same class
        // preparation as owner allocations; otherwise their survivors can
        // occupy every nursery page indefinitely despite mostly empty pages.
        let mut classes = [0usize; 12];
        for object in self.objects.iter().filter(|object| object.committed) {
            let class = object.size.max(8).next_power_of_two();
            classes[(class.trailing_zeros() as usize) - 3] += 1;
        }
        for (index, required) in classes.into_iter().enumerate() {
            if required == 0 {
                continue;
            }
            let required = heap.young_classes[index] + required;
            heap.prepare_promotion_space(index, required);
        }
        // An early publication can contain only a few bytes. Pack it into
        // an already published nursery page while the directory is locked,
        // keeping this lease's empty page available for the next worker.
        // Handles remain stable; only their directory offsets change. Without
        // this, repeated small writes exhaust the nursery's page allowance
        // while almost all of the admitted payload space is still unused.
        let target = heap.nursery_pages.iter().copied().find(|index| {
            let page = &heap.pages[*index as usize];
            *index != self.page
                && !page.leased
                && page.used != 0
                && self.used <= page.len() as u32 - page.used
        });
        let (published_page, published_offset) = if let Some(target) = target {
            let page = &mut heap.pages[target as usize];
            let offset = page.used;
            page.write_range(
                offset as usize,
                &self.page_bytes
                    [self.initial_used as usize..(self.initial_used + self.used) as usize],
            );
            page.used += self.used;
            (target, offset)
        } else {
            heap.pages[self.page as usize].used = self.initial_used + self.used;
            (self.page, self.initial_used)
        };
        *heap.pages[self.page as usize].bytes.write().unwrap() =
            std::mem::take(&mut self.page_bytes);
        for object in &self.objects {
            let slot = object.handle.slot as usize;
            if object.committed {
                let occupied_index = heap.occupied.len();
                let young_index = heap.young_slots.len();
                heap.occupied.push(object.handle.slot);
                heap.young_slots.push(object.handle.slot);
                let class = max(3, 32 - object.size.saturating_sub(1).leading_zeros()) as usize - 3;
                heap.young_classes[class] += 1;
                let entry = &mut heap.slots[slot];
                entry.generation = object.handle.generation;
                entry.state = PUBLISHED;
                entry.layout = object.layout;
                entry.count = object.count;
                entry.page = published_page;
                entry.offset = published_offset + object.offset - self.initial_used;
                entry.size = object.size;
                entry.logical = object.logical;
                entry.marked.store(0, Ordering::Relaxed);
                entry.age = 0;
                entry.young = true;
                *entry.metadata.write().unwrap() = object.metadata.clone();
                entry.occupied_index = occupied_index;
                entry.young_index = young_index;
            } else {
                heap.free_slots.push(object.handle.slot);
            }
        }
        heap.stats.occupied_bytes += self.committed;
        heap.stats.reserved_bytes = heap.stats.reserved_bytes.saturating_sub(self.credit);
        heap.stats.young_bytes += self.committed;
        heap.stats.total_allocated_bytes += self.committed;
        heap.stats.allocations += self
            .objects
            .iter()
            .filter(|object| object.committed)
            .count() as u64;
        heap.stats.peak_bytes = heap.stats.peak_bytes.max(
            heap.stats
                .occupied_bytes
                .saturating_add(heap.stats.reserved_bytes),
        );
        heap.free_slots
            .extend(self.slot_ids.drain(..).map(|ticket| ticket.slot));
        heap.stats.resident_page_bytes =
            heap.stats.resident_page_bytes.saturating_sub(self.resident);
        heap.pages[self.page as usize].leased = false;
        heap.active_workers -= 1;
        self.finished = true;
        Ok(())
    }
}

impl Drop for StorageWorkerLease {
    fn drop(&mut self) {
        if self.finished {
            return;
        }
        let Ok(mut heap) = self.pool.heap.write() else {
            return;
        };
        if heap.state == StorageState::Closed || self.page as usize >= heap.pages.len() {
            // The owning instance may close while another executor still
            // holds a worker-local lease. Close already discarded the
            // directory and reservation counters; dropping this lease must
            // not index the released page or subtract its credit again.
            self.page_bytes.clear();
            self.slot_ids.clear();
            self.objects.clear();
            self.finished = true;
            return;
        }
        *heap.pages[self.page as usize].bytes.write().unwrap() =
            std::mem::take(&mut self.page_bytes);
        heap.pages[self.page as usize].used = self.initial_used;
        heap.free_slots
            .extend(self.slot_ids.drain(..).map(|ticket| ticket.slot));
        heap.free_slots
            .extend(self.objects.iter().map(|object| object.handle.slot));
        heap.stats.resident_page_bytes =
            heap.stats.resident_page_bytes.saturating_sub(self.resident);
        heap.stats.reserved_bytes = heap.stats.reserved_bytes.saturating_sub(self.credit);
        heap.pages[self.page as usize].leased = false;
        heap.active_workers = heap.active_workers.saturating_sub(1);
    }
}

fn copy_page_bytes(
    pages: &mut [Page],
    from: u32,
    source: u32,
    to: u32,
    target: u32,
    size: u32,
) -> Result<(), RuntimeError> {
    if from == to {
        return Err(storage_error("nursery pages must be paired"));
    }
    let (left, right) = if from < to {
        let (left, right) = pages.split_at_mut(to as usize);
        (&mut left[from as usize], &mut right[0])
    } else {
        let (left, right) = pages.split_at_mut(from as usize);
        (&mut right[0], &mut left[to as usize])
    };
    let source =
        left.bytes.read().unwrap()[source as usize..source as usize + size as usize].to_vec();
    right.bytes.write().unwrap()[target as usize..target as usize + size as usize]
        .copy_from_slice(&source);
    Ok(())
}

fn storage_error(message: &str) -> RuntimeError {
    RuntimeError::new("invalid_operation", "managed_storage", message)
}
fn storage_error_limit() -> RuntimeError {
    RuntimeError::new(
        "allocation_limit",
        "managed_storage",
        "resident limit exceeded",
    )
}
fn collection_error() -> RuntimeError {
    RuntimeError::new(
        "collection_required",
        "managed_storage",
        "collection or refill required",
    )
}
fn stale_error() -> RuntimeError {
    RuntimeError::new(
        "stale_reference",
        "managed_storage",
        "object handle is not live",
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn large_initial_payload_remembers_young_references_across_minor_collections() {
        let mut heap = StorageHeap::new(None, None, 4 << 20);
        let bytes = heap
            .register_layout(StorageLayout {
                stride: 8,
                ..Default::default()
            })
            .unwrap();
        let parent_layout = heap
            .register_layout(StorageLayout {
                stride: TLAB_MAX_OBJECT_BYTES + 8,
                references: vec![0, TLAB_MAX_OBJECT_BYTES],
                dynamics: Vec::new(),
            })
            .unwrap();
        let first = heap.reserve(bytes, 1, 8).unwrap().commit();
        let last = heap.reserve(bytes, 1, 8).unwrap().commit();
        let mut reservation = heap
            .reserve(parent_layout, 1, u64::from(TLAB_MAX_OBJECT_BYTES + 8))
            .unwrap();
        {
            let mut payload = reservation.bytes().unwrap();
            payload[..8].copy_from_slice(&first.identity_for_payload().to_le_bytes());
            payload[TLAB_MAX_OBJECT_BYTES as usize..]
                .copy_from_slice(&last.identity_for_payload().to_le_bytes());
        }
        let parent = reservation.commit();
        assert!(heap.get(parent).unwrap().3);
        for _ in 0..2 {
            heap.collect_minor([parent]).unwrap();
            assert!(heap.get(first).is_ok());
            assert!(heap.get(last).is_ok());
        }
        heap.collect_major([]).unwrap();
        assert_eq!(heap.stats().occupied_bytes, 0);
    }

    #[test]
    fn early_worker_publications_pack_nursery_pages_without_changing_handles() {
        let pool = StorageHeap::new(None, None, u64::from(PAGE_BYTES) * 2).into_worker_pool();
        let layout = pool
            .register_layout(StorageLayout {
                stride: 16,
                ..Default::default()
            })
            .unwrap();
        let mut roots = Vec::new();
        for index in 0_u64..256 {
            let mut lease = pool.acquire(16).unwrap();
            let handle = lease.reserve_prepared(layout, 16, 1, 16).unwrap();
            lease.write(handle, 0, &index.to_le_bytes()).unwrap();
            lease.commit(handle).unwrap();
            lease.flush().unwrap();
            roots.push(handle);
        }
        let stats = pool.stats().unwrap();
        assert_eq!(
            (
                stats.allocations,
                stats.occupied_bytes,
                stats.reserved_bytes
            ),
            (256, 4096, 0)
        );
        for collect in [false, true] {
            if collect {
                pool.collect_major(roots.iter().copied()).unwrap();
            } else {
                pool.collect_minor(roots.iter().copied()).unwrap();
            }
            let heap = pool.read_lock().unwrap();
            for (index, handle) in roots.iter().copied().enumerate() {
                assert_eq!(
                    heap.read(handle, 0, 8).unwrap(),
                    (index as u64).to_le_bytes()
                );
            }
        }
    }

    #[test]
    fn worker_lease_reads_fixed_lanes_into_caller_storage_once() {
        let pool = StorageHeap::default().into_worker_pool();
        let layout = pool
            .register_layout(StorageLayout {
                stride: 16,
                ..Default::default()
            })
            .unwrap();
        let mut lease = pool.acquire(16).unwrap();
        let handle = lease.reserve_prepared(layout, 16, 1, 16).unwrap();
        lease
            .write(handle, 0, &0x1122_3344_5566_7788_u64.to_le_bytes())
            .unwrap();
        lease
            .set_metadata(
                handle,
                StorageObjectMeta {
                    logical_bytes: 16,
                    typ: Some(TypeIdentity::Any),
                    initialized: true,
                    payload_len: 16,
                    kind: StoragePayloadKind::RawBytes,
                    version: 1,
                },
            )
            .unwrap();
        lease.commit(handle).unwrap();

        let mut bytes = [0_u8; 8];
        lease
            .read_into_if_present(handle, 0, &mut bytes)
            .expect("committed worker object must be found")
            .unwrap();
        assert_eq!(bytes, 0x1122_3344_5566_7788_u64.to_le_bytes());
        assert!(
            lease
                .read_into_if_present(StorageRef::NULL, 0, &mut bytes)
                .is_none()
        );

        let mut lane = [0_u8; 16];
        let accepted = lease
            .read_snapshot_into_if(handle, 0, &mut lane, |metadata| {
                assert_eq!(metadata.payload_len, 16);
                Ok(true)
            })
            .expect("committed worker object must be found")
            .unwrap();
        assert!(accepted);
        assert_eq!(&lane[..8], &bytes);
    }

    #[test]
    fn range_copy_handles_same_and_cross_page_ranges_and_card_barriers() {
        let mut heap = StorageHeap::default();
        let small = layout(&mut heap, 16, &[0]);
        let same = allocate(&mut heap, small, 16);
        let mut same_bytes = [0_u8; 16];
        same_bytes[8..].copy_from_slice(&1_u64.to_le_bytes());
        heap.write(same, 0, &same_bytes).unwrap();
        *heap.slots[same.slot as usize].metadata.write().unwrap() = Some(StorageObjectMeta {
            logical_bytes: 16,
            typ: None,
            initialized: true,
            payload_len: 16,
            kind: StoragePayloadKind::Typed,
            version: 1,
        });
        let pool = heap.into_worker_pool();
        assert!(
            pool.copy_range_if_version_shared(same, 0, same, 8, 8, 1)
                .unwrap()
        );
        assert_eq!(
            pool.read_lock().unwrap().read(same, 0, 16).unwrap(),
            [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0]
        );
        assert_eq!(pool.metadata(same).unwrap().version, 2);
        assert!(
            !pool
                .copy_range_if_version_shared(same, 0, same, 0, 8, 1)
                .unwrap()
        );

        let large = pool
            .register_layout(StorageLayout {
                stride: TLAB_MAX_OBJECT_BYTES + 8,
                references: vec![0],
                dynamics: Vec::new(),
            })
            .unwrap();
        let source = {
            let mut heap = pool.lock().unwrap();
            heap.reserve(large, 1, u64::from(TLAB_MAX_OBJECT_BYTES + 8))
                .unwrap()
                .commit()
        };
        let destination = {
            let mut heap = pool.lock().unwrap();
            heap.reserve(large, 1, u64::from(TLAB_MAX_OBJECT_BYTES + 8))
                .unwrap()
                .commit()
        };
        {
            let heap = pool.lock().unwrap();
            for handle in [source, destination] {
                *heap.slots[handle.slot as usize].metadata.write().unwrap() =
                    Some(StorageObjectMeta {
                        logical_bytes: u64::from(TLAB_MAX_OBJECT_BYTES + 8),
                        typ: None,
                        initialized: true,
                        payload_len: TLAB_MAX_OBJECT_BYTES + 8,
                        kind: StoragePayloadKind::Typed,
                        version: 1,
                    });
            }
        }
        pool.collect_minor([source, destination]).unwrap();
        pool.collect_minor([source, destination]).unwrap();
        let child = {
            let mut heap = pool.lock().unwrap();
            let child = heap.reserve(small, 1, 16).unwrap().commit();
            heap.write_reference(source, 0, child).unwrap();
            child
        };
        assert!(
            pool.copy_range_if_version_shared(source, 0, destination, 0, 8, 1)
                .unwrap()
        );
        pool.collect_minor([destination]).unwrap();
        assert!(pool.size(child).is_ok());
        assert_eq!(
            pool.read_lock()
                .unwrap()
                .read_reference(destination, 0)
                .unwrap(),
            child
        );
    }

    #[test]
    fn layout_lifetime_follows_empty_container_lease_and_payload_owners() {
        let pool = StorageHeap::default().into_worker_pool();
        let shape = StorageLayout {
            stride: 16,
            references: vec![0],
            dynamics: Vec::new(),
        };
        let layout = pool.register_layout(shape.clone()).unwrap();
        let pin = pool.pin_layout(layout).unwrap();
        let retained_pin = pin.clone();
        let resident = pool.stats().unwrap().resident_page_bytes;
        pool.collect_major([]).unwrap();
        assert_eq!(pool.layout(layout).unwrap(), shape);
        assert_eq!(pool.stats().unwrap().resident_page_bytes, resident);
        drop(pin);
        pool.collect_major([]).unwrap();
        assert_eq!(pool.layout(layout).unwrap(), shape);

        let object = {
            let mut heap = pool.lock().unwrap();
            heap.reserve(layout, 1, 16).unwrap().commit()
        };
        drop(retained_pin);
        pool.collect_major([object]).unwrap();
        assert_eq!(pool.layout(layout).unwrap(), shape);
        pool.collect_major([]).unwrap();
        assert!(pool.layout(layout).is_err());
        assert!(
            pool.lock().unwrap().layouts[layout as usize]
                .references
                .is_empty()
        );

        let reused = pool.register_layout(shape.clone()).unwrap();
        let pin = pool.pin_layout(reused).unwrap();
        assert_eq!(pool.layout(reused).unwrap(), shape);
        pool.close().unwrap();
        drop(pin);
        assert_eq!(pool.stats().unwrap().resident_page_bytes, 0);
    }

    fn layout(heap: &mut StorageHeap, stride: u32, refs: &[u32]) -> u32 {
        heap.register_layout(StorageLayout {
            stride,
            references: refs.to_vec(),
            dynamics: Vec::new(),
        })
        .unwrap()
    }

    fn allocate(heap: &mut StorageHeap, layout: u32, logical: u64) -> StorageRef {
        heap.reserve(layout, 1, logical).unwrap().commit()
    }

    #[test]
    fn payload_identity_matches_reference_lane_order() {
        let reference = StorageRef {
            slot: 7,
            generation: 19,
        };
        let bytes = reference.identity_for_payload().to_le_bytes();
        assert_eq!(&bytes[..4], &7u32.to_le_bytes());
        assert_eq!(&bytes[4..], &19u32.to_le_bytes());
        assert_eq!(
            StorageRef::from_payload_identity(u64::from_le_bytes(bytes)),
            reference
        );
    }

    #[test]
    fn copying_cycles_and_generation_reuse() {
        let mut heap = StorageHeap::default();
        let layout = layout(&mut heap, 16, &[0, 8]);
        let first = allocate(&mut heap, layout, 16);
        let second = allocate(&mut heap, layout, 16);
        heap.write_reference(first, 0, second).unwrap();
        heap.write_reference(second, 0, first).unwrap();
        heap.collect_minor([first]).unwrap();
        assert_eq!(heap.stats.scanned_references, 2);
        assert_eq!(heap.read_reference(first, 0).unwrap(), second);
        heap.release(second).unwrap();
        assert!(heap.read(second, 0, 1).is_err());
        let reused = allocate(&mut heap, layout, 16);
        assert_eq!(reused.slot, second.slot);
        assert_ne!(reused.generation, second.generation);
    }

    #[test]
    fn old_card_transitively_keeps_young_graph() {
        let mut heap = StorageHeap::default();
        let layout = layout(&mut heap, 8, &[0]);
        let parent = allocate(&mut heap, layout, 8);
        heap.collect_minor([parent]).unwrap();
        heap.collect_minor([parent]).unwrap();
        let child = allocate(&mut heap, layout, 8);
        let grandchild = allocate(&mut heap, layout, 8);
        heap.write_reference(parent, 0, child).unwrap();
        heap.write_reference(child, 0, grandchild).unwrap();
        heap.collect_minor([parent]).unwrap();
        assert!(heap.get(child).is_ok());
        assert!(heap.get(grandchild).is_ok());
    }

    #[test]
    fn dynamic_tag_card_boundary_keeps_reference() {
        let mut heap = StorageHeap::default();
        let layout = heap
            .register_layout(StorageLayout {
                stride: 280,
                references: Vec::new(),
                dynamics: vec![248],
            })
            .unwrap();
        let parent = allocate(&mut heap, layout, 280);
        heap.collect_minor([parent]).unwrap();
        heap.collect_minor([parent]).unwrap();
        let child = allocate(&mut heap, layout, 280);
        heap.write(parent, 252, &4u32.to_le_bytes()).unwrap();
        let mut ticket = [0u8; 8];
        ticket[0..4].copy_from_slice(&child.slot.to_le_bytes());
        ticket[4..8].copy_from_slice(&child.generation.to_le_bytes());
        heap.write(parent, 256, &ticket).unwrap();
        heap.collect_minor([parent]).unwrap();
        assert!(heap.get(child).is_ok());
    }

    #[test]
    fn versioned_field_write_preserves_neighbors_and_remembers_young_reference() {
        let mut heap = StorageHeap::default();
        let record_layout = layout(&mut heap, 24, &[8]);
        let scalar_layout = layout(&mut heap, 8, &[]);
        let parent = allocate(&mut heap, record_layout, 24);
        heap.write(parent, 0, &19u64.to_le_bytes()).unwrap();
        heap.write(parent, 16, &73u64.to_le_bytes()).unwrap();
        *heap.slots[parent.slot as usize].metadata.write().unwrap() = Some(StorageObjectMeta {
            logical_bytes: 24,
            typ: None,
            initialized: true,
            payload_len: 24,
            kind: StoragePayloadKind::Typed,
            version: 1,
        });
        heap.collect_minor([parent]).unwrap();
        heap.collect_minor([parent]).unwrap();
        let child = allocate(&mut heap, scalar_layout, 8);
        let pool = heap.into_worker_pool();
        let reference = child.identity_for_payload().to_le_bytes();
        let allocations = pool.stats().unwrap().allocations;
        assert!(
            pool.write_range_if_version(parent, 8, &reference, 1)
                .unwrap()
        );
        assert!(!pool.write_range_if_version(parent, 8, &[0; 8], 1).unwrap());
        assert!(pool.write_range_if_version(parent, 24, &[0; 8], 2).is_err());
        assert_eq!(pool.metadata(parent).unwrap().version, 2);
        pool.collect_minor([parent]).unwrap();
        {
            let heap = pool.lock().unwrap();
            assert_eq!(heap.read(parent, 0, 8).unwrap(), 19u64.to_le_bytes());
            assert_eq!(heap.read(parent, 16, 8).unwrap(), 73u64.to_le_bytes());
            assert_eq!(heap.read_reference(parent, 8).unwrap(), child);
            assert!(heap.get(child).is_ok());
        }
        assert_eq!(pool.stats().unwrap().allocations, allocations);
        assert!(pool.write_range_if_version(parent, 8, &[0; 8], 2).unwrap());
        pool.collect_major([parent]).unwrap();
        assert!(pool.size(child).is_err());
    }

    #[test]
    fn published_writers_share_pages_and_preserve_old_to_young_barrier() {
        let mut heap = StorageHeap::default();
        let record_layout = layout(&mut heap, 24, &[8]);
        let scalar_layout = layout(&mut heap, 8, &[]);
        let parent = allocate(&mut heap, record_layout, 24);
        *heap.slots[parent.slot as usize].metadata.write().unwrap() = Some(StorageObjectMeta {
            logical_bytes: 24,
            typ: None,
            initialized: true,
            payload_len: 24,
            kind: StoragePayloadKind::Typed,
            version: 1,
        });
        heap.collect_minor([parent]).unwrap();
        heap.collect_minor([parent]).unwrap();
        let child = allocate(&mut heap, scalar_layout, 8);
        let pool = heap.into_worker_pool();
        std::thread::scope(|scope| {
            let mut workers = Vec::new();
            for _worker in 0..4u64 {
                let pool = pool.clone();
                workers.push(scope.spawn(move || {
                    for _value in 0..64u64 {
                        loop {
                            let version = pool.metadata(parent).unwrap().version;
                            let mut bytes = [0_u8; 8];
                            // All workers write the same valid young edge and
                            // race only on the versioned short-write
                            // transaction.
                            bytes.copy_from_slice(&child.identity_for_payload().to_le_bytes());
                            if pool
                                .write_range_if_version(parent, 8, &bytes, version)
                                .unwrap()
                            {
                                break;
                            }
                        }
                    }
                }));
            }
            for worker in workers {
                worker.join().unwrap();
            }
        });
        assert_eq!(pool.metadata(parent).unwrap().version, 257);
        pool.collect_minor([parent]).unwrap();
        assert!(pool.size(child).is_ok());
        assert_eq!(
            pool.lock().unwrap().read_reference(parent, 8).unwrap(),
            child
        );
    }

    #[test]
    fn published_reader_never_observes_partial_same_shape_rewrite() {
        let mut heap = StorageHeap::default();
        let stride = 4096_u32;
        let layout = layout(&mut heap, stride, &[]);
        let handle = allocate(&mut heap, layout, u64::from(stride));
        heap.write(handle, 0, &vec![0x11; stride as usize]).unwrap();
        *heap.slots[handle.slot as usize].metadata.write().unwrap() = Some(StorageObjectMeta {
            logical_bytes: u64::from(stride),
            typ: None,
            initialized: true,
            payload_len: stride,
            kind: StoragePayloadKind::RawBytes,
            version: 1,
        });
        let pool = heap.into_worker_pool();
        let start = std::sync::Arc::new(std::sync::Barrier::new(2));
        let writer_pool = pool.clone();
        let writer_start = start.clone();
        let writer = std::thread::spawn(move || {
            let old = vec![0x11; stride as usize];
            let new = vec![0xee; stride as usize];
            writer_start.wait();
            for version in 2..102u64 {
                let bytes = if version % 2 == 0 { &new } else { &old };
                writer_pool
                    .rewrite_with_metadata(
                        handle,
                        layout,
                        1,
                        u64::from(stride),
                        bytes,
                        StorageObjectMeta {
                            logical_bytes: u64::from(stride),
                            typ: None,
                            initialized: true,
                            payload_len: stride,
                            kind: StoragePayloadKind::RawBytes,
                            version,
                        },
                    )
                    .unwrap();
            }
        });
        let reader_pool = pool.clone();
        let reader_start = start;
        let reader = std::thread::spawn(move || {
            reader_start.wait();
            for _ in 0..4000 {
                let bytes = reader_pool
                    .read_lock()
                    .unwrap()
                    .read(handle, 0, stride)
                    .unwrap();
                assert!(bytes.iter().all(|byte| *byte == 0x11 || *byte == 0xee));
                assert!(bytes.iter().all(|byte| *byte == bytes[0]));
            }
        });
        writer.join().unwrap();
        reader.join().unwrap();
    }

    #[test]
    fn published_snapshot_pairs_rewrite_metadata_with_payload() {
        let mut heap = StorageHeap::default();
        let stride = 4096_u32;
        let layout = layout(&mut heap, stride, &[]);
        let handle = allocate(&mut heap, layout, u64::from(stride));
        heap.write(handle, 0, &vec![0x11; stride as usize]).unwrap();
        *heap.slots[handle.slot as usize].metadata.write().unwrap() = Some(StorageObjectMeta {
            logical_bytes: u64::from(stride),
            typ: Some(TypeIdentity::Any),
            initialized: true,
            payload_len: stride,
            kind: StoragePayloadKind::RawBytes,
            version: 1,
        });
        let pool = heap.into_worker_pool();
        let start = std::sync::Arc::new(std::sync::Barrier::new(2));
        let writer_pool = pool.clone();
        let writer_start = start.clone();
        let writer = std::thread::spawn(move || {
            writer_start.wait();
            for version in 2..102_u64 {
                let is_new = version % 2 == 0;
                writer_pool
                    .rewrite_with_metadata(
                        handle,
                        layout,
                        1,
                        u64::from(stride),
                        &vec![if is_new { 0xee } else { 0x11 }; stride as usize],
                        StorageObjectMeta {
                            logical_bytes: u64::from(stride),
                            typ: Some(if is_new {
                                TypeIdentity::Primitive(1)
                            } else {
                                TypeIdentity::Any
                            }),
                            initialized: true,
                            payload_len: stride,
                            kind: StoragePayloadKind::RawBytes,
                            version,
                        },
                    )
                    .unwrap();
            }
        });
        let reader_pool = pool.clone();
        let reader_start = start;
        let reader = std::thread::spawn(move || {
            reader_start.wait();
            for _ in 0..4000 {
                let snapshot = reader_pool.read_snapshot(handle).unwrap();
                let expected = match snapshot.metadata.typ {
                    Some(TypeIdentity::Any) => 0x11,
                    Some(TypeIdentity::Primitive(1)) => 0xee,
                    other => panic!("unexpected snapshot type: {other:?}"),
                };
                assert!(snapshot.bytes.iter().all(|byte| *byte == expected));
            }
        });
        writer.join().unwrap();
        reader.join().unwrap();
    }

    #[test]
    fn fixed_rewrite_batch_validates_all_destinations_before_publish() {
        let mut heap = StorageHeap::default();
        let layout = layout(&mut heap, 8, &[]);
        let first = allocate(&mut heap, layout, 8);
        let second = allocate(&mut heap, layout, 8);
        for handle in [first, second] {
            *heap.slots[handle.slot as usize].metadata.write().unwrap() = Some(StorageObjectMeta {
                logical_bytes: 8,
                typ: None,
                initialized: true,
                payload_len: 8,
                kind: StoragePayloadKind::Typed,
                version: 1,
            });
        }
        let pool = heap.into_worker_pool();
        let replacement = |handle, value| StorageRewrite {
            handle,
            layout,
            bytes: vec![value; 8],
            metadata: StorageObjectMeta {
                logical_bytes: 8,
                typ: None,
                initialized: true,
                payload_len: 8,
                kind: StoragePayloadKind::Typed,
                version: 2,
            },
        };
        let stale = StorageRef {
            slot: u32::MAX,
            generation: 1,
        };
        assert!(
            pool.rewrite_fixed_batch(&[replacement(first, 7), replacement(stale, 9)])
                .is_err()
        );
        assert_eq!(
            pool.read_lock().unwrap().read(first, 0, 8).unwrap(),
            vec![0; 8]
        );
        pool.rewrite_fixed_batch(&[replacement(first, 7), replacement(second, 9)])
            .unwrap();
        assert_eq!(
            pool.read_lock().unwrap().read(first, 0, 8).unwrap(),
            vec![7; 8]
        );
        assert_eq!(
            pool.read_lock().unwrap().read(second, 0, 8).unwrap(),
            vec![9; 8]
        );
        assert_eq!(pool.metadata(first).unwrap().version, 2);
    }

    #[test]
    fn independent_random_graph_oracle() {
        let mut heap = StorageHeap::default();
        let layout = layout(&mut heap, 16, &[0, 8]);
        let objects: Vec<_> = (0..128).map(|_| allocate(&mut heap, layout, 16)).collect();
        let mut seed = 0x9e37_79b9_u32;
        let mut graph = vec![Vec::new(); objects.len()];
        for index in 0..objects.len() {
            for edge in 0..2 {
                seed ^= seed << 13;
                seed ^= seed >> 17;
                seed ^= seed << 5;
                let target = (seed as usize) % objects.len();
                graph[index].push(target);
                heap.write_reference(objects[index], edge * 8, objects[target])
                    .unwrap();
            }
        }
        let roots = [0usize, 17, 17, 91];
        let mut live = vec![false; objects.len()];
        fn visit(index: usize, graph: &[Vec<usize>], live: &mut [bool]) {
            if live[index] {
                return;
            }
            live[index] = true;
            for &child in &graph[index] {
                visit(child, graph, live);
            }
        }
        for root in roots {
            visit(root, &graph, &mut live);
        }
        heap.collect_minor(roots.into_iter().map(|index| objects[index]))
            .unwrap();
        for (index, object) in objects.into_iter().enumerate() {
            assert_eq!(heap.get(object).is_ok(), live[index]);
        }
    }

    #[test]
    fn reservation_blocks_collection_and_drop_aborts() {
        let mut heap = StorageHeap::default();
        let layout = layout(&mut heap, 8, &[]);
        let reservation = heap.reserve(layout, 1, 0).unwrap();
        drop(reservation);
        heap.collect_minor(std::iter::empty()).unwrap();
    }

    #[test]
    fn committed_worker_objects_leave_unused_credit_for_frame_admission() {
        let pool = StorageHeap::new(Some(64), None, u64::from(PAGE_BYTES) * 2).into_worker_pool();
        let layout = pool
            .register_layout(StorageLayout {
                stride: 16,
                ..Default::default()
            })
            .unwrap();
        let mut lease = pool.acquire(64).unwrap();
        let first = lease.reserve_prepared(layout, 16, 1, 16).unwrap();
        lease.commit(first).unwrap();
        assert_eq!(lease.unused_credit(), 48);
        assert!(lease.transfer_unused_credit(32));
        let control = StorageControlLease::from_transferred(&pool, 32).commit();
        let second = lease.reserve_prepared(layout, 16, 1, 16).unwrap();
        lease.commit(second).unwrap();
        assert_eq!(lease.unused_credit(), 0);
        lease.flush().unwrap();
        assert_eq!(pool.stats().unwrap().occupied_bytes, 64);
        assert_eq!(pool.stats().unwrap().reserved_bytes, 0);
        drop(control);
        assert_eq!(pool.stats().unwrap().occupied_bytes, 32);
        pool.collect_major([]).unwrap();
        assert_eq!(pool.stats().unwrap().occupied_bytes, 0);
    }

    #[test]
    fn major_promotes_full_worker_pages_and_allows_refill() {
        let pool = StorageHeap::new(None, None, u64::from(PAGE_BYTES) * 4).into_worker_pool();
        let layout = pool
            .register_layout(StorageLayout {
                stride: PAGE_BYTES / 4,
                ..Default::default()
            })
            .unwrap();
        let mut roots = Vec::new();
        for page in 0..4u64 {
            let mut lease = pool.acquire(u64::from(PAGE_BYTES)).unwrap();
            for item in 0..4u64 {
                let handle = lease
                    .reserve_prepared(layout, PAGE_BYTES / 4, 1, u64::from(PAGE_BYTES / 4))
                    .unwrap();
                lease
                    .write(handle, 0, &(page * 4 + item).to_le_bytes())
                    .unwrap();
                lease.commit(handle).unwrap();
                roots.push(handle);
            }
            lease.flush().unwrap();
        }
        assert!(matches!(pool.acquire(16), Err(error) if error.code == "collection_required"));
        pool.collect_major(roots.iter().copied()).unwrap();
        let mut lease = pool.acquire(u64::from(PAGE_BYTES / 4)).unwrap();
        let handle = lease
            .reserve_prepared(layout, PAGE_BYTES / 4, 1, u64::from(PAGE_BYTES / 4))
            .unwrap();
        lease.commit(handle).unwrap();
        lease.flush().unwrap();
        let stats = pool.stats().unwrap();
        assert_eq!(stats.allocations, 17);
        assert_eq!(stats.occupied_bytes, u64::from(PAGE_BYTES / 4) * 17);
        assert_eq!(stats.promoted_objects, 16);
        assert_eq!(stats.reserved_bytes, 0);
        for (index, handle) in roots.into_iter().enumerate() {
            assert_eq!(
                pool.lock().unwrap().read(handle, 0, 8).unwrap(),
                (index as u64).to_le_bytes()
            );
        }
    }

    #[test]
    fn promotion_space_covers_survivors_from_multiple_worker_batches() {
        let pool = StorageHeap::default().into_worker_pool();
        let layout = pool
            .register_layout(StorageLayout {
                stride: 4096,
                ..Default::default()
            })
            .unwrap();
        let mut roots = Vec::new();
        for _ in 0..4 {
            let mut lease = pool.acquire(16 * 4096).unwrap();
            for _ in 0..16 {
                let handle = lease.reserve_prepared(layout, 4096, 1, 4096).unwrap();
                lease.commit(handle).unwrap();
                roots.push(handle);
            }
            lease.flush().unwrap();
        }
        let before = pool.stats().unwrap();
        pool.collect_major(roots).unwrap();
        let after = pool.stats().unwrap();
        assert_eq!(after.promoted_objects, 64);
        assert_eq!(after.young_bytes, 0);
        assert_eq!(after.old_bytes, before.occupied_bytes);
        assert_eq!(after.total_allocated_bytes, before.total_allocated_bytes);
        assert_eq!(after.allocations, before.allocations);
    }

    #[test]
    fn growing_young_payload_into_large_storage_preserves_identity_and_edges() {
        let mut heap = StorageHeap::default();
        let scalar = layout(&mut heap, 8, &[]);
        let small = layout(&mut heap, 16, &[0]);
        let large = layout(&mut heap, PAGE_BYTES + 8, &[0]);
        let child = heap.reserve(scalar, 1, 8).unwrap().commit();
        let parent = heap.reserve(small, 1, 16).unwrap().commit();
        let mut bytes = vec![0; PAGE_BYTES as usize + 8];
        bytes[..8].copy_from_slice(&child.identity_for_payload().to_le_bytes());
        bytes[PAGE_BYTES as usize] = 42;
        heap.rewrite(parent, large, 1, bytes.len() as u64, &bytes)
            .unwrap();
        assert_eq!(heap.young_slots.len(), 1);
        assert_eq!(heap.young_classes.iter().sum::<usize>(), 1);
        heap.collect_minor([parent]).unwrap();
        assert_eq!(heap.read_reference(parent, 0).unwrap(), child);
        assert_eq!(heap.read(parent, PAGE_BYTES, 1).unwrap(), [42]);
        heap.collect_major([]).unwrap();
        assert_eq!(heap.stats().occupied_bytes, 0);
        assert!(heap.young_slots.is_empty());
        assert_eq!(heap.young_classes, [0; 12]);
    }

    #[test]
    fn worker_batch_publishes_commit_and_abort_at_flush() {
        let mut heap = StorageHeap::default();
        let layout = layout(&mut heap, 8, &[]);
        let pool = heap.into_worker_pool();
        let (committed, aborted) = {
            let mut batch = pool.acquire(16).unwrap();
            let committed = batch.reserve(layout, 1, 8).unwrap();
            let aborted = batch.reserve(layout, 1, 8).unwrap();
            batch.write(committed, 0, &[7; 8]).unwrap();
            batch.commit(committed).unwrap();
            batch.abort(aborted).unwrap();
            batch.flush().unwrap();
            (committed, aborted)
        };
        assert_eq!(pool.stats().unwrap().occupied_bytes, 8);
        assert_eq!(pool.stats().unwrap().reserved_bytes, 0);
        assert!(pool.lock().unwrap().get(committed).is_ok());
        assert!(pool.lock().unwrap().get(aborted).is_err());
        pool.collect_minor([committed]).unwrap();
    }

    #[test]
    fn worker_pool_flushes_concurrent_leases() {
        let mut heap = StorageHeap::default();
        let layout = layout(&mut heap, 8, &[]);
        let pool = heap.into_worker_pool();
        let mut roots = Vec::new();
        std::thread::scope(|scope| {
            let mut workers = Vec::new();
            for worker in 0..4u8 {
                let pool = pool.clone();
                workers.push(scope.spawn(move || {
                    let mut lease = pool.acquire(64).unwrap();
                    let mut handles = Vec::new();
                    for index in 0..8u8 {
                        let handle = lease.reserve(layout, 1, 8).unwrap();
                        lease
                            .write(handle, 0, &[worker, index, 0, 0, 0, 0, 0, 0])
                            .unwrap();
                        lease.commit(handle).unwrap();
                        handles.push(handle);
                    }
                    lease.flush().unwrap();
                    handles
                }));
            }
            for worker in workers {
                roots.extend(worker.join().unwrap());
            }
        });
        assert_eq!(pool.stats().unwrap().allocations, 32);
        pool.collect_minor(roots).unwrap();
        assert_eq!(pool.stats().unwrap().occupied_bytes, 256);
    }

    #[test]
    fn control_capacity_follows_its_owner_across_collection() {
        let pool = StorageHeap::new(Some(96), None, 4 << 20).into_worker_pool();
        {
            let pending = pool.reserve_control(64).unwrap();
            let stats = pool.stats().unwrap();
            assert_eq!((stats.occupied_bytes, stats.reserved_bytes), (0, 64));
            assert!(pool.reserve_control(33).is_err());
            drop(pending);
        }
        assert_eq!(pool.stats().unwrap().reserved_bytes, 0);
        let owned = pool.reserve_control(64).unwrap().commit();
        {
            let mut heap = pool.lock().unwrap();
            heap.collect_minor([]).unwrap();
            heap.collect_major([]).unwrap();
        }
        let stats = pool.stats().unwrap();
        assert_eq!((stats.occupied_bytes, stats.reserved_bytes), (64, 0));
        assert_eq!(stats.last_major_reachable_bytes, 64);
        assert_eq!(stats.total_allocated_bytes, 64);
        assert_eq!(stats.peak_bytes, 64);
        drop(owned);
        let stats = pool.stats().unwrap();
        assert_eq!(stats.occupied_bytes, 0);
        assert_eq!(stats.reclaimed_bytes, 64);
        assert_eq!(stats.total_allocated_bytes, 64);
    }

    #[test]
    fn control_lease_releases_at_owner_end_without_collection_credit() {
        let pool = StorageHeap::new(Some(64), None, 4 << 20).into_worker_pool();
        let owned = pool.reserve_control(64).unwrap().commit();
        assert_eq!(pool.stats().unwrap().occupied_bytes, 64);

        // A live owner consumes the full logical budget. Releasing that owner
        // must make the same budget available immediately; the next major is
        // not an accounting step and must not be required for reuse.
        assert!(pool.reserve_control(1).is_err());
        drop(owned);
        assert_eq!(pool.stats().unwrap().occupied_bytes, 0);
        assert_eq!(pool.stats().unwrap().reserved_bytes, 0);
        let replacement = pool.reserve_control(64).unwrap().commit();
        assert_eq!(pool.stats().unwrap().occupied_bytes, 64);
        drop(replacement);
        assert_eq!(pool.stats().unwrap().occupied_bytes, 0);
    }

    #[test]
    fn control_lease_abort_returns_reservation_without_publishing() {
        let pool = StorageHeap::new(Some(32), None, 4 << 20).into_worker_pool();
        let pending = pool.reserve_control(32).unwrap();
        assert_eq!(pool.stats().unwrap().reserved_bytes, 32);
        drop(pending);
        let stats = pool.stats().unwrap();
        assert_eq!((stats.occupied_bytes, stats.reserved_bytes), (0, 0));
        assert_eq!(stats.total_allocated_bytes, 0);
        let committed = pool.reserve_control(32).unwrap().commit();
        drop(committed);
    }

    #[test]
    fn dropped_worker_lease_reclaims_unflushed_publications() {
        let mut heap = StorageHeap::default();
        let layout = layout(&mut heap, 8, &[]);
        let pool = heap.into_worker_pool();
        {
            let mut lease = pool.acquire(8).unwrap();
            let handle = lease.reserve(layout, 1, 8).unwrap();
            lease.commit(handle).unwrap();
        }
        assert_eq!(pool.stats().unwrap().reserved_bytes, 0);
        assert_eq!(pool.stats().unwrap().occupied_bytes, 0);
        pool.collect_minor(std::iter::empty()).unwrap();
    }

    #[test]
    fn rewrite_accounts_for_growth_and_reserved_capacity() {
        let mut heap = StorageHeap::new(Some(32), None, 64 * 1024);
        let layout = layout(&mut heap, 8, &[]);
        let handle = allocate(&mut heap, layout, 8);
        let pool = heap.into_worker_pool();
        let pending = pool.reserve_control(16).unwrap();
        let before = pool.stats().unwrap();
        assert!(pool.rewrite(handle, layout, 3, 24, &[0; 24]).is_err());
        assert_eq!(pool.stats().unwrap(), before);
        drop(pending);
        pool.rewrite(handle, layout, 3, 24, &[0; 24]).unwrap();
        let grown = pool.stats().unwrap();
        assert_eq!(grown.occupied_bytes, 24);
        assert_eq!(grown.total_allocated_bytes, 24);
        assert_eq!(grown.allocations, 1);
        pool.rewrite(handle, layout, 3, 24, &[1; 24]).unwrap();
        assert_eq!(pool.stats().unwrap(), grown);
        pool.rewrite(handle, layout, 1, 8, &[0; 8]).unwrap();
        let shrunk = pool.stats().unwrap();
        assert_eq!(shrunk.occupied_bytes, 8);
        assert_eq!(shrunk.total_allocated_bytes, 24);
        assert_eq!(shrunk.reclaimed_bytes, 16);
    }

    #[test]
    fn rewrite_relocates_without_changing_handle_identity() {
        let mut heap = StorageHeap::default();
        let small = layout(&mut heap, 8, &[]);
        let large = layout(&mut heap, 16, &[0]);
        let handle = allocate(&mut heap, small, 8);
        let generation = handle.generation;
        let original_stats = heap.stats;
        assert!(heap.rewrite(handle, large, u32::MAX, 16, &[]).is_err());
        assert_eq!(heap.get(handle).unwrap().0, small);
        assert_eq!(heap.stats, original_stats);
        let payload = {
            let mut bytes = vec![0u8; 16];
            bytes[0..4].copy_from_slice(&handle.slot.to_le_bytes());
            bytes[4..8].copy_from_slice(&handle.generation.to_le_bytes());
            bytes
        };
        heap.rewrite(handle, large, 1, 16, &payload).unwrap();
        assert_eq!(heap.get(handle).unwrap().0, large);
        assert!(!heap.get(handle).unwrap().3);
        assert_eq!(heap.read_reference(handle, 0).unwrap(), handle);
        heap.collect_major([handle]).unwrap();
        let collected_stats = heap.stats;
        assert!(heap.rewrite(handle, small, 1, 8, &[]).is_err());
        assert_eq!(heap.stats, collected_stats);
        assert_eq!(heap.get(handle).unwrap().0, large);
        assert!(heap.get(handle).unwrap().3);
        assert_eq!(handle.generation, generation);
    }

    #[test]
    fn major_sweep_uses_bounded_parallel_partition_for_large_old_sets() {
        let mut heap = StorageHeap {
            collector_workers: MAX_COLLECTOR_WORKERS,
            ..Default::default()
        };
        let layout = layout(&mut heap, 8, &[]);
        let objects: Vec<_> = (0..300).map(|_| allocate(&mut heap, layout, 8)).collect();
        heap.collect_minor(objects.iter().copied()).unwrap();
        heap.collect_minor(objects.iter().copied()).unwrap();
        heap.collect_major(std::iter::empty()).unwrap();
        assert_eq!(heap.stats().occupied_bytes, 0);
        assert!(objects.iter().all(|object| heap.get(*object).is_err()));
    }

    #[test]
    fn resident_owner_admission_growth_and_close_preserve_accounting() {
        let mut heap = StorageHeap::default();
        let baseline = heap.stats().resident_page_bytes;
        heap.max_resident_bytes = Some(baseline + 64);
        let pool = heap.into_worker_pool();
        let mut owner = pool.reserve_resident(32).unwrap();
        assert_eq!(pool.stats().unwrap().resident_page_bytes, baseline + 32);
        let before = pool.stats().unwrap();
        assert!(owner.ensure_capacity(65).is_err());
        assert_eq!(pool.stats().unwrap(), before);
        owner.ensure_capacity(64).unwrap();
        owner.ensure_capacity(32).unwrap();
        assert_eq!(pool.stats().unwrap().resident_page_bytes, baseline + 64);
        drop(owner);
        assert_eq!(pool.stats().unwrap().resident_page_bytes, baseline);
        let owner = pool.reserve_resident(64).unwrap();
        pool.close().unwrap();
        drop(owner);
        let after = pool.stats().unwrap();
        assert_eq!(after.resident_page_bytes, 0);
        assert_eq!(after.total_allocated_bytes, 0);
        assert_eq!(after.allocations, 0);
    }

    #[test]
    fn worker_lease_metadata_resident_admission_is_released_on_drop() {
        let mut heap = StorageHeap::new(None, Some(200_000), 4 << 20);
        let _layout = layout(&mut heap, 8, &[]);
        let pool = heap.into_worker_pool();
        let baseline = pool.stats().unwrap().resident_page_bytes;
        let during;
        {
            let _lease = pool.acquire(64).unwrap();
            during = pool.stats().unwrap().resident_page_bytes;
        }
        let after = pool.stats().unwrap().resident_page_bytes;
        assert!(during > after);
        assert!(after >= baseline + u64::from(PAGE_BYTES) * 2);
        drop(pool.acquire(64).unwrap());
        assert_eq!(pool.stats().unwrap().resident_page_bytes, after);
    }

    #[test]
    fn major_releases_unreachable_large_page() {
        let mut heap = StorageHeap::default();
        let layout = layout(&mut heap, 32_768, &[]);
        let large = allocate(&mut heap, layout, 32_768);
        let before = heap.stats().resident_page_bytes;
        heap.collect_major(std::iter::empty()).unwrap();
        assert!(heap.get(large).is_err());
        assert!(heap.stats().resident_page_bytes < before);
    }

    #[test]
    fn major_workers_share_the_same_fixed_and_dynamic_reference_graph() {
        for workers in [1, MAX_COLLECTOR_WORKERS, usize::MAX] {
            let mut heap = StorageHeap {
                collector_workers: workers,
                ..Default::default()
            };
            let layout = heap
                .register_layout(StorageLayout {
                    stride: 32,
                    references: vec![0],
                    dynamics: vec![8],
                })
                .unwrap();
            let objects: Vec<_> = (0..512).map(|_| allocate(&mut heap, layout, 32)).collect();
            for index in 0..objects.len() {
                let partition = index / 256 * 256;
                let next = partition + (index + 1) % 256;
                let other = partition + (index + 7) % 256;
                heap.write_reference(objects[index], 0, objects[next])
                    .unwrap();
                heap.write(objects[index], 12, &VALUE_REF.to_le_bytes())
                    .unwrap();
                heap.write_reference(objects[index], 16, objects[other])
                    .unwrap();
            }
            heap.collect_minor(objects.iter().copied()).unwrap();
            heap.collect_minor(objects.iter().copied()).unwrap();
            let queue_capacity = heap.queue.capacity();
            let cards_capacity = heap.pending_cards.capacity();
            let references = heap.stats.scanned_references;
            assert_eq!(heap.collect_major([objects[0], objects[128]]).unwrap(), 256);
            for (index, object) in objects.iter().enumerate() {
                assert_eq!(
                    heap.get(*object).is_ok(),
                    index < 256,
                    "workers={workers}, object={index}"
                );
            }
            assert_eq!(heap.stats.occupied_bytes, 256 * 32);
            assert_eq!(heap.stats.scanned_references - references, 256 * 2);
            assert_eq!(heap.queue.capacity(), queue_capacity);
            assert_eq!(heap.pending_cards.capacity(), cards_capacity);
        }
    }

    #[test]
    fn promotion_uses_to_space_when_old_page_admission_fails() {
        let mut heap = StorageHeap::new(None, Some(150_000), 4 << 20);
        let layout = layout(&mut heap, 16, &[]);
        let root = allocate(&mut heap, layout, 16);
        heap.collect_minor([root]).unwrap();
        heap.collect_minor([root]).unwrap();
        assert!(!heap.get(root).unwrap().3);
    }

    #[test]
    fn nursery_pair_resident_admission_is_atomic_at_limit() {
        let mut heap = StorageHeap::new(None, Some(100_000), 4 << 20);
        let layout = layout(&mut heap, 8, &[]);
        assert!(heap.reserve(layout, 1, 8).is_err());
    }
}
