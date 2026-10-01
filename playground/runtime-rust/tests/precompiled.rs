use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    value::Value,
};
use serde_json::json;
use std::sync::Arc;
use support::slot_code;
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
            first.start("default", vec![Value::int(input)]).unwrap();
            while first.poll_steps(32).unwrap() == PollStatus::Running {}
            assert_eq!(first.results()[0].integer().unwrap(), result);
        }
        second.start("default", vec![Value::int(10)]).unwrap();
        while second.poll_steps(32).unwrap() == PollStatus::Running {}
        assert_eq!(second.results()[0].integer().unwrap(), expected[0]);
        first.close().unwrap();
        second.close().unwrap();
    }
}

#[test]
fn repeated_scalar_calls_reuse_slots_and_explicit_collection_releases_idle_storage() {
    let program = Arc::new(
        Program::load(
            &support::image(serde_json::json!({
                "functions": [{
                    "id": "fn.Main",
                    "signature": {"params": [{"type": {"kind": 3, "primitive": 3}}], "results": [{"kind": 3, "primitive": 3}]},
                    "locals": [{"id": "n", "type": {"kind": 3, "primitive": 3}}],
                    "code":slot_code(json!([{"kind":3,"primitive":3}]), &[
                        ("load_local",json!({"local":"n"}),json!({"outputs":[0]})),
                        ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))])
                }]
            })),
            LoadLimits::default(),
        )
        .unwrap(),
    );
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    let mut warmed = None;
    let collections = instance.heap_stats().collections;
    for _ in 0..32 {
        instance.start("default", vec![Value::int(10)]).unwrap();
        while instance.poll_steps(1).unwrap() == PollStatus::Running {}
        assert_eq!(instance.results()[0].integer().unwrap(), 10);
        let allocated = instance.heap_stats().total_allocated_bytes;
        if let Some(previous) = warmed {
            assert_eq!(allocated, previous);
        }
        warmed = Some(allocated);
        assert_eq!(instance.heap_stats().collections, collections);
    }
    assert!(instance.heap_stats().live_objects > 0);
    assert_eq!(instance.collect_garbage().unwrap().live_objects, 0);
    instance.start("default", vec![Value::int(20)]).unwrap();
    while instance.poll_steps(32).unwrap() == PollStatus::Running {}
    assert_eq!(instance.results()[0].integer().unwrap(), 20);
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
}
