mod support;

use mini_go::{
    Executor, InstanceOptions,
    error::RuntimeError,
    ffi::{Bridge, Call, Cancellation, Completion, Reply, Request, Session},
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
};
use serde_json::json;
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicUsize, Ordering},
};

#[derive(Clone, Default)]
struct Host {
    capabilities: Vec<String>,
    opened: Arc<AtomicUsize>,
    completion: Arc<Mutex<Option<Completion>>>,
    canceled: Arc<AtomicUsize>,
    shutdown: Arc<AtomicUsize>,
}

impl Call for Host {
    fn cancel(&self) {
        self.canceled.fetch_add(1, Ordering::SeqCst);
    }
}
impl Bridge for Host {
    fn capabilities(&self) -> Vec<String> {
        self.capabilities.clone()
    }
    fn open(&self, _: Cancellation) -> Result<Box<dyn Session>, RuntimeError> {
        self.opened.fetch_add(1, Ordering::SeqCst);
        Ok(Box::new(self.clone()))
    }
}

#[test]
fn invalid_capability_declarations_fail_before_opening_a_session() {
    for capabilities in [vec![""], vec![" echo"], vec!["echo", "echo"]] {
        let host = Host {
            capabilities: capabilities.into_iter().map(str::to_owned).collect(),
            ..Host::default()
        };
        let result = Instance::with_bridge(program(), ExecutionLimits::default(), &host);
        assert_eq!(result.err().unwrap().code, "invalid_capabilities");
        assert_eq!(host.opened.load(Ordering::SeqCst), 0);
        assert_eq!(host.shutdown.load(Ordering::SeqCst), 0);
    }
}
impl Session for Host {
    fn start(
        &self,
        _: Cancellation,
        request: Request,
        completion: Completion,
    ) -> Result<Box<dyn Call>, RuntimeError> {
        assert_eq!(request.route, "echo");
        assert!(request.payload.is_empty());
        *self.completion.lock().unwrap() = Some(completion);
        Ok(Box::new(self.clone()))
    }
    fn shutdown(&self, _: Cancellation) -> Result<(), RuntimeError> {
        self.shutdown.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
}

fn program() -> Arc<Program> {
    let bytes = support::image(json!({
        "type_table": {"nodes": [{"id": "bytes", "kind": 5, "elem": {"kind": 3, "primitive": 9}}]},
        "constants": [{"id": "route", "type": {"kind": 3, "primitive": 2}, "value": "echo"}],
        "functions": [{"id": "fn.Main", "signature": {"results": [{"kind": 3, "primitive": 3}]}, "instructions": [
            {"op": "const", "payload": {"constant": "route"}},
            {"op": "zero", "payload": {"type": {"kind": 5, "node": "bytes"}}},
            {"op": "call_ffi", "payload": {"arg_count": 2, "result_count": 3}},
            {"op": "pop"}, {"op": "pop"}, {"op": "len"},
            {"op": "return", "payload": {"result_count": 1}}
        ]}]
    }));
    Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap())
}

#[test]
fn public_parallel_runtime_delivers_async_ffi_and_releases_its_scope() {
    for workers in [1, 2] {
        let host = Host::default();
        let executor = Executor::new(workers).unwrap();
        let instance = program()
            .instantiate(InstanceOptions {
                bridge: Some(Arc::new(host.clone())),
                parallelism: workers,
                executor: Some(executor.clone()),
                ..Default::default()
            })
            .unwrap();
        let execution = instance.start("default", Vec::new()).unwrap();
        let result = std::thread::scope(|scope| {
            let waiting = scope.spawn(|| execution.wait(&Cancellation::default()));
            let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
            let completion = loop {
                if let Some(completion) = host.completion.lock().unwrap().take() {
                    break completion;
                }
                assert!(
                    std::time::Instant::now() < deadline,
                    "public execution did not publish its FFI request"
                );
                std::thread::yield_now();
            };
            completion.complete(Reply::new(vec![1, 2, 3], None, None));
            waiting.join().unwrap().unwrap()
        });
        assert!(matches!(
            result.roots[0].data,
            mini_go::snapshot::HostData::Integer(3)
        ));
        execution.wait_scope(&Cancellation::default()).unwrap();
        assert_eq!(host.canceled.load(Ordering::SeqCst), 0);
        instance.shutdown(&Cancellation::default()).unwrap();
        assert_eq!(host.shutdown.load(Ordering::SeqCst), 1);
        executor.shutdown(&Cancellation::default()).unwrap();
    }
}

#[test]
fn pending_host_call_keeps_its_revision_until_delivery_or_cancellation() {
    let original = program();
    let target = Arc::new(Program::load(&support::image(json!({
        "type_table":{"nodes":[{"id":"bytes","kind":5,"elem":{"kind":3,"primitive":9}}]},
        "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"instructions":[
            {"op":"const","payload":{"constant":"answer"}},
            {"op":"return","payload":{"result_count":1}}
        ]}]
    })), LoadLimits::default()).unwrap());
    for cancel in [false, true] {
        let host = Host::default();
        let mut instance =
            Instance::with_bridge(original.clone(), ExecutionLimits::default(), &host).unwrap();
        instance.start("default", vec![]).unwrap();
        assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Pending);
        let plan = instance.prepare_patch(target.clone()).unwrap();
        instance.apply_patch(plan).unwrap();
        instance.collect_garbage().unwrap();
        assert_eq!(instance.retained_revisions().len(), 2);
        assert_eq!(instance.stats().pending_ffi_calls, 1);
        if cancel {
            instance.cancel().unwrap();
        }
        host.completion
            .lock()
            .unwrap()
            .take()
            .unwrap()
            .complete(Reply::new(vec![1, 2, 3], None, None));
        if !cancel {
            assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
            assert_eq!(instance.results()[0].integer().unwrap(), 3);
        }
        instance.collect_garbage().unwrap();
        assert_eq!(instance.retained_revisions(), [instance.revision()]);
        assert_eq!(instance.stats().pending_ffi_calls, 0);
        assert_eq!(host.canceled.load(Ordering::SeqCst), usize::from(cancel));
        instance.start("default", vec![]).unwrap();
        assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
        assert_eq!(instance.results()[0].integer().unwrap(), 42);
        instance.close().unwrap();
    }
}

#[test]
fn asynchronous_reply_is_consumed_by_owner_and_close_settles_session() {
    let host = Host::default();
    let mut instance = Instance::with_bridge(program(), ExecutionLimits::default(), &host).unwrap();
    instance.start("default", Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Pending);
    let discarded = Arc::new(AtomicUsize::new(0));
    let cleanup = discarded.clone();
    host.completion
        .lock()
        .unwrap()
        .take()
        .unwrap()
        .complete(Reply::new(
            vec![1, 2, 3],
            None,
            Some(Box::new(move || {
                cleanup.fetch_add(1, Ordering::SeqCst);
            })),
        ));
    assert!(instance.results().is_empty());
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(instance.results()[0].integer().unwrap(), 3);
    instance.close().unwrap();
    assert_eq!(discarded.load(Ordering::SeqCst), 0);
    assert_eq!(host.canceled.load(Ordering::SeqCst), 0);
    assert_eq!(host.shutdown.load(Ordering::SeqCst), 1);
    instance.close().unwrap();
    assert_eq!(host.shutdown.load(Ordering::SeqCst), 1);
}

#[test]
fn closing_blocked_execution_cancels_call_and_discards_late_reply() {
    let host = Host::default();
    let mut instance = Instance::with_bridge(program(), ExecutionLimits::default(), &host).unwrap();
    instance.start("default", Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Pending);
    instance.close().unwrap();
    let discarded = Arc::new(AtomicUsize::new(0));
    let cleanup = discarded.clone();
    host.completion
        .lock()
        .unwrap()
        .take()
        .unwrap()
        .complete(Reply::new(
            vec![1],
            None,
            Some(Box::new(move || {
                cleanup.fetch_add(1, Ordering::SeqCst);
            })),
        ));
    assert_eq!(host.canceled.load(Ordering::SeqCst), 1);
    assert_eq!(discarded.load(Ordering::SeqCst), 1);
    assert_eq!(instance.heap_stats().live_bytes, 0);
}

#[test]
fn result_allocation_failure_returns_host_status_and_preserves_instance() {
    let bytes = support::image(json!({
        "type_table":{"nodes":[{"id":"bytes","kind":5,"elem":{"kind":3,"primitive":9}}]},
        "constants":[{"id":"route","type":{"kind":3,"primitive":2},"value":"echo"}],
        "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"locals":[{"id":"status","type":{"kind":3,"primitive":3}}],"instructions":[
            {"op":"const","payload":{"constant":"route"}},{"op":"zero","payload":{"type":{"kind":5,"node":"bytes"}}},
            {"op":"call_ffi","payload":{"arg_count":2,"result_count":3}},
            {"op":"store_local","payload":{"local":"status"}},{"op":"pop"},{"op":"pop"},
            {"op":"load_local","payload":{"local":"status"}},{"op":"return","payload":{"result_count":1}}
        ]}]
    }));
    let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
    for limits in [
        ExecutionLimits {
            max_heap_bytes: 512,
            ..ExecutionLimits::default()
        },
        ExecutionLimits {
            max_allocated_bytes: 512,
            ..ExecutionLimits::default()
        },
    ] {
        let host = Host::default();
        let mut instance = Instance::with_bridge(program.clone(), limits, &host).unwrap();
        let discarded = Arc::new(AtomicUsize::new(0));
        let consumed = Arc::new(AtomicUsize::new(0));
        for (payload, expected) in [(vec![1; 256], 2), (vec![42], 0)] {
            instance.start("default", vec![]).unwrap();
            assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Pending);
            let cleanup = discarded.clone();
            let receipt = consumed.clone();
            host.completion.lock().unwrap().take().unwrap().complete(
                Reply::new(
                    payload,
                    None,
                    Some(Box::new(move || {
                        cleanup.fetch_add(1, Ordering::SeqCst);
                    })),
                )
                .on_consumed(move || {
                    receipt.fetch_add(1, Ordering::SeqCst);
                }),
            );
            assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
            assert_eq!(instance.results()[0].integer().unwrap(), expected);
        }
        assert_eq!(discarded.load(Ordering::SeqCst), 1);
        assert_eq!(consumed.load(Ordering::SeqCst), 1);
        instance.close().unwrap();
        assert_eq!(instance.heap_stats().live_bytes, 0);
    }
}

#[test]
fn construction_waits_for_initialization_and_cancellation_closes_its_session() {
    let bytes = support::image(json!({
        "type_table":{"nodes":[{"id":"bytes","kind":5,"elem":{"kind":3,"primitive":9}}]},
        "constants":[{"id":"route","type":{"kind":3,"primitive":2},"value":"echo"}],
        "globals":[{"id":"size","type":{"kind":3,"primitive":3}}],
        "functions":[
            {"id":"fn.init","instructions":[
                {"op":"const","payload":{"constant":"route"}},
                {"op":"zero","payload":{"type":{"kind":5,"node":"bytes"}}},
                {"op":"call_ffi","payload":{"arg_count":2,"result_count":3}},
                {"op":"pop"},{"op":"pop"},{"op":"len"},
                {"op":"store_global","payload":{"global":"size"}},
                {"op":"return","payload":{}}
            ]},
            {"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"instructions":[
                {"op":"load_global","payload":{"global":"size"}},
                {"op":"return","payload":{"result_count":1}}
            ]}
        ]
    }));
    let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
    for cancel in [false, true] {
        let host = Host::default();
        let cancellation = Cancellation::default();
        let options = mini_go::InstanceOptions {
            cancellation: cancellation.clone(),
            bridge: Some(Arc::new(host.clone())),
            ..Default::default()
        };
        let program = program.clone();
        let worker = std::thread::spawn(move || program.instantiate(options));
        let started = std::time::Instant::now();
        let completion = loop {
            if let Some(completion) = host.completion.lock().unwrap().take() {
                break completion;
            }
            if started.elapsed() > std::time::Duration::from_secs(5) {
                cancellation.cancel();
                let _ = worker.join();
                panic!("initializer did not reach the host call");
            }
            std::thread::yield_now();
        };
        assert!(!worker.is_finished());
        if cancel {
            cancellation.cancel();
            let result = worker.join().unwrap();
            assert_eq!(result.err().unwrap().code, "canceled");
            let discarded = Arc::new(AtomicUsize::new(0));
            let cleanup = discarded.clone();
            completion.complete(Reply::new(
                vec![1],
                None,
                Some(Box::new(move || {
                    cleanup.fetch_add(1, Ordering::SeqCst);
                })),
            ));
            assert_eq!(discarded.load(Ordering::SeqCst), 1);
            assert_eq!(host.canceled.load(Ordering::SeqCst), 1);
        } else {
            completion.complete(Reply::new(vec![1, 2, 3], None, None));
            let instance = worker.join().unwrap().unwrap();
            let execution = loop {
                match instance.start("default", vec![]) {
                    Ok(execution) => break execution,
                    Err(error) if error.code == "busy" => std::thread::yield_now(),
                    Err(error) => panic!("{error}"),
                }
            };
            let result = execution.wait(&Cancellation::default()).unwrap();
            assert!(matches!(
                result.roots[0].data,
                mini_go::snapshot::HostData::Integer(3)
            ));
            instance.shutdown(&Cancellation::default()).unwrap();
            assert_eq!(host.canceled.load(Ordering::SeqCst), 0);
        }
        assert_eq!(host.shutdown.load(Ordering::SeqCst), 1);
    }
}
