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
    let boolean = json!({"kind":3,"primitive":1});
    let integer = json!({"kind":3,"primitive":3});
    let callable = json!({"kind":10,"node":"action"});
    let channel = json!({"kind":9,"node":"channel"});
    let mut worker = Vec::new();
    for _ in 0..64 {
        worker.push(("zero", json!({"type":boolean}), json!({"outputs":[0]})));
        worker.push(("pop", json!({}), json!({"inputs":[[0,0]],"release":[0]})));
    }
    worker.extend([
        (
            "make_closure",
            json!({"function":"child"}),
            json!({"outputs":[1]}),
        ),
        (
            "spawn",
            json!({"arg_count":0}),
            json!({"inputs":[[0,1]],"release":[1]}),
        ),
        ("zero", json!({"type":channel}), json!({"outputs":[2]})),
        (
            "waitable_recv",
            json!({}),
            json!({"inputs":[[0,2]],"outputs":[3],"release":[2]}),
        ),
        ("pop", json!({}), json!({"inputs":[[0,3]],"release":[3]})),
    ]);
    let worker = support::slot_code(json!([boolean, callable, channel, integer]), &worker);
    let main = support::slot_code(
        json!([callable]),
        &[
            (
                "make_closure",
                json!({"function":"worker"}),
                json!({"outputs":[0]}),
            ),
            (
                "spawn",
                json!({"arg_count":0}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
            ("return", json!({"result_count":0}), json!({})),
        ],
    );
    let child = support::slot_code(
        json!([channel, integer]),
        &[
            ("zero", json!({"type":channel}), json!({"outputs":[0]})),
            (
                "waitable_recv",
                json!({}),
                json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
            ),
            ("pop", json!({}), json!({"inputs":[[0,1]],"release":[1]})),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"action","kind":10,"signature":{}},{"id":"channel","kind":9,"direction":1,"elem":integer}]},
        "functions":[
            {"id":"fn.Main","code":main},
            {"id":"worker","code":worker},
            {"id":"child","code":child}
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
        let code = support::slot_code(
            json!([{"kind":3,"primitive":3},{"kind":5,"node":"slice"}]),
            &[
                (
                    "const",
                    json!({"constant":"length"}),
                    json!({"outputs":[0]}),
                ),
                (
                    "make_slice",
                    json!({"type":{"kind":5,"node":"slice"}}),
                    json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
                ),
                ("pop", json!({}), json!({"inputs":[[0,1]],"release":[1]})),
            ],
        );
        let image = support::image(json!({
            "type_table":{"nodes":[{"id":"slice","kind":5,"elem":{"kind":3,"primitive":primitive}}]},
            "constants":[{"id":"length","type":{"kind":3,"primitive":3},"value":i64::MAX}],
            "functions":[{"id":"fn.Main","code":code}]
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
    let string = json!({"kind":3,"primitive":2});
    let code = support::slot_code(
        json!([string, string, string]),
        &[
            ("const", json!({"constant":"left"}), json!({"outputs":[0]})),
            ("const", json!({"constant":"right"}), json!({"outputs":[1]})),
            (
                "binary",
                json!({"operator":"+"}),
                json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,2]],"release":[2]}),
            ),
        ],
    );
    let image = support::image(json!({
        "constants":[{"id":"left","type":{"kind":3,"primitive":2},"value":"ab"},
            {"id":"right","type":{"kind":3,"primitive":2},"value":"cd"}],
        "functions":[{"id":"fn.Main","signature":{"results":[string]},"code":code}]
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
    let integer = json!({"kind":3,"primitive":3});
    let main = support::slot_code(
        json!([{"kind":10,"node":"action"},integer]),
        &[
            (
                "make_closure",
                json!({"function":"worker"}),
                json!({"outputs":[0]}),
            ),
            (
                "spawn",
                json!({"arg_count":0}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
            (
                "const",
                json!({"constant":"answer"}),
                json!({"outputs":[1]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,1]],"release":[1]}),
            ),
        ],
    );
    let worker = support::slot_code(
        json!([]),
        &[
            ("label", json!({"label":"loop"}), json!({})),
            ("jump", json!({"label":"loop"}), json!({})),
        ],
    );
    let quick = support::slot_code(
        json!([integer]),
        &[
            (
                "const",
                json!({"constant":"answer"}),
                json!({"outputs":[0]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
        ],
    );
    let failing_locals = (0..8)
        .map(|index| json!({"id":format!("slot{index}"),"type":{"kind":3,"primitive":3}}))
        .collect::<Vec<_>>();
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"action","kind":10,"signature":{}}]},
        "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer]},"code":main},
            {"id":"worker","code":worker},
            {"id":"quick","signature":{"results":[integer]},"code":quick},
            {"id":"large","locals":failing_locals,"code":{"descriptors":{}}}
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
    let string = json!({"kind":3,"primitive":2});
    let code = support::slot_code(
        json!([named, string, string, string]),
        &[
            ("load_local", json!({"local":"tag"}), json!({"outputs":[0]})),
            (
                "convert",
                json!({"type":string}),
                json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
            ),
            (
                "const",
                json!({"constant":"suffix"}),
                json!({"outputs":[2]}),
            ),
            (
                "binary",
                json!({"operator":"+"}),
                json!({"inputs":[[0,1],[0,2]],"outputs":[3],"release":[1,2]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,3]],"release":[3]}),
            ),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[
            {"id":"tag","kind":4,"identity":{"module_path":"test","decl_id":"Tag"},"underlying":{"kind":3,"primitive":2}}
        ]},
        "constants":[{"id":"suffix","type":{"kind":3,"primitive":2},"value":"!"}],
        "functions":[{"id":"fn.Main","signature":{"params":[{"type":named}],"results":[string]},"locals":[{"id":"tag","type":named}],"code":code}]
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
    let image = support::image(json!({"functions":[{"id":"fn.Main","code":{"descriptors":{}}}]}));
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
    let code = support::slot_code(
        json!([{"kind":6,"node":"outer"}]),
        &[
            (
                "load_local",
                json!({"local":"large"}),
                json!({"outputs":[0]}),
            ),
            ("pop", json!({}), json!({"inputs":[[0,0]],"release":[0]})),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[
            {"id":"inner","kind":6,"length":1000000,"elem":{"kind":3,"primitive":3}},
            {"id":"outer","kind":6,"length":1000000,"elem":{"kind":6,"node":"inner"}}
        ]},
        "functions":[{"id":"fn.Main","locals":[{"id":"large","type":{"kind":6,"node":"outer"}}],
            "code":code}]
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
    let code = support::slot_code(
        json!([{"kind":3,"primitive":3}]),
        &[
            (
                "const",
                json!({"constant":"answer"}),
                json!({"outputs":[0]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"large","kind":6,"length":1024,"elem":{"kind":3,"primitive":3}}]},
        "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},
            "locals":[{"id":"unused","type":{"kind":6,"node":"large"}}],"code":code}]
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
    let code = support::slot_code(
        json!([{"kind":3,"primitive":2}]),
        &[
            (
                "const",
                json!({"constant":"message"}),
                json!({"outputs":[0]}),
            ),
            ("panic", json!({}), json!({"inputs":[[0,0]],"release":[0]})),
        ],
    );
    let image = support::image(json!({
        "constants": [{"id": "message", "type": {"kind": 3, "primitive": 2}, "value": "failure"}],
        "functions": [{"id": "fn.Main", "code":code}]
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
