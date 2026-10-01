use super::*;
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
    vm.start("default", Vec::new()).unwrap();
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
