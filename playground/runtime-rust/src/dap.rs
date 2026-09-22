use crate::{
    error::RuntimeError,
    execution::{Execution, ExecutionState, SharedInstance},
    instance::debug::{EventKind, FrameRef, StepMode, VariableRef},
};
#[cfg(not(target_arch = "wasm32"))]
use crate::{
    ffi::Cancellation,
    instance::{ExecutionLimits, Instance},
    loader::LoadLimits,
    program::Program,
};
use serde_json::{Value, json};
use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

#[derive(Clone, serde::Serialize, serde::Deserialize)]
pub struct Source {
    pub module: String,
    pub path: String,
    pub text: String,
}
enum Variables {
    Scope(FrameRef),
    Children(VariableRef),
}

#[derive(Clone, Default)]
pub struct OutputSink(Arc<Mutex<(Vec<Value>, usize)>>);
impl OutputSink {
    /// Queue provider output without entering the target VM.
    pub fn push(&self, category: &str, text: &str) -> Result<(), RuntimeError> {
        let mut state = self.0.lock().unwrap();
        if state.1 + text.len() > 1 << 20 || state.0.len() >= 4096 {
            return Err(RuntimeError::new(
                "event_overflow",
                "dap",
                "output queue exhausted",
            ));
        }
        state
            .0
            .push(json!({"event":"output","body":{"category":category,"output":text}}));
        state.1 += text.len();
        Ok(())
    }
}
/// A DAP session owns launched targets; bound targets have explicit ownership.
pub struct DebugSession {
    target: Option<SharedInstance>,
    execution: Option<Execution>,
    owned: bool,
    external: bool,
    next_handle: i64,
    threads: BTreeMap<i64, u64>,
    frames: BTreeMap<i64, FrameRef>,
    variables: BTreeMap<i64, Variables>,
    sources: BTreeMap<String, Source>,
    source_ids: BTreeMap<i64, String>,
    cursor: u64,
    terminated: bool,
    configured: bool,
    entry: String,
    output: OutputSink,
}
impl Default for DebugSession {
    fn default() -> Self {
        Self {
            target: None,
            execution: None,
            owned: false,
            external: false,
            next_handle: 1,
            threads: BTreeMap::new(),
            frames: BTreeMap::new(),
            variables: BTreeMap::new(),
            sources: BTreeMap::new(),
            source_ids: BTreeMap::new(),
            cursor: 0,
            terminated: false,
            configured: false,
            entry: "default".into(),
            output: OutputSink::default(),
        }
    }
}
impl DebugSession {
    pub fn output(&self) -> OutputSink {
        self.output.clone()
    }
    #[cfg(not(target_arch = "wasm32"))]
    pub fn launch(
        &mut self,
        image: &[u8],
        symbols: Option<&str>,
        sources: BTreeMap<String, Source>,
        entry: &str,
    ) -> Result<(), RuntimeError> {
        if self.target.is_some() {
            return Err(failure("target already launched"));
        }
        let mut program = Program::load(image, LoadLimits::default())?;
        if let Some(symbols) = symbols {
            program = program.with_symbols(serde_json::from_str(symbols)?)?;
        }
        let mut machine = Instance::new(Arc::new(program), ExecutionLimits::default())?;
        machine.initialize_root(&Cancellation::default())?;
        let target = SharedInstance::externally_driven(machine)?;
        self.bind(target, true, sources);
        self.external = true;
        self.entry = entry.into();
        Ok(())
    }
    pub fn bind(&mut self, target: SharedInstance, owned: bool, sources: BTreeMap<String, Source>) {
        self.close();
        self.target = Some(target);
        self.owned = owned;
        self.external = false;
        self.sources = sources;
        self.cursor = 0;
        self.terminated = false;
        self.configured = false;
    }
    pub fn set_entry(&mut self, entry: &str) -> Result<(), RuntimeError> {
        if self.configured {
            return Err(failure("target already running"));
        }
        self.entry = entry.to_owned();
        Ok(())
    }
    fn handle(&mut self) -> Result<i64, RuntimeError> {
        if self.next_handle > 9_007_199_254_740_991
            || self.frames.len() + self.variables.len() + self.threads.len() + self.source_ids.len()
                >= 8192
        {
            return Err(failure("debug handle budget exhausted"));
        }
        let id = self.next_handle;
        self.next_handle += 1;
        Ok(id)
    }
    fn thread(&mut self, task: u64) -> Result<i64, RuntimeError> {
        if let Some((&id, _)) = self.threads.iter().find(|(_, value)| **value == task) {
            return Ok(id);
        }
        let id = self.handle()?;
        self.threads.insert(id, task);
        Ok(id)
    }
    pub fn request(&mut self, command: &str, args: &Value) -> Result<Value, RuntimeError> {
        if command == "initialize" {
            return Ok(
                json!({"supportsConfigurationDoneRequest":true,"supportsTerminateRequest":true,"supportsDelayedStackTraceLoading":true}),
            );
        }
        if command == "disconnect" || command == "terminate" {
            self.close();
            return Ok(json!({}));
        }
        if command == "source" {
            let path = args["source"]["path"]
                .as_str()
                .or_else(|| {
                    self.source_ids
                        .get(&args["sourceReference"].as_i64().unwrap_or(0))
                        .map(String::as_str)
                })
                .ok_or_else(|| failure("missing source"))?;
            let source = self
                .sources
                .get(path)
                .ok_or_else(|| failure("unknown source"))?;
            return Ok(json!({"content":source.text,"mimeType":"text/x-go"}));
        }
        let target = self
            .target
            .as_ref()
            .ok_or_else(|| failure("no debug target"))?;
        let debugger = target.debugger();
        match command {
            "setBreakpoints" => {
                let path = args["source"]["path"]
                    .as_str()
                    .ok_or_else(|| failure("missing source path"))?;
                let source = self
                    .sources
                    .get(path)
                    .ok_or_else(|| failure("unknown source path"))?;
                let lines: Vec<i64> = args["breakpoints"]
                    .as_array()
                    .map(|values| {
                        values
                            .iter()
                            .filter_map(|value| value["line"].as_i64())
                            .collect()
                    })
                    .unwrap_or_default();
                let actual = debugger.set_breakpoints(&source.module, &source.path, &lines)?;
                Ok(
                    json!({"breakpoints":lines.into_iter().map(|line|json!({"line":line,"verified":actual.contains(&line),"source":{"path":path}})).collect::<Vec<_>>()}),
                )
            }
            "configurationDone" => {
                if self.configured {
                    return Err(failure("configuration already completed"));
                }
                self.execution = Some(target.start(&self.entry, vec![])?);
                self.configured = true;
                Ok(json!({}))
            }
            "threads" => {
                let threads = debugger.threads()?;
                let live: std::collections::BTreeSet<_> =
                    threads.iter().map(|thread| thread.id).collect();
                self.threads.retain(|_, task| live.contains(task));
                let mut result = Vec::new();
                for thread in threads {
                    let id = self.thread(thread.id)?;
                    result.push(
                        json!({"id":id,"name":format!("task {} ({})",thread.id,thread.state)}),
                    );
                }
                Ok(json!({"threads":result}))
            }
            "stackTrace" => {
                let task = *self
                    .threads
                    .get(&args["threadId"].as_i64().unwrap_or(0))
                    .ok_or_else(|| failure("unknown thread"))?;
                let frames: Vec<_> = debugger
                    .stack()?
                    .into_iter()
                    .filter(|frame| frame.reference.task == task)
                    .collect();
                let total = frames.len();
                let start = args["startFrame"].as_u64().unwrap_or(0) as usize;
                let limit = args["levels"]
                    .as_u64()
                    .filter(|value| *value != 0)
                    .unwrap_or(100)
                    .min(1000) as usize;
                let mut result = Vec::new();
                for frame in frames.into_iter().skip(start).take(limit) {
                    let id = self.handle()?;
                    self.frames.insert(id, frame.reference.clone());
                    let location = frame.locations.first();
                    let file = location
                        .map(|location| location.file.as_str())
                        .unwrap_or("");
                    let path = self
                        .sources
                        .iter()
                        .find(|(_, source)| source.module == frame.module && source.path == file)
                        .map(|(path, _)| path.as_str())
                        .unwrap_or(file)
                        .to_owned();
                    let reference = if let Some((&id, _)) =
                        self.source_ids.iter().find(|(_, value)| **value == path)
                    {
                        id
                    } else {
                        let id = self.handle()?;
                        self.source_ids.insert(id, path.clone());
                        id
                    };
                    result.push(json!({"id":id,"name":frame.function,"source":{"path":path,"sourceReference":reference},"line":location.map(|location|location.line).unwrap_or(1),"column":location.map(|location|location.column).unwrap_or(1)}));
                }
                Ok(json!({"stackFrames":result,"totalFrames":total}))
            }
            "scopes" => {
                let frame = self
                    .frames
                    .get(&args["frameId"].as_i64().unwrap_or(0))
                    .ok_or_else(|| failure("stale frame"))?
                    .clone();
                let id = self.handle()?;
                self.variables.insert(id, Variables::Scope(frame));
                Ok(json!({"scopes":[{"name":"Locals","variablesReference":id,"expensive":false}]}))
            }
            "variables" => {
                let reference = self
                    .variables
                    .get(&args["variablesReference"].as_i64().unwrap_or(0))
                    .ok_or_else(|| failure("stale variable reference"))?;
                let start = args["start"].as_u64().unwrap_or(0) as usize;
                let count = args["count"]
                    .as_u64()
                    .filter(|value| *value != 0)
                    .unwrap_or(100)
                    .min(1000) as usize;
                let values = match reference {
                    Variables::Scope(frame) => debugger.variables(frame, start, count)?,
                    Variables::Children(reference) => debugger.children(reference, start, count)?,
                };
                let mut result = Vec::new();
                for value in values {
                    let reference = if let Some(reference) = value.reference {
                        let id = self.handle()?;
                        self.variables.insert(id, Variables::Children(reference));
                        id
                    } else {
                        0
                    };
                    result.push(json!({"name":value.name,"value":value.summary,"type":format!("{:?}",value.typ),"variablesReference":reference,"indexedVariables":value.children}));
                }
                Ok(json!({"variables":result}))
            }
            "pause" => {
                debugger.pause();
                Ok(json!({}))
            }
            "continue" | "next" | "stepIn" | "stepOut" => {
                let task = *self
                    .threads
                    .get(&args["threadId"].as_i64().unwrap_or(0))
                    .ok_or_else(|| failure("unknown thread"))?;
                let mode = match command {
                    "next" => StepMode::Over,
                    "stepIn" => StepMode::Into,
                    "stepOut" => StepMode::Out,
                    _ => StepMode::Continue,
                };
                debugger.resume_task(mode, task)?;
                self.frames.clear();
                self.variables.clear();
                Ok(json!({"allThreadsContinued":true}))
            }
            _ => Err(failure("unsupported DAP request")),
        }
    }
    pub fn events(&mut self) -> Result<Vec<Value>, RuntimeError> {
        let mut result = {
            let mut output = self.output.0.lock().unwrap();
            output.1 = 0;
            std::mem::take(&mut output.0)
        };
        let Some(target) = &self.target else {
            if self.configured && !self.terminated {
                self.terminated = true;
                result.push(json!({"event":"terminated","body":{}}));
            }
            return Ok(result);
        };
        let events = target.debugger().events()?;
        if events
            .first()
            .is_some_and(|event| event.sequence > self.cursor + 1)
        {
            return Err(RuntimeError::new(
                "event_overflow",
                "dap",
                "debug events exceeded consumer capacity",
            ));
        }
        for event in events {
            if event.sequence == u64::MAX {
                return Err(RuntimeError::new(
                    "event_overflow",
                    "dap",
                    "debug event sequence exhausted",
                ));
            }
            if event.sequence <= self.cursor {
                continue;
            }
            self.cursor = event.sequence;
            let id = self.thread(event.frame.reference.task)?;
            let reason = match event.kind {
                EventKind::Panic => "exception",
                EventKind::Pause => "pause",
                EventKind::Step => "step",
                EventKind::Breakpoint => "breakpoint",
            };
            result.push(json!({"event":"stopped","body":{"reason":reason,"threadId":id,"allThreadsStopped":true}}));
        }
        if !self.terminated
            && self.execution.as_ref().is_some_and(|execution| {
                matches!(
                    execution.state(),
                    ExecutionState::Completed | ExecutionState::Canceled | ExecutionState::Failed
                ) && execution.scope_settled()
            })
        {
            self.terminated = true;
            result.push(json!({"event":"terminated","body":{}}));
        }
        Ok(result)
    }
    /// Advance the target on the protocol owner's event loop.
    pub fn poll(&self) -> Result<(), RuntimeError> {
        if let Some(target) = &self.target {
            target.drive(4096)?;
        }
        Ok(())
    }
    pub fn close(&mut self) {
        if let Some(execution) = self.execution.take() {
            execution.cancel();
        }
        if let Some(target) = self.target.take()
            && self.owned
        {
            #[cfg(not(target_arch = "wasm32"))]
            {
                if self.external {
                    target.begin_shutdown();
                    while target.shutdown_result().is_none() {
                        if target.drive(4096).is_err() {
                            break;
                        }
                    }
                } else {
                    let _ = target.shutdown(&Cancellation::default());
                }
            }
            #[cfg(target_arch = "wasm32")]
            target.begin_shutdown();
        }
        self.frames.clear();
        self.variables.clear();
        self.threads.clear();
        self.sources.clear();
        self.source_ids.clear();
    }
}
impl Drop for DebugSession {
    fn drop(&mut self) {
        self.close();
    }
}
fn failure(message: &str) -> RuntimeError {
    RuntimeError::new("invalid_request", "dap", message)
}
