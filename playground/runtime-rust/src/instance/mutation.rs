//! Resumable writes retain evaluated operands until a versioned commit succeeds.

use super::*;

/// A write returns its operands to the task before requesting a collection.
/// Synchronous host/control operations collect immediately; bytecode resumes
/// only after the scheduler has serviced the request at a rooted boundary.
#[derive(Clone, Copy, PartialEq, Eq)]
pub(super) enum CollectionRequest {
    Heap { clear_cache: bool },
    Census,
}

#[derive(Clone, Copy, Default)]
pub(super) struct WriteCollection {
    pub request: Option<CollectionRequest>,
    pub heap_collected: bool,
    pub census_published: bool,
    pub immediate: bool,
}

impl WriteCollection {
    pub const IMMEDIATE: Self = Self {
        request: None,
        heap_collected: false,
        census_published: false,
        immediate: true,
    };
}

pub(super) enum PendingWrite {
    Map(MapWrite),
    Delete { root: Handle, key: Value },
    Address { address: Address, value: Value },
}

impl Trace for PendingWrite {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        match self {
            Self::Map(write) => write.trace(visit),
            Self::Delete { root, key } => {
                visit(*root);
                key.trace(visit);
            }
            Self::Address { address, value } => {
                visit(address.root);
                value.trace(visit);
            }
        }
    }
}

pub(super) struct MapWrite {
    pub root: Handle,
    pub key: Value,
    pub value: Value,
    charged_entry: bool,
}

impl Trace for MapWrite {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        visit(self.root);
        self.key.trace(visit);
        self.value.trace(visit);
    }
}

struct PreparedMapWrite {
    expected: std::sync::Weak<Value>,
    bytes: u64,
    position: Option<usize>,
    same_edges: bool,
}

impl Instance {
    pub(super) fn prepare_delete(
        &self,
        object: Value,
        key: Value,
    ) -> Result<Option<PendingWrite>, RuntimeError> {
        let key = self.map_key(&object.typ, key)?;
        match object.data {
            Data::Map(root) => Ok(Some(PendingWrite::Delete { root, key })),
            Data::Nil => Ok(None),
            _ => Err(RuntimeError::new("type_error", "delete", "expected map")),
        }
    }

    fn attempt_delete(&self, root: Handle, key: &Value) -> Result<bool, RuntimeError> {
        let (snapshot, base_bytes) = self.heap.read_mutation_base(root)?;
        let Data::MapEntries(entries) = &snapshot.data else {
            return Err(RuntimeError::new(
                "invalid_map",
                "delete",
                "invalid backing",
            ));
        };
        let Some(index) = entries.find(key, &self.types)? else {
            return Ok(true);
        };
        let (key, value) = &entries[index];
        let bytes = base_bytes - key.logical_bytes()? - value.logical_bytes()?;
        let mut edges_unchanged = true;
        key.trace(&mut |_| edges_unchanged = false);
        value.trace(&mut |_| edges_unchanged = false);
        let expected = Arc::downgrade(&snapshot);
        drop(snapshot);
        self.heap
            .update(root, &expected, bytes, edges_unchanged, |backing| {
                let Data::MapEntries(entries) = &mut backing.data else {
                    unreachable!()
                };
                entries.remove(index);
            })
    }

    pub(super) fn prepare_index_write(
        &mut self,
        object: Value,
        key: Value,
        value: Value,
    ) -> Result<PendingWrite, RuntimeError> {
        if matches!(object.data, Data::Map(_)) {
            return self
                .prepare_map_write(&object, key, value)
                .map(PendingWrite::Map);
        }
        if matches!(object.data, Data::Nil) {
            return Err(RuntimeError::new(
                "panic",
                "store_index",
                "assignment to nil container",
            ));
        }
        let index = usize::try_from(key.integer()?)
            .map_err(|_| RuntimeError::new("panic", "store_index", "negative index"))?;
        let address = match object.data {
            Data::Pointer(mut address) => {
                address.path.push(PathElement::Index(index));
                address
            }
            Data::Slice(slice) => {
                if index >= slice.length {
                    return Err(RuntimeError::new(
                        "panic",
                        "store_index",
                        "index outside slice",
                    ));
                }
                let mut address = slice.storage;
                address.path.push(PathElement::Index(slice.start + index));
                address
            }
            _ => {
                return Err(RuntimeError::new(
                    "invalid_address",
                    "store_index",
                    "expected addressable sequence",
                ));
            }
        };
        Ok(PendingWrite::Address { address, value })
    }

    pub(super) fn prepare_map_write(
        &mut self,
        object: &Value,
        key: Value,
        value: Value,
    ) -> Result<MapWrite, RuntimeError> {
        let Data::Map(root) = object.data else {
            return Err(RuntimeError::new("invalid_map", "map", "invalid backing"));
        };
        Ok(MapWrite {
            root,
            key: self.map_key(&object.typ, key)?,
            value: self.coerce(value, &self.element_type(&object.typ)?)?,
            charged_entry: false,
        })
    }

    fn prepare_map_commit(
        &mut self,
        write: &mut MapWrite,
        collection: &mut WriteCollection,
    ) -> Result<Option<PreparedMapWrite>, RuntimeError> {
        let (snapshot, allocation) = self.heap.read_mutation_base(write.root)?;
        let Data::MapEntries(entries) = &snapshot.data else {
            return Err(RuntimeError::new("invalid_map", "map", "invalid backing"));
        };
        let position = entries.find(&write.key, &self.types)?;
        let mut old_edges = Vec::new();
        let mut new_edges = Vec::new();
        write.value.trace(&mut |handle| new_edges.push(handle));
        let previous_bytes = if let Some(index) = position {
            entries[index].1.trace(&mut |handle| old_edges.push(handle));
            entries[index].1.logical_bytes()?
        } else {
            if entries.len() >= self.limits.max_sequence_elements {
                return Err(RuntimeError::new(
                    "value_limit",
                    "map",
                    "entry limit exceeded",
                ));
            }
            write.key.trace(&mut |handle| new_edges.push(handle));
            0
        };
        let added = write
            .value
            .logical_bytes()?
            .checked_add(if position.is_none() {
                write.key.logical_bytes()?
            } else {
                0
            });
        let bytes = added
            .and_then(|added| allocation.checked_sub(previous_bytes)?.checked_add(added))
            .ok_or_else(|| RuntimeError::new("allocation_limit", "map", "logical size overflow"))?;
        if !self.prepare_write_replacements(&[(write.root, bytes)], collection)? {
            return Ok(None);
        }
        if position.is_none() && !write.charged_entry {
            if collection.immediate {
                self.charge_guest(32)?;
            } else if !self.memory.try_charge(32, self.limits.max_allocated_bytes) {
                if collection.census_published {
                    return Err(RuntimeError::new(
                        "allocation_limit",
                        "guest",
                        "guest allocation byte limit exceeded",
                    ));
                }
                collection.request = Some(CollectionRequest::Census);
                return Ok(None);
            }
            write.charged_entry = true;
        }
        Ok(Some(PreparedMapWrite {
            expected: Arc::downgrade(&snapshot),
            bytes,
            position,
            same_edges: old_edges == new_edges,
        }))
    }

    fn commit_map_write(
        &self,
        write: &MapWrite,
        prepared: PreparedMapWrite,
    ) -> Result<bool, RuntimeError> {
        self.heap.update(
            write.root,
            &prepared.expected,
            prepared.bytes,
            prepared.same_edges,
            |backing| {
                let Data::MapEntries(entries) = &mut backing.data else {
                    unreachable!()
                };
                if let Some(index) = prepared.position {
                    entries.set_value(index, write.value.clone());
                } else {
                    entries.insert(write.key.clone(), write.value.clone());
                }
            },
        )
    }

    pub(super) fn attempt_map_write(
        &mut self,
        write: &mut MapWrite,
        collection: &mut WriteCollection,
    ) -> Result<bool, RuntimeError> {
        // The request may be temporarily taken out of Task while executing. Keep
        // both inline values and arena edges visible to allocation-pressure GC.
        let values = self.running.allocation_roots.len();
        let handles = self.running.transient_roots.len();
        self.running
            .allocation_roots
            .extend([write.key.clone(), write.value.clone()]);
        write.trace(&mut |handle| self.running.transient_roots.push(handle));
        let outcome =
            self.prepare_map_commit(write, collection)
                .and_then(|prepared| match prepared {
                    Some(prepared) => self.commit_map_write(write, prepared),
                    None => Ok(false),
                });
        self.running.allocation_roots.truncate(values);
        self.running.transient_roots.truncate(handles);
        outcome
    }

    pub(super) fn attempt_write(
        &mut self,
        write: &mut PendingWrite,
        collection: &mut WriteCollection,
    ) -> Result<bool, RuntimeError> {
        Ok(match write {
            PendingWrite::Map(write) => self.attempt_map_write(write, collection)?,
            PendingWrite::Delete { root, key } => self.attempt_delete(*root, key)?,
            PendingWrite::Address { address, value } => {
                let values = self.running.allocation_roots.len();
                let handles = self.running.transient_roots.len();
                self.running.allocation_roots.push(value.clone());
                self.running.transient_roots.push(address.root);
                value.trace(&mut |handle| self.running.transient_roots.push(handle));
                let outcome = self.attempt_storage_write(
                    address.root,
                    &address.path,
                    value.clone(),
                    collection,
                );
                self.running.allocation_roots.truncate(values);
                self.running.transient_roots.truncate(handles);
                return outcome;
            }
        })
    }

    pub(super) fn resume_write(&mut self) -> Result<(), RuntimeError> {
        let mut write = self.running.pending_write.take().unwrap();
        let mut collection = self.running.write_collection;
        if !self.attempt_write(&mut write, &mut collection)? {
            self.running.pending_write = Some(write);
            self.running.write_collection = if collection.request.is_none() {
                WriteCollection::default()
            } else {
                collection
            };
            self.yield_task();
        } else {
            self.running.write_collection = WriteCollection::default();
        }
        Ok(())
    }

    pub(super) fn start_address_write(
        &mut self,
        address: Address,
        value: Value,
    ) -> Result<(), RuntimeError> {
        self.running.pending_write = Some(PendingWrite::Address { address, value });
        self.resume_write()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bytecode_write_yields_for_collection_and_resumes_at_the_following_instruction() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["type_table"] = serde_json::json!({"nodes":[{
                "id":"map", "kind":7,
                "key":{"kind":3,"primitive":3}, "elem":{"kind":3,"primitive":3}
            }]});
            artifact["constants"] = serde_json::json!([
                {"id":"key","type":{"kind":3,"primitive":3},"value":1},
                {"id":"answer","type":{"kind":3,"primitive":3},"value":42}
            ]);
            artifact["functions"] = serde_json::json!([{
                "id":"fn.Main", "signature":{"results":[{"kind":3,"primitive":3}]},
                "locals":[{"id":"map","type":{"kind":7,"node":"map"}}],
                "instructions":[
                    {"op":"make_map","payload":{"type":{"kind":7,"node":"map"}}},
                    {"op":"store_local","payload":{"local":"map"}},
                    {"op":"load_local","payload":{"local":"map"}},
                    {"op":"const","payload":{"constant":"key"}},
                    {"op":"const","payload":{"constant":"answer"}},
                    {"op":"store_index"},
                    {"op":"load_local","payload":{"local":"map"}},
                    {"op":"const","payload":{"constant":"key"}},
                    {"op":"load_index"},
                    {"op":"return","payload":{"result_count":1}}
                ]
            }]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        assert_eq!(vm.poll_steps(5).unwrap(), PollStatus::Running);
        vm.collect_after_bytes = 0;
        let collections = vm.heap_stats().collections;
        let charged = vm.memory.stats().total_allocated_bytes;
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Running);
        assert_eq!(vm.last_poll_steps, 1);
        assert_eq!(vm.heap_stats().collections, collections);
        assert_eq!(vm.memory.stats().total_allocated_bytes, charged);
        assert_eq!(vm.poll_steps(4).unwrap(), PollStatus::Ready);
        assert_eq!(vm.last_poll_steps, 4);
        assert_eq!(vm.steps(), 10);
        assert_eq!(vm.results()[0].integer().unwrap(), 42);
        assert_eq!(vm.heap_stats().collections, collections + 1);
        assert_eq!(vm.memory.stats().total_allocated_bytes, charged + 32);
        vm.close().unwrap();
    }

    #[test]
    fn write_pressure_returns_rooted_operands_before_collection_and_charges_once() {
        for cancel in [false, true] {
            let program = test_helpers::program_with_artifact(|artifact| {
                artifact["functions"] = serde_json::json!([
                    {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
                ]);
            });
            let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
            vm.start("default", Vec::new()).unwrap();
            let root = vm
                .allocate(Value {
                    typ: TypeIdentity::Any,
                    data: Data::MapEntries(Default::default()),
                })
                .unwrap();
            let pointee = vm.allocate(Value::int(42)).unwrap();
            let scope = vm.running.scope;
            vm.running.pending_write = Some(PendingWrite::Map(MapWrite {
                root,
                key: Value::int(1),
                value: Value {
                    typ: TypeIdentity::Pointer(Arc::new(TypeIdentity::Primitive(
                        wire::PrimitiveInt,
                    ))),
                    data: Data::Pointer(Address {
                        identity: Arc::default(),
                        root: pointee,
                        path: Vec::new(),
                    }),
                },
                charged_entry: false,
            }));
            vm.collect_after_bytes = 0;
            let before = vm.heap_stats();
            let charged = vm.memory.stats().total_allocated_bytes;
            vm.resume_write().unwrap();
            assert_eq!(vm.heap_stats().collections, before.collections);
            assert_eq!(vm.memory.stats().total_allocated_bytes, charged);
            assert!(vm.running.frames.is_empty());
            assert!(
                vm.runnable.front().unwrap().write_collection.request
                    == Some(CollectionRequest::Heap { clear_cache: false })
            );
            vm.runnable.front_mut().unwrap().transient_roots.clear();
            if cancel {
                vm.cancel_scope(scope).unwrap();
                vm.collect_garbage().unwrap();
                assert!(vm.heap.get(root).is_err());
                assert!(vm.heap.get(pointee).is_err());
                assert_eq!(vm.memory.stats().total_allocated_bytes, charged);
            } else {
                assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
                assert_eq!(vm.last_poll_steps, 1);
                assert_eq!(vm.heap_stats().collections, before.collections + 1);
                assert_eq!(vm.memory.stats().total_allocated_bytes, charged + 32);
                let value = vm.heap.get(root).unwrap();
                let Data::MapEntries(entries) = &value.data else {
                    panic!("map backing")
                };
                assert_eq!(entries.len(), 1);
                assert!(
                    matches!(&entries[0].1.data, Data::Pointer(address) if address.root == pointee)
                );
                assert_eq!(vm.heap.get(pointee).unwrap().integer().unwrap(), 42);
                drop(value);
                vm.collect_garbage().unwrap();
                assert!(vm.heap.get(root).is_err());
                assert!(vm.heap.get(pointee).is_err());
            }
            vm.close().unwrap();
        }
    }

    #[test]
    fn write_pressure_that_collection_cannot_resolve_fails_without_replaying() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let root = vm
            .allocate(Value {
                typ: TypeIdentity::Any,
                data: Data::MapEntries(Default::default()),
            })
            .unwrap();
        vm.running.pending_write = Some(PendingWrite::Map(MapWrite {
            root,
            key: Value::int(1),
            value: Value::int(42),
            charged_entry: false,
        }));
        vm.limits.max_allocated_bytes = 1;
        let charged = vm.memory.stats().total_allocated_bytes;
        vm.resume_write().unwrap();
        assert!(
            vm.runnable.front().unwrap().write_collection.request
                == Some(CollectionRequest::Census)
        );
        assert_eq!(vm.poll_steps(1).unwrap_err().code, "allocation_limit");
        assert_eq!(vm.last_poll_steps, 0);
        assert_eq!(vm.memory.stats().total_allocated_bytes, charged);
        assert!(vm.tasks().all(|task| task.pending_write.is_none()));
        assert!(vm.heap.get(root).is_err());
        vm.close().unwrap();
    }

    #[test]
    fn conflicted_map_write_keeps_operands_across_gc_and_does_not_repeat_entry_charge() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let root = vm
            .allocate(Value {
                typ: TypeIdentity::Any,
                data: Data::MapEntries(Default::default()),
            })
            .unwrap();
        let pointee = vm.allocate(Value::int(42)).unwrap();
        let value = Value {
            typ: TypeIdentity::Pointer(Arc::new(TypeIdentity::Primitive(wire::PrimitiveInt))),
            data: Data::Pointer(Address {
                identity: Arc::default(),
                root: pointee,
                path: Vec::new(),
            }),
        };
        let mut write = MapWrite {
            root,
            key: Value::int(1),
            value,
            charged_entry: false,
        };
        let mut collection = WriteCollection::IMMEDIATE;
        let prepared = vm
            .prepare_map_commit(&mut write, &mut collection)
            .unwrap()
            .unwrap();
        let charged = vm.memory.stats().total_allocated_bytes;

        // A different task publishes after preparation, before this write can
        // commit. Neither its new entry nor this write's inputs may be lost.
        let mut peer = MapWrite {
            root,
            key: Value::int(2),
            value: Value::int(7),
            charged_entry: false,
        };
        assert!(vm.attempt_map_write(&mut peer, &mut collection).unwrap());
        assert!(!vm.commit_map_write(&write, prepared).unwrap());
        let after_peer = vm.memory.stats().total_allocated_bytes;
        assert_eq!(after_peer, charged + 32);
        vm.running.pending_write = Some(PendingWrite::Map(write));
        vm.running.transient_roots.clear();
        vm.yield_task();
        vm.collect_at_boundary().unwrap();
        assert_eq!(vm.heap.get(pointee).unwrap().integer().unwrap(), 42);
        assert!(vm.schedule_next());
        vm.resume_write().unwrap();
        assert!(vm.running.pending_write.is_none());
        assert_eq!(vm.memory.stats().total_allocated_bytes, after_peer);
        let stored = vm.heap.get(root).unwrap();
        let Data::MapEntries(entries) = &stored.data else {
            panic!("map backing")
        };
        assert_eq!(entries.len(), 2);
        assert_eq!(
            entries[entries.find(&Value::int(2), &vm.types).unwrap().unwrap()]
                .1
                .integer()
                .unwrap(),
            7
        );
        drop(stored);

        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
        assert_eq!(vm.last_poll_steps, 1);
        vm.collect_at_boundary().unwrap();
        assert!(vm.heap.get(root).is_err());
        assert!(vm.heap.get(pointee).is_err());
    }

    #[test]
    fn cancelling_a_task_discards_its_uncommitted_map_write() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let root = vm
            .allocate(Value {
                typ: TypeIdentity::Any,
                data: Data::MapEntries(Default::default()),
            })
            .unwrap();
        let scope = vm.running.scope;
        vm.running.pending_write = Some(PendingWrite::Map(MapWrite {
            root,
            key: Value::int(1),
            value: Value::int(42),
            charged_entry: false,
        }));
        vm.yield_task();
        vm.cancel_scope(scope).unwrap();
        vm.collect_at_boundary().unwrap();
        assert!(vm.heap.get(root).is_err());
        assert!(vm.tasks().all(|task| task.pending_write.is_none()));
    }

    #[test]
    fn pending_address_write_keeps_its_value_and_resumes_without_a_bytecode_step() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let pointee = vm.allocate(Value::int(42)).unwrap();
        let typ = TypeIdentity::Pointer(Arc::new(TypeIdentity::Primitive(wire::PrimitiveInt)));
        let root = vm
            .allocate(Value {
                typ: typ.clone(),
                data: Data::Nil,
            })
            .unwrap();
        let value = Value {
            typ,
            data: Data::Pointer(Address {
                identity: Arc::default(),
                root: pointee,
                path: Vec::new(),
            }),
        };
        vm.running.pending_write = Some(PendingWrite::Address {
            address: Address {
                identity: Arc::default(),
                root,
                path: Vec::new(),
            },
            value,
        });
        vm.yield_task();
        vm.collect_at_boundary().unwrap();
        assert!(vm.heap.get(root).is_ok());
        assert!(vm.heap.get(pointee).is_ok());
        assert!(vm.schedule_next());
        let steps = vm.steps;
        vm.step().unwrap();
        assert_eq!(vm.steps, steps);
        let stored = vm.heap.get(root).unwrap();
        assert!(matches!(&stored.data, Data::Pointer(address) if address.root == pointee));
        assert!(vm.running.pending_write.is_none());
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
        assert_eq!(vm.last_poll_steps, 1);
    }

    #[test]
    fn pending_delete_keeps_its_map_alive_and_preserves_other_entries() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let mut entries = crate::value::MapStorage::default();
        entries.insert(Value::int(1), Value::int(42));
        entries.insert(Value::int(2), Value::int(7));
        let root = vm
            .allocate(Value {
                typ: TypeIdentity::Any,
                data: Data::MapEntries(entries),
            })
            .unwrap();
        vm.running.pending_write = Some(PendingWrite::Delete {
            root,
            key: Value::int(1),
        });
        vm.yield_task();
        vm.collect_at_boundary().unwrap();
        assert!(vm.heap.get(root).is_ok());
        assert!(vm.schedule_next());
        let charged = vm.memory.stats().total_allocated_bytes;
        vm.step().unwrap();
        assert!(vm.running.pending_write.is_none());
        assert_eq!(vm.memory.stats().total_allocated_bytes, charged);
        let stored = vm.heap.get(root).unwrap();
        let Data::MapEntries(entries) = &stored.data else {
            panic!("map backing")
        };
        assert!(entries.find(&Value::int(1), &vm.types).unwrap().is_none());
        let retained = entries.find(&Value::int(2), &vm.types).unwrap().unwrap();
        assert_eq!(entries[retained].1.integer().unwrap(), 7);
        drop(stored);
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
        assert_eq!(vm.last_poll_steps, 1);
        vm.collect_at_boundary().unwrap();
        assert!(vm.heap.get(root).is_err());
    }
}
