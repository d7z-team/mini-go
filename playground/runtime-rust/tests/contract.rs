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
