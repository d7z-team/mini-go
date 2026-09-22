use crate::{
    environment::{Clock, SystemClock},
    error::RuntimeError,
    ffi::{self, Bridge, Cancellation, Completion, Reply},
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::{DecodedImage, LoadLimits},
    program::Program,
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::Duration,
};

pub const FORMAT: &str = "mini-go-tools";
pub const VERSION: u64 = 2;
const MAX_BYTES: usize = 64 << 20;

#[derive(Default)]
struct ControlState {
    canceled: bool,
    completion: Option<Completion>,
}
#[derive(Clone, Default)]
struct Control(Arc<Mutex<BTreeMap<String, ControlState>>>);
impl Control {
    fn cancel(&self, token: &str) {
        let completion = {
            let mut states = self.0.lock().unwrap();
            let Some(state) = states.get_mut(token) else {
                return;
            };
            state.canceled = true;
            state.completion.take()
        };
        if let Some(completion) = completion {
            completion.complete(Reply::new(b"canceled".to_vec(), None, None));
        }
    }
}
impl Bridge for Control {
    fn open(&self, _: Cancellation) -> Result<Box<dyn ffi::Session>, RuntimeError> {
        Ok(Box::new(self.clone()))
    }
    fn capabilities(&self) -> Vec<String> {
        vec!["minigo.tools.control".into()]
    }
}
struct ControlCall {
    control: Control,
    token: String,
}
impl ffi::Call for ControlCall {
    fn cancel(&self) {
        self.control.0.lock().unwrap().remove(&self.token);
    }
}
impl ffi::Session for Control {
    fn start(
        &self,
        _: Cancellation,
        request: ffi::Request,
        completion: Completion,
    ) -> Result<Box<dyn ffi::Call>, RuntimeError> {
        if request.route != "minigo.tools.control" {
            return Err(RuntimeError::new(
                "provider",
                "tools",
                "unknown tools route",
            ));
        }
        let request: Value = serde_json::from_slice(&request.payload).map_err(json_error)?;
        let token = request["Token"]
            .as_str()
            .ok_or_else(|| RuntimeError::new("invalid_argument", "tools", "missing token"))?
            .to_owned();
        let operation = request["Operation"].as_str().unwrap_or("");
        match operation {
            "wait" => {
                let mut states = self.0.lock().unwrap();
                let state = states
                    .get_mut(&token)
                    .ok_or_else(|| RuntimeError::new("stale", "tools", "unknown token"))?;
                if state.completion.is_some() {
                    return Err(RuntimeError::new(
                        "invalid_argument",
                        "tools",
                        "duplicate wait",
                    ));
                }
                if state.canceled {
                    drop(states);
                    completion.complete(Reply::new(b"canceled".to_vec(), None, None));
                } else {
                    state.completion = Some(completion);
                }
            }
            "finish" => {
                let state = self.0.lock().unwrap().remove(&token);
                if let Some(wait) = state.and_then(|state| state.completion) {
                    wait.complete(Reply::new(Vec::new(), None, None));
                }
                completion.complete(Reply::new(Vec::new(), None, None));
            }
            _ => {
                return Err(RuntimeError::new(
                    "invalid_argument",
                    "tools",
                    "invalid control operation",
                ));
            }
        }
        Ok(Box::new(ControlCall {
            control: self.clone(),
            token,
        }))
    }
    fn shutdown(&self, _: Cancellation) -> Result<(), RuntimeError> {
        self.0.lock().unwrap().clear();
        Ok(())
    }
}

fn json_error(error: serde_json::Error) -> RuntimeError {
    RuntimeError::new("invalid_argument", "tools", error.to_string())
}

/// Owned compiler inputs. This is not an execution snapshot.
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RestoreState {
    version: u32,
    input: Option<Arc<Value>>,
}
impl Default for RestoreState {
    fn default() -> Self {
        Self {
            version: 1,
            input: None,
        }
    }
}
impl RestoreState {
    pub fn decode(bytes: &[u8]) -> Result<Self, RuntimeError> {
        if bytes.len() > MAX_BYTES {
            return Err(failure("budget", "restore state too large"));
        }
        let state: Self = serde_json::from_slice(bytes).map_err(json_error)?;
        state.validate()?;
        Ok(state)
    }
    fn validate(&self) -> Result<(), RuntimeError> {
        if self.version != 1
            || self
                .input
                .as_ref()
                .is_some_and(|v| v["Operation"] != "workspace/open")
        {
            return Err(failure("identity", "invalid compiler restore state"));
        }
        Ok(())
    }
    pub fn encode(&self) -> Result<Vec<u8>, RuntimeError> {
        self.validate()?;
        let bytes = serde_json::to_vec(self).map_err(json_error)?;
        if bytes.len() > MAX_BYTES {
            return Err(failure("budget", "restore state too large"));
        }
        Ok(bytes)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SessionState {
    Idle,
    Discarded,
    Initializing,
    Recovering,
    Running,
    Canceling,
    AwaitingConfirmation,
    Closed,
}
#[derive(Clone, Copy)]
enum Stage {
    Initialize,
    Hello,
    Restore,
    Analyze,
    Request,
}

/// A successful result remains provisional until its owner acknowledges delivery.
#[derive(Debug)]
pub struct CompilerReply {
    pub value: Value,
    pub restore: RestoreState,
}
/// The candidate is committed even if cleanup of the previous owner reports an error.
#[derive(Debug)]
pub struct UpgradeResult {
    pub cleanup_error: Option<RuntimeError>,
}
#[derive(Debug)]
pub enum CompilerPoll {
    Running,
    Pending,
    Ready(CompilerReply),
}

struct Operation {
    input: Value,
    stage: Stage,
    token: String,
    until: u64,
    wall_deadline: String,
    canceled: Option<(&'static str, u64)>,
}

/// Platform-neutral compiler owner. Each poll executes at most the supplied
/// instruction budget; external IO and result delivery remain with the caller.
pub struct CompilerSession {
    program: Option<Arc<Program>>,
    machine: Option<Instance>,
    control: Control,
    clock: Arc<dyn Clock>,
    state: SessionState,
    operation: Option<Operation>,
    provisional: Option<RestoreState>,
    confirmed: RestoreState,
    generation: u64,
    generation_high_watermark: u64,
    next_token: u64,
    session: String,
    revision: String,
    snapshot: String,
}

fn failure(code: &'static str, message: impl Into<String>) -> RuntimeError {
    RuntimeError::new(code, "tools", message)
}

impl CompilerSession {
    pub fn from_image(
        image: &[u8],
        generation: u64,
        restore: RestoreState,
    ) -> Result<Self, RuntimeError> {
        Self::with_clock(image, generation, restore, Arc::new(SystemClock::default()))
    }
    pub fn with_clock(
        image: &[u8],
        generation: u64,
        restore: RestoreState,
        clock: Arc<dyn Clock>,
    ) -> Result<Self, RuntimeError> {
        if generation == 0 {
            return Err(failure("invalid_argument", "generation must be positive"));
        }
        restore.encode()?;
        let decoded = if image.starts_with(&[0x1f, 0x8b]) {
            DecodedImage::decode_gzip(image, LoadLimits::compiler())?
        } else {
            DecodedImage::decode(image, LoadLimits::compiler())?
        };
        Ok(Self {
            program: Some(Arc::new(Program::prepare(decoded)?)),
            machine: None,
            control: Control::default(),
            clock,
            state: SessionState::Idle,
            operation: None,
            provisional: None,
            confirmed: restore,
            generation,
            generation_high_watermark: generation,
            next_token: 0,
            session: String::new(),
            revision: String::new(),
            snapshot: String::new(),
        })
    }
    pub fn state(&self) -> SessionState {
        self.state
    }
    pub fn generation(&self) -> u64 {
        self.generation
    }
    pub fn revision(&self) -> &str {
        &self.revision
    }
    pub fn snapshot(&self) -> &str {
        &self.snapshot
    }
    pub fn confirmed_input(&self) -> Option<Value> {
        self.confirmed.input.as_deref().cloned()
    }
    pub fn restore_state(&self) -> &RestoreState {
        &self.confirmed
    }
    pub fn stats(&self) -> Option<crate::instance::stats::Stats> {
        self.machine.as_ref().map(Instance::stats)
    }

    pub fn start(&mut self, input: Value, timeout: Duration) -> Result<(), RuntimeError> {
        if self.state == SessionState::Closed {
            return Err(failure("closed", "compiler session closed"));
        }
        if self.state != SessionState::Idle {
            return Err(failure("busy", "compiler delivery is still active"));
        }
        if !input.is_object() {
            return Err(failure(
                "invalid_argument",
                "compiler request must be an object",
            ));
        }
        if serde_json::to_vec(&input).map_err(json_error)?.len() > MAX_BYTES {
            return Err(failure("budget", "request too large"));
        }
        let (seconds, nanos) = self.clock.unix_time();
        let now_wall = i128::from(seconds) * 1_000_000_000 + i128::from(nanos);
        let mut budget = timeout.as_nanos().min(30_000_000_000) as u64;
        if let Some(deadline) = input.get("Deadline") {
            let deadline = deadline
                .as_str()
                .and_then(|v| v.parse::<i128>().ok())
                .ok_or_else(|| failure("invalid_argument", "invalid request deadline"))?;
            budget =
                budget.min(deadline.saturating_sub(now_wall).clamp(0, u64::MAX as i128) as u64);
        }
        if budget == 0 {
            return Err(failure("deadline", "compiler request deadline"));
        }
        let stage = if self.machine.is_none() {
            self.machine = Some(Instance::with_bridge(
                self.program
                    .as_ref()
                    .expect("open compiler program")
                    .clone(),
                ExecutionLimits::compiler(),
                &self.control,
            )?);
            self.state = SessionState::Initializing;
            Stage::Initialize
        } else {
            self.state = SessionState::Running;
            Stage::Request
        };
        self.operation = Some(Operation {
            input,
            stage,
            token: String::new(),
            until: self.clock.monotonic_ns().saturating_add(budget),
            wall_deadline: (now_wall + i128::from(budget)).to_string(),
            canceled: None,
        });
        Ok(())
    }
    pub fn cancel(&mut self) {
        if self.state == SessionState::AwaitingConfirmation {
            self.abandon();
            return;
        }
        if let Some(op) = &mut self.operation {
            if op.canceled.is_none() {
                op.canceled = Some(("canceled", self.clock.monotonic_ns()));
            }
            self.control.cancel(&op.token);
            self.state = SessionState::Canceling;
        }
    }
    /// Discard unacknowledged guest state, retaining only confirmed inputs.
    pub fn abandon(&mut self) {
        let _ = self.close_machine();
        self.control.0.lock().unwrap().clear();
        self.operation = None;
        self.provisional = None;
        self.session.clear();
        self.revision.clear();
        self.snapshot.clear();
        if self.state != SessionState::Closed {
            self.state = SessionState::Discarded;
        }
    }
    /// The platform owner allocates each replacement generation exactly once.
    pub fn set_generation(&mut self, generation: u64) -> Result<(), RuntimeError> {
        if self.machine.is_some()
            || !matches!(self.state, SessionState::Idle | SessionState::Discarded)
            || generation <= self.generation_high_watermark
        {
            return Err(failure(
                "stale",
                "replacement generation must increase on an idle discarded session",
            ));
        }
        self.generation = generation;
        self.generation_high_watermark = generation;
        self.state = SessionState::Idle;
        Ok(())
    }
    pub fn acknowledge(&mut self) -> Result<(), RuntimeError> {
        if self.state != SessionState::AwaitingConfirmation {
            return Err(failure("stale", "no compiler response to confirm"));
        }
        self.confirmed = self.provisional.take().expect("provisional response");
        self.state = SessionState::Idle;
        Ok(())
    }
    pub fn close_now(&mut self) -> Result<(), RuntimeError> {
        self.state = SessionState::Closed;
        let result = self.close_machine();
        self.abandon();
        self.confirmed = RestoreState::default();
        self.program = None;
        result
    }
    fn close_machine(&mut self) -> Result<(), RuntimeError> {
        let Some(mut machine) = self.machine.take() else {
            return Ok(());
        };
        // Control is the sole provider and its shutdown is synchronous on both targets.
        match machine.poll_close(&mut std::task::Context::from_waker(std::task::Waker::noop())) {
            std::task::Poll::Ready(result) => result,
            std::task::Poll::Pending => {
                Err(failure("internal", "compiler control shutdown pending"))
            }
        }
    }
    pub fn poll(&mut self, steps: usize) -> Result<CompilerPoll, RuntimeError> {
        let until = self.operation.as_ref().map(|operation| operation.until);
        let result = self.poll_operation(steps);
        if matches!(result, Ok(CompilerPoll::Ready(_)))
            && until.is_some_and(|until| self.clock.monotonic_ns() >= until)
        {
            self.abandon();
            return Err(failure("deadline", "compiler response delivery deadline"));
        }
        if result.is_err() && self.operation.is_some() {
            self.abandon();
        }
        result
    }
    fn poll_operation(&mut self, steps: usize) -> Result<CompilerPoll, RuntimeError> {
        let op = self
            .operation
            .as_mut()
            .ok_or_else(|| failure("stale", "no active compiler request"))?;
        let now = self.clock.monotonic_ns();
        if now >= op.until && op.canceled.is_none() {
            op.canceled = Some(("deadline", now));
        }
        if let Some((code, at)) = op.canceled {
            self.state = SessionState::Canceling;
            self.control.cancel(&op.token);
            if op.token.is_empty() || now.saturating_sub(at) >= 2_000_000_000 {
                return Err(failure(code, "compiler operation canceled"));
            }
        }
        let machine = self.machine.as_mut().expect("active compiler machine");
        if matches!(op.stage, Stage::Initialize) {
            match machine.poll_initialize(&Cancellation::default(), steps)? {
                PollStatus::Ready => {
                    op.stage = Stage::Hello;
                }
                PollStatus::Pending => return Ok(CompilerPoll::Pending),
                PollStatus::Running => return Ok(CompilerPoll::Running),
                PollStatus::Paused => {
                    return Err(failure("internal", "compiler initialization paused"));
                }
            }
            return Ok(CompilerPoll::Running);
        }
        if op.token.is_empty() {
            let mut request = match op.stage {
                Stage::Hello => json!({"Operation":"hello"}),
                Stage::Restore => self
                    .confirmed
                    .input
                    .as_deref()
                    .expect("confirmed workspace")
                    .clone(),
                Stage::Analyze => json!({"Operation":"workspace/analyze"}),
                Stage::Request => op.input.clone(),
                Stage::Initialize => unreachable!(),
            };
            self.next_token = self
                .next_token
                .checked_add(1)
                .ok_or_else(|| failure("budget", "request IDs exhausted"))?;
            op.token = self.next_token.to_string();
            request["Format"] = json!(FORMAT);
            request["Version"] = json!(VERSION);
            request["Token"] = json!(op.token);
            request["Epoch"] = json!(self.generation.to_string());
            request["Deadline"] = json!(op.wall_deadline);
            if request.get("Session").is_none() {
                request["Session"] = json!(self.session);
            }
            if request.get("Revision").is_none() {
                request["Revision"] = json!(self.revision);
            }
            if request["Query"].is_object() && request["Query"].get("Snapshot").is_none() {
                request["Query"]["Snapshot"] = json!(self.snapshot);
            }
            if request["Build"].is_object() && request["Build"].get("Revision").is_none() {
                request["Build"]["Revision"] = json!(self.revision);
            }
            let bytes = serde_json::to_vec(&request).map_err(json_error)?;
            if bytes.len() > MAX_BYTES {
                return Err(failure("budget", "request too large"));
            }
            self.control
                .0
                .lock()
                .unwrap()
                .insert(op.token.clone(), ControlState::default());
            machine.start_bytes("tools", &bytes)?;
        }
        match machine.poll_steps(steps)? {
            PollStatus::Running => return Ok(CompilerPoll::Running),
            PollStatus::Pending => return Ok(CompilerPoll::Pending),
            PollStatus::Paused => return Err(failure("internal", "compiler unexpectedly paused")),
            PollStatus::Ready => {}
        }
        self.control.0.lock().unwrap().remove(&op.token);
        if let Some((code, _)) = op.canceled {
            return Err(failure(code, "compiler operation canceled"));
        }
        let result = machine.snapshot_results(Default::default())?;
        let root = result
            .roots
            .first()
            .ok_or_else(|| failure("internal", "missing compiler result"))?;
        let bytes = result.bytes(root)?;
        if bytes.len() > MAX_BYTES {
            return Err(failure("budget", "compiler result too large"));
        }
        let response: Value = serde_json::from_slice(&bytes).map_err(json_error)?;
        if response["Format"] != FORMAT
            || response["Version"] != VERSION
            || response["CompilerID"].as_str()
                != Some(
                    self.program
                        .as_ref()
                        .expect("open compiler program")
                        .image()
                        .compiler_id
                        .as_str(),
                )
        {
            return Err(failure("identity", "unsupported compiler tools response"));
        }
        if let Some(error) = response["Error"].as_object() {
            let code = match error.get("Code").and_then(Value::as_str).unwrap_or("") {
                "canceled" => "canceled",
                "deadline" => "deadline",
                "stale" => "stale",
                "closed" => "closed",
                "budget" => "budget",
                "invalid_argument" => "invalid_argument",
                _ => "internal",
            };
            let error = failure(
                code,
                error
                    .get("Message")
                    .and_then(Value::as_str)
                    .unwrap_or("compiler request failed"),
            );
            let mutation = matches!(
                op.input["Operation"].as_str(),
                Some("workspace/open" | "workspace/update" | "document/update")
            );
            if matches!(op.stage, Stage::Request)
                && !mutation
                && !matches!(code, "canceled" | "deadline" | "internal")
            {
                self.operation = None;
                self.state = SessionState::Idle;
            }
            return Err(error);
        }
        if let Some(v) = response["Session"].as_str() {
            self.session = v.into();
        }
        if let Some(v) = response["Revision"].as_str().filter(|v| !v.is_empty()) {
            self.revision = v.into();
        }
        if let Some(v) = response["Analysis"]["Snapshot"].as_str() {
            self.snapshot = v.into();
        }
        op.token.clear();
        op.stage = match op.stage {
            Stage::Hello if self.confirmed.input.is_some() => {
                self.state = SessionState::Recovering;
                Stage::Restore
            }
            Stage::Hello => Stage::Request,
            Stage::Restore => Stage::Analyze,
            Stage::Analyze => Stage::Request,
            Stage::Request => {
                let mut restore = self.confirmed.clone();
                if response["Recovery"].is_object() {
                    restore.input = Some(Arc::new(response["Recovery"].clone()));
                }
                restore.encode()?;
                self.provisional = Some(restore.clone());
                self.operation = None;
                self.state = SessionState::AwaitingConfirmation;
                return Ok(CompilerPoll::Ready(CompilerReply {
                    value: response,
                    restore,
                }));
            }
            Stage::Initialize => unreachable!(),
        };
        self.state = match op.stage {
            Stage::Request => SessionState::Running,
            Stage::Restore | Stage::Analyze => SessionState::Recovering,
            _ => SessionState::Initializing,
        };
        Ok(CompilerPoll::Running)
    }
}
impl Drop for CompilerSession {
    fn drop(&mut self) {
        let _ = self.close_now();
    }
}

#[cfg(not(target_arch = "wasm32"))]
mod native;

#[cfg(all(test, not(target_arch = "wasm32")))]
mod tests {
    use super::*;

    #[test]
    fn admission_limits_and_exhausted_tokens_leave_no_active_work() {
        let mut session = CompilerSession::from_image(
            include_bytes!("../assets/compiler.json.gz"),
            1,
            RestoreState::default(),
        )
        .unwrap();
        let oversized = json!({"Operation":"hello", "Root":"x".repeat(MAX_BYTES)});
        assert_eq!(
            session
                .start(oversized, Duration::from_secs(30))
                .unwrap_err()
                .code,
            "budget"
        );
        assert_eq!(session.state(), SessionState::Idle);
        assert!(session.stats().is_none());
        session.next_token = u64::MAX;
        session
            .start(json!({"Operation":"hello"}), Duration::from_secs(30))
            .unwrap();
        loop {
            match session.poll(4096) {
                Ok(CompilerPoll::Running | CompilerPoll::Pending) => {}
                other => {
                    assert_eq!(other.unwrap_err().code, "budget");
                    break;
                }
            }
        }
        assert_eq!(session.state(), SessionState::Discarded);
        assert!(session.stats().is_none());
        assert!(session.control.0.lock().unwrap().is_empty());
        let program = Arc::downgrade(session.program.as_ref().unwrap());
        session.close_now().unwrap();
        assert!(program.upgrade().is_none());
    }
}
