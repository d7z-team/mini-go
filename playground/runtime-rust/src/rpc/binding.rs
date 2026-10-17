use super::platform::Handle;
use super::*;
use crate::ffi::Cancellation;
use std::collections::BTreeMap;
use std::sync::{
    Arc, Mutex, Weak,
    atomic::{AtomicBool, AtomicU64, Ordering},
};
use tokio::sync::{Notify, Semaphore, oneshot, watch};

pub(crate) async fn invoke_user<T>(
    future: impl std::future::Future<Output = Result<T>>,
) -> Result<T> {
    let mut future = std::pin::pin!(future);
    std::future::poll_fn(|context| {
        std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            future.as_mut().poll(context)
        }))
        .unwrap_or_else(|_| {
            std::task::Poll::Ready(Err(Status::new("internal", "RPC handler panicked")))
        })
    })
    .await
}

pub trait Provider: Send + Sync {
    fn contract(&self) -> Contract;
    fn provider_id(&self) -> String {
        self.contract()
            .methods
            .into_iter()
            .filter(|method| method.resource_type_hash.is_empty())
            .map(|method| method.service)
            .min()
            .unwrap_or_default()
    }
    fn bind(
        &self,
        context: CallContext,
        request: BindRequest,
    ) -> BoxFuture<'_, Result<Arc<dyn ProviderLease>>>;
}
pub trait ProviderLease: Send + Sync {
    fn invoke(
        &self,
        context: CallContext,
        method: Method,
        arguments: Vec<Value>,
    ) -> BoxFuture<'_, Result<ProviderResult>>;
    fn close(&self) -> BoxFuture<'_, Result<()>>;
}
pub trait Resource: Send + Sync + std::any::Any {
    fn invoke(
        &self,
        context: CallContext,
        method: String,
        arguments: Vec<Value>,
    ) -> BoxFuture<'_, Result<Vec<Value>>>;
    fn close(&self) -> BoxFuture<'_, Result<()>>;
}
pub trait Binder: Send + Sync {
    fn bind(
        &self,
        context: CallContext,
        request: BindRequest,
    ) -> BoxFuture<'_, Result<Arc<RouteSet>>>;
}

pub type MethodHandler =
    Arc<dyn Fn(CallContext, Vec<Value>) -> BoxFuture<'static, Result<Vec<Value>>> + Send + Sync>;
pub struct MethodBinding {
    pub method: Method,
    pub invoke: Option<MethodHandler>,
}

/// Immutable provider declarations; each binding owns its selected handler set.
pub struct StaticProvider {
    contract: Contract,
    handlers: BTreeMap<Method, MethodHandler>,
}
impl StaticProvider {
    pub fn new(bindings: Vec<MethodBinding>) -> Result<Self> {
        let mut methods = Vec::new();
        let mut handlers = BTreeMap::new();
        for binding in bindings {
            if binding.invoke.is_none() && binding.method.resource_type_hash.is_empty() {
                return Err(Status::new("invalid_argument", "RPC method has no handler"));
            }
            methods.push(binding.method.clone());
            if let Some(handler) = binding.invoke {
                handlers.insert(binding.method, handler);
            }
        }
        Ok(Self {
            contract: Contract::new(methods).normalized(&Limits::default())?,
            handlers,
        })
    }
}
impl Provider for StaticProvider {
    fn contract(&self) -> Contract {
        self.contract.clone()
    }
    fn bind(
        &self,
        context: CallContext,
        request: BindRequest,
    ) -> BoxFuture<'_, Result<Arc<dyn ProviderLease>>> {
        Box::pin(async move {
            context.check()?;
            self.contract.check_support(&request.contract)?;
            let handlers = self
                .handlers
                .iter()
                .filter(|(method, _)| request.contract.methods.contains(method))
                .map(|(m, h)| (m.clone(), h.clone()))
                .collect();
            Ok(Arc::new(StaticLease {
                handlers,
                closed: AtomicBool::new(false),
            }) as Arc<dyn ProviderLease>)
        })
    }
}
struct StaticLease {
    handlers: BTreeMap<Method, MethodHandler>,
    closed: AtomicBool,
}
impl ProviderLease for StaticLease {
    fn invoke(
        &self,
        context: CallContext,
        method: Method,
        arguments: Vec<Value>,
    ) -> BoxFuture<'_, Result<ProviderResult>> {
        Box::pin(async move {
            if self.closed.load(Ordering::Acquire) {
                return Err(Status::new("unavailable", "RPC provider lease closed"));
            }
            let handler = self
                .handlers
                .get(&method)
                .ok_or_else(|| Status::new("unimplemented", "RPC handler unavailable"))?;
            Ok(ProviderResult::new(handler(context, arguments).await?))
        })
    }
    fn close(&self) -> BoxFuture<'_, Result<()>> {
        Box::pin(async {
            self.closed.store(true, Ordering::Release);
            Ok(())
        })
    }
}

/// Proxy providers retain a downstream decision until their caller decides.
pub struct ProviderResult {
    pub values: Vec<Value>,
    pub decision: Option<PendingResult>,
}
impl ProviderResult {
    pub fn new(values: Vec<Value>) -> Self {
        Self {
            values,
            decision: None,
        }
    }
    pub fn proxy(result: PendingResult) -> Self {
        Self {
            values: result.values.clone(),
            decision: Some(result),
        }
    }
}

#[derive(Clone, Debug)]
pub struct Call {
    pub method: Method,
    pub receiver: Option<ResourceRef>,
    pub arguments: Vec<Value>,
}

type Decision = (bool, oneshot::Sender<Result<AcceptedResult>>);

/// A provisional reply; dropping it requests discard from its existing owner.
pub struct PendingResult {
    pub values: Vec<Value>,
    pub(crate) decision: Option<oneshot::Sender<Decision>>,
}
impl PendingResult {
    pub async fn accept(mut self) -> Result<AcceptedResult> {
        let (sender, receiver) = oneshot::channel();
        self.decision
            .take()
            .ok_or_else(|| Status::protocol("RPC result already decided"))?
            .send((true, sender))
            .map_err(|_| Status::new("unavailable", "RPC result owner closed"))?;
        receiver
            .await
            .map_err(|_| Status::new("unavailable", "RPC result owner closed"))?
    }
    pub async fn discard(mut self) -> Result<()> {
        let (sender, receiver) = oneshot::channel();
        if let Some(decision) = self.decision.take() {
            if decision.send((false, sender)).is_err() {
                return Ok(());
            }
            receiver
                .await
                .map_err(|_| Status::new("unavailable", "RPC result owner closed"))??;
        }
        Ok(())
    }
}

impl Drop for PendingResult {
    fn drop(&mut self) {
        let Some(decision) = self.decision.take() else {
            return;
        };
        let (sender, _receiver) = oneshot::channel();
        let _ = decision.send((false, sender));
    }
}

/// Accepted resources remain protected until their final consumer commits delivery.
pub struct AcceptedResult {
    pub values: Vec<Value>,
    pub(crate) release: Option<Box<dyn FnOnce() + Send>>,
    pub(crate) forwarded: Vec<AcceptedResult>,
}
impl AcceptedResult {
    pub fn consume(mut self) -> Vec<Value> {
        self.release = None;
        for forwarded in std::mem::take(&mut self.forwarded) {
            forwarded.consume();
        }
        std::mem::take(&mut self.values)
    }
}
impl Drop for AcceptedResult {
    fn drop(&mut self) {
        if let Some(release) = self.release.take() {
            release();
        }
    }
}

pub(crate) trait Binding: Send + Sync {
    fn resource_count(&self) -> usize;
    fn drain(&self);
    fn invoke(&self, context: CallContext, call: Call) -> BoxFuture<'_, Result<PendingResult>>;
    fn validate_resource(&self, reference: &ResourceRef) -> Result<()>;
    fn drop_resource(
        &self,
        context: CallContext,
        reference: ResourceRef,
    ) -> BoxFuture<'_, Result<()>>;
    fn begin_shutdown(&self);
    fn shutdown(&self) -> BoxFuture<'_, Result<()>>;
}

pub struct RouteSet {
    pub contract: Contract,
    pub epoch: u64,
    pub(crate) binding: Arc<dyn Binding>,
    handles: Mutex<BTreeMap<u64, Weak<ResourceHandle>>>,
}
impl RouteSet {
    pub fn resource_count(&self) -> usize {
        self.binding.resource_count()
    }
    /// Stops new service calls; existing resources keep their binding until released.
    pub fn close(&self) {
        self.binding.drain();
    }
    pub(crate) fn new(contract: Contract, epoch: u64, binding: Arc<dyn Binding>) -> Arc<Self> {
        Arc::new(Self {
            contract,
            epoch,
            binding,
            handles: Mutex::new(BTreeMap::new()),
        })
    }
    pub async fn invoke(&self, context: CallContext, call: Call) -> Result<PendingResult> {
        context.check()?;
        if !self.contract.methods.contains(&call.method) {
            return Err(Status::new("unimplemented", "RPC method is not bound"));
        }
        self.binding.invoke(context, call).await
    }
    pub fn bind_resource(self: &Arc<Self>, reference: ResourceRef) -> Result<Arc<ResourceHandle>> {
        self.binding.validate_resource(&reference)?;
        let mut handles = self.handles.lock().unwrap();
        handles.retain(|_, value| value.strong_count() != 0);
        if let Some(handle) = handles.get(&reference.object_id).and_then(Weak::upgrade) {
            return Ok(handle);
        }
        let handle = Arc::new(ResourceHandle {
            routes: self.clone(),
            reference,
            closed: AtomicBool::new(false),
        });
        handles.insert(handle.reference.object_id, Arc::downgrade(&handle));
        Ok(handle)
    }
    pub async fn drop_resource(&self, context: CallContext, reference: ResourceRef) -> Result<()> {
        self.binding.drop_resource(context, reference).await
    }
    pub async fn shutdown(&self) -> Result<()> {
        self.binding.begin_shutdown();
        self.binding.shutdown().await
    }
}
impl Drop for RouteSet {
    fn drop(&mut self) {
        self.binding.begin_shutdown();
    }
}

pub struct ResourceHandle {
    routes: Arc<RouteSet>,
    reference: ResourceRef,
    closed: AtomicBool,
}
impl ResourceHandle {
    pub fn routes(&self) -> &Arc<RouteSet> {
        &self.routes
    }
    pub fn reference(&self, owner: &Arc<RouteSet>) -> Result<ResourceRef> {
        if !Arc::ptr_eq(owner, &self.routes) || self.closed.load(Ordering::Acquire) {
            return Err(Status::new(
                "invalid_argument",
                "RPC resource belongs to another binding or is closed",
            ));
        }
        self.routes.binding.validate_resource(&self.reference)?;
        Ok(self.reference.clone())
    }
    pub async fn close(&self, context: CallContext) -> Result<()> {
        if self.closed.load(Ordering::Acquire) {
            return Ok(());
        }
        let result = self
            .routes
            .drop_resource(context, self.reference.clone())
            .await;
        if self.closed.load(Ordering::Acquire) {
            return Ok(());
        }
        result?;
        self.closed.store(true, Ordering::Release);
        Ok(())
    }
}

struct ResourceEntry {
    reference: ResourceRef,
    resource: Arc<dyn Resource>,
    active: AtomicBool,
    closing: Cancellation,
    calls: Arc<Semaphore>,
    capacity: u32,
    attempt: Mutex<watch::Sender<Option<Result<()>>>>,
    retry: Notify,
    finished: AtomicBool,
}

impl ResourceEntry {
    fn close_attempt(&self) -> watch::Receiver<Option<Result<()>>> {
        let mut attempt = self.attempt.lock().unwrap();
        if !self.finished.load(Ordering::Acquire) && matches!(&*attempt.borrow(), Some(Err(_))) {
            *attempt = watch::channel(None).0;
            self.retry.notify_one();
        }
        self.closing.cancel();
        attempt.subscribe()
    }
}

pub(crate) struct CallResources {
    owner: Weak<LocalState>,
    borrowed: Mutex<BTreeMap<u64, Arc<ResourceEntry>>>,
    entries: Mutex<Option<Vec<Arc<ResourceEntry>>>>,
}
impl CallContext {
    pub fn export(
        &self,
        resource: Arc<dyn Resource>,
        type_hash: impl Into<String>,
    ) -> Result<Value> {
        let exports = self
            .resources
            .as_ref()
            .ok_or_else(|| Status::new("invalid_argument", "RPC call cannot export resources"))?;
        let owner = exports
            .owner
            .upgrade()
            .ok_or_else(|| Status::new("unavailable", "RPC binding closed"))?;
        let mut entries = exports.entries.lock().unwrap();
        let entries = entries
            .as_mut()
            .ok_or_else(|| Status::new("unavailable", "RPC call finished"))?;
        let mut registry = owner.resources.lock().unwrap();
        if owner.closing.is_cancelled() {
            return Err(Status::new("unavailable", "RPC binding closed"));
        }
        let permit = owner
            .resource_slots
            .clone()
            .try_acquire_owned()
            .map_err(|_| Status::exhausted("RPC resource limit exceeded"))?;
        let id = owner
            .next_resource
            .try_update(Ordering::AcqRel, Ordering::Acquire, |id| id.checked_add(1))
            .map_err(|_| Status::exhausted("RPC resource ID space exhausted"))?;
        let reference = ResourceRef {
            epoch: owner.epoch,
            object_id: id,
            type_hash: type_hash.into(),
        };
        reference.validate()?;
        let (done, _) = watch::channel(None);
        let entry = Arc::new(ResourceEntry {
            reference: reference.clone(),
            resource,
            active: AtomicBool::new(false),
            closing: Cancellation::default(),
            calls: Arc::new(Semaphore::new(owner.limits.max_pending_calls)),
            capacity: owner.limits.max_pending_calls as u32,
            attempt: Mutex::new(done),
            retry: Notify::new(),
            finished: AtomicBool::new(false),
        });
        registry.insert(id, entry.clone());
        entries.push(entry.clone());
        let weak = Arc::downgrade(&owner);
        let shutdown = owner.closing.clone();
        owner.runtime.spawn(async move {
            tokio::select! { _ = shutdown.cancelled() => {}, _ = entry.closing.cancelled() => {} }
            entry.closing.cancel();
            let _calls = entry.calls.acquire_many(entry.capacity).await;
            loop {
                let done = entry.attempt.lock().unwrap().clone();
                let final_attempt = shutdown.is_cancelled();
                let result = invoke_user(async { entry.resource.close().await }).await;
                let terminal = result.is_ok() || final_attempt;
                if terminal {
                    entry.finished.store(true, Ordering::Release);
                }
                if terminal && let Some(owner) = weak.upgrade() {
                    if let Err(error) = &result {
                        *owner.cleanup_error.lock().unwrap() = Some(error.clone());
                    }
                    owner.resources.lock().unwrap().remove(&id);
                    owner.finalize_if_idle();
                }
                done.send_replace(Some(result));
                if terminal {
                    break;
                }
                tokio::select! { _ = shutdown.cancelled() => {}, _ = entry.retry.notified() => {} }
                let mut attempt = entry.attempt.lock().unwrap();
                if matches!(&*attempt.borrow(), Some(Err(_))) {
                    *attempt = watch::channel(None).0;
                }
            }
            drop(permit);
        });
        Ok(Value::resource(reference))
    }
    pub fn resolve(&self, reference: &ResourceRef) -> Result<Arc<dyn Resource>> {
        reference.validate()?;
        if let Some(resources) = &self.resources
            && let Some(entry) = resources.borrowed.lock().unwrap().get(&reference.object_id)
            && entry.reference == *reference
        {
            return Ok(entry.resource.clone());
        }
        let owner = self
            .resources
            .as_ref()
            .and_then(|resources| resources.owner.upgrade())
            .ok_or_else(|| Status::new("unavailable", "RPC binding closed"))?;
        Ok(owner.resource(reference, true)?.resource.clone())
    }
}

struct LocalState {
    runtime: Handle,
    limits: Limits,
    epoch: u64,
    leases: BTreeMap<String, Arc<dyn ProviderLease>>,
    resources: Mutex<BTreeMap<u64, Arc<ResourceEntry>>>,
    next_resource: AtomicU64,
    resource_slots: Arc<Semaphore>,
    call_slots: Arc<Semaphore>,
    result_slots: Arc<Semaphore>,
    closing: Cancellation,
    draining: AtomicBool,
    cleanup_error: Mutex<Option<Status>>,
    done: watch::Receiver<Option<Result<()>>>,
    peer: PeerInfo,
    provider: String,
}
impl LocalState {
    fn finalize_if_idle(&self) {
        if self.draining.load(Ordering::Acquire)
            && self.call_slots.available_permits() == self.limits.max_pending_calls
            && self.resources.lock().unwrap().is_empty()
        {
            self.closing.cancel();
        }
    }
    fn resource(&self, reference: &ResourceRef, active: bool) -> Result<Arc<ResourceEntry>> {
        reference.validate()?;
        if reference.epoch != self.epoch {
            return Err(Status::new(
                "invalid_argument",
                "RPC resource belongs to another binding",
            ));
        }
        let entry = self
            .resources
            .lock()
            .unwrap()
            .get(&reference.object_id)
            .cloned()
            .ok_or_else(|| Status::new("not_found", "RPC resource is closed"))?;
        if entry.reference != *reference
            || entry.closing.is_cancelled()
            || active && !entry.active.load(Ordering::Acquire)
        {
            return Err(Status::new(
                "invalid_argument",
                "RPC resource is closed or provisional",
            ));
        }
        Ok(entry)
    }
}

struct LocalBinding(Arc<LocalState>);
impl Binding for LocalBinding {
    fn resource_count(&self) -> usize {
        self.0.resources.lock().unwrap().len()
    }
    fn drain(&self) {
        self.0.draining.store(true, Ordering::Release);
        self.0.finalize_if_idle();
    }
    fn invoke(&self, mut context: CallContext, call: Call) -> BoxFuture<'_, Result<PendingResult>> {
        Box::pin(async move {
            let state = &self.0;
            if state.closing.is_cancelled()
                || call.receiver.is_none() && state.draining.load(Ordering::Acquire)
            {
                return Err(Status::new("unavailable", "RPC binding closed"));
            }
            validate_values(&call.arguments, &state.limits)?;
            let mut borrowed = BTreeMap::new();
            for value in &call.arguments {
                if let Data::Resource(reference) = &value.data {
                    borrowed.insert(reference.object_id, state.resource(reference, true)?);
                }
            }
            let resource = call
                .receiver
                .as_ref()
                .map(|reference| state.resource(reference, true))
                .transpose()?;
            if let Some(resource) = &resource {
                borrowed.insert(resource.reference.object_id, resource.clone());
            }
            if let Some(resource) = &resource {
                if call.method.resource_type_hash != resource.reference.type_hash {
                    return Err(Status::new(
                        "invalid_argument",
                        "RPC receiver type mismatch",
                    ));
                }
            } else if !call.method.resource_type_hash.is_empty() {
                return Err(Status::new("invalid_argument", "RPC receiver required"));
            }
            let lease = if resource.is_none() {
                Some(
                    state
                        .leases
                        .get(&call.method.service)
                        .cloned()
                        .ok_or_else(|| Status::new("unimplemented", "RPC service not bound"))?,
                )
            } else {
                None
            };
            let permit = state
                .call_slots
                .clone()
                .try_acquire_owned()
                .map_err(|_| Status::exhausted("RPC pending call limit exceeded"))?;
            let result_permit = state
                .result_slots
                .clone()
                .try_acquire_owned()
                .map_err(|_| Status::exhausted("RPC pending result limit exceeded"))?;
            let exports = Arc::new(CallResources {
                owner: Arc::downgrade(state),
                borrowed: Mutex::new(borrowed.clone()),
                entries: Mutex::new(Some(Vec::new())),
            });
            context.resources = Some(exports.clone());
            context.peer = state.peer.clone();
            context.provider = state.provider.clone();
            let resource_permits = tokio::select! {
                biased;
                _ = state.closing.cancelled() => {
                    return Err(Status::new("unavailable", "RPC binding closed"));
                }
                result = context.run(async {
                    let mut permits = Vec::with_capacity(borrowed.len());
                    for resource in borrowed.values() {
                        let permit = resource
                            .calls
                            .clone()
                            .acquire_owned()
                            .await
                            .map_err(|_| Status::new("internal", "RPC resource owner failed"))?;
                        if resource.closing.is_cancelled() {
                            return Err(Status::new("unavailable", "RPC resource closing"));
                        }
                        permits.push(permit);
                    }
                    Ok(permits)
                }) => result?,
            };
            let (mut send, receive) = oneshot::channel();
            let state = state.clone();
            let wait_context = context.clone();
            self.0.runtime.spawn(async move {
                let invocation = async {
                    if let Some(resource) = resource { Ok(ProviderResult::new(resource.resource.invoke(context.clone(), call.method.name, call.arguments).await?)) }
                    else { lease.unwrap().invoke(context.clone(), call.method, call.arguments).await }
                };
                let response = tokio::select! {
                    _ = send.closed() => Err(Status::new("canceled", "RPC caller dropped")),
                    _ = state.closing.cancelled() => Err(Status::new("unavailable", "RPC binding closed")),
                    response = context.run(invoke_user(invocation)) => response,
                };
                let entries = exports.entries.lock().unwrap().take().unwrap_or_default();
                exports.borrowed.lock().unwrap().clear();
                drop(resource_permits);
                let response = response.and_then(|response| { validate_values(&response.values, &state.limits)?; Ok(response) });
                match response {
                    Err(error) => { let _ = send.send(Err(error)); for entry in entries { entry.closing.cancel(); } }
                    Ok(mut response) => {
                        let (decision, decisions) = oneshot::channel();
                        let values = response.values.clone();
                        let _ = send.send(Ok(PendingResult { values, decision: Some(decision) }));
                        let decision = tokio::select! { _ = state.closing.cancelled() => None, decision = decisions => decision.ok() };
                        if let Some((true, ack)) = decision {
                            let accepted = if let Some(proxy) = response.decision.take() { proxy.accept().await.map(Some) } else { Ok(None) };
                            match accepted {
                                Ok(proxy) => {
                                    for entry in &entries { entry.active.store(true, Ordering::Release); }
                                    let _ = ack.send(Ok(AcceptedResult { values: response.values, forwarded: proxy.into_iter().collect(), release: Some(Box::new(move || {
                                        for entry in entries { entry.closing.cancel(); }
                                    })) }));
                                }
                                Err(error) => { for entry in entries { entry.closing.cancel(); } let _ = ack.send(Err(error)); }
                            }
                        } else {
                            for entry in &entries { entry.closing.cancel(); }
                            let mut result = Ok(());
                            if let Some(proxy) = response.decision.take() { result = proxy.discard().await; }
                            for entry in entries {
                                let mut done = entry.attempt.lock().unwrap().subscribe();
                                let closed = done.wait_for(|v| v.is_some()).await;
                                if let Ok(closed) = closed && let Some(Err(error)) = &*closed { result = Err(error.clone()); }
                            }
                            if let Some((false, ack)) = decision { let _ = ack.send(result.map(|()| AcceptedResult { values: Vec::new(), release: None, forwarded: Vec::new() })); }
                        }
                    }
                }
                drop(permit);
                drop(result_permit);
                state.finalize_if_idle();
            });
            wait_context
                .run(async {
                    receive
                        .await
                        .map_err(|_| Status::new("internal", "RPC provider task failed"))?
                })
                .await
        })
    }
    fn validate_resource(&self, reference: &ResourceRef) -> Result<()> {
        self.0.resource(reference, false).map(|_| ())
    }
    fn drop_resource(
        &self,
        context: CallContext,
        reference: ResourceRef,
    ) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            if reference.epoch != self.0.epoch {
                return Err(Status::new(
                    "invalid_argument",
                    "RPC resource belongs to another binding",
                ));
            }
            let entry = self
                .0
                .resources
                .lock()
                .unwrap()
                .get(&reference.object_id)
                .cloned();
            if let Some(entry) = entry {
                if entry.reference != reference {
                    return Err(Status::new(
                        "invalid_argument",
                        "RPC resource type mismatch",
                    ));
                }
                if context.resources.as_ref().is_some_and(|resources| {
                    resources
                        .owner
                        .upgrade()
                        .is_some_and(|owner| Arc::ptr_eq(&owner, &self.0))
                        && resources
                            .borrowed
                            .lock()
                            .unwrap()
                            .contains_key(&reference.object_id)
                }) {
                    return Err(Status::new(
                        "failed_precondition",
                        "RPC resource cannot close while borrowed by the current call",
                    ));
                }
                let mut done = entry.close_attempt();
                context
                    .run(async {
                        let result = done
                            .wait_for(|v| v.is_some())
                            .await
                            .map_err(|_| Status::new("internal", "RPC resource owner failed"))?;
                        result.as_ref().unwrap().clone()
                    })
                    .await?;
            } else if self.0.closing.is_cancelled() {
                let mut done = self.0.done.clone();
                return context
                    .run(async {
                        let result = done
                            .wait_for(|value| value.is_some())
                            .await
                            .map_err(|_| Status::new("internal", "RPC binding owner failed"))?;
                        result.as_ref().unwrap().clone()
                    })
                    .await;
            }
            Ok(())
        })
    }
    fn begin_shutdown(&self) {
        self.0.closing.cancel();
    }
    fn shutdown(&self) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            let mut done = self.0.done.clone();
            let result = done
                .wait_for(|v| v.is_some())
                .await
                .map_err(|_| Status::new("internal", "RPC binding owner failed"))?;
            result.as_ref().unwrap().clone()
        })
    }
}

static NEXT_EPOCH: AtomicU64 = AtomicU64::new(1);

pub struct LocalBinder {
    runtime: Handle,
    limits: Limits,
    services: BTreeMap<String, Arc<dyn Provider>>,
}
impl LocalBinder {
    pub fn new(runtime: Handle, limits: Limits, providers: Vec<Arc<dyn Provider>>) -> Result<Self> {
        if limits.max_pending_calls == 0
            || limits.max_pending_calls > u32::MAX as usize
            || limits.max_resources > u32::MAX as usize
        {
            return Err(Status::new("invalid_argument", "invalid RPC limits"));
        }
        let mut services = BTreeMap::new();
        for provider in providers {
            let contract = provider.contract().normalized(&limits)?;
            let names: std::collections::BTreeSet<_> = contract
                .methods
                .iter()
                .filter(|m| m.resource_type_hash.is_empty())
                .map(|m| m.service.clone())
                .collect();
            if names.is_empty() {
                return Err(Status::new(
                    "invalid_argument",
                    "RPC provider has no service methods",
                ));
            }
            for name in names {
                if services.insert(name, provider.clone()).is_some() {
                    return Err(Status::new(
                        "already_exists",
                        "RPC service has multiple providers",
                    ));
                }
            }
        }
        Ok(Self {
            runtime,
            limits,
            services,
        })
    }
}
impl Binder for LocalBinder {
    fn bind(
        &self,
        context: CallContext,
        request: BindRequest,
    ) -> BoxFuture<'_, Result<Arc<RouteSet>>> {
        Box::pin(async move {
            context.check()?;
            let contract = request.contract.normalized(&self.limits)?;
            let mut groups: BTreeMap<String, Vec<Method>> = BTreeMap::new();
            let mut available = Vec::new();
            for method in &contract.methods {
                if method.resource_type_hash.is_empty() {
                    groups
                        .entry(method.service.clone())
                        .or_default()
                        .push(method.clone());
                }
            }
            for (service, methods) in &groups {
                let provider = self.services.get(service).ok_or_else(|| {
                    Status::new(
                        "unimplemented",
                        format!("RPC service is not implemented: {service}"),
                    )
                })?;
                let declared = provider.contract();
                declared.check_support(&Contract::new(methods.clone()))?;
                available.extend(declared.methods);
            }
            Contract::new(available).check_support(&contract)?;
            let runtime = self.runtime.clone();
            let limits = self.limits.clone();
            let services = self.services.clone();
            let wait = context.clone();
            let (mut sender, receiver) = oneshot::channel();
            self.runtime.spawn(async move {
                let mut leases = BTreeMap::new();
                let mut failure = None;
                for (service, methods) in groups {
                    let mut selected = request.clone();
                    selected.contract = Contract::new(methods);
                    let bound = tokio::select! {
                        _ = sender.closed() => Err(Status::new("canceled", "RPC bind caller dropped")),
                        result = context
                        .run(invoke_user(async {
                            services[&service].bind(context.clone(), selected).await
                        })) => result,
                    };
                    match bound {
                        Ok(lease) => {
                            leases.insert(service, lease);
                        }
                        Err(error) => {
                            failure = Some(error);
                            break;
                        }
                    }
                }
                if let Some(error) = failure {
                    for lease in leases.values() {
                        let _ = lease.close().await;
                    }
                    let _ = sender.send(Err(error));
                    return;
                }
                let epoch =
                    match NEXT_EPOCH
                        .try_update(Ordering::AcqRel, Ordering::Acquire, |v| v.checked_add(1))
                    {
                        Ok(epoch) => epoch,
                        Err(_) => {
                            for lease in leases.values() {
                                let _ = lease.close().await;
                            }
                            let _ =
                                sender.send(Err(Status::exhausted("RPC epoch space exhausted")));
                            return;
                        }
                    };
                let (done, receiver) = watch::channel(None);
                let state = Arc::new(LocalState {
                    runtime: runtime.clone(),
                    resource_slots: Arc::new(Semaphore::new(limits.max_resources)),
                    call_slots: Arc::new(Semaphore::new(limits.max_pending_calls)),
                    result_slots: Arc::new(Semaphore::new(limits.max_pending_results)),
                    limits,
                    epoch,
                    leases,
                    resources: Mutex::new(BTreeMap::new()),
                    next_resource: AtomicU64::new(1),
                    closing: Cancellation::default(),
                    draining: AtomicBool::new(false),
                    cleanup_error: Mutex::new(None),
                    done: receiver,
                    peer: request.peer,
                    provider: request.provider,
                });
                let routes = RouteSet::new(contract, epoch, Arc::new(LocalBinding(state.clone())));
                runtime.spawn(async move {
                    state.closing.cancelled().await;
                    let _calls = state
                        .call_slots
                        .acquire_many(state.limits.max_pending_calls as u32)
                        .await;
                    let _resources = state
                        .resource_slots
                        .acquire_many(state.limits.max_resources as u32)
                        .await;
                    let mut result = state.cleanup_error.lock().unwrap().clone().map_or(Ok(()), Err);
                    for lease in state.leases.values() {
                        if let Err(error) = invoke_user(async { lease.close().await }).await {
                            result = Err(error);
                        }
                    }
                    done.send_replace(Some(result));
                });
                let _ = sender.send(Ok(routes));
            });
            wait.run(async {
                receiver
                    .await
                    .map_err(|_| Status::new("internal", "RPC bind owner failed"))?
            })
            .await
        })
    }
}
