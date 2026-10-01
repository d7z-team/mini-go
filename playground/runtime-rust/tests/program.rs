mod support;

use mini_go::{loader::LoadLimits, program::Program};
use serde_json::json;
use support::slot_code;

#[test]
fn missing_dependency_export_is_rejected_at_load() {
    use mini_go::{
        contract::{canonical_hash, canonical_json},
        contract_generated::ExecutionImage,
    };
    let dependency: ExecutionImage = serde_json::from_slice(&support::image(json!({
        "module": {"path": "dependency", "package": "dependency"},
        "functions": [{"id": "fn.Main", "code":{"descriptors":{}}}]
    })))
    .unwrap();
    let mut image: ExecutionImage = serde_json::from_slice(&support::image(json!({
        "requirements": [{"kind": "source", "module_path": "dependency", "exports": ["Missing"]}],
        "functions": [{"id": "fn.Main", "code":{"descriptors":{}}}]
    })))
    .unwrap();
    image
        .packages
        .as_mut()
        .unwrap()
        .extend(dependency.packages.unwrap());
    image.hash.clear();
    image.hash = canonical_hash(&image).unwrap();
    let error = Program::load(&canonical_json(&image).unwrap(), LoadLimits::default())
        .err()
        .unwrap();
    assert_eq!(error.code, "missing_export");
}

#[test]
fn dependency_cycles_are_rejected_before_preparation() {
    let image = support::image(
        json!({"requirements":[{"kind":"source","module_path":"test"}], "functions":[{"id":"fn.Main","code":{"descriptors":{}}}]}),
    );
    assert_eq!(
        Program::load(&image, LoadLimits::default())
            .err()
            .unwrap()
            .code,
        "dependency_cycle"
    );
}

#[test]
fn rejects_invalid_control_flow_and_captures_before_instantiation() {
    let cases = [
        (
            json!({"id": "fn.Main", "code":slot_code(json!([]), &[("pop",json!({}),json!({}))])}),
            "invalid_operand",
        ),
        (
            json!({"id": "fn.Main", "code":slot_code(json!([]), &[("jump",json!({"label":"missing"}),json!({}))])}),
            "invalid_jump",
        ),
        (
            json!({"id": "fn.Main", "result_locals": ["missing"], "signature": {"results": [{"kind": 3, "primitive": 3}]},"code":{"descriptors":{}}}),
            "invalid_result_local",
        ),
        (
            json!({"id": "fn.Main", "code":slot_code(json!([{"kind":2}]), &[
                ("make_closure",json!({"function":"fn.Main","captures":[{"kind":"local","local":"missing"}]}),json!({"outputs":[0]})),
                ("pop",json!({}),json!({"inputs":[[0,0]],"release":[0]}))])}),
            "invalid_operand",
        ),
        (
            json!({"id": "fn.Main", "code":slot_code(json!([]), &[("call_ffi",json!({"arg_count":1,"result_count":3}),json!({}))])}),
            "invalid_operand",
        ),
    ];
    for (function, code) in cases {
        let image = support::image(json!({"functions": [function]}));
        assert_eq!(
            Program::load(&image, LoadLimits::default())
                .err()
                .unwrap()
                .code,
            code
        );
    }
}

#[test]
fn branch_join_requires_inputs_initialized_on_every_predecessor() {
    let image = support::image(json!({
        "constants": [{"id": "condition", "type": {"kind": 3, "primitive": 1}, "value": true}],
        "functions": [{"id": "fn.Main", "code":slot_code(json!([{"kind":3,"primitive":1},{"kind":3,"primitive":3}]), &[
            ("const",json!({"constant":"condition"}),json!({"outputs":[0]})),
            ("jump_if",json!({"label":"join"}),json!({"inputs":[[0,0]],"release":[0]})),
            ("zero",json!({"type":{"kind":3,"primitive":3}}),json!({"outputs":[1]})),
            ("label",json!({"label":"join"}),json!({})),
            ("pop",json!({}),json!({"inputs":[[0,1]],"release":[1]}))])}]
    }));
    assert_eq!(
        Program::load(&image, LoadLimits::default())
            .err()
            .unwrap()
            .code,
        "invalid_operand"
    );
}
