//! WebAssembly bindings. A worker owns one VM and drives it at task boundaries.
#![forbid(unsafe_code)]
#![cfg(target_arch = "wasm32")]

mod compiler;
mod mailbox;
pub use compiler::WasmCompiler;
#[cfg(feature = "rpc")]
mod network;
#[cfg(feature = "rpc")]
mod rpc;
#[cfg(feature = "rpc")]
mod rpc_api;
#[cfg(feature = "rpc")]
mod transport;
use mini_go::{
    self as runtime, Execution, Instance, Program, RuntimeError,
    ffi::{Bridge, Cancellation},
    instance::{Instance as Machine, PollStatus},
};
#[cfg(feature = "rpc")]
pub use rpc_api::WasmRpc;
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    sync::Arc,
    task::{Context, Waker},
};
use wasm_bindgen::prelude::*;

#[wasm_bindgen(inline_js = "export function notify(){globalThis.__miniGoWake?.()}")]
extern "C" {
    pub(crate) fn notify();
}
struct DriverWake;
impl std::task::Wake for DriverWake {
    fn wake(self: Arc<Self>) {
        notify();
    }
}

pub(crate) fn error(error: impl std::fmt::Display + 'static) -> JsValue {
    let value = js_sys::Error::new(&error.to_string());
    if let Some(failure) = (&error as &dyn std::any::Any).downcast_ref::<RuntimeError>() {
        let _ = js_sys::Reflect::set(&value, &"code".into(), &failure.code.into());
        let _ = js_sys::Reflect::set(&value, &"path".into(), &failure.path.clone().into());
    }
    #[cfg(feature = "rpc")]
    if let Some(failure) = (&error as &dyn std::any::Any).downcast_ref::<runtime::rpc::Status>() {
        let _ = js_sys::Reflect::set(&value, &"code".into(), &failure.code.clone().into());
    }
    value.into()
}
pub(crate) fn encode(value: &impl Serialize) -> Result<JsValue, JsValue> {
    value
        .serialize(
            &serde_wasm_bindgen::Serializer::new()
                .serialize_maps_as_objects(true)
                .serialize_large_number_types_as_bigints(true),
        )
        .map_err(error)
}
#[derive(Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
struct Options {
    capabilities: Vec<String>,
    workload: Workload,
    max_steps: Option<i64>,
    max_heap_bytes: Option<u64>,
    max_pending_calls: usize,
    rpc: bool,
    rpc_options: RpcOptions,
    symbols: Option<Vec<u8>>,
}

#[derive(Default, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct RpcOptions {
    lease_ttl_ms: Option<u64>,
    admission_timeout_ms: Option<u64>,
    max_call_duration_ms: Option<u64>,
}

impl RpcOptions {
    fn validate(&self) -> Result<(), &'static str> {
        if [
            self.lease_ttl_ms,
            self.admission_timeout_ms,
            self.max_call_duration_ms,
        ]
        .into_iter()
        .flatten()
        .any(|duration| duration == 0 || duration > i64::MAX as u64 / 1_000_000)
        {
            return Err("invalid RPC duration in milliseconds");
        }
        Ok(())
    }
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
enum Workload {
    #[default]
    Runtime,
    Compiler,
}
impl Default for Options {
    fn default() -> Self {
        Self {
            capabilities: vec![],
            workload: Workload::Runtime,
            max_steps: None,
            max_heap_bytes: None,
            max_pending_calls: 128,
            rpc: false,
            rpc_options: RpcOptions::default(),
            symbols: None,
        }
    }
}

#[wasm_bindgen]
pub struct WasmVm {
    machine: Option<Machine>,
    instance: Option<Instance>,
    executions: BTreeMap<u32, Execution>,
    next: u32,
    cancellation: Cancellation,
    mailbox: mailbox::Mailbox,
    closing: bool,
    init_error: Option<RuntimeError>,
    max_host_result_bytes: usize,
    debug_session: Option<runtime::dap::DebugSession>,
    #[cfg(feature = "rpc")]
    network: Option<Arc<network::RpcNetwork>>,
    #[cfg(feature = "rpc")]
    host: Option<Arc<runtime::rpc::Host>>,
}

#[wasm_bindgen]
impl WasmVm {
    #[wasm_bindgen(constructor)]
    pub fn new(image: &[u8], options: JsValue) -> Result<WasmVm, JsValue> {
        let options: Options = serde_wasm_bindgen::from_value(options).map_err(error)?;
        options.rpc_options.validate().map_err(error)?;
        let (mut limits, load) = match options.workload {
            Workload::Compiler => (
                runtime::Limits::compiler(),
                runtime::LoadOptions::compiler(),
            ),
            Workload::Runtime => (
                runtime::Limits {
                    max_heap_bytes: 32 << 20,
                    max_ffi_bytes: 8 << 20,
                    max_ffi_result_bytes: 4 << 20,
                    ..Default::default()
                },
                runtime::LoadOptions {
                    max_image_bytes: 32 << 20,
                    max_artifact_bytes: 16 << 20,
                    ..Default::default()
                },
            ),
        };
        if let Some(steps) = options.max_steps {
            limits.max_steps = steps;
        }
        if let Some(bytes) = options.max_heap_bytes {
            limits.max_heap_bytes = bytes;
        }
        limits.max_pending_calls = options.max_pending_calls;
        let limits = limits.normalize().map_err(error)?;
        if options.max_pending_calls == 0
            || options.max_pending_calls > 4096
            || limits.max_heap_bytes > 256 << 20
            || limits.max_heap_bytes == 0
        {
            return Err(error("invalid WASM limits"));
        }
        let decoded = if image.starts_with(&[0x1f, 0x8b]) {
            runtime::loader::DecodedImage::decode_gzip(image, load)
        } else {
            runtime::loader::DecodedImage::decode(image, load)
        }
        .map_err(error)?;
        let mut program = Program::prepare(decoded).map_err(error)?;
        if let Some(symbols) = options.symbols {
            if symbols.len() > 16 << 20 {
                return Err(error("symbols exceed WASM limit"));
            }
            program = program
                .with_symbols(serde_json::from_slice(&symbols).map_err(error)?)
                .map_err(error)?;
        }
        let program = Arc::new(program);
        let mailbox = mailbox::Mailbox::new(options.capabilities, options.max_pending_calls);
        let bridge: Arc<dyn Bridge> = Arc::new(mailbox.clone());
        #[cfg(feature = "rpc")]
        let mut bridge = bridge;
        #[cfg(feature = "rpc")]
        let (mut network, mut host) = (None, None);
        if options.rpc {
            #[cfg(not(feature = "rpc"))]
            return Err(error("this WASM build has no RPC feature"));
            #[cfg(feature = "rpc")]
            {
                let rpc_network = network::RpcNetwork::open(&options.rpc_options)?;
                let mut host_options =
                    runtime::rpc::HostOptions::new(runtime::rpc::platform::Handle);
                host_options.fallback = Some(rpc_network.endpoint.clone());
                host_options.publish_provider =
                    Some(Arc::new(rpc::Publisher(rpc_network.router.clone())));
                let remote = Arc::new(runtime::rpc::Host::new(host_options).map_err(error)?);
                bridge = Arc::new(rpc::Bridge {
                    local: mailbox.clone(),
                    remote: remote.clone(),
                });
                network = Some(rpc_network);
                host = Some(remote);
            }
        }
        let machine = Machine::with_bridge(program, limits, bridge.as_ref()).map_err(error)?;
        Ok(Self {
            machine: Some(machine),
            instance: None,
            executions: BTreeMap::new(),
            next: 0,
            cancellation: Cancellation::default(),
            mailbox,
            closing: false,
            init_error: None,
            max_host_result_bytes: limits.max_ffi_result_bytes,
            debug_session: None,
            #[cfg(feature = "rpc")]
            network,
            #[cfg(feature = "rpc")]
            host,
        })
    }

    /// One bounded VM batch. The JS driver yields between tasks, not microtasks.
    pub fn pump(&mut self, steps: usize) -> Result<JsValue, JsValue> {
        if steps == 0 || steps > 65_536 {
            return Err(error("invalid poll budget"));
        }
        let waker = Waker::from(Arc::new(DriverWake));
        let mut cx = Context::from_waker(&waker);
        let mut running = false;
        let mut closed = false;
        let mut delay = None;
        if let Some(machine) = self.machine.as_mut() {
            if self.closing {
                if let std::task::Poll::Ready(result) = machine.poll_close(&mut cx) {
                    result.map_err(error)?;
                    closed = true;
                }
            } else {
                match machine.poll_initialize(&self.cancellation, steps) {
                    Ok(PollStatus::Ready) => {
                        self.instance = Some(
                            Instance::externally_driven(self.machine.take().unwrap())
                                .map_err(error)?,
                        );
                    }
                    Ok(PollStatus::Running) => running = true,
                    Ok(_) => {}
                    Err(error) => {
                        self.init_error = Some(error);
                        self.closing = true;
                        running = true;
                    }
                }
            }
        }
        if let Some(instance) = &self.instance {
            let observed = instance.wake().epoch();
            if self.closing {
                instance.begin_shutdown();
            }
            match instance.drive(steps) {
                Ok(value) => running |= value,
                Err(failure) if failure.code == "closed" => {}
                Err(failure) => {
                    if instance.stats().state == runtime::instance::stats::InstanceState::Faulted {
                        self.init_error = Some(failure);
                        self.closing = true;
                    }
                    running = true;
                }
            }
            if let Some(result) = instance.shutdown_result() {
                result.map_err(error)?;
                closed = true;
            }
            delay = instance.next_timer_delay().map_err(error)?;
            if instance.wake().poll_changed(observed, &cx).is_ready() {
                running = true;
            }
        } else if let Some(machine) = &self.machine {
            let observed = machine.wake().epoch();
            delay = machine.next_timer_delay();
            if machine.wake().poll_changed(observed, &cx).is_ready() {
                running = true;
            }
        }
        let executions: Vec<_> = self
            .executions
            .iter()
            .map(|(id, execution)| {
                serde_json::json!({
                    "id": id,
                    "state": format!("{:?}", execution.state()),
                    "settled": execution.scope_settled(),
                    "scopeError": execution.scope_stats().error.map(|error| error.to_string()),
                })
            })
            .collect();
        // JSON status fields are JS Numbers; HostValue payloads use encode()
        // instead, preserving BigInt and owned typed arrays.
        js_sys::JSON::parse(
            &serde_json::json!({
                "ready": self.instance.is_some(),
                "running": running && !closed,
                "closed": closed,
                "delay": delay.map(|duration| duration.as_secs_f64() * 1000.0),
                "executions": executions,
                "error": self.init_error.as_ref().map(ToString::to_string),
            })
            .to_string(),
        )
    }

    pub fn start(&mut self, entry: &str, arguments: JsValue) -> Result<u32, JsValue> {
        if self.closing || self.executions.len() >= 128 {
            return Err(error("instance call capacity unavailable"));
        }
        let arguments: Vec<runtime::HostValue> =
            serde_wasm_bindgen::from_value(arguments).map_err(error)?;
        let instance = self
            .instance
            .as_ref()
            .ok_or_else(|| error("initialization pending"))?;
        let id = self
            .next
            .checked_add(1)
            .ok_or_else(|| error("execution identity exhausted"))?;
        let execution = instance.start_host(entry, &arguments).map_err(error)?;
        self.next = id;
        self.executions.insert(id, execution);
        Ok(id)
    }
    pub fn result(&self, id: u32) -> Result<JsValue, JsValue> {
        let execution = self
            .executions
            .get(&id)
            .ok_or_else(|| error("unknown execution"))?;
        let snapshot = execution.result().map_err(error)?;
        #[derive(Serialize)]
        struct Snapshot<'a> {
            roots: &'a [runtime::HostValue],
            objects: &'a [runtime::HostValue],
        }
        encode(&Snapshot {
            roots: &snapshot.roots,
            objects: &snapshot.objects,
        })
    }
    pub fn release(&mut self, id: u32) -> Result<(), JsValue> {
        if self
            .executions
            .get(&id)
            .is_some_and(|execution| !execution.scope_settled())
        {
            return Err(error("execution scope is still active"));
        }
        self.executions.remove(&id);
        Ok(())
    }
    pub fn cancel(&self, id: u32) {
        if let Some(execution) = self.executions.get(&id) {
            execution.cancel();
        }
    }
    pub fn close(&mut self) {
        self.closing = true;
        self.cancellation.cancel();
        if let Some(instance) = &self.instance {
            instance.begin_shutdown();
        }
    }
    pub fn actions(&self) -> Result<JsValue, JsValue> {
        encode(&self.mailbox.drain())
    }
    pub fn complete(
        &self,
        id: u32,
        payload: &[u8],
        failure: Option<String>,
    ) -> Result<(), JsValue> {
        if payload.len() > self.max_host_result_bytes {
            return self
                .mailbox
                .complete(
                    id,
                    vec![],
                    Some(RuntimeError::new(
                        "resource_exhausted",
                        "host",
                        "host result exceeds WASM limit",
                    )),
                )
                .map_err(error);
        }
        self.mailbox
            .complete(
                id,
                payload.to_vec(),
                failure.map(|message| RuntimeError::new("host", "js", message)),
            )
            .map_err(error)
    }
    pub fn host_closed(&self) {
        self.mailbox.closed();
    }
    pub fn decision_done(&self, id: u32) {
        self.mailbox.decision_done(id);
    }
    pub fn pause(&self) {
        if let Some(instance) = &self.instance {
            instance.debugger().pause();
        }
    }

    pub fn debug_open(&mut self, sources: JsValue, entry: &str) -> Result<(), JsValue> {
        if self.debug_session.is_some() {
            return Err(error("debug session already open"));
        }
        let sources = serde_wasm_bindgen::from_value(sources).map_err(error)?;
        let target = self
            .instance
            .as_ref()
            .ok_or_else(|| error("initialization pending"))?
            .clone();
        let mut session = runtime::dap::DebugSession::default();
        session.bind(target, false, sources);
        session.set_entry(entry).map_err(error)?;
        self.debug_session = Some(session);
        Ok(())
    }

    pub fn debug_request(&mut self, command: &str, arguments: JsValue) -> Result<JsValue, JsValue> {
        let arguments = serde_wasm_bindgen::from_value(arguments).map_err(error)?;
        let session = self
            .debug_session
            .as_mut()
            .ok_or_else(|| error("debug session not open"))?;
        let value = session.request(command, &arguments).map_err(error)?;
        js_sys::JSON::parse(&value.to_string())
    }

    pub fn debug_events(&mut self) -> Result<JsValue, JsValue> {
        let session = self
            .debug_session
            .as_mut()
            .ok_or_else(|| error("debug session not open"))?;
        js_sys::JSON::parse(
            &serde_json::to_string(&session.events().map_err(error)?).map_err(error)?,
        )
    }

    pub fn debug_output(&self, category: &str, text: &str) -> Result<(), JsValue> {
        self.debug_session
            .as_ref()
            .ok_or_else(|| error("debug session not open"))?
            .output()
            .push(category, text)
            .map_err(error)
    }
    pub fn stack(&self) -> Result<JsValue, JsValue> {
        encode(
            &self
                .instance
                .as_ref()
                .ok_or_else(|| error("initialization pending"))?
                .debugger()
                .stack()
                .map_err(error)?,
        )
    }
    pub fn bindings(&self, frame: JsValue) -> Result<JsValue, JsValue> {
        let frame = serde_wasm_bindgen::from_value(frame).map_err(error)?;
        let bindings = self
            .instance
            .as_ref()
            .ok_or_else(|| error("initialization pending"))?
            .debugger()
            .bindings(
                &frame,
                runtime::SnapshotLimits {
                    max_bytes: 4 << 20,
                    max_objects: 10_000,
                    max_depth: 64,
                },
            )
            .map_err(error)?;
        #[derive(Serialize)]
        struct Bindings<'a> {
            names: &'a [String],
            roots: &'a [runtime::HostValue],
            objects: &'a [runtime::HostValue],
        }
        encode(&Bindings {
            names: &bindings.names,
            roots: &bindings.values.roots,
            objects: &bindings.values.objects,
        })
    }
    pub fn breakpoints(
        &self,
        module: &str,
        file: &str,
        lines: JsValue,
    ) -> Result<JsValue, JsValue> {
        let lines: Vec<i64> = serde_wasm_bindgen::from_value(lines).map_err(error)?;
        if lines.len() > 4096 {
            return Err(error("breakpoint capacity exceeded"));
        }
        encode(
            &self
                .instance
                .as_ref()
                .ok_or_else(|| error("initialization pending"))?
                .debugger()
                .set_breakpoints(module, file, &lines)
                .map_err(error)?,
        )
    }
    pub fn stats(&self) -> Result<JsValue, JsValue> {
        let stats = self
            .instance
            .as_ref()
            .ok_or_else(|| error("initialization pending"))?
            .stats();
        encode_stats(stats)
    }
    pub fn resume(&self) -> Result<(), JsValue> {
        self.instance
            .as_ref()
            .ok_or_else(|| error("initialization pending"))?
            .debugger()
            .resume(runtime::instance::debug::StepMode::Continue)
            .map_err(error)
    }
    pub fn patch(&self, image: &[u8]) -> Result<(), JsValue> {
        let instance = self
            .instance
            .as_ref()
            .ok_or_else(|| error("initialization pending"))?;
        let program =
            Arc::new(Program::load(image, runtime::LoadOptions::default()).map_err(error)?);
        let plan = instance.prepare_patch(program).map_err(error)?;
        instance.apply_patch(plan).map_err(error)?;
        Ok(())
    }
    #[cfg(feature = "rpc")]
    pub fn receive(&self, frame: &[u8]) -> Result<(), JsValue> {
        self.network
            .as_ref()
            .ok_or_else(|| error("RPC unavailable"))?
            .receive(frame)
    }
    #[cfg(feature = "rpc")]
    pub fn outgoing(&self) -> Result<JsValue, JsValue> {
        encode(
            &self
                .network
                .as_ref()
                .map(|network| network.outgoing())
                .unwrap_or_default(),
        )
    }
    #[cfg(feature = "rpc")]
    pub fn sent(&self, id: u32) {
        if let Some(network) = &self.network {
            network.sent(id);
        }
    }
    #[cfg(feature = "rpc")]
    pub fn disconnect(&self) {
        if let Some(network) = &self.network {
            network.disconnect();
        }
    }
    #[cfg(feature = "rpc")]
    pub fn close_network(&self) -> js_sys::Promise {
        let host = self.host.clone();
        let network = self.network.clone();
        wasm_bindgen_futures::future_to_promise(async move {
            let host = match host {
                Some(host) => host.shutdown().await,
                None => Ok(()),
            };
            let network = match network {
                Some(network) => network.shutdown().await,
                None => Ok(()),
            };
            host.and(network).map_err(error)?;
            Ok(JsValue::UNDEFINED)
        })
    }
}

pub(crate) fn encode_stats(stats: runtime::instance::stats::Stats) -> Result<JsValue, JsValue> {
    #[derive(Serialize)]
    #[serde(rename_all = "camelCase")]
    struct Stats {
        state: String,
        steps: u64,
        heap_bytes: u64,
        heap_objects: usize,
        heap_peak_bytes: u64,
        heap_allocated_bytes: u64,
        memory_bytes: u64,
        memory_peak_bytes: u64,
        memory_allocated_bytes: u64,
        active_scopes: usize,
        tasks: usize,
        timers: usize,
        ffi_calls: usize,
        ffi_bytes: usize,
        generation: u64,
    }
    encode(&Stats {
        state: format!("{:?}", stats.state),
        steps: stats.executed_steps,
        heap_bytes: stats.heap.live_bytes,
        heap_objects: stats.heap.live_objects,
        heap_peak_bytes: stats.heap.peak_bytes,
        heap_allocated_bytes: stats.heap.total_allocated_bytes,
        memory_bytes: stats.memory.live_bytes,
        memory_peak_bytes: stats.memory.peak_bytes,
        memory_allocated_bytes: stats.memory.total_allocated_bytes,
        active_scopes: stats.active_scopes,
        tasks: stats.runnable_tasks + stats.blocked_tasks + stats.paused_tasks,
        timers: stats.timers,
        ffi_calls: stats.pending_ffi_calls,
        ffi_bytes: stats.pending_boundary_bytes,
        generation: stats.revision.generation,
    })
}
