mod support;

use mini_go::{
    Executor, InstanceOptions, LoadOptions, Program,
    contract::{canonical_hash, canonical_json},
    contract_generated as wire,
    ffi::Cancellation,
    snapshot::HostData,
};
use serde_json::json;
use std::sync::Arc;

#[test]
fn tasks_share_initialization_and_resume_without_recharging_the_call() {
    let mut initialization = Vec::new();
    for _ in 0..128 {
        initialization.extend([
            json!({"op":"const","payload":{"constant":"answer"}}),
            json!({"op":"pop"}),
        ]);
    }
    initialization.extend([
        json!({"op":"const","payload":{"constant":"answer"}}),
        json!({"op":"store_global","payload":{"global":"value"}}),
        json!({"op":"return","payload":{}}),
    ]);
    let dependency = support::image(json!({
        "module":{"path":"dependency","package":"dependency"},
        "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "globals":[{"id":"value","type":{"kind":3,"primitive":3}}],
        "functions":[
            {"id":"fn.init","instructions":initialization},
            {"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"instructions":[
                {"op":"load_global","payload":{"global":"value"}},
                {"op":"return","payload":{"result_count":1}}
            ]}
        ]
    }));
    let root = support::image(json!({
        "requirements":[{"kind":"source","module_path":"dependency"}],
        "globals":[{"id":"child","type":{"kind":3,"primitive":3}}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"instructions":[
                {"op":"make_closure","payload":{"function":"worker"}},
                {"op":"spawn","payload":{}},
                {"op":"call_direct","payload":{"module_path":"dependency","function":"fn.Main","result_count":1}},
                {"op":"return","payload":{"result_count":1}}
            ]},
            {"id":"worker","instructions":[
                {"op":"call_direct","payload":{"module_path":"dependency","function":"fn.Main","result_count":1}},
                {"op":"store_global","payload":{"global":"child"}},
                {"op":"return","payload":{}}
            ]},
            {"id":"read","signature":{"results":[{"kind":3,"primitive":3}]},"instructions":[
                {"op":"load_global","payload":{"global":"child"}},
                {"op":"return","payload":{"result_count":1}}
            ]}
        ]
    }));
    let mut image: wire::ExecutionImage = serde_json::from_slice(&root).unwrap();
    let dependency: wire::ExecutionImage = serde_json::from_slice(&dependency).unwrap();
    image
        .packages
        .as_mut()
        .unwrap()
        .extend(dependency.packages.unwrap());
    image.entries.0.as_mut().unwrap().push(
        serde_json::from_value(json!({
            "name":"read","module_path":"test","function_id":"read"
        }))
        .unwrap(),
    );
    image.hash.clear();
    image.hash = canonical_hash(&image).unwrap();
    let program =
        Arc::new(Program::load(&canonical_json(&image).unwrap(), LoadOptions::default()).unwrap());
    for workers in [1, 2, 4] {
        let executor = Executor::new(workers).unwrap();
        let instance = program
            .instantiate(InstanceOptions {
                parallelism: workers,
                executor: Some(executor.clone()),
                ..Default::default()
            })
            .unwrap();
        let execution = instance.start("default", Vec::new()).unwrap();
        let result = execution.wait(&Cancellation::default()).unwrap();
        assert!(matches!(result.roots[0].data, HostData::Integer(42)));
        execution.wait_scope(&Cancellation::default()).unwrap();
        // Init: 259, root: 4, worker: 3, dependency calls: 2 * 2.
        assert_eq!(execution.scope_stats().steps, 270);

        let read = instance.start("read", Vec::new()).unwrap();
        let result = read.wait(&Cancellation::default()).unwrap();
        assert!(matches!(result.roots[0].data, HostData::Integer(42)));
        assert_eq!(read.scope_stats().steps, 2);
        instance.shutdown(&Cancellation::default()).unwrap();
        executor.shutdown(&Cancellation::default()).unwrap();
    }
}
