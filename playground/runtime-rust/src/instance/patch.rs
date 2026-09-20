//! Read-only preparation and atomic publication at an owner boundary.
use super::*;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RevisionInfo {
    pub generation: u64,
    pub hash: String,
    pub symbols_hash: String,
}
pub struct PatchPlan {
    owner: u64,
    base: RevisionInfo,
    target: Arc<Program>,
    types: TypeRegistry,
    globals: Vec<((String, String), Value)>,
    changed_modules: Vec<String>,
}
#[derive(Clone, Debug)]
pub struct PatchResult {
    pub previous: RevisionInfo,
    pub current: RevisionInfo,
    pub changed_modules: Vec<String>,
}
impl PatchPlan {
    pub fn base(&self) -> &RevisionInfo {
        &self.base
    }
    pub fn target_hash(&self) -> &str {
        &self.target.image().hash
    }
    pub fn changed_modules(&self) -> &[String] {
        &self.changed_modules
    }
}
impl Instance {
    fn check_revision_capacity(&self) -> Result<(), RuntimeError> {
        let retained = self
            .retired_revisions
            .values()
            .filter(|revision| revision.strong_count() != 0)
            .count();
        if retained.saturating_add(2) > self.limits.max_retained_revisions {
            return Err(RuntimeError::new(
                "resource_limit",
                "patch",
                "retained revision limit exceeded",
            ));
        }
        Ok(())
    }
    pub fn revision(&self) -> RevisionInfo {
        RevisionInfo {
            generation: self.revision.generation,
            hash: self.revision.program.image().hash.clone(),
            symbols_hash: self
                .revision
                .program
                .symbols()
                .map(|symbols| symbols.hash.clone())
                .unwrap_or_default(),
        }
    }
    pub fn retained_revisions(&self) -> Vec<RevisionInfo> {
        if self.closed {
            return Vec::new();
        }
        self.retired_revisions
            .values()
            .filter_map(std::sync::Weak::upgrade)
            .chain(std::iter::once(self.revision.clone()))
            .map(|revision| RevisionInfo {
                generation: revision.generation,
                hash: revision.program.image().hash.clone(),
                symbols_hash: revision
                    .program
                    .symbols()
                    .map(|symbols| symbols.hash.clone())
                    .unwrap_or_default(),
            })
            .collect()
    }
    pub fn prepare_patch(&self, target: Arc<Program>) -> Result<PatchPlan, RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new("closed", "patch", "instance is closed"));
        }
        if self.faulted {
            return Err(RuntimeError::new(
                "instance_faulted",
                "patch",
                "instance execution failed",
            ));
        }
        self.check_revision_capacity()?;
        let old = &self.revision.program;
        if old.image().hash == target.image().hash
            && old.symbols().map(|symbols| &symbols.hash)
                == target.symbols().map(|symbols| &symbols.hash)
        {
            return Err(RuntimeError::new(
                "unchanged",
                "patch",
                "target is already current",
            ));
        }
        if old.image().root != target.image().root {
            return Err(RuntimeError::new(
                "root_changed",
                "patch",
                "root module changed",
            ));
        }
        if old.image().target.tags[..] != target.image().target.tags[..] {
            return Err(RuntimeError::new(
                "target_changed",
                "patch",
                "build target changed",
            ));
        }
        if target
            .image()
            .capabilities
            .iter()
            .any(|capability| !self.host_capabilities.contains(capability))
        {
            return Err(RuntimeError::new(
                "capability_unavailable",
                "patch",
                "host capability is unavailable",
            ));
        }
        for (module, artifact) in old.decoded.artifacts() {
            let next =
                target.decoded.artifacts().get(module).ok_or_else(|| {
                    RuntimeError::new("module_removed", module, "module was removed")
                })?;
            if artifact.module.package != next.module.package {
                return Err(RuntimeError::new(
                    "package_changed",
                    module,
                    "package name changed",
                ));
            }
            if artifact.globals.len() != next.globals.len() {
                return Err(RuntimeError::new(
                    "global_shape_changed",
                    module,
                    "global declarations changed",
                ));
            }
        }
        let types = self
            .types
            .overlay(target.decoded.types(), self.limits.max_sequence_elements)?;
        for (module, artifact) in old.decoded.artifacts() {
            let next = &target.decoded.artifacts()[module];
            for global in artifact.globals.iter() {
                let replacement = next
                    .globals
                    .iter()
                    .find(|next| next.id == global.id)
                    .ok_or_else(|| {
                        RuntimeError::new("global_shape_changed", module, "global was removed")
                    })?;
                if !types.identical(
                    &old.decoded.types().resolve(module, &global.r#type)?,
                    &target
                        .decoded
                        .types()
                        .resolve(module, &replacement.r#type)?,
                )? {
                    return Err(RuntimeError::new(
                        "global_shape_changed",
                        module,
                        "global type changed",
                    ));
                }
            }
            for export in artifact.exports.iter() {
                let replacement = next
                    .exports
                    .iter()
                    .find(|next| next.name == export.name)
                    .ok_or_else(|| {
                        RuntimeError::new("export_shape_changed", module, "export was removed")
                    })?;
                if export.kind != replacement.kind
                    || export.id != replacement.id
                    || export.untyped != replacement.untyped
                    || !types.identical(
                        &old.decoded.types().resolve(module, &export.r#type)?,
                        &target
                            .decoded
                            .types()
                            .resolve(module, &replacement.r#type)?,
                    )?
                {
                    return Err(RuntimeError::new(
                        "export_shape_changed",
                        module,
                        "export changed shape",
                    ));
                }
            }
            for function in artifact
                .functions
                .iter()
                .filter(|function| !function.revision_local)
            {
                let replacement = target
                    .function(module, &function.id)
                    .ok()
                    .filter(|function| !function.declaration.revision_local)
                    .ok_or_else(|| {
                        RuntimeError::new(
                            "call_shape_changed",
                            module,
                            "logical function was removed",
                        )
                    })?;
                if !types.signature_identical_from(
                    old.decoded.types(),
                    module,
                    &function.signature,
                    target.decoded.types(),
                    module,
                    &replacement.declaration.signature,
                )? || function.upvalues.len() != replacement.declaration.upvalues.len()
                {
                    return Err(RuntimeError::new(
                        "call_shape_changed",
                        module,
                        "function signature changed",
                    ));
                }
                for (old, new) in function
                    .upvalues
                    .iter()
                    .zip(replacement.declaration.upvalues.iter())
                {
                    if old.id != new.id
                        || !types.identical(
                            &self
                                .revision
                                .program
                                .decoded
                                .types()
                                .resolve(module, &old.r#type)?,
                            &target.decoded.types().resolve(module, &new.r#type)?,
                        )?
                    {
                        return Err(RuntimeError::new(
                            "call_shape_changed",
                            module,
                            "captured values changed",
                        ));
                    }
                }
            }
        }
        let mut globals = Vec::new();
        for (module, artifact) in target.decoded.artifacts() {
            if old.decoded.artifacts().contains_key(module) {
                continue;
            }
            for global in artifact.globals.iter() {
                let typ = types.resolve(module, &global.r#type)?;
                let value = Value {
                    typ,
                    data: Data::Uninitialized,
                };
                globals.push(((module.clone(), global.id.clone()), value));
            }
        }
        let changed_modules = target
            .image()
            .packages
            .as_ref()
            .unwrap()
            .iter()
            .filter(|(module, package)| {
                old.image()
                    .packages
                    .as_ref()
                    .unwrap()
                    .get(*module)
                    .is_none_or(|old| old.artifact_hash != package.artifact_hash)
            })
            .map(|(module, _)| module.clone())
            .collect();
        Ok(PatchPlan {
            owner: self.heap.owner_id(),
            base: self.revision(),
            target,
            types,
            globals,
            changed_modules,
        })
    }
    pub fn apply_patch(&mut self, plan: PatchPlan) -> Result<PatchResult, RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new("closed", "patch", "instance is closed"));
        }
        if self.faulted {
            return Err(RuntimeError::new(
                "instance_faulted",
                "patch",
                "instance execution failed",
            ));
        }
        if plan.owner != self.heap.owner_id() {
            return Err(RuntimeError::new(
                "wrong_instance",
                "patch",
                "plan belongs to another instance",
            ));
        }
        if plan.base != self.revision() {
            return Err(RuntimeError::new(
                "stale_plan",
                "patch",
                "base revision is no longer current",
            ));
        }
        if !self.initializing.is_empty() {
            return Err(RuntimeError::new(
                "initialization_active",
                "patch",
                "module initialization is active",
            ));
        }
        self.check_revision_capacity()?;
        let generation =
            self.revision.generation.checked_add(1).ok_or_else(|| {
                RuntimeError::new("revision_limit", "patch", "generation exhausted")
            })?;
        let mut globals = self.globals.clone();
        let mut keys = Vec::with_capacity(plan.globals.len());
        let mut values = Vec::with_capacity(plan.globals.len());
        for (key, value) in plan.globals {
            let bytes = value.logical_bytes()?.checked_add(128).ok_or_else(|| {
                RuntimeError::new("allocation_limit", "patch", "global size overflow")
            })?;
            keys.push(key);
            values.push((value, bytes));
        }
        let allocations = self.heap.prepare_allocations(values)?;
        for (key, handle) in keys.into_iter().zip(allocations.handles()) {
            globals.insert(key, *handle);
        }
        let next = Arc::new(crate::program::Revision {
            generation,
            program: plan.target,
        });
        self.retired_revisions
            .insert(self.revision.generation, Arc::downgrade(&self.revision));
        allocations.commit();
        self.revision = next;
        self.frame_pool.clear();
        self.types = plan.types;
        self.interface_assignments.get_mut().unwrap().clear();
        self.globals = globals;
        let modules = self.revision.program.decoded.artifacts();
        self.initialized
            .retain(|module| modules.contains_key(module));
        self.failed_initializations
            .retain(|module, _| modules.contains_key(module));
        self.debug_revision_changed();
        self.prune_revision_state();
        Ok(PatchResult {
            previous: plan.base,
            current: self.revision(),
            changed_modules: plan.changed_modules,
        })
    }
}
