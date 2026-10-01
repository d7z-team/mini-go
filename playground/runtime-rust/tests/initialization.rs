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
    let integer = json!({"kind":3,"primitive":3});
    let mut initialization = Vec::new();
    for _ in 0..128 {
        initialization.extend([
            (
                "const",
                json!({"constant":"answer"}),
                json!({"outputs":[0]}),
            ),
            ("pop", json!({}), json!({"inputs":[[0,0]],"release":[0]})),
        ]);
    }
    initialization.extend([
        (
            "const",
            json!({"constant":"answer"}),
            json!({"outputs":[0]}),
        ),
        (
            "store_global",
            json!({"global":"value"}),
            json!({"inputs":[[0,0]],"release":[0]}),
        ),
        ("return", json!({}), json!({})),
    ]);
    let initialization = support::slot_code(json!([integer]), &initialization);
    let dependency_main = support::slot_code(
        json!([integer]),
        &[
            (
                "load_global",
                json!({"global":"value"}),
                json!({"outputs":[0]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
        ],
    );
    let dependency = support::image(json!({
        "module":{"path":"dependency","package":"dependency"},
        "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "globals":[{"id":"value","type":{"kind":3,"primitive":3}}],
        "functions":[
            {"id":"fn.init","code":initialization},
            {"id":"fn.Main","signature":{"results":[integer]},"code":dependency_main}
        ]
    }));
    let main = support::slot_code(
        json!([{"kind":wire::Function,"node":"worker.signature"},integer]),
        &[
            (
                "make_closure",
                json!({"function":"worker"}),
                json!({"outputs":[0]}),
            ),
            ("spawn", json!({}), json!({"inputs":[[0,0]],"release":[0]})),
            (
                "call_direct",
                json!({"module_path":"dependency","function":"fn.Main","result_count":1}),
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
        json!([integer]),
        &[
            (
                "call_direct",
                json!({"module_path":"dependency","function":"fn.Main","result_count":1}),
                json!({"outputs":[0]}),
            ),
            (
                "store_global",
                json!({"global":"child"}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
            ("return", json!({}), json!({})),
        ],
    );
    let read = support::slot_code(
        json!([integer]),
        &[
            (
                "load_global",
                json!({"global":"child"}),
                json!({"outputs":[0]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
        ],
    );
    let root = support::image(json!({
        "type_table":{"nodes":[{"id":"worker.signature","kind":wire::Function,"signature":{}}]},
        "requirements":[{"kind":"source","module_path":"dependency"}],
        "globals":[{"id":"child","type":{"kind":3,"primitive":3}}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer]},"code":main},
            {"id":"worker","code":worker},
            {"id":"read","signature":{"results":[integer]},"code":read}
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
