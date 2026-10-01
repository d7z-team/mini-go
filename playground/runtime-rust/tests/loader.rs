use mini_go::{
    contract::canonical_json,
    contract_generated as wire,
    loader::{DecodedImage, LoadLimits},
};
use serde::Deserialize;
use std::io::Write;

#[path = "support/execution_vectors.rs"]
mod execution_vectors;

#[derive(Deserialize)]
struct Vector {
    name: String,
    image: wire::ExecutionImage,
}

#[test]
fn current_go_compiler_images_decode_at_all_optimization_levels() {
    let vectors: Vec<Vector> = execution_vectors::load();
    for vector in vectors {
        let bytes = canonical_json(&vector.image).unwrap();
        let decoded = DecodedImage::decode(&bytes, LoadLimits::default())
            .unwrap_or_else(|error| panic!("{}: {error}", vector.name));
        assert_eq!(decoded.image().hash, vector.image.hash);
        assert!(decoded.artifacts().contains_key(&vector.image.root));
    }
}

#[test]
fn malformed_image_and_limits_fail_without_publishing_a_graph() {
    let vectors: Vec<Vector> = execution_vectors::load();
    let image = &vectors[0].image;
    let bytes = canonical_json(image).unwrap();
    let limits = LoadLimits {
        max_image_bytes: bytes.len() - 1,
        ..LoadLimits::default()
    };
    assert_eq!(
        DecodedImage::decode(&bytes, limits).err().unwrap().code,
        "load_limit"
    );
    let mut wrong = image.clone();
    wrong.hash = "0".repeat(64);
    assert_eq!(
        DecodedImage::decode(&canonical_json(&wrong).unwrap(), LoadLimits::default())
            .err()
            .unwrap()
            .code,
        "hash_mismatch"
    );
    wrong.compiler_id = "wrong".to_owned();
    assert_eq!(
        DecodedImage::decode(&canonical_json(&wrong).unwrap(), LoadLimits::default())
            .err()
            .unwrap()
            .code,
        "identity_mismatch"
    );
}

#[test]
fn compressed_image_checks_expansion_checksum_and_trailing_data() {
    let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    encoder.write_all(&vec![b' '; 4096]).unwrap();
    let bomb = encoder.finish().unwrap();
    let limits = LoadLimits {
        max_image_bytes: 256,
        ..LoadLimits::default()
    };
    assert_eq!(
        DecodedImage::decode_gzip(&bomb, limits).err().unwrap().code,
        "load_limit"
    );

    let vectors: Vec<Vector> = execution_vectors::load();
    let mut encoder = flate2::write::GzEncoder::new(Vec::new(), flate2::Compression::default());
    encoder
        .write_all(&canonical_json(&vectors[0].image).unwrap())
        .unwrap();
    let compressed = encoder.finish().unwrap();
    assert_eq!(
        DecodedImage::decode_gzip(&compressed, LoadLimits::default())
            .unwrap()
            .image()
            .hash,
        vectors[0].image.hash
    );
    let mut corrupt = compressed.clone();
    let crc = corrupt.len() - 8;
    corrupt[crc] ^= 1;
    assert_eq!(
        DecodedImage::decode_gzip(&corrupt, LoadLimits::default())
            .err()
            .unwrap()
            .code,
        "invalid_gzip"
    );
    let truncated = compressed[..compressed.len() - 1].to_vec();
    let mut multiple = compressed.clone();
    multiple.extend_from_slice(&compressed);
    for invalid in [truncated, multiple] {
        assert_eq!(
            DecodedImage::decode_gzip(&invalid, LoadLimits::default())
                .err()
                .unwrap()
                .code,
            "invalid_gzip"
        );
    }
    let mut trailing = compressed;
    trailing.push(0);
    assert_eq!(
        DecodedImage::decode_gzip(&trailing, LoadLimits::default())
            .err()
            .unwrap()
            .code,
        "invalid_gzip"
    );
}
