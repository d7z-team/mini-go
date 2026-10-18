use super::wire;
use serde_json::json;

/// Completes compact hand-written test artifacts with valid one-instruction
/// basic blocks. Tests that exercise block validation provide `blocks`
/// explicitly and are left unchanged.
pub fn complete_blocks(artifact: &mut serde_json::Value, contract: &serde_json::Value) {
    let label_opcode = contract["spec"]["opcodes"]
        .as_array()
        .unwrap()
        .iter()
        .position(|entry| entry["op"] == "label")
        .unwrap() as u64
        + 1;
    let Some(functions) = artifact
        .get_mut("functions")
        .and_then(|value| value.as_array_mut())
    else {
        return;
    };
    for function in functions {
        let Some(code) = function
            .get_mut("code")
            .and_then(|value| value.as_object_mut())
        else {
            continue;
        };
        if code.contains_key("blocks") {
            continue;
        }
        let mut blocks = Vec::new();
        if let Some(instructions) = code.get("instructions").and_then(|value| value.as_array()) {
            for instruction in instructions {
                let words = instruction.as_array().unwrap();
                if words[0].as_u64().unwrap() & 0xffff == label_opcode {
                    continue;
                }
                blocks.push(json!({
                    "first": blocks.len(),
                    "count": 1,
                    "fallthrough": u32::MAX,
                    "cost": words[3]
                }));
            }
        }
        code.insert("blocks".into(), blocks.into());
    }
}

/// Assemble explicit micro-instructions using the generated opcode catalog.
/// Every input, output and release is supplied by the behavioral fixture.
#[allow(dead_code)]
pub fn micro_code(
    types: serde_json::Value,
    operations: &[(&str, serde_json::Value, serde_json::Value)],
) -> serde_json::Value {
    let contract: serde_json::Value = serde_json::from_str(wire::CONTRACT_JSON).unwrap();
    let catalog = contract["spec"]["opcodes"].as_array().unwrap();
    let mut code = json!({"types":types,"instructions":[],"operands":[],"descriptors":{}});
    for (pc, (name, payload, operands)) in operations.iter().enumerate() {
        let opcode = catalog
            .iter()
            .position(|entry| entry["op"] == *name)
            .unwrap();
        let mut descriptor = 0;
        if let Some(kind) = catalog[opcode]["payload"].as_str() {
            let key = kind.replace('_', "");
            let table = code["descriptors"]
                .as_object_mut()
                .unwrap()
                .entry(key)
                .or_insert_with(|| json!([]))
                .as_array_mut()
                .unwrap();
            descriptor = table.len();
            table.push(payload.clone());
        }
        code["instructions"]
            .as_array_mut()
            .unwrap()
            .push(json!([opcode + 1, descriptor, pc, 1]));
        code["operands"]
            .as_array_mut()
            .unwrap()
            .push(operands.clone());
    }
    code
}
