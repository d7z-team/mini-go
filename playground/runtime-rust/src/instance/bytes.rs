//! Compact byte storage operations. Reads snapshot before writes so aliases
//! retain memmove/append semantics; mutations commit through the heap owner.

use super::*;

impl Instance {
    pub(super) fn check_string_size(&self, length: usize) -> Result<(), RuntimeError> {
        if length > self.limits.max_string_bytes {
            return Err(RuntimeError::new(
                "string_limit",
                "string",
                "string exceeds byte limit",
            ));
        }
        if length as u64 > self.limits.max_heap_bytes {
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
            let backing = self.snapshot_address(&slice.storage)?;
            if let Data::Bytes(bytes) = &backing.data {
                return bytes
                    .get(slice.start..slice.start + slice.length)
                    .map(<[u8]>::to_vec)
                    .ok_or_else(|| {
                        RuntimeError::new("invalid_slice", "bytes", "invalid byte view")
                    });
            }
            let Data::Array(values) = &backing.data else {
                return Err(RuntimeError::new(
                    "invalid_slice",
                    "slice",
                    "invalid backing",
                ));
            };
            let values = values
                .get(slice.start..slice.start + slice.length)
                .ok_or_else(|| RuntimeError::new("invalid_slice", "slice", "invalid view"))?;
            let mut bytes = Vec::with_capacity(values.len());
            for value in values {
                match value.data {
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
        if initial.len() > length
            || length > capacity
            || capacity > self.limits.max_sequence_elements
            || (capacity as u64).saturating_add(272) > self.limits.max_heap_bytes
        {
            return Err(RuntimeError::new(
                "allocation_limit",
                "bytes",
                "byte backing exceeds instance limits",
            ));
        }
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
        let root = self.allocate(Value {
            typ: TypeIdentity::Any,
            data: Data::Bytes(bytes),
        })?;
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
            let backing = self.snapshot_address(&slice.storage)?;
            match &backing.data {
                Data::Bytes(bytes) => {
                    return Ok(bytes[slice.start + start..slice.start + start + length].to_vec());
                }
                Data::Array(values) => {
                    return values[slice.start + start..slice.start + start + length]
                        .iter()
                        .map(|value| match value.data {
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
                let (snapshot, bytes) = self.heap.read_mutation_base(slice.storage.root)?;
                if !matches!(snapshot.data, Data::Bytes(_)) {
                    break;
                }
                let expected = Arc::downgrade(&snapshot);
                drop(snapshot);
                if self
                    .heap
                    .update(slice.storage.root, expected, bytes, true, |backing| {
                        let Data::Bytes(bytes) = &mut backing.data else {
                            unreachable!()
                        };
                        bytes[slice.start + offset..slice.start + offset + source.len()]
                            .copy_from_slice(source);
                    })?
                {
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
        let new_length = length
            .checked_add(source.len())
            .filter(|length| *length <= self.limits.max_sequence_elements)
            .ok_or_else(|| {
                RuntimeError::new("value_limit", "append", "slice length exceeds limit")
            })?;
        if new_length > capacity {
            let new_capacity = new_length.max(capacity.saturating_mul(2));
            if new_capacity > self.limits.max_sequence_elements {
                return Err(RuntimeError::new(
                    "value_limit",
                    "append",
                    "slice capacity exceeds limit",
                ));
            }
            self.charge_guest((new_capacity - capacity) as u64 * 16)?;
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
            let root = vm
                .allocate(Value {
                    typ: TypeIdentity::Any,
                    data: Data::Array(
                        vec![Value {
                            typ: element.clone(),
                            data,
                        }]
                        .into(),
                    ),
                })
                .unwrap();
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
