mod lease;

use super::binding::Binding;
use super::platform::Handle;
use super::protocol::*;
use super::*;
use crate::ffi::Cancellation;
use std::collections::BTreeMap;
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering},
};
use std::time::Duration;
use tokio::sync::{OwnedSemaphorePermit, Semaphore, mpsc, oneshot, watch};
use web_time::Instant;

/// Read and write transfer complete physical fragments; close wakes both directions.
pub trait MessageConn: Send + Sync {
    fn read(&self) -> BoxFuture<'_, Result<Vec<u8>>>;
    fn write(&self, fragment: Vec<u8>) -> BoxFuture<'_, Result<()>>;
    fn close(&self);
}

#[derive(Clone)]
pub struct EndpointOptions {
    pub limits: Limits,
    pub peer: PeerInfo,
    pub lease_ttl: Duration,
    pub admission_timeout: Duration,
    pub max_call_duration: Option<Duration>,
}
impl Default for EndpointOptions {
    fn default() -> Self {
        Self {
            limits: Limits::default(),
            peer: PeerInfo::default(),
            lease_ttl: Duration::from_secs(60),
            admission_timeout: Duration::from_secs(10),
            max_call_duration: None,
        }
    }
}

struct Queued {
    frame: Frame,
    queued: Instant,
    context: Option<CallContext>,
    written: oneshot::Sender<Result<Instant>>,
    _bytes: OwnedSemaphorePermit,
}
struct InboundBinding {
    routes: Arc<RouteSet>,
    _permit: OwnedSemaphorePermit,
}
struct InboundResult {
    binding: u64,
    result: PendingResult,
    expiration: Cancellation,
    expires_at: Instant,
    _permit: OwnedSemaphorePermit,
}
struct Pending {
    kind: u64,
    binding: u64,
    reply: oneshot::Sender<Result<Frame>>,
    accepted: watch::Sender<bool>,
}
struct ProtocolState {
    peer: String,
    peer_ready: bool,
    peer_lease_ttl: Duration,
    peer_request: u64,
    peer_renew: u64,
    renew_cursor: usize,
    outbound_limits: Limits,
    pending: BTreeMap<u64, Pending>,
    active: BTreeMap<u64, Cancellation>,
    active_leases: BTreeMap<u64, Instant>,
    active_bindings: BTreeMap<u64, u64>,
    decisions: BTreeMap<u64, bool>,
    binding_admissions: BTreeMap<u64, OwnedSemaphorePermit>,
    result_admissions: BTreeMap<u64, OwnedSemaphorePermit>,
    bindings: BTreeMap<u64, InboundBinding>,
    binding_leases: BTreeMap<u64, Instant>,
    outbound_bindings: BTreeMap<u64, Arc<RouteSet>>,
    outbound_binding_leases: BTreeMap<u64, Instant>,
    outbound_operations: BTreeMap<u64, (Instant, Cancellation)>,
    results: BTreeMap<u64, InboundResult>,
    failure: Option<Status>,
    cleanup_error: Option<Status>,
}
struct EndpointState {
    runtime: Handle,
    options: EndpointOptions,
    conn: Arc<dyn MessageConn>,
    binder: Option<Arc<dyn Binder>>,
    origin: String,
    state: Mutex<ProtocolState>,
    stopping: Cancellation,
    ready: watch::Receiver<bool>,
    done: watch::Receiver<bool>,
    next_request: AtomicU64,
    next_binding: AtomicU64,
    data_slots: Arc<Semaphore>,
    control_slots: Arc<Semaphore>,
    inbound_slots: Arc<Semaphore>,
    inbound_controls: Arc<Semaphore>,
    binding_slots: Arc<Semaphore>,
    outbound_bindings: Arc<Semaphore>,
    result_slots: Arc<Semaphore>,
    outbound_results: Arc<Semaphore>,
    bytes: Arc<Semaphore>,
    control_bytes: Arc<Semaphore>,
    replies: mpsc::Sender<Queued>,
    requests: mpsc::Sender<Queued>,
    request_gate: Arc<tokio::sync::Mutex<()>>,
}

pub struct Endpoint {
    state: Arc<EndpointState>,
}
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct EndpointStats {
    pub pending_calls: usize,
    pub inbound_calls: usize,
    pub bindings: usize,
    pub pending_results: usize,
}
impl Endpoint {
    pub fn stats(&self) -> EndpointStats {
        let state = self.state.state.lock().unwrap();
        EndpointStats {
            pending_calls: state.pending.len(),
            inbound_calls: state.active.len(),
            bindings: self.state.options.limits.max_bindings
                - self.state.binding_slots.available_permits()
                + state.outbound_limits.max_bindings
                - self.state.outbound_bindings.available_permits(),
            pending_results: self.state.options.limits.max_pending_results
                - self.state.result_slots.available_permits()
                + state.outbound_limits.max_pending_results
                - self.state.outbound_results.available_permits(),
        }
    }
    pub fn open(
        runtime: Handle,
        conn: Arc<dyn MessageConn>,
        binder: Option<Arc<dyn Binder>>,
        options: EndpointOptions,
    ) -> Result<Arc<Self>> {
        options.limits.validate_endpoint()?;
        if options.lease_ttl < Duration::from_millis(1)
            || options.admission_timeout < Duration::from_millis(1)
            || options.lease_ttl.as_nanos() > i64::MAX as u128
            || options.admission_timeout.as_nanos() > i64::MAX as u128
            || options.max_call_duration.is_some_and(|duration| {
                duration.is_zero() || duration.as_nanos() > i64::MAX as u128
            })
        {
            return Err(Status::new(
                "invalid_argument",
                "RPC lease and admission timeouts must be at least 1ms and durations must fit signed nanoseconds",
            ));
        }
        if options
            .limits
            .wire_values()
            .iter()
            .any(|v| *v > u32::MAX as usize)
        {
            return Err(Status::new(
                "invalid_argument",
                "RPC limits exceed supported capacity",
            ));
        }
        let mut origin = [0u8; 16];
        getrandom::fill(&mut origin).map_err(|error| Status::new("internal", error.to_string()))?;
        let origin = origin.iter().map(|v| format!("{v:02x}")).collect();
        let (ready, ready_rx) = watch::channel(false);
        let (done, done_rx) = watch::channel(false);
        let (replies, reply_rx) = mpsc::channel(32);
        let (requests, request_rx) = mpsc::channel(64);
        let limits = &options.limits;
        let state = Arc::new(EndpointState {
            runtime: runtime.clone(),
            conn,
            binder,
            origin,
            state: Mutex::new(ProtocolState {
                peer: String::new(),
                peer_ready: false,
                peer_lease_ttl: options.lease_ttl,
                peer_request: 0,
                peer_renew: 0,
                renew_cursor: 0,
                outbound_limits: limits.clone(),
                pending: BTreeMap::new(),
                active: BTreeMap::new(),
                active_leases: BTreeMap::new(),
                active_bindings: BTreeMap::new(),
                decisions: BTreeMap::new(),
                binding_admissions: BTreeMap::new(),
                result_admissions: BTreeMap::new(),
                bindings: BTreeMap::new(),
                binding_leases: BTreeMap::new(),
                outbound_bindings: BTreeMap::new(),
                outbound_binding_leases: BTreeMap::new(),
                outbound_operations: BTreeMap::new(),
                results: BTreeMap::new(),
                failure: None,
                cleanup_error: None,
            }),
            stopping: Cancellation::default(),
            ready: ready_rx,
            done: done_rx,
            next_request: AtomicU64::new(1),
            next_binding: AtomicU64::new(1),
            data_slots: Arc::new(Semaphore::new(limits.max_pending_calls)),
            control_slots: Arc::new(Semaphore::new(limits.max_pending_controls)),
            inbound_slots: Arc::new(Semaphore::new(limits.max_pending_calls)),
            inbound_controls: Arc::new(Semaphore::new(limits.max_pending_controls)),
            binding_slots: Arc::new(Semaphore::new(limits.max_bindings)),
            outbound_bindings: Arc::new(Semaphore::new(limits.max_bindings)),
            result_slots: Arc::new(Semaphore::new(limits.max_pending_results)),
            outbound_results: Arc::new(Semaphore::new(limits.max_pending_results)),
            bytes: Arc::new(Semaphore::new(limits.max_in_flight_bytes)),
            control_bytes: Arc::new(Semaphore::new(
                limits
                    .max_frame_bytes
                    .saturating_mul(limits.max_pending_controls)
                    .min(limits.max_in_flight_bytes),
            )),
            replies,
            requests,
            options,
            request_gate: Arc::new(tokio::sync::Mutex::new(())),
        });
        let owner = state.clone();
        runtime.spawn(async move {
            let writer_state = owner.clone();
            let writer = owner
                .runtime
                .spawn(async move { writer_state.write_loop(reply_rx, request_rx).await });
            let hello = Frame {
                kind: HELLO,
                protocol: ENDPOINT_PROTOCOL.into(),
                limits: Some(owner.options.limits.clone()),
                lease_ttl: owner.options.lease_ttl.as_nanos().min(i64::MAX as u128) as i64,
                admission_timeout: owner
                    .options
                    .admission_timeout
                    .as_nanos()
                    .min(i64::MAX as u128) as i64,
                max_call_duration: owner.options.max_call_duration.map_or(0, |duration| {
                    duration.as_nanos().min(i64::MAX as u128) as i64
                }),
                ..Frame::default()
            };
            let outcome = async {
                owner.enqueue(hello).await?;
                owner.read_loop(ready).await
            }
            .await;
            owner.fail(
                outcome
                    .err()
                    .unwrap_or_else(|| Status::new("unavailable", "RPC endpoint closed")),
            );
            let _ = writer.await;
            let (bindings, outbound_bindings, results) = {
                let mut state = owner.state.lock().unwrap();
                (
                    std::mem::take(&mut state.bindings),
                    std::mem::take(&mut state.outbound_bindings),
                    std::mem::take(&mut state.results),
                )
            };
            for entry in bindings.values() {
                entry.routes.binding.begin_shutdown();
            }
            for routes in outbound_bindings.values() {
                routes.binding.begin_shutdown();
            }
            for entry in results.into_values() {
                entry.expiration.cancel();
                owner.record_cleanup(entry.result.discard().await);
            }
            for entry in bindings.into_values() {
                owner.record_cleanup(entry.routes.shutdown().await);
            }
            for routes in outbound_bindings.into_values() {
                owner.record_cleanup(routes.shutdown().await);
            }
            let _inbound = owner
                .inbound_slots
                .acquire_many(owner.options.limits.max_pending_calls as u32)
                .await;
            let _controls = owner
                .inbound_controls
                .acquire_many(owner.options.limits.max_pending_controls as u32)
                .await;
            let outbound = owner.state.lock().unwrap().outbound_limits.clone();
            let _calls = owner
                .data_slots
                .acquire_many(outbound.max_pending_calls as u32)
                .await;
            let _outbound_controls = owner
                .control_slots
                .acquire_many(outbound.max_pending_controls as u32)
                .await;
            let _bindings = owner
                .outbound_bindings
                .acquire_many(outbound.max_bindings as u32)
                .await;
            let _results = owner
                .outbound_results
                .acquire_many(outbound.max_pending_results as u32)
                .await;
            let _inbound_bindings = owner
                .binding_slots
                .acquire_many(owner.options.limits.max_bindings as u32)
                .await;
            let _inbound_results = owner
                .result_slots
                .acquire_many(owner.options.limits.max_pending_results as u32)
                .await;
            drop((
                _inbound,
                _controls,
                _calls,
                _outbound_controls,
                _bindings,
                _results,
            ));
            done.send_replace(true);
        });
        runtime.spawn(EndpointState::maintain_leases(Arc::downgrade(&state)));
        Ok(Arc::new(Self { state }))
    }
    pub async fn ready(&self, context: CallContext) -> Result<()> {
        self.state.ready(context).await
    }
    pub async fn ping(&self, context: CallContext) -> Result<()> {
        self.state
            .request(
                context,
                Frame {
                    kind: RENEW,
                    values: vec![0],
                    ..Frame::default()
                },
            )
            .await
            .map(|_| ())
    }
    pub fn begin_shutdown(&self) {
        self.state
            .fail(Status::new("unavailable", "RPC endpoint shut down"));
    }
    pub async fn shutdown(&self) -> Result<()> {
        self.begin_shutdown();
        let mut done = self.state.done.clone();
        done.wait_for(|done| *done)
            .await
            .map_err(|_| Status::new("internal", "RPC endpoint owner failed"))?;
        self.state
            .state
            .lock()
            .unwrap()
            .cleanup_error
            .clone()
            .map_or(Ok(()), Err)
    }
}
impl Drop for Endpoint {
    fn drop(&mut self) {
        self.state
            .fail(Status::new("unavailable", "RPC endpoint dropped"));
    }
}

impl EndpointState {
    fn inbound_routes(&self, binding: u64) -> Result<Arc<RouteSet>> {
        let state = self.state.lock().unwrap();
        if !state
            .binding_leases
            .get(&binding)
            .is_some_and(|expires| *expires > Instant::now())
        {
            return Err(Status::new("not_found", "RPC binding lease expired"));
        }
        state
            .bindings
            .get(&binding)
            .map(|entry| entry.routes.clone())
            .ok_or_else(|| Status::new("not_found", "RPC binding closed"))
    }
    fn record_cleanup(&self, outcome: Result<()>) {
        if let Err(error) = outcome {
            self.state
                .lock()
                .unwrap()
                .cleanup_error
                .get_or_insert(error);
        }
    }
    fn fail(&self, error: Status) {
        let (pending, active) = {
            let mut state = self.state.lock().unwrap();
            if state.failure.is_some() {
                return;
            }
            state.failure = Some(error.clone());
            (
                std::mem::take(&mut state.pending),
                std::mem::take(&mut state.active),
            )
        };
        self.stopping.cancel();
        self.conn.close();
        for pending in pending.into_values() {
            let _ = pending.reply.send(Err(error.clone()));
        }
        for cancellation in active.into_values() {
            cancellation.cancel();
        }
    }
    fn error(&self) -> Status {
        self.state
            .lock()
            .unwrap()
            .failure
            .clone()
            .unwrap_or_else(|| Status::new("unavailable", "RPC endpoint closed"))
    }
    async fn ready(&self, context: CallContext) -> Result<()> {
        let mut ready = self.ready.clone();
        context.run(async {
            tokio::select! {
                _ = self.stopping.cancelled() => Err(self.error()),
                result = ready.wait_for(|value| *value) => result.map(|_| ()).map_err(|_| self.error()),
            }
        }).await
    }
    async fn enqueue(&self, frame: Frame) -> Result<()> {
        self.queue_frame(frame, None, None).await.map(|_| ())
    }
    async fn enqueue_written(
        &self,
        frame: Frame,
        context: Option<CallContext>,
        gate: Option<tokio::sync::OwnedMutexGuard<()>>,
    ) -> Result<Instant> {
        self.queue_frame(frame, context, gate)
            .await?
            .await
            .map_err(|_| self.error())?
    }
    async fn queue_frame(
        &self,
        mut frame: Frame,
        context: Option<CallContext>,
        gate: Option<tokio::sync::OwnedMutexGuard<()>>,
    ) -> Result<oneshot::Receiver<Result<Instant>>> {
        frame.origin = self.origin.clone();
        let limits = self.state.lock().unwrap().outbound_limits.clone();
        let size = frame.encode(limits.max_message_bytes)?.len();
        if matches!(frame.kind, RENEW | RENEW_ACK)
            && size > limits.max_frame_bytes.saturating_sub(35)
        {
            return Err(Status::exhausted("RPC renewal exceeds control frame limit"));
        }
        let control =
            is_zero_control(frame.kind) && size <= limits.max_frame_bytes.saturating_sub(35);
        let budget = if control {
            &self.control_bytes
        } else {
            &self.bytes
        };
        let bytes = budget
            .clone()
            .try_acquire_many_owned(size as u32)
            .map_err(|_| Status::exhausted("RPC queued bytes limit exceeded"))?;
        let queue = if control {
            &self.replies
        } else {
            &self.requests
        };
        let (written, receipt) = oneshot::channel();
        tokio::select! {
            _ = self.stopping.cancelled() => Err(self.error()),
            result = super::platform::timeout(self.options.lease_ttl, queue.send(Queued { frame, context, written, queued: Instant::now(), _bytes: bytes })) => result.map_err(|_| Status::new("deadline_exceeded", "RPC write queue stalled"))?.map_err(|_| self.error()),
        }?;
        drop(gate);
        Ok(receipt)
    }
    async fn write_loop(
        self: Arc<Self>,
        mut replies: mpsc::Receiver<Queued>,
        mut requests: mpsc::Receiver<Queued>,
    ) {
        let result: Result<()> = async {
            let mut id = 0u64;
            loop {
                let queued = tokio::select! {
                    _ = self.stopping.cancelled() => return Ok(()),
                    value = replies.recv() => value,
                    value = requests.recv() => value,
                };
                let Some(mut queued) = queued else { return Ok(()); };
                if let Some(context) = &queued.context
                    && let Err(error) = context.check() {
                    let _ = queued.written.send(Err(error));
                    continue;
                }
                if queued.queued.elapsed() >= self.options.lease_ttl {
                    let _ = queued.written.send(Err(Status::new("deadline_exceeded", "RPC write queue stalled")));
                    continue;
                }
                let limits = self.state.lock().unwrap().outbound_limits.clone();
                let payload = self.prepare_payload(&mut queued, &limits)?;
                let message_id = self.message_id(queued.frame.kind, payload.len(), &limits, &mut id)?;
                if message_id == 0 {
                    self.write_payload(&payload, &limits, message_id).await?;
                    self.complete_write(queued, Instant::now());
                    continue;
                }
                let chunk_size = limits.max_frame_bytes - 35;
                let mut sent_at = Instant::now();
                for (index, chunk) in payload.chunks(chunk_size).enumerate() {
                    for _ in 0..4 {
                        if let Ok(mut control) = replies.try_recv() {
                            if let Some(context) = &control.context
                                && let Err(error) = context.check() {
                                let _ = control.written.send(Err(error));
                                continue;
                            }
                            let control_payload = self.prepare_payload(&mut control, &limits)?;
                            self.write_payload(&control_payload, &limits, 0).await?;
                            self.complete_write(control, Instant::now());
                        } else {
                            break;
                        }
                    }
                    let fragment = encode_fragment(
                        message_id,
                        payload.len(),
                        index * chunk_size,
                        chunk,
                        limits.max_frame_bytes,
                    )?;
                    sent_at = Instant::now();
                    if (index + 1) * chunk_size >= payload.len() { self.record_operation(&queued, sent_at); }
                    let write = async {
                        super::platform::timeout(self.options.lease_ttl, self.conn.write(fragment)).await
                            .map_err(|_| Status::new("deadline_exceeded", "RPC physical write timed out"))?
                    };
                    tokio::select! {
                        _ = self.stopping.cancelled() => return Ok(()),
                        result = async { if let Some(context) = &queued.context { context.run(write).await } else { write.await } } => result?,
                    }
                }
                self.complete_write(queued, sent_at);
            }
        }.await;
        if let Err(error) = result {
            self.fail(error);
        }
    }
    fn complete_write(&self, queued: Queued, sent_at: Instant) {
        let _ = queued.written.send(Ok(sent_at));
    }
    fn record_operation(&self, queued: &Queued, sent_at: Instant) {
        if matches!(queued.frame.kind, BIND | CALL | DROP | CLOSE) && !queued.frame.reply {
            let mut state = self.state.lock().unwrap();
            if state.pending.contains_key(&queued.frame.id) {
                let expires = sent_at + state.peer_lease_ttl;
                state
                    .outbound_operations
                    .insert(queued.frame.id, (expires, Cancellation::default()));
            }
        }
    }
    fn prepare_payload(&self, queued: &mut Queued, limits: &Limits) -> Result<Vec<u8>> {
        if queued.frame.timeout > 0 {
            queued.frame.timeout = queued
                .frame
                .timeout
                .saturating_sub(queued.queued.elapsed().as_nanos().min(i64::MAX as u128) as i64)
                .max(1);
        }
        let payload = queued.frame.encode(limits.max_message_bytes)?;
        Ok(payload)
    }
    fn message_id(
        &self,
        kind: u64,
        payload_len: usize,
        limits: &Limits,
        id: &mut u64,
    ) -> Result<u64> {
        let message_id =
            if is_zero_control(kind) && payload_len <= limits.max_frame_bytes.saturating_sub(35) {
                0
            } else {
                *id = id
                    .checked_add(1)
                    .ok_or_else(|| Status::exhausted("RPC fragment ID exhausted"))?;
                *id
            };
        Ok(message_id)
    }
    async fn write_payload(&self, payload: &[u8], limits: &Limits, id: u64) -> Result<()> {
        let chunk_size = limits.max_frame_bytes - 35;
        for (index, chunk) in payload.chunks(chunk_size).enumerate() {
            let fragment = encode_fragment(
                id,
                payload.len(),
                index * chunk_size,
                chunk,
                limits.max_frame_bytes,
            )?;
            tokio::select! {
                _ = self.stopping.cancelled() => return Ok(()),
                result = super::platform::timeout(self.options.lease_ttl, self.conn.write(fragment)) => {
                    result
                        .map_err(|_| Status::new("deadline_exceeded", "RPC physical write timed out"))??;
                },
            }
        }
        Ok(())
    }
    fn request(
        self: &Arc<Self>,
        context: CallContext,
        mut frame: Frame,
    ) -> BoxFuture<'_, Result<Frame>> {
        Box::pin(async move {
            self.ready(context.clone()).await?;
            let control = !matches!(frame.kind, BIND | CALL);
            let semaphore = if control {
                &self.control_slots
            } else {
                &self.data_slots
            };
            let permit = semaphore
                .clone()
                .try_acquire_owned()
                .map_err(|_| Status::exhausted("RPC pending request limit exceeded"))?;
            let gate = context
                .run(async {
                    super::platform::timeout(
                        self.options.lease_ttl,
                        self.request_gate.clone().lock_owned(),
                    )
                    .await
                    .map_err(|_| Status::new("unavailable", "RPC request queue stalled"))
                })
                .await?;
            if frame.kind == DECISION {
                if frame.target_id == 0 {
                    return Err(Status::protocol("RPC decision operation is missing"));
                }
                frame.id = frame.target_id;
            } else {
                frame.id = self
                    .next_request
                    .fetch_update(Ordering::AcqRel, Ordering::Acquire, |v| v.checked_add(1))
                    .map_err(|_| Status::exhausted("RPC request ID exhausted"))?;
            }
            if let Some(deadline) = context.deadline {
                frame.timeout = deadline
                    .saturating_duration_since(Instant::now())
                    .as_nanos()
                    .min(i64::MAX as u128) as i64;
            }
            let id = frame.id;
            let (reply, response) = oneshot::channel();
            let (accepted, mut accepted_rx) = watch::channel(false);
            let (mut deliver, delivered) = oneshot::channel();
            let (received, receipt) = oneshot::channel();
            let reservation = Arc::new(Mutex::new(Some(permit)));
            let owner_reservation = reservation.clone();
            {
                let mut state = self.state.lock().unwrap();
                if let Some(error) = &state.failure {
                    return Err(error.clone());
                }
                if matches!(frame.kind, CALL | DROP)
                    && !state
                        .outbound_binding_leases
                        .get(&frame.binding)
                        .is_some_and(|expires| *expires > Instant::now())
                {
                    return Err(Status::new("unavailable", "RPC binding lease expired"));
                }
                state.pending.insert(
                    id,
                    Pending {
                        kind: frame.kind,
                        binding: frame.binding,
                        reply,
                        accepted,
                    },
                );
            }
            let state = self.clone();
            let kind = frame.kind;
            self.runtime.spawn(async move {
                enum CallStopped {
                    CallerDropped,
                    Report(Status),
                }

                let queued_cancellation = Cancellation::default();
                let mut send_context = context.clone();
                send_context.cancellation = queued_cancellation.clone();
                let enqueued = state.enqueue_written(frame, Some(send_context), Some(gate));
                tokio::pin!(enqueued);
                let enqueued = tokio::select! {
                    result = &mut enqueued => result,
                    _ = deliver.closed() => { queued_cancellation.cancel(); (&mut enqueued).await },
                    _ = context.cancellation.cancelled() => { queued_cancellation.cancel(); (&mut enqueued).await },
                };
                let sent = enqueued.is_ok();
                let expiration = state
                    .state
                    .lock()
                    .unwrap()
                    .outbound_operations
                    .get(&id)
                    .map(|(_, cancel)| cancel.clone())
                    .unwrap_or_default();
                let reply = async { enqueued?; response.await.map_err(|_| state.error())? };
                tokio::pin!(reply);
                let admission_deadline = Instant::now() + state.options.admission_timeout;
                let wait_outcome = tokio::select! {
                    biased;
                    _ = deliver.closed() => Err(CallStopped::CallerDropped),
                    _ = context.cancellation.cancelled() => Err(CallStopped::Report(Status::new("canceled", "RPC call canceled"))),
                    _ = expiration.cancelled() => Err(CallStopped::Report(Status::new("unavailable", "RPC operation lease expired"))),
                    value = &mut reply => Ok(value),
                    changed = accepted_rx.changed() => {
                        if changed.is_ok() && *accepted_rx.borrow() {
                            tokio::select! {
                                biased;
                                _ = deliver.closed() => Err(CallStopped::CallerDropped),
                                _ = context.cancellation.cancelled() => Err(CallStopped::Report(Status::new("canceled", "RPC call canceled"))),
                                _ = expiration.cancelled() => Err(CallStopped::Report(Status::new("unavailable", "RPC operation lease expired"))),
                                value = &mut reply => Ok(value),
                                _ = async { if let Some(deadline) = context.deadline { super::platform::sleep_until(deadline).await } else { std::future::pending().await } } => Err(CallStopped::Report(Status::new("deadline_exceeded", "RPC deadline exceeded"))),
                            }
                        } else {
                            Err(CallStopped::Report(Status::new("unavailable", "RPC operation admission was not confirmed")))
                        }
                    }
                    _ = super::platform::sleep_until(admission_deadline) => Err(CallStopped::Report(Status::new("unavailable", "RPC operation admission was not confirmed"))),
                    _ = async { if let Some(deadline) = context.deadline { super::platform::sleep_until(deadline).await } else { std::future::pending().await } } => Err(CallStopped::Report(Status::new("deadline_exceeded", "RPC deadline exceeded"))),
                };
                let mut delivery = Some(deliver);
                let response_before_cancel = match wait_outcome {
                    Ok(value) => Some(value),
                    Err(CallStopped::CallerDropped) => {
                        delivery.take();
                        None
                    }
                    Err(CallStopped::Report(error)) => {
                        let _ = delivery.take().unwrap().send(Err(error));
                        None
                    }
                };
                let outcome = match response_before_cancel {
                    Some(value) => value,
                    None => {
                        state
                            .state
                            .lock()
                            .unwrap()
                            .outbound_operations
                            .remove(&id);
                        let cancel_and_wait = async {
                            if sent {
                                state
                                    .enqueue(Frame {
                                        kind: CANCEL,
                                        target_id: id,
                                        ..Frame::default()
                                    })
                                    .await?;
                            }
                            (&mut reply).await
                        };
                        match super::platform::timeout(state.options.lease_ttl, cancel_and_wait).await {
                            Ok(value) => value,
                            Err(_) => Err(Status::new("unavailable", "RPC operation wait ended")),
                        }
                    }
                };
                state.state.lock().unwrap().pending.remove(&id);
                if kind != CALL
                    || !outcome
                        .as_ref()
                        .is_ok_and(|frame| frame.kind == OFFER && frame.code.is_empty())
                {
                    state
                        .state
                        .lock()
                        .unwrap()
                        .outbound_operations
                        .remove(&id);
                }
                let cleanup = outcome
                    .as_ref()
                    .ok()
                    .filter(|frame| frame.code.is_empty())
                    .and_then(|frame| match frame.kind {
                        DONE if frame.binding != 0 && frame.epoch != 0 => Some(Frame {
                            kind: CLOSE,
                            binding: frame.binding,
                            ..Frame::default()
                        }),
                        OFFER => Some(Frame {
                            kind: DECISION,
                            binding: frame.binding,
                            target_id: frame.target_id,
                            ..Frame::default()
                        }),
                        _ => None,
                    });
                let handed_off = delivery
                    .is_some_and(|deliver| deliver.send(outcome).is_ok())
                    && receipt.await.is_ok();
                if !handed_off
                    && let Some(cleanup) = cleanup
                {
                    // The retained call reservation owns this cleanup until it completes.
                    let context = CallContext::default();
                    let _ = Box::pin(state.request(context, cleanup)).await;
                }
                owner_reservation.lock().unwrap().take();
            });
            let frame = delivered.await.map_err(|_| self.error())??;
            // A queued oneshot reply can still be dropped without being polled.
            // Acknowledge only once this future actually receives the frame.
            let _ = received.send(());
            reservation.lock().unwrap().take();
            if !frame.code.is_empty() {
                return Err(Status::new(frame.code, frame.message));
            }
            Ok(frame)
        })
    }
    async fn read_loop(self: &Arc<Self>, ready: watch::Sender<bool>) -> Result<()> {
        let mut assembly = Assembly::default();
        let mut data_progress = Instant::now();
        loop {
            let read_timeout = if assembly.has_pending_data() {
                self.options
                    .lease_ttl
                    .saturating_sub(data_progress.elapsed())
            } else {
                self.options.lease_ttl
            };
            let fragment = tokio::select! {
                _ = self.stopping.cancelled() => return Ok(()),
                result = super::platform::timeout(read_timeout, self.conn.read()) => {
                    result
                        .map_err(|_| Status::new("deadline_exceeded", "RPC physical read timed out"))??
                },
            };
            let assembled = assembly.push(&fragment, &self.options.limits)?;
            if !assembly.last_was_control() {
                data_progress = Instant::now();
            }
            let Some(payload) = assembled else {
                continue;
            };
            let physical_control = assembly.last_was_control();
            let frame = Frame::decode(&payload, &self.options.limits)?;
            if physical_control && !is_zero_control(frame.kind) {
                return Err(Status::protocol("RPC data message used control lane"));
            }
            if frame.kind == HELLO {
                {
                    let mut state = self.state.lock().unwrap();
                    if frame.protocol != ENDPOINT_PROTOCOL
                        || frame.origin == self.origin
                        || !state.peer.is_empty()
                    {
                        return Err(Status::protocol("invalid or duplicate RPC hello"));
                    }
                    state.peer = frame.origin;
                    state.peer_lease_ttl = Duration::from_nanos(frame.lease_ttl as u64);
                    state.outbound_limits = self
                        .options
                        .limits
                        .intersect(frame.limits.as_ref().unwrap());
                    // No outbound admission occurs before READY, so shrinking
                    // these capacities cannot race an existing reservation.
                    let local = &self.options.limits;
                    let outbound = &state.outbound_limits;
                    self.data_slots
                        .forget_permits(local.max_pending_calls - outbound.max_pending_calls);
                    self.control_slots
                        .forget_permits(local.max_pending_controls - outbound.max_pending_controls);
                    self.outbound_bindings
                        .forget_permits(local.max_bindings - outbound.max_bindings);
                    self.outbound_results
                        .forget_permits(local.max_pending_results - outbound.max_pending_results);
                }
                self.enqueue(Frame {
                    kind: READY,
                    ..Frame::default()
                })
                .await?;
                continue;
            }
            {
                let state = self.state.lock().unwrap();
                if state.peer.is_empty() || state.peer != frame.origin {
                    return Err(Status::protocol("RPC peer origin changed"));
                }
            }
            if frame.kind == READY {
                let mut state = self.state.lock().unwrap();
                if state.peer_ready {
                    return Err(Status::protocol("duplicate RPC ready"));
                }
                state.peer_ready = true;
                ready.send_replace(true);
                continue;
            }
            if frame.reply {
                let pending = if frame.kind == ACCEPTED {
                    let state = self.state.lock().unwrap();
                    if let Some(pending) = state.pending.get(&frame.target_id) {
                        if pending.kind == RENEW
                            || *pending.accepted.borrow()
                            || pending.binding != frame.binding
                        {
                            return Err(Status::protocol("RPC invalid admission acknowledgement"));
                        }
                        let _ = pending.accepted.send(true);
                    }
                    None
                } else {
                    self.state.lock().unwrap().pending.remove(&frame.target_id)
                };
                if let Some(pending) = pending {
                    let compatible = pending.kind == RENEW && frame.kind == RENEW_ACK
                        || pending.kind == CALL
                            && (frame.kind == OFFER
                                || frame.kind == DONE && !frame.code.is_empty())
                        || matches!(pending.kind, BIND | DROP | CLOSE | DECISION)
                            && frame.kind == DONE;
                    if !compatible || pending.kind != BIND && pending.binding != frame.binding {
                        return Err(Status::protocol("RPC response kind mismatch"));
                    }
                    if pending.kind == BIND
                        && frame.code.is_empty()
                        && (frame.binding == 0 || frame.epoch == 0 || frame.lease_ttl < 1_000_000)
                    {
                        return Err(Status::protocol("RPC invalid binding grant"));
                    }
                    let _ = pending.reply.send(Ok(frame));
                }
                continue;
            }
            if frame.kind == CANCEL {
                let cancellation = self
                    .state
                    .lock()
                    .unwrap()
                    .active
                    .get(&frame.target_id)
                    .cloned();
                if let Some(cancellation) = cancellation {
                    cancellation.cancel();
                }
                continue;
            }
            {
                let mut state = self.state.lock().unwrap();
                if frame.kind == RENEW {
                    if frame.id <= state.peer_renew {
                        return Err(Status::protocol("RPC renewal IDs are not increasing"));
                    }
                    state.peer_renew = frame.id;
                } else if frame.id <= state.peer_request
                    && !(frame.kind == DECISION && frame.id == frame.target_id)
                {
                    return Err(Status::protocol("RPC request IDs are not increasing"));
                } else if frame.kind != DECISION {
                    state.peer_request = frame.id;
                }
                if frame.kind == DECISION {
                    if let Some(accept) = state.decisions.get(&frame.id) {
                        if *accept != frame.accept {
                            return Err(Status::protocol("RPC result decision changed"));
                        }
                        continue;
                    }
                    if state.active.contains_key(&frame.id) {
                        return Err(Status::protocol("RPC operation is already active"));
                    }
                }
            }
            let slots = if matches!(frame.kind, BIND | CALL) {
                &self.inbound_slots
            } else {
                &self.inbound_controls
            };
            let permit = match slots.clone().try_acquire_owned() {
                Ok(permit) => permit,
                Err(_) => {
                    self.enqueue(Frame {
                        kind: match frame.kind {
                            CALL => OFFER,
                            RENEW => RENEW_ACK,
                            _ => DONE,
                        },
                        target_id: frame.id,
                        binding: frame.binding,
                        reply: true,
                        code: "resource_exhausted".into(),
                        message: "RPC inbound request limit exceeded".into(),
                        ..Frame::default()
                    })
                    .await?;
                    continue;
                }
            };
            let reservation = match frame.kind {
                BIND => self
                    .binding_slots
                    .clone()
                    .try_acquire_owned()
                    .map(Some)
                    .map_err(|_| "RPC binding limit exceeded"),
                CALL => self
                    .result_slots
                    .clone()
                    .try_acquire_owned()
                    .map(Some)
                    .map_err(|_| "RPC result limit exceeded"),
                _ => Ok(None),
            };
            let reservation = match reservation {
                Ok(reservation) => reservation,
                Err(message) => {
                    self.enqueue(Frame {
                        kind: if frame.kind == CALL { OFFER } else { DONE },
                        target_id: frame.id,
                        binding: frame.binding,
                        reply: true,
                        code: "resource_exhausted".into(),
                        message: message.into(),
                        ..Frame::default()
                    })
                    .await?;
                    drop(permit);
                    continue;
                }
            };
            let cancellation = Cancellation::default();
            {
                let mut state = self.state.lock().unwrap();
                state.active.insert(frame.id, cancellation.clone());
                if frame.kind == DECISION {
                    state.decisions.insert(frame.id, frame.accept);
                }
                match (frame.kind, reservation) {
                    (BIND, Some(permit)) => {
                        state.binding_admissions.insert(frame.id, permit);
                    }
                    (CALL, Some(permit)) => {
                        state.result_admissions.insert(frame.id, permit);
                    }
                    _ => {}
                }
            }
            if matches!(frame.kind, BIND | CALL | DROP | CLOSE) {
                let expires = Instant::now() + self.options.lease_ttl;
                let mut state = self.state.lock().unwrap();
                state.active_leases.insert(frame.id, expires);
                state.active_bindings.insert(frame.id, frame.binding);
            }
            let timeout = (frame.timeout > 0).then(|| Duration::from_nanos(frame.timeout as u64));
            let mut timeout = timeout;
            if frame.kind == CALL
                && self
                    .options
                    .max_call_duration
                    .is_some_and(|limit| timeout.is_none_or(|current| limit < current))
            {
                timeout = self.options.max_call_duration;
            }
            let context = CallContext {
                cancellation,
                deadline: timeout.map(|timeout| Instant::now() + timeout),
                peer: self.options.peer.clone(),
                ..CallContext::default()
            };
            let state = self.clone();
            self.runtime.spawn(async move {
                let kind = frame.kind;
                let mut reply = Frame {
                    kind: match frame.kind {
                        CALL => OFFER,
                        RENEW => RENEW_ACK,
                        _ => DONE,
                    },
                    target_id: frame.id,
                    binding: frame.binding,
                    reply: true,
                    ..Frame::default()
                };
                if let Err(error) =
                    super::binding::invoke_user(state.dispatch(context, frame, &mut reply)).await
                {
                    reply.code = error.code;
                    reply.message = error.message;
                }
                if kind == CALL {
                    let mut protocol = state.state.lock().unwrap();
                    protocol.active.remove(&reply.target_id);
                    protocol.active_leases.remove(&reply.target_id);
                    protocol.active_bindings.remove(&reply.target_id);
                    protocol.result_admissions.remove(&reply.target_id);
                }
                if let Err(error) = state.enqueue_written(reply.clone(), None, None).await {
                    state.fail(error);
                }
                {
                    let mut protocol = state.state.lock().unwrap();
                    if kind != CALL {
                        protocol.active.remove(&reply.target_id);
                        protocol.active_leases.remove(&reply.target_id);
                        protocol.active_bindings.remove(&reply.target_id);
                        protocol.decisions.remove(&reply.target_id);
                    }
                    protocol.binding_admissions.remove(&reply.target_id);
                    if kind != CALL {
                        protocol.result_admissions.remove(&reply.target_id);
                    }
                }
                drop(permit);
            });
        }
    }
    async fn dispatch(
        self: &Arc<Self>,
        context: CallContext,
        frame: Frame,
        reply: &mut Frame,
    ) -> Result<()> {
        // Keep admission and its outcome in one operation task. The reader
        // remains available for cancellation while the physical write waits.
        if matches!(frame.kind, BIND | CALL | DROP | CLOSE | DECISION)
            && let Err(error) = self
                .enqueue_written(
                    Frame {
                        kind: ACCEPTED,
                        target_id: frame.id,
                        binding: frame.binding,
                        reply: true,
                        ..Frame::default()
                    },
                    None,
                    None,
                )
                .await
        {
            self.fail(error.clone());
            return Err(error);
        }
        match frame.kind {
            BIND => {
                let binder = self.binder.as_ref().ok_or_else(|| {
                    Status::new("unimplemented", "RPC endpoint has no inbound binder")
                })?;
                let permit = self
                    .state
                    .lock()
                    .unwrap()
                    .binding_admissions
                    .remove(&frame.id)
                    .ok_or_else(|| Status::exhausted("RPC binding admission expired"))?;
                let request = BindRequest {
                    contract: frame.contract.unwrap(),
                    options: frame.options,
                    peer: self.options.peer.clone(),
                    hops: frame.hops,
                    provider: String::new(),
                };
                let routes = context.run(binder.bind(context.clone(), request)).await?;
                context.check()?;
                let id = self
                    .next_binding
                    .fetch_update(Ordering::AcqRel, Ordering::Acquire, |v| v.checked_add(1))
                    .map_err(|_| Status::exhausted("RPC binding IDs exhausted"))?;
                reply.binding = id;
                reply.epoch = routes.epoch;
                let binding_expires = Instant::now() + self.options.lease_ttl;
                reply.lease_ttl = self.options.lease_ttl.as_nanos().min(i64::MAX as u128) as i64;
                let mut state = self.state.lock().unwrap();
                if state.failure.is_some() {
                    return Err(Status::new("unavailable", "RPC endpoint closed"));
                }
                state.bindings.insert(
                    id,
                    InboundBinding {
                        routes,
                        _permit: permit,
                    },
                );
                state.binding_leases.insert(id, binding_expires);
            }
            CALL => {
                let routes = self.inbound_routes(frame.binding)?;
                let permit = self
                    .state
                    .lock()
                    .unwrap()
                    .result_admissions
                    .remove(&frame.id)
                    .ok_or_else(|| Status::exhausted("RPC result admission expired"))?;
                let call = frame.call.unwrap();
                let result = routes
                    .invoke(
                        context.clone(),
                        Call {
                            method: call.method,
                            receiver: call.receiver,
                            arguments: decode_values(&call.arguments, &self.options.limits)?,
                        },
                    )
                    .await?;
                context.check()?;
                reply.values = encode_values(&result.values, &self.options.limits)?;
                let expiration = Cancellation::default();
                let result_expires = self
                    .state
                    .lock()
                    .unwrap()
                    .active_leases
                    .get(&frame.id)
                    .copied()
                    .filter(|expires| *expires > Instant::now())
                    .ok_or_else(|| Status::new("unavailable", "RPC operation lease expired"))?;
                reply.lease_ttl = self.options.lease_ttl.as_nanos().min(i64::MAX as u128) as i64;
                {
                    let mut state = self.state.lock().unwrap();
                    if state.failure.is_some() || !state.bindings.contains_key(&frame.binding) {
                        return Err(Status::new("unavailable", "RPC binding closed during call"));
                    }
                    state.results.insert(
                        frame.id,
                        InboundResult {
                            binding: frame.binding,
                            result,
                            expiration: expiration.clone(),
                            expires_at: result_expires,
                            _permit: permit,
                        },
                    );
                }
                reply.target_id = frame.id;
                reply.kind = OFFER;
            }
            DECISION => {
                let entry = {
                    let mut state = self.state.lock().unwrap();
                    if !state.results.get(&frame.target_id).is_some_and(|entry| {
                        entry.binding == frame.binding && entry.expires_at > Instant::now()
                    }) {
                        return Err(Status::new("not_found", "RPC result decision is stale"));
                    }
                    let entry = state.results.remove(&frame.target_id).unwrap();
                    state
                        .active_leases
                        .insert(frame.target_id, entry.expires_at);
                    state.active_bindings.insert(frame.target_id, frame.binding);
                    entry
                };
                entry.expiration.cancel();
                self.state
                    .lock()
                    .unwrap()
                    .result_admissions
                    .insert(frame.id, entry._permit);
                let outcome = if frame.accept {
                    entry.result.accept().await.map(|result| {
                        if !context.cancellation.is_cancelled() {
                            result.consume();
                        }
                    })
                } else {
                    entry.result.discard().await
                };
                outcome?;
                reply.kind = DONE;
            }
            DROP => {
                let routes = self.inbound_routes(frame.binding)?;
                routes
                    .drop_resource(CallContext::default(), frame.reference.unwrap())
                    .await?;
            }
            CLOSE => {
                let (binding, results, active) = {
                    let mut state = self.state.lock().unwrap();
                    let binding = state.bindings.remove(&frame.binding);
                    state.binding_leases.remove(&frame.binding);
                    let ids: Vec<_> = state
                        .results
                        .iter()
                        .filter(|(_, entry)| entry.binding == frame.binding)
                        .map(|(id, _)| *id)
                        .collect();
                    let results: Vec<_> = ids
                        .into_iter()
                        .filter_map(|id| state.results.remove(&id))
                        .collect();
                    let active_ids: Vec<_> = state
                        .active_bindings
                        .iter()
                        .filter_map(|(id, binding)| {
                            (*binding == frame.binding && *id != frame.id).then_some(*id)
                        })
                        .collect();
                    let active: Vec<Cancellation> = active_ids
                        .into_iter()
                        .filter_map(|id| {
                            state.active_leases.remove(&id);
                            state.active_bindings.remove(&id);
                            state.active.get(&id).cloned()
                        })
                        .collect();
                    (binding, results, active)
                };
                for cancellation in active {
                    cancellation.cancel();
                }
                if let Some(binding) = &binding {
                    binding.routes.binding.begin_shutdown();
                }
                let mut cleanup_error = None;
                for entry in results {
                    entry.expiration.cancel();
                    if let Err(error) = entry.result.discard().await {
                        cleanup_error.get_or_insert(error);
                    }
                    drop(entry._permit);
                }
                if let Some(binding) = binding {
                    if let Err(error) = binding.routes.shutdown().await {
                        cleanup_error.get_or_insert(error);
                    }
                    drop(binding._permit);
                }
                if let Some(error) = cleanup_error {
                    return Err(error);
                }
            }
            RENEW => {
                reply.kind = RENEW_ACK;
                reply.lease_ttl = self.options.lease_ttl.as_nanos().min(i64::MAX as u128) as i64;
                reply.values = self.renew_inbound_targets(&frame.values)?;
            }
            _ => return Err(Status::protocol("unexpected RPC request")),
        }
        Ok(())
    }
}

fn is_zero_control(kind: u64) -> bool {
    matches!(
        kind,
        ACCEPTED | RENEW | RENEW_ACK | CANCEL | DECISION | DONE
    )
}

struct RemoteBinding {
    endpoint: Arc<EndpointState>,
    id: u64,
    epoch: u64,
    resources: Arc<Mutex<BTreeMap<u64, (ResourceRef, bool)>>>,
    closing: Cancellation,
    done: watch::Receiver<Option<Result<()>>>,
    lifecycle: Arc<RemoteLifecycle>,
    drops: Arc<Mutex<BTreeMap<u64, ResourceDrop>>>,
}
struct BindingDelivery(Option<Arc<RemoteBinding>>);
impl Drop for BindingDelivery {
    fn drop(&mut self) {
        if let Some(binding) = &self.0 {
            binding.begin_shutdown();
        }
    }
}
struct ResourceDrop {
    done: watch::Receiver<Option<Result<()>>>,
}
struct RemoteLifecycle {
    calls: AtomicUsize,
    draining: AtomicBool,
    closing: Cancellation,
    resources: Arc<Mutex<BTreeMap<u64, (ResourceRef, bool)>>>,
}
impl RemoteLifecycle {
    fn finalize_if_idle(&self) {
        if self.draining.load(Ordering::Acquire)
            && self.calls.load(Ordering::Acquire) == 0
            && self.resources.lock().unwrap().is_empty()
        {
            self.closing.cancel();
        }
    }
}
struct RemoteActivity(Arc<RemoteLifecycle>);
impl Drop for RemoteActivity {
    fn drop(&mut self) {
        self.0.calls.fetch_sub(1, Ordering::AcqRel);
        self.0.finalize_if_idle();
    }
}
impl Binder for Endpoint {
    fn bind(
        &self,
        context: CallContext,
        request: BindRequest,
    ) -> BoxFuture<'_, Result<Arc<RouteSet>>> {
        Box::pin(async move {
            let contract = request.contract.normalized(&self.state.options.limits)?;
            self.state.ready(context.clone()).await?;
            let permit = self
                .state
                .outbound_bindings
                .clone()
                .try_acquire_owned()
                .map_err(|_| Status::exhausted("RPC outbound binding limit exceeded"))?;
            let reply = self
                .state
                .request(
                    context.clone(),
                    Frame {
                        kind: BIND,
                        contract: Some(contract.clone()),
                        options: request.options,
                        hops: request.hops,
                        ..Frame::default()
                    },
                )
                .await?;
            let (done, receiver) = watch::channel(None);
            let resources = Arc::new(Mutex::new(BTreeMap::new()));
            let closing = Cancellation::default();
            let lifecycle = Arc::new(RemoteLifecycle {
                calls: AtomicUsize::new(0),
                draining: AtomicBool::new(false),
                closing: closing.clone(),
                resources: resources.clone(),
            });
            let binding = Arc::new(RemoteBinding {
                endpoint: self.state.clone(),
                id: reply.binding,
                epoch: reply.epoch,
                resources,
                closing,
                done: receiver,
                lifecycle,
                drops: Arc::new(Mutex::new(BTreeMap::new())),
            });
            let routes = RouteSet::new(contract.clone(), reply.epoch, binding.clone());
            {
                let granted = Duration::from_nanos(reply.lease_ttl as u64);
                let expires = Instant::now() + granted;
                let mut state = self.state.state.lock().unwrap();
                if state.outbound_bindings.contains_key(&reply.binding) {
                    drop(state);
                    let error = Status::protocol("RPC binding identity is already active");
                    self.state.fail(error.clone());
                    return Err(error);
                }
                state
                    .outbound_bindings
                    .insert(reply.binding, routes.clone());
                state.outbound_binding_leases.insert(reply.binding, expires);
            }
            let owner = binding.clone();
            self.state.runtime.spawn(async move {
                tokio::select! { _ = owner.closing.cancelled() => {}, _ = owner.endpoint.stopping.cancelled() => {} }
                let context = CallContext::default();
                let result = if owner.endpoint.stopping.is_cancelled() { Ok(()) } else { owner.endpoint.request(context, Frame { kind: CLOSE, binding: owner.id, ..Frame::default() }).await.map(|_| ()) };
                // Once the transport has closed, only local revocation remains.
                let result = if owner.endpoint.stopping.is_cancelled() { Ok(()) } else { result };
                owner.endpoint.record_cleanup(result.clone());
                owner.resources.lock().unwrap().clear(); done.send_replace(Some(result)); drop(permit);
                let mut state = owner.endpoint.state.lock().unwrap();
                state.outbound_bindings.remove(&owner.id);
                state.outbound_binding_leases.remove(&owner.id);
            });
            let mut delivery = BindingDelivery(Some(binding));
            let sent_at = Instant::now();
            let confirmation = self
                .state
                .request(
                    context,
                    Frame {
                        kind: RENEW,
                        values: encode_lease_targets(&[reply.binding], &[]),
                        ..Frame::default()
                    },
                )
                .await;
            let confirmation = confirmation?;
            let (confirmed, _) =
                decode_lease_targets(&confirmation.values, &self.state.options.limits)?;
            if !confirmed.contains(&reply.binding)
                || sent_at + Duration::from_nanos(confirmation.lease_ttl.max(0) as u64)
                    <= Instant::now()
            {
                return Err(Status::new(
                    "unavailable",
                    "RPC binding lease was not confirmed",
                ));
            }
            self.state.apply_renewal_ack(
                &[reply.binding],
                &[],
                sent_at,
                confirmation.lease_ttl,
                &confirmation.values,
            )?;
            let _ = delivery.0.take();
            Ok(routes)
        })
    }
}
impl Binding for RemoteBinding {
    fn resource_count(&self) -> usize {
        self.resources.lock().unwrap().len()
    }
    fn drain(&self) {
        self.lifecycle.draining.store(true, Ordering::Release);
        self.lifecycle.finalize_if_idle();
    }
    fn invoke(&self, context: CallContext, call: Call) -> BoxFuture<'_, Result<PendingResult>> {
        Box::pin(async move {
            if self.closing.is_cancelled()
                || call.receiver.is_none() && self.lifecycle.draining.load(Ordering::Acquire)
            {
                return Err(Status::new("unavailable", "RPC binding closed"));
            }
            if let Some(reference) = &call.receiver {
                self.validate_resource(reference)?;
            }
            for value in &call.arguments {
                if let Data::Resource(reference) = &value.data {
                    self.validate_resource(reference)?;
                }
            }
            {
                let resources = self.resources.lock().unwrap();
                for reference in call
                    .receiver
                    .iter()
                    .chain(call.arguments.iter().filter_map(|v| {
                        if let Data::Resource(r) = &v.data {
                            Some(r)
                        } else {
                            None
                        }
                    }))
                {
                    if !resources
                        .get(&reference.object_id)
                        .is_some_and(|(_, active)| *active)
                    {
                        return Err(Status::new(
                            "invalid_argument",
                            "RPC resource is provisional",
                        ));
                    }
                }
            }
            let arguments = encode_values(&call.arguments, &self.endpoint.options.limits)?;
            self.lifecycle.calls.fetch_add(1, Ordering::AcqRel);
            let activity = RemoteActivity(self.lifecycle.clone());
            let result_permit = self
                .endpoint
                .outbound_results
                .clone()
                .try_acquire_owned()
                .map_err(|_| Status::exhausted("RPC pending result limit exceeded"))?;
            let reply = self
                .endpoint
                .request(
                    context,
                    Frame {
                        kind: CALL,
                        binding: self.id,
                        call: Some(WireCall {
                            method: call.method,
                            receiver: call.receiver,
                            arguments,
                        }),
                        ..Frame::default()
                    },
                )
                .await?;
            let decoded = (|| {
                let values = decode_values(&reply.values, &self.endpoint.options.limits)?;
                let mut references = Vec::new();
                let dropped = self
                    .drops
                    .lock()
                    .unwrap()
                    .keys()
                    .copied()
                    .collect::<std::collections::BTreeSet<_>>();
                let mut resources = self.resources.lock().unwrap();
                let mut aliases = BTreeMap::new();
                for value in &values {
                    if let Data::Resource(reference) = &value.data {
                        if reference.epoch != self.epoch {
                            return Err(Status::protocol(
                                "RPC reply contains invalid resource identity",
                            ));
                        }
                        if dropped.contains(&reference.object_id) {
                            return Err(Status::protocol(
                                "RPC reply contains a resource being dropped",
                            ));
                        }
                        if let Some((stored, active)) = resources.get(&reference.object_id) {
                            if stored != reference || !*active {
                                return Err(Status::protocol(
                                    "RPC reply contains an unconfirmed resource identity",
                                ));
                            }
                            continue;
                        }
                        if let Some(stored) = aliases.get(&reference.object_id) {
                            if stored != reference {
                                return Err(Status::protocol(
                                    "RPC reply contains conflicting resource identity",
                                ));
                            }
                            continue;
                        }
                        aliases.insert(reference.object_id, reference.clone());
                        references.push(reference.clone());
                    }
                }
                if resources.len() + references.len() > self.endpoint.options.limits.max_resources {
                    return Err(Status::exhausted("RPC resource limit exceeded"));
                }
                for reference in &references {
                    resources.insert(reference.object_id, (reference.clone(), false));
                }
                Ok((values, references))
            })();
            let (values, references, mut failure) = match decoded {
                Ok((values, references)) => (values, references, None),
                Err(error) => (Vec::new(), Vec::new(), Some(error)),
            };
            let result_expiration = self
                .endpoint
                .state
                .lock()
                .unwrap()
                .outbound_operations
                .get(&reply.target_id)
                .filter(|(expires, _)| *expires > Instant::now())
                .map(|(_, cancellation)| cancellation.clone())
                .unwrap_or_else(|| {
                    let cancellation = Cancellation::default();
                    cancellation.cancel();
                    cancellation
                });
            if result_expiration.is_cancelled() && failure.is_none() {
                failure = Some(Status::new(
                    "unavailable",
                    "RPC operation lease expired before offer",
                ));
            }
            let (decision, decisions) =
                oneshot::channel::<(bool, oneshot::Sender<Result<AcceptedResult>>)>();
            let endpoint = self.endpoint.clone();
            let closing = self.closing.clone();
            let binding = self.id;
            let owned_values = values.clone();
            let registry = self.resources.clone();
            let lifecycle = self.lifecycle.clone();
            self.endpoint.runtime.spawn(async move {
                let decision = tokio::select! { _ = closing.cancelled() => None, _ = endpoint.stopping.cancelled() => None, _ = result_expiration.cancelled() => None, decision = decisions => decision.ok() };
                let accept = decision.as_ref().is_some_and(|(accept, _)| *accept);
                let context = CallContext::default();
                let result = endpoint.request(context, Frame { kind: DECISION, binding, target_id: reply.target_id, accept, ..Frame::default() }).await;
                if result.as_ref().is_err_and(|error| matches!(error.code.as_str(), "canceled" | "deadline_exceeded" | "unavailable")) { closing.cancel(); }
                endpoint
                    .state
                    .lock()
                    .unwrap()
                    .outbound_operations
                    .remove(&reply.target_id);
                {
                    let mut registry = registry.lock().unwrap();
                    for reference in &references {
                        if accept && result.is_ok() { if let Some((_, active)) = registry.get_mut(&reference.object_id) { *active = true; } }
                        else { registry.remove(&reference.object_id); }
                    }
                }
                if let Some((_, ack)) = decision {
                    let result = result.map(|_| {
                        let release = if accept && !references.is_empty() {
                            let (release, released) = oneshot::channel::<()>();
                            let cleanup = endpoint.clone();
                            let closing = closing.clone();
                            endpoint.runtime.spawn(async move {
                                let release = tokio::select! { _ = cleanup.stopping.cancelled() => false, result = released => result.is_ok() };
                                if release {
                                    for reference in references {
                                        let context = CallContext::default();
                                        let id = reference.object_id;
                                        if let Err(error) = cleanup.request(context, Frame { kind: DROP, binding, reference: Some(reference), ..Frame::default() }).await {
                                            if !cleanup.stopping.is_cancelled() { cleanup.record_cleanup(Err(error)); }
                                            closing.cancel();
                                        }
                                        registry.lock().unwrap().remove(&id);
                                    }
                                    lifecycle.finalize_if_idle();
                                }
                            });
                            Some(Box::new(move || { let _ = release.send(()); }) as Box<dyn FnOnce() + Send>)
                        } else { None };
                        AcceptedResult { values: owned_values, release, forwarded: Vec::new() }
                    });
                    let _ = ack.send(result);
                }
                drop(result_permit);
                drop(activity);
            });
            if let Some(error) = failure {
                drop(decision);
                return Err(error);
            }
            Ok(PendingResult {
                values,
                decision: Some(decision),
            })
        })
    }
    fn validate_resource(&self, reference: &ResourceRef) -> Result<()> {
        if self.closing.is_cancelled()
            || self.endpoint.stopping.is_cancelled()
            || reference.epoch != self.epoch
            || self
                .drops
                .lock()
                .unwrap()
                .contains_key(&reference.object_id)
            || !self
                .resources
                .lock()
                .unwrap()
                .get(&reference.object_id)
                .is_some_and(|(stored, _)| stored == reference)
        {
            return Err(Status::new(
                "invalid_argument",
                "RPC resource is closed or belongs to another binding",
            ));
        }
        Ok(())
    }
    fn drop_resource(
        &self,
        context: CallContext,
        reference: ResourceRef,
    ) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            context.check()?;
            if self.closing.is_cancelled() || self.endpoint.stopping.is_cancelled() {
                return Err(self.endpoint.error());
            }
            let mut done = {
                let mut drops = self.drops.lock().unwrap();
                if reference.epoch != self.epoch
                    || !self
                        .resources
                        .lock()
                        .unwrap()
                        .get(&reference.object_id)
                        .is_some_and(|(stored, _)| stored == &reference)
                {
                    return Err(Status::new(
                        "invalid_argument",
                        "RPC resource is closed or belongs to another binding",
                    ));
                }
                if let Some(drop) = drops
                    .get(&reference.object_id)
                    .filter(|drop| drop.done.borrow().is_none())
                {
                    drop.done.clone()
                } else {
                    let (sender, receiver) = watch::channel(None);
                    drops.insert(
                        reference.object_id,
                        ResourceDrop {
                            done: receiver.clone(),
                        },
                    );
                    let endpoint = self.endpoint.clone();
                    let resources = self.resources.clone();
                    let attempts = self.drops.clone();
                    let lifecycle = self.lifecycle.clone();
                    let closing = self.closing.clone();
                    let binding = self.id;
                    lifecycle.calls.fetch_add(1, Ordering::AcqRel);
                    self.endpoint.runtime.spawn(async move {
                        let _activity = RemoteActivity(lifecycle);
                        let context = CallContext::default();
                        let id = reference.object_id;
                        let result = endpoint
                            .request(
                                context,
                                Frame {
                                    kind: DROP,
                                    binding,
                                    reference: Some(reference),
                                    ..Frame::default()
                                },
                            )
                            .await
                            .map(|_| ());
                        if result.is_ok() {
                            let mut attempts = attempts.lock().unwrap();
                            resources.lock().unwrap().remove(&id);
                            attempts.remove(&id);
                        } else if let Err(error) = &result
                            && matches!(
                                error.code.as_str(),
                                "canceled" | "deadline_exceeded" | "unavailable"
                            )
                        {
                            closing.cancel();
                        }
                        sender.send_replace(Some(result));
                    });
                    receiver
                }
            };
            context
                .run(async {
                    let result = done
                        .wait_for(|value| value.is_some())
                        .await
                        .map_err(|_| Status::new("internal", "RPC resource owner failed"))?;
                    result.as_ref().unwrap().clone()
                })
                .await
        })
    }
    fn begin_shutdown(&self) {
        self.closing.cancel();
    }
    fn shutdown(&self) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            let mut done = self.done.clone();
            let result = done
                .wait_for(|r| r.is_some())
                .await
                .map_err(|_| self.endpoint.error())?;
            result.as_ref().unwrap().clone()
        })
    }
}
