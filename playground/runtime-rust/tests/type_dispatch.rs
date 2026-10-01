mod support;

use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    value::Data,
};
use serde_json::json;
use std::sync::Arc;
use support::slot_code;

#[test]
fn type_dispatch_resumes_candidates_and_preserves_the_matching_binding() {
    let integer = json!({"kind":3,"primitive":3});
    let any = json!({"kind":2});
    for fallback in [false, true] {
        let first_case = if fallback {
            json!({"kind":12,"node":"empty"})
        } else {
            json!({"kind":3,"primitive":1})
        };
        let image = support::image(json!({
            "type_table":{"nodes":[{"id":"empty","kind":12}]},
            "constants":[{"id":"answer","type":integer,"value":42}],
            "functions":[{"id":"fn.Main","signature":{"results":[integer]},
                "locals":[{"id":"subject","type":any},{"id":"result","type":integer}],
                "code":slot_code(json!([integer]), &[
                    ("const",json!({"constant":"answer"}),json!({"outputs":[0]})),
                    ("store_local",json!({"local":"subject"}),json!({"inputs":[[0,0]],"release":[0]})),
                    ("type_dispatch",json!({"subject":"subject","default":"failure","cases":[
                        {"type":first_case,"label":"failure"},
                        {"type":integer,"label":"success","binding":"result"}
                    ]}),json!({})),
                    ("label",json!({"label":"failure"}),json!({})),
                    ("zero",json!({"type":integer}),json!({"outputs":[0]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]})),
                    ("label",json!({"label":"success"}),json!({})),
                    ("load_local",json!({"local":"result"}),json!({"outputs":[0]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))])}]
        }));
        let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
        for quantum in [1, 2, 64] {
            let mut vm = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
            vm.start("default", Vec::new()).unwrap();
            let mut status = PollStatus::Running;
            for _ in 0..32 {
                let before = vm.steps();
                status = vm.poll_steps(quantum).unwrap();
                assert!(vm.steps() - before <= quantum as u64);
                if status == PollStatus::Ready {
                    break;
                }
            }
            assert_eq!(status, PollStatus::Ready);
            assert!(matches!(vm.results()[0].data(), Data::Integer(42)));
            vm.close().unwrap();
        }
    }
}
