#[cfg(not(target_arch = "wasm32"))]
use crate::{error::RuntimeError, ffi::Cancellation};
#[cfg(not(target_arch = "wasm32"))]
use base64::Engine;
use serde::{Deserialize, Serialize};
#[cfg(not(target_arch = "wasm32"))]
use tokio::io::AsyncReadExt;

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct SourceTree {
    pub module_path: String,
    pub editable: Option<bool>,
    pub files: Vec<TreeFile>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct TreeFile {
    pub path: String,
    /// Base64-encoded bytes, including binary resources.
    pub data: String,
    #[serde(rename = "URI")]
    pub uri: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct SourceFile {
    pub path: String,
    pub text: String,
    pub origin_path: String,
    #[serde(rename = "URI")]
    pub uri: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct Resource {
    pub path: String,
    pub source_path: String,
    pub data: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct SourcePackage {
    pub editable: Option<bool>,
    pub namespace: String,
    pub package_path: String,
    pub module_path: String,
    pub files: Option<Vec<SourceFile>>,
    pub test_files: Option<Vec<SourceFile>>,
    pub resources: Option<Vec<Resource>>,
    pub selection_target: SourceTarget,
    pub source_candidates: Option<Vec<SourceCandidate>>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct SourceTarget {
    #[serde(default)]
    pub tags: Option<Vec<String>>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct SourceCandidate {
    pub path: String,
    pub hash: String,
    pub size: u64,
    pub selected: bool,
    pub test: bool,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "PascalCase")]
pub struct SourcePackages {
    pub packages: Vec<SourcePackage>,
}

/// Reads a bounded local source tree. Dependency policy belongs to the host.
#[cfg(not(target_arch = "wasm32"))]
pub async fn read_directory(
    module_path: &str,
    root: &std::path::Path,
    cancel: &Cancellation,
) -> Result<SourceTree, RuntimeError> {
    let read = async {
        let mut pending = vec![std::path::PathBuf::new()];
        let mut files = Vec::new();
        let mut bytes = 0_u64;
        while let Some(relative) = pending.pop() {
            if cancel.is_cancelled() {
                return Err(RuntimeError::new(
                    "canceled",
                    "sources",
                    "directory read canceled",
                ));
            }
            let absolute = root.join(&relative);
            let metadata = tokio::fs::symlink_metadata(&absolute)
                .await
                .map_err(|e| RuntimeError::new("provider", "sources", e.to_string()))?;
            if metadata.file_type().is_symlink() {
                continue;
            }
            if metadata.is_dir() {
                let mut entries = tokio::fs::read_dir(absolute)
                    .await
                    .map_err(|e| RuntimeError::new("provider", "sources", e.to_string()))?;
                while let Some(entry) = entries
                    .next_entry()
                    .await
                    .map_err(|e| RuntimeError::new("provider", "sources", e.to_string()))?
                {
                    if matches!(
                        entry.file_name().to_str(),
                        Some(".git" | ".hg" | ".svn" | ".bzr")
                    ) {
                        continue;
                    }
                    if pending.len() + files.len() >= 100_000 {
                        return Err(RuntimeError::new(
                            "budget",
                            "sources",
                            "too many source files",
                        ));
                    }
                    pending.push(relative.join(entry.file_name()));
                }
            } else if metadata.is_file() {
                if metadata.len() > 64 << 20 || bytes + metadata.len() > 64 << 20 {
                    return Err(RuntimeError::new(
                        "budget",
                        "sources",
                        "source directory exceeds byte budget",
                    ));
                }
                let file = tokio::fs::File::open(absolute)
                    .await
                    .map_err(|e| RuntimeError::new("provider", "sources", e.to_string()))?;
                let mut data = Vec::new();
                file.take((64 << 20) - bytes + 1)
                    .read_to_end(&mut data)
                    .await
                    .map_err(|e| RuntimeError::new("provider", "sources", e.to_string()))?;
                bytes += data.len() as u64;
                if bytes > 64 << 20 {
                    return Err(RuntimeError::new(
                        "budget",
                        "sources",
                        "source directory exceeds byte budget",
                    ));
                }
                let path = relative
                    .to_str()
                    .ok_or_else(|| {
                        RuntimeError::new("provider", "sources", "non UTF-8 source path")
                    })?
                    .replace('\\', "/");
                files.push(TreeFile {
                    path,
                    data: base64::engine::general_purpose::STANDARD.encode(data),
                    uri: None,
                });
            } else {
                return Err(RuntimeError::new(
                    "provider",
                    "sources",
                    "source directory contains a non-regular file",
                ));
            }
        }
        files.sort_by(|a, b| a.path.cmp(&b.path));
        Ok(SourceTree {
            module_path: module_path.to_owned(),
            editable: None,
            files,
        })
    };
    tokio::select! {
     biased;
     _ = cancel.cancelled() => Err(RuntimeError::new("canceled", "sources", "directory read canceled")),
     result = read => result,
    }
}
