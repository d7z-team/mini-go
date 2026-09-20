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

pub(super) struct Frame {
    pub(super) prepared: Arc<crate::program::PreparedFunction>,
    pub(super) memory: memory::GuestFrameAccounting,
    pub(super) revision: Arc<crate::program::Revision>,
    pub(super) module: Arc<str>,
    pub(super) function: Arc<str>,
    pub(super) pc: usize,
    pub(super) locals: Vec<Local>,
    pub(super) globals: Vec<Handle>,
    pub(super) upvalues: Vec<Address>,
    pub(super) stack: Vec<Value>,
    pub(super) map_iterators: HashMap<String, MapIterator>,
    pub(super) popped_roots: memory::OperandRoots,
    pub(super) expected_results: usize,
    pub(super) initializing: bool,
    pub(super) defers: Vec<FunctionValue>,
    pub(super) returning: Option<Vec<Value>>,
    pub(super) panic: Option<Arc<Value>>,
    pub(super) recovered: Option<Arc<Value>>,
    pub(super) deferred: bool,
    pub(super) resume: Option<reflect_async::IntrinsicResume>,
    pub(super) tail_return: Option<usize>,
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
        for value in &self.stack {
            value.trace(visit);
        }
        self.popped_roots.trace(visit);
        if let Some(resume) = &self.resume {
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
    pub stack: Vec<Value>,
    pub popped: memory::OperandRoots,
    pub defers: Vec<FunctionValue>,
}

impl FrameStorage {
    fn bytes(&self) -> usize {
        self.locals.capacity() * size_of::<Local>()
            + self.globals.capacity() * size_of::<Handle>()
            + self.stack.capacity() * size_of::<Value>()
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
                let typ = slot.get().typ.clone();
                let value = Value {
                    typ,
                    data: Data::Uninitialized,
                };
                let bytes = value.logical_bytes()? + 128;
                slot.replace(value, bytes).map_err(|(error, _)| error)?;
            }
        }
        frame.stack.clear();
        frame.popped_roots.clear();
        frame.defers.clear();
        let storage = FrameStorage {
            locals: std::mem::take(&mut frame.locals),
            globals: std::mem::take(&mut frame.globals),
            stack: std::mem::take(&mut frame.stack),
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
    pub(super) fn push_frame(
        &mut self,
        callee: FunctionValue,
        mut arguments: Vec<Value>,
        expected_results: usize,
        initializing: bool,
    ) -> Result<(), RuntimeError> {
        self.running
            .transient_roots
            .extend(callee.captures.iter().map(|address| address.root));
        for value in &arguments {
            value.trace(&mut |handle| self.running.transient_roots.push(handle));
        }
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
        if arguments.len() != function.declaration.signature.params.len()
            || expected_results != function.declaration.signature.results.len()
            || callee.captures.len() != function.upvalues.len()
        {
            return Err(RuntimeError::new(
                "invalid_call",
                &callee.function,
                "argument, result or capture count mismatch",
            ));
        }
        let stack_limit = function.stack_limit;
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
        let mut incoming = arguments.drain(..);
        for (index, typ) in function.local_types.iter().enumerate() {
            let value = match incoming.next() {
                Some(value) => value,
                None => Value {
                    typ: typ.clone(),
                    data: Data::Uninitialized,
                },
            };
            let value = self.coerce(value, typ)?;
            let local = if function.addressable_locals[index] {
                Local::Shared(self.allocate(value)?)
            } else if let Some(Local::Private(slot)) = storage.locals.get_mut(index) {
                value.trace(&mut |handle| self.running.transient_roots.push(handle));
                let bytes = value.logical_bytes()? + 128;
                if let Err((_, value)) = slot.replace(value, bytes) {
                    self.frame_pool.clear();
                    self.collect_rooted()?;
                    slot.replace(value, bytes).map_err(|(error, _)| error)?;
                }
                continue;
            } else {
                Local::Private(self.allocate_private(value)?)
            };
            if index == storage.locals.len() {
                storage.locals.push(local);
            } else {
                storage.locals[index] = local;
            }
        }
        drop(incoming);
        self.frame_pool
            .recycle_operands(arguments, self.limits.max_frame_cache_bytes);
        storage.stack.reserve(stack_limit);
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
        let base_slots = storage.locals.len() + callee.captures.len() + stack_limit;
        let memory =
            match self
                .memory
                .take_frame(revision.generation, function.module_index, function.index)
            {
                Some(memory) => memory,
                None => {
                    let storage = memory::GuestFrameAccounting {
                        base_slots,
                        ..Default::default()
                    };
                    if let Err(error) =
                        self.charge_guest(128 + (base_slots + result_slots) as u64 * 16)
                    {
                        self.memory.recycle_frame_storage(
                            revision.generation,
                            function.module_index,
                            function.index,
                            storage,
                        );
                        return Err(error);
                    }
                    storage
                }
            };
        self.running.frames.push(Frame {
            globals: storage.globals,
            prepared: function,
            memory,
            revision,
            module: callee.module,
            function: callee.function,
            pc: 0,
            locals: storage.locals,
            upvalues: callee.captures,
            stack: storage.stack,
            map_iterators: HashMap::new(),
            popped_roots: storage.popped,
            expected_results,
            initializing,
            defers: storage.defers,
            returning: None,
            panic: None,
            recovered: None,
            deferred: false,
            resume: None,
            tail_return: None,
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
        if frame.panic.is_none() && !function.declaration.result_locals.is_empty() {
            let values = function
                .declaration
                .result_locals
                .iter()
                .map(|id| {
                    let value = frame.locals[function.locals[id]].value(&self.heap)?;
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
                caller.stack.append(&mut values);
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
}

impl scheduler::Task {
    pub(super) fn begin_instruction(&mut self, pc: usize) {
        self.instruction_active = true;
        self.instruction_pc = pc;
        self.instruction_stack_len = self.frames.last().unwrap().stack.len();
        self.retry_operands.clear();
        self.census_request = None;
    }

    pub(super) fn restore_instruction(&mut self) {
        let popped = self.retry_operands.len();
        let prefix = self.instruction_stack_len.saturating_sub(popped);
        let frame = self.frames.last_mut().unwrap();
        frame.stack.truncate(prefix);
        frame.stack.extend(self.retry_operands.drain(..).rev());
        frame.pc = self.instruction_pc;
        frame.popped_roots.clear();
        self.instruction_active = false;
        self.census_request = None;
    }

    pub(super) fn suspend_instruction(&mut self) {
        let prefix = self
            .instruction_stack_len
            .saturating_sub(self.retry_operands.len());
        let frame = self.frames.last_mut().unwrap();
        frame.stack.truncate(prefix);
        frame.pc = self.instruction_pc;
        frame.popped_roots.clear();
        self.instruction_active = false;
        self.census_request = None;
    }

    pub(super) fn finish_instruction(&mut self) {
        self.instruction_active = false;
        self.retry_operands.clear();
        self.census_request = None;
    }

    pub(super) fn pop(&mut self) -> Result<Value, RuntimeError> {
        let count = if self.instruction_active {
            self.retry_operands.len() + 1
        } else {
            1
        };
        let frame = self.frames.last_mut().ok_or_else(|| {
            RuntimeError::new("invalid_stack", "frame", "operand stack underflow")
        })?;
        frame.memory.popped = memory::grow_frame_buffer(frame.memory.popped, count, false)?;
        let value = self
            .frames
            .last_mut()
            .and_then(|frame| frame.stack.pop())
            .ok_or_else(|| {
                RuntimeError::new("invalid_stack", "frame", "operand stack underflow")
            })?;
        if self.instruction_active {
            self.retry_operands.push(value.clone());
        }
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
        let stack = &mut frame.stack;
        let start = stack.len().checked_sub(count).ok_or_else(|| {
            RuntimeError::new("invalid_stack", "frame", "operand stack underflow")
        })?;
        values.extend(stack.drain(start..));
        if self.instruction_active {
            self.retry_operands.extend(values.iter().rev().cloned());
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
            data: Data::Pointer(Address {
                identity: Arc::default(),
                root,
                path: Vec::new(),
            }),
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
