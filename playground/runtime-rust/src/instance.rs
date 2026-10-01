//! Owner-coordinated execution. Native workers own task-private continuations;
//! shared state and control transactions remain with the instance owner.
//! Polling executes an exact bounded number of guest instructions.

use crate::{
    contract_generated as wire,
    error::RuntimeError,
    ffi::{Bridge, Cancellation, PendingCalls, Session, Wake},
    heap::{Handle, Heap, HeapStats, Trace},
    program::{Instruction, Program},
    types::{TypeIdentity, TypeRegistry},
    value::{Address, Data, FunctionValue, PathElement, SliceValue, Value},
};
use std::{
    collections::{BTreeMap, HashMap, HashSet, VecDeque},
    sync::Arc,
};

mod address;
mod budget;
mod bytes;
mod collection;
mod conversion;
pub mod debug;
mod debug_variables;
mod frame;
mod gc;
use frame::Frame;
#[cfg(test)]
mod budget_tests;
mod host;
#[cfg(test)]
mod initialization_tests;
mod intrinsic;
pub mod memory;
mod mutation;
mod mutex;
pub mod patch;
mod reflect;
mod reflect_async;
mod reflect_method;
mod reflect_value;
pub mod scheduler;
mod select;
mod slots;
pub mod stats;
pub(crate) mod task_runner;
#[cfg(test)]
pub(crate) mod test_helpers;
mod timer;
mod waiters;

/// Disables the cumulative instruction budget while retaining poll quanta.
pub const UNLIMITED_STEPS: i64 = -1;

// Cooperative owner checks share one cadence, independent of native batch size.
const TASK_POLL_INTERVAL: u8 = 64;

#[derive(Clone, Copy, Debug)]
pub struct ExecutionLimits {
    pub max_frames: usize,
    /// Physical idle frame storage, independent of guest allocation accounting.
    pub max_frame_cache_bytes: usize,
    /// Includes the current revision and the candidate being published.
    pub max_retained_revisions: usize,
    pub max_dynamic_types: usize,
    pub max_dynamic_type_bytes: u64,
    pub max_objects: usize,
    pub max_heap_bytes: u64,
    pub max_allocated_bytes: u64,
    pub max_string_bytes: usize,
    /// Scope instruction budget: 0 uses the default, -1 is unlimited.
    pub max_steps: i64,
    pub max_value_depth: usize,
    pub max_sequence_elements: usize,
    pub max_tasks: usize,
    pub max_pending_calls: usize,
    pub max_ffi_bytes: usize,
    pub max_ffi_result_bytes: usize,
}

impl Default for ExecutionLimits {
    fn default() -> Self {
        Self {
            max_frames: 1024,
            max_frame_cache_bytes: 8 << 20,
            max_retained_revisions: 128,
            max_dynamic_types: 4096,
            max_dynamic_type_bytes: 16 << 20,
            max_objects: 100_000,
            max_heap_bytes: 64 << 20,
            max_allocated_bytes: 8 << 30,
            max_string_bytes: 64 << 20,
            max_steps: 100_000_000,
            max_value_depth: 128,
            max_sequence_elements: 1_000_000,
            max_tasks: 4096,
            max_pending_calls: 65_536,
            max_ffi_bytes: 64 << 20,
            max_ffi_result_bytes: 64 << 20,
        }
    }
}

impl ExecutionLimits {
    /// Validate and resolve the step budget before allocating host resources.
    pub fn normalize(mut self) -> Result<Self, RuntimeError> {
        if self.max_steps < UNLIMITED_STEPS {
            return Err(RuntimeError::new(
                "invalid_limits",
                "max_steps",
                "expected -1, zero or a positive step limit",
            ));
        }
        if self.max_steps == 0 {
            self.max_steps = Self::default().max_steps;
        }
        Ok(self)
    }
    /// Resource envelope for a persistent, precompiled compiler guest.
    pub fn compiler() -> Self {
        Self {
            max_heap_bytes: 128 << 20,
            max_objects: 500_000,
            max_sequence_elements: 4 << 20,
            max_dynamic_types: 16_384,
            max_dynamic_type_bytes: 64 << 20,
            max_ffi_bytes: 64 << 20,
            max_ffi_result_bytes: 64 << 20,
            ..Self::default()
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PollStatus {
    Ready,
    Running,
    Pending,
    Paused,
}

// Stable logical identities are instance-owned; shared Programs never bind to
// one instance's current revision. IDs are never reused while old code can live.
type LogicalFunctions = HashMap<(Arc<str>, Arc<str>), usize>;
const MAX_LOGICAL_FUNCTIONS: usize = 1 << 20;

pub struct Instance {
    revision: Arc<crate::program::Revision>,
    types: crate::types::TypeRegistry,
    heap: Arc<Heap<Value>>,
    memory: Arc<memory::GuestMemory>,
    globals: BTreeMap<(String, String), Handle>,
    constant_values: BTreeMap<(u64, usize), Value>,
    logical_functions: LogicalFunctions,
    call_targets: Vec<Option<usize>>,
    running: scheduler::Task,
    frame_pool: Arc<frame::FramePool>,
    preparing_task: Option<scheduler::Task>,
    initialized: HashSet<String>,
    initializing: HashMap<String, u64>,
    failed_initializations: BTreeMap<String, RuntimeError>,
    root_initialization: Option<u64>,
    cleanup: Option<crate::ffi::Shutdown<'static>>,
    cleanup_result: Option<Result<(), RuntimeError>>,
    results: Vec<Value>,
    collection_roots: Vec<Handle>,
    collect_after_bytes: u64,
    collect_after_objects: usize,
    limits: ExecutionLimits,
    steps: u64,
    pub(crate) last_poll_steps: usize,
    scope_steps: BTreeMap<u64, Arc<budget::StepBudget>>,
    scope_work: BTreeMap<u64, stats::ScopeWork>,
    pub(crate) changed_scopes: std::collections::BTreeSet<u64>,
    closed: bool,
    faulted: bool,
    next_task: u64,
    foreground: Option<u64>,
    runnable: VecDeque<scheduler::Task>,
    blocked: waiters::Waiters,
    resuming_task: Option<scheduler::Task>,
    select_state: u64,
    ffi_session: Option<Box<dyn Session>>,
    ffi_calls: PendingCalls,
    wake: Arc<Wake>,
    lifetime: Cancellation,
    clock: Arc<dyn crate::environment::Clock>,
    entropy: Arc<dyn crate::environment::Entropy>,
    timers: timer::TimerQueue,
    timer_check_steps: usize,
    reflected_types: Vec<(TypeIdentity, Value)>,
    reflected_type_indices: HashMap<TypeIdentity, usize>,
    reflected_type_keys: HashMap<String, usize>,
    // Method-set validation is immutable within a published revision. Keep only
    // successful assignments, with a fixed bound on this owner-local memo.
    interface_assignments: std::sync::Mutex<HashSet<(TypeIdentity, TypeIdentity)>>,
    scope_cancellations: BTreeMap<u64, crate::ffi::CancellationListener>,
    cancellation_events: Arc<crate::ffi::CancellationEvents>,
    pub(crate) debug: debug::DebugState,
    host_capabilities: HashSet<String>,
    retired_revisions: BTreeMap<u64, std::sync::Weak<crate::program::Revision>>,
}

impl Drop for Instance {
    fn drop(&mut self) {
        #[cfg(not(target_arch = "wasm32"))]
        let _ = self.close();
        #[cfg(target_arch = "wasm32")]
        self.begin_close();
    }
}

impl Instance {
    pub fn new(program: Arc<Program>, limits: ExecutionLimits) -> Result<Self, RuntimeError> {
        if !program.image().capabilities.is_empty() {
            return Err(RuntimeError::new(
                "capability_unavailable",
                "instance",
                "program requires host capabilities",
            ));
        }
        Self::create(program, limits)
    }

    fn create(program: Arc<Program>, limits: ExecutionLimits) -> Result<Self, RuntimeError> {
        let limits = limits.normalize()?;
        let wake = Arc::new(Wake::default());
        let bindings = patch::FunctionBindings::prepare(&program, &LogicalFunctions::new())?;
        let mut instance = Self {
            types: program.decoded.types().clone(),
            revision: Arc::new(crate::program::Revision {
                generation: 1,
                logical_functions: bindings.ids,
                program,
            }),
            heap: Arc::new(Heap::new(limits.max_objects, limits.max_heap_bytes)?),
            memory: Arc::default(),
            globals: BTreeMap::new(),
            constant_values: BTreeMap::new(),
            logical_functions: bindings.symbols,
            call_targets: bindings.targets,
            running: scheduler::Task::default(),
            frame_pool: Arc::default(),
            preparing_task: None,
            initialized: HashSet::new(),
            initializing: HashMap::new(),
            failed_initializations: BTreeMap::new(),
            root_initialization: None,
            cleanup: None,
            cleanup_result: None,
            results: Vec::new(),
            collection_roots: Vec::new(),
            collect_after_bytes: (256 << 10).min(limits.max_heap_bytes),
            collect_after_objects: 256.min(limits.max_objects),
            limits,
            steps: 0,
            last_poll_steps: 0,
            scope_steps: BTreeMap::new(),
            scope_work: BTreeMap::new(),
            changed_scopes: Default::default(),
            closed: false,
            faulted: false,
            next_task: 1,
            foreground: None,
            runnable: VecDeque::new(),
            blocked: waiters::Waiters::default(),
            resuming_task: None,
            select_state: 0,
            ffi_session: None,
            ffi_calls: PendingCalls::new(
                limits.max_pending_calls,
                limits.max_ffi_bytes,
                wake.clone(),
            ),
            wake: wake.clone(),
            lifetime: Cancellation::default(),
            clock: Arc::new(crate::environment::SystemClock::default()),
            entropy: Arc::new(crate::environment::SystemEntropy),
            timers: timer::TimerQueue::default(),
            timer_check_steps: 0,
            reflected_types: Vec::new(),
            reflected_type_indices: HashMap::new(),
            reflected_type_keys: HashMap::new(),
            interface_assignments: std::sync::Mutex::new(HashSet::new()),
            scope_cancellations: BTreeMap::new(),
            cancellation_events: Arc::new(crate::ffi::CancellationEvents::new(wake.clone())),
            debug: debug::DebugState::default(),
            host_capabilities: HashSet::new(),
            retired_revisions: BTreeMap::new(),
        };
        let globals: Vec<_> = instance
            .revision
            .program
            .decoded
            .artifacts()
            .iter()
            .flat_map(|(module, artifact)| {
                artifact
                    .globals
                    .iter()
                    .map(move |global| (module.clone(), global.clone()))
            })
            .collect();
        for (module, global) in globals {
            let typ = instance.types.resolve(&module, &global.r#type)?;
            let value = Value {
                typ,
                data: Data::Uninitialized,
            };
            let handle = instance.allocate(value)?;
            instance.globals.insert((module, global.id), handle);
        }
        Ok(instance)
    }

    pub fn with_bridge(
        program: Arc<Program>,
        limits: ExecutionLimits,
        bridge: &dyn Bridge,
    ) -> Result<Self, RuntimeError> {
        let mut capabilities = HashSet::new();
        for capability in bridge.capabilities() {
            if capability.is_empty()
                || capability.trim() != capability
                || !capabilities.insert(capability)
            {
                return Err(RuntimeError::new(
                    "invalid_capabilities",
                    "instance",
                    "host capabilities must be nonempty, trimmed and unique",
                ));
            }
        }
        if program
            .image()
            .capabilities
            .iter()
            .any(|capability| !capabilities.contains(capability))
        {
            return Err(RuntimeError::new(
                "capability_unavailable",
                "instance",
                "program requires an unavailable host capability",
            ));
        }
        let mut instance = Self::create(program, limits)?;
        instance.host_capabilities = capabilities;
        instance.ffi_session = Some(bridge.open(instance.lifetime.clone())?);
        Ok(instance)
    }

    pub fn wake(&self) -> Arc<Wake> {
        self.wake.clone()
    }

    pub fn root_ready(&self) -> bool {
        !self.closed
            && self.root_initialization.is_none()
            && self
                .initialized
                .contains(&self.revision.program.image().root)
    }

    /// Complete root initialization before publishing an instance to callers.
    #[cfg(not(target_arch = "wasm32"))]
    pub fn initialize_root(&mut self, cancellation: &Cancellation) -> Result<(), RuntimeError> {
        loop {
            let observed = self.wake.epoch();
            match self.poll_initialize(cancellation, 4096) {
                Ok(PollStatus::Ready) => return Ok(()),
                Ok(PollStatus::Running) => continue,
                Ok(PollStatus::Pending) => self.wake.wait(
                    observed,
                    self.next_timer_delay()
                        .unwrap_or(std::time::Duration::from_millis(10))
                        .min(std::time::Duration::from_millis(10)),
                ),
                Ok(PollStatus::Paused) => {
                    return Err(RuntimeError::new(
                        "paused",
                        "instance",
                        "initialization paused",
                    ));
                }
                Err(error) => {
                    let _ = self.close();
                    return Err(error);
                }
            }
        }
    }

    /// Advance initialization using the same budget and events as normal calls.
    pub fn poll_initialize(
        &mut self,
        cancellation: &Cancellation,
        count: usize,
    ) -> Result<PollStatus, RuntimeError> {
        if self.closed || self.faulted {
            return Err(RuntimeError::new(
                "closed",
                "instance",
                "initialization is unavailable",
            ));
        }
        if cancellation.is_cancelled() {
            self.begin_close();
            return Err(RuntimeError::new(
                "canceled",
                "instance",
                "initialization canceled",
            ));
        }
        let root = self.revision.program.image().root.clone();
        if self.root_initialization.is_none() && self.initialized.contains(&root) {
            return Ok(PollStatus::Ready);
        }
        if self.root_initialization.is_none()
            && (self.foreground.is_some() || !self.running.frames.is_empty())
        {
            return Err(RuntimeError::new(
                "busy",
                "instance",
                "instance has active work",
            ));
        }
        let result = (|| {
            if self.root_initialization.is_none() {
                if !self
                    .revision
                    .program
                    .functions
                    .contains_key(&(root.clone(), "fn.init".to_owned()))
                {
                    self.initialized.insert(root);
                    return Ok(PollStatus::Ready);
                }
                let task = self.allocate_task_id()?;
                self.running.id = task;
                self.running.scope = task;
                self.scope_steps
                    .insert(task, Arc::new(budget::StepBudget::new(&self.wake)));
                self.scope_work.insert(
                    task,
                    stats::ScopeWork {
                        tasks: 1,
                        ..Default::default()
                    },
                );
                self.changed_scopes.insert(task);
                self.root_initialization = Some(task);
                self.initialize_module(&root)?;
                self.foreground = Some(task);
            }
            let status = self.poll_steps(count)?;
            if status == PollStatus::Ready {
                let task = self.root_initialization.take().unwrap();
                if !self.scope_active(task) {
                    self.release_scope(task);
                }
            }
            Ok(status)
        })();
        if result.is_err() {
            self.begin_close();
        }
        result
    }

    pub fn set_environment(
        &mut self,
        clock: Arc<dyn crate::environment::Clock>,
        entropy: Arc<dyn crate::environment::Entropy>,
    ) -> Result<(), RuntimeError> {
        if self.foreground.is_some()
            || !self.running.frames.is_empty()
            || !self.runnable.is_empty()
            || !self.blocked.is_empty()
            || !self.timers.is_empty()
        {
            return Err(RuntimeError::new(
                "busy",
                "environment",
                "instance has live work",
            ));
        }
        self.clock = clock;
        self.entropy = entropy;
        Ok(())
    }

    /// Results remain rooted until the next start or close. Guest references in
    /// cloned values belong to this instance and are not persistent host roots.
    pub fn results(&self) -> &[Value] {
        &self.results
    }

    pub fn snapshot_results(
        &self,
        limits: crate::snapshot::SnapshotLimits,
    ) -> Result<crate::snapshot::HostSnapshot, RuntimeError> {
        crate::snapshot::HostSnapshot::capture(&self.heap, &self.types, &self.results, limits)
    }
    pub fn heap_stats(&self) -> HeapStats {
        self.heap.stats()
    }
    pub fn steps(&self) -> u64 {
        self.steps
    }

    pub fn start(&mut self, entry: &str, arguments: Vec<Value>) -> Result<(), RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new(
                "closed",
                "instance",
                "instance is closed",
            ));
        }
        if self.faulted {
            return Err(RuntimeError::new(
                "faulted",
                "instance",
                "instance execution failed",
            ));
        }
        if self.foreground.is_some() {
            return Err(RuntimeError::new("busy", "instance", "execution is active"));
        }
        self.admit_task()?;
        self.types
            .use_context(self.revision.program.decoded.types());
        let entry = self
            .revision
            .program
            .image()
            .entries
            .iter()
            .find(|value| value.name == entry)
            .cloned()
            .ok_or_else(|| RuntimeError::new("missing_entry", "instance", entry))?;
        let task_id = self.allocate_task_id()?;
        let result_count = self
            .revision
            .program
            .function(&entry.module_path, &entry.function_id)?
            .declaration
            .signature
            .results
            .len();
        self.preparing_task = Some(std::mem::replace(
            &mut self.running,
            scheduler::Task {
                id: task_id,
                scope: task_id,
                ..scheduler::Task::default()
            },
        ));
        let initialized = self.initialized.clone();
        let initializing = self.initializing.clone();
        let result = self
            .push_frame(
                FunctionValue {
                    index: None,
                    revision: None,
                    module: entry.module_path.clone().into(),
                    function: entry.function_id.into(),
                    captures: Vec::new(),
                },
                arguments,
                result_count,
                false,
            )
            .and_then(|()| {
                let frames = std::mem::take(&mut self.running.frames);
                let result = self.charge_guest(128);
                self.running.frames = frames;
                result
            });
        let preparing_initialization = result.is_ok();
        let result = result.and_then(|()| self.initialize_module(&entry.module_path).map(|_| ()));
        let prepared_task =
            std::mem::replace(&mut self.running, self.preparing_task.take().unwrap());
        if let Err(error) = result {
            self.initialized = initialized;
            self.initializing = initializing;
            if preparing_initialization {
                for frame in &prepared_task.frames {
                    self.memory.recycle_frame_storage(
                        frame.revision.generation,
                        frame.prepared.module_index,
                        frame.prepared.index,
                        frame.memory,
                    );
                }
            }
            drop(prepared_task);
            self.collect_at_boundary()?;
            return Err(error);
        }
        self.frame_pool.recycle_operands(
            std::mem::take(&mut self.results),
            self.limits.max_frame_cache_bytes,
        );
        let active: HashSet<_> = self
            .runnable
            .iter()
            .chain(self.blocked.iter())
            .map(|task| task.scope)
            .chain(self.timers.iter().map(|timer| timer.scope))
            .chain((!self.running.frames.is_empty()).then_some(self.running.scope))
            .collect();
        self.scope_steps.retain(|scope, _| active.contains(scope));
        self.scope_work.retain(|scope, _| active.contains(scope));
        self.debug
            .scope_profile_dropped
            .retain(|scope, _| active.contains(scope));
        self.yield_task();
        self.running = prepared_task;
        self.foreground = Some(task_id);
        self.scope_steps
            .insert(task_id, Arc::new(budget::StepBudget::new(&self.wake)));
        self.scope_work.insert(
            task_id,
            stats::ScopeWork {
                tasks: 1,
                ..Default::default()
            },
        );
        self.changed_scopes.insert(task_id);
        self.running.transient_roots.clear();
        Ok(())
    }

    /// Starts a binary ABI entry with an owned guest byte slice.
    pub fn start_bytes(&mut self, entry: &str, bytes: &[u8]) -> Result<(), RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new(
                "closed",
                "instance",
                "instance is closed",
            ));
        }
        if self.faulted {
            return Err(RuntimeError::new(
                "faulted",
                "instance",
                "instance execution failed",
            ));
        }
        if self.foreground.is_some() {
            return Err(RuntimeError::new("busy", "instance", "execution is active"));
        }
        self.admit_task()?;
        if bytes.len() > self.limits.max_sequence_elements
            || bytes.len() as u64 > self.limits.max_heap_bytes
        {
            return Err(RuntimeError::new(
                "boundary_limit",
                "argument",
                "byte argument exceeds instance limits",
            ));
        }
        let argument = self.make_bytes(
            TypeIdentity::Slice(std::sync::Arc::new(TypeIdentity::Primitive(
                wire::PrimitiveUint8,
            ))),
            bytes.len(),
            bytes.len(),
            bytes,
        )?;
        let result = self
            .charge_guest(128 + bytes.len() as u64)
            .and_then(|()| self.start(entry, vec![argument]));
        if result.is_err() {
            self.collect_at_boundary()?;
        }
        result
    }

    pub fn poll_steps(&mut self, count: usize) -> Result<PollStatus, RuntimeError> {
        self.poll(count, false)
    }

    #[cfg(not(target_arch = "wasm32"))]
    pub(crate) fn poll_parallel(
        &mut self,
        count: usize,
        parallelism: usize,
        executor: &Arc<crate::executor_pool::Pool>,
        control_waiters: &std::sync::atomic::AtomicUsize,
        observer: Option<&Arc<dyn Fn(u64, bool) + Send + Sync>>,
    ) -> Result<PollStatus, RuntimeError> {
        if count == 0 {
            return Err(RuntimeError::new(
                "step_limit",
                "poll",
                "poll budget must be positive",
            ));
        }
        let parallelism = parallelism.max(1);
        let mut executed = 0usize;
        while executed < count {
            if self.closed {
                return Err(RuntimeError::new(
                    "closed",
                    "instance",
                    "instance is closed",
                ));
            }
            if self.faulted {
                return Err(RuntimeError::new(
                    "faulted",
                    "instance",
                    "instance execution failed",
                ));
            }
            self.cancel_requested_scopes()?;
            if self.debug.paused {
                self.last_poll_steps = executed;
                return Ok(PollStatus::Paused);
            }
            if self.foreground.is_none() {
                self.last_poll_steps = executed;
                return Ok(PollStatus::Ready);
            }
            if let Err(error) = self.deliver_timers().and_then(|()| self.resume_blocked()) {
                self.faulted = true;
                self.abort()?;
                return Err(error);
            }

            let remaining = count - executed;
            if let Some(batch) = self.take_task_batch(parallelism, remaining)? {
                let runs = match task_runner::run_private_batch(
                    executor,
                    batch,
                    self.types.clone(),
                    observer,
                ) {
                    Ok(runs) => runs,
                    Err((error, tasks)) => {
                        for mut task in tasks.into_iter().rev() {
                            task.step_grant = None;
                            self.runnable.push_front(task);
                        }
                        return Err(error);
                    }
                };
                let progressed = self.merge_parallel_runs(runs);
                executed = executed.saturating_add(progressed);
                if progressed != 0 {
                    if control_waiters.load(std::sync::atomic::Ordering::Acquire) != 0 {
                        self.last_poll_steps = executed;
                        return Ok(PollStatus::Running);
                    }
                    continue;
                }
            }

            // The next operation is a control transaction. Execute it through
            // the existing interpreter after every task has returned its
            // private continuation; this is also the progress path when the
            // executor contains only one worker.
            let status = self.poll_steps(1)?;
            let serial_steps = self.last_poll_steps;
            executed = executed.saturating_add(serial_steps);
            if status != PollStatus::Running || serial_steps == 0 {
                self.last_poll_steps = executed;
                return Ok(status);
            }
        }
        self.last_poll_steps = executed;
        Ok(if self.foreground.is_none() {
            PollStatus::Ready
        } else {
            PollStatus::Running
        })
    }

    #[cfg(not(target_arch = "wasm32"))]
    pub(crate) fn start_background_task_batch(
        &mut self,
        parallelism: usize,
        executor: &Arc<crate::executor_pool::Pool>,
        observer: Option<&Arc<dyn Fn(u64, bool) + Send + Sync>>,
    ) -> Result<Option<task_runner::TaskBatch>, RuntimeError> {
        self.deliver_timers()?;
        self.resume_blocked()?;
        let Some(tasks) = self.take_task_batch(parallelism.max(1), 256)? else {
            return Ok(None);
        };
        match task_runner::start_private_batch(
            executor,
            tasks,
            self.types.clone(),
            observer,
            Some(self.wake.clone()),
        ) {
            Ok(batch) => Ok(Some(batch)),
            Err((error, tasks)) => {
                for mut task in tasks.into_iter().rev() {
                    task.step_grant = None;
                    self.runnable.push_front(task);
                }
                Err(error)
            }
        }
    }

    #[cfg(not(target_arch = "wasm32"))]
    pub(crate) fn merge_parallel_runs(&mut self, runs: Vec<task_runner::TaskRun>) -> usize {
        let mut progressed = 0usize;
        for mut run in runs {
            run.task.step_grant = None;
            self.steps = self.steps.saturating_add(run.steps as u64);
            progressed = progressed.saturating_add(run.steps);
            self.changed_scopes.insert(run.task.scope);
            self.runnable.push_back(run.task);
        }
        progressed
    }

    #[cfg(not(target_arch = "wasm32"))]
    fn take_task_batch(
        &mut self,
        parallelism: usize,
        budget: usize,
    ) -> Result<Option<Vec<(scheduler::Task, usize)>>, RuntimeError> {
        if self.debug.profile_every != 0
            || self.debug.resume.is_some()
            || !self.debug.breakpoints.is_empty()
            || self.debug.pause.load(std::sync::atomic::Ordering::Acquire)
        {
            return Ok(None);
        }
        self.yield_task();
        let available = self.runnable.len();
        let wanted = parallelism.min(budget).min(available);
        if wanted == 0 {
            return Ok(None);
        }
        // Preserve queue fairness across the task/control boundary. A CPU task
        // can remain private-runner eligible indefinitely; skipping an older
        // task at a channel, call or other short control instruction would
        // starve that operation forever when the executor has one worker.
        if self
            .runnable
            .front()
            .is_some_and(|task| !task_runner::can_execute_private_instruction(task))
        {
            return Ok(None);
        }
        let mut selected = Vec::with_capacity(wanted);
        for _ in 0..available {
            let task = self.runnable.pop_front().unwrap();
            if selected.len() < wanted && task_runner::can_execute_private_instruction(&task) {
                selected.push(task);
            } else {
                self.runnable.push_back(task);
            }
        }
        if selected.is_empty() {
            return Ok(None);
        }
        if selected.len() < wanted && !self.runnable.is_empty() {
            // Let the owner advance the short control transactions first so
            // several tasks become runnable together. Dispatching the first
            // private task immediately would accidentally serialize a mixed
            // workload behind one task's next control operation.
            self.runnable.extend(selected);
            return Ok(None);
        }

        let mut selected = VecDeque::from(selected);
        let mut runs = Vec::with_capacity(selected.len());
        let mut remaining = budget;
        let selected_count = selected.len();
        for index in 0..selected_count {
            let mut task = selected.pop_front().unwrap();
            let tasks_left = selected_count - index;
            let quantum = remaining
                .div_ceil(tasks_left)
                .clamp(1, task_runner::INSTRUCTION_QUANTUM);
            let Some(steps) = self.scope_steps.get(&task.scope) else {
                self.runnable.push_front(task);
                for task in selected.into_iter().rev() {
                    self.runnable.push_front(task);
                }
                for (task, _) in runs.into_iter().rev() {
                    self.runnable.push_front(task);
                }
                return Ok(None);
            };
            match steps.reserve(self.limits.max_steps, quantum as u64) {
                Ok(Some(grant)) => task.step_grant = Some(grant),
                Ok(None) | Err(_) => {
                    self.runnable.push_front(task);
                    for task in selected.into_iter().rev() {
                        self.runnable.push_front(task);
                    }
                    for (mut task, _) in runs.into_iter().rev() {
                        task.step_grant = None;
                        self.runnable.push_front(task);
                    }
                    return Ok(None);
                }
            }
            runs.push((task, quantum));
            remaining -= quantum;
        }
        Ok(Some(runs))
    }

    pub fn poll_background(&mut self, count: usize) -> Result<PollStatus, RuntimeError> {
        if self.foreground.is_some() {
            return Err(RuntimeError::new(
                "busy",
                "instance",
                "foreground execution is active",
            ));
        }
        self.poll(count, true)
    }

    fn poll(&mut self, count: usize, background: bool) -> Result<PollStatus, RuntimeError> {
        let result = self.poll_loop(count, background).and_then(|status| {
            if matches!(status, PollStatus::Running | PollStatus::Pending) {
                self.poll_ready_events(true)?;
                if status == PollStatus::Pending && !self.runnable.is_empty() {
                    return Ok(PollStatus::Running);
                }
            }
            Ok(status)
        });
        self.running.step_grant = None;
        result
    }

    fn poll_ready(&self, background: bool) -> bool {
        if background {
            self.running.frames.is_empty()
                && self.runnable.is_empty()
                && self.blocked.is_empty()
                && self.timers.is_empty()
        } else {
            self.foreground.is_none()
        }
    }

    fn poll_loop(&mut self, count: usize, background: bool) -> Result<PollStatus, RuntimeError> {
        self.last_poll_steps = 0;
        self.timer_check_steps = usize::from(TASK_POLL_INTERVAL);
        if self.closed {
            return Err(RuntimeError::new(
                "closed",
                "instance",
                "instance is closed",
            ));
        }
        if self.faulted {
            return Err(RuntimeError::new(
                "faulted",
                "instance",
                "instance execution failed",
            ));
        }
        // A census retry or budget failure belongs to the instruction just
        // counted. Complete that zero-step continuation before returning.
        while self.last_poll_steps < count
            || self.running.pending_error.is_some()
            || self.running.retry_instruction
            || self
                .running
                .frames
                .last()
                .is_some_and(|frame| frame.after_init.is_some())
        {
            self.cancel_requested_scopes()?;
            if self.debug.paused {
                return Ok(PollStatus::Paused);
            }
            if self.poll_ready(background) {
                return Ok(PollStatus::Ready);
            }
            self.poll_ready_events(
                self.timer_check_steps >= usize::from(TASK_POLL_INTERVAL)
                    || self.running.frames.is_empty(),
            )?;
            if self.running.frames.is_empty() && !self.schedule_next() {
                if self.debug_pause_waiting() {
                    return Ok(PollStatus::Paused);
                }
                return Ok(PollStatus::Pending);
            }
            let pending_error = self.running.pending_error.take();
            let frame = self.running.frames.last().unwrap();
            let retrying = self.running.retry_instruction;
            let instruction_step = pending_error.is_none()
                && frame.returning.is_none()
                && self.running.selection_completion.is_none()
                && self.running.pending_write.is_none()
                && frame.continuation.is_none()
                && frame.after_init.is_none()
                && frame.pc < frame.prepared.code.len();
            let counted_instruction = instruction_step && !retrying;
            if counted_instruction && self.debug_before_instruction() {
                return Ok(PollStatus::Paused);
            }
            if counted_instruction {
                if self
                    .running
                    .step_grant
                    .as_ref()
                    .is_some_and(|grant| grant.remaining == 0)
                {
                    self.running.step_grant = None;
                }
                if self.running.step_grant.is_none() {
                    match self.scope_steps[&self.running.scope]
                        .reserve(self.limits.max_steps, u64::from(TASK_POLL_INTERVAL))
                    {
                        Ok(Some(grant)) => self.running.step_grant = Some(grant),
                        Ok(None) => {
                            self.yield_task();
                            return Ok(PollStatus::Pending);
                        }
                        Err(error) => {
                            let foreground = self.foreground == Some(self.running.scope);
                            self.cancel_scope(self.running.scope)?;
                            if !foreground {
                                continue;
                            }
                            return Err(error);
                        }
                    }
                }
                if self.debug.profile_every == 0
                    && self.debug.resume.is_none()
                    && self.debug.breakpoints.is_empty()
                    && !self.debug.pause.load(std::sync::atomic::Ordering::Acquire)
                {
                    self.types.use_context(
                        self.running
                            .frames
                            .last()
                            .unwrap()
                            .revision
                            .program
                            .decoded
                            .types(),
                    );
                    let quantum = (count - self.last_poll_steps).min(usize::from(
                        TASK_POLL_INTERVAL - self.running.scheduling_phase,
                    ));
                    let executed =
                        task_runner::execute_private_slice(&mut self.running, quantum, &self.types);
                    if executed != 0 {
                        self.steps = self.steps.saturating_add(executed as u64);
                        self.last_poll_steps += executed;
                        self.timer_check_steps += executed;
                        self.changed_scopes.insert(self.running.scope);
                        if self.running.scheduling_phase == 0 {
                            self.yield_task();
                        }
                        continue;
                    }
                }
                self.running.step_grant.as_mut().unwrap().consume();
            }
            let scope = self.running.scope;
            let task_id = self.running.id;
            let outcome = if let Some(error) = pending_error {
                Err(error)
            } else {
                (|| {
                    if let Some(request) = self.running.write_collection.request.take() {
                        match request {
                            mutation::CollectionRequest::Heap { clear_cache } => {
                                if clear_cache {
                                    self.frame_pool.clear();
                                }
                                self.collect_rooted()?;
                                self.running.write_collection.heap_collected = true;
                            }
                            mutation::CollectionRequest::Census => {
                                self.memory.publish_census(self.live_guest_bytes()?);
                                self.running.write_collection.census_published = true;
                            }
                        }
                    }
                    if instruction_step {
                        self.types.use_context(
                            self.running
                                .frames
                                .last()
                                .unwrap()
                                .revision
                                .program
                                .decoded
                                .types(),
                        );
                        if task_runner::can_execute_private_instruction(&self.running)
                            && task_runner::try_execute_private_instruction(
                                &mut self.running,
                                &self.types,
                            )
                        {
                            return Ok(());
                        }
                        if retrying {
                            self.running.rewind_instruction();
                            self.running.retry_instruction = false;
                        }
                        let pc = self.running.frames.last().unwrap().pc;
                        self.running.begin_instruction(pc);
                    }
                    self.step()
                })()
            };
            if counted_instruction {
                self.steps = self.steps.saturating_add(1);
                self.last_poll_steps += 1;
                self.timer_check_steps += 1;
                if self.debug.profile_every != 0 {
                    self.debug.profile_phase =
                        if self.debug.profile_phase >= self.debug.profile_every - 1 {
                            0
                        } else {
                            self.debug.profile_phase + 1
                        };
                }
                self.changed_scopes.insert(scope);
            }
            if outcome
                .as_ref()
                .is_err_and(|error| error.code == "census_required")
            {
                let requested = self.running.census_request.ok_or_else(|| {
                    RuntimeError::new(
                        "internal",
                        "scheduler",
                        "census request lost its allocation size",
                    )
                })?;
                // Module initialization retains the triggering instruction in
                // `after_init`. Its ordinary PC already points at the next
                // instruction, so rewinding it here would execute the owner
                // action twice after initialization finishes. Other
                // allocation failures use the regular instruction retry
                // continuation and must restore their original PC/operands.
                let initialization_continuation = self
                    .running
                    .frames
                    .last()
                    .is_some_and(|frame| frame.after_init.is_some());
                if initialization_continuation {
                    self.running.finish_instruction();
                } else {
                    self.running.rewind_instruction();
                }
                let live = self.live_guest_bytes()?;
                self.memory.publish_census(live);
                if let Some(index) = self.running.census_completed_frame.take() {
                    let mut frame = self.running.suspended_frames.remove(index);
                    self.cache_frame(&mut frame)?;
                    self.memory.recycle_frame_storage(
                        frame.revision.generation,
                        frame.prepared.module_index,
                        frame.prepared.index,
                        frame.memory,
                    );
                }
                if requested > self.limits.max_allocated_bytes.saturating_sub(live) {
                    if !initialization_continuation {
                        let pc = self.running.instruction_pc;
                        self.running.frames.last_mut().unwrap().pc = pc.saturating_add(1);
                    }
                    self.running.finish_instruction();
                    self.running.pending_error = Some(RuntimeError::new(
                        "allocation_limit",
                        "guest",
                        "guest allocation byte limit exceeded",
                    ));
                } else if !initialization_continuation {
                    self.running.retry_instruction = true;
                }
                self.running.transient_roots.clear();
                if let Some(frame) = self
                    .running
                    .popped_frame
                    .take()
                    .and_then(|index| self.running.frames.get_mut(index))
                {
                    frame.operand_census = None;
                }
                if counted_instruction {
                    self.running.scheduling_phase =
                        (self.running.scheduling_phase + 1) % TASK_POLL_INTERVAL;
                }
                if self.running.pending_error.is_some()
                    || self.running.retry_instruction
                    || initialization_continuation
                {
                    continue;
                }
                return Ok(PollStatus::Running);
            }
            if let Err(mut error) = outcome {
                if error.code == "panic" && !self.running.frames.is_empty() {
                    let frame = self.running.frames.last_mut().unwrap();
                    frame.panic = Some(Arc::new(Value {
                        typ: TypeIdentity::Primitive(wire::PrimitiveString),
                        data: Data::String(error.message.into_bytes().into()),
                    }));
                    frame.returning = Some(Vec::new());
                    self.debug_panic();
                } else {
                    let initialization_failed = error.path == "module_init"
                        && matches!(error.code, "internal" | "runtime.error");
                    if let Some(frame) = self.running.frames.last() {
                        error.path = format!(
                            "{}/{}@{}: {}",
                            frame.module,
                            frame.function,
                            frame.pc.saturating_sub(1),
                            error.path
                        );
                    }
                    if matches!(
                        error.code,
                        "allocation_limit"
                            | "value_limit"
                            | "string_limit"
                            | "task_limit"
                            | "frame_limit"
                            | "dynamic_type_limit"
                            | "dynamic_type_bytes_limit"
                            | "boundary_limit"
                    ) || initialization_failed
                    {
                        let foreground = self.foreground == Some(scope);
                        self.cancel_scope(scope)?;
                        if !foreground {
                            continue;
                        }
                    } else {
                        self.faulted = true;
                        self.abort()?;
                    }
                    return Err(error);
                }
            }
            let task = if self.running.id == task_id {
                Some(&mut self.running)
            } else {
                self.runnable
                    .iter_mut()
                    .chain(self.blocked.iter_mut())
                    .find(|task| task.id == task_id)
            };
            let mutation_pending = task
                .as_ref()
                .is_some_and(|task| task.pending_write.is_some());
            if let Some(task) = task {
                task.finish_instruction();
                task.transient_roots.clear();
                if counted_instruction {
                    task.scheduling_phase = (task.scheduling_phase + 1) % TASK_POLL_INTERVAL;
                }
                if let Some(frame) = task
                    .popped_frame
                    .take()
                    .and_then(|index| task.frames.get_mut(index))
                {
                    frame.popped_roots.clear();
                    frame.operand_census = None;
                }
            }
            if self.debug.paused {
                return Ok(PollStatus::Paused);
            }
            if mutation_pending {
                return Ok(PollStatus::Running);
            }
            if counted_instruction && self.running.scheduling_phase == 0 {
                self.yield_task();
            }
        }
        Ok(if self.poll_ready(background) {
            PollStatus::Ready
        } else {
            PollStatus::Running
        })
    }

    pub fn cancel(&mut self) -> Result<(), RuntimeError> {
        if let Some(scope) = self.foreground {
            self.cancel_scope(scope)?;
        }
        Ok(())
    }

    pub(crate) fn foreground_scope(&self) -> Option<u64> {
        self.foreground
    }

    pub(crate) fn watch_scope_cancellation(&mut self, scope: u64, cancellation: Cancellation) {
        let listener = cancellation.listen(scope, &self.cancellation_events);
        self.scope_cancellations.insert(scope, listener);
    }

    pub(crate) fn cancel_requested_scopes(&mut self) -> Result<Vec<u64>, RuntimeError> {
        let scopes = self.cancellation_events.take();
        for scope in &scopes {
            if self.scope_cancellations.contains_key(scope) {
                self.cancel_scope(*scope)?;
            }
        }
        Ok(scopes.into_iter().collect())
    }

    pub(crate) fn release_scope(&mut self, scope: u64) {
        self.debug.scope_profile_dropped.remove(&scope);
        self.scope_cancellations.remove(&scope);
        self.scope_steps.remove(&scope);
        self.scope_work.remove(&scope);
    }

    pub(crate) fn scope_active(&self, scope: u64) -> bool {
        self.scope_work
            .get(&scope)
            .is_some_and(|work| work.tasks != 0 || work.timers != 0)
    }

    pub(crate) fn cancel_scope(&mut self, scope: u64) -> Result<(), RuntimeError> {
        self.scope_work.insert(scope, stats::ScopeWork::default());
        self.changed_scopes.insert(scope);
        self.debug_cancel_scope(scope);
        self.scope_cancellations.remove(&scope);
        let mut removed = Vec::new();
        if self.running.scope == scope && self.running.id != 0 {
            removed.push(std::mem::take(&mut self.running));
        }
        for pending in [&mut self.preparing_task, &mut self.resuming_task] {
            if pending.as_ref().is_some_and(|task| task.scope == scope) {
                removed.push(pending.take().unwrap());
            }
        }
        let mut runnable = VecDeque::new();
        for task in self.runnable.drain(..) {
            if task.scope == scope {
                removed.push(task);
            } else {
                runnable.push_back(task);
            }
        }
        self.runnable = runnable;
        removed.extend(self.blocked.remove_scope(scope));
        for task in removed {
            for frame in task.frames.iter().chain(&task.suspended_frames) {
                if frame.initializing {
                    self.initializing.remove(frame.module.as_ref());
                    self.blocked.notify_module(frame.module.as_ref());
                    self.failed_initializations.insert(
                        frame.module.to_string(),
                        RuntimeError::new(
                            "runtime.error",
                            "module_init",
                            "module initialization aborted",
                        ),
                    );
                }
                self.memory.recycle_frame_storage(
                    frame.revision.generation,
                    frame.prepared.module_index,
                    frame.prepared.index,
                    frame.memory,
                );
            }
            if let Some(scheduler::Blocked::Mutex(handle)) = task.blocked {
                self.cancel_mutex_wait(handle, task.id)?;
            } else if let Some(scheduler::Blocked::Ffi(id)) = task.blocked {
                self.ffi_calls.cancel(id);
            }
        }
        self.timers.retain(|timer| timer.scope != scope);
        if self.foreground == Some(scope) {
            self.foreground = None;
            self.results.clear();
        }
        self.collect_at_boundary().map(|_| ())
    }

    #[cfg(not(target_arch = "wasm32"))]
    pub fn close(&mut self) -> Result<(), RuntimeError> {
        self.begin_close();
        let waker = std::task::Waker::from(self.wake.clone());
        let mut cx = std::task::Context::from_waker(&waker);
        loop {
            let observed = self.wake.epoch();
            if let std::task::Poll::Ready(result) = self.poll_close(&mut cx) {
                return result;
            }
            self.wake
                .wait(observed, std::time::Duration::from_millis(10));
        }
    }

    pub fn begin_close(&mut self) {
        if self.cleanup.is_some() || self.cleanup_result.is_some() {
            return;
        }
        self.debug = debug::DebugState::default();
        self.closed = true;
        self.lifetime.cancel();
        self.ffi_calls.close();
        self.globals.clear();
        self.constant_values.clear();
        self.logical_functions.clear();
        self.call_targets.clear();
        self.frame_pool.clear();
        self.retired_revisions.clear();
        self.reflected_types.clear();
        self.reflected_type_indices.clear();
        self.reflected_type_keys.clear();
        self.interface_assignments.get_mut().unwrap().clear();
        self.failed_initializations.clear();
        self.types.clear_dynamic();
        let accounting = self.heap.set_external_bytes(0);
        let aborted = self.abort();
        self.memory.publish_census(0);
        self.memory.retain_revisions(|_| false);
        self.scope_steps.clear();
        let session = self.ffi_session.take();
        self.cleanup = Some(Box::pin(async move {
            let cleanup = match session {
                Some(session) => session.shutdown_async().await,
                None => Ok(()),
            };
            accounting?;
            aborted?;
            cleanup
        }));
    }

    pub fn poll_close(
        &mut self,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Result<(), RuntimeError>> {
        self.begin_close();
        if let Some(result) = &self.cleanup_result {
            return std::task::Poll::Ready(result.clone());
        }
        let result = match self.cleanup.as_mut() {
            Some(cleanup) => match cleanup.as_mut().poll(cx) {
                std::task::Poll::Pending => return std::task::Poll::Pending,
                std::task::Poll::Ready(result) => result,
            },
            None => Ok(()),
        };
        self.cleanup = None;
        self.cleanup_result = Some(result.clone());
        std::task::Poll::Ready(result)
    }

    fn abort(&mut self) -> Result<(), RuntimeError> {
        // A failed initializer must not publish a partially initialized module.
        // Poison the instance if initialization was interrupted.
        if !self.initializing.is_empty() {
            self.closed = true;
            self.globals.clear();
        }
        self.initializing.clear();
        for task in std::iter::once(&self.running)
            .chain(&self.preparing_task)
            .chain(&self.resuming_task)
            .chain(&self.runnable)
            .chain(self.blocked.iter())
        {
            for frame in task.frames.iter().chain(&task.suspended_frames) {
                self.memory.recycle_frame_storage(
                    frame.revision.generation,
                    frame.prepared.module_index,
                    frame.prepared.index,
                    frame.memory,
                );
            }
            if let Some(scheduler::Blocked::Ffi(id)) = task.blocked {
                self.ffi_calls.cancel(id);
            }
        }
        self.running = scheduler::Task::default();
        self.preparing_task = None;
        self.resuming_task = None;
        self.runnable.clear();
        self.blocked.clear();
        self.changed_scopes.extend(self.scope_work.keys().copied());
        for work in self.scope_work.values_mut() {
            *work = stats::ScopeWork::default();
        }
        self.foreground = None;
        self.scope_cancellations.clear();
        self.timers.clear();
        self.results.clear();
        self.collect_at_boundary().map(|_| ())
    }

    fn load_prepared_constant(
        &mut self,
        revision: &Arc<crate::program::Revision>,
        index: usize,
    ) -> Result<Value, RuntimeError> {
        let key = (revision.generation, index);
        if let Some(value) = self.constant_values.get(&key) {
            return Ok(value.clone());
        }
        match &revision.program.constant_data[index] {
            crate::program::PreparedConstant::Scalar(value) => Ok(value.clone()),
            crate::program::PreparedConstant::Failure(error) => Err(error.clone()),
            crate::program::PreparedConstant::Metadata => Err(RuntimeError::new(
                "invalid_constant",
                "constant",
                "untyped constant cannot be executed",
            )),
            crate::program::PreparedConstant::Bytes { typ, bytes } => {
                if bytes.len() > self.limits.max_sequence_elements {
                    return Err(RuntimeError::new(
                        "value_limit",
                        "constant",
                        "byte constant exceeds collection limit",
                    ));
                }
                let value = self.make_bytes(typ.clone(), bytes.len(), bytes.len(), bytes)?;
                self.constant_values.insert(key, value.clone());
                Ok(value)
            }
        }
    }

    fn initialize_module(&mut self, module: &str) -> Result<bool, RuntimeError> {
        if let Some(error) = self.failed_initializations.get(module) {
            return Err(error.clone());
        }
        if self.initialized.contains(module) {
            return Ok(false);
        }
        if let Some(&owner) = self.initializing.get(module) {
            let mut owner = Some(owner);
            while let Some(task) = owner {
                if task == self.running.id {
                    return Err(RuntimeError::new(
                        "internal",
                        "module_init",
                        "module initialization cycle",
                    ));
                }
                owner = self
                    .blocked
                    .iter()
                    .find(|waiting| waiting.id == task)
                    .and_then(|waiting| match &waiting.blocked {
                        Some(scheduler::Blocked::Module(module)) => {
                            self.initializing.get(module).copied()
                        }
                        _ => None,
                    });
            }
            self.park(scheduler::Blocked::Module(module.to_owned()));
            return Ok(true);
        }
        if !self
            .revision
            .program
            .functions
            .contains_key(&(module.to_owned(), "fn.init".to_owned()))
        {
            self.initialized.insert(module.to_owned());
            return Ok(false);
        }
        let prepared = self.push_frame(
            FunctionValue {
                index: None,
                revision: None,
                module: module.into(),
                function: "fn.init".into(),
                captures: Vec::new(),
            },
            Vec::new(),
            0,
            true,
        );
        if let Err(error) = prepared {
            if error.code != "census_required" {
                self.failed_initializations
                    .insert(module.to_owned(), error.clone());
            }
            return Err(error);
        }
        self.initializing.insert(module.to_owned(), self.running.id);
        Ok(true)
    }

    fn step(&mut self) -> Result<(), RuntimeError> {
        if self.running.pending_write.is_some() {
            return self.resume_write();
        }
        if self.running.selection_completion.is_some() {
            return self.finish_selection();
        }
        self.types.use_context(
            self.running
                .frames
                .last()
                .unwrap()
                .revision
                .program
                .decoded
                .types(),
        );
        if self.running.frames.last().unwrap().panic.is_some() {
            let frame = self.running.frames.last_mut().unwrap();
            frame.continuation = None;
            frame.delivery.values.clear();
            frame.delivery.reading = false;
        }
        if let Some(continuation) = self.running.frames.last_mut().unwrap().continuation.take() {
            match continuation {
                frame::Continuation::Intrinsic(resume) => {
                    resume.trace(&mut |handle| self.running.transient_roots.push(handle));
                    return self.resume_reflect(resume);
                }
                frame::Continuation::TailReturn(count) => {
                    self.running.frames.last_mut().unwrap().delivery.reading = true;
                    let values = self.running.pop_values(&self.frame_pool, count)?;
                    self.running.frames.last_mut().unwrap().delivery.reading = false;
                    return self.begin_return(values);
                }
            }
        }
        let frame = self.running.frames.last().unwrap();
        if frame.returning.is_some() {
            return self.finish_frame();
        }
        let revision = frame.revision.clone();
        let function = frame.prepared.clone();
        let module = &function.module;
        let pc = if let Some(pc) = self.running.frames.last_mut().unwrap().after_init.take() {
            pc
        } else {
            let pc = self.running.frames.last().unwrap().pc;
            if pc == function.code.len() {
                self.running.frames.last_mut().unwrap().returning = Some(Vec::new());
                return self.finish_frame();
            }
            self.running.frames.last_mut().unwrap().pc += 1;
            pc
        };
        let instruction = function.code.descriptor(pc);
        self.begin_slot_instruction(pc)?;
        use Instruction::*;
        let initialization_target = match instruction {
            Some(CallDirect(payload) | TailCallDirect(payload)) => Some(&payload.module_path),
            Some(LoadExport(payload)) => Some(&payload.module_path),
            Some(AddressOf(payload)) if payload.kind == "export" => Some(&payload.module_path),
            _ => None,
        };
        if let Some(target) = initialization_target
            && !target.is_empty()
            && target.as_str() != module.as_ref()
            && !self.initialized.contains(target)
            && self.initializing.get(target) != Some(&self.running.id)
        {
            let caller = self.running.frames.len() - 1;
            self.running.frames[caller].after_init = Some(pc);
            if self.initialize_module(target)? {
                return Ok(());
            }
            self.running.frames[caller].after_init = None;
        }
        let mut result = None;
        use crate::program::PreparedInstruction as Prepared;
        match function.execution[pc] {
            Prepared::TypeDispatch(index) => {
                let dispatch = &function.type_dispatches[index];
                if self
                    .running
                    .frames
                    .last()
                    .unwrap()
                    .type_dispatch_value
                    .is_none()
                {
                    let value = self.load_local(dispatch.subject)?;
                    self.running.frames.last_mut().unwrap().type_dispatch_value = Some(value);
                }
                let value = self
                    .running
                    .frames
                    .last()
                    .unwrap()
                    .type_dispatch_value
                    .as_ref()
                    .unwrap()
                    .clone();
                let mut offset = self.running.frames.last().unwrap().type_dispatch_index;
                if offset == 0 && dispatch.concrete_end > 0 {
                    let dynamic = match &value.data {
                        Data::Interface(inner) => inner.as_ref(),
                        _ => &value,
                    };
                    let nil_interface = matches!(value.data, Data::Nil)
                        && (value.typ == TypeIdentity::Any
                            || value.typ == TypeIdentity::Primitive(wire::PrimitiveError)
                            || self
                                .types
                                .node(&value.typ)?
                                .is_some_and(|(_, node)| node.kind == wire::Interface));
                    if nil_interface
                        || crate::program::concrete_dispatch_identity(&self.types, &dynamic.typ)?
                    {
                        let key = (!nil_interface).then(|| dynamic.typ.clone());
                        offset = dispatch
                            .concrete
                            .get(&key)
                            .copied()
                            .unwrap_or(dispatch.concrete_end);
                    }
                }
                let mut target = dispatch.fallback;
                let mut binding = dispatch.fallback_local;
                let mut selected = value.clone();
                let mut matched = true;
                if let Some(case) = dispatch.cases.get(offset) {
                    // Only an empty interface is nil here; a boxed nil pointer
                    // retains its concrete dynamic type.
                    let nil_interface = matches!(value.data, Data::Nil)
                        && (value.typ == TypeIdentity::Any
                            || value.typ == TypeIdentity::Primitive(wire::PrimitiveError)
                            || self
                                .types
                                .node(&value.typ)?
                                .is_some_and(|(_, node)| node.kind == wire::Interface));
                    matched = nil_interface;
                    if let Some(typ) = &case.typ {
                        let dynamic = match &value.data {
                            Data::Interface(inner) => inner.as_ref(),
                            _ => &value,
                        };
                        matched = !nil_interface
                            && (self.types.identical(&dynamic.typ, typ)?
                                || self.types.implements(&dynamic.typ, typ)?);
                        if matched && !case.original {
                            selected = self.coerce(dynamic.clone(), typ)?;
                        }
                    }
                    target = case.target;
                    binding = case.binding;
                }
                if matched {
                    if let Some(binding) = binding {
                        self.store_local(binding, selected, false)?;
                    }
                    let frame = self.running.frames.last_mut().unwrap();
                    frame.type_dispatch_index = 0;
                    frame.type_dispatch_value = None;
                    frame.pc = target;
                } else {
                    let frame = self.running.frames.last_mut().unwrap();
                    frame.type_dispatch_index = offset + 1;
                    frame.pc = pc;
                }
            }
            Prepared::Intrinsic(intrinsic) => self.execute_intrinsic(intrinsic)?,
            Prepared::Constant(index) => {
                result = Some(self.load_prepared_constant(&revision, index)?)
            }
            Prepared::Local {
                index,
                store,
                rebind,
                ..
            } => {
                if !store {
                    result = Some(self.load_local(index)?);
                } else {
                    let value = self.running.pop()?;
                    if let Some(root) = self.shared_local_target(index, rebind)? {
                        self.start_address_write(
                            Address {
                                identity: Arc::default(),
                                root,
                                path: Vec::new(),
                            },
                            value,
                        )?;
                    } else {
                        self.store_local(index, value, false)?;
                    }
                }
            }
            Prepared::Upvalue { index, store } => {
                let address = self.running.frames.last().unwrap().upvalues[index].clone();
                if store {
                    let value = self.running.pop()?;
                    self.start_address_write(address, value)?;
                } else {
                    result = Some(self.read_address(&address)?);
                }
            }
            Prepared::Global { index, store } => {
                let root = self.running.frames.last().unwrap().globals[index];
                if store {
                    let value = self.running.pop()?;
                    self.start_address_write(
                        Address {
                            identity: Arc::default(),
                            root,
                            path: Vec::new(),
                        },
                        value,
                    )?;
                } else {
                    result = Some(self.load_slot(root)?);
                }
            }
            Prepared::Jump {
                target,
                conditional,
                negate,
            } => {
                let jump = if conditional {
                    let value = self.running.pop()?;
                    let Data::Bool(condition) = value.data else {
                        return Err(RuntimeError::new("type_error", "jump_if", "expected bool"));
                    };
                    condition != negate
                } else {
                    true
                };
                if jump {
                    self.running.frames.last_mut().unwrap().pc = target;
                }
            }
            Prepared::GetPath(index) => {
                let mut value = self.running.pop()?;
                for step in &function.field_paths[index] {
                    if step.indirect {
                        let Data::Pointer(address) = value.data else {
                            return Err(RuntimeError::new(
                                "panic",
                                "get_path",
                                "nil pointer dereference",
                            ));
                        };
                        value = self.read_address(&address)?;
                    }
                    let Data::Struct(fields) = value.data else {
                        return Err(RuntimeError::new(
                            "type_error",
                            "get_path",
                            "expected struct",
                        ));
                    };
                    value = fields.field_at(step.index).cloned().ok_or_else(|| {
                        RuntimeError::new("missing_field", "get_path", "invalid field layout")
                    })?;
                }
                result = Some(value);
            }
            Prepared::Unary(operator) => {
                let value = self.running.pop()?;
                result = Some(crate::operators::unary(operator, value, &self.types)?);
            }
            Prepared::Binary(operator) | Prepared::CompareBranch { operator, .. } => {
                // Inputs stay rooted in their slots until commit, including
                // while string allocation triggers a memory census.
                let frame = self.running.frames.last().unwrap();
                let (left, right) = (
                    frame.operand_from_end(pc, 1).unwrap(),
                    frame.operand_from_end(pc, 0).unwrap(),
                );
                let concatenation = if operator.operator == crate::operators::Operator::Add
                    && let (Data::String(left), Data::String(right)) = (&left.data, &right.data)
                {
                    Some(left.len().checked_add(right.len()).ok_or_else(|| {
                        RuntimeError::new("string_limit", "string", "concatenation size overflow")
                    })?)
                } else {
                    None
                };
                if let Some(length) = concatenation {
                    self.check_string_size(length)?;
                    self.charge_guest(length as u64)?;
                }
                let frame = self.running.frames.last().unwrap();
                let (left, right) = (
                    frame.operand_from_end(pc, 1).unwrap(),
                    frame.operand_from_end(pc, 0).unwrap(),
                );
                let value = crate::operators::binary(operator, left, right, &self.types)?;
                self.running.frames.last_mut().unwrap().slot_input = 0;
                if let Prepared::CompareBranch { target, when, .. } = function.execution[pc] {
                    let Data::Bool(matched) = value.data else {
                        unreachable!("comparison returns bool")
                    };
                    if matched == when {
                        self.running.frames.last_mut().unwrap().pc = target;
                    }
                } else {
                    result = Some(value);
                }
            }
            Prepared::Operand | Prepared::Construct(_) => match instruction
                .expect("unprepared operation descriptor")
            {
                TypeDispatch(_) | CompareBranch(_) | GetPath(_) => {
                    unreachable!("dispatch is prepared")
                }
                Zero(_) => {
                    result = Some(self.zero(function.operand_types[pc].as_ref().unwrap(), 0)?);
                }
                Pop => {
                    self.running.pop()?;
                }
                AddressOf(payload) => {
                    self.charge_guest(128)?;
                    let mut address = self.resolve_address(payload)?;
                    address.reset_identity(address.identity.path_root);
                    let pointee = if address.path.is_empty() {
                        self.heap.get(address.root)?.typ.clone()
                    } else {
                        self.snapshot_address(&address)?.typ.clone()
                    };
                    let typ = TypeIdentity::Pointer(std::sync::Arc::new(pointee));
                    result = Some(Value {
                        typ,
                        data: Data::Pointer(std::sync::Arc::new(address)),
                    });
                }
                LoadIndirect => {
                    let value = self.running.pop()?;
                    let Data::Pointer(address) = value.data else {
                        return Err(RuntimeError::new(
                            if matches!(value.data, Data::Nil) {
                                "panic"
                            } else {
                                "type_error"
                            },
                            "load_indirect",
                            "cannot dereference value",
                        ));
                    };
                    result = Some(self.read_address(&address)?);
                }
                StoreIndirect => {
                    let value = self.running.pop()?;
                    let pointer = self.running.pop()?;
                    let Data::Pointer(address) = pointer.data else {
                        return Err(RuntimeError::new(
                            if matches!(pointer.data, Data::Nil) {
                                "panic"
                            } else {
                                "type_error"
                            },
                            "store_indirect",
                            "cannot dereference value",
                        ));
                    };
                    self.start_address_write(Arc::unwrap_or_clone(address), value)?;
                }
                MakeStruct(payload) => {
                    let mut values = self
                        .running
                        .pop_values(&self.frame_pool, payload.fields.len())?;
                    let Prepared::Construct(index) = function.execution[pc] else {
                        unreachable!("prepared struct")
                    };
                    let crate::program::AggregateLayout::Struct(layout) =
                        &function.aggregates[index]
                    else {
                        unreachable!("struct layout")
                    };
                    if layout.limits.depth > self.limits.max_value_depth
                        || layout.limits.max_sequence > self.limits.max_sequence_elements
                    {
                        return Err(RuntimeError::new(
                            "value_limit",
                            "make_struct",
                            "aggregate layout exceeds value limits",
                        ));
                    }
                    self.charge_guest_object(layout.declared_fields, 0)?;
                    let typ = function.operand_types[pc].as_ref().unwrap().clone();
                    let mut remaining = self
                        .limits
                        .max_heap_bytes
                        .checked_sub(128 + layout.declared_fields as u64 * 16)
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "allocation_limit",
                                "make_struct",
                                "struct layout exceeds heap budget",
                            )
                        })?;
                    let mut fields = Vec::with_capacity(layout.fields.len());
                    let mut initialized = Vec::with_capacity(layout.fields.len());
                    for (field_type, input) in &layout.fields {
                        fields.push(if let Some(input) = input {
                            let value = self.coerce(
                                std::mem::replace(&mut values[*input], Value::int(0)),
                                field_type,
                            )?;
                            remaining = remaining
                                .checked_sub(value.logical_bytes()?.saturating_sub(16))
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "allocation_limit",
                                        "make_struct",
                                        "field exceeds heap budget",
                                    )
                                })?;
                            value
                        } else {
                            Value::zero_with_budget(
                                &self.types,
                                field_type,
                                1,
                                self.limits.max_value_depth,
                                self.limits.max_sequence_elements,
                                &mut remaining,
                            )?
                        });
                        initialized.push(input.is_some());
                    }
                    result = Some(Value {
                        typ,
                        data: Data::Struct(crate::value::StructStorage::with_layout(
                            layout.names.clone(),
                            fields,
                            initialized,
                        )),
                    });
                }
                MakeSequence(payload) => {
                    let Prepared::Construct(index) = function.execution[pc] else {
                        unreachable!("prepared sequence")
                    };
                    let crate::program::AggregateLayout::Sequence(layout) =
                        &function.aggregates[index]
                    else {
                        unreachable!("sequence layout")
                    };
                    if layout.depth > self.limits.max_value_depth
                        || layout.max_sequence > self.limits.max_sequence_elements
                    {
                        return Err(RuntimeError::new(
                            "value_limit",
                            "make_sequence",
                            "aggregate layout exceeds value limits",
                        ));
                    }
                    let values = self
                        .running
                        .pop_values(&self.frame_pool, payload.element_count as usize)?;
                    self.charge_guest_object(values.len(), 0)?;
                    let typ = function.operand_types[pc].as_ref().unwrap().clone();
                    if self
                        .types
                        .node(&typ)?
                        .is_some_and(|(_, node)| node.kind == wire::Slice)
                    {
                        result = Some(self.make_slice(typ, values.len(), values.len(), values)?);
                    } else {
                        let (module, node) = self
                            .types
                            .node(&typ)?
                            .filter(|(_, node)| node.kind == wire::Array)
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "type_error",
                                    "make_sequence",
                                    "expected sequence type",
                                )
                            })?;
                        if usize::try_from(node.length).ok() != Some(values.len()) {
                            return Err(RuntimeError::new(
                                "invalid_sequence",
                                "make_sequence",
                                "element count mismatch",
                            ));
                        }
                        if values.len() > self.limits.max_sequence_elements {
                            return Err(RuntimeError::new(
                                "value_limit",
                                "array",
                                "array length exceeds limit",
                            ));
                        }
                        let element = self.types.resolve(module, &node.elem)?;
                        let mut elements = Vec::with_capacity(values.len());
                        for value in values {
                            elements.push(self.coerce(value, &element)?);
                        }
                        let value = Value {
                            typ,
                            data: Data::Array(elements.into()),
                        };
                        if value.logical_bytes()? > self.limits.max_heap_bytes {
                            return Err(RuntimeError::new(
                                "allocation_limit",
                                "make_sequence",
                                "array exceeds heap budget",
                            ));
                        }
                        result = Some(value);
                    }
                }
                MakeSlice(payload) => {
                    let values = self
                        .running
                        .pop_values(&self.frame_pool, if payload.has_capacity { 2 } else { 1 })?;
                    let length = values[0].integer()?;
                    let capacity = if payload.has_capacity {
                        values[1].integer()?
                    } else {
                        length
                    };
                    if length < 0 || capacity < length {
                        return Err(RuntimeError::new(
                            "panic",
                            "make_slice",
                            "invalid slice length or capacity",
                        ));
                    }
                    let typ = function.operand_types[pc].as_ref().unwrap().clone();
                    if capacity as u64 > self.limits.max_sequence_elements as u64 {
                        return Err(RuntimeError::new(
                            "value_limit",
                            "make_slice",
                            "slice capacity exceeds limit",
                        ));
                    }
                    self.charge_guest_object(capacity as usize, 0)?;
                    result = Some(self.make_slice(
                        typ,
                        length as usize,
                        capacity as usize,
                        Vec::new(),
                    )?);
                }
                MakeMap(payload) => {
                    let entry_values = payload.entry_count as usize * 2;
                    let mut values = self.running.pop_values(
                        &self.frame_pool,
                        entry_values + usize::from(payload.has_capacity),
                    )?;
                    let mut requested_capacity = payload.entry_count as usize;
                    if payload.has_capacity {
                        let capacity = values.pop().unwrap().integer()?;
                        if capacity < 0
                            || capacity as u64 > self.limits.max_sequence_elements as u64
                        {
                            return Err(RuntimeError::new(
                                "value_limit",
                                "make_map",
                                "invalid map capacity",
                            ));
                        }
                        requested_capacity = requested_capacity.max(capacity as usize);
                    }
                    let typ = function.operand_types[pc].as_ref().unwrap().clone();
                    if requested_capacity > self.limits.max_sequence_elements {
                        return Err(RuntimeError::new(
                            "value_limit",
                            "make_map",
                            "map capacity exceeds limit",
                        ));
                    }
                    let mut entries = crate::value::MapStorage::default();
                    let element = self.element_type(&typ)?;
                    for pair in values.as_chunks::<2>().0 {
                        let key = self.map_key(&typ, pair[0].clone())?;
                        let value = self.coerce(pair[1].clone(), &element)?;
                        if let Some(index) = entries.find(&key, &self.types)? {
                            entries.set_value(index, value);
                        } else {
                            entries.insert(key, value);
                        }
                    }
                    self.charge_guest_object(0, requested_capacity)?;
                    let root = self.allocate(Value {
                        typ: TypeIdentity::Any,
                        data: Data::MapEntries(Box::new(entries)),
                    })?;
                    let map = Value {
                        typ,
                        data: Data::Map(root),
                    };
                    result = Some(map);
                }
                LoadField(payload) => {
                    let mut value = self.running.pop()?;
                    if matches!(value.data, Data::Nil)
                        && self.types.pointer_element(&value.typ)?.is_some()
                    {
                        return Err(RuntimeError::new(
                            "panic",
                            "load_field",
                            "nil pointer dereference",
                        ));
                    }
                    if let Data::Pointer(address) = value.data {
                        value = self.read_address(&address)?;
                    }
                    let Data::Struct(fields) = value.data else {
                        return Err(RuntimeError::new(
                            "type_error",
                            "load_field",
                            "expected struct",
                        ));
                    };
                    result = Some(fields.get(&payload.field).cloned().ok_or_else(|| {
                        RuntimeError::new("missing_field", "load_field", &payload.field)
                    })?);
                }
                StoreField(payload) => {
                    let value = self.running.pop()?;
                    let object = self.running.pop()?;
                    let Data::Pointer(address) = object.data else {
                        return Err(RuntimeError::new(
                            "invalid_address",
                            "store_field",
                            "expected struct pointer",
                        ));
                    };
                    let mut address = Arc::unwrap_or_clone(address);
                    address.path.push(PathElement::Field(payload.field.clone()));
                    self.start_address_write(address, value)?;
                }
                LoadIndex | LoadIndexOk => {
                    let with_ok = matches!(
                        function.code[self.running.frames.last().unwrap().pc - 1],
                        LoadIndexOk
                    );
                    let key = self.running.pop()?;
                    let object = self.running.pop()?;
                    let (value, ok) = self.index_value(&object, &key)?;
                    if with_ok {
                        self.running.frames.last_mut().unwrap().push_result(value);
                        result = Some(Value::boolean(ok));
                    } else {
                        result = Some(value);
                    }
                }
                StoreIndex => {
                    let mut values = self.running.pop_values(&self.frame_pool, 3)?;
                    let value = values.pop().unwrap();
                    let key = values.pop().unwrap();
                    let object = values.pop().unwrap();
                    self.running.pending_write =
                        Some(self.prepare_index_write(object, key, value)?);
                    self.resume_write()?;
                }
                MakeClosure(payload) => {
                    self.charge_guest_object(payload.captures.len(), 0)?;
                    let captures = payload
                        .captures
                        .iter()
                        .map(|address| self.resolve_address(address))
                        .collect::<Result<Vec<_>, _>>()?;
                    let target = &revision.program.function_table[function.targets[pc].unwrap()];
                    result = Some(Value {
                        typ: TypeIdentity::Any,
                        data: Data::Function(std::sync::Arc::new(FunctionValue {
                            index: Some(target.index),
                            revision: Some(revision.clone()),
                            module: target.module.clone(),
                            function: target.name.clone(),
                            captures,
                        })),
                    });
                }
                CallDirect(payload) | TailCallDirect(payload) | CallValue(payload) => {
                    let tail = matches!(
                        function.code[self.running.frames.last().unwrap().pc - 1],
                        TailCallDirect(_)
                    );
                    let indirect = matches!(
                        function.code[self.running.frames.last().unwrap().pc - 1],
                        CallValue(_)
                    );
                    let indirect_function = if indirect {
                        let caller = self.running.frames.last().unwrap();
                        match &caller.input_at(pc, 0).unwrap().data {
                            Data::Function(callee) => Some(Arc::unwrap_or_clone(callee.clone())),
                            _ => None,
                        }
                    } else {
                        None
                    };
                    let mut borrowed_arguments = if !indirect || indirect_function.is_some() {
                        let caller_index = self.running.frames.len() - 1;
                        self.running.popped_frame = Some(caller_index);
                        let caller = &mut self.running.frames[caller_index];
                        let count = payload.arg_count as usize;
                        caller.memory.popped = memory::grow_frame_buffer(
                            caller.memory.popped,
                            count + usize::from(indirect),
                            false,
                        )?;
                        let start = caller.slot_input.checked_sub(count).ok_or_else(|| {
                            RuntimeError::new("invalid_operand", "frame", "input underflow")
                        })?;
                        caller.operand_census =
                            Some(start - usize::from(indirect)..caller.slot_input);
                        Some(frame::FrameArguments::Caller {
                            frame: caller_index,
                            suspended: false,
                            start,
                            count,
                        })
                    } else {
                        None
                    };
                    let mut arguments = if borrowed_arguments.is_some() {
                        Vec::new()
                    } else {
                        self.running.pop_values(
                            &self.frame_pool,
                            payload.arg_count as usize + usize::from(indirect),
                        )?
                    };
                    let callee = if let Some(callee) = indirect_function {
                        callee
                    } else if indirect {
                        let value = arguments.remove(0);
                        if matches!(value.data, Data::DynamicFunction(_)) {
                            return self.start_dynamic_callback(
                                value,
                                arguments,
                                payload.result_count as usize,
                                false,
                            );
                        }
                        match value.data {
                            Data::Function(callee) => Arc::unwrap_or_clone(callee),
                            Data::Method { function, receiver } => {
                                if let Some(receiver) = receiver {
                                    arguments.insert(0, Arc::unwrap_or_clone(receiver));
                                }
                                Arc::unwrap_or_clone(function)
                            }
                            _ => {
                                return Err(RuntimeError::new(
                                    "panic",
                                    "call",
                                    "call of nil function",
                                ));
                            }
                        }
                    } else {
                        let source =
                            &revision.program.function_table[function.targets[pc].unwrap()];
                        let bound_revision;
                        let target_index;
                        if source.declaration.revision_local {
                            bound_revision = revision.clone();
                            target_index = source.index;
                        } else {
                            bound_revision = self.revision.clone();
                            let logical = revision.logical_functions[source.index];
                            target_index = self.call_targets[logical].ok_or_else(|| {
                                RuntimeError::new(
                                    "missing_function",
                                    source.module.as_ref(),
                                    source.name.as_ref(),
                                )
                            })?;
                        }
                        let target = &bound_revision.program.function_table[target_index];
                        FunctionValue {
                            index: Some(target_index),
                            module: target.module.clone(),
                            function: target.name.clone(),
                            revision: Some(bound_revision),
                            captures: Vec::new(),
                        }
                    };
                    let caller = self.running.frames.last().unwrap();
                    let replace = tail
                        && caller.defers.is_empty()
                        && caller.module == callee.module
                        && self.debug.breakpoints.is_empty()
                        && self.debug.resume.is_none()
                        && !self.debug.pause.load(std::sync::atomic::Ordering::Acquire);
                    if replace {
                        if let Some(frame::FrameArguments::Caller {
                            frame, suspended, ..
                        }) = &mut borrowed_arguments
                        {
                            *frame = self.running.suspended_frames.len();
                            *suspended = true;
                        }
                        self.running
                            .suspended_frames
                            .push(self.running.frames.pop().unwrap());
                    }
                    let frame_index = self.running.frames.len();
                    let created = self.push_frame_arguments(
                        callee,
                        borrowed_arguments.unwrap_or(frame::FrameArguments::Owned(arguments)),
                        payload.result_count as usize,
                        false,
                    );
                    let old = if replace {
                        self.running.suspended_frames.pop()
                    } else {
                        None
                    };
                    if let Err(error) = created {
                        if let Some(old) = old {
                            self.running.frames.push(old);
                        }
                        return Err(error);
                    }
                    if let Some(mut old) = old {
                        self.cache_frame(&mut old)?;
                        self.memory.recycle_frame_storage(
                            old.revision.generation,
                            old.prepared.module_index,
                            old.prepared.index,
                            old.memory,
                        );
                        let frame = &mut self.running.frames[frame_index];
                        frame.initializing = old.initializing;
                        frame.deferred = old.deferred;
                        frame.recovered = old.recovered;
                        frame.expected_results = old.expected_results;
                    } else if tail {
                        let caller = &mut self.running.frames[frame_index - 1];
                        caller.continuation = Some(frame::Continuation::TailReturn(
                            payload.result_count as usize,
                        ));
                    }
                }
                CallInterface(payload) => {
                    let mut arguments = self
                        .running
                        .pop_values(&self.frame_pool, payload.arg_count as usize + 1)?;
                    let value = arguments.remove(0);
                    let receiver = match value.data {
                        Data::Interface(value) => Arc::unwrap_or_clone(value),
                        _ => value,
                    };
                    let (owner, method) = self
                        .types
                        .method(&receiver.typ, &payload.method)?
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "panic",
                                "call_interface",
                                "dynamic type has no matching method",
                            )
                        })?;
                    let expected = self.types.resolve(&owner, &method.receiver)?;
                    let receiver = self.method_receiver(receiver, &expected)?;
                    arguments.insert(0, receiver);
                    self.push_frame(
                        FunctionValue {
                            index: None,
                            revision: None,
                            module: if method.module_path.is_empty() {
                                owner.into()
                            } else {
                                method.module_path.into()
                            },
                            function: method.function_id.into(),
                            captures: Vec::new(),
                        },
                        arguments,
                        payload.result_count as usize,
                        false,
                    )?;
                }
                Return(payload) => {
                    self.return_slot_values(payload.result_count as usize)?;
                }
                DeferPush(payload) => {
                    let value = self.running.pop()?;
                    let Data::Function(callee) = value.data else {
                        return Err(RuntimeError::new(
                            "nil_function",
                            "defer",
                            "value is not callable",
                        ));
                    };
                    let owner = self
                        .running
                        .frames
                        .len()
                        .checked_sub(payload.owner_depth as usize + 1)
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "invalid_defer",
                                "defer",
                                "owner depth exceeds call stack",
                            )
                        })?;
                    let frame = &mut self.running.frames[owner];
                    frame.memory.deferred = memory::grow_frame_buffer(
                        frame.memory.deferred,
                        frame.defers.len() + 1,
                        true,
                    )?;
                    frame.defers.push(Arc::unwrap_or_clone(callee));
                }
                Panic => {
                    let mut value = self.running.pop()?;
                    if matches!(value.data, Data::Nil) {
                        value = Value {
                            typ: TypeIdentity::Primitive(wire::PrimitiveString),
                            data: Data::String((&b"panic called with nil argument"[..]).into()),
                        };
                    }
                    let frame = self.running.frames.last_mut().unwrap();
                    frame.panic = Some(Arc::new(value));
                    frame.returning = Some(Vec::new());
                    self.debug_panic();
                }
                Recover => {
                    result = Some(Value {
                        typ: TypeIdentity::Any,
                        data: Data::Nil,
                    });
                    let mut top = self.running.frames.len() - 1;
                    if !self.running.frames[top].deferred && top > 0 {
                        top -= 1;
                    }
                    if top > 0
                        && self.running.frames[top].deferred
                        && self.running.frames[top].recovered.is_none()
                        && let Some(panic) = self.running.frames[top - 1].panic.clone()
                    {
                        result = Some((*panic).clone());
                        self.running.frames[top].recovered = Some(panic);
                    }
                }
                LoadExport(payload) => {
                    let revision = self.revision.clone();
                    let export = revision
                        .program
                        .export(&payload.module_path, &payload.export)?;
                    result = Some(match export.kind.as_str() {
                        "const" => {
                            let index = revision.program.constants
                                [&(payload.module_path.clone(), export.id.clone())];
                            self.load_prepared_constant(&revision, index)?
                        }
                        "function" => Value {
                            typ: TypeIdentity::Any,
                            data: Data::Function(std::sync::Arc::new(FunctionValue {
                                index: None,
                                revision: Some(self.revision.clone()),
                                module: payload.module_path.clone().into(),
                                function: export.id.clone().into(),
                                captures: Vec::new(),
                            })),
                        },
                        "global" => self
                            .heap
                            .get(self.globals[&(payload.module_path.clone(), export.id.clone())])?
                            .as_ref()
                            .clone(),
                        _ => {
                            return Err(RuntimeError::new(
                                "invalid_export",
                                "load_export",
                                "unsupported export kind",
                            ));
                        }
                    });
                }
                InitModule(payload) => {
                    self.initialize_module(&payload.module_path)?;
                }
                Len | Cap => {
                    let capacity = matches!(
                        function.code[self.running.frames.last().unwrap().pc - 1],
                        Cap
                    );
                    let mut value = self.running.pop()?;
                    let pointee = self.types.pointer_element(&value.typ)?;
                    if let Some(elem) = pointee
                        && let Some((_, node)) = self.types.node(&elem)?
                        && node.kind == wire::Array
                    {
                        result = Some(Value::int(node.length));
                    }
                    if result.is_some() {
                        // The type determines the length even for a nil pointer.
                        self.running
                            .frames
                            .last_mut()
                            .unwrap()
                            .push_result(result.take().unwrap());
                        return Ok(());
                    }
                    if let Data::Pointer(address) = &value.data {
                        value = self.read_address(address)?;
                    }
                    let length = match value.data {
                        Data::String(bytes) if !capacity => bytes.len(),
                        Data::Array(values) => values.len(),
                        Data::Nil => 0,
                        Data::Slice(slice) => {
                            if capacity {
                                slice.capacity
                            } else {
                                slice.length
                            }
                        }
                        Data::Map(root) if !capacity => {
                            let Data::MapEntries(entries) = &self.heap.get(root)?.data else {
                                return Err(RuntimeError::new(
                                    "invalid_map",
                                    "len",
                                    "invalid backing",
                                ));
                            };
                            entries.len()
                        }
                        Data::ResourceRef(root) => {
                            let scheduler::Resource::Channel {
                                capacity: size,
                                values,
                                ..
                            } = &*self.resource(root)?
                            else {
                                return Err(RuntimeError::new(
                                    "type_error",
                                    "len",
                                    "expected channel",
                                ));
                            };
                            if capacity {
                                *size
                            } else if *size == 0 {
                                0
                            } else {
                                values.len()
                            }
                        }
                        _ => {
                            return Err(RuntimeError::new(
                                "type_error",
                                "len",
                                "expected container",
                            ));
                        }
                    };
                    result = Some(Value {
                        typ: TypeIdentity::Primitive(wire::PrimitiveInt),
                        data: Data::Integer(length as i64),
                    });
                }
                Slice => {
                    let mut values = self.running.pop_values(&self.frame_pool, 4)?;
                    let max = values.pop().unwrap();
                    let high = values.pop().unwrap().integer()?;
                    let low = values.pop().unwrap().integer()?;
                    let object = values.pop().unwrap();
                    result = Some(self.slice_value(object, low, high, max)?);
                }
                Append(payload) => {
                    let mut values = self
                        .running
                        .pop_values(&self.frame_pool, payload.count as usize + 1)?;
                    let object = values.remove(0);
                    if payload.expand
                        && self.types.identical(
                            &self.element_type(&object.typ)?,
                            &TypeIdentity::Primitive(wire::PrimitiveUint8),
                        )?
                    {
                        let bytes = self.slice_bytes(&values[0])?;
                        result = Some(self.append_bytes(object, &bytes)?);
                    } else {
                        if payload.expand {
                            values = self.slice_values(&values[0])?;
                        }
                        result = Some(self.append_values(object, values)?);
                    }
                }
                Copy => {
                    let source = self.running.pop()?;
                    let destination = self.running.pop()?;
                    result = Some(Value::int(self.copy_values(destination, source)? as i64));
                }
                Delete => {
                    let key = self.running.pop()?;
                    let object = self.running.pop()?;
                    self.running.pending_write = self.prepare_delete(object, key)?;
                    if self.running.pending_write.is_some() {
                        self.resume_write()?;
                    }
                }
                Clear => {
                    let object = self.running.pop()?;
                    match &object.data {
                        Data::Map(root) => {
                            let Data::MapEntries(entries) = &self.heap.get(*root)?.data else {
                                return Err(RuntimeError::new(
                                    "invalid_map",
                                    "clear",
                                    "invalid backing",
                                ));
                            };
                            let backing = Value {
                                typ: TypeIdentity::Any,
                                data: Data::MapEntries(Box::new(entries.cleared())),
                            };
                            let bytes = backing.logical_bytes()? + 128;
                            self.heap
                                .replace(*root, backing, bytes)
                                .map_err(|(error, _)| error)?;
                        }
                        Data::Slice(slice) => {
                            let zero = self.zero(&self.element_type(&object.typ)?, 0)?;
                            for index in 0..slice.length {
                                self.store_index(
                                    object.clone(),
                                    Value::int(index as i64),
                                    zero.clone(),
                                )?;
                            }
                        }
                        Data::Nil => {}
                        _ => {
                            return Err(RuntimeError::new(
                                "type_error",
                                "clear",
                                "expected map or slice",
                            ));
                        }
                    }
                }
                MapIterInit(payload) => {
                    let object = self.running.pop()?;
                    self.init_map_iterator(&payload.local, object)?;
                }
                MapIterNext(payload) => {
                    let (key, value, ok) = self.next_map_entry(&payload.local)?;
                    self.running
                        .frames
                        .last_mut()
                        .unwrap()
                        .extend_results([key, value]);
                    result = Some(Value::boolean(ok));
                }
                MapIterClose(payload) => {
                    self.running
                        .frames
                        .last_mut()
                        .unwrap()
                        .map_iterators
                        .remove(&payload.local);
                }
                MapKeys => {
                    let object = self.running.pop()?;
                    let values = match object.data {
                        Data::Map(root) => {
                            let Data::MapEntries(entries) = &self.heap.get(root)?.data else {
                                return Err(RuntimeError::new(
                                    "invalid_map",
                                    "map_keys",
                                    "invalid backing",
                                ));
                            };
                            entries.iter().map(|(key, _)| key.clone()).collect()
                        }
                        Data::Nil => Vec::new(),
                        _ => {
                            return Err(RuntimeError::new(
                                "type_error",
                                "map_keys",
                                "expected map",
                            ));
                        }
                    };
                    let (module, node) = self.types.node(&object.typ)?.ok_or_else(|| {
                        RuntimeError::new("type_error", "map_keys", "missing map type")
                    })?;
                    let key = self.types.resolve(module, &node.key)?;
                    result = Some(self.make_slice(
                        TypeIdentity::Slice(std::sync::Arc::new(key)),
                        values.len(),
                        values.len(),
                        values,
                    )?);
                }
                StringRuneAt | StringNextRuneIndex => {
                    let next_index = matches!(
                        function.code[self.running.frames.last().unwrap().pc - 1],
                        StringNextRuneIndex
                    );
                    let index = usize::try_from(self.running.pop()?.integer()?)
                        .map_err(|_| RuntimeError::new("panic", "string", "negative rune index"))?;
                    let object = self.running.pop()?;
                    let Data::String(bytes) = object.data else {
                        return Err(RuntimeError::new("type_error", "string", "expected string"));
                    };
                    if index >= bytes.len() {
                        return Err(RuntimeError::new(
                            "panic",
                            "string",
                            "rune index exceeds byte length",
                        ));
                    }
                    let (rune, width) = crate::value::decode_rune(&bytes[index..]);
                    result = Some(if next_index {
                        Value::int((index + width) as i64)
                    } else {
                        Value {
                            typ: TypeIdentity::Primitive(wire::PrimitiveInt32),
                            data: Data::Integer(i64::from(rune)),
                        }
                    });
                }
                TypeAssert(_) | TypeAssertOk(_) => {
                    let with_ok = matches!(
                        function.code[self.running.frames.last().unwrap().pc - 1],
                        TypeAssertOk(_)
                    );
                    let value = self.running.pop()?;
                    let nil_interface = matches!(value.data, Data::Nil)
                        && (value.typ == TypeIdentity::Any
                            || value.typ == TypeIdentity::Primitive(wire::PrimitiveError)
                            || self
                                .types
                                .node(&value.typ)?
                                .is_some_and(|(_, node)| node.kind == wire::Interface));
                    let typ = function.operand_types[pc].as_ref().unwrap().clone();
                    let value = match value.data {
                        Data::Interface(value) => Arc::unwrap_or_clone(value),
                        _ => value,
                    };
                    let ok = !nil_interface
                        && (self.types.identical(&value.typ, &typ)?
                            || self.types.implements(&value.typ, &typ)?);
                    if !with_ok && !ok {
                        return Err(RuntimeError::new(
                            "panic",
                            "type_assert",
                            "dynamic type does not match assertion",
                        ));
                    }
                    let value = if ok {
                        self.coerce(value, &typ)?
                    } else {
                        self.zero(&typ, 0)?
                    };
                    if with_ok {
                        self.running.frames.last_mut().unwrap().push_result(value);
                        result = Some(Value::boolean(ok));
                    } else {
                        result = Some(value);
                    }
                }
                Convert(_) => {
                    let value = self.running.pop()?;
                    let typ = function.operand_types[pc].as_ref().unwrap().clone();
                    result = Some(self.convert_value(value, typ)?);
                }
                instruction @ (CallFfi(_) | Spawn(_) | MakeWaitable(_) | Select(_)
                | WaitableSend | WaitableRecv | WaitableRecvOk | WaitableTryRecv
                | WaitableTrySend | WaitableCanRecv | WaitableCanSend
                | WaitableClose) => {
                    self.execute_wait(instruction.clone(), module)?;
                }
                CallIntrinsic(_) | Unary(_) | Binary(_) | Const(_) | LoadLocal(_)
                | StoreLocal(_) | LoadUpvalue(_) | StoreUpvalue(_) | LoadGlobal(_)
                | StoreGlobal(_) | Jump(_) | JumpIf(_) | Label(_) => {
                    unreachable!("indexed operation")
                }
            },
        }
        if let Some(value) = result {
            if let Data::String(bytes) = &value.data {
                self.check_string_size(bytes.len())?;
            }
            self.running.frames.last_mut().unwrap().push_result(value);
        }
        Ok(())
    }
}
