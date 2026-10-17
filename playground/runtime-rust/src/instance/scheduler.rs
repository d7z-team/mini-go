//! Task continuations and wait registrations. The owner schedules and parks
//! tasks; native workers may own their private state while executing a slice.

use super::*;
use crate::managed_channel::{ManagedChannel, SendResult};

pub(super) struct ResourceRead(Resource);

impl std::ops::Deref for ResourceRead {
    type Target = Resource;
    fn deref(&self) -> &Resource {
        &self.0
    }
}

#[derive(Default)]
pub(super) struct Task {
    pub id: u64,
    pub scope: u64,
    pub frames: Vec<Frame>,
    pub suspended_frames: Vec<Frame>,
    pub blocked: Option<Blocked>,
    pub transient_roots: Vec<Handle>,
    pub allocation_roots: Vec<Value>,
    pub popped_frame: Option<usize>,
    pub scheduling_phase: u8,
    pub step_grant: Option<budget::StepGrant>,
    pub selection_completion: Option<Box<select::Selection>>,
    pub pending_write: Option<mutation::PendingWrite>,
    pub write_collection: mutation::WriteCollection,
    // An allocating instruction may have to stop at the owner boundary. Its
    // slots keep exact operands until the owner either rejects the allocation
    // or resumes the same PC without charging another bytecode step.
    pub instruction_active: bool,
    pub instruction_pc: usize,
    // The program counter advances before execution. Keep the origin frame
    // so a collection during a fallible instruction sees its operands under
    // the liveness map for the instruction being retried.
    pub instruction_frame: Option<usize>,
    pub instruction_frame_suspended: bool,
    pub pending_error: Option<RuntimeError>,
    pub selection_lease: Option<crate::managed_storage::StorageControlLease>,
}

impl Task {
    /// A task may execute on a different executor worker than its owner.  The
    /// worker-local nursery belongs to the thread, so return it before the
    /// task continuation is handed back to the owner or the worker is reused.
    pub(super) fn publish_worker_storage(&self) {
        if let Some(frame) = self
            .frames
            .first()
            .or_else(|| self.suspended_frames.first())
        {
            let _ = frame.heap.flush_worker_lease();
        }
    }
}

impl Trace for Task {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        if let Some(write) = &self.pending_write {
            write.trace(visit);
        }
        if let Some(selection) = &self.selection_completion {
            selection.trace(visit);
        }
        for (index, frame) in self.frames.iter().enumerate() {
            if self.instruction_active
                && !self.instruction_frame_suspended
                && self.instruction_frame == Some(index)
            {
                frame.trace_at(self.instruction_pc, false, visit);
            } else {
                frame.trace(visit);
            }
        }
        for (index, frame) in self.suspended_frames.iter().enumerate() {
            if self.instruction_active
                && self.instruction_frame_suspended
                && index + 1 == self.suspended_frames.len()
            {
                frame.trace_at(self.instruction_pc, false, visit);
            } else {
                frame.trace(visit);
            }
        }
        for handle in &self.transient_roots {
            visit(*handle);
        }
        for value in &self.allocation_roots {
            value.trace(visit);
        }
        if let Some(operation) = &self.blocked {
            operation.trace(visit);
        }
    }
}

#[derive(Clone)]
pub(super) enum Blocked {
    Select(Box<select::Selection>),
    Mutex(Handle),
    Ffi(u64),
    Module(String),
}

impl Trace for Blocked {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        match self {
            Self::Select(selection) => selection.trace(visit),
            Self::Mutex(handle) => visit(*handle),
            Self::Ffi(_) | Self::Module(_) => {}
        }
    }
}

#[derive(Clone, Debug)]
pub enum Resource {
    Mutex {
        locked: bool,
        grant: Option<u64>,
        waiters: VecDeque<u64>,
    },
    Channel {
        channel: Arc<std::sync::Mutex<ManagedChannel>>,
    },
}

impl Resource {
    pub(crate) fn from_page_mutex(
        locked: bool,
        grant: Option<u64>,
        waiters: VecDeque<u64>,
    ) -> Self {
        Self::Mutex {
            locked,
            grant,
            waiters,
        }
    }

    // Keep the page descriptor flat at this boundary; it mirrors the
    // serialized RS01 channel state and avoids a second compatibility struct.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn from_page_channel(
        pool: &crate::managed_storage::StorageWorkerPool,
        layout: u32,
        stride: u32,
        logical_bytes: u64,
        capacity: usize,
        ring: crate::managed_storage::StorageRef,
        head: usize,
        len: usize,
        closed: bool,
    ) -> Result<Self, RuntimeError> {
        Ok(Self::Channel {
            channel: Arc::new(std::sync::Mutex::new(ManagedChannel::from_parts(
                pool,
                layout,
                stride,
                logical_bytes,
                capacity,
                ring,
                head,
                len,
                closed,
            )?)),
        })
    }
}

impl Trace for Resource {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        match self {
            Self::Mutex { .. } => {}
            Self::Channel { channel } => {
                // The guest resource record owns the ring. This edge is
                // traced from the record payload so channel_records remains
                // a lookup cache rather than a collector root.
                if let Ok(channel) = channel.lock() {
                    let ring = channel.root();
                    if ring.valid() {
                        visit(ring);
                    }
                }
            }
        }
    }
}

impl Instance {
    fn channel_layout(
        &self,
        typ: &TypeIdentity,
    ) -> Result<(crate::managed_storage::StorageLayout, u32), RuntimeError> {
        let prepared = self.heap.layout_for_type(typ)?;
        let stride = prepared.stride.max(8);
        let references = if prepared.dynamics.is_empty() {
            prepared.references.to_vec()
        } else {
            Vec::new()
        };
        Ok((
            crate::managed_storage::StorageLayout {
                stride,
                references,
                dynamics: prepared.dynamics.to_vec(),
            },
            stride,
        ))
    }

    pub(super) fn new_channel_resource(
        &self,
        typ: &TypeIdentity,
        capacity: usize,
    ) -> Result<Resource, RuntimeError> {
        let (layout, _stride) = self.channel_layout(typ)?;
        let logical_bytes = self.types.storage_logical_bytes(typ)?;
        let channel =
            ManagedChannel::new(&self.heap.storage_pool(), capacity, layout, logical_bytes)?;
        let channel = Arc::new(std::sync::Mutex::new(channel));
        Ok(Resource::Channel { channel })
    }

    pub(super) fn register_channel_resource(&self, handle: Handle, resource: &Resource) {
        if let Resource::Channel { channel } = resource {
            self.channel_records
                .lock()
                .unwrap()
                .insert(handle, channel.clone());
        }
    }

    pub(super) fn encode_channel_value(
        &self,
        value: &Value,
        _typ: &TypeIdentity,
    ) -> Result<crate::heap::PreparedPayload, RuntimeError> {
        // An `Any` channel stores the runtime type in the typed box node while
        // keeping a uniform descriptor lane for every message.
        if *_typ == TypeIdentity::Any || self.types.is_interface(_typ)? {
            return self.heap.prepared_dynamic_payload(value);
        }
        self.heap.prepared_payload(value)
    }

    pub(super) fn decode_channel_value(
        &self,
        payload: &[u8],
        typ: &TypeIdentity,
    ) -> Result<Value, RuntimeError> {
        let layout = self.types.storage_lane_layout(typ)?;
        // Rings give every element an addressable physical slot, including
        // struct{} and [0]T. Decode only its logical lane; padding is not part
        // of the value. Rendezvous messages already contain the exact lane.
        let width = layout.stride as usize;
        if payload.len() != width && payload.len() != width.max(8) {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "channel",
                "message width does not match its element layout",
            ));
        }
        self.heap
            .decode_prepared_lane(typ, &layout, &payload[..width])
    }

    pub(super) fn tasks(&self) -> impl Iterator<Item = &Task> {
        std::iter::once(&self.running)
            .chain(&self.preparing_task)
            .chain(&self.resuming_task)
            .chain(&self.runnable)
            .chain(self.blocked.iter())
    }

    pub(super) fn admit_task(&self) -> Result<(), RuntimeError> {
        let active: usize = self.scope_work.values().map(|work| work.tasks).sum();
        if active >= self.limits.max_tasks {
            return Err(RuntimeError::new(
                "task_limit",
                "scheduler",
                "task limit exceeded",
            ));
        }
        Ok(())
    }

    pub(super) fn close_channel(&mut self, channel: &Value) -> Result<(), RuntimeError> {
        if matches!(channel.data, Data::Nil) {
            return Err(RuntimeError::new("panic", "close", "close of nil channel"));
        }
        let handle = Self::resource_handle(channel)?;
        let resource = self.resource(handle)?.clone();
        let Resource::Channel { channel } = &resource else {
            return Err(RuntimeError::new("type_error", "close", "expected channel"));
        };
        let record_version = self.prepare_channel_record_update(handle)?;
        channel
            .lock()
            .map_err(|_| RuntimeError::new("runtime", "close", "channel lock poisoned"))?
            .close(&self.heap.storage_pool())?;
        self.commit_channel_record_update(handle, record_version, channel)
    }

    fn materialize_ffi_result(
        &mut self,
        payload: &[u8],
        error: Option<&RuntimeError>,
    ) -> Result<Vec<Value>, RuntimeError> {
        let (message, status) = match error {
            Some(error) => (
                error.message.clone(),
                if error.code == "route_unavailable" {
                    1
                } else {
                    2
                },
            ),
            None => (String::new(), 0),
        };
        let typ = TypeIdentity::Slice(std::sync::Arc::new(TypeIdentity::Primitive(
            wire::PrimitiveUint8,
        )));
        let value = self.make_bytes(typ, payload.len(), payload.len(), payload)?;
        Ok(vec![
            value,
            Value {
                typ: TypeIdentity::Primitive(wire::PrimitiveString),
                data: Data::String(message.into_bytes().into()),
            },
            Value::int(status),
        ])
    }

    /// Publish a ready FFI reply as one result transaction. A suspended
    /// continuation stores result handles in its delivery buffer; allocating
    /// those handles one by one can otherwise leave the first result visible
    /// when a later allocation reaches the resident limit. Prepare the whole
    /// batch and reserve the delivery capacity before committing any handle.
    fn publish_ffi_results(&mut self, values: Vec<Value>) -> Result<(), RuntimeError> {
        let frame = self
            .resuming_task
            .as_mut()
            .and_then(|task| task.frames.last_mut())
            .ok_or_else(|| RuntimeError::new("invalid_frame", "ffi", "missing waiting frame"))?;
        if frame.continuation.is_none() {
            return frame.publish_results_atomic(values);
        }
        if values.is_empty() {
            return Ok(());
        }
        let old_length = frame.delivery.handles.len();
        let required = old_length.checked_add(values.len()).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "frame", "result count overflows")
        })?;
        let old_capacity = frame.delivery.handles.capacity();
        let new_capacity = old_capacity
            .max(required)
            .max(1)
            .checked_next_power_of_two()
            .ok_or_else(|| {
                RuntimeError::new(
                    "allocation_limit",
                    "frame",
                    "result buffer capacity overflows",
                )
            })?;
        frame.ensure_resident_growth(old_capacity, new_capacity, std::mem::size_of::<Handle>())?;
        frame
            .delivery
            .handles
            .try_reserve(required.saturating_sub(old_length))
            .map_err(|error| {
                RuntimeError::new(
                    "allocation_limit",
                    "frame",
                    format!("result buffer allocation failed: {error}"),
                )
            })?;
        let allocations = values
            .into_iter()
            .map(|value| {
                let bytes = frame.heap.logical_allocation_bytes(&value)?;
                Ok((value, bytes))
            })
            .collect::<Result<Vec<_>, RuntimeError>>()?;
        let batch = frame.heap.prepare_allocations(allocations)?;
        frame.delivery.handles.extend_from_slice(batch.handles());
        batch.commit();
        Ok(())
    }

    pub(super) fn allocate_task_id(&mut self) -> Result<u64, RuntimeError> {
        let id = self.next_task;
        self.next_task = id.checked_add(1).ok_or_else(|| {
            RuntimeError::new("task_limit", "scheduler", "task identifiers exhausted")
        })?;
        Ok(id)
    }

    pub(super) fn yield_task(&mut self) {
        self.running.step_grant = None;
        if !self.running.frames.is_empty() {
            self.runnable.push_back(std::mem::take(&mut self.running));
        }
    }

    pub(super) fn schedule_next(&mut self) -> bool {
        if let Some(task) = self.runnable.pop_front() {
            self.running = task;
            true
        } else {
            false
        }
    }

    pub(super) fn park(&mut self, operation: Blocked) {
        self.running.step_grant = None;
        if matches!(operation, Blocked::Ffi(_)) {
            self.scope_work
                .entry(self.running.scope)
                .or_default()
                .ffi_calls += 1;
            self.changed_scopes.insert(self.running.scope);
        }
        let mut dependencies = Vec::new();
        match &operation {
            Blocked::Select(selection) => {
                for case in &selection.cases {
                    if let Data::ResourceRef(handle) = case.channel.data {
                        dependencies.push(handle);
                    }
                }
                let mut seen = HashSet::new();
                dependencies.retain(|handle| seen.insert(*handle));
            }
            Blocked::Mutex(handle) => dependencies.push(*handle),
            Blocked::Ffi(_) | Blocked::Module(_) => {}
        }
        let mut task = std::mem::take(&mut self.running);
        task.blocked = Some(operation);
        self.blocked.push(task, dependencies);
    }

    pub(super) fn resource(&self, handle: Handle) -> Result<ResourceRead, RuntimeError> {
        if let Some(channel) = self.channel_records.lock().unwrap().get(&handle).cloned() {
            return Ok(ResourceRead(Resource::Channel { channel }));
        }
        let value = self.heap.read_typed_value(handle)?;
        match value.data {
            Data::Resource(resource) => {
                let Resource::Channel { channel } = *resource else {
                    return Ok(ResourceRead(*resource));
                };
                let mut records = self.channel_records.lock().unwrap();
                let stable = records
                    .entry(handle)
                    .or_insert_with(|| channel.clone())
                    .clone();
                Ok(ResourceRead(Resource::Channel { channel: stable }))
            }
            _ => Err(RuntimeError::new(
                "type_error",
                "resource",
                "invalid resource handle",
            )),
        }
    }

    pub(super) fn store_resource(
        &mut self,
        handle: Handle,
        resource: Resource,
    ) -> Result<(), RuntimeError> {
        if let Resource::Channel { channel } = &resource {
            self.channel_records
                .lock()
                .unwrap()
                .insert(handle, channel.clone());
        }
        self.blocked.notify_resource(handle);
        let value = Value {
            typ: TypeIdentity::Any,
            data: Data::Resource(Box::new(resource)),
        };
        let bytes = self.prepared_allocation_bytes(&value)?;
        value.trace(&mut |handle| self.running.transient_roots.push(handle));
        self.prepare_heap_replacements(&[(handle, bytes)])?;
        self.heap
            .replace(handle, value, bytes)
            .map_err(|(error, _)| error)
    }

    pub(super) fn prepare_channel_record_update(
        &self,
        handle: Handle,
    ) -> Result<u64, RuntimeError> {
        self.heap.channel_record_version(handle)
    }

    pub(super) fn commit_channel_record_update(
        &mut self,
        handle: Handle,
        expected_version: u64,
        channel: &Arc<std::sync::Mutex<ManagedChannel>>,
    ) -> Result<(), RuntimeError> {
        let (head, len, closed) = {
            let channel = channel
                .lock()
                .map_err(|_| RuntimeError::new("runtime", "channel", "channel lock poisoned"))?;
            (channel.head(), channel.len(), channel.is_closed())
        };
        if self
            .heap
            .update_channel_record(handle, expected_version, head, len, closed)?
        {
            self.blocked.notify_resource(handle);
            Ok(())
        } else {
            Err(RuntimeError::new(
                "concurrent_update",
                "resource",
                "channel resource changed during operation",
            ))
        }
    }

    pub(super) fn prune_channel_records(&mut self) {
        let stale = {
            let records = self.channel_records.lock().unwrap();
            records
                .keys()
                .copied()
                .filter(|handle| !self.heap.is_live(*handle))
                .collect::<Vec<_>>()
        };
        if stale.is_empty() {
            return;
        }
        let mut records = self.channel_records.lock().unwrap();
        for handle in stale {
            if let Some(channel) = records.remove(&handle)
                && let Ok(mut channel) = channel.lock()
            {
                let _ = channel.destroy(&self.heap.storage_pool());
            }
        }
    }

    pub(super) fn destroy_channel_records(&mut self) {
        let records = std::mem::take(&mut *self.channel_records.lock().unwrap());
        for (_, channel) in records {
            if let Ok(mut channel) = channel.lock() {
                let _ = channel.destroy(&self.heap.storage_pool());
            }
        }
    }

    pub(super) fn resource_handle(value: &Value) -> Result<Handle, RuntimeError> {
        match value.data {
            Data::ResourceRef(handle) => Ok(handle),
            _ => Err(RuntimeError::new(
                "type_error",
                "resource",
                "expected resource reference",
            )),
        }
    }

    pub(super) fn channel_ready(
        &self,
        channel: &Value,
        sending: bool,
    ) -> Result<bool, RuntimeError> {
        if matches!(channel.data, Data::Nil) {
            return Ok(false);
        }
        let handle = Self::resource_handle(channel)?;
        let Resource::Channel { channel } = &*self.resource(handle)? else {
            return Err(RuntimeError::new(
                "type_error",
                "channel",
                "expected channel",
            ));
        };
        let channel = channel
            .lock()
            .map_err(|_| RuntimeError::new("runtime", "channel", "channel lock poisoned"))?;
        Ok(channel.is_closed()
            || if sending {
                channel.ready_to_send() || self.blocked.select_peer(handle, true).is_some()
            } else {
                !channel.is_empty() || self.blocked.select_peer(handle, false).is_some()
            })
    }

    pub(super) fn try_send(&mut self, channel: &Value, value: Value) -> Result<bool, RuntimeError> {
        let element = match self.element_type(&channel.typ) {
            Ok(element) => element,
            Err(_) => value.typ.clone(),
        };
        let value = self.coerce(value, &element)?;
        if matches!(channel.data, Data::Nil) {
            return Ok(false);
        }
        let handle = Self::resource_handle(channel)?;
        let resource = self.resource(handle)?.clone();
        let Resource::Channel { channel } = &resource else {
            return Err(RuntimeError::new("type_error", "send", "expected channel"));
        };
        if channel
            .lock()
            .map_err(|_| RuntimeError::new("runtime", "send", "channel lock poisoned"))?
            .is_closed()
        {
            return Err(RuntimeError::new("panic", "send", "send on closed channel"));
        }
        if let Some((key, index, _)) = self.blocked.select_peer(handle, true) {
            self.blocked.complete_selection(
                key,
                select::SelectOutcome {
                    index: index as i64,
                    value: Some(value),
                    received: true,
                    error: None,
                },
            );
            return Ok(true);
        }
        let full = {
            let channel = channel
                .lock()
                .map_err(|_| RuntimeError::new("runtime", "send", "channel lock poisoned"))?;
            channel.len() == channel.capacity()
        };
        if full {
            return Ok(false);
        }
        let payload = self.encode_channel_value(&value, &element)?;
        // Any aggregate encoding can publish a new structural type in Heap's
        // registry. Adopt it before the payload crosses the channel owner so
        // the next receive/coerce path uses the same type catalog.
        self.sync_heap_type_registry();
        self.heap.publish_payload(&payload)?;
        let record_version = self.prepare_channel_record_update(handle)?;
        let sent = channel
            .lock()
            .map_err(|_| RuntimeError::new("runtime", "send", "channel lock poisoned"))?
            .try_send_prepared(&self.heap.storage_pool(), &payload)?;
        if sent == SendResult::Full {
            return Ok(false);
        }
        self.commit_channel_record_update(handle, record_version, channel)?;
        Ok(true)
    }

    pub(super) fn try_receive(
        &mut self,
        channel: &Value,
    ) -> Result<Option<(Value, bool)>, RuntimeError> {
        if matches!(channel.data, Data::Nil) {
            return Ok(None);
        }
        let element_typ = channel.typ.clone();
        let handle = Self::resource_handle(channel)?;
        let resource = self.resource(handle)?.clone();
        let Resource::Channel { channel } = &resource else {
            return Err(RuntimeError::new(
                "type_error",
                "receive",
                "expected channel",
            ));
        };
        let payload = channel
            .lock()
            .map_err(|_| RuntimeError::new("runtime", "receive", "channel lock poisoned"))?
            .peek(&self.heap.storage_pool())?;
        if let Some(payload) = payload {
            let element = self.element_type(&element_typ)?;
            let value = self.decode_channel_value(&payload, &element)?;
            self.sync_heap_type_registry();
            let record_version = self.prepare_channel_record_update(handle)?;
            channel
                .lock()
                .map_err(|_| RuntimeError::new("runtime", "receive", "channel lock poisoned"))?
                .consume(&self.heap.storage_pool())?;
            value.trace(&mut |handle| self.running.transient_roots.push(handle));
            self.commit_channel_record_update(handle, record_version, channel)?;
            return Ok(Some((value, true)));
        }
        if channel
            .lock()
            .map_err(|_| RuntimeError::new("runtime", "receive", "channel lock poisoned"))?
            .is_closed()
        {
            let element = self.element_type(&element_typ)?;
            return Ok(Some((self.zero(&element)?, false)));
        }
        if let Some((key, index, value)) = self.blocked.select_peer(handle, false) {
            self.blocked.complete_selection(
                key,
                select::SelectOutcome {
                    index: index as i64,
                    value: None,
                    received: false,
                    error: None,
                },
            );
            return Ok(Some((value.unwrap(), true)));
        }
        Ok(None)
    }

    pub(super) fn choose_ready(&mut self, ready: &[usize]) -> i64 {
        if ready.is_empty() {
            return -1;
        }
        if ready.len() == 1 {
            return ready[0] as i64;
        }
        let mut state = if self.select_state == 0 {
            0x9e37_79b9_7f4a_7c15
        } else {
            self.select_state
        };
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        self.select_state = state;
        ready[state as usize % ready.len()] as i64
    }

    pub(super) fn resume_blocked(&mut self) -> Result<(), RuntimeError> {
        self.last_error_scope = None;
        for call in self.ffi_calls.take_ready_ids() {
            self.blocked.notify_call(call);
        }
        while let Some(index) = self.blocked.next_ready() {
            // Removing the task prevents a send from rendezvousing with itself.
            let waiting = self.blocked.remove(index);
            self.resuming_task = Some(waiting.task);
            let operation = self
                .resuming_task
                .as_ref()
                .unwrap()
                .blocked
                .clone()
                .unwrap();
            let outcome = (|| -> Result<bool, RuntimeError> {
                Ok(match &operation {
                    Blocked::Select(selection) => {
                        let mut selection = selection.clone();
                        if self.try_selection(&mut selection)? {
                            self.resuming_task.as_mut().unwrap().selection_completion =
                                Some(selection);
                            true
                        } else {
                            false
                        }
                    }
                    Blocked::Mutex(handle) => {
                        let mut resource = self.resource(*handle)?.clone();
                        let Resource::Mutex { locked, grant, .. } = &mut resource else {
                            unreachable!()
                        };
                        if *grant == Some(self.resuming_task.as_ref().unwrap().id) {
                            *grant = None;
                            *locked = true;
                            self.store_resource(*handle, resource)?;
                            true
                        } else {
                            false
                        }
                    }
                    Blocked::Module(module) => {
                        if let Some(error) = self.failed_initializations.get(module) {
                            return Err(error.clone());
                        }
                        !self.initializing.contains_key(module)
                    }
                    Blocked::Ffi(id) => {
                        if let Some((payload, ffi_error)) = self.ffi_calls.preview(*id) {
                            // Keep the completion in PendingCalls until guest
                            // materialization succeeds. If materialization
                            // fails, abort/cancel still owns the reply and its
                            // boundary reservation.
                            let values = match self
                                .materialize_ffi_result(&payload, ffi_error.as_ref())
                            {
                                Ok(values) => values,
                                Err(error) if error.code == "collection_required" => {
                                    // The waiting task remains in `resuming_task`, so
                                    // collect_rooted traces its frames and transient roots.
                                    // Retry the same host completion after one owner
                                    // collection; the reply is still in PendingCalls and
                                    // cannot be delivered twice.
                                    if let Err(error) = self.collect_rooted() {
                                        if error.code == "collection_required" {
                                            return Err(RuntimeError::new(
                                                "allocation_limit",
                                                "guest",
                                                "guest allocation byte limit exceeded",
                                            ));
                                        }
                                        return Err(error);
                                    }
                                    match self.materialize_ffi_result(&payload, ffi_error.as_ref())
                                    {
                                        Ok(values) => values,
                                        Err(error) if error.code == "collection_required" => {
                                            return Err(RuntimeError::new(
                                                "allocation_limit",
                                                "guest",
                                                "guest allocation byte limit exceeded",
                                            ));
                                        }
                                        Err(error) => return Err(error),
                                    }
                                }
                                Err(error) => return Err(error),
                            };
                            self.publish_ffi_results(values)?;
                            // The frame now owns every result. Until this
                            // point a storage failure must leave the reply
                            // and its boundary reservation with PendingCalls.
                            let reply = self.ffi_calls.take(*id).ok_or_else(|| {
                                RuntimeError::new("cancelled", "ffi", "reply was released")
                            })?;
                            reply.consume()?;
                            true
                        } else {
                            false
                        }
                    }
                })
            })();
            let mut task = self.resuming_task.take().unwrap();
            let ready = match outcome {
                Ok(ready) => ready,
                Err(error) if error.code == "panic" => {
                    let frame = task.frames.last_mut().unwrap();
                    if matches!(
                        frame.continuation,
                        Some(frame::Continuation::Intrinsic(
                            super::reflect_async::IntrinsicResume::Send
                        ))
                    ) {
                        frame.continuation = None;
                        frame.extend_results([
                            Value::string(error.message),
                            Value::boolean(false),
                        ])?;
                    } else {
                        // Convert host-side panic text to one managed payload
                        // before attaching it to the resumed task. Deferred
                        // recovery and GC then use the same stable handle
                        // path as an in-guest panic.
                        let panic = self.allocate_private(Value::string(error.message))?;
                        frame.returning = Some(0);
                        frame.panic = Some(panic);
                    }
                    true
                }
                Err(error) => {
                    self.last_error_scope = Some(task.scope);
                    task.blocked = Some(operation);
                    self.blocked
                        .insert(index, waiters::WaitingTask { task, ..waiting });
                    return Err(error);
                }
            };
            if ready {
                if matches!(operation, Blocked::Ffi(_)) {
                    self.scope_work.entry(task.scope).or_default().ffi_calls -= 1;
                    self.changed_scopes.insert(task.scope);
                }
                task.blocked = None;
                self.runnable.push_back(task);
            } else {
                task.blocked = Some(operation);
                self.blocked
                    .insert(index, waiters::WaitingTask { task, ..waiting });
            }
        }
        Ok(())
    }

    pub(super) fn execute_wait(
        &mut self,
        instruction: Instruction,
        module: &str,
    ) -> Result<(), RuntimeError> {
        use Instruction::*;
        let mut result = None;
        match instruction {
            Select(payload) => return self.execute_select(payload),
            CallFfi(_) => {
                let payload = self.running.pop()?;
                let route = self.running.pop()?;
                let Data::String(route) = route.data else {
                    return Err(RuntimeError::new(
                        "type_error",
                        "ffi",
                        "route must be a string",
                    ));
                };
                let route = String::from_utf8(route.to_vec())
                    .map_err(|_| RuntimeError::new("type_error", "ffi", "route is not UTF-8"))?;
                let payload = self.slice_bytes(&payload)?;
                let error = if let Some(session) = &self.ffi_session {
                    let request_bytes =
                        route.len().checked_add(payload.len()).ok_or_else(|| {
                            RuntimeError::new("boundary_limit", "ffi", "request size overflow")
                        })?;
                    let (id, cancellation, completion) = self
                        .ffi_calls
                        .reserve(request_bytes, self.limits.max_ffi_result_bytes)?;
                    let started = session.start(
                        cancellation,
                        crate::ffi::Request { route, payload },
                        completion,
                    );
                    match self.ffi_calls.started(id, started) {
                        Ok(()) => {
                            self.park(Blocked::Ffi(id));
                            return Ok(());
                        }
                        Err(error) => error,
                    }
                } else {
                    RuntimeError::new("route_unavailable", "ffi", "FFI route unavailable")
                };
                let values = self.materialize_ffi_result(&[], Some(&error))?;
                self.extend_results(values)?;
            }
            Spawn(payload) => {
                self.admit_task()?;
                let mut arguments = self
                    .running
                    .pop_values(&self.frame_pool, payload.arg_count as usize + 1)?;
                let value = arguments.remove(0);
                let Data::Function(callee) = value.data else {
                    return Err(RuntimeError::new(
                        "nil_function",
                        "spawn",
                        "expected callable",
                    ));
                };
                let revision = callee.revision.as_ref().unwrap_or(&self.revision);
                let result_count = self
                    .resolve_function_binding(&callee, revision)?
                    .declaration
                    .signature
                    .results
                    .len();
                let id = self.allocate_task_id()?;
                let scope = self.running.scope;
                self.preparing_task = Some(std::mem::replace(
                    &mut self.running,
                    Task {
                        id,
                        scope,
                        ..Task::default()
                    },
                ));
                let created = self.push_frame(
                    Arc::unwrap_or_clone(callee),
                    arguments,
                    result_count,
                    false,
                    crate::contract_generated::STORAGE_CONTROL_RECORD,
                );
                let child =
                    std::mem::replace(&mut self.running, self.preparing_task.take().unwrap());
                created?;
                self.scope_work.entry(self.running.scope).or_default().tasks += 1;
                self.changed_scopes.insert(self.running.scope);
                self.runnable.push_back(child);
                self.yield_task();
            }
            MakeWaitable(payload) => {
                let capacity = self.running.pop()?.integer()?;
                if capacity < 0 {
                    return Err(RuntimeError::new(
                        "value_limit",
                        "channel",
                        "invalid capacity",
                    ));
                }
                let typ = self.types.resolve(module, &payload.r#type)?;
                let element = self.element_type(&typ)?;
                let resource = self.new_channel_resource(&element, capacity as usize)?;
                let handle = self.allocate(Value {
                    typ: TypeIdentity::Any,
                    data: Data::Resource(Box::new(resource.clone())),
                })?;
                self.register_channel_resource(handle, &resource);
                result = Some(Value {
                    typ,
                    data: Data::ResourceRef(handle),
                });
            }
            WaitableSend | WaitableTrySend => {
                let blocking = matches!(instruction, WaitableSend);
                let value = self.running.pop()?;
                let channel = self.running.pop()?;
                if blocking {
                    self.wait_channel(channel, Some(value), false, None)?;
                } else {
                    result = Some(Value::boolean(self.try_send(&channel, value)?));
                }
            }
            WaitableRecv | WaitableRecvOk | WaitableTryRecv => {
                let with_ok = !matches!(instruction, WaitableRecv);
                let blocking = !matches!(instruction, WaitableTryRecv);
                let channel = self.running.pop()?;
                if blocking {
                    self.wait_channel(channel, None, with_ok, None)?;
                } else if let Some((value, ok)) = self.try_receive(&channel)? {
                    self.push_result(value)?;
                    if with_ok {
                        result = Some(Value::boolean(ok));
                    }
                } else {
                    let zero = self.zero(&self.element_type(&channel.typ)?)?;
                    self.push_result(zero)?;
                    result = Some(Value::boolean(false));
                }
            }
            WaitableCanRecv | WaitableCanSend => {
                let channel = self.running.pop()?;
                result = Some(Value::boolean(
                    self.channel_ready(&channel, matches!(instruction, WaitableCanSend))?,
                ));
            }
            WaitableClose => {
                let channel = self.running.pop()?;
                self.close_channel(&channel)?;
            }
            _ => unreachable!("wait dispatch only receives wait instructions"),
        }
        if let Some(value) = result {
            self.push_result(value)?;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ready_ffi_reply_survives_result_resident_failure() {
        use std::sync::atomic::{AtomicUsize, Ordering};

        struct Call(Arc<AtomicUsize>);
        impl crate::ffi::Call for Call {
            fn cancel(&self) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
        }

        for cancel in [false, true] {
            let program = super::super::test_helpers::program_with_artifact(|artifact| {
                artifact["functions"] = serde_json::json!([
                    {"id":"fn.Main", "code":super::super::test_helpers::slot_code(serde_json::json!([]), &[
                        ("return",serde_json::json!({}),serde_json::json!({}))
                    ])}
                ]);
            });
            let resident_limit = 4 << 20;
            let mut vm = Instance::new(
                program,
                ExecutionLimits {
                    max_resident_bytes: resident_limit,
                    ..Default::default()
                },
            )
            .unwrap();
            vm.start("default", &[]).unwrap();
            if vm.running.frames.is_empty() {
                assert!(vm.schedule_next());
            }
            vm.running.frames.last_mut().unwrap().continuation =
                Some(frame::Continuation::TailReturn(3));
            let canceled = Arc::new(AtomicUsize::new(0));
            let consumed = Arc::new(AtomicUsize::new(0));
            let discarded = Arc::new(AtomicUsize::new(0));
            let (id, _, completion) = vm.ffi_calls.reserve(7, 0).unwrap();
            vm.ffi_calls
                .started(id, Ok(Box::new(Call(canceled.clone()))))
                .unwrap();
            vm.park(Blocked::Ffi(id));
            let receipt = consumed.clone();
            let cleanup = discarded.clone();
            completion.complete(
                crate::ffi::Reply::new(
                    Vec::new(),
                    None,
                    Some(Box::new(move || {
                        cleanup.fetch_add(1, Ordering::SeqCst);
                    })),
                )
                .on_consumed(move || {
                    receipt.fetch_add(1, Ordering::SeqCst);
                }),
            );
            let remaining = resident_limit - vm.heap_stats().resident_page_bytes;
            let reservation = vm.heap.reserve_resident(remaining).unwrap();
            let error = vm.resume_blocked().unwrap_err();
            assert_eq!(error.code, "allocation_limit");
            assert_eq!(error.path, "managed_storage");
            assert_eq!(vm.ffi_calls.pending_count(), 1);
            assert_eq!(vm.ffi_calls.reserved_bytes(), 7);
            assert!(vm.ffi_calls.preview(id).is_some());
            assert_eq!(
                vm.blocked
                    .iter()
                    .next()
                    .unwrap()
                    .frames
                    .last()
                    .unwrap()
                    .delivery
                    .handles
                    .len(),
                0,
            );
            assert_eq!(consumed.load(Ordering::SeqCst), 0);
            assert_eq!(discarded.load(Ordering::SeqCst), 0);
            drop(reservation);

            if cancel {
                vm.cancel().unwrap();
            } else {
                vm.blocked.notify_call(id);
                vm.resume_blocked().unwrap();
                assert_eq!(vm.ffi_calls.pending_count(), 0);
                assert_eq!(vm.ffi_calls.reserved_bytes(), 0);
                let frame = vm.runnable.back().unwrap().frames.last().unwrap();
                assert_eq!(frame.delivery.handles.len(), 3);
                assert_eq!(consumed.load(Ordering::SeqCst), 1);
                assert_eq!(discarded.load(Ordering::SeqCst), 0);
            }
            vm.close().unwrap();
            assert_eq!(canceled.load(Ordering::SeqCst), usize::from(cancel));
            assert_eq!(discarded.load(Ordering::SeqCst), usize::from(cancel));
            assert_eq!(consumed.load(Ordering::SeqCst), usize::from(!cancel));
            assert_eq!(vm.ffi_calls.reserved_bytes(), 0);
        }
    }

    #[test]
    fn ffi_result_batch_rolls_back_when_a_later_handle_fails() {
        let program = super::super::test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":super::super::test_helpers::slot_code(serde_json::json!([]), &[
                    ("return",serde_json::json!({}),serde_json::json!({}))
                ])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        if vm.running.frames.is_empty() {
            assert!(vm.schedule_next());
        }
        vm.running.frames.last_mut().unwrap().continuation =
            Some(frame::Continuation::TailReturn(2));

        // Use a one-object heap only for the publication transaction. The
        // first value can be admitted, while the second deterministically
        // fails; prepare_allocations must roll the first one back as well.
        let heap = Heap::new(1, 1 << 20).unwrap();
        heap.set_type_registry(vm.types.clone());
        vm.heap = heap.clone();
        vm.running.frames.last_mut().unwrap().heap = heap.clone();
        vm.resuming_task = Some(std::mem::take(&mut vm.running));
        let error = vm
            .publish_ffi_results(vec![Value::int(1), Value::int(2)])
            .unwrap_err();
        assert_eq!(error.code, "allocation_limit");
        assert!(
            vm.resuming_task
                .as_ref()
                .unwrap()
                .frames
                .last()
                .unwrap()
                .delivery
                .handles
                .is_empty()
        );
        assert_eq!(heap.stats().live_objects, 0);
        vm.close().unwrap();
    }

    #[test]
    fn no_continuation_ffi_results_roll_back_when_a_later_slot_fails() {
        let integer = serde_json::json!({"kind":3,"primitive":3});
        let wide = serde_json::json!({"kind":6,"node":"wide"});
        let program = super::super::test_helpers::program_with_artifact(|artifact| {
            artifact["type_table"] = serde_json::json!({
                "nodes":[{"id":"wide","kind":6,"elem":integer,"length":64}]
            });
            artifact["functions"] = serde_json::json!([
                {
                    "id":"fn.Main",
                    "signature":{"results":[]},
                    "code":super::super::test_helpers::slot_code(
                        serde_json::json!([wide.clone(), wide.clone(), wide.clone(), wide.clone(), wide.clone()]),
                        &[
                            ("zero",serde_json::json!({"type":wide.clone()}),serde_json::json!({"outputs":[0]})),
                            ("zero",serde_json::json!({"type":wide.clone()}),serde_json::json!({"outputs":[1]})),
                            ("call_ffi",serde_json::json!({"arg_count":2,"result_count":3}),serde_json::json!({"inputs":[[0,0],[0,1]],"outputs":[2,3,4]})),
                            ("return",serde_json::json!({}),serde_json::json!({}))
                        ]
                    )
                }
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        if vm.running.frames.is_empty() {
            assert!(vm.schedule_next());
        }
        vm.running.frames.last_mut().unwrap().begin_slots(2);
        let heap = Heap::new(1, 1 << 20).unwrap();
        heap.set_type_registry(vm.types.clone());
        vm.heap = heap.clone();
        vm.running.frames.last_mut().unwrap().heap = heap.clone();
        vm.resuming_task = Some(std::mem::take(&mut vm.running));
        let typ = vm
            .resuming_task
            .as_ref()
            .unwrap()
            .frames
            .last()
            .unwrap()
            .prepared
            .slot_types[2]
            .clone();
        let value = || Value {
            typ: typ.clone(),
            data: Data::Array(Arc::new((0..64).map(Value::int).collect())),
        };
        let error = vm.publish_ffi_results(vec![value(), value()]).unwrap_err();
        assert_eq!(error.code, "allocation_limit");
        let frame = vm.resuming_task.as_ref().unwrap().frames.last().unwrap();
        assert_eq!(frame.slot_output, 0);
        assert!(frame.slot_handles.iter().all(Option::is_none));
        assert_eq!(heap.stats().live_objects, 0);
        vm.close().unwrap();
    }

    #[test]
    fn no_continuation_ffi_replacements_are_atomic_when_a_later_payload_is_invalid() {
        let integer = serde_json::json!({"kind":3,"primitive":3});
        let wide = serde_json::json!({"kind":6,"node":"wide"});
        let program = super::super::test_helpers::program_with_artifact(|artifact| {
            artifact["type_table"] = serde_json::json!({
                "nodes":[{"id":"wide","kind":6,"elem":integer,"length":64}]
            });
            artifact["functions"] = serde_json::json!([
                {
                    "id":"fn.Main",
                    "signature":{"results":[]},
                    "code":super::super::test_helpers::slot_code(
                        serde_json::json!([wide.clone(), wide.clone(), wide.clone(), wide.clone(), wide.clone()]),
                        &[
                            ("zero",serde_json::json!({"type":wide.clone()}),serde_json::json!({"outputs":[0]})),
                            ("zero",serde_json::json!({"type":wide.clone()}),serde_json::json!({"outputs":[1]})),
                            ("call_ffi",serde_json::json!({"arg_count":2,"result_count":3}),serde_json::json!({"inputs":[[0,0],[0,1]],"outputs":[2,3,4]})),
                            ("return",serde_json::json!({}),serde_json::json!({}))
                        ]
                    )
                }
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        if vm.running.frames.is_empty() {
            assert!(vm.schedule_next());
        }
        vm.running.frames.last_mut().unwrap().begin_slots(2);
        let heap = Heap::new(8, 1 << 20).unwrap();
        heap.set_type_registry(vm.types.clone());
        vm.heap = heap.clone();
        vm.running.frames.last_mut().unwrap().heap = heap.clone();
        vm.resuming_task = Some(std::mem::take(&mut vm.running));

        let (typ, original_first, original_second) = {
            let frame = vm
                .resuming_task
                .as_mut()
                .unwrap()
                .frames
                .last_mut()
                .unwrap();
            let typ = frame.prepared.slot_types[2].clone();
            let value = || Value {
                typ: typ.clone(),
                data: Data::Array(Arc::new((0..64).map(Value::int).collect())),
            };
            frame.put_slot_value(2, value()).unwrap();
            frame.put_slot_value(3, value()).unwrap();
            let first = frame.slot_handles[2].unwrap();
            let second = frame.slot_handles[3].unwrap();
            (typ, first, second)
        };
        let first_before = heap.read_typed_value(original_first).unwrap();
        let second_before = heap.read_typed_value(original_second).unwrap();
        let replacement = Value {
            typ: typ.clone(),
            data: Data::Array(Arc::new((100..164).map(Value::int).collect())),
        };
        let invalid = Value {
            typ,
            data: Data::String(b"invalid-wide-payload".to_vec().into()),
        };
        let error = vm
            .publish_ffi_results(vec![replacement, invalid])
            .unwrap_err();
        assert_eq!(error.code, "typed_payload_unsupported");
        let frame = vm.resuming_task.as_ref().unwrap().frames.last().unwrap();
        assert_eq!(frame.slot_output, 0);
        assert_eq!(frame.slot_handles[2], Some(original_first));
        assert_eq!(frame.slot_handles[3], Some(original_second));
        let first_after = heap.read_typed_value(original_first).unwrap();
        let second_after = heap.read_typed_value(original_second).unwrap();
        for (before, after) in [(first_before, first_after), (second_before, second_after)] {
            assert_eq!(after.data.aggregate_shape(), Some((true, 64)));
            for index in 0..64 {
                assert_eq!(
                    before.element(index).unwrap().integer().unwrap(),
                    after.element(index).unwrap().integer().unwrap()
                );
            }
        }
        vm.close().unwrap();
    }

    #[test]
    fn no_continuation_ffi_aggregate_failure_does_not_publish_nested_backing() {
        let string = serde_json::json!({"kind":3,"primitive":2});
        let program = super::super::test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {
                    "id":"fn.Main",
                    "signature":{"results":[]},
                    "code":super::super::test_helpers::slot_code(
                        serde_json::json!([string.clone(), string.clone(), string.clone(), string.clone(), string.clone()]),
                        &[
                            ("zero",serde_json::json!({"type":string.clone()}),serde_json::json!({"outputs":[0]})),
                            ("zero",serde_json::json!({"type":string.clone()}),serde_json::json!({"outputs":[1]})),
                            ("call_ffi",serde_json::json!({"arg_count":2,"result_count":3}),serde_json::json!({"inputs":[[0,0],[0,1]],"outputs":[2,3,4]})),
                            ("return",serde_json::json!({}),serde_json::json!({}))
                        ]
                    )
                }
            ]);
        });
        for (limit, second, code) in [
            (1 << 20, Value::int(2), "invalid_local"),
            // Each string needs a record header plus its byte backing:
            // 21 and 22 bytes respectively. Only the first fits in 32 bytes.
            (32, Value::string("second"), "collection_required"),
        ] {
            let mut vm = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
            vm.start("default", &[]).unwrap();
            if vm.running.frames.is_empty() {
                assert!(vm.schedule_next());
            }
            vm.running.frames.last_mut().unwrap().begin_slots(2);
            let heap = Heap::new(64, limit).unwrap();
            heap.set_type_registry(vm.types.clone());
            vm.heap = heap.clone();
            vm.running.frames.last_mut().unwrap().heap = heap.clone();
            vm.resuming_task = Some(std::mem::take(&mut vm.running));
            let before = heap.stats();
            let error = vm
                .publish_ffi_results(vec![Value::string("first"), second])
                .unwrap_err();
            assert_eq!(error.code, code);
            let frame = vm.resuming_task.as_ref().unwrap().frames.last().unwrap();
            assert_eq!(frame.slot_output, 0);
            assert!(frame.slot_handles.iter().all(Option::is_none));
            assert_eq!(heap.stats().live_objects, before.live_objects);
            assert_eq!(heap.stats().live_bytes, before.live_bytes);
            assert_eq!(heap.stats().reserved_bytes, before.reserved_bytes);
            vm.close().unwrap();
        }
    }

    #[test]
    fn channel_resource_traces_ring_as_a_managed_edge() {
        let ring = crate::managed_storage::StorageRef {
            slot: 17,
            generation: 3,
        };
        let pool =
            crate::managed_storage::StorageHeap::new(None, None, 64 * 1024).into_worker_pool();
        let layout = pool
            .register_layout(crate::managed_storage::StorageLayout {
                stride: 24,
                references: vec![],
                dynamics: vec![],
            })
            .unwrap();
        let resource =
            Resource::from_page_channel(&pool, layout, 24, 24, 2, ring, 0, 1, false).unwrap();
        let mut edges = Vec::new();
        resource.trace(&mut |handle| edges.push(handle));
        assert_eq!(edges, vec![ring]);
    }
}
