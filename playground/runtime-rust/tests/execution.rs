use mini_go::{
    Executor, InstanceOptions,
    ffi::Cancellation,
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
};
use serde::Deserialize;
use std::sync::{Arc, OnceLock};

#[path = "support/execution_vectors.rs"]
mod execution_vectors;

#[derive(Deserialize)]
struct Vector {
    name: String,
    optimization: u8,
    image: Box<serde_json::value::RawValue>,
    result_integer: String,
}

#[test]
fn executes_go_compiler_images_at_every_optimization_level() {
    execute_vectors(|vector| !vector.name.starts_with("stdlib_"));
}

#[test]
fn executes_math_and_strconv_source_at_every_optimization_level() {
    execute_vectors(|vector| vector.name == "stdlib_math" || vector.name == "stdlib_strconv");
}

#[test]
fn executes_sha256_source_at_every_optimization_level() {
    execute_vectors(|vector| vector.name == "stdlib_sha256");
}

#[test]
fn json_roundtrip_and_invalid_input_match_go_observations() {
    execute_vectors(|vector| vector.name == "stdlib_json");
}

#[test]
fn executes_reflect_source_at_every_optimization_level() {
    execute_vectors(|vector| {
        vector.name == "stdlib_reflect_value" || vector.name == "stdlib_reflect_construct"
    });
}

#[test]
fn reflection_field_paths_preserve_mutation_and_visibility() {
    execute_vectors(|vector| vector.name == "stdlib_reflect_fields");
}

#[test]
fn reflection_collections_and_cycles_match_go_observations() {
    execute_vectors(|vector| {
        vector.name == "stdlib_reflect_collections" || vector.name == "stdlib_reflect_cycles"
    });
}

#[test]
fn dynamic_reflection_types_and_storage_match_go_observations() {
    execute_vectors(|vector| {
        matches!(
            vector.name.as_str(),
            "stdlib_reflect_dynamic" | "stdlib_reflect_map" | "stdlib_reflect_slice"
        )
    });
}

#[test]
fn reflection_budget_failure_preserves_registered_metadata_until_close() {
    let vectors: Vec<Vector> = execution_vectors::load();
    let vector = vectors
        .iter()
        .find(|vector| vector.name == "stdlib_reflect_dynamic" && vector.optimization == 2)
        .unwrap();
    let program =
        Arc::new(Program::load(vector.image.get().as_bytes(), LoadLimits::default()).unwrap());
    for (limits, code, registered) in [
        (
            ExecutionLimits {
                max_dynamic_types: 1,
                ..Default::default()
            },
            "dynamic_type_limit",
            1,
        ),
        (
            ExecutionLimits {
                max_dynamic_type_bytes: 1,
                ..Default::default()
            },
            "dynamic_type_bytes_limit",
            0,
        ),
    ] {
        let mut instance = Instance::new(program.clone(), limits).unwrap();
        instance
            .start(&program.image().entries[0].name, vec![])
            .unwrap();
        let error = loop {
            match instance.poll_steps(4096) {
                Ok(PollStatus::Running) => {}
                Err(error) => break error,
                status => panic!("expected reflection budget failure, got {status:?}"),
            }
        };
        assert_eq!(error.code, code);
        assert_eq!(instance.stats().dynamic_types, registered);
        assert_eq!(instance.stats().dynamic_type_bytes == 0, registered == 0);
        instance.close().unwrap();
        assert_eq!(instance.stats().dynamic_types, 0);
        assert_eq!(instance.stats().dynamic_type_bytes, 0);
        assert_eq!(instance.heap_stats().live_bytes, 0);
    }
}

#[test]
fn reflection_channel_operations_resume_through_the_owner() {
    execute_vectors(|vector| {
        matches!(
            vector.name.as_str(),
            "stdlib_reflect_channel" | "stdlib_reflect_select"
        )
    });
}

#[test]
fn reflection_calls_and_callbacks_resume_without_reentering_the_vm() {
    execute_vectors(|vector| {
        matches!(
            vector.name.as_str(),
            "stdlib_reflect_call" | "stdlib_reflect_make_func" | "stdlib_reflect_callback_recover"
        )
    });
}

#[test]
fn reflection_methods_preserve_receiver_and_call_signature() {
    execute_vectors(|vector| vector.name == "stdlib_reflect_method");
}

#[test]
fn reflection_callbacks_run_through_the_public_parallel_runtime() {
    let vectors: Vec<Vector> = execution_vectors::load();
    for vector in vectors.iter().filter(|vector| {
        vector.optimization == 2
            && matches!(
                vector.name.as_str(),
                "stdlib_reflect_select" | "stdlib_reflect_callback_recover"
            )
    }) {
        let program =
            Arc::new(Program::load(vector.image.get().as_bytes(), LoadLimits::default()).unwrap());
        for workers in [1, 2] {
            let executor = Executor::new(workers).unwrap();
            let instance = program
                .clone()
                .instantiate(InstanceOptions {
                    parallelism: workers,
                    executor: Some(executor.clone()),
                    ..Default::default()
                })
                .unwrap();
            let execution = instance
                .start(&program.image().entries[0].name, Vec::new())
                .unwrap();
            let result = execution.wait(&Cancellation::default()).unwrap();
            let mini_go::snapshot::HostData::Integer(value) = result.roots[0].data else {
                panic!("{} returned a non-integer result", vector.name);
            };
            assert_eq!(value.to_string(), vector.result_integer, "{}", vector.name);
            execution.wait_scope(&Cancellation::default()).unwrap();
            instance.shutdown(&Cancellation::default()).unwrap();
            executor.shutdown(&Cancellation::default()).unwrap();
        }
    }
}

fn execute_vectors(select: impl Fn(&Vector) -> bool) {
    static VECTORS: OnceLock<Vec<Vector>> = OnceLock::new();
    let vectors = VECTORS.get_or_init(|| {
        let vectors: Vec<Vector> = execution_vectors::load();
        let expected: std::collections::BTreeMap<String, String> = serde_json::from_slice(
            include_bytes!("../../../testdata/runtime/execution_expected.json"),
        )
        .unwrap();
        for vector in &vectors {
            assert_eq!(
                expected.get(&vector.name),
                Some(&vector.result_integer),
                "independent expectation: {}",
                vector.name
            );
        }
        vectors
    });
    for vector in vectors.iter().filter(|vector| select(vector)) {
        eprintln!("{} O{}", vector.name, vector.optimization);
        let program = Arc::new(
            Program::load(vector.image.get().as_bytes(), LoadLimits::default()).unwrap_or_else(
                |error| panic!("{} O{}: {error}", vector.name, vector.optimization),
            ),
        );
        let entry = program.image().entries[0].name.clone();
        let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
        let has_globals = instance.heap_stats().live_objects != 0;
        // Exercise precise polling and a warm instance with a larger quantum.
        for quantum in [1, 64] {
            instance.start(&entry, Vec::new()).unwrap();
            loop {
                let status = instance.poll_steps(quantum).unwrap_or_else(|error| {
                    panic!("{} O{}: {error}", vector.name, vector.optimization)
                });
                if status == PollStatus::Running {
                    continue;
                }
                assert_eq!(status, PollStatus::Ready, "fixture must not deadlock");
                break;
            }
            assert_eq!(
                instance.results()[0].integer().unwrap().to_string(),
                vector.result_integer,
                "{} O{}",
                vector.name,
                vector.optimization
            );
            loop {
                let status = instance.poll_background(64).unwrap();
                if status == PollStatus::Running {
                    continue;
                }
                assert_eq!(status, PollStatus::Ready, "background task must terminate");
                break;
            }
            if !has_globals {
                instance.collect_garbage().unwrap();
                assert_eq!(
                    instance.heap_stats().live_objects,
                    0,
                    "temporary slots and cycles must be reclaimed"
                );
            }
        }
        instance.close().unwrap();
        assert_eq!(instance.heap_stats().live_bytes, 0);
    }
}

#[test]
fn polling_budget_and_cancellation_preserve_instance_lifecycle() {
    let vectors: Vec<Vector> = execution_vectors::load();
    let vector = vectors
        .iter()
        .find(|vector| vector.name == "arithmetic")
        .unwrap();
    let program =
        Arc::new(Program::load(vector.image.get().as_bytes(), LoadLimits::default()).unwrap());
    let entry = program.image().entries[0].name.clone();
    let mut instance = Instance::new(
        program,
        ExecutionLimits {
            max_steps: 10,
            ..ExecutionLimits::default()
        },
    )
    .unwrap();
    instance.start(&entry, Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(0).unwrap(), PollStatus::Running);
    assert_eq!(instance.steps(), 0);
    assert_eq!(instance.poll_steps(3).unwrap(), PollStatus::Running);
    assert_eq!(instance.steps(), 3);
    instance.cancel().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
    instance.start(&entry, Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(11).unwrap_err().code, "step_limit");
    assert_eq!(instance.steps(), 13);
    assert_eq!(instance.heap_stats().live_objects, 0);
    instance.start(&entry, Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(2).unwrap(), PollStatus::Running);
    instance.cancel().unwrap();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Ready);
    instance.close().unwrap();
    assert_eq!(
        instance.start(&entry, Vec::new()).unwrap_err().code,
        "closed"
    );
}
