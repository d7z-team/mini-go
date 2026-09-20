mod support;

use mini_go::{loader::LoadLimits, program::Program};
use serde_json::json;

#[test]
fn missing_dependency_export_is_rejected_at_load() {
    use mini_go::{
        contract::{canonical_hash, canonical_json},
        contract_generated::ExecutionImage,
    };
    let dependency: ExecutionImage = serde_json::from_slice(&support::image(json!({
        "module": {"path": "dependency", "package": "dependency"},
        "functions": [{"id": "fn.Main"}]
    })))
    .unwrap();
    let mut image: ExecutionImage = serde_json::from_slice(&support::image(json!({
        "requirements": [{"kind": "source", "module_path": "dependency", "exports": ["Missing"]}],
        "functions": [{"id": "fn.Main"}]
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
        json!({"requirements":[{"kind":"source","module_path":"test"}], "functions":[{"id":"fn.Main"}]}),
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
            json!({"id": "fn.Main", "instructions": [{"op": "pop"}]}),
            "invalid_stack",
        ),
        (
            json!({"id": "fn.Main", "instructions": [{"op": "jump", "payload": {"label": "missing"}}]}),
            "invalid_jump",
        ),
        (
            json!({"id": "fn.Main", "result_locals": ["missing"], "signature": {"results": [{"kind": 3, "primitive": 3}]}}),
            "invalid_result_local",
        ),
        (
            json!({"id": "fn.Main", "instructions": [{"op": "make_closure", "payload": {"function": "fn.Main", "captures": [{"kind": "local", "local": "missing"}]}}, {"op": "pop"}]}),
            "invalid_operand",
        ),
        (
            json!({"id": "fn.Main", "instructions": [{"op": "call_ffi", "payload": {"arg_count": 1, "result_count": 3}}]}),
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
fn branch_join_requires_a_consistent_operand_stack() {
    let image = support::image(json!({
        "constants": [{"id": "condition", "type": {"kind": 3, "primitive": 1}, "value": true}],
        "functions": [{"id": "fn.Main", "instructions": [
            {"op": "const", "payload": {"constant": "condition"}},
            {"op": "jump_if", "payload": {"label": "join"}},
            {"op": "zero", "payload": {"type": {"kind": 3, "primitive": 3}}},
            {"op": "label", "payload": {"label": "join"}}, {"op": "pop"}
        ]}]
    }));
    assert_eq!(
        Program::load(&image, LoadLimits::default())
            .err()
            .unwrap()
            .code,
        "invalid_stack"
    );
}
