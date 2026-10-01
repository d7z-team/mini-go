use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
};
use serde::Deserialize;
use serde_json::value::RawValue;
use std::sync::Arc;

#[derive(Deserialize)]
struct Vector {
    name: String,
    image: Box<RawValue>,
    limit: u64,
    actions: Vec<Action>,
    #[serde(default)]
    initialization_error: String,
    initialization_steps: u64,
    initialization_memory: [u64; 4],
}

#[derive(Deserialize)]
struct Action {
    operation: String,
    #[serde(default)]
    state: String,
    #[serde(default)]
    error: String,
    steps: u64,
    memory: [u64; 4],
}

#[test]
fn owner_actions_and_memory_match_current_go_observations() {
    let vectors: Vec<Vector> =
        serde_json::from_str(include_str!("../../../testdata/runtime/state.json")).unwrap();
    assert!(!vectors.is_empty());
    for vector in vectors {
        let program =
            Arc::new(Program::load(vector.image.get().as_bytes(), LoadLimits::default()).unwrap());
        let mut instance = Instance::new(
            program,
            ExecutionLimits {
                max_allocated_bytes: vector.limit,
                max_steps: 1000,
                ..ExecutionLimits::default()
            },
        )
        .unwrap();
        let initialization = instance.initialize_root(&Default::default());
        if !vector.initialization_error.is_empty() {
            assert_eq!(
                initialization.unwrap_err().code,
                vector
                    .initialization_error
                    .strip_prefix("execution.")
                    .unwrap_or(&vector.initialization_error),
                "{} limit {} initialization",
                vector.name,
                vector.limit
            );
            assert_eq!(instance.heap_stats().live_objects, 0);
            continue;
        }
        initialization.unwrap();
        assert_eq!(
            instance.steps(),
            vector.initialization_steps,
            "{} initialization steps",
            vector.name
        );
        let initial = instance.memory_stats();
        assert_eq!(
            [
                initial.live_bytes,
                initial.allocated_since_sweep,
                initial.total_allocated_bytes,
                initial.peak_bytes
            ],
            vector.initialization_memory,
            "{} limit {} initialization memory",
            vector.name,
            vector.limit
        );
        for (index, action) in vector.actions.iter().enumerate() {
            let context = format!(
                "{} limit {} action {index} {}",
                vector.name, vector.limit, action.operation
            );
            let before = instance.steps();
            let result = match action.operation.as_str() {
                "start" => instance.start("default", vec![]).map(|()| String::new()),
                "cancel" => instance.cancel().map(|()| String::new()),
                "poll" => instance.poll_steps(1).map(|state| {
                    match state {
                        PollStatus::Running => "running",
                        PollStatus::Ready => "completed",
                        PollStatus::Pending => "blocked",
                        PollStatus::Paused => "paused",
                    }
                    .to_owned()
                }),
                operation => panic!("unknown owner action {operation}"),
            };
            match result {
                Ok(state) => {
                    assert!(
                        action.error.is_empty(),
                        "{context}: expected {}, got {state}",
                        action.error
                    );
                    assert_eq!(state, action.state, "{context}");
                }
                Err(error) => assert_eq!(
                    error.code,
                    action
                        .error
                        .strip_prefix("execution.")
                        .unwrap_or(&action.error),
                    "{context}: {error}"
                ),
            }
            assert_eq!(
                instance.steps() - before,
                action.steps,
                "{context}: instruction count"
            );
            let stats = instance.memory_stats();
            assert_eq!(
                [
                    stats.live_bytes,
                    stats.allocated_since_sweep,
                    stats.total_allocated_bytes,
                    stats.peak_bytes
                ],
                action.memory,
                "{context}: memory"
            );
        }
        instance.close().unwrap();
        assert_eq!(instance.memory_stats().live_bytes, 0);
        assert_eq!(instance.memory_stats().allocated_since_sweep, 0);
        assert_eq!(instance.heap_stats().live_objects, 0);
    }
}
