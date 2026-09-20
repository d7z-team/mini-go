//! Shared collection algorithms for bytecode and reflection.

use super::*;

impl Instance {
    pub(super) fn init_map_iterator(
        &mut self,
        local: &str,
        object: Value,
    ) -> Result<(), RuntimeError> {
        let entries = match &object.data {
            Data::Map(root) => match &self.heap.get(*root)?.data {
                Data::MapEntries(entries) => entries.entry_ids().to_vec(),
                _ => {
                    return Err(RuntimeError::new(
                        "invalid_map",
                        "iterator",
                        "invalid backing",
                    ));
                }
            },
            Data::Nil
                if self
                    .types
                    .node(&object.typ)?
                    .is_some_and(|(_, node)| node.kind == wire::Map) =>
            {
                Vec::new()
            }
            _ => {
                return Err(RuntimeError::new("type_error", "iterator", "expected map"));
            }
        };
        self.running
            .frames
            .last_mut()
            .unwrap()
            .map_iterators
            .remove(local);
        self.charge_guest(128 + entries.len() as u64 * 32)?;
        self.running
            .frames
            .last_mut()
            .unwrap()
            .map_iterators
            .insert(
                local.to_owned(),
                frame::MapIterator {
                    object,
                    entries,
                    position: 0,
                },
            );
        Ok(())
    }

    pub(super) fn next_map_entry(
        &mut self,
        local: &str,
    ) -> Result<(Value, Value, bool), RuntimeError> {
        let iterator = self
            .running
            .frames
            .last_mut()
            .unwrap()
            .map_iterators
            .get_mut(local)
            .ok_or_else(|| {
                RuntimeError::new(
                    "invalid_iterator",
                    "iterator",
                    "iterator is not initialized",
                )
            })?;
        let mut found = None;
        if let Data::Map(root) = &iterator.object.data {
            let Data::MapEntries(entries) = &self.heap.get(*root)?.data else {
                return Err(RuntimeError::new(
                    "invalid_map",
                    "iterator",
                    "invalid backing",
                ));
            };
            while iterator.position < iterator.entries.len() {
                let id = iterator.entries[iterator.position];
                iterator.position += 1;
                if let Some(entry) = entries.entry_by_id(id) {
                    found = Some(entry.clone());
                    break;
                }
            }
        }
        let typ = iterator.object.typ.clone();
        let ok = found.is_some();
        if !ok {
            iterator.entries = Vec::new();
        }
        let (key, value) = match found {
            Some(pair) => pair,
            None => {
                let (module, node) = self.types.node(&typ)?.ok_or_else(|| {
                    RuntimeError::new("type_error", "iterator", "missing map type")
                })?;
                let key = self.types.resolve(module, &node.key)?;
                let value = self.types.resolve(module, &node.elem)?;
                (self.zero(&key, 0)?, self.zero(&value, 0)?)
            }
        };
        Ok((key, value, ok))
    }
    pub(super) fn slice_value(
        &mut self,
        mut object: Value,
        low: i64,
        high: i64,
        max: Value,
    ) -> Result<Value, RuntimeError> {
        let mut storage = None;
        if let Data::Pointer(address) = &object.data {
            storage = Some(address.clone());
            object = self.read_address(address)?;
        }
        let full = max.typ != TypeIdentity::Void;
        let maximum = if full { max.integer()? } else { high };
        if low < 0 || high < low || maximum < high {
            return Err(RuntimeError::new("panic", "slice", "invalid slice bounds"));
        }
        let (low, high, maximum) = (low as usize, high as usize, maximum as usize);
        Ok(match object.data {
            Data::String(bytes) => {
                if full || high > bytes.len() {
                    return Err(RuntimeError::new(
                        "panic",
                        "slice",
                        "invalid string slice bounds",
                    ));
                }
                self.charge_guest((high - low) as u64)?;
                Value {
                    typ: object.typ,
                    data: Data::String(bytes.slice(low..high)),
                }
            }
            Data::Slice(mut slice) => {
                slice.identity = Arc::default();
                if maximum > slice.capacity {
                    return Err(RuntimeError::new(
                        "panic",
                        "slice",
                        "slice bounds exceed capacity",
                    ));
                }
                slice.start += low;
                slice.length = high - low;
                slice.capacity = if full {
                    maximum - low
                } else {
                    slice.capacity - low
                };
                Value {
                    typ: object.typ,
                    data: Data::Slice(slice),
                }
            }
            Data::Array(values) => {
                if maximum > values.len() {
                    return Err(RuntimeError::new(
                        "panic",
                        "slice",
                        "array slice bounds exceed length",
                    ));
                }
                let capacity = if full {
                    maximum - low
                } else {
                    values.len() - low
                };
                let element = self.element_type(&object.typ)?;
                let storage = match storage {
                    Some(address) => address,
                    None => Address {
                        identity: std::sync::Arc::default(),
                        root: self.allocate(Value {
                            typ: object.typ,
                            data: Data::Array(values),
                        })?,
                        path: Vec::new(),
                    },
                };
                Value {
                    typ: TypeIdentity::Slice(std::sync::Arc::new(element)),
                    data: Data::Slice(SliceValue {
                        identity: std::sync::Arc::default(),
                        storage,
                        start: low,
                        length: high - low,
                        capacity,
                    }),
                }
            }
            Data::Nil if maximum == 0 => object,
            _ => return Err(RuntimeError::new("panic", "slice", "cannot slice value")),
        })
    }

    pub(super) fn delete_key(&mut self, object: Value, key: Value) -> Result<(), RuntimeError> {
        if let Some(mut write) = self.prepare_delete(object, key)? {
            let mut collection = mutation::WriteCollection::IMMEDIATE;
            while !self.attempt_write(&mut write, &mut collection)? {}
        }
        Ok(())
    }

    pub(super) fn map_key(&self, typ: &TypeIdentity, key: Value) -> Result<Value, RuntimeError> {
        let (module, node) = self
            .types
            .node(typ)?
            .filter(|(_, node)| node.kind == wire::Map)
            .ok_or_else(|| RuntimeError::new("type_error", "map", "expected map type"))?;
        let key_type = self.types.resolve(module, &node.key)?;
        let key = self.coerce(key, &key_type)?;
        crate::operators::equal(&key, &key, &self.types)?;
        Ok(key)
    }

    pub(super) fn append_values(
        &mut self,
        mut object: Value,
        values: Vec<Value>,
    ) -> Result<Value, RuntimeError> {
        let (length, capacity) = match &object.data {
            Data::Slice(slice) => (slice.length, slice.capacity),
            Data::Nil => (0, 0),
            _ => return Err(RuntimeError::new("type_error", "append", "expected slice")),
        };
        let element = self.element_type(&object.typ)?;
        let values = values
            .into_iter()
            .map(|value| self.coerce(value, &element))
            .collect::<Result<Vec<_>, _>>()?;
        let new_length = length
            .checked_add(values.len())
            .filter(|length| *length <= self.limits.max_sequence_elements)
            .ok_or_else(|| {
                RuntimeError::new("value_limit", "append", "slice length exceeds limit")
            })?;
        let new_capacity = if new_length > capacity {
            new_length.max(capacity.saturating_mul(2))
        } else {
            capacity
        };
        if new_capacity > capacity {
            if new_capacity > self.limits.max_sequence_elements {
                return Err(RuntimeError::new(
                    "value_limit",
                    "append",
                    "slice capacity exceeds limit",
                ));
            }
            let bytes = ((new_capacity - capacity) as u64)
                .checked_mul(16)
                .ok_or_else(|| {
                    RuntimeError::new("allocation_limit", "append", "slice growth size overflow")
                })?;
            self.charge_guest(bytes)?;
        }
        if self
            .types
            .identical(&element, &TypeIdentity::Primitive(wire::PrimitiveUint8))?
        {
            let compact = match &object.data {
                Data::Slice(slice) => {
                    matches!(self.snapshot_address(&slice.storage)?.data, Data::Bytes(_))
                }
                _ => false,
            };
            if new_capacity > capacity || !compact {
                let mut bytes = self.slice_bytes(&object)?;
                for value in values {
                    let Data::Unsigned(byte) = value.data else {
                        unreachable!("byte coercion succeeded")
                    };
                    bytes.push(byte as u8);
                }
                return self.make_bytes(object.typ, new_length, new_capacity, &bytes);
            }
        }
        if new_capacity > capacity {
            let mut initial = self.slice_values(&object)?;
            initial.extend(values);
            object = self.make_slice(object.typ, new_length, new_capacity, initial)?;
        } else if let Data::Slice(slice) = &mut object.data {
            slice.identity = Arc::default();
            slice.length = new_length;
            for (index, value) in values.into_iter().enumerate() {
                self.store_index(object.clone(), Value::int((length + index) as i64), value)?;
            }
        }
        Ok(object)
    }

    pub(super) fn copy_values(
        &mut self,
        destination: Value,
        source: Value,
    ) -> Result<usize, RuntimeError> {
        if let Data::Slice(slice) = &destination.data
            && slice.storage.path.is_empty()
            && matches!(self.heap.get(slice.storage.root)?.data, Data::Bytes(_))
        {
            let source = self.slice_bytes(&source)?;
            let copied = slice.length.min(source.len());
            loop {
                let (snapshot, bytes) = self.heap.read_mutation_base(slice.storage.root)?;
                let expected = Arc::downgrade(&snapshot);
                drop(snapshot);
                if self
                    .heap
                    .update(slice.storage.root, &expected, bytes, true, |backing| {
                        let Data::Bytes(bytes) = &mut backing.data else {
                            unreachable!()
                        };
                        bytes[slice.start..slice.start + copied].copy_from_slice(&source[..copied]);
                    })?
                {
                    return Ok(copied);
                }
            }
        }
        // Snapshot first so overlapping views implement memmove semantics.
        let source = self.slice_values(&source)?;
        let length = match &destination.data {
            Data::Slice(slice) => slice.length,
            Data::Nil => 0,
            _ => {
                return Err(RuntimeError::new(
                    "type_error",
                    "copy",
                    "expected slice destination",
                ));
            }
        };
        let copied = length.min(source.len());
        for (index, value) in source.into_iter().take(copied).enumerate() {
            self.store_index(destination.clone(), Value::int(index as i64), value)?;
        }
        Ok(copied)
    }
}
