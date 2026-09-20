//! Cooperative tasks and wait registrations, mutated only by the instance owner.

use super::*;

pub(super) struct ResourceRead(Arc<Value>);

impl std::ops::Deref for ResourceRead {
    type Target = Resource;
    fn deref(&self) -> &Resource {
        let Data::Resource(resource) = &self.0.data else {
            unreachable!()
        };
        resource
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
    // An allocating instruction may have to stop at the owner boundary for a
    // complete guest-memory census. Keep its exact operands until the owner
    // either rejects the allocation or resumes the same PC without charging
    // another bytecode step.
    pub instruction_active: bool,
    pub instruction_pc: usize,
    pub instruction_stack_len: usize,
    pub retry_instruction: bool,
    pub retry_operands: Vec<Value>,
    pub census_request: Option<u64>,
    pub pending_error: Option<RuntimeError>,
}

impl Trace for Task {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        if let Some(write) = &self.pending_write {
            write.trace(visit);
        }
        if let Some(selection) = &self.selection_completion {
            selection.trace(visit);
        }
        for frame in self.frames.iter().chain(&self.suspended_frames) {
            frame.trace(visit);
        }
        for handle in &self.transient_roots {
            visit(*handle);
        }
        for value in &self.allocation_roots {
            value.trace(visit);
        }
        for value in &self.retry_operands {
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
        capacity: usize,
        closed: bool,
        values: VecDeque<Value>,
    },
}

impl Resource {
    pub(crate) fn logical_bytes(&self) -> u64 {
        128 + match self {
            Self::Mutex { grant, waiters, .. } => {
                (waiters.len() as u64 + u64::from(grant.is_some())) * 144
            }
            Self::Channel { capacity, .. } => *capacity as u64 * 16,
        }
    }
}

impl Trace for Resource {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        match self {
            Self::Mutex { .. } => {}
            Self::Channel { values, .. } => {
                for value in values {
                    value.trace(visit);
                }
            }
        }
    }
}

impl Instance {
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
        let mut resource = self.resource(handle)?.clone();
        let Resource::Channel { closed, .. } = &mut resource else {
            return Err(RuntimeError::new("type_error", "close", "expected channel"));
        };
        if *closed {
            return Err(RuntimeError::new(
                "panic",
                "close",
                "close of closed channel",
            ));
        }
        *closed = true;
        self.store_resource(handle, resource)
    }

    fn ffi_values(&mut self, reply: crate::ffi::Reply) -> Result<Vec<Value>, RuntimeError> {
        let (mut message, mut status) = match reply.error() {
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
        let imported = self
            .charge_guest(128 + reply.payload().len() as u64)
            .and_then(|()| {
                self.make_bytes(
                    typ.clone(),
                    reply.payload().len(),
                    reply.payload().len(),
                    reply.payload(),
                )
            });
        let value = match imported {
            Ok(value) => value,
            Err(error) => {
                message = error.message;
                status = 2;
                Value {
                    typ,
                    data: Data::Nil,
                }
            }
        };
        if status == 0 {
            reply.consume()?;
        }
        Ok(vec![
            value,
            Value {
                typ: TypeIdentity::Primitive(wire::PrimitiveString),
                data: Data::String(message.into_bytes().into()),
            },
            Value::int(status),
        ])
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
        let value = self.heap.get(handle)?;
        match &value.data {
            Data::Resource(_) => Ok(ResourceRead(value)),
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
        self.blocked.notify_resource(handle);
        let value = Value {
            typ: TypeIdentity::Any,
            data: Data::Resource(Box::new(resource)),
        };
        let bytes = value.logical_bytes()? + 128;
        value.trace(&mut |handle| self.running.transient_roots.push(handle));
        self.prepare_heap_replacements(&[(handle, bytes)])?;
        self.heap
            .replace(handle, value, bytes)
            .map_err(|(error, _)| error)
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
        let Resource::Channel {
            capacity,
            closed,
            values,
            ..
        } = &*self.resource(handle)?
        else {
            return Err(RuntimeError::new(
                "type_error",
                "channel",
                "expected channel",
            ));
        };
        Ok(*closed
            || if sending {
                values.len() < *capacity || self.blocked.select_peer(handle, true).is_some()
            } else {
                !values.is_empty() || self.blocked.select_peer(handle, false).is_some()
            })
    }

    pub(super) fn try_send(&mut self, channel: &Value, value: Value) -> Result<bool, RuntimeError> {
        let value = self.coerce(value, &self.element_type(&channel.typ)?)?;
        if matches!(channel.data, Data::Nil) {
            return Ok(false);
        }
        let handle = Self::resource_handle(channel)?;
        let mut resource = self.resource(handle)?.clone();
        let Resource::Channel {
            capacity,
            closed,
            values,
            ..
        } = &mut resource
        else {
            return Err(RuntimeError::new("type_error", "send", "expected channel"));
        };
        if *closed {
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
        if values.len() >= *capacity {
            return Ok(false);
        }
        values.push_back(value);
        self.store_resource(handle, resource)?;
        Ok(true)
    }

    pub(super) fn try_receive(
        &mut self,
        channel: &Value,
    ) -> Result<Option<(Value, bool)>, RuntimeError> {
        if matches!(channel.data, Data::Nil) {
            return Ok(None);
        }
        let handle = Self::resource_handle(channel)?;
        let mut resource = self.resource(handle)?.clone();
        let Resource::Channel { closed, values, .. } = &mut resource else {
            return Err(RuntimeError::new(
                "type_error",
                "receive",
                "expected channel",
            ));
        };
        if let Some(value) = values.pop_front() {
            value.trace(&mut |handle| self.running.transient_roots.push(handle));
            self.store_resource(handle, resource)?;
            return Ok(Some((value, true)));
        }
        if *closed {
            return Ok(Some((
                self.zero(&self.element_type(&channel.typ)?, 0)?,
                false,
            )));
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
            0x9e3779b97f4a7c15
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
                        if let Some(reply) = self.ffi_calls.take(*id) {
                            let values = self.ffi_values(reply)?;
                            self.resuming_task
                                .as_mut()
                                .unwrap()
                                .frames
                                .last_mut()
                                .unwrap()
                                .stack
                                .extend(values);
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
                        frame.resume,
                        Some(super::reflect_async::IntrinsicResume::Send)
                    ) {
                        frame.resume = None;
                        frame
                            .stack
                            .extend([Value::string(error.message), Value::boolean(false)]);
                    } else {
                        frame.returning = Some(Vec::new());
                        frame.panic = Some(Arc::new(Value {
                            typ: TypeIdentity::Primitive(wire::PrimitiveString),
                            data: Data::String(error.message.into_bytes().into()),
                        }));
                    }
                    true
                }
                Err(error) => {
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
                let values =
                    self.ffi_values(crate::ffi::Reply::new(Vec::new(), Some(error), None))?;
                self.running.frames.last_mut().unwrap().stack.extend(values);
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
                let created =
                    self.push_frame(callee, arguments, payload.result_count as usize, false);
                let child =
                    std::mem::replace(&mut self.running, self.preparing_task.take().unwrap());
                created?;
                self.charge_guest(128)?;
                self.scope_work.entry(self.running.scope).or_default().tasks += 1;
                self.changed_scopes.insert(self.running.scope);
                self.runnable.push_back(child);
                self.yield_task();
            }
            MakeWaitable(payload) => {
                let capacity = self.running.pop()?.integer()?;
                if capacity < 0 || capacity as u64 > self.limits.max_sequence_elements as u64 {
                    return Err(RuntimeError::new(
                        "value_limit",
                        "channel",
                        "invalid capacity",
                    ));
                }
                self.charge_guest_object(capacity as usize, 0)?;
                let typ = self.types.resolve(module, &payload.r#type)?;
                let resource = Resource::Channel {
                    capacity: capacity as usize,
                    closed: false,
                    values: VecDeque::new(),
                };
                let handle = self.allocate(Value {
                    typ: TypeIdentity::Any,
                    data: Data::Resource(Box::new(resource)),
                })?;
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
                    self.wait_channel(channel, Some(value), false)?;
                } else {
                    result = Some(Value::boolean(self.try_send(&channel, value)?));
                }
            }
            WaitableRecv | WaitableRecvOk | WaitableTryRecv => {
                let with_ok = !matches!(instruction, WaitableRecv);
                let blocking = !matches!(instruction, WaitableTryRecv);
                let channel = self.running.pop()?;
                if blocking {
                    self.wait_channel(channel, None, with_ok)?;
                } else if let Some((value, ok)) = self.try_receive(&channel)? {
                    self.running.frames.last_mut().unwrap().stack.push(value);
                    if with_ok {
                        result = Some(Value::boolean(ok));
                    }
                } else {
                    let zero = self.zero(&self.element_type(&channel.typ)?, 0)?;
                    self.running.frames.last_mut().unwrap().stack.push(zero);
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
            self.running.frames.last_mut().unwrap().stack.push(value);
        }
        Ok(())
    }
}
