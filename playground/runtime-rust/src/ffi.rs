//! Opaque host calls. Completion owns a reserved result slot, so early or late
//! host completion never reenters the VM and never needs an unbounded queue.

use crate::error::RuntimeError;
use std::collections::{BTreeMap, BTreeSet};
use std::future::Future;
use std::pin::Pin;
#[cfg(not(target_arch = "wasm32"))]
use std::sync::Condvar;
use std::sync::{
    Arc, Mutex, Weak,
    atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering},
};
use std::task::{Context, Poll, Waker};

#[derive(Clone, Default)]
pub struct Cancellation(Arc<CancellationState>);

#[derive(Default)]
struct CancellationState {
    canceled: AtomicBool,
    next_listener: AtomicU64,
    listeners: Mutex<BTreeMap<u64, (u64, Weak<CancellationEvents>)>>,
    waiters: Mutex<Vec<Arc<Mutex<Option<Waker>>>>>,
}

pub(crate) struct CancellationEvents {
    ready: ReadyQueue,
    wake: Arc<Wake>,
}

impl CancellationEvents {
    pub(crate) fn new(wake: Arc<Wake>) -> Self {
        Self {
            ready: ReadyQueue::default(),
            wake,
        }
    }
    fn signal(&self, scope: u64) {
        self.ready.insert(scope);
        self.wake.signal();
    }
    pub(crate) fn take(&self) -> BTreeSet<u64> {
        self.ready.take()
    }
}

pub(crate) struct CancellationListener {
    state: Arc<CancellationState>,
    id: u64,
}

impl Drop for CancellationListener {
    fn drop(&mut self) {
        self.state.listeners.lock().unwrap().remove(&self.id);
    }
}

impl Cancellation {
    pub fn cancel(&self) {
        if self.0.canceled.swap(true, Ordering::AcqRel) {
            return;
        }
        for (scope, events) in self.0.listeners.lock().unwrap().values() {
            if let Some(events) = events.upgrade() {
                events.signal(*scope);
            }
        }
        let waiters = std::mem::take(&mut *self.0.waiters.lock().unwrap());
        for waiter in waiters {
            let waker = waiter.lock().unwrap().take();
            if let Some(waker) = waker {
                waker.wake();
            }
        }
    }
    pub fn is_cancelled(&self) -> bool {
        self.0.canceled.load(Ordering::Acquire)
    }

    /// Waits without polling or allocating an executor-specific task.
    pub fn cancelled(&self) -> Cancelled {
        Cancelled {
            cancellation: self.clone(),
            waiter: None,
        }
    }

    pub(crate) fn listen(
        &self,
        scope: u64,
        events: &Arc<CancellationEvents>,
    ) -> CancellationListener {
        let mut listeners = self.0.listeners.lock().unwrap();
        let id = self.0.next_listener.fetch_add(1, Ordering::Relaxed);
        listeners.insert(id, (scope, Arc::downgrade(events)));
        if self.is_cancelled() {
            events.signal(scope);
        }
        CancellationListener {
            state: self.0.clone(),
            id,
        }
    }
}

/// A cancellation subscription removed when its future is dropped.
pub struct Cancelled {
    cancellation: Cancellation,
    waiter: Option<Arc<Mutex<Option<Waker>>>>,
}

impl Future for Cancelled {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let this = self.get_mut();
        let waker = cx.waker().clone();
        let mut waiters = this.cancellation.0.waiters.lock().unwrap();
        if this.cancellation.is_cancelled() {
            return Poll::Ready(());
        }
        let previous = if let Some(waiter) = &this.waiter {
            waiter.lock().unwrap().replace(waker)
        } else {
            let waiter = Arc::new(Mutex::new(Some(waker)));
            waiters.push(waiter.clone());
            this.waiter = Some(waiter);
            None
        };
        drop(waiters);
        drop(previous);
        Poll::Pending
    }
}

impl Drop for Cancelled {
    fn drop(&mut self) {
        if let Some(waiter) = &self.waiter {
            self.cancellation
                .0
                .waiters
                .lock()
                .unwrap()
                .retain(|entry| !Arc::ptr_eq(entry, waiter));
        }
    }
}

pub struct Request {
    pub route: String,
    pub payload: Vec<u8>,
}

/// Dropping an undelivered reply discards its represented host resources once.
pub struct Reply {
    payload: Option<Vec<u8>>,
    error: Option<RuntimeError>,
    discard: Option<Box<dyn FnOnce() + Send>>,
    consumed: Option<Box<dyn FnOnce() + Send>>,
    reservations: [Option<BoundaryReservation>; 2],
}

impl Reply {
    pub fn payload(&self) -> &[u8] {
        self.payload.as_deref().unwrap_or_default()
    }
    pub fn error(&self) -> Option<&RuntimeError> {
        self.error.as_ref()
    }
    pub fn new(
        payload: Vec<u8>,
        error: Option<RuntimeError>,
        discard: Option<Box<dyn FnOnce() + Send>>,
    ) -> Self {
        Self {
            payload: Some(payload),
            error,
            discard,
            consumed: None,
            reservations: [None, None],
        }
    }

    /// Registers a receipt after guest delivery commits. Dropped or failed
    /// replies run only their discard callback.
    pub fn on_consumed(mut self, receipt: impl FnOnce() + Send + 'static) -> Self {
        self.consumed = Some(Box::new(receipt));
        self
    }

    /// Transfers the payload to its consumer and disarms host-side discard.
    pub fn consume(mut self) -> Result<Vec<u8>, RuntimeError> {
        if let Some(error) = self.error.take() {
            return Err(error);
        }
        self.discard = None;
        if let Some(receipt) = self.consumed.take() {
            receipt();
        }
        Ok(self.payload.take().unwrap_or_default())
    }
}

impl Drop for Reply {
    fn drop(&mut self) {
        if let Some(discard) = self.discard.take() {
            discard();
        }
    }
}

pub trait Call: Send + Sync {
    fn cancel(&self);
}

/// Session cleanup remains owned until its terminal result is observed.
pub type Shutdown<'a> = Pin<Box<dyn Future<Output = Result<(), RuntimeError>> + Send + 'a>>;

pub trait Session: Send + Sync {
    /// Start must return promptly. A successful call completes at most once;
    /// completion may occur before this method returns.
    fn start(
        &self,
        cancellation: Cancellation,
        request: Request,
        completion: Completion,
    ) -> Result<Box<dyn Call>, RuntimeError>;
    /// Cancels session-owned work and waits for its cleanup. Implementations
    /// continue cleanup if the caller stops waiting.
    fn shutdown(&self, wait: Cancellation) -> Result<(), RuntimeError>;
    /// Nonblocking cleanup entry point. Async hosts override this method; the
    /// default preserves synchronous providers whose cleanup completes promptly.
    fn shutdown_async(&self) -> Shutdown<'_> {
        Box::pin(async { self.shutdown(Cancellation::default()) })
    }
}

pub trait Bridge: Send + Sync {
    fn open(&self, cancellation: Cancellation) -> Result<Box<dyn Session>, RuntimeError>;
    fn capabilities(&self) -> Vec<String> {
        Vec::new()
    }
}

struct Delivery {
    active: bool,
    reply: Option<Reply>,
}

struct ResultSlot {
    id: u64,
    ready: Arc<ReadyQueue>,
    delivery: Mutex<Delivery>,
    wake: Arc<Wake>,
    max_result_bytes: usize,
    budget: Arc<BoundaryBudget>,
}

#[derive(Default)]
struct ReadyQueue {
    pending: AtomicBool,
    ids: Mutex<BTreeSet<u64>>,
}

impl ReadyQueue {
    fn insert(&self, id: u64) {
        let mut ids = self.ids.lock().unwrap();
        ids.insert(id);
        self.pending.store(true, Ordering::Release);
    }

    fn remove(&self, id: u64) {
        let mut ids = self.ids.lock().unwrap();
        ids.remove(&id);
        self.pending.store(!ids.is_empty(), Ordering::Release);
    }

    fn take(&self) -> BTreeSet<u64> {
        if !self.pending.load(Ordering::Acquire) {
            return BTreeSet::new();
        }
        let mut ids = self.ids.lock().unwrap();
        self.pending.store(false, Ordering::Release);
        std::mem::take(&mut *ids)
    }
}

struct BoundaryBudget {
    used: AtomicUsize,
    limit: usize,
}

struct BoundaryReservation {
    budget: Arc<BoundaryBudget>,
    bytes: usize,
}

impl BoundaryBudget {
    fn reserve(self: &Arc<Self>, bytes: usize) -> Result<BoundaryReservation, RuntimeError> {
        self.used
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                used.checked_add(bytes).filter(|total| *total <= self.limit)
            })
            .map_err(|_| {
                RuntimeError::new("boundary_limit", "ffi", "pending byte budget exceeded")
            })?;
        Ok(BoundaryReservation {
            budget: self.clone(),
            bytes,
        })
    }
}

impl Drop for BoundaryReservation {
    fn drop(&mut self) {
        self.budget.used.fetch_sub(self.bytes, Ordering::AcqRel);
    }
}

/// A single-use callback owning exactly one reserved delivery slot.
pub struct Completion(Arc<ResultSlot>);

impl Completion {
    pub fn complete(self, mut reply: Reply) {
        if reply.error.is_some() {
            reply.payload = None;
            if let Some(discard) = reply.discard.take() {
                discard();
            }
        } else {
            let bytes = reply.payload().len();
            let reservation = if bytes > self.0.max_result_bytes {
                Err(RuntimeError::new(
                    "boundary_limit",
                    "ffi",
                    "result payload exceeds boundary limit",
                ))
            } else {
                self.0.budget.reserve(bytes)
            };
            match reservation {
                Ok(reservation) => reply.reservations[1] = Some(reservation),
                Err(error) => {
                    drop(reply);
                    reply = Reply::new(Vec::new(), Some(error), None);
                }
            }
        }
        let mut delivery = self.0.delivery.lock().unwrap();
        if !delivery.active {
            drop(delivery);
            drop(reply);
            return;
        }
        delivery.reply = Some(reply);
        self.0.ready.insert(self.0.id);
        drop(delivery);
        self.0.wake.signal();
    }
}

/// Epoch-based notification prevents a completion between poll and wait from
/// being lost. The VM owner consumes results separately.
#[derive(Default)]
pub struct Wake {
    epoch: AtomicU64,
    wait_lock: Mutex<()>,
    #[cfg(not(target_arch = "wasm32"))]
    changed: Condvar,
    waiter: Mutex<Option<Waker>>,
    #[cfg(not(target_arch = "wasm32"))]
    supervisor: Mutex<Option<Waker>>,
}

impl Wake {
    pub fn epoch(&self) -> u64 {
        self.epoch.load(Ordering::Acquire)
    }
    pub fn signal(&self) {
        let guard = self.wait_lock.lock().unwrap();
        self.epoch.fetch_add(1, Ordering::Release);
        #[cfg(not(target_arch = "wasm32"))]
        self.changed.notify_all();
        drop(guard);
        let waiter = self.waiter.lock().unwrap().take();
        if let Some(waiter) = waiter {
            waiter.wake();
        }
        #[cfg(not(target_arch = "wasm32"))]
        {
            let supervisor = self.supervisor.lock().unwrap().clone();
            if let Some(supervisor) = supervisor {
                supervisor.wake();
            }
        }
    }
    #[cfg(not(target_arch = "wasm32"))]
    pub(crate) fn set_supervisor(&self, waker: Waker) {
        *self.supervisor.lock().unwrap() = Some(waker);
    }
    /// Register the owner's async driver without losing a concurrent signal.
    pub fn poll_changed(&self, observed: u64, cx: &Context<'_>) -> Poll<()> {
        let mut waiter = self.waiter.lock().unwrap();
        if self.epoch() != observed {
            return Poll::Ready(());
        }
        *waiter = Some(cx.waker().clone());
        if self.epoch() != observed {
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    }
    #[cfg(not(target_arch = "wasm32"))]
    pub fn wait(&self, observed: u64, timeout: std::time::Duration) {
        let guard = self.wait_lock.lock().unwrap();
        drop(
            self.changed
                .wait_timeout_while(guard, timeout, |_| self.epoch() == observed)
                .unwrap(),
        );
    }
}

impl std::task::Wake for Wake {
    fn wake(self: Arc<Self>) {
        self.signal();
    }
    fn wake_by_ref(self: &Arc<Self>) {
        self.signal();
    }
}

struct Pending {
    slot: Arc<ResultSlot>,
    cancellation: Cancellation,
    call: Option<Box<dyn Call>>,
    reservation: BoundaryReservation,
}

/// Call registration and terminal transitions are driven by the single VM
/// owner. External Start/Cancel/Discard operations execute outside result locks.
pub struct PendingCalls {
    ready: Arc<ReadyQueue>,
    calls: BTreeMap<u64, Pending>,
    next_id: u64,
    max_calls: usize,
    budget: Arc<BoundaryBudget>,
    closed: bool,
    wake: Arc<Wake>,
}

impl PendingCalls {
    pub fn new(max_calls: usize, max_bytes: usize, wake: Arc<Wake>) -> Self {
        Self {
            ready: Arc::default(),
            calls: BTreeMap::new(),
            next_id: 0,
            max_calls,
            budget: Arc::new(BoundaryBudget {
                used: AtomicUsize::new(0),
                limit: max_bytes,
            }),
            closed: false,
            wake,
        }
    }

    /// Reserves capacity before the host sees the request. The caller keeps the
    /// request while registering, so a rejected reservation cannot lose input.
    pub fn reserve(
        &mut self,
        request_bytes: usize,
        result_capacity: usize,
    ) -> Result<(u64, Cancellation, Completion), RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new("closed", "ffi", "call owner is closed"));
        }
        if self.calls.len() >= self.max_calls {
            return Err(RuntimeError::new(
                "pending_limit",
                "ffi",
                "pending call budget exceeded",
            ));
        }
        let id = self
            .next_id
            .checked_add(1)
            .ok_or_else(|| RuntimeError::new("pending_limit", "ffi", "call identity exhausted"))?;
        let reservation = self.budget.reserve(request_bytes)?;
        let slot = Arc::new(ResultSlot {
            id,
            ready: self.ready.clone(),
            delivery: Mutex::new(Delivery {
                active: true,
                reply: None,
            }),
            wake: self.wake.clone(),
            max_result_bytes: result_capacity,
            budget: self.budget.clone(),
        });
        let cancellation = Cancellation::default();
        self.calls.insert(
            id,
            Pending {
                slot: slot.clone(),
                cancellation: cancellation.clone(),
                call: None,
                reservation,
            },
        );
        self.next_id = id;
        Ok((id, cancellation, Completion(slot)))
    }

    /// Finishes the Starting transition after host Start returns. If shutdown
    /// won the race, the newly returned host handle is cancelled immediately.
    pub fn started(
        &mut self,
        id: u64,
        result: Result<Box<dyn Call>, RuntimeError>,
    ) -> Result<(), RuntimeError> {
        match result {
            Ok(call) => {
                if let Some(pending) = self.calls.get_mut(&id) {
                    if pending.call.is_some() {
                        call.cancel();
                        return Err(RuntimeError::new(
                            "call_state",
                            "ffi",
                            "call already started",
                        ));
                    }
                    pending.call = Some(call);
                    // Completion can precede Start's return and its first
                    // notification may already have been drained by control.
                    let delivery = pending.slot.delivery.lock().unwrap();
                    let ready = delivery.reply.is_some();
                    if ready {
                        pending.slot.ready.insert(id);
                    }
                    drop(delivery);
                    if ready {
                        self.wake.signal();
                    }
                    Ok(())
                } else {
                    call.cancel();
                    Err(RuntimeError::new(
                        "cancelled",
                        "ffi",
                        "call owner released reservation",
                    ))
                }
            }
            Err(error) => {
                self.cancel(id);
                Err(error)
            }
        }
    }

    /// Returns ready results only after Start has successfully established the
    /// call. The reply retains request and result bytes until consumption or discard.
    pub fn take(&mut self, id: u64) -> Option<Reply> {
        let pending = self.calls.get(&id)?;
        pending.call.as_ref()?;
        let mut delivery = pending.slot.delivery.lock().unwrap();
        let mut reply = delivery.reply.take()?;
        delivery.active = false;
        self.ready.remove(id);
        drop(delivery);
        let pending = self.calls.remove(&id).unwrap();
        reply.reservations[0] = Some(pending.reservation);
        Some(reply)
    }

    pub fn cancel(&mut self, id: u64) {
        let Some(pending) = self.calls.remove(&id) else {
            return;
        };
        pending.cancellation.cancel();
        let mut delivery = pending.slot.delivery.lock().unwrap();
        delivery.active = false;
        self.ready.remove(id);
        let reply = delivery.reply.take();
        drop(delivery);
        if let Some(call) = pending.call {
            call.cancel();
        }
        drop(reply);
        self.wake.signal();
    }

    pub fn reserved_bytes(&self) -> usize {
        self.budget.used.load(Ordering::Acquire)
    }
    pub fn pending_count(&self) -> usize {
        self.calls.len()
    }

    pub(crate) fn take_ready_ids(&self) -> BTreeSet<u64> {
        self.ready.take()
    }

    pub fn close(&mut self) {
        self.closed = true;
        while let Some(id) = self.calls.keys().next().copied() {
            self.cancel(id);
        }
    }
}

impl Drop for PendingCalls {
    fn drop(&mut self) {
        self.close();
    }
}

#[cfg(test)]
mod event_tests {
    use super::*;

    #[test]
    fn start_republishes_completion_drained_during_host_handoff() {
        struct HostCall;
        impl Call for HostCall {
            fn cancel(&self) {}
        }
        let wake = Arc::new(Wake::default());
        let mut pending = PendingCalls::new(1, 64, wake.clone());
        let (id, _, complete) = pending.reserve(0, 64).unwrap();
        complete.complete(Reply::new(vec![1, 2], None, None));
        assert_eq!(pending.take_ready_ids(), BTreeSet::from([id]));
        assert!(pending.take(id).is_none());
        let before_start = wake.epoch();
        pending.started(id, Ok(Box::new(HostCall))).unwrap();
        assert_ne!(wake.epoch(), before_start);
        assert_eq!(pending.take_ready_ids(), BTreeSet::from([id]));
        assert_eq!(pending.take(id).unwrap().consume().unwrap(), [1, 2]);
        assert_eq!(pending.reserved_bytes(), 0);
        assert!(pending.take_ready_ids().is_empty());
    }

    #[test]
    fn draining_while_publishing_preserves_each_ready_identity() {
        for _ in 0..32 {
            let queue = Arc::new(ReadyQueue::default());
            let barrier = Arc::new(std::sync::Barrier::new(2));
            queue.insert(1);
            let publisher = {
                let queue = queue.clone();
                let barrier = barrier.clone();
                std::thread::spawn(move || {
                    barrier.wait();
                    queue.insert(2);
                    queue.insert(3);
                })
            };
            barrier.wait();
            let mut delivered = queue.take();
            publisher.join().unwrap();
            delivered.extend(queue.take());
            assert_eq!(delivered, BTreeSet::from([1, 2, 3]));
            assert!(queue.take().is_empty());
            queue.insert(4);
            queue.remove(4);
            assert!(queue.take().is_empty());
            queue.insert(5);
            assert_eq!(queue.take(), BTreeSet::from([5]));
        }
    }

    #[test]
    fn cancellation_delivers_live_scopes_once_and_releases_subscriptions() {
        let cancellation = Cancellation::default();
        let events = Arc::new(CancellationEvents::new(Arc::new(Wake::default())));
        let first = cancellation.listen(1, &events);
        let second = cancellation.listen(2, &events);
        drop(first);
        cancellation.cancel();
        cancellation.cancel();
        assert_eq!(events.take(), BTreeSet::from([2]));
        assert!(events.take().is_empty());
        drop(second);
        assert!(cancellation.0.listeners.lock().unwrap().is_empty());
        let late = cancellation.listen(3, &events);
        assert_eq!(events.take(), BTreeSet::from([3]));
        drop(late);
        assert!(cancellation.0.listeners.lock().unwrap().is_empty());
    }

    #[test]
    fn registration_racing_with_cancel_preserves_delivery() {
        for _ in 0..32 {
            let cancellation = Cancellation::default();
            let events = Arc::new(CancellationEvents::new(Arc::new(Wake::default())));
            let barrier = Arc::new(std::sync::Barrier::new(2));
            let worker = {
                let cancellation = cancellation.clone();
                let barrier = barrier.clone();
                std::thread::spawn(move || {
                    barrier.wait();
                    cancellation.cancel();
                })
            };
            barrier.wait();
            let listener = cancellation.listen(7, &events);
            worker.join().unwrap();
            assert_eq!(events.take(), BTreeSet::from([7]));
            drop(listener);
            assert!(cancellation.0.listeners.lock().unwrap().is_empty());
        }
    }
}
