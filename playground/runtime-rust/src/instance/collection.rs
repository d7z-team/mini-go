//! Shared collection algorithms for bytecode and reflection.

use super::*;

impl Instance {
    pub(super) fn element_type(&self, typ: &TypeIdentity) -> Result<TypeIdentity, RuntimeError> {
        if let TypeIdentity::Slice(element) = typ {
            return Ok((**element).clone());
        }
        let (module, node) = self
            .types
            .node(typ)?
            .ok_or_else(|| RuntimeError::new("type_error", "container", "missing element type"))?;
        self.types.resolve(module, &node.elem)
    }

    pub(super) fn slice_values(&self, value: &Value) -> Result<Vec<Value>, RuntimeError> {
        match &value.data {
            Data::Slice(slice) => {
                let backing = self.snapshot_address(&slice.storage)?;
                if let Data::Bytes(bytes) = &backing.data {
                    return bytes
                        .get(slice.start..slice.start + slice.length)
                        .ok_or_else(|| {
                            RuntimeError::new("invalid_slice", "slice", "invalid byte view")
                        })
                        .map(|bytes| {
                            bytes
                                .iter()
                                .map(|byte| Value {
                                    typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                                    data: Data::Unsigned(u64::from(*byte)),
                                })
                                .collect()
                        });
                }
                let Data::Array(values) = &backing.data else {
                    return Err(RuntimeError::new(
                        "invalid_slice",
                        "slice",
                        "invalid backing",
                    ));
                };
                Ok(values
                    .get(slice.start..slice.start + slice.length)
                    .ok_or_else(|| RuntimeError::new("invalid_slice", "slice", "invalid view"))?
                    .to_vec())
            }
            Data::Nil => Ok(Vec::new()),
            Data::String(bytes) => Ok(bytes
                .iter()
                .map(|byte| Value {
                    typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                    data: Data::Unsigned(u64::from(*byte)),
                })
                .collect()),
            _ => Err(RuntimeError::new("type_error", "slice", "expected slice")),
        }
    }

    pub(super) fn make_slice(
        &mut self,
        typ: TypeIdentity,
        length: usize,
        capacity: usize,
        initial: Vec<Value>,
    ) -> Result<Value, RuntimeError> {
        let element = self.element_type(&typ)?;
        if self
            .types
            .identical(&element, &TypeIdentity::Primitive(wire::PrimitiveUint8))?
        {
            if !matches!(typ, TypeIdentity::Slice(_))
                && !self
                    .types
                    .node(&typ)?
                    .is_some_and(|(_, node)| node.kind == wire::Slice)
            {
                return Err(RuntimeError::new(
                    "type_error",
                    "slice",
                    "expected slice type",
                ));
            }
            let bytes = initial
                .into_iter()
                .map(|value| {
                    let value = self.coerce(value, &element)?;
                    match value.data {
                        Data::Unsigned(byte) => Ok(byte as u8),
                        _ => Err(RuntimeError::new(
                            "type_error",
                            "slice",
                            "expected byte element",
                        )),
                    }
                })
                .collect::<Result<Vec<_>, _>>()?;
            return self.make_bytes(typ, length, capacity, &bytes);
        }
        self.make_slot_slice(typ, length, capacity, initial)
    }

    pub(super) fn make_slot_slice(
        &mut self,
        typ: TypeIdentity,
        length: usize,
        capacity: usize,
        initial: Vec<Value>,
    ) -> Result<Value, RuntimeError> {
        if !matches!(typ, TypeIdentity::Slice(_))
            && !self
                .types
                .node(&typ)?
                .is_some_and(|(_, node)| node.kind == wire::Slice)
        {
            return Err(RuntimeError::new(
                "type_error",
                "slice",
                "expected slice type",
            ));
        }
        if length > capacity || capacity > self.limits.max_sequence_elements {
            return Err(RuntimeError::new(
                "value_limit",
                "slice",
                "invalid or excessive slice capacity",
            ));
        }
        let element = self.element_type(&typ)?;
        if initial.len() > length {
            return Err(RuntimeError::new(
                "invalid_slice",
                "slice",
                "too many initial elements",
            ));
        }
        let zero = self.zero(&element, 0)?;
        let initial = initial
            .into_iter()
            .map(|value| self.coerce(value, &element))
            .collect::<Result<Vec<_>, _>>()?;
        let zero_bytes = zero.logical_bytes()?.saturating_sub(16);
        let mut logical = (capacity as u64)
            .checked_mul(16)
            .and_then(|bytes| bytes.checked_add(272))
            .and_then(|bytes| {
                zero_bytes
                    .checked_mul((capacity - initial.len()) as u64)?
                    .checked_add(bytes)
            })
            .ok_or_else(|| {
                RuntimeError::new("allocation_limit", "slice", "backing size overflow")
            })?;
        for value in &initial {
            logical = logical
                .checked_add(value.logical_bytes()?.saturating_sub(16))
                .ok_or_else(|| {
                    RuntimeError::new("allocation_limit", "slice", "backing size overflow")
                })?;
        }
        if logical > self.limits.max_heap_bytes {
            return Err(RuntimeError::new(
                "allocation_limit",
                "slice",
                "backing exceeds heap budget",
            ));
        }
        let mut values = Vec::new();
        values.try_reserve_exact(capacity).map_err(|_| {
            RuntimeError::new(
                "allocation_limit",
                "slice",
                "slot backing allocation failed",
            )
        })?;
        values.resize(capacity, zero);
        for (destination, value) in values.iter_mut().zip(initial) {
            *destination = value;
        }
        let root = self.allocate(Value {
            typ: TypeIdentity::Any,
            data: Data::Array(values.into()),
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

    pub(super) fn index_value(
        &mut self,
        object: &Value,
        key: &Value,
    ) -> Result<(Value, bool), RuntimeError> {
        if let Data::Map(root) = object.data {
            let key = self.map_key(&object.typ, key.clone())?;
            let Data::MapEntries(entries) = &self.heap.get(root)?.data else {
                return Err(RuntimeError::new("invalid_map", "map", "invalid backing"));
            };
            if let Some(index) = entries.find(&key, &self.types)? {
                return Ok((entries[index].1.clone(), true));
            }
            return Ok((self.zero(&self.element_type(&object.typ)?, 0)?, false));
        }
        if matches!(object.data, Data::Nil)
            && self
                .types
                .node(&object.typ)?
                .is_some_and(|(_, node)| node.kind == wire::Map)
        {
            self.map_key(&object.typ, key.clone())?;
            return Ok((self.zero(&self.element_type(&object.typ)?, 0)?, false));
        }
        let index = usize::try_from(key.integer()?)
            .map_err(|_| RuntimeError::new("panic", "index", "negative index"))?;
        let value = match &object.data {
            Data::Array(values) => values.get(index).cloned(),
            Data::String(bytes) => bytes.get(index).map(|byte| Value {
                typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                data: Data::Unsigned(u64::from(*byte)),
            }),
            Data::Slice(slice) if index < slice.length => {
                if slice.storage.path.is_empty() {
                    let backing = self.heap.get(slice.storage.root)?;
                    let value = match &backing.data {
                        Data::Array(values) => values
                            .get(slice.start + index)
                            .filter(|value| !matches!(value.data, Data::Uninitialized))
                            .cloned(),
                        Data::Bytes(bytes) => bytes.get(slice.start + index).map(|byte| Value {
                            typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                            data: Data::Unsigned(u64::from(*byte)),
                        }),
                        _ => None,
                    };
                    if let Some(value) = value {
                        return Ok((value, true));
                    }
                }
                let mut address = slice.storage.clone();
                address.path.push(PathElement::Index(slice.start + index));
                Some(self.read_address(&address)?)
            }
            Data::Pointer(address) => {
                let value = self.read_address(address)?;
                return self.index_value(&value, key);
            }
            _ => None,
        }
        .ok_or_else(|| RuntimeError::new("panic", "index", "index outside sequence"))?;
        Ok((value, true))
    }

    pub(super) fn store_index(
        &mut self,
        object: Value,
        key: Value,
        value: Value,
    ) -> Result<(), RuntimeError> {
        let mut write = self.prepare_index_write(object, key, value)?;
        let mut collection = mutation::WriteCollection::IMMEDIATE;
        while !self.attempt_write(&mut write, &mut collection)? {}
        Ok(())
    }

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
                let header = Arc::make_mut(&mut slice);
                header.identity = Arc::default();
                if maximum > header.capacity {
                    return Err(RuntimeError::new(
                        "panic",
                        "slice",
                        "slice bounds exceed capacity",
                    ));
                }
                header.start += low;
                header.length = high - low;
                header.capacity = if full {
                    maximum - low
                } else {
                    header.capacity - low
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
                    Some(address) => Arc::unwrap_or_clone(address),
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
                    data: Data::Slice(std::sync::Arc::new(SliceValue {
                        identity: std::sync::Arc::default(),
                        storage,
                        start: low,
                        length: high - low,
                        capacity,
                    })),
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
        if self
            .types
            .identical(&element, &TypeIdentity::Primitive(wire::PrimitiveUint8))?
        {
            let bytes = values
                .iter()
                .map(|value| match value.data {
                    Data::Unsigned(byte) => byte as u8,
                    _ => unreachable!("byte coercion succeeded"),
                })
                .collect::<Vec<_>>();
            return self.append_bytes(object, &bytes);
        }
        let (new_length, new_capacity) =
            self.reserve_slice_append(length, capacity, values.len())?;
        if new_capacity > capacity {
            let mut initial = self.slice_values(&object)?;
            initial.extend(values);
            object = self.make_slice(object.typ, new_length, new_capacity, initial)?;
        } else if let Data::Slice(slice) = &mut object.data {
            let slice = Arc::make_mut(slice);
            slice.identity = Arc::default();
            slice.length = new_length;
            for (index, value) in values.into_iter().enumerate() {
                self.store_index(object.clone(), Value::int((length + index) as i64), value)?;
            }
        }
        Ok(object)
    }

    /// Validate and charge logical growth before mutating either backing kind.
    pub(super) fn reserve_slice_append(
        &mut self,
        length: usize,
        capacity: usize,
        added: usize,
    ) -> Result<(usize, usize), RuntimeError> {
        let new_length = length
            .checked_add(added)
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
        Ok((new_length, new_capacity))
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
            let length = match &source.data {
                Data::Slice(slice) => slice.length,
                Data::String(bytes) => bytes.len(),
                Data::Nil => 0,
                _ => {
                    return Err(RuntimeError::new(
                        "type_error",
                        "copy",
                        "expected byte source",
                    ));
                }
            };
            let copied = slice.length.min(length);
            let source = self.read_byte_range(&source, 0, copied)?;
            self.write_slice_bytes(&destination, 0, &source)?;
            return Ok(copied);
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
