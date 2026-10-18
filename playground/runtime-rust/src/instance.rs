//! Owner-coordinated execution. Native workers own task-private continuations;
//! shared state and control transactions remain with the instance owner.
//! Polling executes an exact bounded number of guest instructions.

use crate::{
    contract_generated as wire,
    error::RuntimeError,
    ffi::{Bridge, Cancellation, PendingCalls, Session, Wake},
    heap::{Handle, Heap, HeapStats, Trace},
    managed_map::ManagedMapOwner,
    program::{Instruction, PreparedFunction, Program, Revision},
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
#[cfg(test)]
mod gc_tests;
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
pub(crate) mod scheduler;
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
const TASK_POLL_INTERVAL: u32 = 512;

#[derive(Clone, Copy, Debug)]
pub struct ExecutionLimits {
    pub max_call_depth: usize,
    /// Physical idle frame storage, independent of guest allocation accounting.
    pub max_frame_cache_bytes: usize,
    /// Includes the current revision and the candidate being published.
    pub max_retained_revisions: usize,
    pub max_dynamic_types: usize,
    pub max_dynamic_type_bytes: u64,
    /// Logical managed payload admitted by the instance.
    pub max_guest_bytes: u64,
    /// Resident managed pages and runtime metadata.
    pub max_resident_bytes: u64,
    /// Boundary payload bytes used by host conversion and RPC snapshots.
    pub max_boundary_bytes: usize,
    /// Scope instruction budget: 0 uses the default, -1 is unlimited.
    pub max_steps: i64,
    pub max_boundary_depth: usize,
    pub max_boundary_elements: usize,
    pub max_tasks: usize,
    pub max_pending_events: usize,
    pub max_ffi_bytes: usize,
    pub max_ffi_result_bytes: usize,
}

impl Default for ExecutionLimits {
    fn default() -> Self {
        Self {
            max_call_depth: 1024,
            max_frame_cache_bytes: 8 << 20,
            max_retained_revisions: 128,
            max_dynamic_types: 4096,
            max_dynamic_type_bytes: 16 << 20,
            max_guest_bytes: 8 << 30,
            max_resident_bytes: 0,
            max_boundary_bytes: 64 << 20,
            max_steps: 100_000_000,
            max_boundary_depth: 128,
            max_boundary_elements: 1_000_000,
            max_tasks: 4096,
            max_pending_events: 65_536,
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
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PollStatus {
    Ready,
    Running,
    Pending,
    Paused,
}

pub struct Instance {
    revision: Arc<crate::program::Revision>,
    types: crate::types::TypeRegistry,
    type_registry_revision: usize,
    heap: Heap,
    /// Numeric map owners are kept beside the instance; map payloads and
    /// child edges live in `StorageHeap`, never in this registry.
    pub(crate) managed_maps: ManagedMapOwner,
    /// Channel records are instance-owned. Serialized resource pages carry
    /// only channel metadata; this table keeps one stable managed ring owner
    /// across resource reads and lets collection retire it when its handle is
    /// gone.
    pub(crate) channel_records: std::sync::Mutex<
        HashMap<Handle, Arc<std::sync::Mutex<crate::managed_channel::ManagedChannel>>>,
    >,
    revision_anchors: BTreeMap<u64, Handle>,
    globals: BTreeMap<(String, String), Handle>,
    constant_values: BTreeMap<(u64, usize), Value>,
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
    collect_major_after_bytes: u64,
    limits: ExecutionLimits,
    steps: u64,
    pub(crate) last_poll_steps: usize,
    scope_steps: BTreeMap<u64, Arc<budget::StepBudget>>,
    scope_work: BTreeMap<u64, stats::ScopeWork>,
    pub(crate) changed_scopes: std::collections::BTreeSet<u64>,
    // Identifies the task scope when a blocked operation fails while the
    // supervisor is advancing background work. This keeps error cleanup
    // local when other scopes are concurrently active.
    pub(crate) last_error_scope: Option<u64>,
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
    // Reflection metadata is an index, not a root. The runtimeType value is
    // rebuilt at the call boundary and only a managed record that receives it
    // can keep its dynamic type metadata alive.
    reflected_types: Vec<(TypeIdentity, String)>,
    reflected_type_indices: HashMap<TypeIdentity, usize>,
    reflected_type_keys: HashMap<String, usize>,
    // Dynamic metadata is charged while at least one managed runtimeType
    // record owns it. Constructing a reflection value in a frame does not
    // create a permanent instance root.
    dynamic_type_costs: HashMap<String, u64>,
    dynamic_type_identities: HashMap<String, TypeIdentity>,
    dynamic_type_leases: HashMap<String, crate::managed_storage::StorageControlLease>,
    dynamic_type_owner_counts: HashMap<String, usize>,
    dynamic_type_owners: HashMap<Handle, Vec<String>>,
    // Method-set validation is immutable within a published revision. Keep only
    // successful assignments, with a fixed bound on this owner-local memo.
    interface_assignments: std::sync::Mutex<HashSet<(TypeIdentity, TypeIdentity)>>,
    scope_cancellations: BTreeMap<u64, crate::ffi::CancellationListener>,
    cancellation_events: Arc<crate::ffi::CancellationEvents>,
    pub(crate) debug: debug::DebugState,
    host_capabilities: HashSet<String>,
    retired_revisions: BTreeMap<u64, Arc<crate::program::Revision>>,
}

impl Drop for Instance {
    fn drop(&mut self) {
        let _ = self.close();
    }
}

impl Instance {
    /// Adopt a heap-created structural catalog only after its revision changes.
    /// Ordinary allocations never clone the dynamic registry; Any aggregate
    /// materialization bumps the heap revision and the owner observes it at a
    /// safepoint or before publishing a task result.
    pub(super) fn sync_heap_type_registry(&mut self) {
        let revision = self.heap.type_registry_revision();
        if revision == self.type_registry_revision {
            return;
        }
        if let Some(types) = self.heap.type_registry_snapshot() {
            self.types = types;
            self.type_registry_revision = revision;
        }
    }

    pub(super) fn resolve_function_binding(
        &self,
        callee: &FunctionValue,
        revision: &Arc<Revision>,
    ) -> Result<Arc<PreparedFunction>, RuntimeError> {
        if let Some(index) = callee.index {
            return revision.function_by_index(index);
        }
        revision
            .program
            .function(&callee.module, &callee.function)
            .cloned()
    }
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
        let mut types = program.decoded.types().clone();
        // Storage type IDs are instance-local publication state. A new VM
        // starts with the program catalog's type definitions but never shares
        // the mutable numeric-ID table with another instance.
        types.fork_storage_catalog();
        let heap = Heap::new(usize::MAX, limits.max_guest_bytes)?
            .with_dynamic_type_limits(limits.max_dynamic_types, limits.max_dynamic_type_bytes);
        heap.set_resident_limit(limits.max_resident_bytes)?;
        heap.set_type_registry(types.clone());
        let type_registry_revision = heap.type_registry_revision();
        let wake = Arc::new(Wake::default());
        let mut instance = Self {
            types,
            type_registry_revision,
            revision: Arc::new(crate::program::Revision {
                generation: 1,
                program,
            }),
            heap,
            managed_maps: ManagedMapOwner::default(),
            channel_records: std::sync::Mutex::new(HashMap::new()),
            revision_anchors: BTreeMap::new(),
            globals: BTreeMap::new(),
            constant_values: BTreeMap::new(),
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
            collect_after_bytes: (256 << 10).min(limits.max_guest_bytes),
            collect_major_after_bytes: (1 << 20).min(limits.max_guest_bytes),
            limits,
            steps: 0,
            last_poll_steps: 0,
            scope_steps: BTreeMap::new(),
            scope_work: BTreeMap::new(),
            changed_scopes: Default::default(),
            last_error_scope: None,
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
                limits.max_pending_events,
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
            dynamic_type_costs: HashMap::new(),
            dynamic_type_identities: HashMap::new(),
            dynamic_type_leases: HashMap::new(),
            dynamic_type_owner_counts: HashMap::new(),
            dynamic_type_owners: HashMap::new(),
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
        // Code records retain the revision's global cells through numeric
        // reference lanes, with the shared record-header accounting.
        let anchor = instance
            .heap
            .allocate_revision_anchor(instance.globals.values().copied())
            .map_err(|error| {
                if error.code == "collection_required" {
                    RuntimeError::new(
                        "allocation_limit",
                        "revision",
                        "guest allocation byte limit exceeded",
                    )
                } else {
                    error
                }
            })?;
        instance
            .revision_anchors
            .insert(instance.revision.generation, anchor);
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

    #[cfg(test)]
    pub(crate) fn guest_results(&self) -> &[Value] {
        &self.results
    }

    pub fn snapshot_results(
        &self,
        limits: crate::snapshot::SnapshotLimits,
    ) -> Result<crate::snapshot::HostSnapshot, RuntimeError> {
        crate::snapshot::HostSnapshot::capture(
            &self.heap,
            &self.types,
            &self.managed_maps,
            &self.results,
            limits,
        )
    }
    pub fn heap_stats(&self) -> HeapStats {
        self.heap.stats()
    }

    /// Publish the current worker's nursery before this instance crosses an
    /// ownership boundary (executor queue, host result or external owner).
    /// A lease is thread-local, so another executor worker cannot resolve its
    /// handles until this explicit handoff point.
    pub(crate) fn publish_worker_storage(&self) -> Result<(), RuntimeError> {
        self.heap.flush_worker_lease()
    }

    pub fn steps(&self) -> u64 {
        self.steps
    }

    pub(crate) fn start_values(
        &mut self,
        entry: &str,
        arguments: Vec<Value>,
    ) -> Result<(), RuntimeError> {
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
        let frame_result = self.push_frame(
            FunctionValue {
                index: None,
                revision: None,
                revision_generation: None,
                revision_anchor: None,
                module: entry.module_path.clone().into(),
                function: entry.function_id.into(),
                captures: Vec::new(),
            },
            arguments,
            result_count,
            false,
            128,
        );
        let result =
            frame_result.and_then(|()| self.initialize_module(&entry.module_path).map(|_| ()));
        let prepared_task =
            std::mem::replace(&mut self.running, self.preparing_task.take().unwrap());
        if let Err(error) = result {
            self.initialized = initialized;
            self.initializing = initializing;
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
        self.heap.flush_worker_lease()
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
        if bytes.len() > self.limits.max_boundary_bytes
            || bytes.len() as u64 > self.limits.max_guest_bytes
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
        let result = self.start_values(entry, vec![argument]);
        if result.is_err() {
            self.collect_at_boundary()?;
        }
        result
    }

    pub fn poll_steps(&mut self, count: usize) -> Result<PollStatus, RuntimeError> {
        self.poll(count, false)
    }

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

    pub(crate) fn merge_parallel_runs(&mut self, runs: Vec<task_runner::TaskRun>) -> usize {
        // Private execution can materialize a structural Any aggregate while
        // encoding a frame bank.  The shared Heap owns that catalog during
        // the worker slice; adopt it before the task becomes visible to the
        // owner and before the next collection prunes dynamic metadata.
        self.sync_heap_type_registry();
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
        // Selected tasks may run on a different executor worker. Publish
        // nursery handles before moving them out of this owner thread's TLS
        // lease; private scalar instructions can then read the shared
        // directory without a cross-thread lease lookup.
        self.publish_worker_storage()?;
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
        let published = self.heap.flush_worker_lease();
        match (result, published) {
            (Err(error), _) => Err(error),
            (Ok(_), Err(error)) => Err(error),
            (Ok(status), Ok(())) => Ok(status),
        }
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
        self.timer_check_steps = TASK_POLL_INTERVAL as usize;
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
        // Allocation retries belong to the instruction transaction and are
        // completed before returning a poll status.
        while self.last_poll_steps < count
            || self.running.pending_error.is_some()
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
                self.timer_check_steps >= TASK_POLL_INTERVAL as usize
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
            let instruction_step = pending_error.is_none()
                && frame.returning.is_none()
                && self.running.selection_completion.is_none()
                && self.running.pending_write.is_none()
                && frame.continuation.is_none()
                && frame.after_init.is_none()
                && frame.pc < frame.prepared.code.len();
            let counted_instruction = instruction_step;
            if counted_instruction
                && self.running.charged_instruction_steps == 0
                && self.debug_before_instruction()
            {
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
                if self.running.charged_instruction_steps == 0
                    && self.debug.profile_every == 0
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
                    let quantum = (count - self.last_poll_steps)
                        .min((TASK_POLL_INTERVAL - self.running.scheduling_phase) as usize);
                    let executed =
                        task_runner::execute_private_slice(&mut self.running, quantum, &self.types);
                    if executed != 0 {
                        self.sync_heap_type_registry();
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
                let pc = self.running.frames.last().unwrap().pc;
                let logical_cost = self.running.frames.last().unwrap().prepared.costs[pc].max(1);
                self.running.step_grant.as_mut().unwrap().consume();
                if self.running.charged_instruction_steps + 1 < logical_cost {
                    self.running.charged_instruction_steps += 1;
                    self.steps = self.steps.saturating_add(1);
                    self.last_poll_steps += 1;
                    self.timer_check_steps += 1;
                    self.running.scheduling_phase =
                        (self.running.scheduling_phase + 1) % TASK_POLL_INTERVAL;
                    self.changed_scopes.insert(self.running.scope);
                    continue;
                }
                self.running.charged_instruction_steps = 0;
            }
            let scope = self.running.scope;
            let task_id = self.running.id;
            let outcome = if let Some(error) = pending_error {
                Err(error)
            } else {
                (|| {
                    if let Some(request) = self.running.write_collection.request.take() {
                        self.collect_pending_write(request)?;
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
            if let Err(mut error) = outcome {
                if error.code == "collection_required" {
                    error = RuntimeError::new(
                        "allocation_limit",
                        "guest",
                        "guest allocation byte limit exceeded",
                    );
                }
                if error.code == "allocation_limit" && counted_instruction {
                    self.running.finish_instruction();
                    self.running.transient_roots.clear();
                }
                if error.code == "panic" && !self.running.frames.is_empty() {
                    let panic = self.allocate_private(Value::string(error.message))?;
                    let frame = self.running.frames.last_mut().unwrap();
                    frame.panic = Some(panic);
                    frame.returning = Some(0);
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
                        if let Err(cleanup) = self.abort() {
                            error.message = format!("{}; abort cleanup: {cleanup}", error.message);
                        }
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
                let popped_frame_suspended = task.instruction_frame_suspended;
                task.finish_instruction();
                task.transient_roots.clear();
                if counted_instruction {
                    task.scheduling_phase = (task.scheduling_phase + 1) % TASK_POLL_INTERVAL;
                }
                let popped_frame = task.popped_frame.take();
                let frame = if popped_frame_suspended {
                    task.suspended_frames.last_mut()
                } else {
                    popped_frame.and_then(|index| task.frames.get_mut(index))
                };
                if let Some(frame) = frame {
                    frame.popped_roots.clear();
                }
            }
            if self.debug.paused {
                return Ok(PollStatus::Paused);
            }
            if mutation_pending {
                if self.running.id == task_id
                    && let Some(request) = self.running.write_collection.request.take()
                {
                    // All outputs are now owned by the pending write. Reclaim
                    // at this boundary, then resume only their publication on
                    // the next poll, without charging another opcode.
                    if let Err(error) = self.collect_pending_write(request) {
                        self.running.pending_error = Some(error);
                    }
                }
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
            if self.scope_cancellations.contains_key(scope)
                && let Err(error) = self.cancel_scope(*scope)
            {
                self.last_error_scope = Some(*scope);
                return Err(error);
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

    pub(crate) fn take_last_error_scope(&mut self) -> Option<u64> {
        self.last_error_scope.take()
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
        // Cancellation releases task/frame owners immediately. Unreachable
        // payload remains charged until an allocation or explicit collection
        // reaches the collector; cancelling a scope is not a heap safepoint.
        Ok(())
    }

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
        if self.closed || self.cleanup.is_some() || self.cleanup_result.is_some() {
            return;
        }
        self.debug = debug::DebugState::default();
        self.closed = true;
        self.lifetime.cancel();
        self.ffi_calls.close();
        self.globals.clear();
        self.constant_values.clear();
        self.frame_pool.clear();
        self.retired_revisions.clear();
        self.reflected_types.clear();
        self.reflected_type_indices.clear();
        self.reflected_type_keys.clear();
        self.destroy_channel_records();
        // Drop dynamic metadata owner records together with the registry. In
        // normal execution their leases are released when the last managed
        // runtimeType handle is reclaimed; close is the final owner boundary.
        self.dynamic_type_costs.clear();
        self.dynamic_type_identities.clear();
        self.dynamic_type_leases.clear();
        self.dynamic_type_owner_counts.clear();
        self.dynamic_type_owners.clear();
        self.interface_assignments.get_mut().unwrap().clear();
        self.failed_initializations.clear();
        self.types.clear_dynamic();
        self.revision_anchors.clear();
        self.scope_steps.clear();
        let session = self.ffi_session.take();
        self.cleanup = Some(Box::pin(async move {
            match session {
                Some(session) => session.shutdown_async().await,
                None => Ok(()),
            }
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
        if let Err(error) = result {
            self.cleanup_result = Some(Err(error.clone()));
            return std::task::Poll::Ready(Err(error));
        }
        // The owner itself can still have a TLS lease when a host calls
        // Instance::close directly.  Return that lease before waiting for
        // executor workers; otherwise the active-worker gate would wait on
        // the current thread forever.
        if let Err(error) = self.heap.flush_worker_lease() {
            self.cleanup_result = Some(Err(error.clone()));
            return std::task::Poll::Ready(Err(error));
        }
        // Executor workers own thread-local nursery leases.  Closing the
        // directory while one is still active would discard its page and
        // make the worker's eventual handoff stale.  Keep the close future
        // pending until every worker has returned its lease; the worker pool
        // wakes the owner through the normal execution wake path.
        if self.heap.active_worker_leases()? != 0 {
            cx.waker().wake_by_ref();
            return std::task::Poll::Pending;
        }
        let result = self.abort().and_then(|()| self.heap.close());
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
                revision_generation: None,
                revision_anchor: None,
                module: module.into(),
                function: "fn.init".into(),
                captures: Vec::new(),
            },
            Vec::new(),
            0,
            true,
            0,
        );
        if let Err(error) = prepared {
            self.failed_initializations
                .insert(module.to_owned(), error.clone());
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
            frame.delivery.handles.clear();
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
                self.running.frames.last_mut().unwrap().returning = Some(0);
                return self.finish_frame();
            }
            self.running.frames.last_mut().unwrap().pc += 1;
            pc
        };
        let instruction = function.code.descriptor(pc);
        if let Err(error) = self.begin_micro_instruction(pc) {
            if error.code != "collection_required" {
                return Err(error);
            }
            // Non-scalar constants are materialized while preparing the
            // operand lanes. Keep the instruction at its original PC and
            // retry that preparation after the owner collection; unlike a
            // pending address write there is no guest-visible mutation yet.
            self.collect_rooted()?;
            self.begin_micro_instruction(pc)?;
        }
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
                        self.store_local_retry_after_collection(binding, selected, false)?;
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
                        self.store_local_retry_after_collection(index, value, false)?;
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
                    value = value.element(step.index)?;
                }
                result = Some(value);
            }
            Prepared::Unary(operator) => {
                let value = self.running.pop()?;
                result = Some(crate::operators::unary(operator, value, &self.types)?);
            }
            Prepared::LenCompareBranch {
                operator,
                target,
                when,
            } => {
                let frame = self.running.frames.last().unwrap();
                let object = frame.operand_from_end(pc, 1).unwrap().clone();
                let right = frame.operand_from_end(pc, 0).unwrap().clone();
                let length = self.container_length(object, false)?;
                let matched = crate::operators::compare_length(operator.operator, length, &right)?;
                let frame = self.running.frames.last_mut().unwrap();
                frame.slot_input = 0;
                if matched == when {
                    frame.pc = target;
                }
            }
            Prepared::UpdateCompareBranch {
                update,
                comparison,
                target,
                when,
            } => {
                let comparison_value = self.running.pop()?;
                let update_value = self.running.pop()?;
                let current_value = self.running.pop()?;
                let updated =
                    crate::operators::binary(update, &current_value, &update_value, &self.types)?;
                let local = function.operands.inputs_at(pc)[0].index as usize;
                self.store_local_retry_after_collection(local, updated.clone(), false)?;
                let matched =
                    crate::operators::binary(comparison, &updated, &comparison_value, &self.types)?;
                let Data::Bool(matched) = matched.data else {
                    unreachable!("comparison returns bool")
                };
                if matched == when {
                    self.running.frames.last_mut().unwrap().pc = target;
                }
            }
            Prepared::Binary(operator) | Prepared::CompareBranch { operator, .. } => {
                // Inputs stay rooted in their slots until commit, including
                // while string allocation requests collection.
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
                }
                let frame = self.running.frames.last().unwrap();
                let (left_value, right_value) = (
                    frame.operand_from_end(pc, 1).unwrap().clone(),
                    frame.operand_from_end(pc, 0).unwrap().clone(),
                );
                let value =
                    crate::operators::binary(operator, &left_value, &right_value, &self.types)?;
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
            Prepared::MinMax {
                kind,
                choose_min,
                arg_count,
            } => {
                let values = self.running.pop_values(&self.frame_pool, arg_count)?;
                let value = crate::operators::min_max(
                    kind,
                    if choose_min { "min" } else { "max" },
                    &values,
                )?;
                self.frame_pool
                    .recycle_operands(values, self.limits.max_frame_cache_bytes);
                result = Some(value);
            }
            Prepared::Operand | Prepared::Construct(_) => match instruction
                .expect("unprepared operation descriptor")
            {
                TypeDispatch(_)
                | CompareBranch(_)
                | LenCompareBranch(_)
                | UpdateCompareBranch(_)
                | GetPath(_)
                | MinMax(_) => {
                    unreachable!("dispatch is prepared")
                }
                Zero(_) => {
                    result = Some(self.zero(function.operand_types[pc].as_ref().unwrap())?);
                }
                Pop => {
                    self.running.pop()?;
                }
                AddressOf(payload) => {
                    let mut address = self.resolve_address(payload)?;
                    let numeric = address.identity.numeric;
                    address.reset_identity(address.identity.path_root);
                    Arc::make_mut(&mut address.identity).numeric = numeric;
                    let pointee = self.snapshot_address(&address)?.typ.clone();
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
                    let layout = &function.aggregates[index];
                    let typ = function.operand_types[pc].as_ref().unwrap().clone();
                    if self.types.storage_logical_bytes(&typ)? > self.limits.max_guest_bytes {
                        return Err(RuntimeError::new(
                            "allocation_limit",
                            "make_struct",
                            "struct layout exceeds heap budget",
                        ));
                    }
                    let mut fields = Vec::with_capacity(layout.fields.len());
                    for (field_type, input) in &layout.fields {
                        fields.push(if let Some(input) = input {
                            self.coerce(
                                std::mem::replace(&mut values[*input], Value::int(0)),
                                field_type,
                            )?
                        } else {
                            self.zero(field_type)?
                        });
                    }
                    result = Some(Value {
                        typ,
                        data: Data::Struct(crate::value::StructStorage::with_layout(
                            layout.names.clone(),
                            fields,
                        )),
                    });
                }
                MakeSequence(payload) => {
                    let values = self
                        .running
                        .pop_values(&self.frame_pool, payload.element_count as usize)?;
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
                        let element = self.types.resolve(module, &node.elem)?;
                        let mut elements = Vec::with_capacity(values.len());
                        for value in values {
                            elements.push(self.coerce(value, &element)?);
                        }
                        let value = Value {
                            typ,
                            data: Data::Array(elements.into()),
                        };
                        if self.heap.logical_allocation_bytes(&value)? > self.limits.max_guest_bytes
                        {
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
                    let length = values[0].index()?;
                    let capacity = if payload.has_capacity {
                        values[1].index()?
                    } else {
                        length
                    };
                    if capacity < length {
                        return Err(RuntimeError::new(
                            "panic",
                            "make_slice",
                            "invalid slice length or capacity",
                        ));
                    }
                    let typ = function.operand_types[pc].as_ref().unwrap().clone();
                    self.running.pending_write = Some(mutation::PendingWrite::Slice {
                        typ,
                        length,
                        capacity,
                    });
                    self.resume_write()?;
                }
                MakeMap(payload) => {
                    let entry_values = payload.entry_count as usize * 2;
                    let mut values = self.running.pop_values(
                        &self.frame_pool,
                        entry_values + usize::from(payload.has_capacity),
                    )?;
                    if payload.has_capacity {
                        values.pop().unwrap().index()?;
                    }
                    let typ = function.operand_types[pc].as_ref().unwrap().clone();
                    let mut entries = VecDeque::with_capacity(payload.entry_count as usize);
                    let mut values = values.into_iter();
                    while let Some(key) = values.next() {
                        entries.push_back((key, values.next().unwrap()));
                    }
                    self.running.pending_write = Some(mutation::PendingWrite::MapLiteral {
                        typ,
                        map: None,
                        entries,
                    });
                    self.resume_write()?;
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
                    result = Some(value.field(&payload.field)?);
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
                        self.push_result(value)?;
                        result = Some(Value::boolean(ok));
                    } else {
                        result = Some(value);
                    }
                }
                IndexConvert(_) => {
                    let key = self.running.pop()?;
                    let object = self.running.pop()?;
                    let (value, _) = self.index_value(&object, &key)?;
                    let typ = function.operand_types[pc].as_ref().unwrap().clone();
                    result = Some(self.convert_value(value, typ)?);
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
                            revision_generation: Some(revision.generation),
                            revision_anchor: self
                                .revision_anchors
                                .get(&revision.generation)
                                .copied(),
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
                        match &caller.input_value(0)?.data {
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
                        )?;
                        let start = caller.slot_input.checked_sub(count).ok_or_else(|| {
                            RuntimeError::new("invalid_operand", "frame", "input underflow")
                        })?;
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
                        let bound_revision = revision.clone();
                        let target_index = source.index;
                        let target = &bound_revision.program.function_table[target_index];
                        let revision_generation = bound_revision.generation;
                        FunctionValue {
                            index: Some(target_index),
                            module: target.module.clone(),
                            function: target.name.clone(),
                            revision: Some(bound_revision),
                            revision_generation: Some(revision_generation),
                            revision_anchor: None,
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
                        self.running.instruction_frame_suspended = true;
                    }
                    let frame_index = self.running.frames.len();
                    let created = self.push_frame_arguments(
                        callee,
                        borrowed_arguments.unwrap_or(frame::FrameArguments::Owned(arguments)),
                        payload.result_count as usize,
                        false,
                        0,
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
                        // Caller slots still own their values after a failed
                        // frame build. Their leases therefore remain attached
                        // to the caller and are released by normal liveness,
                        // rather than being reconstructed at the next GC.
                        return Err(error);
                    }
                    if let Some(mut old) = old {
                        self.cache_frame(&mut old)?;
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
                            revision_generation: None,
                            revision_anchor: None,
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
                        0,
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
                    let old_capacity = frame.memory.deferred;
                    let new_capacity =
                        memory::grow_frame_buffer(old_capacity, frame.defers.len() + 1)?;
                    frame.ensure_resident_growth(
                        old_capacity,
                        new_capacity,
                        std::mem::size_of::<Value>(),
                    )?;
                    frame.memory.deferred = new_capacity;
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
                    // Unwinding may cross deferred frames and collection
                    // boundaries. Store the panic payload in the managed
                    // heap before publishing it on the frame so its nested
                    // references remain visible to the collector.
                    let panic = self.allocate_private(value)?;
                    let frame = self.running.frames.last_mut().unwrap();
                    frame.panic = Some(panic);
                    frame.returning = Some(0);
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
                        && let Some(panic) = self.running.frames[top - 1].panic
                    {
                        result = Some(self.heap.read_typed_value(panic)?);
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
                                index: revision
                                    .program
                                    .function_index(&payload.module_path, &export.id),
                                revision: Some(self.revision.clone()),
                                revision_generation: Some(self.revision.generation),
                                revision_anchor: None,
                                module: payload.module_path.clone().into(),
                                function: export.id.clone().into(),
                                captures: Vec::new(),
                            })),
                        },
                        "global" => self.heap.read_typed_value(
                            self.globals[&(payload.module_path.clone(), export.id.clone())],
                        )?,
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
                    let value = self.running.pop()?;
                    result = Some(Value::int(self.container_length(value, capacity)?));
                }
                Slice => {
                    let mut values = self.running.pop_values(&self.frame_pool, 4)?;
                    let max = values.pop().unwrap();
                    let high = values.pop().unwrap().index()?;
                    let low = values.pop().unwrap().index()?;
                    let object = values.pop().unwrap();
                    let maximum = (max.typ != TypeIdentity::Void)
                        .then(|| max.index())
                        .transpose()?;
                    result = Some(self.slice_value(object, low, high, maximum)?);
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
                            self.managed_maps
                                .get_mut(*root)
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "invalid_map",
                                        "clear",
                                        "map owner is missing",
                                    )
                                })?
                                .clear()?;
                        }
                        Data::Slice(slice) => {
                            let zero = self.zero(&self.element_type(&object.typ)?)?;
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
                    let local = self
                        .running
                        .frames
                        .last()
                        .and_then(|frame| {
                            frame
                                .prepared
                                .map_iterator_indices
                                .get(frame.pc.saturating_sub(1))
                        })
                        .and_then(|local| *local)
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "invalid_iterator",
                                "iterator",
                                format!("iterator local is not prepared: {}", payload.local),
                            )
                        })?;
                    let object = self.running.pop()?;
                    self.init_map_iterator(local, object)?;
                }
                MapIterNext(payload) => {
                    let local = self
                        .running
                        .frames
                        .last()
                        .and_then(|frame| {
                            frame
                                .prepared
                                .map_iterator_indices
                                .get(frame.pc.saturating_sub(1))
                        })
                        .and_then(|local| *local)
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "invalid_iterator",
                                "iterator",
                                format!("iterator local is not prepared: {}", payload.local),
                            )
                        })?;
                    let (key, value, ok) = self.next_map_entry(local)?;
                    self.extend_results([key, value])?;
                    result = Some(Value::boolean(ok));
                }
                MapIterClose(payload) => {
                    let local = self
                        .running
                        .frames
                        .last()
                        .and_then(|frame| {
                            frame
                                .prepared
                                .map_iterator_indices
                                .get(frame.pc.saturating_sub(1))
                        })
                        .and_then(|local| *local)
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "invalid_iterator",
                                "iterator",
                                format!("iterator local is not prepared: {}", payload.local),
                            )
                        })?;
                    if let Some(iterator) = self
                        .running
                        .frames
                        .last_mut()
                        .unwrap()
                        .map_iterators
                        .get_mut(local)
                    {
                        *iterator = None;
                    }
                }
                MapKeys => {
                    let object = self.running.pop()?;
                    let values = match &object.data {
                        Data::Map(root) => self
                            .materialize_managed_map(*root, &object.typ)?
                            .into_iter()
                            .map(|(key, _)| key)
                            .collect(),
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
                    let index = self.running.pop()?.index()?;
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
                    let dynamic_type = match &value.data {
                        Data::DynamicFunction(callback) => &callback.typ,
                        _ => &value.typ,
                    };
                    let ok = !nil_interface
                        && (self.types.identical(dynamic_type, &typ)?
                            || self.types.implements(dynamic_type, &typ)?);
                    if !with_ok && !ok {
                        return Err(RuntimeError::new(
                            "panic",
                            "type_assert",
                            "dynamic type does not match assertion",
                        ));
                    }
                    let value = if ok {
                        if matches!(value.data, Data::DynamicFunction(_)) {
                            Value {
                                typ: typ.clone(),
                                data: value.data,
                            }
                        } else {
                            self.coerce(value, &typ)?
                        }
                    } else {
                        self.zero(&typ)?
                    };
                    if with_ok {
                        self.push_result(value)?;
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
            self.push_result(value)?;
        }
        Ok(())
    }

    fn store_local_retry_after_collection(
        &mut self,
        index: usize,
        value: Value,
        rebind: bool,
    ) -> Result<(), RuntimeError> {
        value.trace(&mut |handle| self.running.transient_roots.push(handle));
        match self.store_local(index, value.clone(), rebind) {
            Err(error) if error.code == "collection_required" => {
                self.collect_rooted()?;
                self.store_local(index, value, rebind)
            }
            outcome => outcome,
        }
    }
}
