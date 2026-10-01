use super::*;
use crate::contract::{canonical_hash, canonical_json};

#[path = "../../tests/support/slots.rs"]
mod slots;
pub(crate) use slots::slot_code;

/// Rebuild a real, validated execution image after changing its single artifact.
pub(super) fn program_with_artifact(update: impl FnOnce(&mut serde_json::Value)) -> Arc<Program> {
    let mut image: wire::ExecutionImage =
        serde_json::from_slice(include_bytes!("../../examples/blocks/arithmetic.json")).unwrap();
    let package = image
        .packages
        .as_mut()
        .unwrap()
        .values_mut()
        .next()
        .unwrap();
    let mut artifact: serde_json::Value =
        serde_json::from_str(package.artifact.as_ref().unwrap().get()).unwrap();
    update(&mut artifact);
    let artifact: wire::Artifact = serde_json::from_value(artifact).unwrap();
    package.artifact_hash = canonical_hash(&artifact).unwrap();
    package.artifact = Some(
        serde_json::value::RawValue::from_string(
            String::from_utf8(canonical_json(&artifact).unwrap()).unwrap(),
        )
        .unwrap(),
    );
    image.hash.clear();
    image.hash = canonical_hash(&image).unwrap();
    Arc::new(Program::load(&canonical_json(&image).unwrap(), Default::default()).unwrap())
}
