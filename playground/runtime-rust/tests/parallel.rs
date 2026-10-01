mod support;

#[path = "support/execution_vectors.rs"]
mod execution_vectors;

use mini_go::{
    Executor, InstanceOptions, ffi::Cancellation, instance::ExecutionLimits, loader::LoadLimits,
    program::Program, snapshot::HostData,
};
use serde::Deserialize;
use serde_json::json;
use std::sync::Arc;
use support::slot_code;

#[derive(Deserialize)]
struct Vector {
    name: String,
    optimization: u8,
    image: Box<serde_json::value::RawValue>,
}

fn shared_program() -> Arc<Program> {
    let integer = json!({"kind":3,"primitive":3});
    let boolean = json!({"kind":3,"primitive":1});
    let channel = json!({"kind":9,"node":"channel"});
    let lock = json!({"kind":9,"node":"lock"});
    let pointer = json!({"kind":8,"node":"lockptr"});
    let function = json!({"kind":10,"node":"function"});
    let slice = json!({"kind":5,"node":"slice"});
    let mut worker = vec![
        (
            "address_of",
            json!({"kind":"upvalue","upvalue":"guard"}),
            json!({"outputs":[0]}),
        ),
        (
            "call_intrinsic",
            json!({"id":"sync.mutex_lock","arg_count":1}),
            json!({"inputs":[[0,0]],"release":[0]}),
        ),
        (
            "load_upvalue",
            json!({"upvalue":"counter"}),
            json!({"outputs":[1]}),
        ),
        ("const", json!({"constant":"one"}), json!({"outputs":[2]})),
        (
            "binary",
            json!({"operator":"+"}),
            json!({"inputs":[[0,1],[0,2]],"outputs":[3],"release":[1,2]}),
        ),
        (
            "store_upvalue",
            json!({"upvalue":"counter"}),
            json!({"inputs":[[0,3]],"release":[3]}),
        ),
        (
            "address_of",
            json!({"kind":"upvalue","upvalue":"guard"}),
            json!({"outputs":[0]}),
        ),
        (
            "call_intrinsic",
            json!({"id":"sync.mutex_unlock","arg_count":1}),
            json!({"inputs":[[0,0]],"release":[0]}),
        ),
    ];
    for _ in 0..96 {
        worker.extend([
            (
                "make_sequence",
                json!({"type":slice,"element_count":0}),
                json!({"outputs":[4]}),
            ),
            ("pop", json!({}), json!({"inputs":[[0,4]],"release":[4]})),
        ]);
    }
    worker.extend([
        (
            "load_global",
            json!({"global":"done"}),
            json!({"outputs":[5]}),
        ),
        ("zero", json!({"type":boolean}), json!({"outputs":[6]})),
        (
            "waitable_send",
            json!({}),
            json!({"inputs":[[0,5],[0,6]],"release":[5,6]}),
        ),
        ("return", json!({}), json!({})),
    ]);

    let mut main = vec![
        (
            "const",
            json!({"constant":"workers"}),
            json!({"outputs":[0]}),
        ),
        (
            "make_waitable",
            json!({"type":channel}),
            json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
        ),
        (
            "store_global",
            json!({"global":"done"}),
            json!({"inputs":[[0,1]],"release":[1]}),
        ),
    ];
    for _ in 0..4 {
        main.extend([
            (
                "make_closure",
                json!({"function":"worker","captures":[
                    {"kind":"local","local":"guard"},{"kind":"local","local":"counter"}
                ]}),
                json!({"outputs":[2]}),
            ),
            (
                "spawn",
                json!({"arg_count":0}),
                json!({"inputs":[[0,2]],"release":[2]}),
            ),
        ]);
    }
    for _ in 0..4 {
        main.extend([
            (
                "load_global",
                json!({"global":"done"}),
                json!({"outputs":[1]}),
            ),
            (
                "waitable_recv",
                json!({}),
                json!({"inputs":[[0,1]],"outputs":[3],"release":[1]}),
            ),
            ("pop", json!({}), json!({"inputs":[[0,3]],"release":[3]})),
        ]);
    }
    main.extend([
        (
            "load_local",
            json!({"local":"counter"}),
            json!({"outputs":[0]}),
        ),
        (
            "return",
            json!({"result_count":1}),
            json!({"inputs":[[0,0]],"release":[0]}),
        ),
    ]);

    Arc::new(
        Program::load(
            &support::image(json!({
                "type_table":{"nodes":[
                    {"id":"channel","kind":9,"direction":1,"elem":boolean},
                    {"id":"lock","kind":9,"direction":1,"elem":boolean},
                    {"id":"slice","kind":5,"elem":integer},
                    {"id":"lockptr","kind":8,"elem":lock},
                    {"id":"function","kind":10,"signature":{}}
                ]},
                "globals":[{"id":"done","type":channel}],
                "constants":[
                    {"id":"one","type":integer,"value":1},
                    {"id":"workers","type":integer,"value":4}
                ],
                "functions":[
                    {"id":"fn.Main","signature":{"results":[integer]},
                     "locals":[{"id":"guard","type":lock},{"id":"counter","type":integer}],
                     "code":slot_code(json!([integer,channel,function,boolean]),&main)},
                    {"id":"worker","revision_local":true,
                     "upvalues":[{"id":"guard","type":lock},{"id":"counter","type":integer}],
                     "code":slot_code(json!([pointer,integer,integer,integer,slice,channel,boolean]),&worker)}
                ]
            })),
            LoadLimits::default(),
        )
        .unwrap(),
    )
}

fn call_defer_program() -> Arc<Program> {
    let integer = json!({"kind":3,"primitive":3});
    let channel = json!({"kind":9,"node":"channel"});
    let boolean = json!({"kind":3,"primitive":1});
    let function = json!({"kind":10,"node":"function"});
    let helper = [
        ("label", json!({"label":"loop"}), json!({})),
        ("load_local", json!({"local":"i"}), json!({"outputs":[0]})),
        ("const", json!({"constant":"one"}), json!({"outputs":[1]})),
        (
            "binary",
            json!({"operator":"+"}),
            json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]}),
        ),
        (
            "store_local",
            json!({"local":"i"}),
            json!({"inputs":[[0,2]],"release":[2]}),
        ),
        ("load_local", json!({"local":"i"}), json!({"outputs":[0]})),
        ("const", json!({"constant":"limit"}), json!({"outputs":[1]})),
        (
            "binary",
            json!({"operator":"<"}),
            json!({"inputs":[[0,0],[0,1]],"outputs":[3],"release":[0,1]}),
        ),
        (
            "jump_if",
            json!({"label":"loop"}),
            json!({"inputs":[[0,3]],"release":[3]}),
        ),
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
    let mut main = Vec::new();
    for global in ["start", "done"] {
        main.extend([
            (
                "const",
                json!({"constant":"workers"}),
                json!({"outputs":[0]}),
            ),
            (
                "make_waitable",
                json!({"type":channel}),
                json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
            ),
            (
                "store_global",
                json!({"global":global}),
                json!({"inputs":[[0,1]],"release":[1]}),
            ),
        ]);
    }
    for _ in 0..4 {
        main.extend([
            (
                "make_closure",
                json!({"function":"worker"}),
                json!({"outputs":[2]}),
            ),
            ("spawn", json!({}), json!({"inputs":[[0,2]],"release":[2]})),
        ]);
    }
    for _ in 0..4 {
        main.extend([
            (
                "load_global",
                json!({"global":"start"}),
                json!({"outputs":[1]}),
            ),
            ("zero", json!({"type":integer}), json!({"outputs":[0]})),
            (
                "waitable_send",
                json!({}),
                json!({"inputs":[[0,1],[0,0]],"release":[1,0]}),
            ),
        ]);
    }
    for _ in 0..4 {
        main.extend([
            (
                "load_local",
                json!({"local":"total"}),
                json!({"outputs":[0]}),
            ),
            (
                "load_global",
                json!({"global":"done"}),
                json!({"outputs":[1]}),
            ),
            (
                "waitable_recv",
                json!({}),
                json!({"inputs":[[0,1]],"outputs":[3],"release":[1]}),
            ),
            (
                "binary",
                json!({"operator":"+"}),
                json!({"inputs":[[0,0],[0,3]],"outputs":[4],"release":[0,3]}),
            ),
            (
                "store_local",
                json!({"local":"total"}),
                json!({"inputs":[[0,4]],"release":[4]}),
            ),
        ]);
    }
    main.extend([
        (
            "load_local",
            json!({"local":"total"}),
            json!({"outputs":[0]}),
        ),
        (
            "return",
            json!({"result_count":1}),
            json!({"inputs":[[0,0]],"release":[0]}),
        ),
    ]);

    Arc::new(
        Program::load(
            &support::image(json!({
                "type_table":{"nodes":[{"id":"channel","kind":9,"direction":1,"elem":integer},{"id":"function","kind":10,"signature":{}}]},
                "globals":[{"id":"start","type":channel},{"id":"done","type":channel}],
                "constants":[
                    {"id":"workers","type":integer,"value":4},
                    {"id":"one","type":integer,"value":1},
                    {"id":"limit","type":integer,"value":1000},
                    {"id":"answer","type":integer,"value":42},
                    {"id":"panic","type":{"kind":3,"primitive":2},"value":"expected"}
                ],
                "functions":[
                    {"id":"fn.Main","signature":{"results":[integer]},
                     "locals":[{"id":"total","type":integer}],"code":slot_code(json!([integer,channel,function,integer,integer]),&main)},
                    {"id":"worker","locals":[{"id":"result","type":integer}],"code":slot_code(json!([channel,integer]), &[
                        ("load_global",json!({"global":"start"}),json!({"outputs":[0]})),
                        ("waitable_recv",json!({}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
                        ("pop",json!({}),json!({"inputs":[[0,1]],"release":[1]})),
                        ("call_direct",json!({"function":"recovered","result_count":1}),json!({"outputs":[1]})),
                        ("pop",json!({}),json!({"inputs":[[0,1]],"release":[1]})),
                        ("call_direct",json!({"function":"nested","result_count":1}),json!({"outputs":[1]})),
                        ("store_local",json!({"local":"result"}),json!({"inputs":[[0,1]],"release":[1]})),
                        ("load_global",json!({"global":"done"}),json!({"outputs":[0]})),
                        ("load_local",json!({"local":"result"}),json!({"outputs":[1]})),
                        ("waitable_send",json!({}),json!({"inputs":[[0,0],[0,1]],"release":[0,1]})),
                        ("return",json!({}),json!({}))])},
                    {"id":"nested","signature":{"results":[integer]},"code":slot_code(json!([function,integer]), &[
                        ("make_closure",json!({"function":"cleanup"}),json!({"outputs":[0]})),
                        ("defer_push",json!({}),json!({"inputs":[[0,0]],"release":[0]})),
                        ("call_direct",json!({"function":"helper","result_count":1}),json!({"outputs":[1]})),
                        ("return",json!({"result_count":1}),json!({"inputs":[[0,1]],"release":[1]}))])},
                    {"id":"cleanup","code":slot_code(json!([]), &[("return",json!({}),json!({}))])},
                    {"id":"helper","signature":{"results":[integer]},
                     "locals":[{"id":"i","type":integer}],"code":slot_code(json!([integer,integer,integer,boolean]),&helper)},
                    {"id":"recovered","signature":{"results":[integer]},"code":slot_code(json!([function,{"kind":3,"primitive":2},integer]), &[
                        ("make_closure",json!({"function":"recoverer"}),json!({"outputs":[0]})),
                        ("defer_push",json!({}),json!({"inputs":[[0,0]],"release":[0]})),
                        ("const",json!({"constant":"panic"}),json!({"outputs":[1]})),
                        ("panic",json!({}),json!({"inputs":[[0,1]],"release":[1]})),
                        ("zero",json!({"type":integer}),json!({"outputs":[2]})),
                        ("return",json!({"result_count":1}),json!({"inputs":[[0,2]],"release":[2]}))])},
                    {"id":"recoverer","code":slot_code(json!([{"kind":2}]), &[
                        ("recover",json!({}),json!({"outputs":[0]})),
                        ("pop",json!({}),json!({"inputs":[[0,0]],"release":[0]})),
                        ("return",json!({}),json!({}))])}
                ]
            })),
            LoadLimits::default(),
        )
        .unwrap(),
    )
}

#[test]
fn public_parallel_runtime_preserves_shared_cells_channels_mutexes_and_gc_roots() {
    let program = shared_program();
    for workers in [1, 2, 4] {
        let executor = Executor::new(workers).unwrap();
        let instance = program
            .instantiate(InstanceOptions {
                parallelism: workers,
                executor: Some(executor.clone()),
                limits: ExecutionLimits {
                    max_objects: 32,
                    max_heap_bytes: 16 * 1024,
                    max_allocated_bytes: 2 << 20,
                    ..Default::default()
                },
                ..Default::default()
            })
            .unwrap();
        let execution = instance.start("default", Vec::new()).unwrap();
        let result = execution.wait(&Cancellation::default()).unwrap();
        assert!(matches!(result.roots[0].data, HostData::Integer(4)));
        instance.collect_garbage().unwrap();
        instance.shutdown(&Cancellation::default()).unwrap();
        executor.shutdown(&Cancellation::default()).unwrap();
    }
}

#[test]
fn calls_returns_defers_and_recovery_cross_parallel_task_quanta() {
    let program = call_defer_program();
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
        assert!(matches!(result.roots[0].data, HostData::Integer(168)));
        execution.wait_scope(&Cancellation::default()).unwrap();
        instance.shutdown(&Cancellation::default()).unwrap();
        executor.shutdown(&Cancellation::default()).unwrap();
    }
}

#[test]
fn shared_go_fixtures_run_through_the_public_parallel_runtime() {
    let vectors = execution_vectors::load::<Vector>();
    for (name, expected) in [
        ("parallel_shared", 4),
        ("parallel_cpu", 2_121_168),
        ("select_transaction", 42),
    ] {
        let vector = vectors
            .iter()
            .find(|vector| vector.name == name && vector.optimization == 2)
            .unwrap_or_else(|| panic!("generated {name} O2 fixture"));
        let program =
            Arc::new(Program::load(vector.image.get().as_bytes(), LoadLimits::default()).unwrap());
        for workers in [1, 2, 4] {
            let executor = Executor::new(workers).unwrap();
            let instance = program
                .clone()
                .instantiate(InstanceOptions {
                    parallelism: workers,
                    executor: Some(executor.clone()),
                    ..Default::default()
                })
                .unwrap();
            let execution = instance.start("default", Vec::new()).unwrap();
            let result = execution.wait(&Cancellation::default()).unwrap();
            assert!(matches!(result.roots[0].data, HostData::Integer(value) if value == expected));
            execution.wait_scope(&Cancellation::default()).unwrap();
            instance.shutdown(&Cancellation::default()).unwrap();
            executor.shutdown(&Cancellation::default()).unwrap();
        }
    }
}
