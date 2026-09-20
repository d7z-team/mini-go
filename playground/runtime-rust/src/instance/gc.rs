//! Arena roots, collection boundaries, pressure thresholds and allocation recovery.

use super::*;

impl Instance {
    pub(super) fn allocate_private(
        &mut self,
        value: Value,
    ) -> Result<crate::heap::OwnedValue<Value>, RuntimeError> {
        let bytes = value.logical_bytes()?.checked_add(128).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "local", "logical size overflow")
        })?;
        value.trace(&mut |handle| self.running.transient_roots.push(handle));
        match self.heap.own(value, bytes) {
            Ok(value) => Ok(value),
            Err((_, value)) => {
                self.frame_pool.clear();
                self.collect_rooted()?;
                self.heap.own(value, bytes).map_err(|(error, _)| error)
            }
        }
    }

    pub(super) fn collect_at_boundary(&mut self) -> Result<usize, RuntimeError> {
        self.running.transient_roots.clear();
        self.collect_rooted()
    }

    /// Reclaims unreachable arena objects at an explicit owner boundary.
    /// This does not change the Go guest allocation ledger.
    pub fn collect_garbage(&mut self) -> Result<HeapStats, RuntimeError> {
        self.frame_pool.clear();
        self.collect_at_boundary()?;
        Ok(self.heap.stats())
    }

    pub(super) fn prune_revision_state(&mut self) {
        self.retired_revisions
            .retain(|_, revision| revision.strong_count() != 0);
        let current = self.revision.generation;
        let retired = &self.retired_revisions;
        let is_live_revision =
            |generation| generation == current || retired.contains_key(&generation);
        self.memory.retain_revisions(is_live_revision);
        self.constant_values
            .retain(|(generation, _), _| is_live_revision(*generation));
        self.call_bindings
            .get_mut()
            .unwrap()
            .retain(|(generation, _, _), _| is_live_revision(*generation));
    }

    pub(super) fn collect_rooted(&mut self) -> Result<usize, RuntimeError> {
        self.prune_revision_state();
        let mut roots = std::mem::take(&mut self.collection_roots);
        roots.clear();
        roots.extend(self.globals.values().copied());
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
        let outcome = self.heap.collect(roots.iter().copied());
        roots.clear();
        self.collection_roots = roots;
        let released = outcome?;
        let stats = self.heap.stats();
        self.collect_after_bytes = stats.total_allocated_bytes.saturating_add(
            (stats.live_bytes / 2)
                .max(256 << 10)
                .min(self.limits.max_heap_bytes.saturating_sub(stats.live_bytes))
                .max(1),
        );
        self.collect_after_objects = stats.live_objects.saturating_add(
            (stats.live_objects / 2)
                .max(256)
                .min(self.limits.max_objects.saturating_sub(stats.live_objects))
                .max(1),
        );
        self.prune_revision_state();
        Ok(released)
    }

    pub(super) fn allocate(&mut self, value: Value) -> Result<Handle, RuntimeError> {
        // Slot and node costs are independent of the host allocator layout.
        let bytes = value.logical_bytes()?.checked_add(128).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "value", "logical size overflow")
        })?;
        value.trace(&mut |handle| self.running.transient_roots.push(handle));
        let stats = self.heap.stats();
        if stats.total_allocated_bytes >= self.collect_after_bytes
            || stats.live_objects >= self.collect_after_objects
        {
            self.collect_rooted()?;
        }
        let handle = match self.heap.allocate(value, bytes) {
            Ok(handle) => handle,
            Err((error, value)) if error.code == "allocation_limit" => {
                self.frame_pool.clear();
                self.collect_rooted()?;
                self.heap
                    .allocate(value, bytes)
                    .map_err(|(error, _)| error)?
            }
            Err((error, _)) => return Err(error),
        };
        self.running.transient_roots.push(handle);
        Ok(handle)
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
        if !fits
            || stats.total_allocated_bytes >= self.collect_after_bytes
            || stats.live_objects >= self.collect_after_objects
        {
            if !collection.immediate {
                if !collection.heap_collected {
                    collection.request =
                        Some(mutation::CollectionRequest::Heap { clear_cache: !fits });
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
        }
        Ok(true)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn task_handoff_retains_transient_roots_until_the_owner_releases_them() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
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
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let frame_root = vm.allocate(Value::int(41)).unwrap();
        let allocation_root = vm.allocate(Value::int(42)).unwrap();
        let pointer = |root| Value {
            typ: TypeIdentity::Any,
            data: Data::Pointer(Address {
                identity: Arc::default(),
                root,
                path: Vec::new(),
            }),
        };
        let mut suspended = vm.running.frames.pop().unwrap();
        suspended.stack.push(pointer(frame_root));
        vm.running.suspended_frames.push(suspended);
        vm.running.allocation_roots.push(pointer(allocation_root));
        vm.running.transient_roots.clear();
        vm.park(scheduler::Blocked::Module("pending".into()));
        vm.collect_at_boundary().unwrap();
        assert_eq!(vm.heap.get(frame_root).unwrap().integer().unwrap(), 41);
        assert_eq!(vm.heap.get(allocation_root).unwrap().integer().unwrap(), 42);
        let task = vm.blocked.iter_mut().next().unwrap();
        task.suspended_frames.clear();
        task.allocation_roots.clear();
        vm.collect_at_boundary().unwrap();
        assert!(vm.heap.get(frame_root).is_err());
        assert!(vm.heap.get(allocation_root).is_err());
        vm.close().unwrap();
    }

    #[test]
    fn cancellation_reclaims_tasks_during_preparation_and_resume_handoff() {
        for preparing in [true, false] {
            let program = test_helpers::program_with_artifact(|artifact| {
                artifact["functions"] = serde_json::json!([
                    {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
                ]);
            });
            let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
            vm.start("default", Vec::new()).unwrap();
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
            assert!(vm.heap.get(root).is_err());
            assert!(!vm.scope_active(scope));
            assert!(vm.preparing_task.is_none());
            assert!(vm.resuming_task.is_none());
            vm.close().unwrap();
        }
    }

    fn program_with_constant(number: i64) -> Arc<Program> {
        super::super::test_helpers::program_with_artifact(|artifact| {
            artifact["constants"][0]["value"] = number.into();
        })
    }

    #[test]
    fn idle_patches_release_revision_indexes_without_collection() {
        let programs = [program_with_constant(10), program_with_constant(20)];
        let mut vm = Instance::new(programs[0].clone(), ExecutionLimits::default()).unwrap();
        let allocated = vm.heap_stats().total_allocated_bytes;
        for index in 0..10_000 {
            let old = vm.revision.generation;
            vm.constant_values.insert((old, 0), Value::int(42));
            vm.call_bindings
                .get_mut()
                .unwrap()
                .insert((old, 0, 0), (old, 0));
            vm.memory
                .recycle_frame_storage(old, 0, 0, memory::GuestFrameAccounting::default());
            let plan = vm.prepare_patch(programs[(index + 1) % 2].clone()).unwrap();
            vm.apply_patch(plan).unwrap();
            assert!(vm.retired_revisions.is_empty());
            assert!(vm.constant_values.is_empty());
            assert!(vm.call_bindings.get_mut().unwrap().is_empty());
            assert!(vm.memory.take_frame(old, 0, 0).is_none());
        }
        assert_eq!(vm.retained_revisions().len(), 1);
        assert_eq!(vm.heap_stats().total_allocated_bytes, allocated);
        vm.close().unwrap();
    }

    #[test]
    fn collection_releases_revision_caches_after_last_closure() {
        let mut vm = Instance::new(program_with_constant(10), ExecutionLimits::default()).unwrap();
        let old = vm.revision.generation;
        let closure = vm
            .allocate(Value {
                typ: TypeIdentity::Any,
                data: Data::Function(FunctionValue {
                    index: Some(0),
                    revision: Some(vm.revision.clone()),
                    module: "examples/arithmetic".into(),
                    function: "fn.Main".into(),
                    captures: vec![],
                }),
            })
            .unwrap();
        vm.globals
            .insert(("examples/arithmetic".into(), "retained".into()), closure);
        let plan = vm.prepare_patch(program_with_constant(20)).unwrap();
        vm.apply_patch(plan).unwrap();
        let current = vm.revision.generation;
        for generation in [old, current] {
            vm.constant_values.insert((generation, 0), Value::int(42));
            vm.call_bindings
                .get_mut()
                .unwrap()
                .insert((generation, 0, 0), (current, 0));
            vm.memory.recycle_frame_storage(
                generation,
                0,
                0,
                memory::GuestFrameAccounting::default(),
            );
        }
        vm.collect_garbage().unwrap();
        assert!(vm.retired_revisions.contains_key(&old));
        assert!(vm.constant_values.contains_key(&(old, 0)));
        assert!(vm.memory.take_frame(old, 0, 0).is_some());
        vm.memory
            .recycle_frame_storage(old, 0, 0, memory::GuestFrameAccounting::default());
        vm.globals
            .remove(&("examples/arithmetic".into(), "retained".into()));
        vm.collect_garbage().unwrap();
        assert!(vm.retired_revisions.is_empty());
        assert!(!vm.constant_values.contains_key(&(old, 0)));
        assert!(
            !vm.call_bindings
                .get_mut()
                .unwrap()
                .contains_key(&(old, 0, 0))
        );
        assert!(vm.memory.take_frame(old, 0, 0).is_none());
        assert!(vm.constant_values.contains_key(&(current, 0)));
        assert!(
            vm.call_bindings
                .get_mut()
                .unwrap()
                .contains_key(&(current, 0, 0))
        );
        assert!(vm.memory.take_frame(current, 0, 0).is_some());
        vm.close().unwrap();
    }
}
