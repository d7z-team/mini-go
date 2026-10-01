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
fn unbuffered_try_send_delivers_to_waiting_task_without_buffering() {
    let channel = json!({"kind":9,"node":"channel"});
    let integer = json!({"kind":3,"primitive":3});
    let boolean = json!({"kind":3,"primitive":1});
    let function = json!({"kind":10,"node":"function"});
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"channel","kind":9,"direction":1,"elem":integer},{"id":"function","kind":10,"signature":{}}]},
        "globals":[{"id":"channel","type":channel},{"id":"ack","type":channel}],
        "constants":[{"id":"answer","type":integer,"value":42},{"id":"capacity","type":integer,"value":1}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer,integer,{"kind":3,"primitive":1}]},
             "locals":[{"id":"sent","type":boolean}],"code":slot_code(json!([integer,channel,function,boolean,integer]), &[
                ("zero",json!({"type":integer}),json!({"outputs":[0]})),
                ("make_waitable",json!({"type":channel}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
                ("store_global",json!({"global":"channel"}),json!({"inputs":[[0,1]],"release":[1]})),
                ("const",json!({"constant":"capacity"}),json!({"outputs":[0]})),
                ("make_waitable",json!({"type":channel}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
                ("store_global",json!({"global":"ack"}),json!({"inputs":[[0,1]],"release":[1]})),
                ("make_closure",json!({"function":"receiver"}),json!({"outputs":[2]})),
                ("spawn",json!({"arg_count":0}),json!({"inputs":[[0,2]],"release":[2]})),
                ("load_global",json!({"global":"ack"}),json!({"outputs":[1]})),
                ("waitable_recv",json!({}),json!({"inputs":[[0,1]],"outputs":[0],"release":[1]})),
                ("pop",json!({}),json!({"inputs":[[0,0]],"release":[0]})),
                ("load_global",json!({"global":"channel"}),json!({"outputs":[1]})),
                ("const",json!({"constant":"answer"}),json!({"outputs":[0]})),
                ("waitable_try_send",json!({}),json!({"inputs":[[0,1],[0,0]],"outputs":[3],"release":[1,0]})),
                ("store_local",json!({"local":"sent"}),json!({"inputs":[[0,3]],"release":[3]})),
                ("load_global",json!({"global":"channel"}),json!({"outputs":[1]})),
                ("len",json!({}),json!({"inputs":[[0,1]],"outputs":[4],"release":[1]})),
                ("load_global",json!({"global":"ack"}),json!({"outputs":[1]})),
                ("waitable_recv",json!({}),json!({"inputs":[[0,1]],"outputs":[0],"release":[1]})),
                ("load_local",json!({"local":"sent"}),json!({"outputs":[3]})),
                ("return",json!({"result_count":3}),json!({"inputs":[[0,4],[0,0],[0,3]],"release":[4,0,3]}))])},
            {"id":"receiver","code":slot_code(json!([channel,integer,channel]), &[
                ("load_global",json!({"global":"ack"}),json!({"outputs":[0]})),
                ("zero",json!({"type":integer}),json!({"outputs":[1]})),
                ("waitable_send",json!({}),json!({"inputs":[[0,0],[0,1]],"release":[0,1]})),
                ("load_global",json!({"global":"ack"}),json!({"outputs":[0]})),
                ("load_global",json!({"global":"channel"}),json!({"outputs":[2]})),
                ("waitable_recv",json!({}),json!({"inputs":[[0,2]],"outputs":[1],"release":[2]})),
                ("waitable_send",json!({}),json!({"inputs":[[0,0],[0,1]],"release":[0,1]})),
                ("return",json!({}),json!({}))])}
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
