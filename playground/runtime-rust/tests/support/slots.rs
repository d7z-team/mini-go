use super::wire;
use serde_json::json;

/// Assemble explicit slot instructions using the generated opcode catalog.
/// Every input, output and release is supplied by the behavioral fixture.
#[allow(dead_code)]
pub fn slot_code(
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
            .push(json!([opcode + 1, descriptor, pc]));
        code["operands"]
            .as_array_mut()
            .unwrap()
            .push(operands.clone());
    }
    code
}
