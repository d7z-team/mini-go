use super::platform::Handle;
use super::protocol::FfiResponse;
use super::*;
use crate::ffi::Cancellation;
use std::collections::{BTreeMap, VecDeque};
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicU64, Ordering},
};
use tokio::sync::{Notify, Semaphore, oneshot};

struct Pending {
    result: Option<oneshot::Sender<()>>,
    reply: Arc<Mutex<GuestReply>>,
    accepted: bool,
    canceled: bool,
}
#[derive(Default)]
struct GuestReply {
    outcome: Option<Result<Vec<Value>>>,
    resources: Vec<ResourceRef>,
}
struct State {
    deliveries: usize,
    pending: BTreeMap<u64, Pending>,
    events: VecDeque<FfiResponse>,
    canceled: VecDeque<u64>,
}
pub(crate) struct GuestEvent {
    pub(crate) response: FfiResponse,
    provider: Arc<GuestProvider>,
    consumed: bool,
}
impl GuestEvent {
    pub(crate) fn consume(mut self) {
        self.consumed = true;
    }
}
impl Drop for GuestEvent {
    fn drop(&mut self) {
        let mut state = self.provider.state.lock().unwrap();
        state.deliveries -= 1;
        if !self.consumed && !self.provider.closing.is_cancelled() {
            state.events.push_front(std::mem::take(&mut self.response));
        }
        self.provider.changed.notify_waiters();
    }
}
pub(crate) struct GuestProvider {
    runtime: Handle,
    calls: Arc<Semaphore>,
    contract: Contract,
    limits: Limits,
    next_id: AtomicU64,
    state: Mutex<State>,
    changed: Notify,
    pub(crate) closing: Cancellation,
}
impl GuestProvider {
    pub(crate) fn new(runtime: Handle, contract: Contract, limits: Limits) -> Result<Arc<Self>> {
        Ok(Arc::new(Self {
            runtime,
            calls: Arc::new(Semaphore::new(limits.max_pending_calls)),
            contract: contract.normalized(&limits)?,
            limits,
            next_id: AtomicU64::new(1),
            state: Mutex::new(State {
                deliveries: 0,
                pending: BTreeMap::new(),
                events: VecDeque::new(),
                canceled: VecDeque::new(),
            }),
            changed: Notify::new(),
            closing: Cancellation::default(),
        }))
    }
    pub(crate) async fn accept(self: &Arc<Self>, context: CallContext) -> Result<GuestEvent> {
        context.run(async {
            loop {
                let notified = self.changed.notified();
                {
                    let mut state = self.state.lock().unwrap();
                    if self.closing.is_cancelled() { return Err(Status::new("unavailable", "RPC guest provider closed")); }
                    while let Some(id) = state.canceled.pop_front() {
                        if state.pending.remove(&id).is_some() {
                            state.events.push_front(FfiResponse { operation: "cancel".into(), request_id: id, ..FfiResponse::default() });
                            break;
                        }
                    }
                    if let Some(mut event) = state.events.pop_front() {
                        if event.operation == "call" {
                            let Some(pending) = state.pending.get_mut(&event.request_id) else { continue; };
                            if pending.canceled { state.pending.remove(&event.request_id); event.operation = "cancel".into(); event.payload.clear(); }
                            else { pending.accepted = true; }
                        }
                        self.changed.notify_waiters();
                        state.deliveries += 1;
                        return Ok(GuestEvent { response: event, provider: self.clone(), consumed: false });
                    }
                }
                tokio::select! { _ = self.closing.cancelled() => return Err(Status::new("unavailable", "RPC guest provider closed")), _ = notified => {} }
            }
        }).await
    }
    pub(crate) fn respond(
        &self,
        id: u64,
        payload: Vec<u8>,
        code: String,
        message: String,
    ) -> Result<()> {
        {
            let mut state = self.state.lock().unwrap();
            let pending = state
                .pending
                .get_mut(&id)
                .ok_or_else(|| Status::new("not_found", "RPC guest request closed"))?;
            if pending.canceled {
                return Err(Status::new("canceled", "RPC guest request canceled"));
            }
            if !pending.accepted {
                return Err(Status::new(
                    "failed_precondition",
                    "RPC guest request not accepted",
                ));
            }
            let sender = pending
                .result
                .take()
                .ok_or_else(|| Status::new("not_found", "RPC guest request already answered"))?;
            let result = if code.is_empty() {
                decode_values(&payload, &self.limits)
            } else {
                Err(Status::new(code, message))
            };
            let mut reply = pending.reply.lock().unwrap();
            if let Ok(values) = &result {
                reply.resources = values
                    .iter()
                    .filter_map(|value| match &value.data {
                        Data::Resource(reference) => Some(reference.clone()),
                        _ => None,
                    })
                    .collect();
            }
            reply.outcome = Some(result);
            let _ = sender.send(());
        }
        Ok(())
    }
    async fn release(&self, reference: ResourceRef) -> Result<()> {
        loop {
            let notified = self.changed.notified();
            {
                let mut state = self.state.lock().unwrap();
                if self.closing.is_cancelled() {
                    return Ok(());
                }
                if state.events.len() + state.deliveries
                    < self.limits.max_pending_calls.min(64) + self.limits.max_resources
                {
                    state.events.push_back(FfiResponse {
                        operation: "release".into(),
                        receiver: Some(reference),
                        ..FfiResponse::default()
                    });
                    self.changed.notify_waiters();
                    return Ok(());
                }
            }
            tokio::select! { _ = self.closing.cancelled() => return Ok(()), _ = notified => {} }
        }
    }
    async fn invoke(
        self: &Arc<Self>,
        context: CallContext,
        method: Method,
        receiver: Option<ResourceRef>,
        mut arguments: Vec<Value>,
    ) -> Result<ProviderResult> {
        for value in &mut arguments {
            if let Data::Resource(reference) = &value.data {
                let resource: Arc<dyn std::any::Any + Send + Sync> = context.resolve(reference)?;
                let resource = resource.downcast::<GuestResource>().map_err(|_| {
                    Status::new(
                        "invalid_argument",
                        "RPC resource belongs to another provider",
                    )
                })?;
                if !Arc::ptr_eq(&resource.provider, self) {
                    return Err(Status::new(
                        "invalid_argument",
                        "RPC resource belongs to another guest provider",
                    ));
                }
                *value = Value::resource(resource.reference.clone());
            }
        }
        let payload = encode_values(&arguments, &self.limits)?;
        let permit = self
            .calls
            .clone()
            .try_acquire_owned()
            .map_err(|_| Status::exhausted("RPC guest request limit exceeded"))?;
        let id = self
            .next_id
            .try_update(Ordering::AcqRel, Ordering::Acquire, |v| v.checked_add(1))
            .map_err(|_| Status::exhausted("RPC guest request IDs exhausted"))?;
        let (sender, reply) = oneshot::channel();
        let response = Arc::new(Mutex::new(GuestReply::default()));
        {
            let mut state = self.state.lock().unwrap();
            if self.closing.is_cancelled() {
                return Err(Status::new("unavailable", "RPC guest provider closed"));
            }
            if state.pending.len() >= self.limits.max_pending_calls
                || state.events.len() + state.deliveries
                    >= self.limits.max_pending_calls.min(64) + self.limits.max_resources
            {
                return Err(Status::exhausted("RPC guest request limit exceeded"));
            }
            state.pending.insert(
                id,
                Pending {
                    result: Some(sender),
                    reply: response.clone(),
                    accepted: false,
                    canceled: false,
                },
            );
            state.events.push_back(FfiResponse {
                operation: "call".into(),
                request_id: id,
                method,
                receiver,
                payload,
                ..FfiResponse::default()
            });
        }
        self.changed.notify_waiters();
        let (cleanup, cleaned) = oneshot::channel::<()>();
        let provider = self.clone();
        let owned_response = response.clone();
        self.runtime.spawn(async move {
            tokio::select! { _ = cleaned => {}, _ = provider.closing.cancelled() => {} }
            {
                let mut state = provider.state.lock().unwrap();
                if let Some(pending) = state.pending.get_mut(&id) {
                    if pending.result.is_none() {
                        state.pending.remove(&id);
                    } else {
                        pending.canceled = true;
                        if pending.accepted {
                            state.canceled.push_back(id);
                        }
                    }
                }
            }
            provider.changed.notify_waiters();
            let references = std::mem::take(&mut owned_response.lock().unwrap().resources);
            for reference in references {
                let _ = provider.release(reference).await;
            }
            drop(permit);
        });
        // Dropping this sender transfers cleanup to the already reserved call owner.
        let _cleanup = cleanup;
        context.run(async {tokio::select!{ _=self.closing.cancelled()=>Err(Status::new("unavailable","RPC guest provider closed")),outcome=reply=>outcome.map_err(|_|Status::new("unavailable","RPC guest call closed")) }}).await?;
        let mut values = response
            .lock()
            .unwrap()
            .outcome
            .take()
            .ok_or_else(|| Status::new("internal", "RPC guest response missing"))??;
        for value in &mut values {
            if let Data::Resource(reference) = &value.data {
                let transferred = reference.clone();
                *value = context.export(
                    Arc::new(GuestResource {
                        provider: self.clone(),
                        reference: reference.clone(),
                    }),
                    reference.type_hash.clone(),
                )?;
                let mut response = response.lock().unwrap();
                if let Some(index) = response
                    .resources
                    .iter()
                    .position(|reference| reference == &transferred)
                {
                    response.resources.remove(index);
                }
            }
        }
        Ok(ProviderResult::new(values))
    }
    pub(crate) fn close(&self) {
        self.closing.cancel();
        let mut state = self.state.lock().unwrap();
        state.pending.clear();
        state.events.clear();
        state.canceled.clear();
        self.changed.notify_waiters();
    }
}
pub(crate) struct GuestDeclaration(pub Arc<GuestProvider>);
impl Provider for GuestDeclaration {
    fn contract(&self) -> Contract {
        self.0.contract.clone()
    }
    fn bind(
        &self,
        context: CallContext,
        request: BindRequest,
    ) -> BoxFuture<'_, Result<Arc<dyn ProviderLease>>> {
        Box::pin(async move {
            context.check()?;
            self.0.contract.check_support(&request.contract)?;
            Ok(Arc::new(GuestLease {
                provider: self.0.clone(),
                contract: request.contract,
                closing: Cancellation::default(),
            }) as Arc<dyn ProviderLease>)
        })
    }
}
struct GuestLease {
    provider: Arc<GuestProvider>,
    contract: Contract,
    closing: Cancellation,
}

impl ProviderLease for GuestLease {
    fn invoke(
        &self,
        context: CallContext,
        method: Method,
        arguments: Vec<Value>,
    ) -> BoxFuture<'_, Result<ProviderResult>> {
        Box::pin(async move {
            if self.closing.is_cancelled() || !self.contract.methods.contains(&method) {
                return Err(Status::new("unavailable", "RPC guest lease closed"));
            }
            self.provider.invoke(context, method, None, arguments).await
        })
    }
    fn close(&self) -> BoxFuture<'_, Result<()>> {
        Box::pin(async {
            self.closing.cancel();
            Ok(())
        })
    }
}
struct GuestResource {
    provider: Arc<GuestProvider>,
    reference: ResourceRef,
}
impl Resource for GuestResource {
    fn invoke(
        &self,
        context: CallContext,
        method: String,
        arguments: Vec<Value>,
    ) -> BoxFuture<'_, Result<Vec<Value>>> {
        Box::pin(async move {
            let method = self
                .provider
                .contract
                .methods
                .iter()
                .find(|m| m.resource_type_hash == self.reference.type_hash && m.name == method)
                .cloned()
                .ok_or_else(|| {
                    Status::new("unimplemented", "RPC guest resource method unavailable")
                })?;
            Ok(self
                .provider
                .invoke(context, method, Some(self.reference.clone()), arguments)
                .await?
                .values)
        })
    }
    fn close(&self) -> BoxFuture<'_, Result<()>> {
        Box::pin(self.provider.release(self.reference.clone()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn partial_export_failure_releases_every_guest_resource() {
        for available in 0..3 {
            let limits = Limits {
                max_resources: 3,
                ..Limits::default()
            };
            let method = Method {
                id: "fixture.Service.Open".into(),
                service: "fixture.Service".into(),
                name: "Open".into(),
                contract_hash: "a".repeat(64),
                resource_type_hash: String::new(),
            };
            let contract = Contract::new(vec![method.clone()]);
            let guest =
                GuestProvider::new(Handle::current(), contract.clone(), limits.clone()).unwrap();
            let binder = LocalBinder::new(
                Handle::current(),
                limits.clone(),
                vec![Arc::new(GuestDeclaration(guest.clone()))],
            )
            .unwrap();
            let routes = binder
                .bind(CallContext::default(), BindRequest::new(contract))
                .await
                .unwrap();
            for (ids, accept) in [(1..4 - available, true), (11..14, false)] {
                let caller = routes.clone();
                let method = method.clone();
                let call = tokio::spawn(async move {
                    caller
                        .invoke(
                            CallContext::default(),
                            Call {
                                method,
                                receiver: None,
                                arguments: vec![],
                            },
                        )
                        .await
                });
                let event = guest.accept(CallContext::default()).await.unwrap();
                let request_id = event.response.request_id;
                event.consume();
                let values: Vec<_> = ids
                    .map(|object_id| {
                        Value::resource(ResourceRef {
                            epoch: 1,
                            object_id,
                            type_hash: "b".repeat(64),
                        })
                    })
                    .collect();
                guest
                    .respond(
                        request_id,
                        encode_values(&values, &limits).unwrap(),
                        String::new(),
                        String::new(),
                    )
                    .unwrap();
                let result = call.await.unwrap();
                if accept {
                    result.unwrap().accept().await.unwrap().consume();
                } else {
                    assert!(matches!(result, Err(error) if error.code == "resource_exhausted"));
                }
            }
            let mut released = Vec::new();
            for _ in 0..3 {
                let event = guest
                    .accept(CallContext::with_deadline(
                        std::time::Instant::now() + std::time::Duration::from_secs(1),
                    ))
                    .await
                    .unwrap();
                assert_eq!(event.response.operation, "release");
                released.push(event.response.receiver.as_ref().unwrap().object_id);
                event.consume();
            }
            released.sort_unstable();
            assert_eq!(released, vec![11, 12, 13]);
            routes.shutdown().await.unwrap();
            guest.close();
        }
    }
}
