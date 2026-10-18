use mini_go::{
    HostValue,
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
};
use serde_json::json;
use std::sync::Arc;
use support::micro_code;
mod support;

#[test]
fn precompiled_blocks_reuse_code_and_keep_instance_state_isolated() {
    for (image, expected) in [
        (
            &include_bytes!("../examples/blocks/arithmetic.json")[..],
            [55, 210],
        ),
        (
            &include_bytes!("../examples/blocks/closure.json")[..],
            [23, 43],
        ),
        (
            &include_bytes!("../examples/blocks/stateful.json")[..],
            [10, 30],
        ),
    ] {
        let program = Arc::new(Program::load(image, LoadLimits::default()).unwrap());
        let mut first = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
        let mut second = Instance::new(program, ExecutionLimits::default()).unwrap();
        for (input, result) in [10, 20].into_iter().zip(expected) {
            first.start("default", &[HostValue::int(input)]).unwrap();
            while first.poll_steps(32).unwrap() == PollStatus::Running {}
            assert_eq!(
                first.snapshot_results(Default::default()).unwrap().roots[0]
                    .integer()
                    .unwrap(),
                result
            );
        }
        second.start("default", &[HostValue::int(10)]).unwrap();
        while second.poll_steps(32).unwrap() == PollStatus::Running {}
        assert_eq!(
            second.snapshot_results(Default::default()).unwrap().roots[0]
                .integer()
                .unwrap(),
            expected[0]
        );
        first.close().unwrap();
        second.close().unwrap();
    }
}

#[test]
fn repeated_scalar_calls_release_activation_capacity_and_keep_code_ready() {
    let program = Arc::new(
        Program::load(
            &support::image(serde_json::json!({
                "functions": [{
                    "id": "fn.Main",
                    "signature": {"params": [{"type": {"kind": 3, "primitive": 3}}], "results": [{"kind": 3, "primitive": 3}]},
                    "locals": [{"id": "n", "type": {"kind": 3, "primitive": 3}}],
                    "code":micro_code(json!([{"kind":3,"primitive":3}]), &[
                        ("load_local",json!({"local":"n"}),json!({"outputs":[0]})),
                        ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))])
                }]
            })),
            LoadLimits::default(),
        )
        .unwrap(),
    );
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    let idle = instance.heap_stats();
    let collections = instance.heap_stats().collections;
    for _ in 0..32 {
        let before = instance.heap_stats().total_allocated_bytes;
        instance.start("default", &[HostValue::int(10)]).unwrap();
        while instance.poll_steps(1).unwrap() == PollStatus::Running {}
        assert_eq!(
            instance.snapshot_results(Default::default()).unwrap().roots[0]
                .integer()
                .unwrap(),
            10
        );
        // Each activation owns a task, a frame, one local, one temporary,
        // and one result lane, even when the physical buffers are reused.
        let charge = mini_go::contract_generated::STORAGE_CONTROL_RECORD
            + mini_go::contract_generated::STORAGE_FRAME_HEADER
            + 3 * mini_go::contract_generated::STORAGE_SCALAR;
        assert_eq!(instance.heap_stats().total_allocated_bytes - before, charge);
        assert_eq!(instance.heap_stats().live_bytes, idle.live_bytes);
        assert_eq!(instance.heap_stats().collections, collections);
    }
    assert_eq!(
        instance.collect_garbage().unwrap().live_objects,
        idle.live_objects
    );
    instance.start("default", &[HostValue::int(20)]).unwrap();
    while instance.poll_steps(32).unwrap() == PollStatus::Running {}
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        20
    );
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
}
