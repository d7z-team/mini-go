//! Bounded physical frame storage. The guest capacity ledger is independent.

use super::*;

pub(super) enum Local {
    Private(crate::heap::OwnedValue<Value>),
    Shared(Handle),
}

impl Local {
    pub(super) fn value<'a>(
        &'a self,
        heap: &Heap<Value>,
    ) -> Result<crate::value::ValueRead<'a>, RuntimeError> {
        match self {
            Self::Private(value) => Ok(crate::value::ValueRead::Borrowed(value.get())),
            Self::Shared(handle) => heap.get(*handle).map(crate::value::ValueRead::Shared),
        }
    }

    pub(super) fn address(&self) -> Result<Handle, RuntimeError> {
        match self {
            Self::Shared(handle) => Ok(*handle),
            Self::Private(_) => Err(RuntimeError::new(
                "invalid_address",
                "local",
                "local has no shared storage",
            )),
        }
    }
}

impl Trace for Local {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        match self {
            Self::Private(value) => value.get().trace(visit),
            Self::Shared(handle) => visit(*handle),
        }
    }
}

pub(super) enum Continuation {
    Intrinsic(reflect_async::IntrinsicResume),
    TailReturn(usize),
}

pub(super) enum FrameArguments {
    Owned(Vec<Value>),
    Caller {
        frame: usize,
        suspended: bool,
        start: usize,
        count: usize,
    },
}

// A suspended operation owns delivered values until its owner resumes it.
// They are independent of bytecode operands and remain GC roots on the frame.
#[derive(Default)]
pub(super) struct ResultBuffer {
    pub values: Vec<Value>,
    pub reading: bool,
}

pub(super) struct Frame {
    pub(super) prepared: Arc<crate::program::PreparedFunction>,
    pub(super) memory: memory::GuestFrameAccounting,
    pub(super) revision: Arc<crate::program::Revision>,
    pub(super) module: Arc<str>,
    pub(super) function: Arc<str>,
    pub(super) pc: usize,
    pub(super) type_dispatch_index: usize,
    pub(super) type_dispatch_value: Option<Value>,
    pub(super) locals: Vec<Local>,
    pub(super) globals: Vec<Handle>,
    pub(super) upvalues: Vec<Address>,
    pub(super) slot_values: Vec<Option<Value>>,
    pub(super) slot_constants: Vec<(usize, Value)>,
    pub(super) slot_pc: Option<usize>,
    pub(super) slot_input: usize,
    pub(super) slot_output: usize,
    pub(super) delivery: ResultBuffer,
    pub(super) map_iterators: HashMap<String, MapIterator>,
    pub(super) popped_roots: memory::OperandRoots,
    // Borrowing removes host copies, but the logical operand batch remains
    // part of the guest census until its instruction commits or retries.
    pub(super) operand_census: Option<std::ops::Range<usize>>,
    pub(super) expected_results: usize,
    pub(super) initializing: bool,
    pub(super) defers: Vec<FunctionValue>,
    pub(super) returning: Option<Vec<Value>>,
    pub(super) panic: Option<Arc<Value>>,
    pub(super) recovered: Option<Arc<Value>>,
    pub(super) deferred: bool,
    pub(super) continuation: Option<Continuation>,
    pub(super) after_init: Option<usize>,
}

pub(super) struct MapIterator {
    pub object: Value,
    pub entries: Vec<u64>,
    pub position: usize,
}

impl Trace for Frame {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        for iterator in self.map_iterators.values() {
            iterator.object.trace(visit);
        }
        for local in &self.locals {
            local.trace(visit);
        }
        for address in &self.upvalues {
            visit(address.root);
        }
        for value in &self.delivery.values {
            value.trace(visit);
        }
        for value in self.slot_values.iter().flatten() {
            value.trace(visit);
        }
        for (_, value) in &self.slot_constants {
            value.trace(visit);
        }
        if let Some(value) = &self.type_dispatch_value {
            value.trace(visit);
        }
        self.popped_roots.trace(visit);
        if let Some(Continuation::Intrinsic(resume)) = &self.continuation {
            resume.trace(visit);
        }
        for value in self.returning.iter().flatten() {
            value.trace(visit);
        }
        for function in &self.defers {
            for address in &function.captures {
                visit(address.root);
            }
        }
        if let Some(value) = &self.panic {
            value.trace(visit);
        }
        if let Some(value) = &self.recovered {
            value.trace(visit);
        }
    }
}

#[derive(Default)]
pub(super) struct FramePool {
    state: std::sync::Mutex<FramePoolState>,
}

#[derive(Default)]
struct FramePoolState {
    frames: HashMap<(u64, usize), Vec<FrameStorage>>,
    operands: Vec<Vec<Value>>,
    bytes: usize,
}

#[derive(Default)]
pub(super) struct FrameStorage {
    pub locals: Vec<Local>,
    pub globals: Vec<Handle>,
    pub delivery: Vec<Value>,
    pub slot_values: Vec<Option<Value>>,
    pub popped: memory::OperandRoots,
    pub defers: Vec<FunctionValue>,
}

impl FrameStorage {
    fn bytes(&self) -> usize {
        self.locals.capacity() * size_of::<Local>()
            + self.globals.capacity() * size_of::<Handle>()
            + self.delivery.capacity() * size_of::<Value>()
            + self.slot_values.capacity() * size_of::<Option<Value>>()
            + self.popped.storage_bytes()
            + self.defers.capacity() * size_of::<FunctionValue>()
    }
}

impl FramePool {
    pub fn take_operands(&self, count: usize) -> Vec<Value> {
        let mut state = self.state.lock().unwrap();
        if let Some(index) = state
            .operands
            .iter()
            .position(|values| values.capacity() >= count)
        {
            let values = state.operands.swap_remove(index);
            state.bytes -= values.capacity() * size_of::<Value>();
            values
        } else {
            drop(state);
            Vec::with_capacity(count)
        }
    }

    pub fn recycle_operands(&self, mut values: Vec<Value>, limit: usize) {
        values.clear();
        let bytes = values.capacity() * size_of::<Value>();
        let mut state = self.state.lock().unwrap();
        if bytes != 0 && state.operands.len() < 8 && bytes <= limit.saturating_sub(state.bytes) {
            state.bytes += bytes;
            state.operands.push(values);
        }
    }

    pub fn take(&self, generation: u64, function: usize) -> FrameStorage {
        let key = (generation, function);
        let mut state = self.state.lock().unwrap();
        let storage = state
            .frames
            .get_mut(&key)
            .and_then(Vec::pop)
            .unwrap_or_default();
        state.bytes -= storage.bytes();
        storage
    }

    fn recycle(&self, generation: u64, function: usize, storage: FrameStorage, limit: usize) {
        let bytes = storage.bytes();
        let key = (generation, function);
        let mut state = self.state.lock().unwrap();
        if bytes <= limit.saturating_sub(state.bytes)
            && state.frames.get(&key).map_or(0, Vec::len) < 8
        {
            state.frames.entry(key).or_default().push(storage);
            state.bytes += bytes;
        }
    }

    pub fn clear(&self) {
        let previous = std::mem::take(&mut *self.state.lock().unwrap());
        drop(previous);
    }
}

impl Trace for FramePool {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        let mut roots = Vec::new();
        let state = self.state.lock().unwrap();
        for storage in state.frames.values().flatten() {
            for local in &storage.locals {
                if let Local::Private(value) = local {
                    value.get().trace(&mut |handle| roots.push(handle));
                }
            }
        }
        drop(state);
        for handle in roots {
            visit(handle);
        }
    }
}

impl Instance {
    pub(super) fn load_local(&mut self, index: usize) -> Result<Value, RuntimeError> {
        let local = &self.running.frames.last().unwrap().locals[index];
        if let Local::Shared(handle) = local {
            return self.load_slot(*handle);
        }
        let value = local.value(&self.heap)?;
        if matches!(value.data, Data::Uninitialized) {
            let value = self.zero(&value.typ, 0)?;
            self.store_local(index, value, false)?;
        }
        Ok(self.running.frames.last().unwrap().locals[index]
            .value(&self.heap)?
            .clone())
    }

    pub(super) fn store_local(
        &mut self,
        index: usize,
        value: Value,
        rebind: bool,
    ) -> Result<(), RuntimeError> {
        if let Some(handle) = self.shared_local_target(index, rebind)? {
            return self.store_slot(handle, value);
        }
        let local = &self.running.frames.last().unwrap().locals[index];
        let typ = local.value(&self.heap)?.typ.clone();
        let value = self.coerce(value, &typ)?;
        let bytes = value.logical_bytes()?.checked_add(128).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "local", "logical size overflow")
        })?;
        value.trace(&mut |handle| self.running.transient_roots.push(handle));
        let Local::Private(slot) = &mut self.running.frames.last_mut().unwrap().locals[index]
        else {
            unreachable!()
        };
        if let Err((_, value)) = slot.replace(value, bytes) {
            self.frame_pool.clear();
            self.collect_rooted()?;
            let Local::Private(slot) = &mut self.running.frames.last_mut().unwrap().locals[index]
            else {
                unreachable!()
            };
            slot.replace(value, bytes).map_err(|(error, _)| error)?;
        }
        Ok(())
    }

    pub(super) fn shared_local_target(
        &mut self,
        index: usize,
        rebind: bool,
    ) -> Result<Option<Handle>, RuntimeError> {
        let Local::Shared(handle) = &self.running.frames.last().unwrap().locals[index] else {
            return Ok(None);
        };
        if !rebind {
            return Ok(Some(*handle));
        }
        let typ = self.heap.get(*handle)?.typ.clone();
        let handle = self.allocate(self.zero(&typ, 0)?)?;
        self.running.frames.last_mut().unwrap().locals[index] = Local::Shared(handle);
        Ok(Some(handle))
    }

    pub(super) fn cache_frame(&mut self, frame: &mut Frame) -> Result<(), RuntimeError> {
        if frame.revision.generation != self.revision.generation
            || self.limits.max_frame_cache_bytes == 0
        {
            return Ok(());
        }
        for local in &mut frame.locals {
            if let Local::Private(slot) = local {
                if matches!(slot.get().data, Data::Uninitialized) {
                    continue;
                }
                let typ = slot.get().typ.clone();
                let value = Value {
                    typ,
                    data: Data::Uninitialized,
                };
                let bytes = value.logical_bytes()? + 128;
                slot.replace(value, bytes).map_err(|(error, _)| error)?;
            }
        }
        frame.delivery.values.clear();
        frame.slot_values.fill(None);
        frame.slot_constants.clear();
        frame.type_dispatch_value = None;
        frame.popped_roots.clear();
        frame.defers.clear();
        let storage = FrameStorage {
            locals: std::mem::take(&mut frame.locals),
            globals: std::mem::take(&mut frame.globals),
            delivery: std::mem::take(&mut frame.delivery.values),
            slot_values: std::mem::take(&mut frame.slot_values),
            popped: std::mem::take(&mut frame.popped_roots),
            defers: std::mem::take(&mut frame.defers),
        };
        self.frame_pool.recycle(
            frame.revision.generation,
            frame.prepared.index,
            storage,
            self.limits.max_frame_cache_bytes,
        );
        Ok(())
    }
}

impl Instance {
    pub(super) fn call_with_continuation(
        &mut self,
        callee: FunctionValue,
        arguments: Vec<Value>,
        expected_results: usize,
        continuation: Continuation,
    ) -> Result<(), RuntimeError> {
        let caller = self.running.frames.len() - 1;
        self.push_frame(callee, arguments, expected_results, false)?;
        // Frame preparation may fail or request a census. Publish the resume
        // state only once a callee owns the call, so retry reads the operands.
        self.running.frames[caller].continuation = Some(continuation);
        Ok(())
    }

    pub(super) fn push_frame(
        &mut self,
        callee: FunctionValue,
        arguments: Vec<Value>,
        expected_results: usize,
        initializing: bool,
    ) -> Result<(), RuntimeError> {
        self.push_frame_arguments(
            callee,
            FrameArguments::Owned(arguments),
            expected_results,
            initializing,
        )
    }

    pub(super) fn push_frame_arguments(
        &mut self,
        callee: FunctionValue,
        mut arguments: FrameArguments,
        expected_results: usize,
        initializing: bool,
    ) -> Result<(), RuntimeError> {
        self.running
            .transient_roots
            .extend(callee.captures.iter().map(|address| address.root));
        let argument_count = match &arguments {
            FrameArguments::Owned(values) => {
                for value in values {
                    value.trace(&mut |handle| self.running.transient_roots.push(handle));
                }
                values.len()
            }
            FrameArguments::Caller { count, .. } => *count,
        };
        if self.running.frames.len() >= self.limits.max_frames {
            return Err(RuntimeError::new(
                "frame_limit",
                &callee.function,
                "frame budget exhausted",
            ));
        }
        let revision = callee
            .revision
            .clone()
            .unwrap_or_else(|| self.revision.clone());
        self.types.use_context(revision.program.decoded.types());
        let function = match callee.index {
            Some(index) if callee.revision.is_some() => {
                revision.program.function_table[index].clone()
            }
            _ => revision
                .program
                .function(&callee.module, &callee.function)?
                .clone(),
        };
        if argument_count != function.declaration.signature.params.len()
            || expected_results != function.declaration.signature.results.len()
            || callee.captures.len() != function.upvalues.len()
        {
            return Err(RuntimeError::new(
                "invalid_call",
                &callee.function,
                "argument, result or capture count mismatch",
            ));
        }
        let mut storage = self.frame_pool.take(revision.generation, function.index);
        for local in &storage.locals {
            if let Local::Private(value) = local {
                value
                    .get()
                    .trace(&mut |handle| self.running.transient_roots.push(handle));
            }
        }
        storage.locals.reserve(
            function
                .local_types
                .len()
                .saturating_sub(storage.locals.len()),
        );
        for (index, typ) in function.local_types.iter().enumerate() {
            // Recycled private locals already hold this function's typed,
            // uninitialized value and its live allocation charge. Only
            // parameters and shared cells need preparation on every call.
            if index >= argument_count
                && !function.addressable_locals[index]
                && matches!(storage.locals.get(index), Some(Local::Private(slot)) if matches!(slot.get().data, Data::Uninitialized))
            {
                continue;
            }
            let mut reserved_bytes = None;
            let value = if index >= argument_count {
                Value {
                    typ: typ.clone(),
                    data: Data::Uninitialized,
                }
            } else {
                match &mut arguments {
                    FrameArguments::Owned(values) => {
                        std::mem::replace(&mut values[index], Value::int(0))
                    }
                    FrameArguments::Caller {
                        frame,
                        suspended,
                        start,
                        ..
                    } => {
                        let caller = if *suspended {
                            &self.running.suspended_frames[*frame]
                        } else {
                            &self.running.frames[*frame]
                        };
                        if !function.addressable_locals[index]
                            && caller.transferable_input(*start + index, typ).is_some()
                        {
                            let value = caller
                                .input_at(caller.slot_pc.unwrap(), *start + index)
                                .unwrap();
                            if let Data::String(bytes) = &value.data {
                                self.check_string_size(bytes.len())?;
                            }
                            reserved_bytes =
                                Some(value.logical_bytes()?.checked_add(128).ok_or_else(|| {
                                    RuntimeError::new(
                                        "allocation_limit",
                                        "local",
                                        "logical size overflow",
                                    )
                                })?);
                            Value {
                                typ: typ.clone(),
                                data: Data::Uninitialized,
                            }
                        } else {
                            caller.input_value(*start + index)?
                        }
                    }
                }
            };
            let value = self.coerce(value, typ)?;
            let local = if function.addressable_locals[index] {
                Local::Shared(self.allocate(value)?)
            } else if let Some(Local::Private(slot)) = storage.locals.get_mut(index) {
                value.trace(&mut |handle| self.running.transient_roots.push(handle));
                let bytes = match reserved_bytes {
                    Some(bytes) => bytes,
                    None => value.logical_bytes()? + 128,
                };
                if let Err((_, value)) = slot.replace(value, bytes) {
                    self.frame_pool.clear();
                    self.collect_rooted()?;
                    slot.replace(value, bytes).map_err(|(error, _)| error)?;
                }
                continue;
            } else {
                Local::Private(match reserved_bytes {
                    Some(bytes) => self.allocate_private_storage(value, bytes)?,
                    None => self.allocate_private(value)?,
                })
            };
            if index == storage.locals.len() {
                storage.locals.push(local);
            } else {
                storage.locals[index] = local;
            }
        }
        storage
            .slot_values
            .resize_with(function.slot_types.len(), || None);
        if storage.globals.is_empty() {
            storage.globals.extend(
                function
                    .globals
                    .iter()
                    .map(|id| self.globals[&(callee.module.to_string(), id.clone())]),
            );
        }
        let result_slots = if self.running.frames.is_empty() {
            0
        } else {
            expected_results
        };
        let base_slots = storage.locals.len() + callee.captures.len() + storage.slot_values.len();
        let memory =
            match self
                .memory
                .take_frame(revision.generation, function.module_index, function.index)
            {
                Some(memory) => memory,
                None => {
                    self.charge_guest(128 + (base_slots + result_slots) as u64 * 16)?;
                    memory::GuestFrameAccounting {
                        base_slots,
                        ..Default::default()
                    }
                }
            };
        // All fallible preparation is complete. A failed allocation or census
        // above leaves caller operands intact; only this commit consumes them.
        match arguments {
            FrameArguments::Owned(values) => self
                .frame_pool
                .recycle_operands(values, self.limits.max_frame_cache_bytes),
            FrameArguments::Caller {
                frame,
                suspended,
                start,
                count,
            } => {
                let caller = if suspended {
                    &mut self.running.suspended_frames[frame]
                } else {
                    &mut self.running.frames[frame]
                };
                for index in 0..count {
                    if !function.addressable_locals[index]
                        && let Some(source) =
                            caller.transferable_input(start + index, &function.local_types[index])
                    {
                        let value = caller.slot_values[source as usize].take().unwrap();
                        let Local::Private(local) = &mut storage.locals[index] else {
                            unreachable!()
                        };
                        local.commit_reserved(value);
                    }
                }
                caller.slot_input = 0;
            }
        }
        self.running.frames.push(Frame {
            globals: storage.globals,
            prepared: function,
            memory,
            revision,
            module: callee.module,
            function: callee.function,
            pc: 0,
            type_dispatch_index: 0,
            type_dispatch_value: None,
            locals: storage.locals,
            upvalues: callee.captures,
            slot_values: storage.slot_values,
            slot_constants: Vec::new(),
            slot_pc: None,
            slot_input: 0,
            slot_output: 0,
            delivery: ResultBuffer {
                values: storage.delivery,
                reading: false,
            },
            map_iterators: HashMap::new(),
            popped_roots: storage.popped,
            operand_census: None,
            expected_results,
            initializing,
            defers: storage.defers,
            returning: None,
            panic: None,
            recovered: None,
            deferred: false,
            continuation: None,
            after_init: None,
        });
        Ok(())
    }

    pub(super) fn finish_frame(&mut self) -> Result<(), RuntimeError> {
        let frame = self.running.frames.last_mut().unwrap();
        if let Some(callee) = frame.defers.pop() {
            let index = self.running.frames.len();
            self.push_frame(callee, Vec::new(), 0, false)?;
            self.running.frames[index].deferred = true;
            return Ok(());
        }
        let function = &frame.prepared;
        if frame.panic.is_none() && !function.result_locals.is_empty() {
            let values = function
                .result_locals
                .iter()
                .map(|&index| {
                    let value = frame.locals[index].value(&self.heap)?;
                    if matches!(value.data, Data::Uninitialized) {
                        let mut remaining = self.limits.max_heap_bytes;
                        Value::zero_with_budget(
                            &self.types,
                            &value.typ,
                            0,
                            self.limits.max_value_depth,
                            self.limits.max_sequence_elements,
                            &mut remaining,
                        )
                    } else {
                        Ok(value.clone())
                    }
                })
                .collect::<Result<Vec<_>, _>>()?;
            frame.memory.returned =
                memory::grow_frame_buffer(frame.memory.returned, values.len(), false)?;
            frame.returning = Some(values);
        }
        let mut frame = self.running.frames.pop().unwrap();
        let mut values = frame.returning.take().unwrap_or_default();
        let panic = frame.panic.clone();
        let recovered = frame.recovered.clone();
        let (deferred, initializing, expected_results) =
            (frame.deferred, frame.initializing, frame.expected_results);
        let module = frame.module.clone();
        let function_id = frame.function.clone();
        let task_id = self.running.id;
        let completion_index = self.running.suspended_frames.len();
        self.running.suspended_frames.push(frame);
        let outcome = (|| {
            if let Some(panic) = panic {
                if let Some(owner) = self.running.frames.last_mut() {
                    owner.returning = Some(Vec::new());
                    owner.panic = Some(panic);
                    return Ok(());
                }
                return Err(RuntimeError::new("panic", "guest", panic.to_string()));
            }
            if deferred {
                let owner = self.running.frames.last().ok_or_else(|| {
                    RuntimeError::new("invalid_defer", "frame", "deferred frame lost owner")
                })?;
                if recovered
                    .as_ref()
                    .zip(owner.panic.as_ref())
                    .is_some_and(|(recovered, panic)| Arc::ptr_eq(recovered, panic))
                {
                    let function = owner
                        .revision
                        .program
                        .function(&owner.module, &owner.function)?;
                    let values = function
                        .declaration
                        .signature
                        .results
                        .iter()
                        .map(|typ| self.types.resolve(&owner.module, typ))
                        .collect::<Result<Vec<_>, _>>()?
                        .iter()
                        .map(|typ| self.zero(typ, 0))
                        .collect::<Result<Vec<_>, _>>()?;
                    let owner = self.running.frames.last_mut().unwrap();
                    owner.panic = None;
                    owner.returning = Some(values);
                }
                return Ok(());
            }
            if values.len() != expected_results {
                return Err(RuntimeError::new(
                    "invalid_return",
                    &function_id,
                    "result count mismatch",
                ));
            }
            if initializing {
                self.initializing.remove(module.as_ref());
                self.initialized.insert(module.to_string());
                self.blocked.notify_module(module.as_ref());
            }
            if let Some(caller) = self.running.frames.last_mut() {
                caller.extend_results(values.drain(..));
                self.frame_pool
                    .recycle_operands(values, self.limits.max_frame_cache_bytes);
            } else if self.foreground == Some(self.running.id) {
                self.results = values;
                self.foreground = None;
            }
            if self.running.frames.is_empty() {
                self.scope_work.entry(self.running.scope).or_default().tasks -= 1;
                self.changed_scopes.insert(self.running.scope);
            }
            if initializing
                && self
                    .running
                    .frames
                    .last()
                    .is_some_and(|caller| caller.after_init.is_some())
            {
                // Complete the suspended owner action after initialization, without
                // executing or charging its bytecode instruction a second time. Any
                // allocation pressure now belongs to the caller continuation rather
                // than the initializer's return instruction.
                let pc = self.running.frames.last().unwrap().after_init.unwrap();
                self.running.begin_instruction(pc);
                return self.step();
            }
            Ok(())
        })();
        let task = std::iter::once(&mut self.running)
            .chain(self.preparing_task.iter_mut())
            .chain(self.resuming_task.iter_mut())
            .chain(self.runnable.iter_mut())
            .chain(self.blocked.iter_mut())
            .find(|task| task.id == task_id)
            .expect("completion retains its task");
        if outcome
            .as_ref()
            .is_err_and(|error| error.code == "census_required")
        {
            task.census_completed_frame = Some(completion_index);
            return outcome;
        }
        let mut frame = task.suspended_frames.remove(completion_index);
        self.cache_frame(&mut frame)?;
        self.memory.recycle_frame_storage(
            frame.revision.generation,
            frame.prepared.module_index,
            frame.prepared.index,
            frame.memory,
        );
        outcome
    }

    pub(super) fn begin_return(&mut self, values: Vec<Value>) -> Result<(), RuntimeError> {
        let frame = self.running.frames.last().unwrap();
        let function = &frame.prepared;
        if values.len() != function.result_types.len() {
            return Err(RuntimeError::new(
                "invalid_return",
                &frame.function,
                "result count mismatch",
            ));
        }
        let values = values
            .into_iter()
            .zip(function.result_types.iter())
            .map(|(value, typ)| self.coerce(value, typ))
            .collect::<Result<Vec<_>, _>>()?;
        let frame = self.running.frames.last_mut().unwrap();
        frame.memory.returned =
            memory::grow_frame_buffer(frame.memory.returned, values.len(), false)?;
        frame.returning = Some(values);
        self.finish_frame()
    }

    pub(super) fn return_slot_values(&mut self, count: usize) -> Result<(), RuntimeError> {
        let frame = self.running.frames.last().unwrap();
        let function = frame.prepared.clone();
        if count != function.result_types.len() {
            return Err(RuntimeError::new(
                "invalid_return",
                &frame.function,
                "result count mismatch",
            ));
        }
        let start = frame
            .slot_input
            .checked_sub(count)
            .ok_or_else(|| RuntimeError::new("invalid_operand", "frame", "input underflow"))?;
        let mut values = self.frame_pool.take_operands(count);
        for (index, typ) in function.result_types.iter().enumerate() {
            if frame.transferable_input(start + index, typ).is_some() {
                let value = frame
                    .input_at(frame.slot_pc.unwrap(), start + index)
                    .unwrap();
                if let Data::String(bytes) = &value.data {
                    self.check_string_size(bytes.len())?;
                }
                values.push(Value::int(0));
            } else {
                values.push(self.coerce(frame.input_value(start + index)?, typ)?);
            }
        }
        let popped = memory::grow_frame_buffer(frame.memory.popped, count, false)?;
        let returned = memory::grow_frame_buffer(frame.memory.returned, count, false)?;
        let frame = self.running.frames.last_mut().unwrap();
        for (index, typ) in function.result_types.iter().enumerate() {
            if let Some(source) = frame.transferable_input(start + index, typ) {
                values[index] = frame.slot_values[source as usize].take().unwrap();
            }
        }
        frame.memory.popped = popped;
        frame.memory.returned = returned;
        frame.slot_input = start;
        // The return state owns transferred values before defer/recover or a
        // suspended frame completion can run, including on a census retry.
        frame.returning = Some(values);
        self.finish_frame()
    }
}

impl scheduler::Task {
    pub(super) fn begin_instruction(&mut self, pc: usize) {
        self.instruction_active = true;
        self.instruction_pc = pc;
        self.census_request = None;
    }

    pub(super) fn rewind_instruction(&mut self) {
        let frame = self.frames.last_mut().unwrap();
        frame.pc = self.instruction_pc;
        frame.popped_roots.clear();
        self.instruction_active = false;
        self.census_request = None;
    }

    pub(super) fn finish_instruction(&mut self) {
        self.instruction_active = false;
        self.census_request = None;
    }

    pub(super) fn pop(&mut self) -> Result<Value, RuntimeError> {
        if let Some(frame) = self.frames.last_mut()
            && !frame.delivery.reading
        {
            frame.slot_input = frame
                .slot_input
                .checked_sub(1)
                .ok_or_else(|| RuntimeError::new("invalid_operand", "frame", "input underflow"))?;
            // Slot inputs and materialized constants remain frame roots until
            // the instruction completes; its disjoint outputs cannot destroy
            // retry operands. No second root buffer is needed here.
            return frame.input_value(frame.slot_input);
        }
        let frame = self
            .frames
            .last_mut()
            .ok_or_else(|| RuntimeError::new("invalid_operand", "frame", "missing result frame"))?;
        frame.memory.popped = memory::grow_frame_buffer(frame.memory.popped, 1, false)?;
        let value = frame.delivery.values.pop().ok_or_else(|| {
            RuntimeError::new("invalid_operand", "frame", "missing continuation result")
        })?;
        value.trace(&mut |handle| self.transient_roots.push(handle));
        Ok(value)
    }

    pub(super) fn pop_values(
        &mut self,
        pool: &frame::FramePool,
        count: usize,
    ) -> Result<Vec<Value>, RuntimeError> {
        let mut values = pool.take_operands(count);
        self.popped_frame = Some(self.frames.len() - 1);
        let frame = self.frames.last_mut().unwrap();
        frame.memory.popped = memory::grow_frame_buffer(frame.memory.popped, count, false)?;
        if !frame.delivery.reading {
            let start = frame
                .slot_input
                .checked_sub(count)
                .ok_or_else(|| RuntimeError::new("invalid_operand", "frame", "input underflow"))?;
            frame.operand_census = Some(start..frame.slot_input);
            for index in start..frame.slot_input {
                values.push(frame.input_value(index)?);
            }
            frame.slot_input = start;
            return Ok(values);
        } else {
            let results = &mut frame.delivery.values;
            let start = results.len().checked_sub(count).ok_or_else(|| {
                RuntimeError::new("invalid_operand", "frame", "missing continuation results")
            })?;
            values.extend(results.drain(start..));
        }
        frame.popped_roots.capture(&values);
        for value in &values {
            value.trace(&mut |handle| self.transient_roots.push(handle));
        }
        Ok(values)
    }
}

#[cfg(test)]
mod pool_tests {
    use super::*;

    #[test]
    fn call_parameter_transfer_commits_after_preflight_and_preserves_duplicate_inputs() {
        use serde_json::json;
        let contract: serde_json::Value = serde_json::from_str(wire::CONTRACT_JSON).unwrap();
        let operation = |name: &str, descriptor: usize, operands: usize| {
            let opcode = contract["spec"]["opcodes"]
                .as_array()
                .unwrap()
                .iter()
                .position(|entry| entry["op"] == name)
                .unwrap()
                + 1;
            json!([opcode, descriptor, operands])
        };
        for (count, suspended) in [(1, false), (2, false), (16, false), (1, true), (2, true)] {
            let program = test_helpers::program_with_artifact(|artifact| {
                let string = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveString});
                artifact["functions"] = json!([
                    {"id":"fn.Main","code":{
                        "types":[string],
                        "descriptors":{"type":[{"type":string}],"call":[{"module_path":artifact["module"]["path"],"function":"callee","arg_count":count}],"return":[{}]},
                        "instructions":[operation("zero",0,0),operation("call_direct",0,1),operation("return",0,2)],
                        "operands":[{"outputs":[0]},{"inputs":vec![json!([0,0]);count],"release":[0]},{}]
                    }},
                    {"id":"callee","signature":{"params":vec![json!({"type":string});count]},
                        "locals":(0..count).map(|index|json!({"id":format!("p{index}"),"type":string})).collect::<Vec<_>>(),
                        "code":{"descriptors":{}}}
                ]);
            });
            let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
            vm.start("default", vec![]).unwrap();
            assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Running);
            let backing: Arc<[u8]> = Arc::from(&b"owned argument"[..]);
            let caller = &mut vm.running.frames[0];
            caller.slot_values[0] = Some(Value {
                typ: TypeIdentity::Primitive(wire::PrimitiveString),
                data: Data::String(backing.clone().into()),
            });
            caller.begin_slots(1);
            caller.pc = 2;
            let callee = FunctionValue {
                module: caller.module.clone(),
                function: "callee".into(),
                index: None,
                revision: None,
                captures: vec![],
            };
            vm.running.begin_instruction(1);
            if suspended {
                vm.running
                    .suspended_frames
                    .push(vm.running.frames.pop().unwrap());
            }
            let limit = vm.limits.max_allocated_bytes;
            vm.limits.max_allocated_bytes = vm.memory.stats().live_bytes;
            let heap_before = vm.heap_stats().live_bytes;
            let error = vm
                .push_frame_arguments(
                    callee.clone(),
                    FrameArguments::Caller {
                        frame: 0,
                        suspended,
                        start: 0,
                        count,
                    },
                    0,
                    false,
                )
                .unwrap_err();
            assert_eq!(error.code, "census_required");
            assert_eq!(vm.running.frames.len(), usize::from(!suspended));
            assert_eq!(vm.heap_stats().live_bytes, heap_before);
            assert_eq!(Arc::strong_count(&backing), 2);
            let caller = if suspended {
                &vm.running.suspended_frames[0]
            } else {
                &vm.running.frames[0]
            };
            assert!(matches!(&caller.slot_values[0].as_ref().unwrap().data,
                Data::String(bytes) if **bytes == *b"owned argument"));
            vm.running.finish_instruction();
            vm.limits.max_allocated_bytes = limit;
            vm.push_frame_arguments(
                callee,
                FrameArguments::Caller {
                    frame: 0,
                    suspended,
                    start: 0,
                    count,
                },
                0,
                false,
            )
            .unwrap();
            for local in &vm.running.frames.last().unwrap().locals {
                assert!(matches!(&local.value(&vm.heap).unwrap().data,
                    Data::String(bytes) if **bytes == *b"owned argument"));
            }
            let caller = if suspended {
                &vm.running.suspended_frames[0]
            } else {
                &vm.running.frames[0]
            };
            assert_eq!(caller.slot_values[0].is_none(), count == 1);
            assert_eq!(
                Arc::strong_count(&backing),
                if count == 1 { 2 } else { count + 2 }
            );
            if !suspended {
                assert_eq!(vm.poll_steps(10).unwrap(), PollStatus::Ready);
            }
            vm.close().unwrap();
            assert_eq!(Arc::strong_count(&backing), 1);
        }
    }

    #[test]
    fn continuation_is_published_only_after_callee_preparation() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":{"descriptors":{}}},
                {"id":"callee", "locals":[{"id":"value","type":{"kind":3,"primitive":3}}], "code":{"descriptors":{}}}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", vec![]).unwrap();
        let callee = FunctionValue {
            module: vm.running.frames[0].module.clone(),
            function: "callee".into(),
            index: None,
            revision: None,
            captures: vec![],
        };
        let frame_limit = vm.limits.max_frames;
        vm.limits.max_frames = 1;
        assert_eq!(
            vm.call_with_continuation(callee.clone(), vec![], 0, Continuation::TailReturn(0))
                .unwrap_err()
                .code,
            "frame_limit"
        );
        assert_eq!(vm.running.frames.len(), 1);
        assert!(vm.running.frames[0].continuation.is_none());
        vm.limits.max_frames = frame_limit;

        let allocation_limit = vm.limits.max_allocated_bytes;
        vm.limits.max_allocated_bytes = vm.memory.stats().live_bytes;
        vm.running.begin_instruction(0);
        let before = vm.heap_stats().live_bytes;
        assert_eq!(
            vm.call_with_continuation(callee.clone(), vec![], 0, Continuation::TailReturn(0))
                .unwrap_err()
                .code,
            "census_required"
        );
        assert_eq!(vm.running.frames.len(), 1);
        assert!(vm.running.frames[0].continuation.is_none());
        assert_eq!(vm.heap_stats().live_bytes, before);
        vm.running.finish_instruction();
        vm.limits.max_allocated_bytes = allocation_limit;

        vm.call_with_continuation(callee, vec![], 0, Continuation::TailReturn(0))
            .unwrap();
        assert_eq!(vm.running.frames.len(), 2);
        assert!(matches!(
            vm.running.frames[0].continuation,
            Some(Continuation::TailReturn(0))
        ));
        assert!(vm.running.frames[1].continuation.is_none());
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
        assert_eq!(vm.steps(), 0);
        vm.close().unwrap();
    }

    #[test]
    fn concurrent_frame_and_operand_reuse_preserves_ownership_and_capacity() {
        let pool = FramePool::default();
        let heap = Heap::new(128, 128 * 1024).unwrap();
        let limit = 16 * 1024;
        std::thread::scope(|scope| {
            for worker in 0..4 {
                let pool = &pool;
                let heap = &heap;
                scope.spawn(move || {
                    for iteration in 0..200 {
                        let mut storage = pool.take(1, worker);
                        if storage.locals.is_empty() {
                            storage
                                .locals
                                .push(Local::Private(heap.own(Value::int(0), 128).unwrap()));
                        }
                        let Local::Private(local) = &mut storage.locals[0] else {
                            unreachable!()
                        };
                        assert_eq!(local.get().integer().unwrap(), 0);
                        let marker = (worker * 200 + iteration + 1) as i64;
                        local.replace(Value::int(marker), 128).unwrap();
                        let mut operands = pool.take_operands(4);
                        assert!(operands.is_empty());
                        operands.push(Value::int(marker));
                        assert_eq!(
                            local.get().integer().unwrap(),
                            operands[0].integer().unwrap()
                        );
                        local.replace(Value::int(0), 128).unwrap();
                        pool.recycle(1, worker, storage, limit);
                        pool.recycle_operands(operands, limit);
                    }
                });
            }
        });
        let state = pool.state.lock().unwrap();
        let retained = state
            .frames
            .values()
            .flatten()
            .map(FrameStorage::bytes)
            .sum::<usize>()
            + state
                .operands
                .iter()
                .map(|values| values.capacity() * size_of::<Value>())
                .sum::<usize>();
        assert_eq!(state.bytes, retained);
        assert!(retained <= limit);
        drop(state);
        pool.clear();
        assert_eq!(heap.stats().live_objects, 0);
        assert_eq!(heap.stats().live_bytes, 0);
    }

    #[test]
    fn frame_pool_visitors_run_after_releasing_storage_lock() {
        let pool = FramePool::default();
        let heap = Heap::new(4, 1024).unwrap();
        let root = heap.allocate(Value::int(42), 128).unwrap();
        let pointer = Value {
            typ: TypeIdentity::Any,
            data: Data::Pointer(std::sync::Arc::new(Address {
                identity: Arc::default(),
                root,
                path: Vec::new(),
            })),
        };
        let storage = FrameStorage {
            locals: vec![Local::Private(heap.own(pointer, 128).unwrap())],
            ..FrameStorage::default()
        };
        pool.recycle(1, 0, storage, 1024);
        let mut roots = Vec::new();
        pool.trace(&mut |handle| {
            pool.clear();
            roots.push(handle);
        });
        assert_eq!(roots, vec![root]);
        assert_eq!(heap.stats().live_objects, 1);
    }
}
