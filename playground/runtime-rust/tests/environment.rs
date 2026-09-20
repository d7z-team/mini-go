mod support;

use mini_go::{
    Executor, InstanceOptions,
    environment::{Clock, Entropy},
    error::RuntimeError,
    execution::ExecutionState,
    ffi::Cancellation,
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    value::Data,
};
use serde_json::json;
use std::sync::{
    Arc,
    atomic::{AtomicU64, Ordering},
};

#[derive(Default)]
struct ManualClock(AtomicU64);
impl Clock for ManualClock {
    fn unix_time(&self) -> (i64, u32) {
        (0, self.0.load(Ordering::SeqCst) as u32)
    }
    fn monotonic_ns(&self) -> u64 {
        self.0.load(Ordering::SeqCst)
    }
}

struct PartialEntropy;
impl Entropy for PartialEntropy {
    fn read(&self, bytes: &mut [u8]) -> (usize, Option<RuntimeError>) {
        bytes[..2].copy_from_slice(&[20, 20]);
        (2, Some(RuntimeError::new("entropy", "test", "short read")))
    }
}

#[test]
fn timer_event_resumes_a_blocked_task_only_after_deadline() {
    let image = support::image(json!({
        "type_table": {"nodes": [{"id": "channel", "kind": 9, "direction": 1, "elem": {"kind": 3, "primitive": 1}}]},
        "constants": [
            {"id": "capacity", "type": {"kind": 3, "primitive": 3}, "value": 1},
            {"id": "delay", "type": {"kind": 3, "primitive": 7}, "value": 10},
            {"id": "result", "type": {"kind": 3, "primitive": 3}, "value": 42}
        ],
        "functions": [{"id": "fn.Main", "signature": {"results": [{"kind": 3, "primitive": 3}]}, "locals": [{"id": "channel", "type": {"kind": 9, "node": "channel"}}], "instructions": [
            {"op": "const", "payload": {"constant": "capacity"}}, {"op": "make_waitable", "payload": {"type": {"kind": 9, "node": "channel"}}},
            {"op": "store_local", "payload": {"local": "channel"}}, {"op": "load_local", "payload": {"local": "channel"}},
            {"op": "const", "payload": {"constant": "delay"}}, {"op": "zero", "payload": {"type": {"kind": 3, "primitive": 7}}},
            {"op": "call_intrinsic", "payload": {"id": "time.timer_start", "arg_count": 3}},
            {"op": "load_local", "payload": {"local": "channel"}}, {"op": "waitable_recv"}, {"op": "pop"},
            {"op": "const", "payload": {"constant": "result"}}, {"op": "return", "payload": {"result_count": 1}}
        ]}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    for workers in [1, 2] {
        let clock = Arc::new(ManualClock::default());
        let executor = Executor::new(workers).unwrap();
        let instance = program
            .clone()
            .instantiate(InstanceOptions {
                clock: clock.clone(),
                parallelism: workers,
                executor: Some(executor.clone()),
                ..Default::default()
            })
            .unwrap();
        let execution = instance.start("default", Vec::new()).unwrap();
        drive_until_state(&execution, ExecutionState::Pending);
        clock.0.store(9, Ordering::SeqCst);
        drive_until_state(&execution, ExecutionState::Pending);
        clock.0.store(10, Ordering::SeqCst);
        let result = execution.wait(&Cancellation::default()).unwrap();
        assert!(matches!(
            result.roots[0].data,
            mini_go::snapshot::HostData::Integer(42)
        ));
        execution.wait_scope(&Cancellation::default()).unwrap();
        instance.collect_garbage().unwrap();
        assert_eq!(instance.heap_stats().live_objects, 0);
        instance.shutdown(&Cancellation::default()).unwrap();
        executor.shutdown(&Cancellation::default()).unwrap();
    }
}

fn drive_until_state(execution: &mini_go::Execution, expected: ExecutionState) {
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
    loop {
        match execution.poll_steps(100) {
            Ok((state, _)) if state == expected => return,
            Ok((ExecutionState::Running, _)) => {}
            Err(RuntimeError { code: "busy", .. }) => {}
            Ok((state, _)) => panic!("execution reached {state:?}, expected {expected:?}"),
            Err(error) => panic!("{error}"),
        }
        assert!(
            std::time::Instant::now() < deadline,
            "execution did not reach {expected:?}"
        );
        std::thread::yield_now();
    }
}

#[test]
fn partial_entropy_failure_preserves_written_bytes_and_count() {
    let image = support::image(json!({
        "type_table": {"nodes": [{"id": "bytes", "kind": 5, "elem": {"kind": 3, "primitive": 9}}]},
        "constants": [
            {"id": "length", "type": {"kind": 3, "primitive": 3}, "value": 4},
            {"id": "one", "type": {"kind": 3, "primitive": 3}, "value": 1}
        ],
        "functions": [{"id": "fn.Main", "signature": {"results": [{"kind": 3, "primitive": 3}, {"kind": 3, "primitive": 1}]},
        "locals": [{"id": "bytes", "type": {"kind": 5, "node": "bytes"}}, {"id": "ok", "type": {"kind": 3, "primitive": 1}}], "instructions": [
            {"op": "const", "payload": {"constant": "length"}}, {"op": "make_slice", "payload": {"type": {"kind": 5, "node": "bytes"}}},
            {"op": "store_local", "payload": {"local": "bytes"}}, {"op": "load_local", "payload": {"local": "bytes"}},
            {"op": "call_intrinsic", "payload": {"id": "crypto.rand.read", "arg_count": 1, "result_count": 3}},
            {"op": "store_local", "payload": {"local": "ok"}}, {"op": "pop"},
            {"op": "load_local", "payload": {"local": "bytes"}}, {"op": "zero", "payload": {"type": {"kind": 3, "primitive": 3}}}, {"op": "load_index"}, {"op": "convert", "payload": {"type": {"kind": 3, "primitive": 3}}}, {"op": "binary", "payload": {"operator": "+"}},
            {"op": "load_local", "payload": {"local": "bytes"}}, {"op": "const", "payload": {"constant": "one"}}, {"op": "load_index"}, {"op": "convert", "payload": {"type": {"kind": 3, "primitive": 3}}}, {"op": "binary", "payload": {"operator": "+"}},
            {"op": "load_local", "payload": {"local": "ok"}}, {"op": "return", "payload": {"result_count": 2}}
        ]}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance
        .set_environment(Arc::new(ManualClock::default()), Arc::new(PartialEntropy))
        .unwrap();
    instance.start("default", Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(instance.results()[0].integer().unwrap(), 42);
    assert!(matches!(instance.results()[1].data(), Data::Bool(false)));
    instance.collect_garbage().unwrap();
    assert_eq!(instance.heap_stats().live_bytes, 0);
}

#[test]
fn stopping_a_timer_closes_its_signal_and_releases_waiters() {
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"signal","kind":9,"direction":1,"elem":{"kind":3,"primitive":1}}]},
        "constants":[{"id":"capacity","type":{"kind":3,"primitive":3},"value":1},{"id":"delay","type":{"kind":3,"primitive":7},"value":30000000000_i64}],
        "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":1}]},"locals":[{"id":"signal","type":{"kind":9,"node":"signal"}}],"instructions":[
            {"op":"const","payload":{"constant":"capacity"}}, {"op":"make_waitable","payload":{"type":{"kind":9,"node":"signal"}}},
            {"op":"store_local","payload":{"local":"signal"}}, {"op":"load_local","payload":{"local":"signal"}},
            {"op":"const","payload":{"constant":"delay"}}, {"op":"zero","payload":{"type":{"kind":3,"primitive":7}}},
            {"op":"call_intrinsic","payload":{"id":"time.timer_start","arg_count":3}},
            {"op":"load_local","payload":{"local":"signal"}}, {"op":"call_intrinsic","payload":{"id":"time.timer_stop","arg_count":1,"result_count":1}}, {"op":"pop"},
            {"op":"load_local","payload":{"local":"signal"}}, {"op":"waitable_recv"}, {"op":"return","payload":{"result_count":1}}
        ]}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.start("default", vec![]).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert!(matches!(instance.results()[0].data(), Data::Bool(false)));
    assert_eq!(instance.stats().timers, 0);
    assert_eq!(instance.stats().blocked_tasks, 0);
}
