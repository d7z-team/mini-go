mod support;

use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    value::Data,
};
use serde_json::json;
use std::sync::Arc;

#[test]
fn unbuffered_try_send_delivers_to_waiting_task_without_buffering() {
    let channel = json!({"kind":9,"node":"channel"});
    let integer = json!({"kind":3,"primitive":3});
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"channel","kind":9,"direction":1,"elem":integer}]},
        "globals":[{"id":"channel","type":channel},{"id":"ack","type":channel}],
        "constants":[{"id":"answer","type":integer,"value":42},{"id":"capacity","type":integer,"value":1}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer,integer,{"kind":3,"primitive":1}]},
             "locals":[{"id":"sent","type":{"kind":3,"primitive":1}}],"instructions":[
                {"op":"zero","payload":{"type":integer}},
                {"op":"make_waitable","payload":{"type":channel}},
                {"op":"store_global","payload":{"global":"channel"}},
                {"op":"const","payload":{"constant":"capacity"}},
                {"op":"make_waitable","payload":{"type":channel}},
                {"op":"store_global","payload":{"global":"ack"}},
                {"op":"make_closure","payload":{"function":"receiver"}},
                {"op":"spawn","payload":{"arg_count":0}},
                {"op":"load_global","payload":{"global":"ack"}},{"op":"waitable_recv"},{"op":"pop"},
                {"op":"load_global","payload":{"global":"channel"}},
                {"op":"const","payload":{"constant":"answer"}},{"op":"waitable_try_send"},
                {"op":"store_local","payload":{"local":"sent"}},
                {"op":"load_global","payload":{"global":"channel"}},{"op":"len"},
                {"op":"load_global","payload":{"global":"ack"}},{"op":"waitable_recv"},
                {"op":"load_local","payload":{"local":"sent"}},{"op":"return","payload":{"result_count":3}}
             ]},
            {"id":"receiver","instructions":[
                {"op":"load_global","payload":{"global":"ack"}},
                {"op":"zero","payload":{"type":integer}},{"op":"waitable_send"},
                {"op":"load_global","payload":{"global":"ack"}},
                {"op":"load_global","payload":{"global":"channel"}},{"op":"waitable_recv"},
                {"op":"waitable_send"},{"op":"return","payload":{}}
            ]}
        ]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    for quantum in [1, 64, 1024] {
        let mut vm = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let mut status = PollStatus::Running;
        for _ in 0..256 {
            status = vm.poll_steps(quantum).unwrap();
            if status == PollStatus::Ready {
                break;
            }
        }
        assert_eq!(
            status,
            PollStatus::Ready,
            "quantum {quantum}, steps {}, stats {:?}",
            vm.steps(),
            vm.stats()
        );
        assert_eq!(vm.results()[0].integer().unwrap(), 0);
        assert_eq!(vm.results()[1].integer().unwrap(), 42);
        assert!(matches!(vm.results()[2].data(), Data::Bool(true)));
        vm.close().unwrap();
        assert_eq!(vm.heap_stats().live_objects, 0);
    }
}
