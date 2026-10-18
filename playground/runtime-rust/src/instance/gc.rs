//! Arena roots, collection boundaries, pressure thresholds and allocation recovery.

use super::*;

impl Instance {
    pub(super) fn prepared_allocation_bytes(&self, value: &Value) -> Result<u64, RuntimeError> {
        self.heap.logical_allocation_bytes(value)
    }

    fn acquire_dynamic_type_owners(&mut self, value: &Value) -> Result<Vec<String>, RuntimeError> {
        let keys = self.dynamic_type_owner_keys(value)?;
        let mut acquired: Vec<String> = Vec::with_capacity(keys.len());
        for key in &keys {
            let owner_count = self
                .dynamic_type_owner_counts
                .get(key)
                .copied()
                .unwrap_or_default();
            if owner_count == 0 {
                let bytes = self.dynamic_type_costs[key];
                let lease = match self.acquire_owner_lease(bytes) {
                    Ok(lease) => lease,
                    Err(error) => {
                        for acquired_key in acquired.drain(..) {
                            self.release_dynamic_type_owner(&acquired_key);
                        }
                        return Err(error);
                    }
                };
                self.dynamic_type_leases.insert(key.clone(), lease);
            }
            self.dynamic_type_owner_counts
                .insert(key.clone(), owner_count + 1);
            acquired.push(key.clone());
        }
        Ok(acquired)
    }

    fn release_dynamic_type_owner(&mut self, key: &str) {
        let Some(owners) = self.dynamic_type_owner_counts.get_mut(key) else {
            return;
        };
        *owners = owners.saturating_sub(1);
        if *owners == 0 {
            self.dynamic_type_owner_counts.remove(key);
            self.dynamic_type_leases.remove(key);
        }
    }

    fn prune_dynamic_type_owners(&mut self, prune_registry: bool) -> Result<(), RuntimeError> {
        let stale = self
            .dynamic_type_owners
            .iter()
            .filter(|(handle, _)| !self.heap.is_live(**handle))
            .map(|(handle, keys)| (*handle, keys.clone()))
            .collect::<Vec<_>>();
        for (handle, keys) in stale {
            self.dynamic_type_owners.remove(&handle);
            for key in keys {
                self.release_dynamic_type_owner(&key);
            }
        }
        if prune_registry {
            self.prune_dynamic_type_registry()?;
        }
        Ok(())
    }

    fn prune_dynamic_type_registry(&mut self) -> Result<(), RuntimeError> {
        let mut live_keys = self
            .dynamic_type_owner_counts
            .iter()
            .filter_map(|(key, count)| (*count != 0).then_some(key.clone()))
            .collect::<HashSet<_>>();
        // Prepared frame banks expose their dynamic storage IDs directly
        // below. Do not rebuild local/slot Values here: this is a registry
        // maintenance boundary, and decoding every aggregate would recreate
        // the execution graph solely to rediscover metadata roots.
        let heap_roots = self.heap.live_dynamic_type_roots()?;
        let payload_types = self.heap.live_payload_types()?;
        let mut frame_type_ids = HashSet::new();
        for task in self.tasks() {
            for frame in task.frames.iter().chain(&task.suspended_frames) {
                let mut ids = Vec::new();
                frame.dynamic_type_ids(&mut ids);
                frame_type_ids.extend(ids);
            }
        }
        let mut pooled_frame_type_ids = Vec::new();
        self.frame_pool.dynamic_type_ids(&mut pooled_frame_type_ids);
        frame_type_ids.extend(pooled_frame_type_ids);
        let registry = self.heap.type_registry_snapshot();
        let frame_roots = if let Some(registry) = registry {
            frame_type_ids
                .into_iter()
                .map(|id| {
                    registry.identity_for_storage_type_id(id).ok_or_else(|| {
                        RuntimeError::new(
                            "unknown_type",
                            "heap",
                            "frame dynamic type ID is missing",
                        )
                    })
                })
                .collect::<Result<Vec<_>, _>>()?
        } else {
            Vec::new()
        };
        let managed_map_roots: Vec<_> = self.managed_maps.dynamic_type_roots().collect();
        // Convert prepared/heap type identities back to the existing reflected
        // keys without decoding any frame payload. Pointer and slice views use
        // the same type spine as TypeRegistry's dynamic closure.
        let mut retain_type_keys = |typ: &TypeIdentity| {
            let mut pending = vec![typ];
            while let Some(typ) = pending.pop() {
                if let Some((key, _)) = self
                    .dynamic_type_identities
                    .iter()
                    .find(|(_, identity)| *identity == typ)
                {
                    live_keys.insert(key.clone());
                }
                match typ {
                    TypeIdentity::Pointer(element) | TypeIdentity::Slice(element) => {
                        pending.push(element);
                    }
                    _ => {}
                }
            }
        };
        for typ in heap_roots
            .iter()
            .chain(payload_types.iter())
            .chain(frame_roots.iter())
            .chain(managed_map_roots.iter())
        {
            retain_type_keys(typ);
        }
        let stale_keys = self
            .dynamic_type_costs
            .keys()
            .filter(|key| !live_keys.contains(*key))
            .cloned()
            .collect::<HashSet<_>>();
        let mut roots = live_keys
            .iter()
            .filter_map(|key| self.dynamic_type_identities.get(key).cloned())
            .collect::<Vec<_>>();
        // A value under construction can be the only owner of a runtime
        // array/function type before its first heap record is published.
        // These transaction roots are released immediately after admission.
        for task in self.tasks() {
            let mut pending = task.allocation_roots.iter().collect::<Vec<_>>();
            while let Some(value) = pending.pop() {
                roots.push(value.typ.clone());
                match &value.data {
                    Data::Aggregate(aggregate) => aggregate.type_roots(&mut roots),
                    Data::Interface(inner) | Data::DynamicFunction(inner) => pending.push(inner),
                    Data::Array(values) => pending.extend(values.iter()),
                    Data::Struct(fields) => pending.extend(fields.values()),
                    Data::Method {
                        receiver: Some(receiver),
                        ..
                    } => pending.push(receiver),
                    _ => {}
                }
            }
        }
        // Managed map headers use an Any storage record, so the structural
        // map identity is carried by the owner table rather than the heap
        // payload metadata. Keep that identity's dynamic node closure alive
        // for as long as the guest map handle remains reachable.
        roots.extend(managed_map_roots);
        // Heap-produced Any aggregate descriptors carry their structural type
        // ID directly in the live payload and do not have a runtimeType key.
        // Include those IDs in the data closure before pruning the catalog.
        roots.extend(heap_roots);
        roots.extend(payload_types);
        roots.extend(frame_roots);
        self.types.prune_dynamic(&roots);
        self.heap.set_type_registry(self.types.clone());
        self.dynamic_type_costs
            .retain(|key, _| live_keys.contains(key));
        self.dynamic_type_identities
            .retain(|key, _| live_keys.contains(key));
        self.reflected_types
            .retain(|(_, key)| !stale_keys.contains(key));
        self.reflected_type_indices.clear();
        self.reflected_type_keys.clear();
        for (index, (typ, key)) in self.reflected_types.iter().enumerate() {
            self.reflected_type_indices.insert(typ.clone(), index);
            self.reflected_type_keys.insert(key.clone(), index);
        }
        Ok(())
    }

    pub(super) fn allocate_private(&mut self, value: Value) -> Result<Handle, RuntimeError> {
        let bytes = self.prepared_allocation_bytes(&value)?;
        self.allocate_private_storage(value, bytes)
    }

    // Frame preparation may reserve the final parameter size while its value
    // remains rooted in the caller until the entire frame is ready to commit.
    pub(super) fn allocate_private_storage(
        &mut self,
        value: Value,
        bytes: u64,
    ) -> Result<Handle, RuntimeError> {
        let value = self.attach_revision_anchor(value);
        let root_start = self.running.allocation_roots.len();
        self.running.allocation_roots.push(value.clone());
        let outcome = self.allocate_rooted_storage(value, bytes);
        self.running.allocation_roots.truncate(root_start);
        outcome
    }

    fn allocate_rooted_storage(
        &mut self,
        value: Value,
        bytes: u64,
    ) -> Result<Handle, RuntimeError> {
        let channel_owner = match &value.data {
            Data::Resource(resource) => match &**resource {
                scheduler::Resource::Channel { channel } => Some(channel.clone()),
                _ => None,
            },
            _ => None,
        };
        value.trace(&mut |handle| self.running.transient_roots.push(handle));
        let stats = self.heap.stats();
        if stats.old_bytes >= self.collect_major_after_bytes {
            self.collect_rooted()?;
        } else if stats.young_bytes >= self.collect_after_bytes {
            self.collect_minor_rooted()?;
        }
        let dynamic_owner = self.acquire_dynamic_type_owners(&value)?;
        let mut candidate = value;
        let mut minor_attempted = false;
        let mut major_attempted = false;
        loop {
            match self.heap.allocate(candidate, bytes) {
                Ok(handle) => {
                    self.sync_heap_type_registry();
                    if !dynamic_owner.is_empty() {
                        self.dynamic_type_owners.insert(handle, dynamic_owner);
                    }
                    if let Some(channel) = &channel_owner {
                        self.channel_records
                            .lock()
                            .unwrap()
                            .insert(handle, channel.clone());
                    }
                    return Ok(handle);
                }
                Err((error, returned))
                    if matches!(error.code, "collection_required" | "allocation_limit") =>
                {
                    self.frame_pool.clear();
                    let collection = if !minor_attempted
                        && !major_attempted
                        && self.allocation_failure_prefers_minor()?
                    {
                        minor_attempted = true;
                        self.collect_minor_rooted()
                    } else if !major_attempted {
                        major_attempted = true;
                        self.collect_rooted()
                    } else {
                        Err(error)
                    };
                    if let Err(error) = collection {
                        for key in &dynamic_owner {
                            self.release_dynamic_type_owner(key);
                        }
                        return Err(if error.code == "collection_required" {
                            RuntimeError::new(
                                "allocation_limit",
                                "guest",
                                "guest allocation byte limit exceeded",
                            )
                        } else {
                            error
                        });
                    }
                    candidate = returned;
                    candidate.trace(&mut |handle| self.running.transient_roots.push(handle));
                }
                Err((error, _)) => {
                    for key in &dynamic_owner {
                        self.release_dynamic_type_owner(key);
                    }
                    return Err(error);
                }
            }
        }
    }

    pub(super) fn allocation_failure_prefers_minor(&self) -> Result<bool, RuntimeError> {
        let stats = self.heap.stats();
        if stats.young_bytes >= self.collect_after_bytes {
            return Ok(true);
        }
        Ok(self.heap.nursery_pressure()?.refill_required)
    }

    pub(super) fn collect_pending_write(
        &mut self,
        request: mutation::CollectionRequest,
    ) -> Result<(), RuntimeError> {
        if request.clear_cache {
            self.frame_pool.clear();
        }
        if request.major {
            self.collect_rooted()?;
        } else {
            self.collect_minor_rooted()?;
        }
        self.running.write_collection.heap_collected = true;
        self.running.write_collection.major_collected |= request.major;
        Ok(())
    }

    pub(super) fn collect_at_boundary(&mut self) -> Result<usize, RuntimeError> {
        self.running.transient_roots.clear();
        self.collect_rooted()
    }

    /// Reclaims unreachable arena objects at an explicit owner boundary and
    /// returns statistics from the managed storage ledger.
    pub fn collect_garbage(&mut self) -> Result<HeapStats, RuntimeError> {
        if self.cleanup_result.is_some() {
            return Ok(self.heap.stats());
        }
        self.frame_pool.clear();
        self.collect_at_boundary()?;
        Ok(self.heap.stats())
    }

    fn prune_revision_state(&mut self) {
        let mut live_revisions = self
            .tasks()
            .flat_map(|task| task.frames.iter().chain(&task.suspended_frames))
            .map(|frame| frame.revision.generation)
            .collect::<HashSet<_>>();
        live_revisions.insert(self.revision.generation);
        // Function and method records carry a managed edge to their revision
        // anchor. The major collector has already followed those edges, so an
        // anchor that is still live is the complete owner signal for its
        // retired revision. Never decode the complete graph to rediscover
        // this relationship: that makes patch retention proportional to the
        // entire reachable object graph.
        live_revisions.extend(
            self.revision_anchors
                .iter()
                .filter_map(|(generation, anchor)| {
                    self.heap.is_live(*anchor).then_some(*generation)
                }),
        );
        self.retired_revisions
            .retain(|generation, _| live_revisions.contains(generation));
        let current = self.revision.generation;
        self.revision_anchors.retain(|generation, anchor| {
            (*generation == current || self.retired_revisions.contains_key(generation))
                && self.heap.is_live(*anchor)
        });
        let retired = &self.retired_revisions;
        let is_live_revision =
            |generation| generation == current || retired.contains_key(&generation);
        self.constant_values
            .retain(|(generation, _), _| is_live_revision(*generation));
    }

    pub(super) fn collect_rooted(&mut self) -> Result<usize, RuntimeError> {
        self.sync_heap_type_registry();
        // Collection is an owner boundary. Flush the current worker lease so
        // the collector never observes unpublished reservations as a live
        // graph and rejects an otherwise valid safepoint.
        self.heap.flush_worker_lease()?;
        let mut roots = self.collection_root_handles();
        let outcome = self.heap.collect(roots.iter().copied());
        roots.clear();
        self.collection_roots = roots;
        let released = outcome?;
        self.prune_managed_maps();
        self.prune_channel_records();
        self.prune_dynamic_type_owners(true)?;
        let stats = self.heap.stats();
        let nursery_limit = self.heap.nursery_pressure()?.limit.max(1);
        self.collect_after_bytes = stats
            .gc_pressure_bytes
            .saturating_add(
                (stats.live_bytes / 2)
                    .max(256 << 10)
                    .min(self.limits.max_guest_bytes.saturating_sub(stats.live_bytes))
                    .max(1),
            )
            .min(nursery_limit);
        self.collect_major_after_bytes = stats.old_bytes.saturating_add(
            (stats.live_bytes / 2)
                .max(1 << 20)
                .min(self.limits.max_guest_bytes.saturating_sub(stats.old_bytes))
                .max(1),
        );
        // A major collection is the owner boundary for revision retention.
        self.prune_revision_state();
        Ok(released)
    }

    fn prune_managed_maps(&mut self) {
        let stale = self
            .managed_maps
            .guests()
            .filter(|guest| !self.heap.is_live(*guest))
            .collect::<Vec<_>>();
        for guest in stale {
            self.managed_maps.remove(guest);
        }
    }

    pub(super) fn collection_root_handles(&mut self) -> Vec<Handle> {
        let mut roots = std::mem::take(&mut self.collection_roots);
        roots.clear();
        roots.extend(self.globals.values().copied());
        // The current code owner keeps its anchor even while idle. Retired
        // code is retained only by its active frames or managed functions.
        if let Some(anchor) = self.revision_anchors.get(&self.revision.generation) {
            roots.push(*anchor);
        }
        for task in self.tasks() {
            for frame in task.frames.iter().chain(&task.suspended_frames) {
                if let Some(anchor) = self.revision_anchors.get(&frame.revision.generation) {
                    roots.push(*anchor);
                }
            }
        }
        self.frame_pool.trace(&mut |handle| roots.push(handle));
        for value in self.constant_values.values() {
            value.trace(&mut |handle| roots.push(handle));
        }
        for task in self.tasks() {
            task.trace(&mut |handle| roots.push(handle));
        }
        for value in &self.results {
            value.trace(&mut |handle| roots.push(handle));
        }
        for timer in &self.timers {
            timer.channel.trace(&mut |handle| roots.push(handle));
        }
        roots
    }

    pub(super) fn collect_minor_rooted(&mut self) -> Result<usize, RuntimeError> {
        self.sync_heap_type_registry();
        self.heap.flush_worker_lease()?;
        let mut roots = self.collection_root_handles();
        let released = self.heap.collect_minor(roots.iter().copied())?;
        roots.clear();
        self.collection_roots = roots;
        self.prune_managed_maps();
        self.prune_channel_records();
        self.prune_dynamic_type_owners(false)?;
        Ok(released)
    }

    pub(super) fn allocate(&mut self, value: Value) -> Result<Handle, RuntimeError> {
        let bytes = self.prepared_allocation_bytes(&value)?;
        let handle = self.allocate_private_storage(value, bytes)?;
        self.running.transient_roots.push(handle);
        Ok(handle)
    }

    fn attach_revision_anchor(&self, value: Value) -> Value {
        let binding_for = |function: &FunctionValue| {
            let revision = function.revision.clone().or_else(|| {
                function.revision_generation.and_then(|generation| {
                    if generation == self.revision.generation {
                        Some(self.revision.clone())
                    } else {
                        self.retired_revisions.get(&generation).cloned()
                    }
                })
            });
            let generation = function
                .revision_generation
                .or_else(|| revision.as_ref().map(|revision| revision.generation));
            let anchor =
                generation.and_then(|generation| self.revision_anchors.get(&generation).copied());
            let index = function.index.or_else(|| {
                revision.as_ref().and_then(|revision| {
                    revision
                        .program
                        .function_index(&function.module, &function.function)
                })
            });
            (index, anchor)
        };
        match &value.data {
            Data::Function(function) => {
                let (index, anchor) = binding_for(function);
                if function.index == index && function.revision_anchor == anchor {
                    return value;
                }
                let mut function = Arc::unwrap_or_clone(function.clone());
                function.index = index;
                function.revision_anchor = anchor;
                Value {
                    typ: value.typ,
                    data: Data::Function(Arc::new(function)),
                }
            }
            Data::Method { function, receiver } => {
                let (index, anchor) = binding_for(function);
                if function.index == index && function.revision_anchor == anchor {
                    return value;
                }
                let mut function = Arc::unwrap_or_clone(function.clone());
                function.index = index;
                function.revision_anchor = anchor;
                Value {
                    typ: value.typ,
                    data: Data::Method {
                        function: Arc::new(function),
                        receiver: receiver.clone(),
                    },
                }
            }
            _ => value,
        }
    }

    pub(super) fn prepare_heap_replacements(
        &mut self,
        replacements: &[(Handle, u64)],
    ) -> Result<(), RuntimeError> {
        let mut collection = mutation::WriteCollection::IMMEDIATE;
        self.prepare_write_replacements(replacements, &mut collection)?;
        Ok(())
    }

    pub(super) fn prepare_write_replacements(
        &mut self,
        replacements: &[(Handle, u64)],
        collection: &mut mutation::WriteCollection,
    ) -> Result<bool, RuntimeError> {
        let fits = self.heap.replacements_fit(replacements)?;
        let stats = self.heap.stats();
        if !fits || stats.old_bytes >= self.collect_major_after_bytes {
            if !collection.immediate {
                if !collection.major_collected {
                    collection.request = Some(mutation::CollectionRequest {
                        clear_cache: !fits,
                        major: true,
                    });
                    return Ok(false);
                }
                return Ok(true);
            }
            if !fits {
                self.frame_pool.clear();
            }
            self.running
                .transient_roots
                .extend(replacements.iter().map(|(handle, _)| *handle));
            self.collect_rooted()?;
        } else if stats.young_bytes >= self.collect_after_bytes {
            if !collection.immediate {
                if !collection.heap_collected {
                    collection.request = Some(mutation::CollectionRequest {
                        clear_cache: false,
                        major: false,
                    });
                    return Ok(false);
                }
                return Ok(true);
            }
            self.collect_minor_rooted()?;
        }
        Ok(true)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn heap_stats_report_storage_pressure_and_object_liveness() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::micro_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let before = vm.heap_stats();
        let _handle = vm.allocate(Value::int(7)).unwrap();
        vm.heap.flush_worker_lease().unwrap();
        let after = vm.heap_stats();
        assert!(after.live_objects > before.live_objects);
        assert!(after.allocations > before.allocations);
        assert!(after.gc_pressure_bytes > 0);
        assert_eq!(
            after.collections,
            after.minor_collections + after.major_collections
        );
        vm.close().unwrap();
    }

    #[test]
    fn task_handoff_retains_transient_roots_until_the_owner_releases_them() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::micro_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let handle = vm.allocate(Value::int(42)).unwrap();
        vm.yield_task();
        vm.collect_at_boundary().unwrap();
        assert_eq!(vm.heap.get(handle).unwrap().integer().unwrap(), 42);
        assert!(vm.schedule_next());
        vm.park(scheduler::Blocked::Module("pending".into()));
        vm.collect_at_boundary().unwrap();
        assert_eq!(vm.heap.get(handle).unwrap().integer().unwrap(), 42);
        vm.blocked
            .iter_mut()
            .next()
            .unwrap()
            .transient_roots
            .clear();
        vm.collect_at_boundary().unwrap();
        assert!(vm.heap.get(handle).is_err());
        vm.close().unwrap();
    }

    #[test]
    fn parked_task_owns_suspended_frames_and_allocation_values() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "signature":{"results":[{"kind":2}]},
                 "code":test_helpers::micro_code(serde_json::json!([{"kind":2}]), &[
                    ("zero",serde_json::json!({"type":{"kind":2}}),serde_json::json!({"outputs":[0]})),
                    ("return",serde_json::json!({"result_count":1}),serde_json::json!({"inputs":[[0,0]],"release":[0]}))
                 ])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let frame_root = vm.allocate(Value::int(41)).unwrap();
        let allocation_root = vm.allocate(Value::int(42)).unwrap();
        let delivery_root = vm.allocate(Value::int(43)).unwrap();
        let pointer = |root| Value {
            typ: TypeIdentity::Any,
            data: Data::Pointer(std::sync::Arc::new(Address {
                identity: Arc::default(),
                root,
                path: Vec::new(),
            })),
        };
        vm.store_return_values(vec![pointer(frame_root)]).unwrap();
        let mut suspended = vm.running.frames.pop().unwrap();
        suspended.continuation = Some(frame::Continuation::TailReturn(1));
        suspended.push_result(&pointer(delivery_root)).unwrap();
        vm.running.suspended_frames.push(suspended);
        vm.running.allocation_roots.push(pointer(allocation_root));
        vm.running.transient_roots.clear();
        vm.park(scheduler::Blocked::Module("pending".into()));
        vm.collect_at_boundary().unwrap();
        assert_eq!(vm.heap.get(frame_root).unwrap().integer().unwrap(), 41);
        assert_eq!(vm.heap.get(allocation_root).unwrap().integer().unwrap(), 42);
        assert_eq!(vm.heap.get(delivery_root).unwrap().integer().unwrap(), 43);
        let task = vm.blocked.iter_mut().next().unwrap();
        task.suspended_frames.clear();
        task.allocation_roots.clear();
        vm.collect_at_boundary().unwrap();
        assert!(vm.heap.get(frame_root).is_err());
        assert!(vm.heap.get(allocation_root).is_err());
        assert!(vm.heap.get(delivery_root).is_err());
        vm.close().unwrap();
    }

    #[test]
    fn cancellation_reclaims_tasks_during_preparation_and_resume_handoff() {
        for preparing in [true, false] {
            let program = test_helpers::program_with_artifact(|artifact| {
                artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::micro_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
                ]);
            });
            let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
            vm.start("default", &[]).unwrap();
            let scope = vm.running.scope;
            let root = vm.allocate(Value::int(42)).unwrap();
            let mut task = std::mem::take(&mut vm.running);
            task.suspended_frames.push(task.frames.pop().unwrap());
            if preparing {
                vm.preparing_task = Some(task);
            } else {
                vm.resuming_task = Some(task);
            }
            vm.collect_at_boundary().unwrap();
            assert_eq!(vm.heap.get(root).unwrap().integer().unwrap(), 42);
            vm.cancel_scope(scope).unwrap();
            assert!(!vm.scope_active(scope));
            assert!(vm.preparing_task.is_none());
            assert!(vm.resuming_task.is_none());
            vm.collect_at_boundary().unwrap();
            assert!(vm.heap.get(root).is_err());
            vm.close().unwrap();
        }
    }
}
