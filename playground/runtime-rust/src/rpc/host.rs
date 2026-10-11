use super::guest::{GuestDeclaration, GuestProvider};
use super::platform::Handle;
use super::protocol::{FfiRequest, FfiResponse};
use super::*;
use crate::{RuntimeError, ffi};
use std::collections::BTreeMap;
use std::sync::{
    Arc, Mutex, Weak,
    atomic::{AtomicBool, AtomicU64, Ordering},
};
use std::time::Duration;
use std::time::Instant;
use tokio::sync::{OwnedSemaphorePermit, Semaphore, watch};

pub trait UnaryInvoker: Send + Sync {
    fn invoke(&self, context: CallContext, call: Call) -> BoxFuture<'_, Result<PendingResult>>;
}
pub trait UnaryInterceptor: Send + Sync {
    fn invoke(
        &self,
        context: CallContext,
        call: Call,
        next: Arc<dyn UnaryInvoker>,
    ) -> BoxFuture<'_, Result<PendingResult>>;
}

pub trait ProviderPublication: Send + Sync {
    fn close(&self) -> BoxFuture<'_, Result<()>>;
}
pub trait ProviderPublisher: Send + Sync {
    fn publish(
        &self,
        context: CallContext,
        name: String,
        provider: Arc<dyn Provider>,
    ) -> BoxFuture<'_, Result<Arc<dyn ProviderPublication>>>;
}
struct BoundInvoker(Arc<RouteSet>);
impl UnaryInvoker for BoundInvoker {
    fn invoke(&self, context: CallContext, call: Call) -> BoxFuture<'_, Result<PendingResult>> {
        Box::pin(self.0.invoke(context, call))
    }
}
struct Intercepted {
    interceptor: Arc<dyn UnaryInterceptor>,
    next: Arc<dyn UnaryInvoker>,
}
impl UnaryInvoker for Intercepted {
    fn invoke(&self, context: CallContext, call: Call) -> BoxFuture<'_, Result<PendingResult>> {
        self.interceptor.invoke(context, call, self.next.clone())
    }
}

pub struct HostOptions {
    pub runtime: Handle,
    pub providers: Vec<Arc<dyn Provider>>,
    pub fallback: Option<Arc<dyn Binder>>,
    pub limits: Limits,
    /// Maximum number of FFI sessions, including sessions still cleaning up.
    pub max_sessions: usize,
    pub interceptors: Vec<Arc<dyn UnaryInterceptor>>,
    pub publish_provider: Option<Arc<dyn ProviderPublisher>>,
}
impl HostOptions {
    pub fn new(runtime: Handle) -> Self {
        let limits = Limits::default();
        Self {
            runtime,
            providers: Vec::new(),
            fallback: None,
            max_sessions: 1024,
            limits,
            interceptors: Vec::new(),
            publish_provider: None,
        }
    }
}
struct Fallback {
    local: LocalBinder,
    fallback: Arc<dyn Binder>,
}
impl Binder for Fallback {
    fn bind(
        &self,
        context: CallContext,
        request: BindRequest,
    ) -> BoxFuture<'_, Result<Arc<RouteSet>>> {
        Box::pin(async move {
            match self.local.bind(context.clone(), request.clone()).await {
                Err(error) if error.code == "unimplemented" => {
                    self.fallback.bind(context, request).await
                }
                result => result,
            }
        })
    }
}

struct HostState {
    closing: bool,
    sessions: Vec<Weak<SessionState>>,
}
pub struct Host {
    options: Arc<HostOptions>,
    binder: Arc<dyn Binder>,
    state: Mutex<HostState>,
    done: watch::Sender<Option<Result<()>>>,
}
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct HostStats {
    pub sessions: usize,
    pub leases: usize,
    pub pending_calls: usize,
    pub pending_results: usize,
    pub resources: usize,
}
impl Host {
    pub fn stats(&self) -> HostStats {
        let sessions = self
            .state
            .lock()
            .unwrap()
            .sessions
            .iter()
            .filter_map(Weak::upgrade)
            .collect::<Vec<_>>();
        let mut stats = HostStats::default();
        for session in sessions {
            if !session.closing.is_cancelled() {
                stats.sessions += 1;
            }
            let leases = session.leases.lock().unwrap();
            stats.leases += leases.len();
            for lease in leases.values() {
                if let LeaseValue::Routes(routes) = &lease.value {
                    stats.resources += routes.resource_count();
                }
            }
            stats.pending_calls +=
                session.options.limits.max_pending_calls - session.calls.available_permits();
            stats.pending_results += session.results.lock().unwrap().len();
        }
        stats
    }
    pub fn new(mut options: HostOptions) -> Result<Self> {
        if options.max_sessions == 0 {
            return Err(Status::new(
                "invalid_argument",
                "RPC host session limit must be positive",
            ));
        }
        let providers = std::mem::take(&mut options.providers);
        let local = LocalBinder::new(options.runtime.clone(), options.limits.clone(), providers)?;
        let binder: Arc<dyn Binder> = if let Some(fallback) = options.fallback.take() {
            Arc::new(Fallback { local, fallback })
        } else {
            Arc::new(local)
        };
        Ok(Self {
            options: Arc::new(options),
            binder,
            state: Mutex::new(HostState {
                closing: false,
                sessions: Vec::new(),
            }),
            done: watch::channel(None).0,
        })
    }
    pub fn begin_shutdown(&self) {
        let sessions = {
            let mut state = self.state.lock().unwrap();
            if state.closing {
                return;
            }
            state.closing = true;
            state
                .sessions
                .iter()
                .filter_map(Weak::upgrade)
                .collect::<Vec<_>>()
        };
        for session in &sessions {
            session.closing.cancel();
        }
        let done = self.done.clone();
        self.options.runtime.spawn(async move {
            let mut result = Ok(());
            for session in sessions {
                if let Err(error) = session.wait_closed().await {
                    result = Err(error);
                }
            }
            done.send_replace(Some(result));
        });
    }
    pub async fn shutdown(&self) -> Result<()> {
        self.begin_shutdown();
        let mut done = self.done.subscribe();
        let result = done
            .wait_for(Option::is_some)
            .await
            .map_err(|_| Status::new("internal", "RPC host cleanup failed"))?;
        result.as_ref().unwrap().clone()
    }
}
impl Drop for Host {
    fn drop(&mut self) {
        self.begin_shutdown();
    }
}
impl ffi::Bridge for Host {
    fn open(
        &self,
        cancellation: ffi::Cancellation,
    ) -> std::result::Result<Box<dyn ffi::Session>, RuntimeError> {
        let mut host = self.state.lock().unwrap();
        if host.closing || cancellation.is_cancelled() {
            return Err(RuntimeError::new(
                "ffi_closed",
                "$rpc",
                "RPC host closed or canceled",
            ));
        }
        host.sessions.retain(|entry| {
            entry
                .upgrade()
                .is_some_and(|session| !session.finished.load(Ordering::Acquire))
        });
        if host.sessions.len() >= self.options.max_sessions {
            return Err(RuntimeError::new(
                "ffi_limit",
                "$rpc",
                "RPC session limit exceeded",
            ));
        }
        let (done, receiver) = watch::channel(None);
        let state = Arc::new(SessionState {
            options: self.options.clone(),
            binder: self.binder.clone(),
            closing: ffi::Cancellation::default(),
            leases: Mutex::new(BTreeMap::new()),
            results: Mutex::new(BTreeMap::new()),
            next_lease: AtomicU64::new(1),
            next_result: AtomicU64::new(1),
            calls: Arc::new(Semaphore::new(self.options.limits.max_pending_calls)),
            lease_slots: Arc::new(Semaphore::new(self.options.limits.max_bindings)),
            result_slots: Arc::new(Semaphore::new(self.options.limits.max_pending_results)),
            result_bytes: Arc::new(Semaphore::new(self.options.limits.max_in_flight_bytes)),
            finished: AtomicBool::new(false),
            done: receiver,
        });
        host.sessions.push(Arc::downgrade(&state));
        let owner = state.clone();
        self.options.runtime.spawn(async move {
            owner.closing.cancelled().await;
            let _calls = owner
                .calls
                .acquire_many(owner.options.limits.max_pending_calls as u32)
                .await;
            let results = std::mem::take(&mut *owner.results.lock().unwrap());
            for result in results.into_values() {
                let _ = result.result.discard().await;
            }
            let leases: Vec<_> = owner.leases.lock().unwrap().values().cloned().collect();
            for lease in &leases {
                lease.closing.cancel();
            }
            let mut result = Ok(());
            for lease in leases {
                if let Err(error) = lease.wait_closed().await {
                    result = Err(error);
                }
            }
            drop(_calls);
            owner.finished.store(true, Ordering::Release);
            done.send_replace(Some(result));
        });
        Ok(Box::new(Session(state)))
    }
    fn capabilities(&self) -> Vec<String> {
        vec![FFI_ROUTE.into()]
    }
}

struct Lease {
    value: LeaseValue,
    closing: ffi::Cancellation,
    done: watch::Receiver<Option<Result<()>>>,
}
enum LeaseValue {
    Routes(Arc<RouteSet>),
    Provider(Arc<GuestProvider>, Arc<dyn ProviderPublication>),
}
impl Lease {
    async fn wait_closed(&self) -> Result<()> {
        let mut done = self.done.clone();
        let result = done
            .wait_for(|value| value.is_some())
            .await
            .map_err(|_| Status::new("internal", "RPC lease owner failed"))?;
        result.as_ref().unwrap().clone()
    }
}
struct HeldResult {
    lease: u64,
    result: PendingResult,
    _slot: OwnedSemaphorePermit,
    _bytes: OwnedSemaphorePermit,
}
struct SessionState {
    options: Arc<HostOptions>,
    binder: Arc<dyn Binder>,
    closing: ffi::Cancellation,
    leases: Mutex<BTreeMap<u64, Arc<Lease>>>,
    results: Mutex<BTreeMap<u64, HeldResult>>,
    next_lease: AtomicU64,
    next_result: AtomicU64,
    calls: Arc<Semaphore>,
    lease_slots: Arc<Semaphore>,
    result_slots: Arc<Semaphore>,
    result_bytes: Arc<Semaphore>,
    finished: AtomicBool,
    done: watch::Receiver<Option<Result<()>>>,
}
impl SessionState {
    async fn wait_closed(&self) -> Result<()> {
        let mut done = self.done.clone();
        let result = done
            .wait_for(|value| value.is_some())
            .await
            .map_err(|_| Status::new("internal", "RPC session owner failed"))?;
        result.as_ref().unwrap().clone()
    }
    async fn handle(
        self: &Arc<Self>,
        context: CallContext,
        request: FfiRequest,
    ) -> Result<ffi::Reply> {
        let mut response = FfiResponse::default();
        let mut accepted = None;
        let mut event = None;
        let mut discard: Option<Box<dyn FnOnce() + Send>> = None;
        match request.operation.as_str() {
            "open" | "open_provider" => {
                let permit = self
                    .lease_slots
                    .clone()
                    .try_acquire_owned()
                    .map_err(|_| Status::exhausted("RPC lease limit exceeded"))?;
                let id = self
                    .next_lease
                    .fetch_update(Ordering::AcqRel, Ordering::Acquire, |v| v.checked_add(1))
                    .map_err(|_| Status::exhausted("RPC lease IDs exhausted"))?;
                let value = if request.operation == "open_provider" {
                    let publisher = self.options.publish_provider.as_ref().ok_or_else(|| {
                        Status::new("unavailable", "RPC provider publication is not configured")
                    })?;
                    let provider = GuestProvider::new(
                        self.options.runtime.clone(),
                        request.contract,
                        self.options.limits.clone(),
                    )?;
                    let publication = publisher
                        .publish(
                            context.clone(),
                            request.target,
                            Arc::new(GuestDeclaration(provider.clone())),
                        )
                        .await?;
                    LeaseValue::Provider(provider, publication)
                } else {
                    let routes = self
                        .binder
                        .bind(
                            context.clone(),
                            BindRequest {
                                contract: request.contract,
                                options: request.options,
                                peer: PeerInfo::default(),
                                provider: String::new(),
                                hops: 0,
                            },
                        )
                        .await?;
                    LeaseValue::Routes(routes)
                };
                let (done, receiver) = watch::channel(None);
                let lease = Arc::new(Lease {
                    value,
                    closing: ffi::Cancellation::default(),
                    done: receiver,
                });
                self.leases.lock().unwrap().insert(id, lease.clone());
                let owner = Arc::downgrade(self);
                let cleanup = lease.clone();
                let shutdown = self.closing.clone();
                self.options.runtime.spawn(async move {
                    tokio::select! { _ = cleanup.closing.cancelled() => {}, _ = shutdown.cancelled() => {} }
                    cleanup.closing.cancel();
                    if let Some(owner) = owner.upgrade() {
                        let results = {
                            let mut results = owner.results.lock().unwrap();
                            let ids: Vec<_> = results.iter().filter(|(_, r)| r.lease == id).map(|(id, _)| *id).collect();
                            ids.into_iter().filter_map(|id| results.remove(&id)).collect::<Vec<_>>()
                        };
                        for result in results { let _ = result.result.discard().await; }
                    }
                    let result = match &cleanup.value {
                        LeaseValue::Routes(routes) => routes.shutdown().await,
                        LeaseValue::Provider(provider, publication) => { provider.close(); publication.close().await },
                    };
                    if let Some(owner) = owner.upgrade() { owner.leases.lock().unwrap().remove(&id); }
                    done.send_replace(Some(result)); drop(permit);
                });
                response.lease = id;
                discard = Some(Box::new(move || lease.closing.cancel()));
            }
            "call" | "call_owned" => {
                let lease = self
                    .leases
                    .lock()
                    .unwrap()
                    .get(&request.lease)
                    .cloned()
                    .ok_or_else(|| Status::new("unavailable", "RPC lease closed"))?;
                if lease.closing.is_cancelled() {
                    return Err(Status::new("unavailable", "RPC lease closed"));
                }
                let LeaseValue::Routes(routes) = &lease.value else {
                    return Err(Status::new("invalid_argument", "RPC lease is a provider"));
                };
                let mut invoke: Arc<dyn UnaryInvoker> = Arc::new(BoundInvoker(routes.clone()));
                for interceptor in self.options.interceptors.iter().rev() {
                    invoke = Arc::new(Intercepted {
                        interceptor: interceptor.clone(),
                        next: invoke,
                    });
                }
                let result = invoke
                    .invoke(
                        context,
                        Call {
                            method: request.method,
                            receiver: request.receiver,
                            arguments: decode_values(&request.payload, &self.options.limits)?,
                        },
                    )
                    .await?;
                response.payload = encode_values(&result.values, &self.options.limits)?;
                if request.operation == "call_owned" {
                    let slot = self
                        .result_slots
                        .clone()
                        .try_acquire_owned()
                        .map_err(|_| Status::exhausted("RPC pending result limit exceeded"))?;
                    let bytes = self
                        .result_bytes
                        .clone()
                        .try_acquire_many_owned(response.payload.len() as u32)
                        .map_err(|_| Status::exhausted("RPC pending result byte limit exceeded"))?;
                    let id = self
                        .next_result
                        .fetch_update(Ordering::AcqRel, Ordering::Acquire, |v| v.checked_add(1))
                        .map_err(|_| Status::exhausted("RPC result IDs exhausted"))?;
                    let mut results = self.results.lock().unwrap();
                    if lease.closing.is_cancelled() || self.closing.is_cancelled() {
                        return Err(Status::new("unavailable", "RPC lease closed"));
                    }
                    results.insert(
                        id,
                        HeldResult {
                            lease: request.lease,
                            result,
                            _slot: slot,
                            _bytes: bytes,
                        },
                    );
                    let owner = Arc::downgrade(self);
                    discard = Some(Box::new(move || {
                        if let Some(owner) = owner.upgrade() {
                            owner.results.lock().unwrap().remove(&id);
                        }
                    }));
                    response.request_id = id;
                } else {
                    accepted = Some(result.accept().await?);
                }
            }
            "accept_result" | "discard_result" => {
                let result = {
                    let mut results = self.results.lock().unwrap();
                    if results
                        .get(&request.request_id)
                        .is_some_and(|result| result.lease != request.lease)
                    {
                        return Err(Status::new(
                            "invalid_argument",
                            "RPC result belongs to another lease",
                        ));
                    }
                    results.remove(&request.request_id)
                };
                if let Some(result) = result {
                    if request.operation == "accept_result" {
                        accepted = Some(result.result.accept().await?);
                    } else {
                        result.result.discard().await?;
                    }
                } else if request.operation == "accept_result" {
                    return Err(Status::new("not_found", "RPC result closed"));
                }
            }
            "release" => {
                let lease = self
                    .leases
                    .lock()
                    .unwrap()
                    .get(&request.lease)
                    .cloned()
                    .ok_or_else(|| Status::new("unavailable", "RPC lease closed"))?;
                let LeaseValue::Routes(routes) = &lease.value else {
                    return Err(Status::new("invalid_argument", "RPC lease is a provider"));
                };
                routes
                    .drop_resource(
                        context,
                        request.resource.ok_or_else(|| {
                            Status::new("invalid_argument", "release requires a resource")
                        })?,
                    )
                    .await?;
            }
            "accept" | "respond" => {
                let lease = self
                    .leases
                    .lock()
                    .unwrap()
                    .get(&request.lease)
                    .cloned()
                    .ok_or_else(|| Status::new("unavailable", "RPC guest provider closed"))?;
                let LeaseValue::Provider(provider, _) = &lease.value else {
                    return Err(Status::new(
                        "invalid_argument",
                        "RPC lease is not a provider",
                    ));
                };
                if request.operation == "accept" {
                    let delivery = provider.accept(context).await?;
                    response = delivery.response.clone();
                    event = Some(delivery);
                } else {
                    provider.respond(
                        request.request_id,
                        request.payload,
                        request.code,
                        request.message,
                    )?;
                }
            }
            "close" | "close_provider" => {
                let lease = self.leases.lock().unwrap().get(&request.lease).cloned();
                if let Some(lease) = lease {
                    lease.closing.cancel();
                    context.run(lease.wait_closed()).await?;
                }
            }
            _ => {
                return Err(Status::new(
                    "invalid_argument",
                    "unknown MRPC FFI operation",
                ));
            }
        }
        let payload = response.encode();
        if payload.len() > self.options.limits.max_message_bytes {
            if let Some(discard) = discard {
                discard();
            }
            return Err(Status::exhausted("RPC response limit exceeded"));
        }
        if let Some(accepted) = accepted {
            let receipt = Arc::new(Mutex::new(Some(accepted)));
            let consumed = receipt.clone();
            Ok(ffi::Reply::new(
                payload,
                None,
                Some(Box::new(move || {
                    receipt.lock().unwrap().take();
                })),
            )
            .on_consumed(move || {
                if let Some(accepted) = consumed.lock().unwrap().take() {
                    accepted.consume();
                }
            }))
        } else if let Some(event) = event {
            let receipt = Arc::new(Mutex::new(Some(event)));
            let consumed = receipt.clone();
            Ok(ffi::Reply::new(
                payload,
                None,
                Some(Box::new(move || {
                    receipt.lock().unwrap().take();
                })),
            )
            .on_consumed(move || {
                if let Some(event) = consumed.lock().unwrap().take() {
                    event.consume();
                }
            }))
        } else {
            Ok(ffi::Reply::new(payload, None, discard))
        }
    }
}

struct Session(Arc<SessionState>);
struct HostCall(ffi::Cancellation);
impl ffi::Call for HostCall {
    fn cancel(&self) {
        self.0.cancel();
    }
}
impl Drop for Session {
    fn drop(&mut self) {
        self.0.closing.cancel();
    }
}
impl ffi::Session for Session {
    fn start(
        &self,
        cancellation: ffi::Cancellation,
        request: ffi::Request,
        completion: ffi::Completion,
    ) -> std::result::Result<Box<dyn ffi::Call>, RuntimeError> {
        if request.route != FFI_ROUTE {
            return Err(RuntimeError::new(
                "ffi_route_unavailable",
                "$rpc",
                "RPC route unavailable",
            ));
        }
        if self.0.closing.is_cancelled() {
            return Err(RuntimeError::new(
                "ffi_closed",
                "$rpc",
                "RPC session closed",
            ));
        }
        let call = self
            .0
            .calls
            .clone()
            .try_acquire_owned()
            .map_err(|_| RuntimeError::new("ffi_limit", "$rpc", "RPC call limit exceeded"))?;
        let request = FfiRequest::decode(&request.payload, &self.0.options.limits)
            .map_err(|error| RuntimeError::new("ffi_protocol", "$rpc", error.to_string()))?;
        if request.version != FFI_PROTOCOL || request.timeout_nanos < 0 {
            return Err(RuntimeError::new(
                "ffi_protocol",
                "$rpc",
                "invalid MRPC version or timeout",
            ));
        }
        let owner = self.0.clone();
        let canceled = cancellation.clone();
        self.0.options.runtime.spawn(async move {
            let context = CallContext { cancellation, deadline: (request.timeout_nanos > 0).then(|| Instant::now()+Duration::from_nanos(request.timeout_nanos as u64)), ..CallContext::default() };
            let reply = tokio::select! {
                _ = owner.closing.cancelled() => Err(Status::new("unavailable", "RPC session closed")),
                result = context.run(super::binding::invoke_user(owner.handle(context.clone(), request))) => result,
            };
            let reply = reply.unwrap_or_else(|error| ffi::Reply::new(FfiResponse { code: error.code, message: error.message, ..FfiResponse::default() }.encode(), None, None));
            completion.complete(reply); drop(call);
        });
        Ok(Box::new(HostCall(canceled)))
    }
    fn shutdown(&self, wait: ffi::Cancellation) -> std::result::Result<(), RuntimeError> {
        self.0.closing.cancel();
        {
            let context = CallContext {
                cancellation: wait,
                ..CallContext::default()
            };
            blocking_wait(context.run(self.0.wait_closed()))
                .map_err(|error| RuntimeError::new("ffi_shutdown", "$rpc", error.to_string()))
        }
    }

    fn shutdown_async(&self) -> ffi::Shutdown<'_> {
        self.0.closing.cancel();
        Box::pin(async {
            self.0
                .wait_closed()
                .await
                .map_err(|error| RuntimeError::new("ffi_shutdown", "$rpc", error.to_string()))
        })
    }
}

fn blocking_wait<F: std::future::Future>(future: F) -> F::Output {
    struct ThreadWake(std::thread::Thread);
    impl std::task::Wake for ThreadWake {
        fn wake(self: Arc<Self>) {
            self.0.unpark();
        }
        fn wake_by_ref(self: &Arc<Self>) {
            self.0.unpark();
        }
    }
    let waker = std::task::Waker::from(Arc::new(ThreadWake(std::thread::current())));
    let mut context = std::task::Context::from_waker(&waker);
    let mut future = std::pin::pin!(future);
    loop {
        match future.as_mut().poll(&mut context) {
            std::task::Poll::Ready(value) => return value,
            std::task::Poll::Pending => std::thread::park(),
        }
    }
}

/// Bounds VM work hosted by an asynchronous application. Running work owns its permit.
pub struct VmExecutor {
    runtime: Handle,
    slots: Arc<Semaphore>,
}
impl VmExecutor {
    pub fn new(runtime: Handle, capacity: usize) -> Result<Self> {
        if capacity == 0 {
            return Err(Status::new(
                "invalid_argument",
                "VM executor capacity must be positive",
            ));
        }
        Ok(Self {
            runtime,
            slots: Arc::new(Semaphore::new(capacity)),
        })
    }
    pub async fn run<T: Send + 'static>(
        &self,
        work: impl FnOnce() -> T + Send + 'static,
    ) -> Result<T> {
        let permit = self
            .slots
            .clone()
            .try_acquire_owned()
            .map_err(|_| Status::exhausted("VM executor is full"))?;
        self.runtime
            .spawn_blocking(move || {
                let _permit = permit;
                work()
            })
            .await
            .map_err(|error| Status::new("internal", error.to_string()))
    }
}
