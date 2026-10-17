use mini_go::contract::{canonical_hash, canonical_json, strict_json_from_str};
use mini_go::contract_generated::*;
use serde::Deserialize;

#[derive(Deserialize)]
struct Vector {
    name: String,
    model: String,
    input: Box<serde_json::value::RawValue>,
    canonical: String,
    hash: String,
    #[serde(default)]
    error: String,
}

#[test]
fn canonical_wire_matches_go_without_losing_raw_numbers() {
    let vectors: Vec<Vector> =
        serde_json::from_slice(include_bytes!("../../../testdata/runtime/wire.json")).unwrap();
    for vector in vectors {
        macro_rules! compare {
            ($model:ty) => {{
                let decoded = strict_json_from_str::<$model>(vector.input.get());
                if vector.error.is_empty() {
                    let value = decoded.unwrap_or_else(|error| panic!("{}: {error}", vector.name));
                    assert_eq!(
                        canonical_json(&value).unwrap(),
                        vector.canonical.as_bytes(),
                        "{}",
                        vector.name
                    );
                    assert_eq!(
                        canonical_hash(&value).unwrap(),
                        vector.hash,
                        "{}",
                        vector.name
                    );
                } else {
                    assert!(
                        decoded.is_err(),
                        "{}: Go rejected {}",
                        vector.name,
                        vector.error
                    );
                }
            }};
        }
        match vector.model.as_str() {
            "TypeRef" => compare!(TypeRef),
            "TypeNode" => compare!(TypeNode),
            "ExecutionImage" => compare!(ExecutionImage),
            "ProgramSymbols" => compare!(ProgramSymbols),
            "PackageArchive" => compare!(PackageArchive),
            "Constant" => compare!(Constant),
            model => panic!("unhandled oracle model: {model}"),
        }
    }
}

#[test]
fn payload_rejects_unknown_fields_and_trailing_values() {
    for input in [
        r#"{"constant":"one","unexpected":true}"#,
        r#"{"constant":"one"} {}"#,
    ] {
        assert!(strict_json_from_str::<ConstPayload>(input).is_err());
    }
}

#[test]
fn storage_cost_contract_uses_the_shared_logical_widths() {
    assert_eq!(STORAGE_VERSION, 1);
    assert_eq!(STORAGE_BYTE, 1);
    assert_eq!(STORAGE_SCALAR, 8);
    assert_eq!(STORAGE_REFERENCE, 8);
    assert_eq!(STORAGE_DYNAMIC, 24);
    assert_eq!(STORAGE_STRING, 24);
    assert_eq!(STORAGE_SLICE, 32);
    assert_eq!(STORAGE_POINTER, 24);
    assert_eq!(STORAGE_RECORD_HEADER, 16);
    assert_eq!(STORAGE_FRAME_HEADER, 32);
    assert_eq!(STORAGE_CONTROL_RECORD, 64);
    assert_eq!(STORAGE_MAP_BUCKET, 4);
    assert_eq!(STORAGE_MAP_ENTRY, 16);
    assert_eq!(STORAGE_PRIMITIVE_BYTES[PrimitiveBool as usize], 1);
    assert_eq!(STORAGE_PRIMITIVE_BYTES[PrimitiveString as usize], 24);
    assert_eq!(STORAGE_PRIMITIVE_BYTES[PrimitiveComplex128 as usize], 16);
    assert_eq!(STORAGE_PRIMITIVE_BYTES[PrimitiveFunction as usize], 8);
}

#[test]
fn storage_cost_contract_rejects_overflow_before_admission() {
    assert_eq!(checked_storage_bytes(16, 7, 8), Some(72));
    assert_eq!(checked_storage_bytes(16, u64::MAX, 0), Some(16));
    assert_eq!(checked_storage_bytes(0, u64::MAX, 8), None);
    assert_eq!(checked_storage_bytes(u64::MAX, 0, 0), None);
}
