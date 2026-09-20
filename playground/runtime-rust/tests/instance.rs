mod support;

use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
};
use serde_json::json;
use std::sync::Arc;

#[test]
fn entry_admission_counts_background_tasks_after_root_completion() {
    let mut worker = Vec::new();
    for _ in 0..64 {
        worker.push(json!({"op":"zero","payload":{"type":{"kind":3,"primitive":1}}}));
        worker.push(json!({"op":"pop"}));
    }
    worker.extend([
        json!({"op":"make_closure","payload":{"function":"child"}}),
        json!({"op":"spawn","payload":{"arg_count":0}}),
        json!({"op":"zero","payload":{"type":{"kind":9,"node":"channel"}}}),
        json!({"op":"waitable_recv"}),
        json!({"op":"pop"}),
    ]);
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"channel","kind":9,"direction":1,"elem":{"kind":3,"primitive":3}}]},
        "functions":[
            {"id":"fn.Main","instructions":[
                {"op":"make_closure","payload":{"function":"worker"}},
                {"op":"spawn","payload":{"arg_count":0}},
                {"op":"return","payload":{"result_count":0}}
            ]},
            {"id":"worker","instructions":worker},
            {"id":"child","instructions":[
                {"op":"zero","payload":{"type":{"kind":9,"node":"channel"}}},{"op":"waitable_recv"},{"op":"pop"}
            ]}
        ]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(
        program,
        ExecutionLimits {
            max_tasks: 2,
            ..ExecutionLimits::default()
        },
    )
    .unwrap();
    instance.start("default", vec![]).unwrap();
    assert_eq!(instance.poll_steps(1000).unwrap(), PollStatus::Ready);
    assert_eq!(instance.poll_background(1000).unwrap(), PollStatus::Pending);
    assert_eq!(instance.stats().blocked_tasks, 2);
    let before = instance.heap_stats();
    assert_eq!(
        instance.start("default", vec![]).unwrap_err().code,
        "task_limit"
    );
    assert_eq!(
        instance.heap_stats().total_allocated_bytes,
        before.total_allocated_bytes
    );
    assert_eq!(instance.stats().blocked_tasks, 2);
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
}

#[test]
fn backing_capacity_overflow_returns_a_resource_error() {
    for primitive in [3, 9] {
        let image = support::image(json!({
            "type_table":{"nodes":[{"id":"slice","kind":5,"elem":{"kind":3,"primitive":primitive}}]},
            "constants":[{"id":"length","type":{"kind":3,"primitive":3},"value":i64::MAX}],
            "functions":[{"id":"fn.Main","instructions":[
                {"op":"const","payload":{"constant":"length"}},
                {"op":"make_slice","payload":{"type":{"kind":5,"node":"slice"}}},
                {"op":"pop"}
            ]}]
        }));
        let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
        let mut instance = Instance::new(
            program,
            ExecutionLimits {
                max_heap_bytes: u64::MAX,
                max_sequence_elements: usize::MAX,
                ..ExecutionLimits::default()
            },
        )
        .unwrap();
        instance.start("default", vec![]).unwrap();
        assert_eq!(instance.poll_steps(2).unwrap_err().code, "allocation_limit");
        assert_eq!(instance.heap_stats().live_bytes, 0);
        instance.close().unwrap();
    }
}

#[test]
fn concatenation_checks_the_complete_string_before_allocating() {
    let image = support::image(json!({
        "constants":[{"id":"left","type":{"kind":3,"primitive":2},"value":"ab"},
            {"id":"right","type":{"kind":3,"primitive":2},"value":"cd"}],
        "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":2}]},"instructions":[
            {"op":"const","payload":{"constant":"left"}},
            {"op":"const","payload":{"constant":"right"}},
            {"op":"binary","payload":{"operator":"+"}},
            {"op":"return","payload":{"result_count":1}}
        ]}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    for limit in [3, 4] {
        let mut instance = Instance::new(
            program.clone(),
            ExecutionLimits {
                max_string_bytes: limit,
                ..ExecutionLimits::default()
            },
        )
        .unwrap();
        instance.start("default", vec![]).unwrap();
        if limit == 4 {
            assert_eq!(instance.poll_steps(4).unwrap(), PollStatus::Ready);
            let snapshot = instance.snapshot_results(Default::default()).unwrap();
            assert_eq!(snapshot.bytes(&snapshot.roots[0]).unwrap(), b"abcd");
        } else {
            assert_eq!(instance.poll_steps(4).unwrap_err().code, "string_limit");
            assert!(instance.results().is_empty());
        }
        instance.close().unwrap();
        assert_eq!(instance.heap_stats().live_bytes, 0);
    }
}

#[test]
fn surviving_tasks_keep_their_scope_budget_across_new_invocations() {
    let failing_locals = (0..8)
        .map(|index| json!({"id":format!("slot{index}"),"type":{"kind":3,"primitive":3}}))
        .collect::<Vec<_>>();
    let image = support::image(json!({
        "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"instructions":[
                {"op":"make_closure","payload":{"function":"worker"}},{"op":"spawn","payload":{"arg_count":0}},
                {"op":"const","payload":{"constant":"answer"}},{"op":"return","payload":{"result_count":1}}
            ]},
            {"id":"worker","instructions":[{"op":"label","payload":{"label":"loop"}},{"op":"jump","payload":{"label":"loop"}}]},
            {"id":"quick","signature":{"results":[{"kind":3,"primitive":3}]},"instructions":[{"op":"const","payload":{"constant":"answer"}},{"op":"return","payload":{"result_count":1}}]},
            {"id":"large","locals":failing_locals}
        ]
    }));
    let mut image: mini_go::contract_generated::ExecutionImage =
        serde_json::from_slice(&image).unwrap();
    image.entries.0.as_mut().unwrap().push(
        serde_json::from_value(json!({"name":"quick","module_path":"test","function_id":"quick"}))
            .unwrap(),
    );
    image.entries.0.as_mut().unwrap().push(
        serde_json::from_value(json!({"name":"large","module_path":"test","function_id":"large"}))
            .unwrap(),
    );
    image.hash.clear();
    image.hash = mini_go::contract::canonical_hash(&image).unwrap();
    let program = Arc::new(
        Program::load(
            &mini_go::contract::canonical_json(&image).unwrap(),
            LoadLimits::default(),
        )
        .unwrap(),
    );
    let mut instance = Instance::new(
        program,
        ExecutionLimits {
            max_steps: 100,
            max_heap_bytes: 1024,
            ..ExecutionLimits::default()
        },
    )
    .unwrap();
    for entry in ["default", "quick"] {
        instance.start(entry, vec![]).unwrap();
        assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
        assert_eq!(instance.results()[0].integer().unwrap(), 42);
        let before = instance.heap_stats();
        assert_eq!(
            instance
                .start("quick", vec![mini_go::value::Value::int(1)])
                .unwrap_err()
                .code,
            "invalid_call"
        );
        assert_eq!(
            instance.start("large", vec![]).unwrap_err().code,
            "allocation_limit"
        );
        assert_eq!(instance.results()[0].integer().unwrap(), 42);
        let after = instance.heap_stats();
        assert_eq!(after.live_objects, before.live_objects);
        assert_eq!(after.live_bytes, before.live_bytes);
        assert!(after.total_allocated_bytes >= before.total_allocated_bytes);
        assert!(after.peak_bytes <= 1024);
    }
    // The child owns a full quantum, independent of the parent's two spawn
    // instructions. Main executes four instructions and quick executes two.
    assert_eq!(instance.steps(), 64 + 4 + 2);
    assert_eq!(instance.poll_background(100).unwrap(), PollStatus::Ready);
    assert_eq!(instance.steps(), 102);
    assert_eq!(instance.heap_stats().live_objects, 0);
    instance.start("quick", vec![]).unwrap();
    assert_eq!(instance.poll_steps(2).unwrap(), PollStatus::Ready);
    assert_eq!(instance.results()[0].integer().unwrap(), 42);
    instance.close().unwrap();
}

#[test]
fn named_string_conversion_preserves_binary_bytes_and_detached_results() {
    let named = json!({"kind":4,"named":{"module_path":"test","decl_id":"Tag"}});
    let image = support::image(json!({
        "type_table":{"nodes":[
            {"id":"tag","kind":4,"identity":{"module_path":"test","decl_id":"Tag"},"underlying":{"kind":3,"primitive":2}}
        ]},
        "constants":[{"id":"suffix","type":{"kind":3,"primitive":2},"value":"!"}],
        "functions":[{"id":"fn.Main","signature":{"params":[{"type":named}],"results":[{"kind":3,"primitive":2}]},"locals":[{"id":"tag","type":named}],"instructions":[
            {"op":"load_local","payload":{"local":"tag"}},
            {"op":"convert","payload":{"type":{"kind":3,"primitive":2}}},
            {"op":"const","payload":{"constant":"suffix"}},
            {"op":"binary","payload":{"operator":"+"}},
            {"op":"return","payload":{"result_count":1}}
        ]}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    let argument = mini_go::value::Value::string(vec![255, 0, 128]);
    instance.start("default", vec![argument.clone()]).unwrap();
    assert_eq!(instance.poll_steps(10).unwrap(), PollStatus::Ready);
    let result = instance.snapshot_results(Default::default()).unwrap();
    instance.close().unwrap();
    assert_eq!(
        result.bytes(&result.roots[0]).unwrap(),
        vec![255, 0, 128, b'!']
    );
    if let mini_go::value::Data::String(bytes) = argument.data() {
        assert_eq!(&**bytes, &[255, 0, 128]);
    } else {
        panic!("expected original string");
    }
}

#[test]
fn invalid_binary_entry_reclaims_its_argument_and_closed_start_allocates_nothing() {
    let image = support::image(json!({"functions":[{"id":"fn.Main"}]}));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    assert!(instance.start_bytes("missing", &[0, 255]).is_err());
    assert_eq!(instance.heap_stats().live_bytes, 0);
    instance.close().unwrap();
    let allocated = instance.heap_stats().total_allocated_bytes;
    assert_eq!(
        instance.start_bytes("default", &[1, 2]).unwrap_err().code,
        "closed"
    );
    assert_eq!(instance.heap_stats().total_allocated_bytes, allocated);
    assert_eq!(instance.heap_stats().live_objects, 0);
}

#[test]
fn nested_array_layout_fails_within_budget_before_materialization() {
    let image = support::image(json!({
        "type_table":{"nodes":[
            {"id":"inner","kind":6,"length":1000000,"elem":{"kind":3,"primitive":3}},
            {"id":"outer","kind":6,"length":1000000,"elem":{"kind":6,"node":"inner"}}
        ]},
        "functions":[{"id":"fn.Main","locals":[{"id":"large","type":{"kind":6,"node":"outer"}}],
            "instructions":[{"op":"load_local","payload":{"local":"large"}},{"op":"pop"}]}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(
        program,
        ExecutionLimits {
            max_heap_bytes: 1024,
            ..ExecutionLimits::default()
        },
    )
    .unwrap();
    instance.start("default", Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(1).unwrap_err().code, "allocation_limit");
    assert_eq!(instance.heap_stats().live_bytes, 0);
    assert!(instance.heap_stats().peak_bytes <= 1024);
    instance.close().unwrap();
}

#[test]
fn unused_array_slots_do_not_materialize_their_layout() {
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"large","kind":6,"length":1024,"elem":{"kind":3,"primitive":3}}]},
        "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},
            "locals":[{"id":"unused","type":{"kind":6,"node":"large"}}],"instructions":[
                {"op":"const","payload":{"constant":"answer"}},{"op":"return","payload":{"result_count":1}}
            ]}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(
        program,
        ExecutionLimits {
            max_heap_bytes: 1024,
            ..ExecutionLimits::default()
        },
    )
    .unwrap();
    instance.start("default", vec![]).unwrap();
    assert_eq!(instance.poll_steps(2).unwrap(), PollStatus::Ready);
    assert_eq!(instance.results()[0].integer().unwrap(), 42);
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_bytes, 0);
}

#[test]
fn unrecovered_guest_panic_faults_the_instance_and_releases_frames() {
    let image = support::image(json!({
        "constants": [{"id": "message", "type": {"kind": 3, "primitive": 2}, "value": "failure"}],
        "functions": [{"id": "fn.Main", "instructions": [
            {"op": "const", "payload": {"constant": "message"}}, {"op": "panic"}
        ]}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let entry = program.image().entries[0].name.clone();
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.start(&entry, Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Running);
    let error = instance.poll_steps(2).unwrap_err();
    assert_eq!(error.code, "panic");
    assert!(error.message.contains("failure"));
    assert_eq!(instance.heap_stats().live_bytes, 0);
    assert_eq!(
        instance.start(&entry, Vec::new()).unwrap_err().code,
        "faulted"
    );
    instance.close().unwrap();
    instance.close().unwrap();
}
