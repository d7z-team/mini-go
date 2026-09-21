//! Host-side JavaScript RPC bindings backed by the native Rust endpoint.

use crate::{RpcOptions, encode, error, network::RpcNetwork, notify};
use mini_go::{
    ffi::Cancellation,
    rpc::{self, Binder, BoxFuture, CallContext, Provider, ProviderLease, Resource},
};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use std::{
    any::Any,
    collections::{BTreeMap, VecDeque},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU32, Ordering},
    },
    time::Duration,
};
use tokio::sync::oneshot;
use wasm_bindgen::prelude::*;

fn decode_js<T: DeserializeOwned>(value: JsValue) -> rpc::Result<T> {
    serde_wasm_bindgen::from_value(value)
        .map_err(|failure| rpc::Status::new("invalid_argument", failure.to_string()))
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct JsMethod {
    id: String,
    service: String,
    name: String,
    contract_hash: String,
    #[serde(default)]
    resource_type_hash: String,
}

impl From<JsMethod> for rpc::Method {
    fn from(value: JsMethod) -> Self {
        Self {
            id: value.id,
            service: value.service,
            name: value.name,
            contract_hash: value.contract_hash,
            resource_type_hash: value.resource_type_hash,
        }
    }
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct JsContract {
    protocol: String,
    methods: Vec<JsMethod>,
}

impl From<JsContract> for rpc::Contract {
    fn from(value: JsContract) -> Self {
        Self {
            protocol: value.protocol,
            methods: value.methods.into_iter().map(Into::into).collect(),
        }
    }
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct JsField {
    id: u32,
    value: JsRpcValue,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct JsMapEntry {
    key: JsRpcValue,
    value: JsRpcValue,
}

#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct JsRpcValue {
    #[serde(rename = "type")]
    typ: String,
    data: JsData,
}

#[derive(Deserialize, Serialize)]
#[serde(tag = "kind", content = "value", rename_all = "camelCase")]
enum JsData {
    Nil,
    Bool(bool),
    Int(i64),
    Uint(u64),
    Float(f64),
    Complex((f64, f64)),
    String(String),
    Bytes(#[serde(with = "serde_bytes")] Vec<u8>),
    Slice(Vec<JsRpcValue>),
    Map(Vec<JsMapEntry>),
    Struct(Vec<JsField>),
    Optional(Box<JsRpcValue>),
    Resource(u32),
    LocalResource(u32),
}

fn decode_plain_value(value: JsRpcValue) -> rpc::Result<rpc::Value> {
    let data = match value.data {
        JsData::Nil => rpc::Data::Nil,
        JsData::Bool(value) => rpc::Data::Bool(value),
        JsData::Int(value) => rpc::Data::Int(value),
        JsData::Uint(value) => rpc::Data::Uint(value),
        JsData::Float(value) => rpc::Data::Float(value),
        JsData::Complex((real, imaginary)) => rpc::Data::Complex(real, imaginary),
        JsData::String(value) => rpc::Data::String(value),
        JsData::Bytes(value) => rpc::Data::Bytes(value),
        JsData::Slice(values) => rpc::Data::Slice(
            values
                .into_iter()
                .map(decode_plain_value)
                .collect::<rpc::Result<_>>()?,
        ),
        JsData::Map(entries) => rpc::Data::Map(
            entries
                .into_iter()
                .map(|entry| {
                    Ok(rpc::MapEntry {
                        key: decode_plain_value(entry.key)?,
                        value: decode_plain_value(entry.value)?,
                    })
                })
                .collect::<rpc::Result<_>>()?,
        ),
        JsData::Struct(fields) => rpc::Data::Struct(
            fields
                .into_iter()
                .map(|field| {
                    Ok(rpc::Field {
                        id: field.id,
                        value: decode_plain_value(field.value)?,
                    })
                })
                .collect::<rpc::Result<_>>()?,
        ),
        JsData::Optional(value) => rpc::Data::Optional(Box::new(decode_plain_value(*value)?)),
        JsData::Resource(_) | JsData::LocalResource(_) => {
            return Err(rpc::Status::new(
                "invalid_argument",
                "RPC resources are only valid as direct method fields",
            ));
        }
    };
    Ok(rpc::Value::new(value.typ, data))
}

fn encode_plain_value(value: &rpc::Value) -> rpc::Result<JsRpcValue> {
    let data = match &value.data {
        rpc::Data::Nil => JsData::Nil,
        rpc::Data::Bool(value) => JsData::Bool(*value),
        rpc::Data::Int(value) => JsData::Int(*value),
        rpc::Data::Uint(value) => JsData::Uint(*value),
        rpc::Data::Float(value) => JsData::Float(*value),
        rpc::Data::Complex(real, imaginary) => JsData::Complex((*real, *imaginary)),
        rpc::Data::String(value) => JsData::String(value.clone()),
        rpc::Data::Bytes(value) => JsData::Bytes(value.clone()),
        rpc::Data::Slice(values) => JsData::Slice(
            values
                .iter()
                .map(encode_plain_value)
                .collect::<rpc::Result<_>>()?,
        ),
        rpc::Data::Map(entries) => JsData::Map(
            entries
                .iter()
                .map(|entry| {
                    Ok(JsMapEntry {
                        key: encode_plain_value(&entry.key)?,
                        value: encode_plain_value(&entry.value)?,
                    })
                })
                .collect::<rpc::Result<_>>()?,
        ),
        rpc::Data::Struct(fields) => JsData::Struct(
            fields
                .iter()
                .map(|field| {
                    Ok(JsField {
                        id: field.id,
                        value: encode_plain_value(&field.value)?,
                    })
                })
                .collect::<rpc::Result<_>>()?,
        ),
        rpc::Data::Optional(value) => JsData::Optional(Box::new(encode_plain_value(value)?)),
        rpc::Data::Resource(_) => {
            return Err(rpc::Status::new(
                "protocol",
                "nested RPC resource cannot cross the JavaScript boundary",
            ));
        }
    };
    Ok(JsRpcValue {
        typ: value.typ.clone(),
        data,
    })
}

#[derive(Default, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct JsCallOptions {
    timeout_ms: Option<u64>,
}

#[derive(Default, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct JsBindOptions {
    affinity_key: String,
    labels: BTreeMap<String, String>,
    timeout_ms: Option<u64>,
}

#[derive(Default, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct JsPublishOptions {
    name: String,
    priority: i64,
    weight: u64,
    max_leases: usize,
    labels: BTreeMap<String, String>,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct JsPeer {
    identity: String,
    attributes: BTreeMap<String, String>,
}

#[derive(Serialize)]
#[serde(tag = "kind", rename_all = "camelCase")]
enum JsAction {
    ProviderCall {
        id: u32,
        provider: u32,
        resource: Option<u32>,
        method: String,
        arguments: Vec<JsRpcValue>,
        peer: JsPeer,
        provider_name: String,
        timeout_ms: Option<f64>,
    },
    ProviderCancel {
        id: u32,
    },
    ResourceClose {
        id: u32,
        resource: u32,
    },
}

struct JsBridge {
    next: AtomicU32,
    actions: Mutex<VecDeque<JsAction>>,
    calls: Mutex<BTreeMap<u32, oneshot::Sender<JsCompletion>>>,
    closes: Mutex<BTreeMap<u32, oneshot::Sender<rpc::Result<()>>>>,
    closing: Cancellation,
}

struct JsCompletion {
    result: rpc::Result<Vec<JsRpcValue>>,
    retained: oneshot::Sender<Vec<u32>>,
}

impl JsBridge {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            next: AtomicU32::new(1),
            actions: Mutex::new(VecDeque::new()),
            calls: Mutex::new(BTreeMap::new()),
            closes: Mutex::new(BTreeMap::new()),
            closing: Cancellation::default(),
        })
    }

    fn next_identity(&self) -> rpc::Result<u32> {
        self.next
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |value| {
                value.checked_add(1)
            })
            .map_err(|_| {
                rpc::Status::new("resource_exhausted", "JavaScript RPC identity exhausted")
            })
    }

    fn enqueue_action(&self, action: JsAction) {
        self.actions.lock().unwrap().push_back(action);
        notify();
    }

    async fn invoke_provider(
        &self,
        context: &CallContext,
        provider: u32,
        resource: Option<u32>,
        method: String,
        arguments: Vec<JsRpcValue>,
    ) -> rpc::Result<JsCompletion> {
        context.check()?;
        if self.closing.is_cancelled() {
            return Err(rpc::Status::new(
                "unavailable",
                "JavaScript RPC bridge closed",
            ));
        }
        let id = self.next_identity()?;
        let (send, receive) = oneshot::channel();
        self.calls.lock().unwrap().insert(id, send);
        let timeout_ms = context.deadline.map(|deadline| {
            deadline
                .saturating_duration_since(web_time::Instant::now())
                .as_secs_f64()
                * 1000.0
        });
        self.enqueue_action(JsAction::ProviderCall {
            id,
            provider,
            resource,
            method,
            arguments,
            peer: JsPeer {
                identity: context.peer.identity.clone(),
                attributes: context.peer.attributes.clone(),
            },
            provider_name: context.provider.clone(),
            timeout_ms,
        });
        let result = context
            .run(async {
                receive.await.map_err(|_| {
                    rpc::Status::new("unavailable", "JavaScript RPC handler abandoned")
                })
            })
            .await;
        if self.calls.lock().unwrap().remove(&id).is_some() {
            self.enqueue_action(JsAction::ProviderCancel { id });
        }
        result
    }

    async fn close_resource(&self, resource: u32) -> rpc::Result<()> {
        if self.closing.is_cancelled() {
            return Ok(());
        }
        let id = self.next_identity()?;
        let (send, receive) = oneshot::channel();
        self.closes.lock().unwrap().insert(id, send);
        self.enqueue_action(JsAction::ResourceClose { id, resource });
        tokio::select! {
            _ = self.closing.cancelled() => Ok(()),
            result = receive => result.map_err(|_| rpc::Status::new("unavailable", "JavaScript resource close abandoned"))?,
        }
    }

    async fn complete_provider_call(
        &self,
        id: u32,
        result: rpc::Result<Vec<JsRpcValue>>,
    ) -> Vec<u32> {
        let Some(send) = self.calls.lock().unwrap().remove(&id) else {
            return Vec::new();
        };
        let (retained, receive) = oneshot::channel();
        if send.send(JsCompletion { result, retained }).is_err() {
            return Vec::new();
        }
        receive.await.unwrap_or_default()
    }

    fn complete_resource_close(&self, id: u32, result: rpc::Result<()>) {
        let Some(send) = self.closes.lock().unwrap().remove(&id) else {
            return;
        };
        let _ = send.send(result);
    }

    fn drain_actions(&self) -> Vec<JsAction> {
        self.actions.lock().unwrap().drain(..).collect()
    }

    fn shutdown(&self) {
        self.closing.cancel();
        let failure = rpc::Status::new("unavailable", "JavaScript RPC bridge closed");
        for (_, send) in std::mem::take(&mut *self.calls.lock().unwrap()) {
            let (retained, _receive) = oneshot::channel();
            let _ = send.send(JsCompletion {
                result: Err(failure.clone()),
                retained,
            });
        }
        for (_, send) in std::mem::take(&mut *self.closes.lock().unwrap()) {
            let _ = send.send(Err(failure.clone()));
        }
    }
}

struct JsProvider {
    id: u32,
    contract: rpc::Contract,
    bridge: Arc<JsBridge>,
}

impl Provider for JsProvider {
    fn contract(&self) -> rpc::Contract {
        self.contract.clone()
    }

    fn bind(
        &self,
        context: CallContext,
        request: rpc::BindRequest,
    ) -> BoxFuture<'_, rpc::Result<Arc<dyn ProviderLease>>> {
        Box::pin(async move {
            context.check()?;
            self.contract.check_support(&request.contract)?;
            Ok(Arc::new(JsLease {
                provider: self.id,
                contract: request.contract,
                bridge: self.bridge.clone(),
                closed: AtomicBool::new(false),
            }) as Arc<dyn ProviderLease>)
        })
    }
}

struct JsLease {
    provider: u32,
    contract: rpc::Contract,
    bridge: Arc<JsBridge>,
    closed: AtomicBool,
}

impl ProviderLease for JsLease {
    fn invoke(
        &self,
        context: CallContext,
        method: rpc::Method,
        arguments: Vec<rpc::Value>,
    ) -> BoxFuture<'_, rpc::Result<rpc::ProviderResult>> {
        Box::pin(async move {
            if self.closed.load(Ordering::Acquire) {
                return Err(rpc::Status::new(
                    "unavailable",
                    "JavaScript provider lease closed",
                ));
            }
            if !self.contract.methods.contains(&method) {
                return Err(rpc::Status::new("unimplemented", "RPC method is not bound"));
            }
            let arguments = provider_arguments(&context, self.provider, &arguments)?;
            let completion = self
                .bridge
                .invoke_provider(&context, self.provider, None, method.id, arguments)
                .await?;
            let ProviderValues { values, retained } =
                provider_results(&context, self.provider, &self.bridge, completion.result);
            let _ = completion.retained.send(retained);
            Ok(rpc::ProviderResult::new(values?))
        })
    }

    fn close(&self) -> BoxFuture<'_, rpc::Result<()>> {
        Box::pin(async {
            self.closed.store(true, Ordering::Release);
            Ok(())
        })
    }
}

struct JsResource {
    provider: u32,
    id: u32,
    bridge: Arc<JsBridge>,
}

impl Resource for JsResource {
    fn invoke(
        &self,
        context: CallContext,
        method: String,
        arguments: Vec<rpc::Value>,
    ) -> BoxFuture<'_, rpc::Result<Vec<rpc::Value>>> {
        Box::pin(async move {
            let arguments = provider_arguments(&context, self.provider, &arguments)?;
            let completion = self
                .bridge
                .invoke_provider(&context, self.provider, Some(self.id), method, arguments)
                .await?;
            let ProviderValues { values, retained } =
                provider_results(&context, self.provider, &self.bridge, completion.result);
            let _ = completion.retained.send(retained);
            values
        })
    }

    fn close(&self) -> BoxFuture<'_, rpc::Result<()>> {
        Box::pin(self.bridge.close_resource(self.id))
    }
}

fn provider_arguments(
    context: &CallContext,
    provider: u32,
    arguments: &[rpc::Value],
) -> rpc::Result<Vec<JsRpcValue>> {
    arguments
        .iter()
        .map(|value| {
            if let rpc::Data::Resource(reference) = &value.data {
                let resource: Arc<dyn Any + Send + Sync> = context.resolve(reference)?;
                let resource = resource.downcast::<JsResource>().map_err(|_| {
                    rpc::Status::new(
                        "invalid_argument",
                        "RPC resource is not owned by the JavaScript provider",
                    )
                })?;
                if resource.provider != provider {
                    return Err(rpc::Status::new(
                        "invalid_argument",
                        "RPC resource belongs to another JavaScript provider",
                    ));
                }
                Ok(JsRpcValue {
                    typ: value.typ.clone(),
                    data: JsData::LocalResource(resource.id),
                })
            } else {
                encode_plain_value(value)
            }
        })
        .collect()
}

struct ProviderValues {
    values: rpc::Result<Vec<rpc::Value>>,
    retained: Vec<u32>,
}

fn provider_results(
    context: &CallContext,
    provider: u32,
    bridge: &Arc<JsBridge>,
    values: rpc::Result<Vec<JsRpcValue>>,
) -> ProviderValues {
    let values = match values {
        Ok(values) => values,
        Err(failure) => {
            return ProviderValues {
                values: Err(failure),
                retained: Vec::new(),
            };
        }
    };
    let mut converted = Vec::with_capacity(values.len());
    let mut retained = Vec::new();
    for value in values {
        let result = match value.data {
            JsData::LocalResource(id) => context
                .export(
                    Arc::new(JsResource {
                        provider,
                        id,
                        bridge: bridge.clone(),
                    }),
                    value.typ,
                )
                .inspect(|_| retained.push(id)),
            JsData::Resource(_) => Err(rpc::Status::new(
                "invalid_argument",
                "remote resources cannot be returned by a JavaScript provider",
            )),
            _ => decode_plain_value(value),
        };
        match result {
            Ok(value) => converted.push(value),
            Err(failure) => {
                return ProviderValues {
                    values: Err(failure),
                    retained,
                };
            }
        }
    }
    ProviderValues {
        values: Ok(converted),
        retained,
    }
}

struct ClientResource {
    binding: u32,
    handle: Arc<rpc::ResourceHandle>,
}

struct PendingEntry {
    result: rpc::PendingResult,
    resources: Vec<u32>,
}

struct RpcOwner {
    network: Arc<RpcNetwork>,
    bridge: Arc<JsBridge>,
    next: AtomicU32,
    closing: AtomicBool,
    operations: Mutex<BTreeMap<u32, Cancellation>>,
    bindings: Mutex<BTreeMap<u32, Arc<rpc::RouteSet>>>,
    results: Mutex<BTreeMap<u32, PendingEntry>>,
    resources: Mutex<BTreeMap<u32, ClientResource>>,
    publications: Mutex<BTreeMap<u32, Arc<rpc::router::Registration>>>,
}

impl RpcOwner {
    fn next_identity(&self) -> rpc::Result<u32> {
        self.next
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |value| {
                value.checked_add(1)
            })
            .map_err(|_| rpc::Status::new("resource_exhausted", "WASM RPC identity exhausted"))
    }

    fn begin_operation(&self, operation: u32, timeout_ms: Option<u64>) -> rpc::Result<CallContext> {
        if operation == 0 {
            return Err(rpc::Status::new(
                "invalid_argument",
                "RPC operation identity must be nonzero",
            ));
        }
        if self.closing.load(Ordering::Acquire) {
            return Err(rpc::Status::new(
                "unavailable",
                "WASM RPC connection is closing",
            ));
        }
        if timeout_ms.is_some_and(|value| value == 0 || value > i64::MAX as u64 / 1_000_000) {
            return Err(rpc::Status::new("invalid_argument", "invalid RPC timeout"));
        }
        let cancellation = Cancellation::default();
        let mut operations = self.operations.lock().unwrap();
        if operations.contains_key(&operation) {
            return Err(rpc::Status::new(
                "already_exists",
                "RPC operation identity is already active",
            ));
        }
        operations.insert(operation, cancellation.clone());
        drop(operations);
        let mut context = CallContext::with_cancellation(cancellation);
        context.deadline =
            timeout_ms.map(|value| web_time::Instant::now() + Duration::from_millis(value));
        Ok(context)
    }

    fn finish_operation(&self, operation: u32) {
        self.operations.lock().unwrap().remove(&operation);
    }

    fn decode_argument(
        &self,
        routes: &Arc<rpc::RouteSet>,
        value: JsRpcValue,
    ) -> rpc::Result<rpc::Value> {
        match value.data {
            JsData::Resource(id) => {
                let resources = self.resources.lock().unwrap();
                let resource = resources.get(&id).ok_or_else(|| {
                    rpc::Status::new("not_found", "JavaScript RPC resource is closed")
                })?;
                let reference = resource.handle.reference(routes)?;
                if value.typ != reference.type_hash {
                    return Err(rpc::Status::new(
                        "invalid_argument",
                        "JavaScript RPC resource type does not match",
                    ));
                }
                Ok(rpc::Value::resource(reference))
            }
            JsData::LocalResource(_) => Err(rpc::Status::new(
                "invalid_argument",
                "provider resource cannot be used by an RPC client",
            )),
            _ => decode_plain_value(value),
        }
    }

    fn encode_result_value(
        &self,
        binding: u32,
        routes: &Arc<rpc::RouteSet>,
        value: &rpc::Value,
        created: &mut Vec<u32>,
    ) -> rpc::Result<JsRpcValue> {
        if let rpc::Data::Resource(reference) = &value.data {
            let id = self.next_identity()?;
            let handle = routes.bind_resource(reference.clone())?;
            self.resources
                .lock()
                .unwrap()
                .insert(id, ClientResource { binding, handle });
            created.push(id);
            return Ok(JsRpcValue {
                typ: value.typ.clone(),
                data: JsData::Resource(id),
            });
        }
        encode_plain_value(value)
    }

    fn remove_client_resources(&self, resources: &[u32]) {
        let mut owned = self.resources.lock().unwrap();
        for id in resources {
            owned.remove(id);
        }
    }
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct JsPendingResult {
    result: u32,
    values: Vec<JsRpcValue>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct JsStats {
    bindings: usize,
    resources: usize,
    pending_results: usize,
    publications: usize,
    pending_calls: usize,
    inbound_calls: usize,
}

#[wasm_bindgen]
pub struct WasmRpc {
    state: Arc<RpcOwner>,
}

#[wasm_bindgen]
impl WasmRpc {
    #[wasm_bindgen(constructor)]
    pub fn new(options: JsValue) -> Result<Self, JsValue> {
        let options: RpcOptions = decode_js(options).map_err(error)?;
        let network = RpcNetwork::open(&options)?;
        Ok(Self {
            state: Arc::new(RpcOwner {
                network,
                bridge: JsBridge::new(),
                next: AtomicU32::new(1),
                closing: AtomicBool::new(false),
                operations: Mutex::new(BTreeMap::new()),
                bindings: Mutex::new(BTreeMap::new()),
                results: Mutex::new(BTreeMap::new()),
                resources: Mutex::new(BTreeMap::new()),
                publications: Mutex::new(BTreeMap::new()),
            }),
        })
    }

    pub fn bind(&self, operation: u32, contract: JsValue, options: JsValue) -> js_sys::Promise {
        let state = self.state.clone();
        wasm_bindgen_futures::future_to_promise(async move {
            let contract: JsContract = decode_js(contract).map_err(error)?;
            let options: JsBindOptions = decode_js(options).map_err(error)?;
            let context = state
                .begin_operation(operation, options.timeout_ms)
                .map_err(error)?;
            let mut request = rpc::BindRequest::new(contract.into());
            request.options = rpc::BindOptions {
                affinity_key: options.affinity_key,
                labels: options.labels,
            };
            let result = state.network.endpoint.bind(context, request).await;
            state.finish_operation(operation);
            let routes = result.map_err(error)?;
            let id = state.next_identity().map_err(error)?;
            state.bindings.lock().unwrap().insert(id, routes);
            Ok(JsValue::from(id))
        })
    }

    pub fn invoke(
        &self,
        operation: u32,
        binding: u32,
        method: JsValue,
        receiver: Option<u32>,
        arguments: JsValue,
        options: JsValue,
    ) -> js_sys::Promise {
        let state = self.state.clone();
        wasm_bindgen_futures::future_to_promise(async move {
            let method: rpc::Method = decode_js::<JsMethod>(method).map_err(error)?.into();
            let arguments: Vec<JsRpcValue> = decode_js(arguments).map_err(error)?;
            let options: JsCallOptions = decode_js(options).map_err(error)?;
            let (routes, receiver) = if let Some(resource) = receiver {
                let resources = state.resources.lock().unwrap();
                let resource = resources.get(&resource).ok_or_else(|| {
                    error(rpc::Status::new("not_found", "RPC resource is closed"))
                })?;
                if resource.binding != binding {
                    return Err(error(rpc::Status::new(
                        "invalid_argument",
                        "RPC resource belongs to another binding",
                    )));
                }
                let routes = resource.handle.routes().clone();
                let reference = resource.handle.reference(&routes).map_err(error)?;
                (routes, Some(reference))
            } else {
                let routes = state
                    .bindings
                    .lock()
                    .unwrap()
                    .get(&binding)
                    .cloned()
                    .ok_or_else(|| {
                        error(rpc::Status::new("unavailable", "RPC binding is closed"))
                    })?;
                (routes, None)
            };
            let arguments = arguments
                .into_iter()
                .map(|value| state.decode_argument(&routes, value))
                .collect::<rpc::Result<Vec<_>>>()
                .map_err(error)?;
            let context = state
                .begin_operation(operation, options.timeout_ms)
                .map_err(error)?;
            let result = routes
                .invoke(
                    context,
                    rpc::Call {
                        method,
                        receiver,
                        arguments,
                    },
                )
                .await;
            state.finish_operation(operation);
            let result = result.map_err(error)?;
            let id = state.next_identity().map_err(error)?;
            let mut resources = Vec::new();
            let values = result
                .values
                .iter()
                .map(|value| state.encode_result_value(binding, &routes, value, &mut resources))
                .collect::<rpc::Result<Vec<_>>>();
            let values = match values {
                Ok(values) => values,
                Err(failure) => {
                    state.remove_client_resources(&resources);
                    return Err(error(failure));
                }
            };
            state
                .results
                .lock()
                .unwrap()
                .insert(id, PendingEntry { result, resources });
            encode(&JsPendingResult { result: id, values })
        })
    }

    pub fn accept(&self, result: u32) -> js_sys::Promise {
        let state = self.state.clone();
        wasm_bindgen_futures::future_to_promise(async move {
            let entry = state
                .results
                .lock()
                .unwrap()
                .remove(&result)
                .ok_or_else(|| error(rpc::Status::new("not_found", "RPC result is unavailable")))?;
            match entry.result.accept().await {
                Ok(accepted) => {
                    accepted.consume();
                    Ok(JsValue::UNDEFINED)
                }
                Err(failure) => {
                    state.remove_client_resources(&entry.resources);
                    Err(error(failure))
                }
            }
        })
    }

    pub fn discard(&self, result: u32) -> js_sys::Promise {
        let state = self.state.clone();
        wasm_bindgen_futures::future_to_promise(async move {
            let entry = state
                .results
                .lock()
                .unwrap()
                .remove(&result)
                .ok_or_else(|| error(rpc::Status::new("not_found", "RPC result is unavailable")))?;
            state.remove_client_resources(&entry.resources);
            entry.result.discard().await.map_err(error)?;
            Ok(JsValue::UNDEFINED)
        })
    }

    pub fn close_binding(&self, binding: u32) {
        if let Some(routes) = self.state.bindings.lock().unwrap().remove(&binding) {
            routes.close();
        }
    }

    pub fn close_resource(
        &self,
        operation: u32,
        resource: u32,
        options: JsValue,
    ) -> js_sys::Promise {
        let state = self.state.clone();
        wasm_bindgen_futures::future_to_promise(async move {
            let options: JsCallOptions = decode_js(options).map_err(error)?;
            let handle = {
                let resources = state.resources.lock().unwrap();
                let resource = resources.get(&resource).ok_or_else(|| {
                    error(rpc::Status::new("not_found", "RPC resource is closed"))
                })?;
                resource.handle.clone()
            };
            let context = state
                .begin_operation(operation, options.timeout_ms)
                .map_err(error)?;
            let result = handle.close(context).await;
            state.finish_operation(operation);
            result.map_err(error)?;
            state.resources.lock().unwrap().remove(&resource);
            Ok(JsValue::UNDEFINED)
        })
    }

    pub fn publish(
        &self,
        provider: u32,
        contract: JsValue,
        options: JsValue,
    ) -> Result<u32, JsValue> {
        if provider == 0 {
            return Err(error(rpc::Status::new(
                "invalid_argument",
                "JavaScript RPC provider identity must be nonzero",
            )));
        }
        if self.state.closing.load(Ordering::Acquire) {
            return Err(error(rpc::Status::new(
                "unavailable",
                "WASM RPC connection is closing",
            )));
        }
        let contract: rpc::Contract = decode_js::<JsContract>(contract).map_err(error)?.into();
        let options: JsPublishOptions = decode_js(options).map_err(error)?;
        let registration = self
            .state
            .network
            .router
            .register(
                Arc::new(JsProvider {
                    id: provider,
                    contract,
                    bridge: self.state.bridge.clone(),
                }),
                rpc::router::RegistrationOptions {
                    name: options.name,
                    priority: options.priority,
                    weight: options.weight,
                    max_leases: options.max_leases,
                    labels: options.labels,
                },
            )
            .map_err(error)?;
        let id = self.state.next_identity().map_err(error)?;
        self.state
            .publications
            .lock()
            .unwrap()
            .insert(id, registration);
        Ok(id)
    }

    pub fn close_publication(&self, publication: u32) -> js_sys::Promise {
        let state = self.state.clone();
        wasm_bindgen_futures::future_to_promise(async move {
            let registration = state
                .publications
                .lock()
                .unwrap()
                .get(&publication)
                .cloned()
                .ok_or_else(|| error(rpc::Status::new("not_found", "RPC publication is closed")))?;
            registration.abort();
            registration.close().await.map_err(error)?;
            let mut publications = state.publications.lock().unwrap();
            if publications
                .get(&publication)
                .is_some_and(|current| Arc::ptr_eq(current, &registration))
            {
                publications.remove(&publication);
            }
            Ok(JsValue::UNDEFINED)
        })
    }

    pub fn cancel(&self, operation: u32) {
        if let Some(cancellation) = self.state.operations.lock().unwrap().get(&operation) {
            cancellation.cancel();
        }
    }

    pub fn actions(&self) -> Result<JsValue, JsValue> {
        encode(&self.state.bridge.drain_actions())
    }

    pub fn provider_complete(
        &self,
        call: u32,
        values: JsValue,
        code: Option<String>,
        message: Option<String>,
    ) -> js_sys::Promise {
        let result: rpc::Result<Vec<JsRpcValue>> = if let Some(code) = code {
            Err(rpc::Status::new(code, message.unwrap_or_default()))
        } else {
            decode_js(values)
        };
        let bridge = self.state.bridge.clone();
        wasm_bindgen_futures::future_to_promise(async move {
            encode(&bridge.complete_provider_call(call, result).await)
        })
    }

    pub fn resource_complete(
        &self,
        close: u32,
        code: Option<String>,
        message: Option<String>,
    ) -> Result<(), JsValue> {
        self.state.bridge.complete_resource_close(
            close,
            code.map_or(Ok(()), |code| {
                Err(rpc::Status::new(code, message.unwrap_or_default()))
            }),
        );
        Ok(())
    }

    pub fn receive(&self, frame: &[u8]) -> Result<(), JsValue> {
        self.state.network.receive(frame)
    }

    pub fn outgoing(&self) -> Result<JsValue, JsValue> {
        encode(&self.state.network.outgoing())
    }

    pub fn sent(&self, id: u32) {
        self.state.network.sent(id);
    }

    pub fn disconnect(&self) {
        self.state.network.disconnect();
    }

    pub fn stats(&self) -> Result<JsValue, JsValue> {
        let endpoint = self.state.network.endpoint.stats();
        encode(&JsStats {
            bindings: self.state.bindings.lock().unwrap().len(),
            resources: self.state.resources.lock().unwrap().len(),
            pending_results: self.state.results.lock().unwrap().len(),
            publications: self.state.publications.lock().unwrap().len(),
            pending_calls: endpoint.pending_calls,
            inbound_calls: endpoint.inbound_calls,
        })
    }

    pub fn shutdown(&self) -> js_sys::Promise {
        let state = self.state.clone();
        state.closing.store(true, Ordering::Release);
        for cancellation in state.operations.lock().unwrap().values() {
            cancellation.cancel();
        }
        state.results.lock().unwrap().clear();
        wasm_bindgen_futures::future_to_promise(async move {
            let result = state.network.shutdown().await;
            state.bridge.shutdown();
            state.bindings.lock().unwrap().clear();
            state.resources.lock().unwrap().clear();
            state.publications.lock().unwrap().clear();
            result.map_err(error)?;
            Ok(JsValue::UNDEFINED)
        })
    }
}
