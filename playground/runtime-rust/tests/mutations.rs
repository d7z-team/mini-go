mod support;

use mini_go::{
    LoadOptions, Program,
    instance::{ExecutionLimits, Instance, PollStatus},
};
use serde_json::json;
use std::sync::Arc;
use support::slot_code;

#[test]
fn bounded_image_mutations_and_patch_failures_preserve_the_active_revision() {
    let limits = LoadOptions {
        max_image_bytes: 64 << 10,
        max_artifact_bytes: 32 << 10,
        max_packages: 4,
        max_type_nodes: 64,
    };
    let image = |number: i64, mutation: u64| {
        let mut operations = vec![
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
        ];
        match mutation % 8 {
            1 => operations[0].1["constant"] = json!("missing"),
            2 => operations[0] = ("jump", json!({"label":"missing"}), json!({})),
            3 => operations[0] = ("pop", json!({}), json!({"inputs":[[0,0]]})),
            4 => operations[1].1["result_count"] = json!(2),
            _ => {}
        }
        let mut artifact = json!({
            "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":number}],
            "globals":[{"id":"state","type":{"kind":3,"primitive":3}}],
            "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},
                "code":slot_code(json!([{"kind":3,"primitive":3}]),&operations)}]
        });
        if mutation % 8 == 5 {
            artifact["globals"][0]["type"]["primitive"] = json!(2);
        }
        support::image(artifact)
    };
    let original = Arc::new(Program::load(&image(42, 0), limits).unwrap());
    let mut instance = Instance::new(
        original,
        ExecutionLimits {
            max_steps: 128,
            max_objects: 64,
            max_heap_bytes: 64 << 10,
            ..Default::default()
        },
    )
    .unwrap();
    let mut state = 0x5d68_9e31_81cd_473a_u64;
    let mut expected = 42;
    for iteration in 0..256 {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        let number = (state >> 8) as i64;
        let mut bytes = image(number, state);
        if state % 8 == 6 {
            let at = (state as usize >> 16) % bytes.len();
            bytes[at] ^= 0x80;
        }
        if state % 8 == 7 {
            bytes.truncate((state as usize >> 16) % bytes.len());
        }
        let revision = instance.revision();
        let heap = instance.heap_stats();
        match Program::load(&bytes, limits) {
            Ok(program) => match instance.prepare_patch(Arc::new(program)) {
                Ok(plan) => {
                    instance.apply_patch(plan).unwrap();
                    expected = number;
                }
                Err(_) => {
                    assert_eq!(instance.revision(), revision, "mutation {iteration}");
                    assert_eq!(instance.heap_stats(), heap);
                }
            },
            Err(_) => {
                assert_eq!(instance.revision(), revision, "mutation {iteration}");
                assert_eq!(instance.heap_stats(), heap);
            }
        }
        instance.start("default", vec![]).unwrap();
        assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Running);
        assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Ready);
        assert_eq!(instance.results()[0].integer().unwrap(), expected);
        assert_eq!(instance.retained_revisions(), [instance.revision()]);
    }
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
}
