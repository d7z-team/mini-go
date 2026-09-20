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
    let mut worker = vec![
        json!({"op":"address_of","payload":{"kind":"upvalue","upvalue":"guard"}}),
        json!({"op":"call_intrinsic","payload":{"id":"sync.mutex_lock","arg_count":1}}),
        json!({"op":"load_upvalue","payload":{"upvalue":"counter"}}),
        json!({"op":"const","payload":{"constant":"one"}}),
        json!({"op":"binary","payload":{"operator":"+"}}),
        json!({"op":"store_upvalue","payload":{"upvalue":"counter"}}),
        json!({"op":"address_of","payload":{"kind":"upvalue","upvalue":"guard"}}),
        json!({"op":"call_intrinsic","payload":{"id":"sync.mutex_unlock","arg_count":1}}),
    ];
    for _ in 0..96 {
        worker.extend([
            json!({"op":"make_sequence","payload":{"type":{"kind":5,"node":"slice"},"element_count":0}}),
            json!({"op":"pop"}),
        ]);
    }
    worker.extend([
        json!({"op":"load_global","payload":{"global":"done"}}),
        json!({"op":"zero","payload":{"type":boolean}}),
        json!({"op":"waitable_send"}),
        json!({"op":"return","payload":{}}),
    ]);

    let mut main = vec![
        json!({"op":"const","payload":{"constant":"workers"}}),
        json!({"op":"make_waitable","payload":{"type":channel}}),
        json!({"op":"store_global","payload":{"global":"done"}}),
    ];
    for _ in 0..4 {
        main.extend([
            json!({"op":"make_closure","payload":{"function":"worker","captures":[
                {"kind":"local","local":"guard"},{"kind":"local","local":"counter"}
            ]}}),
            json!({"op":"spawn","payload":{"arg_count":0}}),
        ]);
    }
    for _ in 0..4 {
        main.extend([
            json!({"op":"load_global","payload":{"global":"done"}}),
            json!({"op":"waitable_recv"}),
            json!({"op":"pop"}),
        ]);
    }
    main.extend([
        json!({"op":"load_local","payload":{"local":"counter"}}),
        json!({"op":"return","payload":{"result_count":1}}),
    ]);

    Arc::new(
        Program::load(
            &support::image(json!({
                "type_table":{"nodes":[
                    {"id":"channel","kind":9,"direction":1,"elem":boolean},
                    {"id":"lock","kind":9,"direction":1,"elem":boolean},
                    {"id":"slice","kind":5,"elem":integer}
                ]},
                "globals":[{"id":"done","type":channel}],
                "constants":[
                    {"id":"one","type":integer,"value":1},
                    {"id":"workers","type":integer,"value":4}
                ],
                "functions":[
                    {"id":"fn.Main","signature":{"results":[integer]},
                     "locals":[{"id":"guard","type":lock},{"id":"counter","type":integer}],
                     "instructions":main},
                    {"id":"worker","revision_local":true,
                     "upvalues":[{"id":"guard","type":lock},{"id":"counter","type":integer}],
                     "instructions":worker}
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
    let mut helper = vec![json!({"op":"label","payload":{"label":"loop"}})];
    helper.extend([
        json!({"op":"load_local","payload":{"local":"i"}}),
        json!({"op":"const","payload":{"constant":"one"}}),
        json!({"op":"binary","payload":{"operator":"+"}}),
        json!({"op":"store_local","payload":{"local":"i"}}),
        json!({"op":"load_local","payload":{"local":"i"}}),
        json!({"op":"const","payload":{"constant":"limit"}}),
        json!({"op":"binary","payload":{"operator":"<"}}),
        json!({"op":"jump_if","payload":{"label":"loop"}}),
        json!({"op":"const","payload":{"constant":"answer"}}),
        json!({"op":"return","payload":{"result_count":1}}),
    ]);
    let mut main = vec![
        json!({"op":"const","payload":{"constant":"workers"}}),
        json!({"op":"make_waitable","payload":{"type":channel}}),
        json!({"op":"store_global","payload":{"global":"start"}}),
        json!({"op":"const","payload":{"constant":"workers"}}),
        json!({"op":"make_waitable","payload":{"type":channel}}),
        json!({"op":"store_global","payload":{"global":"done"}}),
    ];
    for _ in 0..4 {
        main.extend([
            json!({"op":"make_closure","payload":{"function":"worker"}}),
            json!({"op":"spawn","payload":{}}),
        ]);
    }
    for _ in 0..4 {
        main.extend([
            json!({"op":"load_global","payload":{"global":"start"}}),
            json!({"op":"zero","payload":{"type":integer}}),
            json!({"op":"waitable_send"}),
        ]);
    }
    for _ in 0..4 {
        main.extend([
            json!({"op":"load_local","payload":{"local":"total"}}),
            json!({"op":"load_global","payload":{"global":"done"}}),
            json!({"op":"waitable_recv"}),
            json!({"op":"binary","payload":{"operator":"+"}}),
            json!({"op":"store_local","payload":{"local":"total"}}),
        ]);
    }
    main.extend([
        json!({"op":"load_local","payload":{"local":"total"}}),
        json!({"op":"return","payload":{"result_count":1}}),
    ]);

    Arc::new(
        Program::load(
            &support::image(json!({
                "type_table":{"nodes":[{"id":"channel","kind":9,"direction":1,"elem":integer}]},
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
                     "locals":[{"id":"total","type":integer}],"instructions":main},
                    {"id":"worker","locals":[{"id":"result","type":integer}],"instructions":[
                        {"op":"load_global","payload":{"global":"start"}},
                        {"op":"waitable_recv"},{"op":"pop"},
                        {"op":"call_direct","payload":{"function":"recovered","result_count":1}},
                        {"op":"pop"},
                        {"op":"call_direct","payload":{"function":"nested","result_count":1}},
                        {"op":"store_local","payload":{"local":"result"}},
                        {"op":"load_global","payload":{"global":"done"}},
                        {"op":"load_local","payload":{"local":"result"}},
                        {"op":"waitable_send"},{"op":"return","payload":{}}
                    ]},
                    {"id":"nested","signature":{"results":[integer]},"instructions":[
                        {"op":"make_closure","payload":{"function":"cleanup"}},
                        {"op":"defer_push","payload":{}},
                        {"op":"call_direct","payload":{"function":"helper","result_count":1}},
                        {"op":"return","payload":{"result_count":1}}
                    ]},
                    {"id":"cleanup","instructions":[{"op":"return","payload":{}}]},
                    {"id":"helper","signature":{"results":[integer]},
                     "locals":[{"id":"i","type":integer}],"instructions":helper},
                    {"id":"recovered","signature":{"results":[integer]},"instructions":[
                        {"op":"make_closure","payload":{"function":"recoverer"}},
                        {"op":"defer_push","payload":{}},
                        {"op":"const","payload":{"constant":"panic"}},
                        {"op":"panic"},
                        {"op":"zero","payload":{"type":integer}},
                        {"op":"return","payload":{"result_count":1}}
                    ]},
                    {"id":"recoverer","instructions":[
                        {"op":"recover"},{"op":"pop"},{"op":"return","payload":{}}
                    ]}
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
