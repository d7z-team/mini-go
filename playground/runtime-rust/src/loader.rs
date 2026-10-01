//! Image identity, package closure and structural type decoding. Executable
//! control-flow verification is a separate step from this wire boundary.

use crate::{
    contract::canonical_hash, contract_generated as wire, error::RuntimeError, types::TypeRegistry,
};
use serde::Deserialize;
use std::collections::{BTreeMap, HashSet};
use std::io::Read;
use std::sync::OnceLock;

#[derive(Deserialize)]
struct Contract {
    compiler_id: String,
    execution_format: String,
    execution_version: i64,
    execution_contract: String,
    spec: Spec,
}

#[derive(Deserialize)]
struct Spec {
    format: String,
    version: i64,
    opcode_set: String,
}

fn contract() -> &'static Contract {
    static CONTRACT: OnceLock<Contract> = OnceLock::new();
    CONTRACT.get_or_init(|| {
        serde_json::from_str(wire::CONTRACT_JSON).expect("generated contract must be valid")
    })
}

#[derive(Clone, Copy)]
pub struct LoadLimits {
    pub max_image_bytes: usize,
    pub max_artifact_bytes: usize,
    pub max_packages: usize,
    pub max_type_nodes: usize,
}

impl Default for LoadLimits {
    fn default() -> Self {
        Self {
            max_image_bytes: 64 << 20,
            max_artifact_bytes: 64 << 20,
            max_packages: 1024,
            max_type_nodes: 100_000,
        }
    }
}

/// A decoded package graph, not yet a prepared executable. Its identity and
/// type references have been checked, but this does not validate instructions.
pub struct DecodedImage {
    image: wire::ExecutionImage,
    artifacts: BTreeMap<String, wire::Artifact>,
    types: TypeRegistry,
}

impl DecodedImage {
    /// Decodes one gzip member with bounded expansion and checksum validation.
    pub fn decode_gzip(bytes: &[u8], limits: LoadLimits) -> Result<Self, RuntimeError> {
        if bytes.len() > limits.max_image_bytes {
            return Err(RuntimeError::new(
                "load_limit",
                "$",
                "compressed image byte limit exceeded",
            ));
        }
        let mut decoder = flate2::bufread::GzDecoder::new(bytes);
        let mut expanded = Vec::new();
        let bound = u64::try_from(limits.max_image_bytes)
            .unwrap_or(u64::MAX)
            .saturating_add(1);
        decoder
            .by_ref()
            .take(bound)
            .read_to_end(&mut expanded)
            .map_err(|error| RuntimeError::new("invalid_gzip", "$", error.to_string()))?;
        if expanded.len() > limits.max_image_bytes {
            return Err(RuntimeError::new(
                "load_limit",
                "$",
                "expanded image byte limit exceeded",
            ));
        }
        if !decoder.into_inner().is_empty() {
            return Err(RuntimeError::new("invalid_gzip", "$", "trailing gzip data"));
        }
        Self::decode(&expanded, limits)
    }

    pub fn decode(bytes: &[u8], limits: LoadLimits) -> Result<Self, RuntimeError> {
        if bytes.len() > limits.max_image_bytes {
            return Err(RuntimeError::new(
                "load_limit",
                "$",
                "image byte limit exceeded",
            ));
        }
        let mut image: wire::ExecutionImage = serde_json::from_slice(bytes)?;
        let contract = contract();
        if image.format != contract.execution_format
            || image.version != contract.execution_version
            || image.contract_id != contract.execution_contract
            || image.compiler_id != contract.compiler_id
        {
            return Err(RuntimeError::new(
                "identity_mismatch",
                "$",
                "execution image identity mismatch",
            ));
        }
        let packages = image.packages.as_ref().ok_or_else(|| {
            RuntimeError::new("invalid_image", "packages", "missing package graph")
        })?;
        if packages.len() > limits.max_packages {
            return Err(RuntimeError::new(
                "load_limit",
                "packages",
                "package count exceeded",
            ));
        }
        if image.root.trim().is_empty() || !packages.contains_key(&image.root) {
            return Err(RuntimeError::new(
                "invalid_image",
                "root",
                "root package is missing",
            ));
        }
        if image.target.tags.is_empty()
            || !image.target.tags.iter().any(|tag| tag == "minigo")
            || image.target.tags.windows(2).any(|pair| pair[0] >= pair[1])
            || image.target.tags.iter().any(|tag| {
                tag.is_empty()
                    || !tag
                        .bytes()
                        .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_' || byte == b'.')
            })
        {
            return Err(RuntimeError::new(
                "invalid_image",
                "target",
                "target tags are not canonical",
            ));
        }
        if image.capabilities.iter().any(|name| name.trim().is_empty())
            || image.capabilities.windows(2).any(|pair| pair[0] >= pair[1])
        {
            return Err(RuntimeError::new(
                "invalid_image",
                "capabilities",
                "capabilities are not sorted unique names",
            ));
        }
        let mut total_bytes = 0usize;
        for archive in packages.values() {
            let raw = archive.artifact.as_ref().ok_or_else(|| {
                RuntimeError::new("invalid_image", "packages", "missing artifact")
            })?;
            total_bytes = total_bytes
                .checked_add(raw.get().len())
                .filter(|total| *total <= limits.max_artifact_bytes)
                .ok_or_else(|| {
                    RuntimeError::new("load_limit", "packages", "artifact byte limit exceeded")
                })?;
        }
        // The image is still exclusively owned here; hashing its unsealed form
        // need not duplicate every package's raw artifact bytes.
        let expected_hash = std::mem::take(&mut image.hash);
        let actual_hash = canonical_hash(&image)?;
        image.hash = expected_hash;
        if actual_hash != image.hash {
            return Err(RuntimeError::new(
                "hash_mismatch",
                "hash",
                "image content hash mismatch",
            ));
        }
        let mut artifacts = BTreeMap::new();
        let mut type_nodes = 0usize;
        for (path, archive) in packages {
            let artifact: wire::Artifact =
                serde_json::from_str(archive.artifact.as_ref().unwrap().get())?;
            if artifact.format != contract.spec.format
                || artifact.version != contract.spec.version
                || artifact.opcode_set != contract.spec.opcode_set
                || artifact.module.path != *path
            {
                return Err(RuntimeError::new(
                    "identity_mismatch",
                    path,
                    "artifact identity or module path mismatch",
                ));
            }
            if canonical_hash(&artifact)? != archive.artifact_hash {
                return Err(RuntimeError::new(
                    "hash_mismatch",
                    path,
                    "artifact content hash mismatch",
                ));
            }
            type_nodes = type_nodes
                .checked_add(artifact.type_table.nodes.len())
                .filter(|count| *count <= limits.max_type_nodes)
                .ok_or_else(|| RuntimeError::new("load_limit", path, "type node count exceeded"))?;
            artifacts.insert(path.clone(), artifact);
        }
        for (module, artifact) in &artifacts {
            for requirement in artifact.requirements.iter() {
                let dependency = artifacts.get(&requirement.module_path).ok_or_else(|| {
                    RuntimeError::new("missing_dependency", module, &requirement.module_path)
                })?;
                if requirement.kind != "source"
                    || (!requirement.hash.is_empty()
                        && packages[&requirement.module_path].artifact_hash != requirement.hash)
                {
                    return Err(RuntimeError::new(
                        "identity_mismatch",
                        module,
                        "dependency kind or hash mismatch",
                    ));
                }
                for name in requirement.exports.iter() {
                    if !dependency.exports.iter().any(|export| export.name == *name)
                        && !dependency.type_table.nodes.iter().any(|node| {
                            node.kind == wire::Named
                                && node.identity.module_path == requirement.module_path
                                && node.identity.decl_id == *name
                        })
                    {
                        return Err(RuntimeError::new("missing_export", module, name));
                    }
                }
            }
        }
        let mut visited = HashSet::new();
        let mut visiting = HashSet::new();
        for root in artifacts.keys() {
            let mut pending = vec![(root.as_str(), false)];
            while let Some((module, completed)) = pending.pop() {
                if completed {
                    visiting.remove(module);
                    visited.insert(module);
                    continue;
                }
                if visited.contains(module) {
                    continue;
                }
                if !visiting.insert(module) {
                    return Err(RuntimeError::new(
                        "dependency_cycle",
                        module,
                        "module dependency cycle",
                    ));
                }
                pending.push((module, true));
                for requirement in artifacts[module].requirements.iter().rev() {
                    pending.push((&requirement.module_path, false));
                }
            }
        }
        let types = TypeRegistry::new(artifacts.values())?;
        let mut entries = HashSet::new();
        if image.entries.is_empty() {
            return Err(RuntimeError::new(
                "invalid_image",
                "entries",
                "no entry points",
            ));
        }
        for entry in image.entries.iter() {
            if entry.name.is_empty()
                || entry.module_path != image.root
                || !entries.insert(&entry.name)
                || !artifacts.get(&entry.module_path).is_some_and(|artifact| {
                    artifact
                        .functions
                        .iter()
                        .any(|function| function.id == entry.function_id)
                })
            {
                return Err(RuntimeError::new(
                    "invalid_image",
                    "entries",
                    "duplicate, empty or unresolved entry",
                ));
            }
        }
        for (module, artifact) in &artifacts {
            for function in artifact.functions.iter() {
                types.validate_signature(module, &function.signature)?;
            }
        }
        Ok(Self {
            image,
            artifacts,
            types,
        })
    }

    pub fn image(&self) -> &wire::ExecutionImage {
        &self.image
    }
    pub fn artifacts(&self) -> &BTreeMap<String, wire::Artifact> {
        &self.artifacts
    }
    pub fn types(&self) -> &TypeRegistry {
        &self.types
    }

    // Program consumes DecodedImage exclusively. Once its execution plans are
    // validated, retain declarations for patch/type lookup and the original
    // sealed image for export, without another decoded copy of every body.
    pub(crate) fn release_function_bodies(&mut self) {
        for artifact in self.artifacts.values_mut() {
            for function in artifact.functions.iter_mut() {
                function.code = None;
            }
        }
    }
}
