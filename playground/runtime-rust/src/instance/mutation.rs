//! Resumable writes retain evaluated operands until a versioned commit succeeds.

use super::*;

/// A write returns its operands to the task before requesting a collection.
/// Synchronous host/control operations collect immediately; bytecode resumes
/// only after the scheduler has serviced the request at a rooted boundary.
#[derive(Clone, Copy, PartialEq, Eq)]
pub(super) struct CollectionRequest {
    pub clear_cache: bool,
    pub major: bool,
}

#[derive(Clone, Copy, Default)]
pub(super) struct WriteCollection {
    pub request: Option<CollectionRequest>,
    pub heap_collected: bool,
    pub major_collected: bool,
    pub immediate: bool,
}

impl WriteCollection {
    pub const IMMEDIATE: Self = Self {
        request: None,
        heap_collected: false,
        major_collected: false,
        immediate: true,
    };
}

pub(super) enum PendingWrite {
    Map(MapWrite),
    MapLiteral {
        typ: TypeIdentity,
        map: Option<Value>,
        entries: VecDeque<(Value, Value)>,
    },
    Delete {
        root: Handle,
        key: Value,
    },
    Address {
        address: Address,
        value: Value,
    },
    Slice {
        typ: TypeIdentity,
        length: usize,
        capacity: usize,
    },
    Results(VecDeque<Value>),
}

impl Trace for PendingWrite {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        match self {
            Self::Map(write) => write.trace(visit),
            Self::MapLiteral { map, entries, .. } => {
                if let Some(map) = map {
                    map.trace(visit);
                }
                for (key, value) in entries {
                    key.trace(visit);
                    value.trace(visit);
                }
            }
            Self::Delete { root, key } => {
                visit(*root);
                key.trace(visit);
            }
            Self::Address { address, value } => {
                visit(address.root);
                value.trace(visit);
            }
            Self::Results(values) => {
                for value in values {
                    value.trace(visit);
                }
            }
            Self::Slice { .. } => {}
        }
    }
}

pub(super) struct MapWrite {
    pub root: Handle,
    pub key: Value,
    pub value: Value,
}

impl Trace for MapWrite {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        visit(self.root);
        self.key.trace(visit);
        self.value.trace(visit);
    }
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

    fn attempt_delete(&mut self, root: Handle, key: &Value) -> Result<bool, RuntimeError> {
        let hash = crate::managed_map::hash_value(key)?;
        let key_type = key.typ.clone();
        let map = self
            .managed_maps
            .get_mut(root)
            .ok_or_else(|| RuntimeError::new("invalid_map", "delete", "map owner is missing"))?;
        map.delete_lane(hash, |candidate| {
            self.heap
                .decode_map_lane(&key_type, &candidate.bytes, &candidate.references)
                .ok()
                .and_then(|other| crate::operators::equal(&other, key, &self.types).ok())
                .unwrap_or(false)
        })?;
        Ok(true)
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
            Data::Pointer(address) => {
                let mut address = Arc::unwrap_or_clone(address);
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
                let mut address = slice.storage.clone();
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
        })
    }

    pub(super) fn attempt_map_write(
        &mut self,
        write: &mut MapWrite,
        _collection: &mut WriteCollection,
    ) -> Result<bool, RuntimeError> {
        let allocation_scope = self.heap.begin_allocation_scope();
        let key_type = write.key.typ.clone();
        let value_type = write.value.typ.clone();
        let encoded_key = self.heap.encode_map_lane(&write.key, &key_type)?;
        let encoded_value = self.heap.encode_map_lane(&write.value, &value_type)?;
        // Map storage is shared and writes through StorageWorkerPool. Publish
        // any typed boxes/backing pages produced by encoding before the map
        // installs their reference lanes.
        self.heap.publish_payload(&encoded_key)?;
        self.heap.publish_payload(&encoded_value)?;
        let hash = crate::managed_map::hash_value(&write.key)?;
        let key = write.key.clone();
        let map = self
            .managed_maps
            .get_mut(write.root)
            .expect("managed map owner was checked above");
        map.set_lane(
            hash,
            crate::managed_map::MapValueBytes::from_prepared(encoded_key),
            crate::managed_map::MapValueBytes::from_prepared(encoded_value),
            |candidate| {
                self.heap
                    .decode_map_lane(&key_type, &candidate.bytes, &candidate.references)
                    .ok()
                    .and_then(|other| crate::operators::equal(&other, &key, &self.types).ok())
                    .unwrap_or(false)
            },
        )?;
        allocation_scope.commit();
        Ok(true)
    }

    pub(super) fn attempt_write(
        &mut self,
        write: &mut PendingWrite,
        collection: &mut WriteCollection,
    ) -> Result<bool, RuntimeError> {
        Ok(match write {
            PendingWrite::MapLiteral { typ, map, entries } => {
                if map.is_none() {
                    *map = Some(self.make_empty_map(typ.clone())?);
                }
                let map = map.as_ref().unwrap();
                while let Some((key, value)) = entries.front() {
                    let mut entry = self.prepare_map_write(map, key.clone(), value.clone())?;
                    if !self.attempt_map_write(&mut entry, collection)? {
                        return Ok(false);
                    }
                    entries.pop_front();
                    // A literal can span several nursery collections. Limit
                    // retries of one entry, not the whole evaluated literal.
                    collection.heap_collected = false;
                    collection.major_collected = false;
                }
                self.publish_result(map.clone())?;
                true
            }
            PendingWrite::Slice {
                typ,
                length,
                capacity,
            } => {
                let element = self.element_type(typ)?;
                let bytes = self
                    .types
                    .storage_logical_bytes(&element)?
                    .checked_mul(*capacity as u64)
                    .and_then(|bytes| bytes.checked_add(wire::STORAGE_RECORD_HEADER))
                    .ok_or_else(|| {
                        RuntimeError::new("allocation_limit", "slice", "backing size overflow")
                    })?;
                if self.heap.stats().guest_bytes.saturating_add(bytes) > self.limits.max_guest_bytes
                {
                    if collection.major_collected {
                        return Err(RuntimeError::new(
                            "allocation_limit",
                            "slice",
                            "backing exceeds guest allocation limit",
                        ));
                    }
                    collection.request = Some(CollectionRequest {
                        clear_cache: true,
                        major: true,
                    });
                    return Ok(false);
                }
                let value = self.make_slice(typ.clone(), *length, *capacity, Vec::new())?;
                if let Err(error) = self.publish_result(value.clone()) {
                    // Only an interrupted publication needs an owned queue.
                    // The backing has already been allocated and must survive
                    // the next collection without being constructed again.
                    *write = PendingWrite::Results(VecDeque::from([value]));
                    return Err(error);
                }
                true
            }
            PendingWrite::Results(values) => {
                while let Some(value) = values.front() {
                    self.publish_result(value.clone())?;
                    values.pop_front();
                    collection.heap_collected = false;
                    collection.major_collected = false;
                }
                true
            }
            PendingWrite::Map(write) => self.attempt_map_write(write, collection)?,
            PendingWrite::Delete { root, key } => self.attempt_delete(*root, key)?,
            PendingWrite::Address { address, value } => {
                let values = self.running.allocation_roots.len();
                let handles = self.running.transient_roots.len();
                self.running.allocation_roots.push(value.clone());
                self.running.transient_roots.push(address.root);
                value.trace(&mut |handle| self.running.transient_roots.push(handle));
                let outcome = (|| loop {
                    let attempt = (|| {
                        if let Some((offset, type_id, flags)) = self.numeric_location(address)? {
                            let target = self
                                .types
                                .identity_for_storage_type_id(type_id)
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "unknown_type",
                                        "pointer",
                                        "unknown numeric view type",
                                    )
                                })?;
                            let replacement = self.coerce(value.clone(), &target)?;
                            self.heap.write_prepared_location(
                                address.root,
                                offset,
                                type_id,
                                flags,
                                &replacement,
                            )
                        } else {
                            self.attempt_storage_write(
                                address.root,
                                &address.path,
                                value.clone(),
                                collection,
                            )
                        }
                    })();
                    match attempt {
                        Err(error)
                            if error.code == "collection_required"
                                && collection.immediate
                                && !collection.heap_collected =>
                        {
                            self.frame_pool.clear();
                            self.collect_rooted()?;
                            collection.heap_collected = true;
                            collection.major_collected = true;
                        }
                        outcome => return outcome,
                    }
                })();
                self.running.allocation_roots.truncate(values);
                self.running.transient_roots.truncate(handles);
                return outcome;
            }
        })
    }

    pub(super) fn resume_write(&mut self) -> Result<(), RuntimeError> {
        let mut write = self.running.pending_write.take().unwrap();
        let mut collection = self.running.write_collection;
        let committed = match self.attempt_write(&mut write, &mut collection) {
            Ok(committed) => committed,
            Err(error) if error.code == "collection_required" && !collection.major_collected => {
                collection.request = Some(CollectionRequest {
                    clear_cache: true,
                    major: collection.heap_collected || !self.allocation_failure_prefers_minor()?,
                });
                false
            }
            Err(error) => {
                self.running.pending_write = Some(write);
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
        };
        if !committed {
            self.running.pending_write = Some(write);
            self.running.write_collection = if collection.request.is_none() {
                WriteCollection::default()
            } else {
                collection
            };
            if collection.request.is_none() {
                self.yield_task();
            }
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
    use serde_json::json;
    use test_helpers::slot_code;

    #[test]
    fn map_literal_keeps_pending_entries_across_collection() {
        let count = 140usize;
        let program = test_helpers::program_with_artifact(|artifact| {
            let integer = json!({"kind":3,"primitive":3});
            let string = json!({"kind":3,"primitive":2});
            let map = json!({"kind":7,"node":"map"});
            artifact["type_table"] = json!({"nodes":[{
                "id":"map", "kind":7, "key":integer, "elem":string
            }]});
            let mut constants = Vec::with_capacity(count * 2);
            let mut inputs = Vec::with_capacity(count * 2);
            for index in 0..count {
                constants
                    .push(json!({"id":format!("key.{index}"),"type":integer,"value":index % 3}));
                constants.push(json!({"id":format!("value.{index}"),"type":string,"value":format!("entry {index} {}", "x".repeat(128))}));
                inputs.push(json!([1, index * 2]));
                inputs.push(json!([1, index * 2 + 1]));
            }
            artifact["constants"] = json!(constants);
            artifact["functions"] = json!([{
                "id":"fn.Main", "signature":{"results":[map]},
                "code":slot_code(json!([map]), &[
                    ("make_map",json!({"type":map,"entry_count":count}),json!({"inputs":inputs,"outputs":[0]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))
                ])
            }]);
        });
        // Replacing a few keys creates reclaimable backing while the literal
        // still owns the unevaluated publication queue. The small guest quota
        // forces collection independently of nursery page packing policy.
        let mut vm = Instance::new(
            program,
            ExecutionLimits {
                max_guest_bytes: 8192,
                ..ExecutionLimits::default()
            },
        )
        .unwrap();
        vm.start("default", &[]).unwrap();
        while vm.poll_steps(1).unwrap() == PollStatus::Running {}
        assert_eq!(vm.steps(), 2);
        assert!(vm.heap_stats().major_collections >= 2);
        let map = vm.guest_results()[0].clone();
        let Data::Map(root) = map.data else {
            panic!("map result")
        };
        let entries = vm.materialize_managed_map(root, &map.typ).unwrap();
        assert_eq!(entries.len(), 3);
        for (key, value) in entries {
            let key = key.integer().unwrap() as usize;
            let index = (0..count).rev().find(|index| index % 3 == key).unwrap();
            assert!(
                matches!(value.data, Data::String(bytes) if bytes.as_ref() == format!("entry {index} {}", "x".repeat(128)).as_bytes())
            );
        }
        vm.close().unwrap();
    }

    #[test]
    fn bytecode_managed_map_write_recovers_nursery_pressure_without_replaying_opcode() {
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
                "code":slot_code(json!([{"kind":7,"node":"map"},{"kind":3,"primitive":3},{"kind":3,"primitive":3}]), &[
                    ("make_map",json!({"type":{"kind":7,"node":"map"}}),json!({"outputs":[0]})),
                    ("store_local",json!({"local":"map"}),json!({"inputs":[[0,0]],"release":[0]})),
                    ("load_local",json!({"local":"map"}),json!({"outputs":[0]})),
                    ("const",json!({"constant":"key"}),json!({"outputs":[1]})),
                    ("const",json!({"constant":"answer"}),json!({"outputs":[2]})),
                    ("store_index",json!({}),json!({"inputs":[[0,0],[0,1],[0,2]],"release":[0,1,2]})),
                    ("load_local",json!({"local":"map"}),json!({"outputs":[0]})),
                    ("const",json!({"constant":"key"}),json!({"outputs":[1]})),
                    ("load_index",json!({}),json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,2]],"release":[2]}))])
            }]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        assert_eq!(vm.poll_steps(5).unwrap(), PollStatus::Running);
        // Fill the actual nursery with unreachable payloads, including its
        // remaining short spans. The map's first slab must request recovery
        // from the opcode's pending write, rather than from allocate(Value).
        vm.heap.flush_worker_lease().unwrap();
        {
            let pool = vm.heap.storage_pool();
            let mut storage = pool.lock().unwrap();
            let layout = storage
                .register_layout(crate::managed_storage::StorageLayout {
                    stride: 8,
                    ..Default::default()
                })
                .unwrap();
            for count in [1024, 1] {
                loop {
                    match storage.reserve(layout, count, u64::from(count) * 8) {
                        Ok(reservation) => {
                            reservation.commit();
                        }
                        Err(error) => {
                            assert_eq!(error.code, "collection_required");
                            break;
                        }
                    }
                }
            }
        }
        assert!(vm.heap.nursery_pressure().unwrap().refill_required);
        let before = vm.heap_stats();
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Running);
        assert_eq!(vm.last_poll_steps, 1);
        assert_eq!(
            vm.heap_stats().minor_collections,
            before.minor_collections + 1
        );
        assert_eq!(vm.heap_stats().major_collections, before.major_collections);
        assert_eq!(vm.poll_steps(4).unwrap(), PollStatus::Ready);
        assert_eq!(vm.last_poll_steps, 4);
        assert_eq!(vm.steps(), 10);
        assert_eq!(vm.guest_results()[0].integer().unwrap(), 42);
        assert_eq!(vm.heap_stats().major_collections, before.major_collections);
        vm.close().unwrap();
    }

    #[test]
    fn map_writes_preserve_existing_entries_and_pointer_values() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::slot_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let integer = TypeIdentity::Primitive(wire::PrimitiveInt);
        let pointer = TypeIdentity::Pointer(Arc::new(integer.clone()));
        let map_type = vm
            .types
            .construct(
                crate::types::ConstructedType::Map {
                    key: integer.clone(),
                    element: pointer.clone(),
                },
                100,
                32,
            )
            .unwrap();
        vm.heap.set_type_registry(vm.types.clone());
        let map = vm.make_empty_map(map_type.clone()).unwrap();
        let Data::Map(root) = map.data else {
            panic!("map root")
        };
        let pointee = vm.allocate(Value::int(42)).unwrap();
        let value = Value {
            typ: pointer,
            data: Data::Pointer(std::sync::Arc::new(Address {
                identity: Arc::default(),
                root: pointee,
                path: Vec::new(),
            })),
        };
        let mut write = MapWrite {
            root,
            key: Value::int(1),
            value,
        };
        let mut collection = WriteCollection::IMMEDIATE;
        let mut peer = MapWrite {
            root,
            key: Value::int(2),
            value: Value {
                typ: TypeIdentity::Pointer(Arc::new(integer)),
                data: Data::Pointer(std::sync::Arc::new(Address {
                    identity: Arc::default(),
                    root: pointee,
                    path: Vec::new(),
                })),
            },
        };
        assert!(vm.attempt_map_write(&mut peer, &mut collection).unwrap());
        assert!(vm.attempt_map_write(&mut write, &mut collection).unwrap());
        assert_eq!(vm.managed_maps.get(root).unwrap().len().unwrap(), 2);
        let entries = vm.materialize_managed_map(root, &map_type).unwrap();
        assert_eq!(entries.len(), 2);
        assert!(entries.iter().all(|(_, value)| {
            matches!(&value.data, Data::Pointer(address) if address.root == pointee)
        }));
        vm.close().unwrap();
    }

    #[test]
    fn managed_map_round_trips_dynamic_string_lanes() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::slot_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let string = TypeIdentity::Primitive(wire::PrimitiveString);
        let map_type = vm
            .types
            .construct(
                crate::types::ConstructedType::Map {
                    key: string.clone(),
                    element: string,
                },
                100,
                32,
            )
            .unwrap();
        vm.heap.set_type_registry(vm.types.clone());
        let map = vm.make_empty_map(map_type.clone()).unwrap();
        let Data::Map(root) = map.data else {
            panic!("map root")
        };
        let mut write = MapWrite {
            root,
            key: Value::string("key"),
            value: Value::string("value"),
        };
        let mut collection = WriteCollection::IMMEDIATE;
        assert!(vm.attempt_map_write(&mut write, &mut collection).unwrap());
        let entries = vm.materialize_managed_map(root, &map_type).unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].0.to_string(), "key");
        assert_eq!(entries[0].1.to_string(), "value");
        vm.close().unwrap();
    }

    #[test]
    fn managed_map_growth_failure_rolls_back_encoded_string_backing() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::slot_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let limits = ExecutionLimits {
            max_guest_bytes: 1 << 20,
            ..ExecutionLimits::default()
        };
        let mut vm = Instance::new(program, limits).unwrap();
        vm.start("default", &[]).unwrap();
        let map_type = vm
            .types
            .construct(
                crate::types::ConstructedType::Map {
                    key: TypeIdentity::Primitive(wire::PrimitiveInt),
                    element: TypeIdentity::Primitive(wire::PrimitiveString),
                },
                100,
                32,
            )
            .unwrap();
        vm.heap.set_type_registry(vm.types.clone());
        let map = vm.make_empty_map(map_type.clone()).unwrap();
        let Data::Map(root) = map.data else {
            panic!("expected managed map");
        };
        vm.heap.flush_worker_lease().unwrap();
        // Admit the string backing but leave insufficient room for the first
        // entry slab. A failed growth must also discard that encoded string.
        let held = vm
            .heap
            .reserve_control_capacity(limits.max_guest_bytes - vm.heap.stats().guest_bytes - 64)
            .unwrap();
        let before = vm.heap.stats();
        let mut write = MapWrite {
            root,
            key: Value::int(7),
            value: Value::string("pending"),
        };
        let mut collection = WriteCollection::IMMEDIATE;
        let error = vm
            .attempt_map_write(&mut write, &mut collection)
            .unwrap_err();
        assert_eq!(error.code, "collection_required");
        assert!(
            vm.materialize_managed_map(root, &map_type)
                .unwrap()
                .is_empty()
        );
        assert_eq!(vm.heap.stats().live_objects, before.live_objects);
        assert_eq!(vm.heap.stats().guest_bytes, before.guest_bytes);
        assert_eq!(vm.heap.stats().reserved_bytes, before.reserved_bytes);
        drop(held);
        assert!(vm.attempt_map_write(&mut write, &mut collection).unwrap());
        let entries = vm.materialize_managed_map(root, &map_type).unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].0.integer().unwrap(), 7);
        assert_eq!(entries[0].1.to_string(), "pending");
        vm.close().unwrap();
    }

    #[test]
    fn managed_map_round_trips_any_lane_without_host_map_storage() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::slot_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let integer = TypeIdentity::Primitive(wire::PrimitiveInt);
        let map_type = vm
            .types
            .construct(
                crate::types::ConstructedType::Map {
                    key: integer,
                    element: TypeIdentity::Any,
                },
                100,
                32,
            )
            .unwrap();
        vm.heap.set_type_registry(vm.types.clone());
        let map = vm.make_empty_map(map_type.clone()).unwrap();
        let Data::Map(root) = map.clone().data else {
            panic!("map root")
        };
        let mut write = vm
            .prepare_map_write(&map, Value::int(1), Value::int(42))
            .unwrap();
        let mut collection = WriteCollection::IMMEDIATE;
        assert!(vm.attempt_map_write(&mut write, &mut collection).unwrap());
        let entries = vm.materialize_managed_map(root, &map_type).unwrap();
        let Data::Interface(value) = &entries[0].1.data else {
            panic!("any map value must preserve its dynamic value")
        };
        assert_eq!(value.integer().unwrap(), 42);
        vm.close().unwrap();
    }

    #[test]
    fn managed_map_keeps_dynamic_slice_lane_reachable() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::slot_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let integer = TypeIdentity::Primitive(wire::PrimitiveInt);
        let slice = TypeIdentity::Slice(Arc::new(integer.clone()));
        let map_type = vm
            .types
            .construct(
                crate::types::ConstructedType::Map {
                    key: integer,
                    element: slice.clone(),
                },
                100,
                32,
            )
            .unwrap();
        vm.heap.set_type_registry(vm.types.clone());
        let map = vm.make_empty_map(map_type.clone()).unwrap();
        let Data::Map(root) = map.data else {
            panic!("map root")
        };
        let value = vm
            .make_slice(slice, 2, 2, vec![Value::int(3), Value::int(4)])
            .unwrap();
        let mut write = MapWrite {
            root,
            key: Value::int(1),
            value,
        };
        let mut collection = WriteCollection::IMMEDIATE;
        assert!(vm.attempt_map_write(&mut write, &mut collection).unwrap());
        let entries = vm.materialize_managed_map(root, &map_type).unwrap();
        assert_eq!(entries.len(), 1);
        let Data::Slice(slice) = &entries[0].1.data else {
            panic!("dynamic map value must decode as a slice")
        };
        assert_eq!(slice.length, 2);
        vm.close().unwrap();
    }

    #[test]
    fn managed_map_preserves_empty_and_mixed_reference_values_across_collection() {
        use crate::types::{ConstructedField, ConstructedType};

        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::slot_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let empty_type = vm
            .types
            .construct(ConstructedType::Struct(vec![]), 100, 32)
            .unwrap();
        let mixed_type = vm
            .types
            .construct(
                ConstructedType::Struct(vec![
                    ConstructedField {
                        name: "Name".into(),
                        typ: TypeIdentity::Primitive(wire::PrimitiveString),
                        tag: String::new(),
                        embedded: false,
                    },
                    ConstructedField {
                        name: "Item".into(),
                        typ: TypeIdentity::Any,
                        tag: String::new(),
                        embedded: false,
                    },
                ]),
                100,
                32,
            )
            .unwrap();
        let cases = [
            Value::boolean(true),
            Value {
                typ: empty_type,
                data: Data::Struct(crate::value::StructStorage::with_layout(
                    Arc::new(vec![]),
                    vec![],
                )),
            },
            Value {
                typ: mixed_type,
                data: Data::Struct(crate::value::StructStorage::with_layout(
                    Arc::new(vec!["Name".into(), "Item".into()]),
                    vec![
                        Value::string("kept by a fixed reference lane"),
                        Value {
                            typ: TypeIdentity::Any,
                            data: Data::Interface(Arc::new(Value::string("dynamic lane"))),
                        },
                    ],
                )),
            },
        ];
        let mut expected = Vec::new();
        for value in cases {
            let map_type = vm
                .types
                .construct(
                    ConstructedType::Map {
                        key: TypeIdentity::Primitive(wire::PrimitiveInt),
                        element: value.typ.clone(),
                    },
                    100,
                    32,
                )
                .unwrap();
            vm.heap.set_type_registry(vm.types.clone());
            let map = vm.make_empty_map(map_type.clone()).unwrap();
            let Data::Map(root) = map.data else {
                panic!("expected managed map")
            };
            let mut write = vm
                .prepare_map_write(&map, Value::int(1), value.clone())
                .unwrap();
            let mut collection = WriteCollection::IMMEDIATE;
            vm.attempt_map_write(&mut write, &mut collection).unwrap();
            vm.running.transient_roots.push(root);
            expected.push((root, map_type, value));
        }
        vm.collect_minor_rooted().unwrap();
        vm.collect_rooted().unwrap();
        for (root, map_type, value) in expected {
            let entries = vm.materialize_managed_map(root, &map_type).unwrap();
            assert_eq!(entries.len(), 1);
            assert!(crate::operators::equal(&entries[0].1, &value, &vm.types).unwrap());
        }
        vm.close().unwrap();
    }

    #[test]
    fn managed_map_round_trips_nested_map_lane() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::slot_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let integer = TypeIdentity::Primitive(wire::PrimitiveInt);
        let nested_type = vm
            .types
            .construct(
                crate::types::ConstructedType::Map {
                    key: integer.clone(),
                    element: integer.clone(),
                },
                100,
                32,
            )
            .unwrap();
        let outer_type = vm
            .types
            .construct(
                crate::types::ConstructedType::Map {
                    key: integer.clone(),
                    element: nested_type.clone(),
                },
                100,
                32,
            )
            .unwrap();
        vm.heap.set_type_registry(vm.types.clone());
        let nested = vm.make_empty_map(nested_type.clone()).unwrap();
        let Data::Map(nested_root) = nested.clone().data else {
            panic!("nested map root")
        };
        let mut nested_write = MapWrite {
            root: nested_root,
            key: Value::int(7),
            value: Value::int(42),
        };
        let mut collection = WriteCollection::IMMEDIATE;
        assert!(
            vm.attempt_map_write(&mut nested_write, &mut collection)
                .unwrap()
        );
        let outer = vm.make_empty_map(outer_type.clone()).unwrap();
        let Data::Map(outer_root) = outer.data else {
            panic!("outer map root")
        };
        let mut outer_write = MapWrite {
            root: outer_root,
            key: Value::int(1),
            value: nested,
        };
        assert!(
            vm.attempt_map_write(&mut outer_write, &mut collection)
                .unwrap()
        );
        let entries = vm.materialize_managed_map(outer_root, &outer_type).unwrap();
        let Data::Map(decoded_root) = entries[0].1.data else {
            panic!("nested map value must decode as a map")
        };
        let nested_entries = vm
            .materialize_managed_map(decoded_root, &nested_type)
            .unwrap();
        assert_eq!(nested_entries[0].1.integer().unwrap(), 42);
        vm.close().unwrap();
    }

    #[test]
    fn pending_address_write_keeps_its_value_and_resumes_without_a_bytecode_step() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::slot_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
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
            data: Data::Pointer(std::sync::Arc::new(Address {
                identity: Arc::default(),
                root: pointee,
                path: Vec::new(),
            })),
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
                {"id":"fn.Main", "code":test_helpers::slot_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let integer = TypeIdentity::Primitive(wire::PrimitiveInt);
        let map_type = vm
            .types
            .construct(
                crate::types::ConstructedType::Map {
                    key: integer.clone(),
                    element: integer.clone(),
                },
                100,
                32,
            )
            .unwrap();
        vm.heap.set_type_registry(vm.types.clone());
        let map = vm.make_empty_map(map_type.clone()).unwrap();
        let Data::Map(root) = map.data else {
            panic!("map root")
        };
        vm.results.push(map.clone());
        vm.globals.insert(("test".into(), "map".into()), root);
        let mut collection = WriteCollection::IMMEDIATE;
        for (key, value) in [(1, 42), (2, 7)] {
            let mut write = MapWrite {
                root,
                key: Value::int(key),
                value: Value::int(value),
            };
            assert!(vm.attempt_map_write(&mut write, &mut collection).unwrap());
        }
        vm.running.pending_write = Some(PendingWrite::Delete {
            root,
            key: Value::int(1),
        });
        vm.yield_task();
        vm.collect_at_boundary().unwrap();
        assert!(vm.heap.is_live(root));
        assert!(vm.schedule_next());
        vm.step().unwrap();
        assert!(vm.running.pending_write.is_none());
        let entries = vm.materialize_managed_map(root, &map_type).unwrap();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].0.integer().unwrap(), 2);
        assert_eq!(entries[0].1.integer().unwrap(), 7);
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
        assert_eq!(vm.last_poll_steps, 1);
        vm.collect_at_boundary().unwrap();
        assert!(vm.managed_maps.get(root).is_some());
        vm.close().unwrap();
    }

    #[test]
    fn managed_map_owner_is_pruned_when_guest_anchor_is_unreachable() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::slot_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let integer = TypeIdentity::Primitive(wire::PrimitiveInt);
        let map_type = vm
            .types
            .construct(
                crate::types::ConstructedType::Map {
                    key: integer.clone(),
                    element: integer,
                },
                100,
                32,
            )
            .unwrap();
        vm.heap.set_type_registry(vm.types.clone());
        let map = vm.make_empty_map(map_type).unwrap();
        let Data::Map(root) = map.data else {
            panic!("map root")
        };
        assert!(vm.managed_maps.get(root).is_some());
        vm.collect_at_boundary().unwrap();
        assert!(!vm.heap.is_live(root));
        assert!(vm.managed_maps.get(root).is_none());
        vm.close().unwrap();
    }
}
