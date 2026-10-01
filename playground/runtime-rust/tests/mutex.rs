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
fn mutex_zero_state_try_lock_and_reuse() {
    let address = (
        "address_of",
        json!({"kind":"local","local":"state"}),
        json!({"outputs":[0]}),
    );
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"state","kind":9,"direction":1,"elem":{"kind":3,"primitive":1}},
            {"id":"pointer","kind":8,"elem":{"kind":9,"node":"state"}}]},
        "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":1},{"kind":3,"primitive":1}]},
            "locals":[{"id":"state","type":{"kind":9,"node":"state"}}],
            "code":slot_code(json!([{"kind":8,"node":"pointer"},{"kind":3,"primitive":1},{"kind":3,"primitive":1}]), &[
                address.clone(),("call_intrinsic",json!({"id":"sync.mutex_try_lock","arg_count":1,"result_count":1}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
                address.clone(),("call_intrinsic",json!({"id":"sync.mutex_try_lock","arg_count":1,"result_count":1}),json!({"inputs":[[0,0]],"outputs":[2],"release":[0]})),
                address.clone(),("call_intrinsic",json!({"id":"sync.mutex_unlock","arg_count":1}),json!({"inputs":[[0,0]],"release":[0]})),
                address.clone(),("call_intrinsic",json!({"id":"sync.mutex_lock","arg_count":1}),json!({"inputs":[[0,0]],"release":[0]})),
                address,("call_intrinsic",json!({"id":"sync.mutex_unlock","arg_count":1}),json!({"inputs":[[0,0]],"release":[0]})),
                ("return",json!({"result_count":2}),json!({"inputs":[[0,1],[0,2]],"release":[1,2]}))])}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    vm.start("default", Vec::new()).unwrap();
    while vm.poll_steps(1).unwrap() != PollStatus::Ready {}
    assert!(matches!(vm.results()[0].data(), Data::Bool(true)));
    assert!(matches!(vm.results()[1].data(), Data::Bool(false)));
    vm.collect_garbage().unwrap();
    assert_eq!(vm.heap_stats().live_objects, 0);
    vm.close().unwrap();
}
