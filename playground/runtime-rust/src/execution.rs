//! Shared execution control. An owner takes the machine out of the control lock
//! before running guest instructions or invoking any external host code.

use crate::{
    error::RuntimeError,
    ffi::{Cancellation, Wake},
    heap::HeapStats,
    instance::{ExecutionLimits, Instance, PollStatus},
    program::Program,
    snapshot::{HostSnapshot, SnapshotLimits},
    value::Value,
};
#[cfg(not(target_arch = "wasm32"))]
use std::sync::Weak;
use std::{
    cell::Cell,
    collections::BTreeMap,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::Duration,
};

thread_local! {
    /// Synchronous host callbacks must not wait for a VM owner held by the
    /// same thread. This also keeps a pool worker from blocking on reentry.
    static OWNER_DEPTH: Cell<usize> = const { Cell::new(0) };
}

/// Bounded native worker pool shared by any number of instances. Instances do
/// not own threads; closing an executor is an explicit host action after all
/// attached instances have stopped.
#[cfg(not(target_arch = "wasm32"))]
#[derive(Clone)]
pub struct Executor {
    inner: Arc<ExecutorInner>,
}

#[cfg(not(target_arch = "wasm32"))]
struct ExecutorInner {
    pool: Arc<crate::executor_pool::Pool>,
    controls: Mutex<Vec<Weak<Control>>>,
    closing: AtomicBool,
    shared: bool,
}

#[cfg(not(target_arch = "wasm32"))]
impl Executor {
    pub fn new(workers: usize) -> Result<Self, RuntimeError> {
        Ok(Self {
            inner: Arc::new(ExecutorInner {
                pool: crate::executor_pool::Pool::new(workers)?,
                controls: Mutex::new(Vec::new()),
                closing: AtomicBool::new(false),
                shared: false,
            }),
        })
    }

    pub fn workers(&self) -> usize {
        self.inner.pool.worker_count()
    }

    fn shared() -> Result<Self, RuntimeError> {
        static SHARED: std::sync::OnceLock<Result<Executor, RuntimeError>> =
            std::sync::OnceLock::new();
        SHARED
            .get_or_init(|| {
                Ok(Executor {
                    inner: Arc::new(ExecutorInner {
                        pool: crate::executor_pool::Pool::shared()?,
                        controls: Mutex::new(Vec::new()),
                        closing: AtomicBool::new(false),
                        shared: true,
                    }),
                })
            })
            .clone()
    }

    fn register(&self, control: &Arc<Control>) -> Result<(), RuntimeError> {
        if self.inner.closing.load(Ordering::Acquire) {
            return Err(RuntimeError::new(
                "closed",
                "executor",
                "executor is closing",
            ));
        }
        let mut controls = self.inner.controls.lock().unwrap();
        controls.retain(|control| control.strong_count() != 0);
        if self.inner.closing.load(Ordering::Acquire) {
            return Err(RuntimeError::new(
                "closed",
                "executor",
                "executor is closing",
            ));
        }
        controls.push(Arc::downgrade(control));
        Ok(())
    }

    /// Requests shutdown of every attached instance, waits for their owned
    /// cleanup, and then stops the host-created worker pool. Canceling this
    /// wait does not undo the shutdown request; a later call may finish it.
    pub fn shutdown(&self, cancellation: &Cancellation) -> Result<(), RuntimeError> {
        if self.inner.shared {
            return Err(RuntimeError::new(
                "executor",
                "shutdown",
                "the process-wide executor cannot be shut down",
            ));
        }
        if OWNER_DEPTH.with(Cell::get) != 0 {
            return Err(RuntimeError::new(
                "busy",
                "executor",
                "executor shutdown cannot synchronously reenter its VM owner",
            ));
        }
        self.inner.closing.store(true, Ordering::Release);
        loop {
            let controls = {
                let mut controls = self.inner.controls.lock().unwrap();
                controls.retain(|control| control.strong_count() != 0);
                controls
                    .iter()
                    .filter_map(Weak::upgrade)
                    .collect::<Vec<_>>()
            };
            let mut pending = None;
            for control in controls {
                if control.state.lock().unwrap().shutdown.is_none() {
                    let observed = control.wake.epoch();
                    control.closing.store(true, Ordering::Release);
                    control.wake.signal();
                    pending.get_or_insert((control.wake.clone(), observed));
                }
            }
            let Some((wake, observed)) = pending else {
                self.inner.pool.shutdown();
                return Ok(());
            };
            if cancellation.is_cancelled() {
                return Err(RuntimeError::new(
                    "canceled",
                    "executor",
                    "caller stopped waiting for executor shutdown",
                ));
            }
            wake.wait(observed, Duration::from_millis(10));
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ExecutionState {
    Running,
    Pending,
    Paused,
    Completed,
    Failed,
    Canceled,
}

#[derive(Clone, Debug)]
pub struct ScopeStats {
    pub id: u64,
    pub started: crate::instance::patch::RevisionInfo,
    pub root_state: ExecutionState,
    pub tasks: usize,
    pub timers: usize,
    pub ffi_calls: usize,
    pub steps: u64,
    pub done: bool,
    pub error: Option<RuntimeError>,
}

pub struct InstanceOptions {
    /// Cancellation applies to construction and root initialization only.
    pub cancellation: Cancellation,
    pub limits: ExecutionLimits,
    pub bridge: Option<Arc<dyn crate::ffi::Bridge>>,
    pub clock: Arc<dyn crate::environment::Clock>,
    pub entropy: Arc<dyn crate::environment::Entropy>,
    /// Maximum guest tasks that may execute concurrently in this instance.
    /// Zero selects one. Tasks use the process-wide bounded executor and do
    /// not own operating-system threads.
    pub parallelism: usize,
    #[cfg(not(target_arch = "wasm32"))]
    pub executor: Option<Executor>,
    #[cfg(test)]
    pub(crate) task_observer: Option<Arc<dyn Fn(u64, bool) + Send + Sync>>,
}

impl Default for InstanceOptions {
    fn default() -> Self {
        Self {
            cancellation: Cancellation::default(),
            limits: ExecutionLimits::default(),
            bridge: None,
            clock: Arc::new(crate::environment::SystemClock::default()),
            entropy: Arc::new(crate::environment::SystemEntropy),
            parallelism: 1,
            #[cfg(not(target_arch = "wasm32"))]
            executor: None,
            #[cfg(test)]
            task_observer: None,
        }
    }
}

struct Outcome {
    state: ExecutionState,
    result: Option<Arc<HostSnapshot>>,
    error: Option<RuntimeError>,
}
struct Record {
    scope: u64,
    cancellation: Cancellation,
    settled: AtomicBool,
    outcome: Mutex<Outcome>,
    scope_error: Mutex<Option<RuntimeError>>,
    stats: Mutex<ScopeStats>,
    profile: Mutex<crate::instance::debug::Profile>,
}

impl Record {
    fn refresh(&self, machine: &Instance) {
        let work = machine.scope_work(self.scope);
        let root_state = self.outcome.lock().unwrap().state;
        let error = self.scope_error.lock().unwrap().clone();
        let mut stats = self.stats.lock().unwrap();
        stats.root_state = root_state;
        stats.tasks = work.tasks;
        stats.timers = work.timers;
        stats.ffi_calls = work.ffi_calls;
        stats.steps = stats.steps.max(work.steps);
        stats.done = !machine.scope_active(self.scope);
        stats.error = error;
    }
    fn fail(&self, error: RuntimeError, canceled: bool) {
        self.scope_error
            .lock()
            .unwrap()
            .get_or_insert_with(|| error.clone());
        let mut outcome = self.outcome.lock().unwrap();
        if matches!(
            outcome.state,
            ExecutionState::Running | ExecutionState::Pending | ExecutionState::Paused
        ) {
            outcome.state = if canceled {
                ExecutionState::Canceled
            } else {
                ExecutionState::Failed
            };
            outcome.error = Some(error);
        }
    }
}

struct State {
    machine: Option<Instance>,
    #[cfg(not(target_arch = "wasm32"))]
    batch: Option<crate::instance::task_runner::TaskBatch>,
    supervisor_waiting: bool,
    records: BTreeMap<u64, Arc<Record>>,
    shutdown: Option<Result<(), RuntimeError>>,
    stats: crate::instance::stats::Stats,
}
struct Control {
    state: Mutex<State>,
    wake: Arc<Wake>,
    #[cfg(not(target_arch = "wasm32"))]
    supervised: bool,
    closing: AtomicBool,
    control_waiters: AtomicUsize,
    handles: AtomicUsize,
    pause: Arc<AtomicBool>,
    close_sender: std::sync::mpsc::Sender<Arc<Control>>,
    #[cfg(not(target_arch = "wasm32"))]
    parallelism: usize,
    #[cfg(not(target_arch = "wasm32"))]
    task_observer: Option<Arc<dyn Fn(u64, bool) + Send + Sync>>,
    #[cfg(not(target_arch = "wasm32"))]
    executor: Executor,
}

/// Cloneable public handle. Dropping the final instance handle starts shutdown;
/// outstanding Execution values retain only their results and control state.
pub struct SharedInstance {
    control: Arc<Control>,
}
pub struct Execution {
    control: Arc<Control>,
    record: Arc<Record>,
}

/// Inspection and resume requests use the same owner lease as execution.
pub struct Debugger {
    control: Arc<Control>,
}

impl Debugger {
    pub fn set_break_on_panic(&self, enabled: bool) -> Result<(), RuntimeError> {
        Owner::acquire_control(&self.control)?
            .machine
            .as_mut()
            .unwrap()
            .set_break_on_panic(enabled)
    }
    pub fn pause(&self) {
        self.control.pause.store(true, Ordering::Release);
        self.control.wake.signal();
    }

    pub fn set_breakpoints(
        &self,
        module: &str,
        file: &str,
        lines: &[i64],
    ) -> Result<Vec<i64>, RuntimeError> {
        let mut owner = Owner::acquire_control(&self.control)?;
        owner
            .machine
            .as_mut()
            .unwrap()
            .set_breakpoints(module, file, lines)
    }

    pub fn events(&self) -> Result<Vec<crate::instance::debug::DebugEvent>, RuntimeError> {
        let owner = Owner::acquire_control(&self.control)?;
        Ok(owner.machine.as_ref().unwrap().debug_events())
    }

    pub fn threads(&self) -> Result<Vec<crate::instance::debug::ThreadInfo>, RuntimeError> {
        let owner = Owner::acquire_control(&self.control)?;
        Ok(owner.machine.as_ref().unwrap().debug_threads())
    }

    pub fn stack(&self) -> Result<Vec<crate::instance::debug::FrameInfo>, RuntimeError> {
        Owner::acquire_control(&self.control)?
            .machine
            .as_ref()
            .unwrap()
            .debug_stack()
    }

    pub fn bindings(
        &self,
        frame: &crate::instance::debug::FrameRef,
        limits: SnapshotLimits,
    ) -> Result<crate::instance::debug::Bindings, RuntimeError> {
        Owner::acquire_control(&self.control)?
            .machine
            .as_ref()
            .unwrap()
            .debug_bindings(frame, limits)
    }

    pub fn variables(
        &self,
        frame: &crate::instance::debug::FrameRef,
        offset: usize,
        limit: usize,
    ) -> Result<Vec<crate::instance::debug::VariableInfo>, RuntimeError> {
        Owner::acquire_control(&self.control)?
            .machine
            .as_ref()
            .unwrap()
            .debug_variables(frame, offset, limit)
    }

    pub fn children(
        &self,
        reference: &crate::instance::debug::VariableRef,
        offset: usize,
        limit: usize,
    ) -> Result<Vec<crate::instance::debug::VariableInfo>, RuntimeError> {
        Owner::acquire_control(&self.control)?
            .machine
            .as_ref()
            .unwrap()
            .debug_children(reference, offset, limit)
    }

    pub fn resume(&self, mode: crate::instance::debug::StepMode) -> Result<(), RuntimeError> {
        self.resume_selected(mode, None)
    }

    pub fn resume_task(
        &self,
        mode: crate::instance::debug::StepMode,
        task: u64,
    ) -> Result<(), RuntimeError> {
        self.resume_selected(mode, Some(task))
    }

    fn resume_selected(
        &self,
        mode: crate::instance::debug::StepMode,
        task: Option<u64>,
    ) -> Result<(), RuntimeError> {
        let mut owner = Owner::acquire_control(&self.control)?;
        let machine = owner.machine.as_mut().unwrap();
        if let Some(task) = task {
            machine.debug_resume_task(mode, task)?;
        } else {
            machine.debug_resume(mode)?;
        }
        if let Some(scope) = machine.foreground_scope()
            && let Some(record) = owner.records.get(&scope)
        {
            let mut outcome = record.outcome.lock().unwrap();
            if outcome.state == ExecutionState::Paused {
                outcome.state = ExecutionState::Running;
                machine.changed_scopes.insert(scope);
            }
        }
        owner.notify = true;
        Ok(())
    }

    pub fn start_profile(&self, sample_every: u64, max_samples: usize) -> Result<(), RuntimeError> {
        Owner::acquire_control(&self.control)?
            .machine
            .as_mut()
            .unwrap()
            .start_profile(sample_every, max_samples)
    }

    pub fn profile(&self) -> Result<crate::instance::debug::Profile, RuntimeError> {
        Ok(Owner::acquire_control(&self.control)?
            .machine
            .as_ref()
            .unwrap()
            .profile())
    }
}

struct Owner {
    control: Arc<Control>,
    machine: Option<Instance>,
    records: BTreeMap<u64, Arc<Record>>,
    notify: bool,
    shutdown: Option<Result<(), RuntimeError>>,
    #[cfg(not(target_arch = "wasm32"))]
    batch: Option<crate::instance::task_runner::TaskBatch>,
}

impl Owner {
    fn acquire(control: &Arc<Control>) -> Result<Self, RuntimeError> {
        if control.control_waiters.load(Ordering::Acquire) != 0 {
            return Err(RuntimeError::new(
                "busy",
                "owner",
                "an instance control request is pending",
            ));
        }
        Self::acquire_raw(control)
    }

    fn acquire_raw(control: &Arc<Control>) -> Result<Self, RuntimeError> {
        let mut state = control.state.lock().unwrap();
        #[cfg(not(target_arch = "wasm32"))]
        if let Some(batch) = state.batch.as_mut() {
            let Some(runs) = batch.try_complete()? else {
                return Err(RuntimeError::new(
                    "busy",
                    "owner",
                    "guest task slices are active",
                ));
            };
            state.machine.as_mut().unwrap().merge_parallel_runs(runs);
            state.batch = None;
        }
        let machine = state
            .machine
            .take()
            .ok_or_else(|| RuntimeError::new("busy", "owner", "instance already has an owner"))?;
        OWNER_DEPTH.with(|depth| depth.set(depth.get() + 1));
        Ok(Self {
            control: control.clone(),
            machine: Some(machine),
            records: std::mem::take(&mut state.records),
            notify: false,
            shutdown: None,
            #[cfg(not(target_arch = "wasm32"))]
            batch: None,
        })
    }

    /// Acquire the all-task boundary used by GC, patching and debugger
    /// inspection. Only the external caller waits. Publishing the request
    /// prevents the supervisor and execution handles from starting another
    /// batch after the current finite slices return.
    fn acquire_control(control: &Arc<Control>) -> Result<Self, RuntimeError> {
        if OWNER_DEPTH.with(Cell::get) != 0 {
            return Err(RuntimeError::new(
                "busy",
                "owner",
                "synchronous VM reentry is not allowed",
            ));
        }
        #[cfg(target_arch = "wasm32")]
        {
            Self::acquire_raw(control)
        }
        #[cfg(not(target_arch = "wasm32"))]
        {
            control.control_waiters.fetch_add(1, Ordering::AcqRel);
            // A supervised instance may be asleep in its executor or have
            // finite task slices in flight. External drivers have no
            // supervisor to wake; an active owner publishes a notification
            // when it observes the waiter.
            if control.supervised {
                control.wake.signal();
            }
            loop {
                let observed = control.wake.epoch();
                match Self::acquire_raw(control) {
                    Ok(owner) => {
                        control.control_waiters.fetch_sub(1, Ordering::AcqRel);
                        return Ok(owner);
                    }
                    Err(error) if error.code == "busy" => {
                        control.wake.wait(observed, Duration::from_millis(10));
                    }
                    Err(error) => {
                        control.control_waiters.fetch_sub(1, Ordering::AcqRel);
                        control.wake.signal();
                        return Err(error);
                    }
                }
            }
        }
    }

    fn maintain(&mut self) -> Result<bool, RuntimeError> {
        if self.control.state.lock().unwrap().shutdown.is_some() {
            return Ok(false);
        }
        let machine = self.machine.as_mut().unwrap();
        if self.control.closing.load(Ordering::Acquire) {
            for record in self.records.values() {
                *record.profile.lock().unwrap() = machine.profile_for_scope(record.scope);
                record.fail(
                    RuntimeError::new("closed", "execution", "instance is closing"),
                    true,
                );
            }
            let waker = std::task::Waker::from(self.control.wake.clone());
            let mut cx = std::task::Context::from_waker(&waker);
            if let std::task::Poll::Ready(result) = machine.poll_close(&mut cx) {
                self.shutdown = Some(result);
            }
            self.notify = self.shutdown.is_some();
            return Ok(false);
        }
        for scope in machine.cancel_requested_scopes()? {
            if let Some(record) = self.records.get(&scope) {
                self.notify = true;
                record.fail(
                    RuntimeError::new("canceled", "execution", "execution was canceled"),
                    true,
                );
            }
        }
        Ok(true)
    }

    fn poll(
        &mut self,
        record: &Arc<Record>,
        count: usize,
    ) -> Result<(ExecutionState, usize), RuntimeError> {
        if !self.maintain()? {
            return Err(RuntimeError::new(
                "closed",
                "execution",
                "instance is closed",
            ));
        }
        let state = record.outcome.lock().unwrap().state;
        if !matches!(state, ExecutionState::Running | ExecutionState::Pending) {
            return Ok((state, 0));
        }
        let machine = self.machine.as_mut().unwrap();
        if machine.foreground_scope() != Some(record.scope) {
            return Err(RuntimeError::new(
                "stale_execution",
                "execution",
                "execution is no longer active",
            ));
        }
        #[cfg(not(target_arch = "wasm32"))]
        let status = machine.poll_parallel(
            count,
            self.control.parallelism,
            &self.control.executor.inner.pool,
            &self.control.control_waiters,
            self.control.task_observer.as_ref(),
        );
        #[cfg(target_arch = "wasm32")]
        let status = machine.poll_steps(count);
        let steps = machine.last_poll_steps;
        if record.cancellation.is_cancelled() {
            record.refresh(machine);
            machine.cancel_scope(record.scope)?;
            record.fail(
                RuntimeError::new("canceled", "execution", "execution was canceled"),
                true,
            );
            self.notify = true;
            return Ok((ExecutionState::Canceled, steps));
        }
        match status {
            Ok(status) => {
                let mut outcome = record.outcome.lock().unwrap();
                outcome.state = match status {
                    PollStatus::Running => ExecutionState::Running,
                    PollStatus::Pending => ExecutionState::Pending,
                    PollStatus::Paused => ExecutionState::Paused,
                    PollStatus::Ready => ExecutionState::Completed,
                };
                if status == PollStatus::Ready {
                    self.notify = true;
                    match machine.snapshot_results(SnapshotLimits::default()) {
                        Ok(result) => outcome.result = Some(Arc::new(result)),
                        Err(error) => {
                            outcome.state = ExecutionState::Failed;
                            outcome.error = Some(error.clone());
                            return Err(error);
                        }
                    }
                }
            }
            Err(error) => {
                self.notify = true;
                record.fail(error.clone(), false);
                return Err(error);
            }
        }
        machine.changed_scopes.insert(record.scope);
        self.maintain()?;
        Ok((record.outcome.lock().unwrap().state, steps))
    }
}

impl Drop for Owner {
    fn drop(&mut self) {
        OWNER_DEPTH.with(|depth| depth.set(depth.get() - 1));
        let mut machine = self.machine.take().unwrap();
        #[cfg(not(target_arch = "wasm32"))]
        if self.batch.is_some() {
            let mut state = self.control.state.lock().unwrap();
            state.records = std::mem::take(&mut self.records);
            state.machine = Some(machine);
            debug_assert!(state.batch.is_none());
            state.batch = self.batch.take();
            let supervisor_waiting = std::mem::take(&mut state.supervisor_waiting);
            drop(state);
            if self.notify || supervisor_waiting {
                self.control.wake.signal();
            }
            return;
        }
        for scope in std::mem::take(&mut machine.changed_scopes) {
            let Some(record) = self.records.get(&scope) else {
                continue;
            };
            if record.cancellation.is_cancelled() {
                record.fail(
                    RuntimeError::new("canceled", "execution", "execution was canceled"),
                    true,
                );
            }
            record.refresh(&machine);
            if !machine.scope_active(scope) {
                if matches!(
                    machine.state(),
                    crate::instance::stats::InstanceState::Open
                        | crate::instance::stats::InstanceState::Faulted
                ) {
                    *record.profile.lock().unwrap() = machine.profile_for_scope(scope);
                }
                machine.release_scope(scope);
                record.settled.store(true, Ordering::Release);
                self.notify = true;
                self.records.remove(&scope);
            }
        }
        let mut state = self.control.state.lock().unwrap();
        state.stats = machine.stats();
        state.records = std::mem::take(&mut self.records);
        state.machine = Some(machine);
        #[cfg(not(target_arch = "wasm32"))]
        {
            debug_assert!(state.batch.is_none());
            state.batch = self.batch.take();
        }
        let supervisor_waiting = std::mem::take(&mut state.supervisor_waiting);
        if self.shutdown.is_some() {
            state.shutdown = self.shutdown.take();
        }
        drop(state);
        if self.notify
            || supervisor_waiting
            || self.control.control_waiters.load(Ordering::Acquire) != 0
        {
            self.control.wake.signal();
        }
    }
}

impl SharedInstance {
    #[cfg(not(target_arch = "wasm32"))]
    pub fn new(program: Arc<Program>, limits: ExecutionLimits) -> Result<Self, RuntimeError> {
        Self::from_machine(Instance::new(program, limits)?)
    }

    #[cfg(not(target_arch = "wasm32"))]
    pub fn from_machine(mut machine: Instance) -> Result<Self, RuntimeError> {
        machine.initialize_root(&Cancellation::default())?;
        Self::attach(machine, true, 1, Executor::shared()?, None)
    }

    #[cfg(not(target_arch = "wasm32"))]
    pub(crate) fn from_machine_with_options(
        mut machine: Instance,
        parallelism: usize,
        executor: Option<Executor>,
        task_observer: Option<Arc<dyn Fn(u64, bool) + Send + Sync>>,
    ) -> Result<Self, RuntimeError> {
        machine.initialize_root(&Cancellation::default())?;
        Self::attach(
            machine,
            true,
            parallelism,
            match executor {
                Some(executor) => executor,
                None => Executor::shared()?,
            },
            task_observer,
        )
    }

    /// Attach an initialized machine to an external event-loop driver.
    /// Retain this handle and keep driving after `begin_shutdown` until
    /// `shutdown_result` is available, so asynchronous cleanup can finish.
    pub fn externally_driven(machine: Instance) -> Result<Self, RuntimeError> {
        if !machine.root_ready() {
            return Err(RuntimeError::new(
                "pending",
                "instance",
                "root initialization must complete before attaching a driver",
            ));
        }
        Self::attach(
            machine,
            false,
            1,
            #[cfg(not(target_arch = "wasm32"))]
            Executor::shared()?,
            None,
        )
    }

    fn attach(
        machine: Instance,
        supervised: bool,
        parallelism: usize,
        #[cfg(not(target_arch = "wasm32"))] executor: Executor,
        task_observer: Option<Arc<dyn Fn(u64, bool) + Send + Sync>>,
    ) -> Result<Self, RuntimeError> {
        let wake = machine.wake();
        let pause = machine.debug.pause.clone();
        let (close_sender, close_receiver) = std::sync::mpsc::channel();
        let control = Arc::new(Control {
            state: Mutex::new(State {
                supervisor_waiting: false,
                stats: machine.stats(),
                machine: Some(machine),
                #[cfg(not(target_arch = "wasm32"))]
                batch: None,
                records: BTreeMap::new(),
                shutdown: None,
            }),
            wake,
            #[cfg(not(target_arch = "wasm32"))]
            supervised,
            closing: AtomicBool::new(false),
            control_waiters: AtomicUsize::new(0),
            handles: AtomicUsize::new(1),
            pause,
            close_sender,
            #[cfg(not(target_arch = "wasm32"))]
            parallelism: parallelism.max(1),
            #[cfg(not(target_arch = "wasm32"))]
            task_observer,
            #[cfg(not(target_arch = "wasm32"))]
            executor,
        });
        #[cfg(not(target_arch = "wasm32"))]
        control.executor.register(&control)?;
        #[cfg(not(target_arch = "wasm32"))]
        if supervised {
            use crate::executor_pool::Next;
            use std::task::Wake as _;
            let weak = Arc::downgrade(&control);
            let cleanup = Mutex::new((Some(close_receiver), None::<Arc<Control>>));
            let job = control.executor.inner.pool.job(move || {
                let control = {
                    let mut cleanup = cleanup.lock().unwrap();
                    if cleanup.1.is_none() {
                        cleanup.1 = cleanup
                            .0
                            .as_ref()
                            .and_then(|receiver| receiver.try_recv().ok());
                    }
                    cleanup.1.clone().or_else(|| weak.upgrade())
                };
                let Some(control) = control else {
                    return Next::Done;
                };
                if control.state.lock().unwrap().shutdown.is_some() {
                    *cleanup.lock().unwrap() = (None, None);
                    return Next::Done;
                }
                if control.control_waiters.load(Ordering::Acquire) != 0 {
                    control.state.lock().unwrap().supervisor_waiting = true;
                    return Next::Idle;
                }
                let Ok(mut owner) = Owner::acquire(&control) else {
                    let mut state = control.state.lock().unwrap();
                    if state.batch.is_some() {
                        state.supervisor_waiting = true;
                        return Next::Idle;
                    }
                    if state.machine.is_some() {
                        return Next::Ready;
                    }
                    state.supervisor_waiting = true;
                    return Next::Idle;
                };
                match owner.maintain() {
                    Ok(false) => {
                        if owner.shutdown.is_some() {
                            *cleanup.lock().unwrap() = (None, None);
                            return Next::Done;
                        }
                    }
                    Err(error) => {
                        for record in owner.records.values() {
                            record.fail(error.clone(), false);
                        }
                    }
                    Ok(true) => {
                        let machine = owner.machine.as_mut().unwrap();
                        if machine.foreground_scope().is_none() {
                            match machine.start_background_task_batch(
                                control.parallelism,
                                &control.executor.inner.pool,
                                control.task_observer.as_ref(),
                            ) {
                                Ok(Some(batch)) => {
                                    owner.batch = Some(batch);
                                    return Next::Idle;
                                }
                                Ok(None) => match machine.poll_background(1) {
                                    Ok(PollStatus::Running) => return Next::Ready,
                                    Ok(_) => {}
                                    Err(error) => {
                                        for record in owner.records.values() {
                                            record.fail(error.clone(), false);
                                        }
                                    }
                                },
                                Err(error) => {
                                    for record in owner.records.values() {
                                        record.fail(error.clone(), false);
                                    }
                                }
                            }
                        }
                    }
                }
                owner
                    .machine
                    .as_ref()
                    .unwrap()
                    .next_timer_delay()
                    .map_or(Next::Idle, Next::After)
            })?;
            control
                .wake
                .set_supervisor(std::task::Waker::from(job.clone()));
            job.wake_by_ref();
        }
        #[cfg(target_arch = "wasm32")]
        {
            let _ = (supervised, parallelism, task_observer, close_receiver);
        }
        Ok(Self { control })
    }

    /// Drive foreground, background, cancellation and cleanup from one owner.
    pub fn drive(&self, count: usize) -> Result<bool, RuntimeError> {
        if count == 0 {
            return Err(RuntimeError::new(
                "step_limit",
                "poll",
                "poll budget must be positive",
            ));
        }
        let mut owner = Owner::acquire_control(&self.control)?;
        if !owner.maintain()? {
            return Ok(false);
        }
        let scope = owner.machine.as_ref().unwrap().foreground_scope();
        if let Some(record) = scope.and_then(|scope| owner.records.get(&scope).cloned()) {
            return owner
                .poll(&record, count)
                .map(|(state, _)| state == ExecutionState::Running);
        }
        match owner.machine.as_mut().unwrap().poll_background(count) {
            Ok(status) => Ok(status == PollStatus::Running),
            Err(error) => {
                for record in owner.records.values() {
                    record.fail(error.clone(), false);
                }
                Err(error)
            }
        }
    }

    pub fn next_timer_delay(&self) -> Result<Option<Duration>, RuntimeError> {
        Ok(Owner::acquire_control(&self.control)?
            .machine
            .as_ref()
            .unwrap()
            .next_timer_delay())
    }

    pub fn begin_shutdown(&self) {
        if !self.control.closing.swap(true, Ordering::AcqRel) {
            self.control.wake.signal();
        }
    }

    pub fn shutdown_result(&self) -> Option<Result<(), RuntimeError>> {
        self.control.state.lock().unwrap().shutdown.clone()
    }

    pub fn start(&self, entry: &str, arguments: Vec<Value>) -> Result<Execution, RuntimeError> {
        self.start_invocation(|machine| machine.start(entry, arguments))
    }

    pub fn start_host(
        &self,
        entry: &str,
        arguments: &[crate::snapshot::HostValue],
    ) -> Result<Execution, RuntimeError> {
        self.start_invocation(|machine| machine.start_host(entry, arguments))
    }

    fn start_invocation(
        &self,
        start: impl FnOnce(&mut Instance) -> Result<(), RuntimeError>,
    ) -> Result<Execution, RuntimeError> {
        let mut owner = Owner::acquire_control(&self.control)?;
        if !owner.maintain()? {
            return Err(RuntimeError::new(
                "closed",
                "instance",
                "instance is closed",
            ));
        }
        let machine = owner.machine.as_mut().unwrap();
        start(machine)?;
        let record = Arc::new(Record {
            scope: machine.foreground_scope().unwrap(),
            cancellation: Cancellation::default(),
            settled: AtomicBool::new(false),
            outcome: Mutex::new(Outcome {
                state: ExecutionState::Running,
                result: None,
                error: None,
            }),
            scope_error: Mutex::new(None),
            profile: Mutex::new(Default::default()),
            stats: Mutex::new(ScopeStats {
                id: machine.foreground_scope().unwrap(),
                started: machine.revision(),
                root_state: ExecutionState::Running,
                tasks: 1,
                timers: 0,
                ffi_calls: 0,
                steps: 0,
                done: false,
                error: None,
            }),
        });
        machine.watch_scope_cancellation(record.scope, record.cancellation.clone());
        owner.records.insert(record.scope, record.clone());
        owner.notify = true;
        Ok(Execution {
            control: self.control.clone(),
            record,
        })
    }

    pub fn heap_stats(&self) -> HeapStats {
        self.control.state.lock().unwrap().stats.heap
    }

    pub fn collect_garbage(&self) -> Result<HeapStats, RuntimeError> {
        let mut owner = Owner::acquire_control(&self.control)?;
        owner.maintain()?;
        owner.machine.as_mut().unwrap().collect_garbage()
    }

    pub fn stats(&self) -> crate::instance::stats::Stats {
        let mut stats = self.control.state.lock().unwrap().stats.clone();
        if self.control.closing.load(Ordering::Acquire)
            && stats.state != crate::instance::stats::InstanceState::Closed
        {
            stats.state = crate::instance::stats::InstanceState::Closing;
        }
        stats
    }

    pub fn revision(&self) -> Result<crate::instance::patch::RevisionInfo, RuntimeError> {
        let owner = Owner::acquire_control(&self.control)?;
        Ok(owner.machine.as_ref().unwrap().revision())
    }

    pub fn prepare_patch(
        &self,
        target: Arc<Program>,
    ) -> Result<crate::instance::patch::PatchPlan, RuntimeError> {
        let mut owner = Owner::acquire_control(&self.control)?;
        owner.maintain()?;
        owner.machine.as_ref().unwrap().prepare_patch(target)
    }

    pub fn apply_patch(
        &self,
        plan: crate::instance::patch::PatchPlan,
    ) -> Result<crate::instance::patch::PatchResult, RuntimeError> {
        let mut owner = Owner::acquire_control(&self.control)?;
        owner.maintain()?;
        let result = owner.machine.as_mut().unwrap().apply_patch(plan)?;
        owner.notify = true;
        Ok(result)
    }

    pub fn debugger(&self) -> Debugger {
        Debugger {
            control: self.control.clone(),
        }
    }
    pub fn wake(&self) -> Arc<Wake> {
        self.control.wake.clone()
    }

    #[cfg(not(target_arch = "wasm32"))]
    pub fn shutdown(&self, wait: &Cancellation) -> Result<(), RuntimeError> {
        self.begin_shutdown();
        loop {
            let observed = self.control.wake.epoch();
            if let Some(result) = &self.control.state.lock().unwrap().shutdown {
                return result.clone();
            }
            if wait.is_cancelled() {
                return Err(RuntimeError::new(
                    "canceled",
                    "shutdown",
                    "caller stopped waiting for shutdown",
                ));
            }
            self.control.wake.wait(observed, Duration::from_millis(10));
        }
    }
}

impl Clone for SharedInstance {
    fn clone(&self) -> Self {
        self.control.handles.fetch_add(1, Ordering::Relaxed);
        Self {
            control: self.control.clone(),
        }
    }
}

impl Drop for SharedInstance {
    fn drop(&mut self) {
        if self.control.handles.fetch_sub(1, Ordering::AcqRel) == 1 {
            if self.control.state.lock().unwrap().shutdown.is_some() {
                return;
            }
            self.control.closing.store(true, Ordering::Release);
            // Transfer the final cleanup reference to the existing supervisor.
            // Drop never needs to allocate another operating-system thread.
            let _ = self.control.close_sender.send(self.control.clone());
            self.control.wake.signal();
        }
    }
}

impl Execution {
    pub fn profile(&self) -> crate::instance::debug::Profile {
        if !self.scope_settled()
            && let Ok(owner) = Owner::acquire_control(&self.control)
        {
            *self.record.profile.lock().unwrap() = owner
                .machine
                .as_ref()
                .unwrap()
                .profile_for_scope(self.record.scope);
        }
        self.record.profile.lock().unwrap().clone()
    }
    pub fn scope_stats(&self) -> ScopeStats {
        self.record.stats.lock().unwrap().clone()
    }
    pub fn state(&self) -> ExecutionState {
        self.record.outcome.lock().unwrap().state
    }
    pub fn scope_settled(&self) -> bool {
        self.record.settled.load(Ordering::Acquire)
    }

    #[cfg(not(target_arch = "wasm32"))]
    pub fn wait_scope(&self, cancellation: &Cancellation) -> Result<(), RuntimeError> {
        loop {
            let observed = self.control.wake.epoch();
            if self.scope_settled() {
                return self
                    .record
                    .scope_error
                    .lock()
                    .unwrap()
                    .clone()
                    .map_or(Ok(()), Err);
            }
            if cancellation.is_cancelled() {
                return Err(RuntimeError::new(
                    "canceled",
                    "scope",
                    "caller stopped waiting for scope",
                ));
            }
            self.control.wake.wait(observed, Duration::from_millis(10));
        }
    }
    pub fn cancel(&self) {
        self.record.cancellation.cancel();
        self.control.wake.signal();
    }

    pub fn poll_steps(&self, count: usize) -> Result<(ExecutionState, usize), RuntimeError> {
        if count == 0 {
            return Err(RuntimeError::new(
                "step_limit",
                "poll",
                "poll budget must be positive",
            ));
        }
        let state = self.state();
        if !matches!(state, ExecutionState::Running | ExecutionState::Pending) {
            return Ok((state, 0));
        }
        Owner::acquire(&self.control)?.poll(&self.record, count)
    }

    pub fn result(&self) -> Result<Arc<HostSnapshot>, RuntimeError> {
        let outcome = self.record.outcome.lock().unwrap();
        if let Some(error) = &outcome.error {
            return Err(error.clone());
        }
        outcome
            .result
            .clone()
            .ok_or_else(|| RuntimeError::new("pending", "result", "execution has not completed"))
    }

    #[cfg(not(target_arch = "wasm32"))]
    pub fn wait(&self, cancellation: &Cancellation) -> Result<Arc<HostSnapshot>, RuntimeError> {
        loop {
            let observed = self.control.wake.epoch();
            if cancellation.is_cancelled() {
                self.cancel();
                return Err(RuntimeError::new(
                    "canceled",
                    "wait",
                    "caller stopped waiting",
                ));
            }
            match self.poll_steps(4096) {
                Ok((ExecutionState::Completed, _)) => return self.result(),
                Ok((ExecutionState::Canceled | ExecutionState::Failed, _)) => return self.result(),
                Ok((ExecutionState::Paused, _)) => {
                    return Err(RuntimeError::new("paused", "wait", "execution is paused"));
                }
                Ok((ExecutionState::Running, _)) => continue,
                Err(error) if error.code == "busy" => {}
                Err(error) => return Err(error),
                Ok(_) => {}
            }
            self.control.wake.wait(observed, Duration::from_millis(10));
        }
    }
}

#[cfg(all(test, not(target_arch = "wasm32")))]
mod tests {
    use super::*;
    use serde_json::json;
    use std::{
        collections::HashSet,
        sync::{Condvar, mpsc},
    };

    fn parallel_program() -> Arc<Program> {
        parallel_program_with_limit(2_000)
    }

    fn parallel_program_with_limit(limit: i64) -> Arc<Program> {
        let contract: serde_json::Value =
            serde_json::from_str(crate::contract_generated::CONTRACT_JSON).unwrap();
        let integer = json!({"kind":3,"primitive":3});
        let channel = json!({"kind":9,"node":"channel"});
        let mut main = vec![
            json!({"op":"const","payload":{"constant":"capacity"}}),
            json!({"op":"make_waitable","payload":{"type":channel}}),
            json!({"op":"store_global","payload":{"global":"start"}}),
            json!({"op":"const","payload":{"constant":"capacity"}}),
            json!({"op":"make_waitable","payload":{"type":channel}}),
            json!({"op":"store_global","payload":{"global":"ready"}}),
            json!({"op":"const","payload":{"constant":"capacity"}}),
            json!({"op":"make_waitable","payload":{"type":channel}}),
            json!({"op":"store_global","payload":{"global":"release"}}),
            json!({"op":"const","payload":{"constant":"capacity"}}),
            json!({"op":"make_waitable","payload":{"type":channel}}),
            json!({"op":"store_global","payload":{"global":"done"}}),
        ];
        for _ in 0..4 {
            main.extend([
                json!({"op":"make_closure","payload":{"function":"worker"}}),
                json!({"op":"spawn","payload":{"arg_count":0}}),
            ]);
        }
        for _ in 0..4 {
            main.extend([
                json!({"op":"load_global","payload":{"global":"start"}}),
                json!({"op":"zero","payload":{"type":integer}}),
                json!({"op":"waitable_send"}),
            ]);
        }
        for _ in 0..4 {
            main.extend([
                json!({"op":"load_global","payload":{"global":"ready"}}),
                json!({"op":"waitable_recv"}),
                json!({"op":"pop"}),
            ]);
        }
        for _ in 0..4 {
            main.extend([
                json!({"op":"load_global","payload":{"global":"release"}}),
                json!({"op":"zero","payload":{"type":integer}}),
                json!({"op":"waitable_send"}),
            ]);
        }
        let mut background = main.clone();
        background.push(json!({"op":"return","payload":{}}));
        for index in 0..4 {
            main.extend([
                json!({"op":"load_global","payload":{"global":"done"}}),
                json!({"op":"waitable_recv"}),
            ]);
            if index != 3 {
                main.push(json!({"op":"pop"}));
            }
        }
        main.push(json!({"op":"return","payload":{"result_count":1}}));
        let mut artifact = json!({
            "module":{"path":"test","package":"main"},
            "type_table":{"nodes":[{"id":"channel","kind":9,"direction":1,"elem":integer}]},
            "globals":[
                {"id":"start","type":channel},{"id":"ready","type":channel},
                {"id":"release","type":channel},{"id":"done","type":channel}
            ],
            "constants":[
                {"id":"capacity","type":integer,"value":4},
                {"id":"one","type":integer,"value":1},
                {"id":"limit","type":integer,"value":limit}
            ],
            "functions":[
                {"id":"fn.Main","signature":{"results":[integer]},"instructions":main},
                {"id":"fn.Background","instructions":background},
                {"id":"worker","locals":[{"id":"i","type":integer}],"instructions":[
                    {"op":"load_global","payload":{"global":"start"}},
                    {"op":"waitable_recv"},{"op":"pop"},
                    {"op":"load_global","payload":{"global":"ready"}},
                    {"op":"zero","payload":{"type":integer}},
                    {"op":"waitable_send"},
                    {"op":"load_global","payload":{"global":"release"}},
                    {"op":"waitable_recv"},{"op":"pop"},
                    {"op":"label","payload":{"label":"loop"}},
                    {"op":"load_local","payload":{"local":"i"}},
                    {"op":"const","payload":{"constant":"one"}},
                    {"op":"binary","payload":{"operator":"+"}},
                    {"op":"store_local","payload":{"local":"i"}},
                    {"op":"load_local","payload":{"local":"i"}},
                    {"op":"const","payload":{"constant":"limit"}},
                    {"op":"binary","payload":{"operator":"<"}},
                    {"op":"jump_if","payload":{"label":"loop"}},
                    {"op":"load_global","payload":{"global":"done"}},
                    {"op":"load_local","payload":{"local":"i"}},
                    {"op":"waitable_send"},{"op":"return","payload":{}}
                ]}
            ]
        });
        artifact["format"] = contract["spec"]["format"].clone();
        artifact["version"] = contract["spec"]["version"].clone();
        artifact["opcode_set"] = contract["spec"]["opcode_set"].clone();
        let artifact: crate::contract_generated::Artifact =
            serde_json::from_value(artifact).unwrap();
        let mut image: crate::contract_generated::ExecutionImage = serde_json::from_value(json!({
            "format":contract["execution_format"],"version":contract["execution_version"],
            "contract_id":contract["execution_contract"],"compiler_id":contract["compiler_id"],
            "root":"test","target":{"tags":["minigo"]},
            "entries":[
                {"name":"default","module_path":"test","function_id":"fn.Main"},
                {"name":"background","module_path":"test","function_id":"fn.Background"}
            ],
            "packages":{"test":{"artifact":{},"artifact_hash":crate::contract::canonical_hash(&artifact).unwrap()}}
        })).unwrap();
        image
            .packages
            .as_mut()
            .unwrap()
            .get_mut("test")
            .unwrap()
            .artifact = Some(
            serde_json::value::RawValue::from_string(
                String::from_utf8(crate::contract::canonical_json(&artifact).unwrap()).unwrap(),
            )
            .unwrap(),
        );
        image.hash = crate::contract::canonical_hash(&image).unwrap();
        Arc::new(
            Program::load(
                &crate::contract::canonical_json(&image).unwrap(),
                Default::default(),
            )
            .unwrap(),
        )
    }

    #[derive(Default)]
    struct SliceGate {
        state: Mutex<(usize, bool)>,
        changed: Condvar,
    }

    impl SliceGate {
        fn observer(self: &Arc<Self>) -> Arc<dyn Fn(u64, bool) + Send + Sync> {
            let gate = self.clone();
            Arc::new(move |task, entered| {
                if task == 1 || !entered {
                    return;
                }
                let mut state = gate.state.lock().unwrap();
                state.0 += 1;
                gate.changed.notify_all();
                while !state.1 {
                    state = gate.changed.wait(state).unwrap();
                }
            })
        }

        fn wait_until_entered(&self) {
            let state = self.state.lock().unwrap();
            let (state, timeout) = self
                .changed
                .wait_timeout_while(state, Duration::from_secs(5), |state| state.0 == 0)
                .unwrap();
            assert!(!timeout.timed_out(), "no child task entered a worker");
            assert_ne!(state.0, 0);
        }

        fn release(&self) {
            self.state.lock().unwrap().1 = true;
            self.changed.notify_all();
        }
    }

    #[derive(Default)]
    struct ParallelGate {
        state: Mutex<ParallelGateState>,
        changed: Condvar,
    }

    #[derive(Default)]
    struct ParallelGateState {
        active: HashSet<u64>,
        maximum: usize,
        armed: bool,
        released: bool,
    }

    impl ParallelGate {
        fn observer(self: &Arc<Self>) -> Arc<dyn Fn(u64, bool) + Send + Sync> {
            let gate = self.clone();
            Arc::new(move |task, entered| {
                let mut state = gate.state.lock().unwrap();
                if entered {
                    if !state.armed {
                        return;
                    }
                    state.active.insert(task);
                    state.maximum = state.maximum.max(state.active.len());
                    gate.changed.notify_all();
                    while !state.released {
                        state = gate.changed.wait(state).unwrap();
                    }
                } else if state.active.remove(&task) {
                    gate.changed.notify_all();
                }
            })
        }

        fn arm(&self) {
            self.state.lock().unwrap().armed = true;
        }

        fn wait_for(&self, expected: usize) {
            let state = self.state.lock().unwrap();
            let (state, timeout) = self
                .changed
                .wait_timeout_while(state, Duration::from_secs(5), |state| {
                    state.maximum < expected
                })
                .unwrap();
            if timeout.timed_out() {
                drop(state);
                self.release();
                let state = self.state.lock().unwrap();
                panic!("only {} task slices overlapped", state.maximum);
            }
            assert!(
                state.maximum >= expected,
                "only {} task slices overlapped",
                state.maximum,
            );
        }

        fn release(&self) {
            self.state.lock().unwrap().released = true;
            self.changed.notify_all();
        }

        fn maximum(&self) -> usize {
            self.state.lock().unwrap().maximum
        }
    }

    fn poll_after_supervisor_handoff(
        execution: &Execution,
        count: usize,
    ) -> Result<(ExecutionState, usize), RuntimeError> {
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        loop {
            match execution.poll_steps(count) {
                Err(error) if error.code == "busy" && std::time::Instant::now() < deadline => {
                    std::thread::yield_now()
                }
                result => return result,
            }
        }
    }

    fn run_control_while_slices_are_active<T: Send>(
        instance: &SharedInstance,
        execution: &Execution,
        gate: &SliceGate,
        operation: impl FnOnce() -> T + Send,
    ) -> (usize, T) {
        std::thread::scope(|scope| {
            let poll = scope.spawn(|| poll_after_supervisor_handoff(execution, 4096));
            gate.wait_until_entered();
            let (sender, receiver) = mpsc::channel();
            scope.spawn(move || sender.send(operation()).unwrap());
            let deadline = std::time::Instant::now() + Duration::from_secs(5);
            while instance.control.control_waiters.load(Ordering::Acquire) == 0 {
                assert!(
                    std::time::Instant::now() < deadline,
                    "control request was not published"
                );
                std::thread::yield_now();
            }
            assert!(matches!(
                receiver.try_recv(),
                Err(mpsc::TryRecvError::Empty)
            ));
            gate.release();
            let output = receiver.recv_timeout(Duration::from_secs(5)).unwrap();
            let (_, steps) = poll.join().unwrap().unwrap();
            assert!(steps < 4096, "control request did not stop redispatch");
            (steps, output)
        })
    }

    fn drive_until_root_waits_with_four_ready_children(
        instance: &SharedInstance,
        execution: &Execution,
    ) {
        let mut polls = 0;
        while polls < 512 {
            let (state, steps) = match execution.poll_steps(64) {
                Ok(result) => result,
                Err(error) if error.code == "busy" => {
                    std::thread::yield_now();
                    continue;
                }
                Err(error) => panic!("{error}"),
            };
            polls += 1;
            assert!(steps <= 64);
            assert!(matches!(
                state,
                ExecutionState::Running | ExecutionState::Pending
            ));
            let stats = instance.stats();
            if stats.blocked_tasks != 0 && stats.runnable_tasks >= 4 {
                return;
            }
        }
        let stats = instance.stats();
        let threads = instance.debugger().threads().unwrap();
        panic!(
            "root did not reach its completion wait: runnable={}, blocked={}, threads={threads:?}",
            stats.runnable_tasks, stats.blocked_tasks,
        );
    }

    #[test]
    fn completed_shutdown_rejects_a_late_final_reference_handoff() {
        let program = Arc::new(
            Program::load(
                include_bytes!("../examples/blocks/arithmetic.json"),
                Default::default(),
            )
            .unwrap(),
        );
        let instance = program.instantiate(InstanceOptions::default()).unwrap();
        instance.shutdown(&Cancellation::default()).unwrap();
        // A final handle may have observed Open immediately before shutdown
        // committed. Its delayed handoff must not form a receiver/Control cycle.
        assert!(
            instance
                .control
                .close_sender
                .send(instance.control.clone())
                .is_err()
        );
    }

    #[test]
    fn public_instance_runs_task_slices_on_the_configured_executor() {
        let program = parallel_program();
        for workers in [1, 2, 4] {
            let executor = Executor::new(workers).unwrap();
            let gate = Arc::new(ParallelGate::default());
            let instance = program
                .instantiate(InstanceOptions {
                    parallelism: workers,
                    executor: Some(executor.clone()),
                    task_observer: Some(gate.observer()),
                    ..Default::default()
                })
                .unwrap();
            let execution = instance.start("default", Vec::new()).unwrap();
            drive_until_root_waits_with_four_ready_children(&instance, &execution);
            gate.arm();
            std::thread::scope(|scope| {
                let waiting = scope.spawn(|| execution.wait(&Cancellation::default()));
                gate.wait_for(workers);
                gate.release();
                waiting.join().unwrap().unwrap();
            });
            assert_eq!(execution.state(), ExecutionState::Completed);
            assert_eq!(gate.maximum(), workers);
            instance.shutdown(&Cancellation::default()).unwrap();
            executor.shutdown(&Cancellation::default()).unwrap();
        }
    }

    #[test]
    fn executor_shutdown_owns_attached_instances_and_rejects_new_ones() {
        let program = Arc::new(
            Program::load(
                include_bytes!("../examples/blocks/arithmetic.json"),
                Default::default(),
            )
            .unwrap(),
        );
        let executor = Executor::new(1).unwrap();
        let instance = program
            .instantiate(InstanceOptions {
                executor: Some(executor.clone()),
                ..Default::default()
            })
            .unwrap();
        executor.shutdown(&Cancellation::default()).unwrap();
        assert!(instance.shutdown_result().is_some());
        let error = program
            .instantiate(InstanceOptions {
                executor: Some(executor),
                ..Default::default()
            })
            .err()
            .unwrap();
        assert_eq!(error.code, "closed");
    }

    #[test]
    fn executor_shutdown_rejects_synchronous_owner_reentry_without_closing() {
        let executor = Executor::new(1).unwrap();
        OWNER_DEPTH.with(|depth| depth.set(1));
        let error = executor.shutdown(&Cancellation::default()).unwrap_err();
        OWNER_DEPTH.with(|depth| depth.set(0));
        assert_eq!(error.code, "busy");

        let program = Arc::new(
            Program::load(
                include_bytes!("../examples/blocks/arithmetic.json"),
                Default::default(),
            )
            .unwrap(),
        );
        let instance = program
            .instantiate(InstanceOptions {
                executor: Some(executor.clone()),
                ..Default::default()
            })
            .unwrap();
        instance.shutdown(&Cancellation::default()).unwrap();
        executor.shutdown(&Cancellation::default()).unwrap();
    }

    #[test]
    fn supervisor_releases_its_worker_before_parallel_background_slices() {
        let program = parallel_program();
        for workers in [1, 2, 4] {
            let executor = Executor::new(workers).unwrap();
            let instance = program
                .instantiate(InstanceOptions {
                    parallelism: workers,
                    executor: Some(executor.clone()),
                    ..Default::default()
                })
                .unwrap();
            let execution = instance.start("background", Vec::new()).unwrap();
            execution.wait(&Cancellation::default()).unwrap();
            execution.wait_scope(&Cancellation::default()).unwrap();
            instance.shutdown(&Cancellation::default()).unwrap();
            executor.shutdown(&Cancellation::default()).unwrap();
        }
    }

    #[test]
    fn one_worker_executor_advances_multiple_instances_without_pinned_supervisors() {
        let program = parallel_program();
        let executor = Executor::new(1).unwrap();
        let instances = (0..2)
            .map(|_| {
                program
                    .instantiate(InstanceOptions {
                        parallelism: 4,
                        executor: Some(executor.clone()),
                        ..Default::default()
                    })
                    .unwrap()
            })
            .collect::<Vec<_>>();
        let executions = instances
            .iter()
            .map(|instance| instance.start("default", Vec::new()).unwrap())
            .collect::<Vec<_>>();
        std::thread::scope(|scope| {
            let waits = executions
                .iter()
                .map(|execution| {
                    scope.spawn(|| {
                        execution.wait(&Cancellation::default())?;
                        execution.wait_scope(&Cancellation::default())
                    })
                })
                .collect::<Vec<_>>();
            for wait in waits {
                wait.join().unwrap().unwrap();
            }
        });
        executor.shutdown(&Cancellation::default()).unwrap();
        assert!(
            instances
                .iter()
                .all(|instance| instance.shutdown_result().is_some())
        );
    }

    #[test]
    fn gc_patch_and_debug_inspection_wait_for_the_same_task_boundary() {
        {
            let executor = Executor::new(2).unwrap();
            let gate = Arc::new(SliceGate::default());
            let instance = parallel_program()
                .instantiate(InstanceOptions {
                    parallelism: 2,
                    executor: Some(executor.clone()),
                    task_observer: Some(gate.observer()),
                    ..Default::default()
                })
                .unwrap();
            let execution = instance.start("default", Vec::new()).unwrap();
            let (_, stats) =
                run_control_while_slices_are_active(&instance, &execution, &gate, || {
                    instance.collect_garbage()
                });
            assert!(stats.unwrap().live_objects != 0);
            execution.wait(&Cancellation::default()).unwrap();
            execution.wait_scope(&Cancellation::default()).unwrap();
            instance.shutdown(&Cancellation::default()).unwrap();
            executor.shutdown(&Cancellation::default()).unwrap();
        }
        {
            let executor = Executor::new(2).unwrap();
            let gate = Arc::new(SliceGate::default());
            let instance = parallel_program()
                .instantiate(InstanceOptions {
                    parallelism: 2,
                    executor: Some(executor.clone()),
                    task_observer: Some(gate.observer()),
                    ..Default::default()
                })
                .unwrap();
            let patch = instance
                .prepare_patch(parallel_program_with_limit(2_001))
                .unwrap();
            let execution = instance.start("default", Vec::new()).unwrap();
            let patch_instance = instance.clone();
            let (_, result) =
                run_control_while_slices_are_active(&instance, &execution, &gate, move || {
                    patch_instance.apply_patch(patch)
                });
            assert_eq!(result.unwrap().current.generation, 2);
            execution.wait(&Cancellation::default()).unwrap();
            execution.wait_scope(&Cancellation::default()).unwrap();
            instance.shutdown(&Cancellation::default()).unwrap();
            executor.shutdown(&Cancellation::default()).unwrap();
        }
        {
            let executor = Executor::new(2).unwrap();
            let gate = Arc::new(SliceGate::default());
            let instance = parallel_program()
                .instantiate(InstanceOptions {
                    parallelism: 2,
                    executor: Some(executor.clone()),
                    task_observer: Some(gate.observer()),
                    ..Default::default()
                })
                .unwrap();
            let execution = instance.start("default", Vec::new()).unwrap();
            let debugger = instance.debugger();
            let (_, threads) =
                run_control_while_slices_are_active(&instance, &execution, &gate, || {
                    debugger.threads()
                });
            assert!(!threads.unwrap().is_empty());
            execution.wait(&Cancellation::default()).unwrap();
            execution.wait_scope(&Cancellation::default()).unwrap();
            instance.shutdown(&Cancellation::default()).unwrap();
            executor.shutdown(&Cancellation::default()).unwrap();
        }
    }

    #[test]
    fn cancellation_waits_for_active_slices_and_settles_every_task() {
        let executor = Executor::new(2).unwrap();
        let gate = Arc::new(SliceGate::default());
        let instance = parallel_program()
            .instantiate(InstanceOptions {
                parallelism: 2,
                executor: Some(executor.clone()),
                task_observer: Some(gate.observer()),
                ..Default::default()
            })
            .unwrap();
        let execution = instance.start("default", Vec::new()).unwrap();
        std::thread::scope(|scope| {
            let poll = scope.spawn(|| poll_after_supervisor_handoff(&execution, 4096));
            gate.wait_until_entered();
            execution.cancel();
            assert_eq!(execution.state(), ExecutionState::Running);
            gate.release();
            let (state, steps) = poll.join().unwrap().unwrap();
            assert_eq!(state, ExecutionState::Canceled);
            assert!(steps < 4096);
        });
        assert_eq!(
            execution
                .wait_scope(&Cancellation::default())
                .unwrap_err()
                .code,
            "canceled"
        );
        let stats = instance.stats();
        assert_eq!(stats.active_scopes, 0);
        assert_eq!(
            stats.runnable_tasks + stats.blocked_tasks + stats.paused_tasks,
            0
        );
        instance.shutdown(&Cancellation::default()).unwrap();
        executor.shutdown(&Cancellation::default()).unwrap();
    }

    #[test]
    fn parallel_poll_budget_is_an_exact_instance_wide_limit() {
        let program = parallel_program();
        for workers in [1, 2, 4] {
            let executor = Executor::new(workers).unwrap();
            let instance = program
                .instantiate(InstanceOptions {
                    parallelism: workers,
                    executor: Some(executor.clone()),
                    ..Default::default()
                })
                .unwrap();
            let execution = instance.start("default", Vec::new()).unwrap();
            let mut total = 0usize;
            loop {
                let (state, steps) = poll_after_supervisor_handoff(&execution, 17).unwrap();
                assert!(steps <= 17);
                total += steps;
                if state == ExecutionState::Completed {
                    break;
                }
            }
            // Entry completion and scope completion are intentionally
            // distinct: the final child may still need to return after its
            // channel send released the root. It may do so before this thread
            // can sample live scope stats, so assert the two stable boundaries
            // instead of racing the supervisor between them.
            assert_eq!(total, 64_119);
            execution.wait_scope(&Cancellation::default()).unwrap();
            assert_eq!(execution.scope_stats().steps, total as u64 + 1);
            instance.shutdown(&Cancellation::default()).unwrap();
            executor.shutdown(&Cancellation::default()).unwrap();
        }
    }
}
