use mini_go::{
    contract::{canonical_hash, canonical_json},
    contract_generated as wire,
};
use serde_json::json;

mod slots;
#[allow(unused_imports)]
pub use slots::slot_code;

/// Builds a sealed current-contract image around a minimal test artifact.
pub fn image(mut artifact: serde_json::Value) -> Vec<u8> {
    let contract: serde_json::Value = serde_json::from_str(wire::CONTRACT_JSON).unwrap();
    artifact["format"] = contract["spec"]["format"].clone();
    artifact["version"] = contract["spec"]["version"].clone();
    artifact["opcode_set"] = contract["spec"]["opcode_set"].clone();
    if artifact.get("module").is_none() {
        artifact["module"] = json!({"path": "test", "package": "main"});
    }
    let artifact: wire::Artifact = serde_json::from_value(artifact).unwrap();
    let module = &artifact.module.path;
    let mut image: wire::ExecutionImage = serde_json::from_value(json!({
        "format": contract["execution_format"], "version": contract["execution_version"],
        "contract_id": contract["execution_contract"], "compiler_id": contract["compiler_id"],
        "root": module, "target": {"tags": ["minigo"]},
        "entries": [{"name": "default", "module_path": module, "function_id": "fn.Main"}],
        "packages": {(module): {"artifact": {}, "artifact_hash": canonical_hash(&artifact).unwrap()}}
    }))
    .unwrap();
    image
        .packages
        .as_mut()
        .unwrap()
        .get_mut(module)
        .unwrap()
        .artifact = Some(
        serde_json::value::RawValue::from_string(
            String::from_utf8(canonical_json(&artifact).unwrap()).unwrap(),
        )
        .unwrap(),
    );
    image.hash = canonical_hash(&image).unwrap();
    canonical_json(&image).unwrap()
}
