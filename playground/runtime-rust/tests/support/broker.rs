use base64::Engine;
use mini_go::{
    RuntimeError,
    ffi::{Bridge, Call, Cancellation, Completion, Reply, Request, Session},
};
use serde::Deserialize;
use std::{
    collections::BTreeMap,
    io::{BufRead, BufReader, Read, Write},
    path::{Path, PathBuf},
    process::{Child, ChildStdin, Command, Stdio},
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicBool, AtomicU64, Ordering},
        mpsc,
    },
    thread::JoinHandle,
    time::Duration,
};

fn transport(error: impl std::fmt::Display) -> RuntimeError {
    RuntimeError::new("broker", "transport", error.to_string())
}

#[derive(Deserialize)]
struct Event {
    event: String,
    #[serde(default)]
    version: u8,
    #[serde(default)]
    id: u64,
    #[serde(default)]
    capabilities: Vec<String>,
    payload: Option<String>,
    #[serde(default)]
    code: String,
    #[serde(default)]
    error: String,
}

struct Pending {
    completion: Option<Completion>,
    started: Option<mpsc::Sender<Result<(), RuntimeError>>>,
}

struct State {
    writer: Mutex<Option<ChildStdin>>,
    child: Mutex<Child>,
    reader: Mutex<Option<JoinHandle<()>>>,
    pending: Mutex<BTreeMap<u64, Pending>>,
    next_id: AtomicU64,
    closed: AtomicBool,
    shutdown: Mutex<Option<Result<(), RuntimeError>>>,
}

impl State {
    fn send(
        &self,
        operation: &str,
        id: u64,
        route: Option<&str>,
        payload: Option<&[u8]>,
    ) -> Result<(), RuntimeError> {
        let payload = payload.map(|bytes| base64::engine::general_purpose::STANDARD.encode(bytes));
        let command =
            serde_json::json!({"operation":operation,"id":id,"route":route,"payload":payload});
        let mut writer = self.writer.lock().unwrap();
        let writer = writer
            .as_mut()
            .ok_or_else(|| transport("broker input is closed"))?;
        serde_json::to_writer(&mut *writer, &command).map_err(transport)?;
        writer.write_all(b"\n").map_err(transport)?;
        writer.flush().map_err(transport)
    }

    fn shutdown(&self) -> Result<(), RuntimeError> {
        let mut finished = self.shutdown.lock().unwrap();
        if let Some(result) = &*finished {
            return result.clone();
        }
        self.closed.store(true, Ordering::Release);
        let sent = self.send("shutdown", 0, None, None);
        self.writer.lock().unwrap().take();
        let status = {
            let mut child = self.child.lock().unwrap();
            if sent.is_err() {
                let _ = child.kill();
            }
            child.wait().map_err(transport)
        };
        let result = status.and_then(|status| {
            if status.success() {
                Ok(())
            } else {
                Err(transport(format!("broker exited with {status}")))
            }
        });
        if let Some(reader) = self.reader.lock().unwrap().take() {
            reader
                .join()
                .map_err(|_| transport("broker reader panicked"))?;
        }
        *finished = Some(result.clone());
        result
    }
}

pub struct BrokerBridge {
    path: PathBuf,
    capabilities: Vec<String>,
    prepared: Mutex<Option<BrokerSession>>,
}

struct BrokerSession {
    state: Arc<State>,
    capabilities: Vec<String>,
}

impl BrokerBridge {
    pub fn new(path: impl AsRef<Path>) -> Result<Self, RuntimeError> {
        let path = path.as_ref().to_owned();
        let session = BrokerSession::spawn(&path)?;
        Ok(Self {
            path,
            capabilities: session.capabilities.clone(),
            prepared: Mutex::new(Some(session)),
        })
    }
}

impl Bridge for BrokerBridge {
    fn capabilities(&self) -> Vec<String> {
        self.capabilities.clone()
    }
    fn open(&self, cancellation: Cancellation) -> Result<Box<dyn Session>, RuntimeError> {
        if cancellation.is_cancelled() {
            return Err(transport("session canceled"));
        }
        let session = match self.prepared.lock().unwrap().take() {
            Some(session) => session,
            None => BrokerSession::spawn(&self.path)?,
        };
        if session.capabilities != self.capabilities {
            return Err(transport("host capabilities changed"));
        }
        Ok(Box::new(session))
    }
}

impl BrokerSession {
    fn spawn(path: &Path) -> Result<Self, RuntimeError> {
        let mut child = Command::new(path)
            .arg("runtime-host-broker")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .map_err(transport)?;
        let writer = child.stdin.take().unwrap();
        let mut reader = BufReader::new(child.stdout.take().unwrap());
        let ready = (|| {
            let mut line = String::new();
            reader
                .by_ref()
                .take(64 << 10)
                .read_line(&mut line)
                .map_err(transport)?;
            let ready: Event = serde_json::from_str(&line).map_err(transport)?;
            if ready.event != "ready" || ready.version != 1 {
                return Err(transport("invalid broker handshake"));
            }
            Ok(ready)
        })();
        let ready = match ready {
            Ok(ready) => ready,
            Err(error) => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(error);
            }
        };
        let state = Arc::new(State {
            writer: Mutex::new(Some(writer)),
            child: Mutex::new(child),
            reader: Mutex::new(None),
            pending: Mutex::new(BTreeMap::new()),
            next_id: AtomicU64::new(0),
            closed: AtomicBool::new(false),
            shutdown: Mutex::new(None),
        });
        let weak = Arc::downgrade(&state);
        let thread = std::thread::spawn(move || {
            let mut line = Vec::new();
            let failure = loop {
                line.clear();
                match reader
                    .by_ref()
                    .take((32 << 20) + 1)
                    .read_until(b'\n', &mut line)
                {
                    Ok(0) => break transport("broker closed"),
                    Ok(_) if line.len() <= 32 << 20 => {}
                    Ok(_) => break transport("broker event exceeds limit"),
                    Err(error) => break transport(error),
                }
                let event: Event = match serde_json::from_slice(&line) {
                    Ok(event) => event,
                    Err(error) => break transport(error),
                };
                let Some(state) = weak.upgrade() else { return };
                if event.event == "closed" {
                    break transport("broker closed");
                }
                if event.event == "result" {
                    let completion = {
                        let mut pending = state.pending.lock().unwrap();
                        let completion = pending
                            .get_mut(&event.id)
                            .and_then(|pending| pending.completion.take());
                        if pending
                            .get(&event.id)
                            .is_some_and(|pending| pending.started.is_none())
                        {
                            pending.remove(&event.id);
                        }
                        completion
                    };
                    let Some(completion) = completion else {
                        let _ = state.send("discard", event.id, None, None);
                        continue;
                    };
                    let payload = match base64::engine::general_purpose::STANDARD
                        .decode(event.payload.unwrap_or_default())
                    {
                        Ok(payload) => payload,
                        Err(error) => {
                            completion.complete(Reply::new(vec![], Some(transport(error)), None));
                            break transport("invalid broker payload");
                        }
                    };
                    let code = match event.code.as_str() {
                        "route_unavailable" => "route_unavailable",
                        "pending_limit" => "pending_limit",
                        _ => "host",
                    };
                    let error = (!event.error.is_empty())
                        .then(|| RuntimeError::new(code, "host", event.error));
                    let discard = weak.clone();
                    let consumed = weak.clone();
                    let id = event.id;
                    completion.complete(
                        Reply::new(
                            payload,
                            error,
                            Some(Box::new(move || {
                                if let Some(state) = discard.upgrade() {
                                    let _ = state.send("discard", id, None, None);
                                }
                            })),
                        )
                        .on_consumed(move || {
                            if let Some(state) = consumed.upgrade() {
                                let _ = state.send("consumed", id, None, None);
                            }
                        }),
                    );
                } else if event.event == "started" || event.event == "start_error" {
                    let started = {
                        let mut pending = state.pending.lock().unwrap();
                        let started = pending
                            .get_mut(&event.id)
                            .and_then(|pending| pending.started.take());
                        if event.event == "start_error"
                            || pending
                                .get(&event.id)
                                .is_some_and(|pending| pending.completion.is_none())
                        {
                            pending.remove(&event.id);
                        }
                        started
                    };
                    if let Some(started) = started {
                        let result = if event.event == "started" {
                            Ok(())
                        } else {
                            let code = match event.code.as_str() {
                                "route_unavailable" => "route_unavailable",
                                "pending_limit" => "pending_limit",
                                _ => "host",
                            };
                            Err(RuntimeError::new(code, "host", event.error))
                        };
                        let _ = started.send(result);
                    }
                } else {
                    break transport("unexpected broker event");
                }
            };
            if let Some(state) = weak.upgrade() {
                state.closed.store(true, Ordering::Release);
                let pending = std::mem::take(&mut *state.pending.lock().unwrap());
                for (_, pending) in pending {
                    if let Some(started) = pending.started {
                        let _ = started.send(Err(failure.clone()));
                    }
                    if let Some(completion) = pending.completion {
                        completion.complete(Reply::new(vec![], Some(failure.clone()), None));
                    }
                }
            }
        });
        *state.reader.lock().unwrap() = Some(thread);
        Ok(Self {
            state,
            capabilities: ready.capabilities,
        })
    }
}

struct BrokerCall {
    state: Weak<State>,
    id: u64,
}
impl Call for BrokerCall {
    fn cancel(&self) {
        if let Some(state) = self.state.upgrade() {
            state.pending.lock().unwrap().remove(&self.id);
            let _ = state.send("cancel", self.id, None, None);
        }
    }
}

impl Session for BrokerSession {
    fn start(
        &self,
        cancellation: Cancellation,
        request: Request,
        completion: Completion,
    ) -> Result<Box<dyn Call>, RuntimeError> {
        if self.state.closed.load(Ordering::Acquire) || cancellation.is_cancelled() {
            return Err(transport("session is closed or canceled"));
        }
        let id = self
            .state
            .next_id
            .try_update(Ordering::Relaxed, Ordering::Relaxed, |id| id.checked_add(1))
            .map_err(|_| transport("call IDs exhausted"))?
            + 1;
        let (send, receive) = mpsc::channel();
        self.state.pending.lock().unwrap().insert(
            id,
            Pending {
                completion: Some(completion),
                started: Some(send),
            },
        );
        if let Err(error) =
            self.state
                .send("start", id, Some(&request.route), Some(&request.payload))
        {
            self.state.pending.lock().unwrap().remove(&id);
            return Err(error);
        }
        match receive
            .recv_timeout(Duration::from_secs(30))
            .map_err(transport)
            .and_then(|result| result)
        {
            Ok(()) => Ok(Box::new(BrokerCall {
                state: Arc::downgrade(&self.state),
                id,
            })),
            Err(error) => {
                self.state.pending.lock().unwrap().remove(&id);
                let _ = self.state.send("cancel", id, None, None);
                Err(error)
            }
        }
    }
    fn shutdown(&self, _: Cancellation) -> Result<(), RuntimeError> {
        self.state.shutdown()
    }
}

impl Drop for BrokerSession {
    fn drop(&mut self) {
        let _ = self.state.shutdown();
    }
}
