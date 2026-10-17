//! Compact byte storage operations. Reads snapshot before writes so aliases
//! retain memmove/append semantics; mutations commit through the heap owner.

use super::*;

impl Instance {
    pub(super) fn check_string_size(&self, length: usize) -> Result<(), RuntimeError> {
        if length as u64 > self.limits.max_guest_bytes {
            return Err(RuntimeError::new(
                "allocation_limit",
                "string",
                "string exceeds heap budget",
            ));
        }
        Ok(())
    }

    pub(super) fn slice_bytes(&self, value: &Value) -> Result<Vec<u8>, RuntimeError> {
        if let Data::String(bytes) = &value.data {
            return Ok(bytes.to_vec());
        }
        if let Data::Slice(slice) = &value.data {
            if slice.storage.path.is_empty()
                && let Some(bytes) = self.heap.read_byte_storage_range(
                    slice.storage.root,
                    slice.start,
                    slice.length,
                )?
            {
                return Ok(bytes);
            }
            let backing = self.snapshot_address(&slice.storage)?;
            if let Data::Bytes(bytes) = &backing.data {
                return bytes
                    .get(slice.start..slice.start + slice.length)
                    .map(<[u8]>::to_vec)
                    .ok_or_else(|| {
                        RuntimeError::new("invalid_slice", "bytes", "invalid byte view")
                    });
            }
            let Some((true, count)) = backing.data.aggregate_shape() else {
                return Err(RuntimeError::new(
                    "invalid_slice",
                    "slice",
                    "invalid backing",
                ));
            };
            if slice
                .start
                .checked_add(slice.length)
                .is_none_or(|end| end > count)
            {
                return Err(RuntimeError::new("invalid_slice", "slice", "invalid view"));
            }
            let mut bytes = Vec::with_capacity(slice.length);
            for index in slice.start..slice.start + slice.length {
                match backing.element(index)?.data {
                    Data::Unsigned(byte) if byte <= 255 => bytes.push(byte as u8),
                    _ => {
                        return Err(RuntimeError::new(
                            "type_error",
                            "bytes",
                            "expected byte elements",
                        ));
                    }
                }
            }
            return Ok(bytes);
        }
        if matches!(value.data, Data::Nil) {
            return Ok(Vec::new());
        }
        Err(RuntimeError::new("type_error", "slice", "expected slice"))
    }

    pub(super) fn make_bytes(
        &mut self,
        typ: TypeIdentity,
        length: usize,
        capacity: usize,
        initial: &[u8],
    ) -> Result<Value, RuntimeError> {
        if initial.len() > length || length > capacity {
            return Err(RuntimeError::new(
                "allocation_limit",
                "bytes",
                "byte backing exceeds instance limits",
            ));
        }
        let capacity = u64::try_from(capacity).map_err(|_| {
            RuntimeError::new(
                "allocation_limit",
                "bytes",
                "byte backing length overflows guest accounting",
            )
        })?;
        let logical = crate::contract_generated::STORAGE_RECORD_HEADER
            .checked_add(capacity)
            .ok_or_else(|| {
                RuntimeError::new(
                    "allocation_limit",
                    "bytes",
                    "byte backing size overflows guest accounting",
                )
            })?;
        let physical = capacity
            .max(1)
            .checked_add(7)
            .map(|size| size & !7)
            .ok_or_else(|| {
                RuntimeError::new(
                    "allocation_limit",
                    "bytes",
                    "byte backing size overflows managed storage",
                )
            })?;
        if physical > u64::from(u32::MAX - 7) {
            return Err(RuntimeError::new(
                "allocation_limit",
                "bytes",
                "byte backing exceeds managed storage size",
            ));
        }
        if logical > self.limits.max_guest_bytes {
            return Err(RuntimeError::new(
                "allocation_limit",
                "bytes",
                "byte backing exceeds guest allocation limit",
            ));
        }
        if self.limits.max_resident_bytes != 0 && physical > self.limits.max_resident_bytes {
            return Err(RuntimeError::new(
                "allocation_limit",
                "bytes",
                "byte backing exceeds resident limit",
            ));
        }
        let capacity = usize::try_from(capacity).map_err(|_| {
            RuntimeError::new(
                "allocation_limit",
                "bytes",
                "byte backing length exceeds host size",
            )
        })?;
        let mut bytes = Vec::new();
        bytes.try_reserve_exact(capacity).map_err(|_| {
            RuntimeError::new(
                "allocation_limit",
                "bytes",
                "byte backing allocation failed",
            )
        })?;
        bytes.extend_from_slice(initial);
        bytes.resize(capacity, 0);
        // Raw byte backing is an internal storage object, so it does not pass
        // through `Instance::allocate(Value)`.  Keep the same one-shot owner
        // collection/retry contract here; a direct heap call must not turn a
        // reclaimable `collection_required` into an externally visible
        // allocation failure.
        let root = match self.heap.allocate_raw_bytes(&bytes) {
            Ok(root) => root,
            Err(error) if matches!(error.code, "collection_required" | "allocation_limit") => {
                self.collect_rooted().map_err(|error| {
                    if error.code == "collection_required" {
                        RuntimeError::new(
                            "allocation_limit",
                            "guest",
                            "guest allocation byte limit exceeded",
                        )
                    } else {
                        error
                    }
                })?;
                self.heap.allocate_raw_bytes(&bytes).map_err(|error| {
                    if error.code == "collection_required" {
                        RuntimeError::new(
                            "allocation_limit",
                            "guest",
                            "guest allocation byte limit exceeded",
                        )
                    } else {
                        error
                    }
                })?
            }
            Err(error) => return Err(error),
        };
        // Unlike `Instance::allocate`, raw byte storage bypasses the normal
        // value allocation helper. Keep the returned backing alive for the
        // current operation so a subsequent host argument allocation or
        // collection cannot reclaim an earlier byte argument before its
        // owner publishes it.
        self.running.transient_roots.push(root);
        Ok(Value {
            typ,
            data: Data::Slice(std::sync::Arc::new(SliceValue {
                identity: std::sync::Arc::default(),
                storage: Address {
                    identity: std::sync::Arc::default(),
                    root,
                    path: Vec::new(),
                },
                start: 0,
                length,
                capacity,
            })),
        })
    }

    pub(super) fn read_byte_range(
        &self,
        value: &Value,
        start: usize,
        length: usize,
    ) -> Result<Vec<u8>, RuntimeError> {
        if let Data::String(bytes) = &value.data {
            return bytes
                .get(start..start.saturating_add(length))
                .map(<[u8]>::to_vec)
                .ok_or_else(|| RuntimeError::new("panic", "bytes", "byte range outside string"));
        }
        if let Data::Slice(slice) = &value.data {
            if start > slice.length || length > slice.length - start {
                return Err(RuntimeError::new(
                    "panic",
                    "bytes",
                    "byte range outside slice",
                ));
            }
            if slice.storage.path.is_empty()
                && let Some(bytes) = self.heap.read_byte_storage_range(
                    slice.storage.root,
                    slice.start + start,
                    length,
                )?
            {
                return Ok(bytes);
            }
            let backing = self.snapshot_address(&slice.storage)?;
            match &backing.data {
                Data::Bytes(bytes) => {
                    return Ok(bytes[slice.start + start..slice.start + start + length].to_vec());
                }
                data if data.aggregate_shape().is_some_and(|(array, _)| array) => {
                    return (slice.start + start..slice.start + start + length)
                        .map(|index| match backing.element(index)?.data {
                            Data::Unsigned(byte) if byte <= 255 => Ok(byte as u8),
                            _ => Err(RuntimeError::new(
                                "type_error",
                                "bytes",
                                "expected byte elements",
                            )),
                        })
                        .collect();
                }
                _ => {}
            }
        } else if matches!(value.data, Data::Nil) && start == 0 && length == 0 {
            return Ok(Vec::new());
        }
        Err(RuntimeError::new(
            "type_error",
            "bytes",
            "expected byte slice",
        ))
    }

    pub(super) fn write_slice_bytes(
        &mut self,
        value: &Value,
        offset: usize,
        source: &[u8],
    ) -> Result<(), RuntimeError> {
        let Data::Slice(slice) = &value.data else {
            return if source.is_empty() && offset == 0 && matches!(value.data, Data::Nil) {
                Ok(())
            } else {
                Err(RuntimeError::new("panic", "bytes", "write outside slice"))
            };
        };
        if offset > slice.length || source.len() > slice.length - offset {
            return Err(RuntimeError::new("panic", "bytes", "write outside slice"));
        }
        if slice.storage.path.is_empty() {
            loop {
                let (snapshot, _bytes, version) =
                    self.heap.read_typed_mutation_base(slice.storage.root)?;
                if !matches!(snapshot.data, Data::Bytes(_)) {
                    break;
                }
                drop(snapshot);
                if self.heap.write_byte_storage_range_if_version(
                    slice.storage.root,
                    slice.start + offset,
                    source,
                    version,
                )? {
                    return Ok(());
                }
            }
        }
        for (index, byte) in source.iter().enumerate() {
            self.store_index(
                value.clone(),
                Value::int((offset + index) as i64),
                Value {
                    typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                    data: Data::Unsigned(u64::from(*byte)),
                },
            )?;
        }
        Ok(())
    }

    pub(super) fn append_bytes(
        &mut self,
        mut value: Value,
        source: &[u8],
    ) -> Result<Value, RuntimeError> {
        let (length, capacity) = match &value.data {
            Data::Slice(slice) => (slice.length, slice.capacity),
            Data::Nil => (0, 0),
            _ => {
                return Err(RuntimeError::new(
                    "type_error",
                    "append",
                    "expected byte slice",
                ));
            }
        };
        let (new_length, new_capacity) =
            self.reserve_slice_append(length, capacity, source.len())?;
        if new_length > capacity {
            let mut bytes = self.read_byte_range(&value, 0, length)?;
            bytes.extend_from_slice(source);
            return self.make_bytes(value.typ, new_length, new_capacity, &bytes);
        }
        if let Data::Slice(slice) = &mut value.data {
            let slice = Arc::make_mut(slice);
            slice.length = new_length;
            slice.identity = Arc::default();
            self.write_slice_bytes(&value, length, source)?;
        }
        Ok(value)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn storage_cost(name: &str) -> (u64, u64) {
        let vectors: Vec<serde_json::Value> = serde_json::from_str(include_str!(
            "../../../../testdata/runtime/storage_cost.json"
        ))
        .unwrap();
        let vector = vectors
            .iter()
            .find(|vector| vector["name"] == name)
            .unwrap();
        (
            vector["bytes"].as_u64().unwrap(),
            vector["allocations"].as_u64().unwrap(),
        )
    }

    #[test]
    fn byte_capacity_budget_rejects_before_host_scratch_allocation() {
        let program = test_helpers::program_with_artifact(|_| {});
        let mut vm = Instance::new(
            program,
            ExecutionLimits {
                max_guest_bytes: 64,
                ..ExecutionLimits::default()
            },
        )
        .unwrap();
        let before = vm.heap_stats();
        let error = vm
            .make_bytes(
                TypeIdentity::Slice(Arc::new(TypeIdentity::Primitive(wire::PrimitiveUint8))),
                0,
                usize::MAX,
                &[],
            )
            .unwrap_err();
        assert_eq!(error.code, "allocation_limit");
        let after = vm.heap_stats();
        assert_eq!(after.guest_bytes, before.guest_bytes);
        assert_eq!(after.allocations, before.allocations);
        vm.close().unwrap();
    }

    #[test]
    fn byte_backing_stays_live_until_operation_owner_releases_it() {
        let program = test_helpers::program_with_artifact(|_| {});
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        let value = vm
            .make_bytes(
                TypeIdentity::Slice(Arc::new(TypeIdentity::Primitive(wire::PrimitiveUint8))),
                3,
                3,
                &[7, 8, 9],
            )
            .unwrap();
        let root = match &value.data {
            Data::Slice(slice) => slice.storage.root,
            _ => panic!("byte backing must be a slice"),
        };
        vm.heap.flush_worker_lease().unwrap();
        vm.collect_rooted().unwrap();
        assert!(vm.heap.is_live(root));
        assert_eq!(vm.slice_bytes(&value).unwrap(), [7, 8, 9]);

        vm.running.transient_roots.clear();
        vm.collect_rooted().unwrap();
        assert!(!vm.heap.is_live(root));
        vm.close().unwrap();
    }

    #[test]
    fn slice_backings_match_shared_storage_cost_contract() {
        for (name, element, initial) in [
            (
                "byte_slice_backing",
                TypeIdentity::Primitive(wire::PrimitiveUint8),
                vec![Value {
                    typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                    data: Data::Unsigned(7),
                }],
            ),
            (
                "int_slice_backing",
                TypeIdentity::Primitive(wire::PrimitiveInt),
                vec![Value::int(7)],
            ),
        ] {
            let program = test_helpers::program_with_artifact(|_| {});
            let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
            let before = vm.heap_stats();
            let slice = vm
                .make_slice(TypeIdentity::Slice(Arc::new(element)), 1, 3, initial)
                .unwrap();
            vm.heap.flush_worker_lease().unwrap();
            let after = vm.heap_stats();
            let (expected_bytes, expected_allocations) = storage_cost(name);
            assert_eq!(
                after.total_allocated_bytes - before.total_allocated_bytes,
                expected_bytes,
                "{name}"
            );
            assert_eq!(
                after.allocations - before.allocations,
                expected_allocations,
                "{name}"
            );
            assert_eq!(vm.slice_values(&slice).unwrap().len(), 1);
            vm.close().unwrap();
        }
    }

    #[test]
    fn append_growth_rejection_preserves_both_backing_kinds() {
        for element in [wire::PrimitiveUint8, wire::PrimitiveInt] {
            let program = test_helpers::program_with_artifact(|_| {});
            // The empty program also owns its revision anchor. Admit that
            // owner and one backing while leaving growth unable to publish.
            let guest_budget = wire::STORAGE_RECORD_HEADER
                + storage_cost(if element == wire::PrimitiveUint8 {
                    "byte_slice_backing"
                } else {
                    "int_slice_backing"
                })
                .0;
            let mut vm = Instance::new(
                program,
                ExecutionLimits {
                    max_guest_bytes: guest_budget,
                    ..ExecutionLimits::default()
                },
            )
            .unwrap();
            let typ = TypeIdentity::Primitive(element);
            let initial = if element == wire::PrimitiveUint8 {
                Value {
                    typ: typ.clone(),
                    data: Data::Unsigned(7),
                }
            } else {
                Value::int(7)
            };
            let slice = vm
                .make_slice(
                    TypeIdentity::Slice(Arc::new(typ)),
                    1,
                    2,
                    vec![initial.clone()],
                )
                .unwrap();
            let before = vm.heap_stats();
            let error = vm
                .append_values(slice.clone(), vec![initial.clone(), initial.clone()])
                .unwrap_err();
            assert_eq!(error.code, "allocation_limit");
            assert_eq!(
                vm.heap_stats().total_allocated_bytes,
                before.total_allocated_bytes
            );
            let Data::Slice(header) = &slice.data else {
                panic!("slice header")
            };
            assert_eq!((header.length, header.capacity), (1, 2));
            assert!(matches!(
                vm.slice_values(&slice).unwrap()[0].data,
                Data::Integer(7) | Data::Unsigned(7)
            ));
            // Spare capacity remains usable after the failed growth.
            let appended = vm.append_values(slice.clone(), vec![initial]).unwrap();
            assert_eq!(vm.slice_values(&appended).unwrap().len(), 2);
            assert_eq!(vm.slice_values(&slice).unwrap().len(), 1);
            vm.close().unwrap();
        }
    }

    #[test]
    fn byte_reads_own_visible_range_for_compact_and_array_backing() {
        let program = test_helpers::program_with_artifact(|_| {});
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        let element = TypeIdentity::Primitive(wire::PrimitiveUint8);
        for compact in [false, true] {
            let bytes = [8, 0, 127, 255, 9];
            let data = if compact {
                Data::Bytes(bytes.to_vec())
            } else {
                Data::Array(
                    bytes
                        .iter()
                        .map(|byte| Value {
                            typ: element.clone(),
                            data: Data::Unsigned(u64::from(*byte)),
                        })
                        .collect::<Vec<_>>()
                        .into(),
                )
            };
            let root = vm
                .allocate(Value {
                    typ: TypeIdentity::Any,
                    data,
                })
                .unwrap();
            let slice = Value {
                typ: TypeIdentity::Slice(Arc::new(element.clone())),
                data: Data::Slice(Arc::new(SliceValue {
                    identity: Arc::default(),
                    storage: Address {
                        identity: Arc::default(),
                        root,
                        path: Vec::new(),
                    },
                    start: 1,
                    length: 3,
                    capacity: 4,
                })),
            };
            let mut owned = vm.slice_bytes(&slice).unwrap();
            assert_eq!(owned, [0, 127, 255]);
            owned[0] = 42;
            assert_eq!(vm.slice_bytes(&slice).unwrap(), [0, 127, 255]);
            assert_eq!(vm.read_byte_range(&slice, 1, 2).unwrap(), [127, 255]);
            assert!(vm.read_byte_range(&slice, 3, 0).unwrap().is_empty());
            assert_eq!(vm.read_byte_range(&slice, 3, 1).unwrap_err().code, "panic");
        }
        assert!(
            vm.slice_bytes(&Value {
                typ: TypeIdentity::Any,
                data: Data::Nil
            })
            .unwrap()
            .is_empty()
        );
        assert_eq!(
            vm.slice_bytes(&Value::int(42)).unwrap_err().code,
            "type_error"
        );
        vm.close().unwrap();
    }

    #[test]
    fn byte_reads_reject_invalid_array_elements_and_views() {
        let program = test_helpers::program_with_artifact(|_| {});
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        let element = TypeIdentity::Primitive(wire::PrimitiveUint8);
        for data in [Data::Unsigned(256), Data::Integer(1)] {
            let integer_input = matches!(data, Data::Integer(_));
            let allocation = vm.allocate(Value {
                typ: TypeIdentity::Any,
                data: Data::Array(
                    vec![Value {
                        typ: element.clone(),
                        data,
                    }]
                    .into(),
                ),
            });
            if integer_input {
                assert_eq!(allocation.unwrap_err().code, "typed_payload_unsupported");
                continue;
            }
            let root = allocation.unwrap();
            let mut slice = SliceValue {
                identity: Arc::default(),
                storage: Address {
                    identity: Arc::default(),
                    root,
                    path: Vec::new(),
                },
                start: 0,
                length: 1,
                capacity: 1,
            };
            let value = Value {
                typ: TypeIdentity::Slice(Arc::new(element.clone())),
                data: Data::Slice(Arc::new(slice.clone())),
            };
            assert_eq!(vm.slice_bytes(&value).unwrap_err().code, "type_error");
            slice.start = 1;
            let invalid = Value {
                typ: value.typ,
                data: Data::Slice(Arc::new(slice)),
            };
            assert_eq!(vm.slice_bytes(&invalid).unwrap_err().code, "invalid_slice");
        }
        vm.close().unwrap();
    }
}
