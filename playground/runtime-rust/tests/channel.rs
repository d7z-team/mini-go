mod support;

use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    snapshot::HostData,
};
use serde_json::json;
use std::sync::Arc;
use support::slot_code;

#[test]
fn buffered_zero_sized_elements_preserve_type_through_ring_wrap() {
    let channel = json!({"kind":9,"node":"channel"});
    let integer = json!({"kind":3,"primitive":3});
    for (element, node) in [
        (
            json!({"kind":12,"node":"element"}),
            json!({"id":"element","kind":12}),
        ),
        (
            json!({"kind":6,"node":"element"}),
            json!({"id":"element","kind":6,"length":0,"elem":integer}),
        ),
    ] {
        let mut instructions = vec![
            (
                "const",
                json!({"constant":"capacity"}),
                json!({"outputs":[0]}),
            ),
            (
                "make_waitable",
                json!({"type":channel}),
                json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
            ),
        ];
        for _ in 0..3 {
            instructions.extend([
                ("zero", json!({"type":element}), json!({"outputs":[2]})),
                (
                    "waitable_send",
                    json!({}),
                    json!({"inputs":[[0,1],[0,2]],"release":[2]}),
                ),
                (
                    "waitable_recv",
                    json!({}),
                    json!({"inputs":[[0,1]],"outputs":[2]}),
                ),
            ]);
        }
        instructions.push((
            "return",
            json!({"result_count":1}),
            json!({"inputs":[[0,2]],"release":[1,2]}),
        ));
        let image = support::image(json!({
            "type_table":{"nodes":[node,{"id":"channel","kind":9,"direction":1,"elem":element}]},
            "constants":[{"id":"capacity","type":integer,"value":1}],
            "functions":[{"id":"fn.Main","signature":{"results":[element]},
                "code":slot_code(json!([integer,channel,element]), &instructions)}]
        }));
        let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        for step in 1..=12 {
            let status = vm.poll_steps(1).unwrap();
            assert_eq!(
                status,
                if step == 12 {
                    PollStatus::Ready
                } else {
                    PollStatus::Running
                }
            );
        }
        let result = vm.snapshot_results(Default::default()).unwrap();
        match (&result.roots[0].data, element["kind"].as_u64()) {
            (HostData::Array(values), Some(6)) => assert!(values.is_empty()),
            (HostData::Struct(fields), Some(12)) => assert!(fields.is_empty()),
            (data, kind) => panic!("channel changed element shape: {kind:?} -> {data:?}"),
        }
        vm.close().unwrap();
    }
}

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
        vm.start("default", &[]).unwrap();
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
        assert_eq!(
            vm.snapshot_results(Default::default()).unwrap().roots[0]
                .integer()
                .unwrap(),
            0
        );
        assert_eq!(
            vm.snapshot_results(Default::default()).unwrap().roots[1]
                .integer()
                .unwrap(),
            42
        );
        assert!(matches!(
            &vm.snapshot_results(Default::default()).unwrap().roots[2].data,
            HostData::Bool(true)
        ));
        vm.close().unwrap();
        assert_eq!(vm.heap_stats().live_objects, 0);
    }
}
