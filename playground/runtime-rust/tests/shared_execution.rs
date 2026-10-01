mod support;

use mini_go::{
    InstanceOptions,
    contract::{canonical_hash, canonical_json},
    contract_generated as wire,
    error::RuntimeError,
    execution::{Execution, ExecutionState, SharedInstance},
    ffi::{Bridge, Call, Cancellation, Completion, Reply, Request, Session},
    loader::LoadLimits,
    program::Program,
    snapshot::HostData,
};
use serde_json::json;
use std::{
    sync::{Arc, Mutex, mpsc},
    time::Duration,
};
use support::slot_code;

#[derive(Clone)]
struct Host {
    completions: mpsc::Sender<Completion>,
    closing: mpsc::Sender<()>,
    release: Arc<Mutex<mpsc::Receiver<()>>>,
    peer: Arc<Mutex<Option<SharedInstance>>>,
}
impl Call for Host {
    fn cancel(&self) {}
}
impl Bridge for Host {
    fn open(&self, _: Cancellation) -> Result<Box<dyn Session>, RuntimeError> {
        Ok(Box::new(self.clone()))
    }
}
impl Session for Host {
    fn start(
        &self,
        _: Cancellation,
        _: Request,
        completion: Completion,
    ) -> Result<Box<dyn Call>, RuntimeError> {
        if let Some(peer) = self.peer.lock().unwrap().take() {
            let _ = peer.heap_stats();
            assert_eq!(peer.start("read", Vec::new()).err().unwrap().code, "busy");
        }
        self.completions.send(completion).unwrap();
        Ok(Box::new(self.clone()))
    }
    fn shutdown(&self, _: Cancellation) -> Result<(), RuntimeError> {
        self.closing.send(()).unwrap();
        let _ = self.release.lock().unwrap().recv();
        Ok(())
    }
}

fn program() -> Arc<Program> {
    let bytes = support::image(json!({
        "type_table":{"nodes":[{"id":"bytes","kind":5,"elem":{"kind":3,"primitive":9}},{"id":"channel","kind":9,"direction":1,"elem":{"kind":3,"primitive":3}},{"id":"function","kind":10,"signature":{}}]},
        "constants":[{"id":"route","type":{"kind":3,"primitive":2},"value":"gate"},{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "globals":[{"id":"answer","type":{"kind":3,"primitive":3}}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"code":slot_code(json!([{"kind":10,"node":"function"},{"kind":3,"primitive":3}]), &[
                ("make_closure",json!({"function":"worker"}),json!({"outputs":[0]})),
                ("spawn",json!({"arg_count":0}),json!({"inputs":[[0,0]],"release":[0]})),
                ("const",json!({"constant":"answer"}),json!({"outputs":[1]})),
                ("return",json!({"result_count":1}),json!({"inputs":[[0,1]],"release":[1]}))])},
            {"id":"worker","code":slot_code(json!([{"kind":3,"primitive":2},{"kind":5,"node":"bytes"},{"kind":5,"node":"bytes"},{"kind":3,"primitive":2},{"kind":3,"primitive":3}]), &[
                ("const",json!({"constant":"route"}),json!({"outputs":[0]})),
                ("zero",json!({"type":{"kind":5,"node":"bytes"}}),json!({"outputs":[1]})),
                ("call_ffi",json!({"arg_count":2,"result_count":3}),json!({"inputs":[[0,0],[0,1]],"outputs":[2,3,4],"release":[0,1]})),
                ("pop",json!({}),json!({"inputs":[[0,4]],"release":[4]})),
                ("pop",json!({}),json!({"inputs":[[0,3]],"release":[3]})),
                ("pop",json!({}),json!({"inputs":[[0,2]],"release":[2]})),
                ("const",json!({"constant":"answer"}),json!({"outputs":[4]})),
                ("store_global",json!({"global":"answer"}),json!({"inputs":[[0,4]],"release":[4]}))])},
            {"id":"park","code":slot_code(json!([{"kind":9,"node":"channel"},{"kind":3,"primitive":3}]), &[
                ("zero",json!({"type":{"kind":9,"node":"channel"}}),json!({"outputs":[0]})),
                ("waitable_recv",json!({}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
                ("pop",json!({}),json!({"inputs":[[0,1]],"release":[1]}))])},
            {"id":"read","signature":{"results":[{"kind":3,"primitive":3}]},"code":slot_code(json!([{"kind":3,"primitive":3}]), &[
                ("load_global",json!({"global":"answer"}),json!({"outputs":[0]})),
                ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))])}
        ]
    }));
    let mut image: wire::ExecutionImage = serde_json::from_slice(&bytes).unwrap();
    for name in ["park", "read"] {
        image.entries.0.as_mut().unwrap().push(
            serde_json::from_value(json!({"name":name,"module_path":"test","function_id":name}))
                .unwrap(),
        );
    }
    image.hash.clear();
    image.hash = canonical_hash(&image).unwrap();
    Arc::new(Program::load(&canonical_json(&image).unwrap(), LoadLimits::default()).unwrap())
}

fn start(instance: &SharedInstance, entry: &str) -> Execution {
    loop {
        match instance.start(entry, Vec::new()) {
            Ok(execution) => return execution,
            Err(error) if error.code == "busy" => std::thread::yield_now(),
            Err(error) => panic!("{error}"),
        }
    }
}

#[test]
fn supervisor_preserves_other_scopes_and_host_callbacks_run_outside_control_lock() {
    let (completed, completions) = mpsc::channel();
    let (closing, closed) = mpsc::channel();
    let (release, released) = mpsc::channel();
    let host = Host {
        completions: completed,
        closing,
        release: Arc::new(Mutex::new(released)),
        peer: Arc::new(Mutex::new(None)),
    };
    let instance = program()
        .instantiate(InstanceOptions {
            bridge: Some(Arc::new(host.clone())),
            ..InstanceOptions::default()
        })
        .unwrap();
    *host.peer.lock().unwrap() = Some(instance.clone());
    loop {
        match instance.debugger().start_profile(1, 128) {
            Ok(()) => break,
            Err(error) if error.code == "busy" => std::thread::yield_now(),
            Err(error) => panic!("{error}"),
        }
    }
    let first = start(&instance, "default");
    let result = first.wait(&Cancellation::default()).unwrap();
    assert!(matches!(result.roots[0].data, HostData::Integer(42)));
    let completion = completions.recv_timeout(Duration::from_secs(5)).unwrap();
    assert!(!first.scope_settled());
    let pending = first.scope_stats();
    assert_eq!(pending.root_state, ExecutionState::Completed);
    assert_eq!((pending.tasks, pending.ffi_calls), (1, 1));
    assert!(!pending.done);
    assert_eq!(pending.started.generation, 1);
    assert_eq!(instance.stats().pending_ffi_calls, 1);
    let second = start(&instance, "park");
    second.cancel();
    assert_eq!(
        second.wait(&Cancellation::default()).unwrap_err().code,
        "canceled"
    );
    assert_eq!(second.state(), ExecutionState::Canceled);
    completion.complete(Reply::new(Vec::new(), None, None));
    first.wait_scope(&Cancellation::default()).unwrap();
    let completed = first.scope_stats();
    assert!(completed.done);
    assert!(completed.steps > pending.steps);
    assert_eq!(
        (completed.tasks, completed.timers, completed.ffi_calls),
        (0, 0, 0)
    );
    assert!(completed.error.is_none());
    let profile = first.profile();
    assert!(
        profile
            .samples
            .iter()
            .all(|sample| sample.scope == completed.id)
    );
    assert_eq!(
        profile
            .samples
            .iter()
            .map(|sample| sample.count)
            .sum::<u64>(),
        completed.steps
    );
    assert!(second.scope_stats().done);
    assert_eq!(second.scope_stats().root_state, ExecutionState::Canceled);
    let read = start(&instance, "read")
        .wait(&Cancellation::default())
        .unwrap();
    assert!(matches!(read.roots[0].data, HostData::Integer(42)));

    let canceled_wait = Cancellation::default();
    canceled_wait.cancel();
    assert_eq!(
        instance.shutdown(&canceled_wait).unwrap_err().code,
        "canceled"
    );
    closed.recv_timeout(Duration::from_secs(5)).unwrap();
    assert!(matches!(
        first.result().unwrap().roots[0].data,
        HostData::Integer(42)
    ));
    release.send(()).unwrap();
    instance.shutdown(&Cancellation::default()).unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
    assert_eq!(instance.stats().state, mini_go::InstanceState::Closed);
    assert_eq!(instance.stats().active_scopes, 0);
    assert_eq!(first.scope_stats().steps, completed.steps);
    assert_eq!(
        first
            .profile()
            .samples
            .iter()
            .map(|sample| sample.count)
            .sum::<u64>(),
        completed.steps
    );
    drop(instance);
    assert!(closed.try_recv().is_err());
}

#[test]
fn dropping_last_instance_handle_closes_with_execution_still_alive() {
    let (completed, completions) = mpsc::channel();
    let (closing, closed) = mpsc::channel();
    let (release, released) = mpsc::channel();
    let host = Host {
        completions: completed,
        closing,
        release: Arc::new(Mutex::new(released)),
        peer: Arc::new(Mutex::new(None)),
    };
    let instance = program()
        .instantiate(InstanceOptions {
            bridge: Some(Arc::new(host.clone())),
            ..InstanceOptions::default()
        })
        .unwrap();
    let other = instance.clone();
    let execution = start(&instance, "default");
    execution.wait(&Cancellation::default()).unwrap();
    let completion = completions.recv_timeout(Duration::from_secs(5)).unwrap();
    drop(instance);
    assert!(closed.try_recv().is_err());
    drop(other);
    closed.recv_timeout(Duration::from_secs(5)).unwrap();
    completion.complete(Reply::new(Vec::new(), None, None));
    release.send(()).unwrap();
    assert!(execution.wait_scope(&Cancellation::default()).is_err());
    assert!(matches!(
        execution.result().unwrap().roots[0].data,
        HostData::Integer(42)
    ));
}
