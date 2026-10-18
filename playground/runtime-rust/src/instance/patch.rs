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
        let retained = self.retired_revisions.values().count();
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
            .cloned()
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
        let types = self.types.overlay(target.decoded.types())?;
        let mut globals = Vec::new();
        for (module, artifact) in target.decoded.artifacts() {
            for global in artifact.globals.iter() {
                let typ = types.resolve(module, &global.r#type)?;
                let retained = old
                    .decoded
                    .artifacts()
                    .get(module)
                    .and_then(|artifact| {
                        artifact.globals.iter().find(|value| value.id == global.id)
                    })
                    .is_some_and(|previous| {
                        old.decoded
                            .types()
                            .resolve(module, &previous.r#type)
                            .ok()
                            .and_then(|old_type| types.identical(&old_type, &typ).ok())
                            .unwrap_or(false)
                    });
                if retained {
                    continue;
                }
                let value = Value {
                    typ,
                    data: Data::Uninitialized,
                };
                globals.push(((module.clone(), global.id.clone()), value));
            }
        }
        let target_packages = target.image().packages.as_ref().unwrap();
        let old_packages = old.image().packages.as_ref().unwrap();
        let mut changed_modules = target_packages
            .iter()
            .filter(|(module, package)| {
                old_packages
                    .get(*module)
                    .is_none_or(|old| old.artifact_hash != package.artifact_hash)
            })
            .map(|(module, _)| module.clone())
            .collect::<Vec<_>>();
        changed_modules.extend(
            old_packages
                .keys()
                .filter(|module| !target_packages.contains_key(*module))
                .cloned(),
        );
        changed_modules.sort_unstable();
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
        // The staged type registry must be active while encoding new globals;
        // otherwise a structural type introduced by the target is decoded
        // through the retired registry.  Restore the old registry on every
        // preparation failure so a rejected patch is observationally atomic.
        let previous_types = self.types.clone();
        self.heap.set_type_registry(plan.types.clone());
        let mut globals = self.globals.clone();
        globals.retain(|(module, id), _| {
            plan.target
                .decoded
                .artifacts()
                .get(module)
                .is_some_and(|artifact| artifact.globals.iter().any(|global| global.id == *id))
        });
        let mut keys = Vec::with_capacity(plan.globals.len());
        let mut values = Vec::with_capacity(plan.globals.len());
        for (key, value) in plan.globals {
            let bytes = match self.prepared_allocation_bytes(&value) {
                Ok(bytes) => bytes,
                Err(error) => {
                    self.heap.set_type_registry(previous_types.clone());
                    return Err(RuntimeError::new(error.code, "patch", error.message));
                }
            };
            keys.push(key);
            values.push((value, bytes));
        }
        let mut allocations = match self.heap.prepare_allocations(values) {
            Ok(allocations) => allocations,
            Err(error) => {
                self.heap.set_type_registry(previous_types.clone());
                return Err(error);
            }
        };
        for (key, handle) in keys.into_iter().zip(allocations.handles()) {
            globals.insert(key, *handle);
        }
        let anchor = match allocations.prepare_revision_anchor(globals.values().copied()) {
            Ok(anchor) => anchor,
            Err(error) => {
                drop(allocations);
                self.heap.set_type_registry(previous_types.clone());
                return Err(error);
            }
        };
        let next = Arc::new(crate::program::Revision {
            generation,
            program: plan.target,
        });
        self.retired_revisions
            .insert(self.revision.generation, self.revision.clone());
        allocations.commit();
        self.revision = next;
        self.frame_pool.clear();
        self.types = plan.types;
        self.heap.set_type_registry(self.types.clone());
        self.interface_assignments.get_mut().unwrap().clear();
        self.globals = globals;
        self.revision_anchors.insert(generation, anchor);
        let modules = self.revision.program.decoded.artifacts();
        self.initialized
            .retain(|module| modules.contains_key(module));
        self.failed_initializations
            .retain(|module, _| modules.contains_key(module));
        self.debug_revision_changed();
        Ok(PatchResult {
            previous: plan.base,
            current: self.revision(),
            changed_modules: plan.changed_modules,
        })
    }
}
