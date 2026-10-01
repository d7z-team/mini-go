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
use support::slot_code;

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

#[test]
fn computing_with_a_timer_reads_time_at_bounded_owner_boundaries() {
    struct CountingClock(AtomicU64);
    impl Clock for CountingClock {
        fn unix_time(&self) -> (i64, u32) {
            (0, 0)
        }
        fn monotonic_ns(&self) -> u64 {
            self.0.fetch_add(1, Ordering::Relaxed);
            0
        }
    }
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"channel","kind":9,"direction":1,"elem":{"kind":3,"primitive":1}}]},
        "constants":[{"id":"capacity","type":{"kind":3,"primitive":3},"value":1},
            {"id":"delay","type":{"kind":3,"primitive":7},"value":1000000}],
        "functions":[{"id":"fn.Main","code":slot_code(json!([{"kind":3,"primitive":3},{"kind":9,"node":"channel"},{"kind":3,"primitive":7},{"kind":3,"primitive":7}]), &[
            ("const",json!({"constant":"capacity"}),json!({"outputs":[0]})),
            ("make_waitable",json!({"type":{"kind":9,"node":"channel"}}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
            ("const",json!({"constant":"delay"}),json!({"outputs":[2]})),
            ("zero",json!({"type":{"kind":3,"primitive":7}}),json!({"outputs":[3]})),
            ("call_intrinsic",json!({"id":"time.timer_start","arg_count":3}),json!({"inputs":[[0,1],[0,2],[0,3]],"release":[1,2,3]})),
            ("label",json!({"label":"loop"}),json!({})),
            ("jump",json!({"label":"loop"}),json!({}))])}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let clock = Arc::new(CountingClock(AtomicU64::new(0)));
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    vm.set_environment(clock.clone(), Arc::new(mini_go::environment::SystemEntropy))
        .unwrap();
    vm.start("default", vec![]).unwrap();
    assert_eq!(vm.poll_steps(10000).unwrap(), PollStatus::Running);
    assert_eq!(vm.steps(), 10000);
    let reads = clock.0.load(Ordering::Relaxed);
    assert!(
        reads > 1 && reads < 200,
        "clock reads for 10000 steps: {reads}"
    );
    assert_eq!(vm.stats().timers, 1);
    vm.cancel().unwrap();
    assert_eq!(vm.stats().timers, 0);
    vm.close().unwrap();
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
        "functions": [{"id": "fn.Main", "signature": {"results": [{"kind": 3, "primitive": 3}]}, "locals": [{"id": "channel", "type": {"kind": 9, "node": "channel"}}],
        "code":slot_code(json!([{"kind":3,"primitive":3},{"kind":9,"node":"channel"},{"kind":3,"primitive":7},{"kind":3,"primitive":7},{"kind":3,"primitive":1}]), &[
            ("const",json!({"constant":"capacity"}),json!({"outputs":[0]})),
            ("make_waitable",json!({"type":{"kind":9,"node":"channel"}}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
            ("store_local",json!({"local":"channel"}),json!({"inputs":[[0,1]],"release":[1]})),
            ("load_local",json!({"local":"channel"}),json!({"outputs":[1]})),
            ("const",json!({"constant":"delay"}),json!({"outputs":[2]})),
            ("zero",json!({"type":{"kind":3,"primitive":7}}),json!({"outputs":[3]})),
            ("call_intrinsic",json!({"id":"time.timer_start","arg_count":3}),json!({"inputs":[[0,1],[0,2],[0,3]],"release":[1,2,3]})),
            ("load_local",json!({"local":"channel"}),json!({"outputs":[1]})),
            ("waitable_recv",json!({}),json!({"inputs":[[0,1]],"outputs":[4],"release":[1]})),
            ("pop",json!({}),json!({"inputs":[[0,4]],"release":[4]})),
            ("const",json!({"constant":"result"}),json!({"outputs":[0]})),
            ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))])}]
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
        "locals": [{"id": "bytes", "type": {"kind": 5, "node": "bytes"}}, {"id": "ok", "type": {"kind": 3, "primitive": 1}}],
        "code":slot_code(json!([{"kind":3,"primitive":3},{"kind":5,"node":"bytes"},{"kind":3,"primitive":2},{"kind":3,"primitive":1},{"kind":3,"primitive":3},{"kind":3,"primitive":9},{"kind":3,"primitive":3}]), &[
            ("const",json!({"constant":"length"}),json!({"outputs":[0]})),
            ("make_slice",json!({"type":{"kind":5,"node":"bytes"}}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
            ("store_local",json!({"local":"bytes"}),json!({"inputs":[[0,1]],"release":[1]})),
            ("load_local",json!({"local":"bytes"}),json!({"outputs":[1]})),
            ("call_intrinsic",json!({"id":"crypto.rand.read","arg_count":1,"result_count":3}),json!({"inputs":[[0,1]],"outputs":[0,2,3],"release":[1]})),
            ("store_local",json!({"local":"ok"}),json!({"inputs":[[0,3]],"release":[3]})),
            ("pop",json!({}),json!({"inputs":[[0,2]],"release":[2]})),
            ("load_local",json!({"local":"bytes"}),json!({"outputs":[1]})),
            ("zero",json!({"type":{"kind":3,"primitive":3}}),json!({"outputs":[4]})),
            ("load_index",json!({}),json!({"inputs":[[0,1],[0,4]],"outputs":[5],"release":[1,4]})),
            ("convert",json!({"type":{"kind":3,"primitive":3}}),json!({"inputs":[[0,5]],"outputs":[4],"release":[5]})),
            ("binary",json!({"operator":"+"}),json!({"inputs":[[0,0],[0,4]],"outputs":[6],"release":[0,4]})),
            ("load_local",json!({"local":"bytes"}),json!({"outputs":[1]})),
            ("const",json!({"constant":"one"}),json!({"outputs":[4]})),
            ("load_index",json!({}),json!({"inputs":[[0,1],[0,4]],"outputs":[5],"release":[1,4]})),
            ("convert",json!({"type":{"kind":3,"primitive":3}}),json!({"inputs":[[0,5]],"outputs":[4],"release":[5]})),
            ("binary",json!({"operator":"+"}),json!({"inputs":[[0,6],[0,4]],"outputs":[0],"release":[6,4]})),
            ("load_local",json!({"local":"ok"}),json!({"outputs":[3]})),
            ("return",json!({"result_count":2}),json!({"inputs":[[0,0],[0,3]],"release":[0,3]}))])}]
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
        "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":1}]},"locals":[{"id":"signal","type":{"kind":9,"node":"signal"}}],
        "code":slot_code(json!([{"kind":3,"primitive":3},{"kind":9,"node":"signal"},{"kind":3,"primitive":7},{"kind":3,"primitive":7},{"kind":3,"primitive":1}]), &[
            ("const",json!({"constant":"capacity"}),json!({"outputs":[0]})),
            ("make_waitable",json!({"type":{"kind":9,"node":"signal"}}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
            ("store_local",json!({"local":"signal"}),json!({"inputs":[[0,1]],"release":[1]})),
            ("load_local",json!({"local":"signal"}),json!({"outputs":[1]})),
            ("const",json!({"constant":"delay"}),json!({"outputs":[2]})),
            ("zero",json!({"type":{"kind":3,"primitive":7}}),json!({"outputs":[3]})),
            ("call_intrinsic",json!({"id":"time.timer_start","arg_count":3}),json!({"inputs":[[0,1],[0,2],[0,3]],"release":[1,2,3]})),
            ("load_local",json!({"local":"signal"}),json!({"outputs":[1]})),
            ("call_intrinsic",json!({"id":"time.timer_stop","arg_count":1,"result_count":1}),json!({"inputs":[[0,1]],"outputs":[4],"release":[1]})),
            ("pop",json!({}),json!({"inputs":[[0,4]],"release":[4]})),
            ("load_local",json!({"local":"signal"}),json!({"outputs":[1]})),
            ("waitable_recv",json!({}),json!({"inputs":[[0,1]],"outputs":[4],"release":[1]})),
            ("return",json!({"result_count":1}),json!({"inputs":[[0,4]],"release":[4]}))])}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.start("default", vec![]).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert!(matches!(instance.results()[0].data(), Data::Bool(false)));
    assert_eq!(instance.stats().timers, 0);
    assert_eq!(instance.stats().blocked_tasks, 0);
}
