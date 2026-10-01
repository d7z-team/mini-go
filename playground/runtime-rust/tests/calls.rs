mod support;

use mini_go::{
    LoadOptions, Program,
    instance::{ExecutionLimits, Instance, PollStatus},
};
use serde_json::json;
use std::sync::Arc;

#[test]
fn tail_call_keeps_pending_defers_and_counts_only_bytecode_steps() {
    let integer = json!({"kind":3,"primitive":3});
    let main = support::slot_code(
        json!([integer, integer, integer]),
        &[
            (
                "call_direct",
                json!({"function":"wrapper","result_count":1}),
                json!({"outputs":[0]}),
            ),
            (
                "load_global",
                json!({"global":"deferred"}),
                json!({"outputs":[1]}),
            ),
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
    let wrapper = support::slot_code(
        json!([{"kind":mini_go::contract_generated::Function,"node":"cleanup.signature"}]),
        &[
            (
                "make_closure",
                json!({"function":"cleanup"}),
                json!({"outputs":[0]}),
            ),
            (
                "defer_push",
                json!({}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
            (
                "tail_call_direct",
                json!({"function":"answer","result_count":1}),
                json!({}),
            ),
        ],
    );
    let answer = support::slot_code(
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
    let cleanup = support::slot_code(
        json!([integer]),
        &[
            ("const", json!({"constant":"delta"}), json!({"outputs":[0]})),
            (
                "store_global",
                json!({"global":"deferred"}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
            ("return", json!({}), json!({})),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"cleanup.signature","kind":mini_go::contract_generated::Function,"signature":{}}]},
        "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":40},{"id":"delta","type":{"kind":3,"primitive":3},"value":2}],
        "globals":[{"id":"deferred","type":{"kind":3,"primitive":3}}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer]},"code":main},
            {"id":"wrapper","signature":{"results":[integer]},"code":wrapper},
            {"id":"answer","signature":{"results":[integer]},"code":answer},
            {"id":"cleanup","code":cleanup}
        ]
    }));
    let program = Arc::new(Program::load(&image, LoadOptions::default()).unwrap());
    let mut instance = Instance::new(
        program,
        ExecutionLimits {
            max_frames: 3,
            ..Default::default()
        },
    )
    .unwrap();
    instance.start("default", vec![]).unwrap();
    assert_eq!(instance.poll_steps(6).unwrap(), PollStatus::Running);
    assert_eq!(instance.steps(), 6);
    assert_eq!(instance.poll_steps(6).unwrap(), PollStatus::Ready);
    assert_eq!(instance.steps(), 12);
    assert_eq!(instance.results()[0].integer().unwrap(), 42);
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
}
