//! Explicit host overrides shared by runtime and compiler entry points.

use mini_go::{Limits, LoadOptions, RuntimeError};
use serde::Deserialize;

#[derive(Default, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct ExecutionOptions {
    max_steps: Option<i64>,
    max_frames: Option<usize>,
    max_frame_cache_bytes: Option<usize>,
    max_retained_revisions: Option<usize>,
    max_dynamic_types: Option<usize>,
    max_dynamic_type_bytes: Option<u64>,
    max_objects: Option<usize>,
    max_heap_bytes: Option<u64>,
    max_allocated_bytes: Option<u64>,
    max_string_bytes: Option<usize>,
    max_value_depth: Option<usize>,
    max_sequence_elements: Option<usize>,
    max_tasks: Option<usize>,
    max_pending_calls: Option<usize>,
    max_ffi_bytes: Option<usize>,
    max_ffi_result_bytes: Option<usize>,
}

impl ExecutionOptions {
    pub fn apply(self, mut limits: Limits) -> Result<Limits, RuntimeError> {
        if let Some(steps) = self.max_steps.filter(|steps| *steps != 0) {
            limits.max_steps = steps;
        }
        macro_rules! positive {
            ($($field:ident),+ $(,)?) => {$(
                if let Some(value) = self.$field {
                    if value == 0 {
                        return Err(RuntimeError::new("invalid_limits", stringify!($field), "expected a positive limit"));
                    }
                    limits.$field = value;
                }
            )+};
        }
        positive!(
            max_frames,
            max_retained_revisions,
            max_dynamic_types,
            max_dynamic_type_bytes,
            max_objects,
            max_heap_bytes,
            max_allocated_bytes,
            max_string_bytes,
            max_value_depth,
            max_sequence_elements,
            max_tasks,
            max_pending_calls,
            max_ffi_bytes,
            max_ffi_result_bytes
        );
        if let Some(bytes) = self.max_frame_cache_bytes {
            limits.max_frame_cache_bytes = bytes;
        }
        limits.normalize()
    }
}

#[derive(Default, Deserialize)]
#[serde(default, rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct ImageOptions {
    max_image_bytes: Option<usize>,
    max_artifact_bytes: Option<usize>,
    max_packages: Option<usize>,
    max_type_nodes: Option<usize>,
}

impl ImageOptions {
    pub fn apply(self, mut limits: LoadOptions) -> Result<LoadOptions, RuntimeError> {
        for (name, value, destination) in [
            (
                "maxImageBytes",
                self.max_image_bytes,
                &mut limits.max_image_bytes,
            ),
            (
                "maxArtifactBytes",
                self.max_artifact_bytes,
                &mut limits.max_artifact_bytes,
            ),
            ("maxPackages", self.max_packages, &mut limits.max_packages),
            (
                "maxTypeNodes",
                self.max_type_nodes,
                &mut limits.max_type_nodes,
            ),
        ] {
            if let Some(value) = value {
                if value == 0 {
                    return Err(RuntimeError::new(
                        "invalid_limits",
                        name,
                        "expected a positive limit",
                    ));
                }
                *destination = value;
            }
        }
        Ok(limits)
    }
}
