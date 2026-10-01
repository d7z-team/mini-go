mod support;

use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
};
use serde_json::json;
use std::sync::Arc;

#[test]
fn frame_owned_map_iterator_is_traced_and_released_on_completion_cancel_and_close() {
    let integer = json!({"kind":3,"primitive":3});
    let map = json!({"kind":7,"node":"map"});
    let code = support::slot_code(
        json!([integer,integer,map,integer,integer,{"kind":3,"primitive":1}]),
        &[
            ("const", json!({"constant":"key"}), json!({"outputs":[0]})),
            (
                "const",
                json!({"constant":"answer"}),
                json!({"outputs":[1]}),
            ),
            (
                "make_map",
                json!({"type":map,"entry_count":1}),
                json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]}),
            ),
            (
                "map_iter_init",
                json!({"local":"cursor"}),
                json!({"inputs":[[0,2]],"release":[2]}),
            ),
            (
                "map_iter_next",
                json!({"local":"cursor"}),
                json!({"outputs":[3,4,5]}),
            ),
            ("pop", json!({}), json!({"inputs":[[0,5]],"release":[5]})),
            (
                "store_local",
                json!({"local":"answer"}),
                json!({"inputs":[[0,4]],"release":[4]}),
            ),
            ("pop", json!({}), json!({"inputs":[[0,3]],"release":[3]})),
            ("map_iter_close", json!({"local":"cursor"}), json!({})),
            (
                "load_local",
                json!({"local":"answer"}),
                json!({"outputs":[0]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
        ],
    );
    let program = Arc::new(Program::load(&support::image(json!({
        "type_table":{"nodes":[{"id":"map","kind":7,"key":{"kind":3,"primitive":3},"elem":{"kind":3,"primitive":3}}]},
        "constants":[{"id":"key","type":{"kind":3,"primitive":3},"value":1},{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},
            "locals":[{"id":"cursor","type":{"kind":7,"node":"map"}},{"id":"answer","type":{"kind":3,"primitive":3}}],
            "code":code}]
    })), LoadLimits::default()).unwrap());
    for action in ["complete", "cancel", "close"] {
        let mut instance = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
        instance.start("default", vec![]).unwrap();
        assert_eq!(instance.poll_steps(4).unwrap(), PollStatus::Running);
        instance.collect_garbage().unwrap();
        match action {
            "cancel" => instance.cancel().unwrap(),
            "close" => instance.close().unwrap(),
            _ => {
                while instance.poll_steps(1).unwrap() == PollStatus::Running {
                    instance.collect_garbage().unwrap();
                }
                assert_eq!(instance.results()[0].integer().unwrap(), 42);
            }
        }
        instance.collect_garbage().unwrap();
        assert_eq!(instance.heap_stats().live_objects, 0, "{action}");
        instance.close().unwrap();
    }
}

#[test]
fn allocation_pressure_preserves_popped_arguments_and_unpublished_frame_slots() {
    let integer = json!({"kind":3,"primitive":3});
    let slice = json!({"kind":5,"node":"slice"});
    let mut operations = Vec::new();
    for _ in 0..200 {
        operations.push((
            "make_sequence",
            json!({"type":slice,"element_count":0}),
            json!({"outputs":[0]}),
        ));
        operations.push(("pop", json!({}), json!({"inputs":[[0,0]],"release":[0]})));
    }
    operations.extend([
        (
            "const",
            json!({"constant":"answer"}),
            json!({"outputs":[1]}),
        ),
        (
            "make_sequence",
            json!({"type":slice,"element_count":1}),
            json!({"inputs":[[0,1]],"outputs":[0],"release":[1]}),
        ),
        (
            "call_direct",
            json!({"function":"read","arg_count":1,"result_count":1}),
            json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
        ),
        (
            "return",
            json!({"result_count":1}),
            json!({"inputs":[[0,1]],"release":[1]}),
        ),
    ]);
    let code = support::slot_code(json!([slice, integer]), &operations);
    let read = support::slot_code(
        json!([slice, integer, integer]),
        &[
            (
                "load_local",
                json!({"local":"input"}),
                json!({"outputs":[0]}),
            ),
            ("const", json!({"constant":"index"}), json!({"outputs":[1]})),
            (
                "load_index",
                json!({}),
                json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,2]],"release":[2]}),
            ),
        ],
    );
    let program = Arc::new(Program::load(&support::image(json!({
        "type_table":{"nodes":[{"id":"slice","kind":5,"elem":{"kind":3,"primitive":3}}]},
        "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42},{"id":"index","type":{"kind":3,"primitive":3},"value":0}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"locals":[{"id":"parent","type":{"kind":3,"primitive":3}}],"code":code},
            {"id":"read","signature":{"params":[{"type":{"kind":5,"node":"slice"}}],"results":[{"kind":3,"primitive":3}]},
                "locals":[{"id":"input","type":{"kind":5,"node":"slice"}},{"id":"temporary","type":{"kind":3,"primitive":3}}],"code":read}
        ]
    })), LoadLimits::default()).unwrap());
    let mut observations = Vec::new();
    for quantum in [1, 4096] {
        let mut instance = Instance::new(
            program.clone(),
            ExecutionLimits {
                max_objects: 4,
                max_heap_bytes: 4096,
                ..Default::default()
            },
        )
        .unwrap();
        instance.start("default", vec![]).unwrap();
        while instance.poll_steps(quantum).unwrap() == PollStatus::Running {}
        assert_eq!(instance.results()[0].integer().unwrap(), 42);
        instance.collect_garbage().unwrap();
        assert_eq!(instance.heap_stats().live_objects, 0);
        observations.push((
            instance.steps(),
            instance.heap_stats().total_allocated_bytes,
        ));
        instance.close().unwrap();
        assert_eq!(instance.heap_stats().live_bytes, 0);
    }
    assert_eq!(observations[0], observations[1]);
}
