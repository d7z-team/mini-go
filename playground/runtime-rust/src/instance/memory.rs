//! Guest allocation accounting is independent of arena allocation and collection.

use super::*;

/// The operand batch owns the values. Its temporary census roots retain only
/// allocation costs and shared reference metadata, never array/byte payloads.
#[derive(Default)]
pub(super) struct OperandRoots {
    bytes: u64,
    data: Vec<Data>,
    slots: Vec<Handle>,
}

impl OperandRoots {
    pub fn clear(&mut self) {
        self.bytes = 0;
        self.data.clear();
        self.slots.clear();
    }
    pub fn storage_bytes(&self) -> usize {
        self.data.capacity() * size_of::<Data>() + self.slots.capacity() * size_of::<Handle>()
    }
    pub fn capture(&mut self, values: &[Value]) {
        self.clear();
        let mut inputs = values.iter();
        let mut pending = Vec::new();
        while let Some(value) = pending.pop().or_else(|| inputs.next()) {
            match &value.data {
                Data::String(bytes) => self.bytes = self.bytes.saturating_add(bytes.len() as u64),
                Data::Array(values) => {
                    self.bytes = self.bytes.saturating_add(128 + values.len() as u64 * 16);
                    pending.extend(values);
                }
                Data::Interface(value) | Data::DynamicFunction(value) => {
                    self.bytes = self.bytes.saturating_add(128);
                    pending.push(value);
                }
                Data::Function(function) => {
                    self.bytes = self
                        .bytes
                        .saturating_add(128 + function.captures.len() as u64 * 16);
                    self.slots
                        .extend(function.captures.iter().map(|address| address.root));
                }
                Data::Method { receiver, .. } => {
                    self.bytes = self.bytes.saturating_add(128);
                    if let Some(value) = receiver {
                        pending.push(value);
                    }
                }
                data @ (Data::Struct(_)
                | Data::Pointer(_)
                | Data::Slice(_)
                | Data::Map(_)
                | Data::ResourceRef(_)) => self.data.push(data.clone()),
                _ => {}
            }
        }
    }
}

impl Trace for OperandRoots {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        for handle in &self.slots {
            visit(*handle);
        }
        for data in &self.data {
            data.trace(visit);
        }
    }
}

#[derive(serde::Deserialize)]
struct BufferLayout {
    value_bytes: usize,
    page_bytes: usize,
    growth_threshold: usize,
    small_capacities: Vec<usize>,
    defer_bytes: usize,
    defer_capacities: Vec<usize>,
}

static BUFFER_LAYOUT: std::sync::LazyLock<BufferLayout> = std::sync::LazyLock::new(|| {
    #[derive(serde::Deserialize)]
    struct Manifest {
        buffer_layout: BufferLayout,
    }
    serde_json::from_str::<Manifest>(wire::CONTRACT_JSON)
        .expect("generated buffer layout")
        .buffer_layout
});

// Capacity growth follows Go's runtime/slice.go; see LICENSE-Go.
pub(super) fn grow_frame_buffer(
    capacity: usize,
    length: usize,
    deferred: bool,
) -> Result<usize, RuntimeError> {
    if length <= capacity {
        return Ok(capacity);
    }
    let overflow = || {
        RuntimeError::new(
            "allocation_limit",
            "frame",
            "frame buffer capacity overflow",
        )
    };
    let layout = &*BUFFER_LAYOUT;
    let mut next = capacity.checked_mul(2).ok_or_else(overflow)?;
    if length > next {
        next = length;
    } else if capacity >= layout.growth_threshold {
        next = capacity;
        while next < length {
            let growth = next
                .checked_add(3 * layout.growth_threshold)
                .ok_or_else(overflow)?
                >> 2;
            next = next.checked_add(growth).ok_or_else(overflow)?;
        }
    }
    let (element_bytes, capacities) = if deferred {
        (layout.defer_bytes, &layout.defer_capacities)
    } else {
        (layout.value_bytes, &layout.small_capacities)
    };
    if let Some(capacity) = capacities.get(next) {
        return Ok(*capacity);
    }
    next.checked_mul(element_bytes)
        .and_then(|bytes| bytes.checked_next_multiple_of(layout.page_bytes))
        .map(|bytes| bytes / element_bytes)
        .ok_or_else(overflow)
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct MemoryStats {
    pub live_bytes: u64,
    pub allocated_since_sweep: u64,
    pub total_allocated_bytes: u64,
    pub peak_bytes: u64,
}

#[derive(Default)]
pub(super) struct GuestMemory {
    state: std::sync::Mutex<GuestMemoryState>,
}

#[derive(Default)]
struct GuestMemoryState {
    stats: MemoryStats,
    frames: BTreeMap<(u64, usize, usize), Vec<GuestFrameAccounting>>,
    pooled_bytes: BTreeMap<(u64, usize), u64>,
}

#[derive(Clone, Copy, Debug, Default)]
pub(super) struct GuestFrameAccounting {
    pub base_slots: usize,
    pub popped: usize,
    pub returned: usize,
    pub deferred: usize,
}

impl GuestFrameAccounting {
    fn bytes(self) -> u64 {
        128u64.saturating_add(
            (self.base_slots as u64)
                .saturating_add(self.popped as u64)
                .saturating_add(self.returned as u64)
                .saturating_mul(16),
        )
    }
}

impl GuestMemory {
    pub fn stats(&self) -> MemoryStats {
        self.state.lock().unwrap().stats
    }

    pub fn try_charge(&self, bytes: u64, limit: u64) -> bool {
        let mut state = self.state.lock().unwrap();
        let stats = &mut state.stats;
        if bytes > limit.saturating_sub(stats.live_bytes) {
            return false;
        }
        stats.live_bytes = stats.live_bytes.saturating_add(bytes);
        stats.allocated_since_sweep = stats.allocated_since_sweep.saturating_add(bytes);
        stats.total_allocated_bytes = stats.total_allocated_bytes.saturating_add(bytes);
        stats.peak_bytes = stats.peak_bytes.max(stats.live_bytes);
        true
    }

    // The controller publishes a census only after every task is quiescent.
    pub fn publish_census(&self, live: u64) {
        let mut state = self.state.lock().unwrap();
        state.stats.live_bytes = live;
        state.stats.allocated_since_sweep = 0;
        state.stats.peak_bytes = state.stats.peak_bytes.max(live);
    }

    pub fn take_frame(
        &self,
        generation: u64,
        module: usize,
        function: usize,
    ) -> Option<GuestFrameAccounting> {
        let mut state = self.state.lock().unwrap();
        let storage = state
            .frames
            .get_mut(&(generation, module, function))?
            .pop()?;
        *state.pooled_bytes.get_mut(&(generation, module)).unwrap() -=
            storage.bytes() + storage.deferred as u64 * 16;
        Some(storage)
    }

    pub fn recycle_frame_storage(
        &self,
        generation: u64,
        module: usize,
        function: usize,
        storage: GuestFrameAccounting,
    ) {
        let mut state = self.state.lock().unwrap();
        let GuestMemoryState {
            frames,
            pooled_bytes,
            ..
        } = &mut *state;
        let bytes = pooled_bytes.entry((generation, module)).or_default();
        let pooled = storage.bytes().saturating_add(storage.deferred as u64 * 16);
        if bytes.saturating_add(pooled) > 8 << 20 {
            return;
        }
        let pool = frames.entry((generation, module, function)).or_default();
        if pool.len() < 8 {
            *bytes += pooled;
            pool.push(storage);
        }
    }

    pub fn retain_revisions(&self, mut retain: impl FnMut(u64) -> bool) {
        let mut state = self.state.lock().unwrap();
        state
            .frames
            .retain(|(generation, _, _), _| retain(*generation));
        state
            .pooled_bytes
            .retain(|(generation, _), _| retain(*generation));
    }
}

impl Instance {
    pub fn memory_stats(&self) -> MemoryStats {
        self.memory.stats()
    }

    pub(super) fn charge_guest_object(
        &mut self,
        slots: usize,
        entries: usize,
    ) -> Result<(), RuntimeError> {
        let bytes = (slots as u64)
            .checked_mul(16)
            .and_then(|bytes| (entries as u64).checked_mul(32)?.checked_add(bytes))
            .and_then(|bytes| bytes.checked_add(128))
            .ok_or_else(|| {
                RuntimeError::new(
                    "allocation_limit",
                    "guest",
                    "guest allocation size overflow",
                )
            })?;
        self.charge_guest(bytes)
    }

    pub(super) fn charge_guest(&mut self, bytes: u64) -> Result<(), RuntimeError> {
        if bytes == 0 {
            return Ok(());
        }
        let limit = self.limits.max_allocated_bytes;
        if self.memory.try_charge(bytes, limit) {
            return Ok(());
        }
        if self.running.instruction_active {
            self.running.census_request = Some(bytes);
            return Err(RuntimeError::new(
                "census_required",
                "guest",
                "guest memory census required",
            ));
        }
        let live = self.live_guest_bytes()?;
        self.memory.publish_census(live);
        if !self.memory.try_charge(bytes, limit) {
            return Err(RuntimeError::new(
                "allocation_limit",
                "guest",
                "guest allocation byte limit exceeded",
            ));
        }
        Ok(())
    }

    pub(super) fn live_guest_bytes(&self) -> Result<u64, RuntimeError> {
        let heap = self.heap.snapshot();
        let mut sizer = GuestSizer {
            heap: &heap,
            bytes: self.types.dynamic_stats().1,
            slots: HashSet::new(),
            storage: HashSet::new(),
            maps: HashSet::new(),
            resources: HashSet::new(),
            structs: HashSet::new(),
            pointers: HashSet::new(),
            slices: HashSet::new(),
            pending: self
                .tasks()
                .flat_map(|task| &task.allocation_roots)
                .map(|value| &value.data)
                .collect(),
            pending_resources: Vec::new(),
        };
        for handle in self.globals.values() {
            sizer.slot(*handle)?;
        }
        sizer
            .pending
            .extend(self.reflected_types.iter().map(|(_, value)| &value.data));
        for frame in self
            .tasks()
            .flat_map(|task| task.frames.iter().chain(&task.suspended_frames))
        {
            sizer.add(frame.memory.bytes());
            for iterator in frame.map_iterators.values() {
                sizer.add(128 + iterator.entries.capacity() as u64 * 32);
                sizer.pending.push(&iterator.object.data);
            }
            for local in &frame.locals {
                match local {
                    frame::Local::Shared(handle) => sizer.slot(*handle)?,
                    frame::Local::Private(value) => sizer.pending.push(&value.get().data),
                }
            }
            for address in &frame.upvalues {
                sizer.slot(address.root)?;
            }
            sizer
                .pending
                .extend(frame.stack.iter().map(|value| &value.data));
            sizer.add(frame.popped_roots.bytes);
            sizer.pending.extend(&frame.popped_roots.data);
            for handle in &frame.popped_roots.slots {
                sizer.slot(*handle)?;
            }
            if let Some(values) = &frame.returning {
                sizer.pending.extend(values.iter().map(|value| &value.data));
            }
            if let Some(value) = &frame.panic {
                sizer.pending.push(&value.data);
            }
            if let Some(value) = &frame.recovered {
                sizer.pending.push(&value.data);
            }
            for function in &frame.defers {
                sizer.add(128 + function.captures.len() as u64 * 16);
                for address in &function.captures {
                    sizer.slot(address.root)?;
                }
            }
        }
        for task in self.tasks() {
            sizer
                .pending
                .extend(task.retry_operands.iter().map(|value| &value.data));
            match &task.pending_write {
                Some(mutation::PendingWrite::Map(write)) => {
                    sizer.slot(write.root)?;
                    sizer.pending.extend([&write.key.data, &write.value.data]);
                }
                Some(mutation::PendingWrite::Delete { root, key }) => {
                    sizer.slot(*root)?;
                    sizer.pending.push(&key.data);
                }
                Some(mutation::PendingWrite::Address { address, value }) => {
                    sizer.slot(address.root)?;
                    sizer.pending.push(&value.data);
                }
                None => {}
            }
            if let Some(selection) = &task.selection_completion {
                sizer.selection(selection);
            }
            match &task.blocked {
                Some(scheduler::Blocked::Select(selection)) => sizer.selection(selection),
                Some(scheduler::Blocked::Mutex(handle)) => sizer.pending_resources.push(*handle),
                _ => {}
            }
        }
        for timer in &self.timers {
            sizer.pending.push(&timer.channel.data);
        }
        sizer.finish()
    }
}

struct GuestSizer<'a> {
    heap: &'a crate::heap::HeapSnapshot<Value>,
    bytes: u64,
    slots: HashSet<Handle>,
    storage: HashSet<Address>,
    maps: HashSet<Handle>,
    resources: HashSet<Handle>,
    structs: HashSet<usize>,
    pointers: HashSet<usize>,
    slices: HashSet<usize>,
    pending: Vec<&'a Data>,
    pending_resources: Vec<Handle>,
}

impl<'a> GuestSizer<'a> {
    fn selection(&mut self, selection: &'a select::Selection) {
        self.add(128 + selection.cases.len() as u64 * 176);
        for case in &selection.cases {
            self.pending.push(&case.channel.data);
            if let Some(value) = &case.send {
                self.pending.push(&value.data);
            }
            if let Some(value) = &case.zero {
                self.pending.push(&value.data);
            }
        }
        if let Some(outcome) = &selection.outcome
            && let Some(value) = &outcome.value
        {
            self.pending.push(&value.data);
        }
    }
    fn add(&mut self, bytes: u64) {
        self.bytes = self.bytes.saturating_add(bytes);
    }

    fn slot(&mut self, handle: Handle) -> Result<(), RuntimeError> {
        if self.slots.insert(handle) {
            self.pending.push(&self.heap.get(handle)?.data);
        }
        Ok(())
    }

    // Projection for accounting borrows the backing window. Type views change
    // guest type identity, but not the storage shape measured here.
    fn backing(
        &mut self,
        root: Handle,
        path: &[PathElement],
        charge: bool,
    ) -> Result<(), RuntimeError> {
        let mut value = self.heap.get(root)?;
        let mut window: Option<std::ops::Range<usize>> = None;
        for segment in path {
            match segment {
                PathElement::TypeView(_) => {}
                PathElement::ArrayView { start, length, .. } => {
                    let available = match &value.data {
                        Data::Array(values) => values.len(),
                        Data::Bytes(bytes) => bytes.len(),
                        _ => {
                            return Err(RuntimeError::new(
                                "invalid_address",
                                "memory",
                                "array view requires backing",
                            ));
                        }
                    };
                    let previous = window.unwrap_or(0..available);
                    if *start > previous.len() || *length > previous.len() - start {
                        return Err(RuntimeError::new(
                            "invalid_address",
                            "memory",
                            "array view exceeds backing",
                        ));
                    }
                    let begin = previous.start + start;
                    window = Some(begin..begin + length);
                }
                PathElement::Index(index) => {
                    let Data::Array(values) = &value.data else {
                        return Err(RuntimeError::new(
                            "invalid_address",
                            "memory",
                            "index requires array",
                        ));
                    };
                    let range = window.take().unwrap_or(0..values.len());
                    value = values[range].get(*index).ok_or_else(|| {
                        RuntimeError::new("invalid_address", "memory", "index exceeds backing")
                    })?;
                }
                PathElement::Field(name) => {
                    let Data::Struct(fields) = &value.data else {
                        return Err(RuntimeError::new(
                            "invalid_address",
                            "memory",
                            "field requires struct",
                        ));
                    };
                    value = fields.get(name).ok_or_else(|| {
                        RuntimeError::new("invalid_address", "memory", "missing backing field")
                    })?;
                    window = None;
                }
            }
        }
        match &value.data {
            Data::Bytes(bytes) => {
                let range = window.unwrap_or(0..bytes.len());
                if charge {
                    self.add(if path.is_empty() {
                        range.len() as u64
                    } else {
                        range.len() as u64 * 16
                    });
                }
            }
            Data::Array(values) => {
                let range = window.unwrap_or(0..values.len());
                if charge {
                    self.add(range.len() as u64 * 16);
                }
                self.pending
                    .extend(values[range].iter().map(|value| &value.data));
            }
            _ => {
                return Err(RuntimeError::new(
                    "invalid_slice",
                    "memory",
                    "invalid backing",
                ));
            }
        }
        Ok(())
    }

    fn finish(mut self) -> Result<u64, RuntimeError> {
        while !self.pending.is_empty() || !self.pending_resources.is_empty() {
            if let Some(handle) = self.pending_resources.pop() {
                if !self.resources.insert(handle) {
                    continue;
                }
                let Data::Resource(resource) = &self.heap.get(handle)?.data else {
                    return Err(RuntimeError::new(
                        "type_error",
                        "resource",
                        "invalid resource handle",
                    ));
                };
                self.add(resource.logical_bytes());
                match &**resource {
                    scheduler::Resource::Channel { values, .. } => {
                        self.pending.extend(values.iter().map(|value| &value.data));
                    }
                    scheduler::Resource::Mutex { .. } => {}
                }
                continue;
            }
            let data = self.pending.pop().unwrap();
            match data {
                Data::String(bytes) => self.add(bytes.len() as u64),
                Data::Array(values) => {
                    self.add(128 + values.len() as u64 * 16);
                    self.pending.extend(values.iter().map(|value| &value.data));
                }
                Data::Struct(fields) if self.structs.insert(fields.identity()) => {
                    self.add(128 + fields.guest_slots() as u64 * 16);
                    self.pending
                        .extend(fields.initialized_values().map(|value| &value.data));
                }
                Data::Interface(value) | Data::DynamicFunction(value) => {
                    self.add(128);
                    self.pending.push(&value.data);
                }
                Data::Function(function) => {
                    self.add(128 + function.captures.len() as u64 * 16);
                    for address in &function.captures {
                        self.slot(address.root)?;
                    }
                }
                Data::Method { receiver, .. } => {
                    self.add(128);
                    if let Some(value) = receiver {
                        self.pending.push(&value.data);
                    }
                }
                Data::Pointer(address) => {
                    for origin in std::iter::once(address.pointer_origin())
                        .chain(address.identity.original.iter().cloned())
                    {
                        if !self.pointers.insert(Arc::as_ptr(&origin.key) as usize) {
                            continue;
                        }
                        self.add(128 + origin.slots as u64 * 16);
                        if let Some(root) = origin.root {
                            self.slot(root)?;
                        }
                        if let Some((root, path, capacity)) = origin.array {
                            self.add(128 + capacity as u64 * 16);
                            self.backing(root, &path, false)?;
                        }
                    }
                }
                Data::Slice(slice) if self.slices.insert(Arc::as_ptr(&slice.identity) as usize) => {
                    self.add(128);
                    if self.storage.insert(slice.storage.clone()) {
                        self.backing(slice.storage.root, &slice.storage.path, true)?;
                    }
                }
                Data::Map(handle) if self.maps.insert(*handle) => {
                    let Data::MapEntries(entries) = &self.heap.get(*handle)?.data else {
                        unreachable!()
                    };
                    self.add(128 + entries.len() as u64 * 32);
                    for (key, value) in entries {
                        self.pending.push(&key.data);
                        self.pending.push(&value.data);
                    }
                }
                Data::ResourceRef(handle) => self.pending_resources.push(*handle),
                _ => {}
            }
        }
        Ok(self.bytes)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn operand_census_tracks_large_payload_cost_without_retaining_payload_storage() {
        for size in [32, 1 << 20] {
            let values = vec![Value::string(vec![b'x'; size])];
            let mut roots = OperandRoots::default();
            roots.capture(&values);
            assert_eq!(roots.bytes, size as u64);
            assert_eq!(roots.storage_bytes(), 0);
            roots.clear();
            assert_eq!(roots.bytes, 0);
        }
    }

    #[test]
    fn byte_backing_census_preserves_shared_storage_and_counts_it_once() {
        let program = Arc::new(
            Program::load(
                include_bytes!("../../examples/blocks/arithmetic.json"),
                crate::loader::LoadLimits::default(),
            )
            .unwrap(),
        );
        let mut instance = Instance::new(
            program,
            ExecutionLimits {
                max_sequence_elements: 2 << 20,
                ..ExecutionLimits::default()
            },
        )
        .unwrap();
        let size = 1 << 20;
        let value = instance
            .make_bytes(
                TypeIdentity::Slice(Arc::new(TypeIdentity::Primitive(wire::PrimitiveUint8))),
                size,
                size,
                &vec![42; size],
            )
            .unwrap();
        instance.running.allocation_roots = vec![value.clone(), value];
        assert_eq!(instance.live_guest_bytes().unwrap(), 128 + size as u64);
        assert_eq!(instance.live_guest_bytes().unwrap(), 128 + size as u64);
        instance.close().unwrap();
    }

    #[test]
    fn concurrent_allocations_share_one_guest_limit_and_failed_charges_leave_stats_unchanged() {
        for workers in [1, 2, 4] {
            let memory = GuestMemory::default();
            let admitted = std::sync::atomic::AtomicU64::new(0);
            std::thread::scope(|scope| {
                for _ in 0..workers {
                    scope.spawn(|| {
                        for _ in 0..256 {
                            if memory.try_charge(16, 2048) {
                                admitted.fetch_add(16, std::sync::atomic::Ordering::Relaxed);
                            }
                        }
                    });
                }
            });
            assert_eq!(admitted.load(std::sync::atomic::Ordering::Relaxed), 2048);
            let full = MemoryStats {
                live_bytes: 2048,
                allocated_since_sweep: 2048,
                total_allocated_bytes: 2048,
                peak_bytes: 2048,
            };
            assert_eq!(memory.stats(), full);
            assert!(!memory.try_charge(1, 2048));
            assert_eq!(memory.stats(), full);
            memory.publish_census(1024);
            assert!(memory.try_charge(1024, 2048));
            assert_eq!(
                memory.stats(),
                MemoryStats {
                    live_bytes: 2048,
                    allocated_since_sweep: 1024,
                    total_allocated_bytes: 3072,
                    peak_bytes: 2048
                }
            );
        }
    }

    #[test]
    fn idle_frame_budget_includes_retained_defer_capacity() {
        let memory = GuestMemory::default();
        let storage = GuestFrameAccounting {
            deferred: ((8 << 20) - 128) / 16,
            ..GuestFrameAccounting::default()
        };
        memory.recycle_frame_storage(1, 0, 1, storage);
        memory.recycle_frame_storage(1, 0, 2, GuestFrameAccounting::default());
        assert!(memory.take_frame(1, 0, 2).is_none());
        assert!(memory.take_frame(1, 0, 1).is_some());
        memory.recycle_frame_storage(1, 0, 2, GuestFrameAccounting::default());
        assert!(memory.take_frame(1, 0, 2).is_some());
        memory.recycle_frame_storage(
            1,
            0,
            3,
            GuestFrameAccounting {
                deferred: storage.deferred + 1,
                ..GuestFrameAccounting::default()
            },
        );
        assert!(memory.take_frame(1, 0, 3).is_none());
    }
}
