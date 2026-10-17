use super::*;
use crate::HostValue;
use serde_json::json;
use test_helpers::slot_code;

#[test]
fn saturated_totals_preserve_poll_fairness_and_sampling() {
    let program = super::test_helpers::program_with_artifact(|artifact| {
        artifact["type_table"] = json!({"nodes":[{"id":"function","kind":10,"signature":{}}]});
        artifact["functions"] = serde_json::json!([
            {"id":"fn.Main", "code":slot_code(json!([{"kind":10,"node":"function"}]), &[
                ("make_closure",json!({"function":"child"}),json!({"outputs":[0]})),
                ("spawn",json!({"arg_count":0}),json!({"inputs":[[0,0]],"release":[0]})),
                ("label",json!({"label":"loop"}),json!({})),
                ("jump",json!({"label":"loop"}),json!({}))])},
            {"id":"child", "code":slot_code(json!([]), &[("return",json!({}),json!({}))])}
        ]);
    });
    let mut vm = Instance::new(
        program,
        ExecutionLimits {
            max_steps: UNLIMITED_STEPS,
            ..Default::default()
        },
    )
    .unwrap();
    vm.initialize_root(&Cancellation::default()).unwrap();
    vm.start("default", &[]).unwrap();
    let scope = vm.foreground_scope().unwrap();
    vm.start_profile(4, 32).unwrap();
    vm.steps = u64::MAX - 1;
    vm.scope_steps.insert(
        scope,
        Arc::new(budget::StepBudget::with_executed(u64::MAX - 1)),
    );
    for _ in 0..256 {
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Running);
        assert_eq!(vm.last_poll_steps, 1);
    }
    assert_eq!(vm.steps(), u64::MAX);
    assert_eq!(vm.scope_steps[&scope].executed(), u64::MAX);
    assert_eq!(
        vm.scope_work[&scope].tasks, 1,
        "spawned child must run despite saturated totals"
    );
    assert_eq!(
        vm.profile()
            .samples
            .iter()
            .map(|sample| sample.count)
            .sum::<u64>(),
        64
    );
    assert_eq!(vm.poll_steps(0).unwrap(), PollStatus::Running);
    assert_eq!(vm.last_poll_steps, 0);
    vm.cancel_scope(scope).unwrap();
    vm.poll_steps(1).unwrap();
    assert!(!vm.scope_active(scope));
    vm.close().unwrap();
}

#[test]
fn shared_step_limit_configuration() {
    let cases: serde_json::Value = serde_json::from_str(include_str!(
        "../../../../testdata/runtime/step_limits.json"
    ))
    .unwrap();
    for case in cases.as_array().unwrap() {
        let limits = ExecutionLimits {
            max_steps: case["input"].as_str().unwrap().parse().unwrap(),
            ..Default::default()
        };
        let normalized = limits.normalize();
        if case["error"].as_bool().unwrap_or(false) {
            assert!(normalized.is_err());
        } else {
            assert_eq!(
                normalized.unwrap().max_steps.to_string(),
                case["normalized"].as_str().unwrap()
            );
        }
    }
}

#[test]
fn internal_bytes_and_slice_opcodes_ignore_host_boundary_budgets() {
    let integer = json!({"kind": wire::Primitive, "primitive": wire::PrimitiveInt});
    let byte = json!({
        "kind": wire::Primitive,
        "primitive": wire::PrimitiveUint8
    });
    let bytes = json!({"kind": wire::Slice, "node": "bytes"});
    let program = super::test_helpers::program_with_artifact(|artifact| {
        artifact["type_table"] = json!({
            "nodes": [{"id": "bytes", "kind": wire::Slice, "elem": byte.clone()}]
        });
        artifact["constants"] = json!([
            {"id": "data", "type": bytes.clone(), "value": "/wA="},
            {"id": "index", "type": integer.clone(), "value": 0},
            {"id": "length", "type": integer.clone(), "value": 2}
        ]);
        artifact["functions"] = json!([{
            "id": "fn.Main",
            "signature": {"results": [integer.clone(), integer.clone()]},
            "code": slot_code(
                json!([bytes.clone(), integer.clone(), byte.clone(), integer.clone()]),
                &[
                    ("const", json!({"constant": "data"}), json!({"outputs": [0]})),
                    ("const", json!({"constant": "index"}), json!({"outputs": [1]})),
                    ("load_index", json!({}), json!({
                        "inputs": [[0, 0], [0, 1]],
                        "outputs": [2],
                        "release": [0, 1]
                    })),
                    ("convert", json!({"type": integer.clone()}), json!({
                        "inputs": [[0, 2]],
                        "outputs": [3],
                        "release": [2]
                    })),
                    ("const", json!({"constant": "length"}), json!({"outputs": [1]})),
                    ("make_slice", json!({"type": bytes.clone()}), json!({
                        "inputs": [[0, 1]],
                        "outputs": [0],
                        "release": [1]
                    })),
                    ("len", json!({}), json!({"inputs": [[0, 0]], "outputs": [1]})),
                    ("return", json!({"result_count": 2}), json!({
                        "inputs": [[0, 3], [0, 1]],
                        "release": [3, 1]
                    }))
                ]
            )
        }]);
    });
    let mut vm = Instance::new(
        program,
        ExecutionLimits {
            max_boundary_bytes: 1,
            max_boundary_depth: 1,
            max_boundary_elements: 1,
            ..ExecutionLimits::default()
        },
    )
    .unwrap();
    vm.start("default", &[]).unwrap();
    assert_eq!(vm.poll_steps(256).unwrap(), PollStatus::Ready);
    let results = vm.snapshot_results(Default::default()).unwrap();
    assert_eq!(results.roots[0].integer().unwrap(), 255);
    assert_eq!(results.roots[1].integer().unwrap(), 2);
    vm.close().unwrap();
}

#[test]
fn host_byte_input_still_obeys_boundary_byte_budget() {
    let bytes = json!({"kind": wire::Slice, "node": "bytes"});
    let program = super::test_helpers::program_with_artifact(|artifact| {
        artifact["type_table"] = json!({
            "nodes": [{
                "id": "bytes",
                "kind": wire::Slice,
                "elem": {"kind": wire::Primitive, "primitive": wire::PrimitiveUint8}
            }]
        });
        artifact["functions"] = json!([{
            "id": "fn.Main",
            "signature": {"params": [{"type": bytes.clone()}], "results": [bytes.clone()]},
            "locals": [{"id": "input", "type": bytes.clone()}],
            "code": slot_code(json!([bytes.clone()]), &[
                ("load_local", json!({"local": "input"}), json!({"outputs": [0]})),
                ("return", json!({"result_count": 1}), json!({
                    "inputs": [[0, 0]],
                    "release": [0]
                }))
            ])
        }]);
    });
    let mut vm = Instance::new(
        program,
        ExecutionLimits {
            max_boundary_bytes: 18,
            max_boundary_depth: 1,
            max_boundary_elements: 1,
            ..ExecutionLimits::default()
        },
    )
    .unwrap();
    let before = vm.heap_stats().live_objects;
    let error = vm
        .start("default", &[HostValue::bytes([1, 2, 3])])
        .unwrap_err();
    assert_eq!(error.code, "boundary_limit");
    assert_eq!(vm.heap_stats().live_objects, before);
    vm.close().unwrap();
}
