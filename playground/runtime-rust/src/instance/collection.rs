//! Shared collection algorithms for bytecode and reflection.

use super::*;

impl Instance {
    pub(super) fn make_empty_map(&mut self, typ: TypeIdentity) -> Result<Value, RuntimeError> {
        let element = self.element_type(&typ)?;
        let key_type = self
            .types
            .node(&typ)?
            .and_then(|(module, node)| self.types.resolve(module, &node.key).ok())
            .ok_or_else(|| RuntimeError::new("type_error", "make_map", "missing map key type"))?;
        let key_layout = self
            .managed_map_layout(&key_type)?
            .expect("map layout is total");
        let value_layout = self
            .managed_map_layout(&element)?
            .expect("map layout is total");
        let root = self.allocate(Value {
            typ: TypeIdentity::Any,
            data: Data::Nil,
        })?;
        self.heap.flush_worker_lease()?;
        let storage = self.heap.storage_ref(root)?;
        let managed = crate::managed_map::ManagedMap::attach(
            self.heap.storage_pool(),
            storage,
            key_layout,
            value_layout,
        )?;
        self.managed_maps.insert(root, managed)?;
        self.managed_maps.set_type(root, typ.clone())?;
        Ok(Value {
            typ,
            data: Data::Map(root),
        })
    }

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

    pub(super) fn container_length(
        &mut self,
        mut value: Value,
        capacity: bool,
    ) -> Result<i64, RuntimeError> {
        let pointee = self.types.pointer_element(&value.typ)?;
        if let Some(element) = pointee
            && let Some((_, node)) = self.types.node(&element)?
            && node.kind == wire::Array
        {
            return Ok(node.length);
        }
        if let Data::Pointer(address) = &value.data {
            value = self.read_address(address)?;
        }
        let length = match value.data {
            Data::String(bytes) if !capacity => bytes.len(),
            Data::Array(values) => values.len(),
            Data::Aggregate(value) if value.is_array() => value.len(),
            Data::Nil => 0,
            Data::Slice(slice) => {
                if capacity {
                    slice.capacity
                } else {
                    slice.length
                }
            }
            Data::Map(root) => {
                let map = self.managed_maps.get(root).ok_or_else(|| {
                    RuntimeError::new("invalid_map", "len", "map owner is missing")
                })?;
                let length = if capacity {
                    map.capacity()?
                } else {
                    map.len()?
                };
                length as usize
            }
            Data::ResourceRef(root) => {
                let scheduler::Resource::Channel { channel } = &*self.resource(root)? else {
                    return Err(RuntimeError::new("type_error", "len", "expected channel"));
                };
                let channel = channel
                    .lock()
                    .map_err(|_| RuntimeError::new("runtime", "len", "channel lock poisoned"))?;
                if capacity {
                    channel.capacity()
                } else {
                    channel.len()
                }
            }
            _ => {
                return Err(RuntimeError::new("type_error", "len", "expected container"));
            }
        };
        Ok(length as i64)
    }

    pub(super) fn slice_values(&self, value: &Value) -> Result<Vec<Value>, RuntimeError> {
        match &value.data {
            Data::Slice(slice) => {
                if slice.storage.path.is_empty()
                    && let Some(bytes) = self.heap.read_byte_storage_range(
                        slice.storage.root,
                        slice.start,
                        slice.length,
                    )?
                {
                    return Ok(bytes
                        .into_iter()
                        .map(|byte| Value {
                            typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                            data: Data::Unsigned(u64::from(byte)),
                        })
                        .collect());
                }
                (0..slice.length)
                    .map(|index| {
                        let mut address = slice.storage.clone();
                        address.path.push(PathElement::Index(slice.start + index));
                        self.snapshot_address(&address)
                    })
                    .collect()
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
        if length > capacity {
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
        let initial = initial
            .into_iter()
            .map(|value| self.coerce(value, &element))
            .collect::<Result<Vec<_>, _>>()?;
        let backing_type = self.types.construct(
            crate::types::ConstructedType::Array {
                length: capacity,
                element: element.clone(),
            },
            self.limits.max_dynamic_types,
            self.types.traversal_limit(self.limits.max_dynamic_types),
        )?;
        self.types.register_dynamic(
            &backing_type,
            self.limits.max_dynamic_types,
            self.limits.max_dynamic_type_bytes,
            self.types.traversal_limit(self.limits.max_dynamic_types),
        )?;
        self.heap.set_type_registry(self.types.clone());
        let value =
            self.construct_aggregate(backing_type, initial.into_iter().enumerate().collect())?;
        let root = self.allocate(value)?;
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
            let hash = crate::managed_map::hash_value(&key)?;
            let key_type = key.typ.clone();
            let map = self
                .managed_maps
                .get(root)
                .ok_or_else(|| RuntimeError::new("invalid_map", "map", "map owner is missing"))?;
            let identity = map.find_lane(hash, |candidate| {
                self.heap
                    .decode_map_lane(&key_type, &candidate.bytes, &candidate.references)
                    .ok()
                    .and_then(|other| crate::operators::equal(&other, &key, &self.types).ok())
                    .unwrap_or(false)
            })?;
            if let Some(identity) = identity {
                let entry = map
                    .entry(identity)?
                    .ok_or_else(|| RuntimeError::new("invalid_map", "map", "stale map entry"))?;
                let value_type = self.element_type(&object.typ)?;
                let decoded = self.heap.decode_map_lane(
                    &value_type,
                    &entry.value,
                    &entry.value_references,
                )?;
                return Ok((decoded, true));
            }
            let value_type = self.element_type(&object.typ)?;
            return Ok((self.zero(&value_type)?, false));
        }
        if matches!(object.data, Data::Nil)
            && self
                .types
                .node(&object.typ)?
                .is_some_and(|(_, node)| node.kind == wire::Map)
        {
            self.map_key(&object.typ, key.clone())?;
            return Ok((self.zero(&self.element_type(&object.typ)?)?, false));
        }
        let index = key.index()?;
        let value = match &object.data {
            Data::Aggregate(value) if value.is_array() && index < value.len() => {
                Some(value.get(index)?)
            }
            Data::Array(values) => values.get(index).cloned(),
            Data::String(bytes) => bytes.get(index).map(|byte| Value {
                typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                data: Data::Unsigned(u64::from(*byte)),
            }),
            Data::Slice(slice) if index < slice.length => {
                if slice.storage.path.is_empty()
                    && let Some(bytes) = self.heap.read_byte_storage_range(
                        slice.storage.root,
                        slice.start + index,
                        1,
                    )?
                {
                    return Ok((
                        Value {
                            typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                            data: Data::Unsigned(u64::from(bytes[0])),
                        },
                        true,
                    ));
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
        local: usize,
        object: Value,
    ) -> Result<(), RuntimeError> {
        if self
            .running
            .frames
            .last()
            .is_none_or(|frame| local >= frame.map_iterators.len())
        {
            return Err(RuntimeError::new(
                "invalid_iterator",
                "iterator",
                "iterator local is out of range",
            ));
        }
        let entries = match &object.data {
            Data::Map(root) => {
                let map = self.managed_maps.get(*root).ok_or_else(|| {
                    RuntimeError::new("invalid_map", "iterator", "map owner is missing")
                })?;
                map.snapshot()?
                    .into_iter()
                    .map(|identity| {
                        (u64::from(identity.generation) << 32) | u64::from(identity.index)
                    })
                    .collect()
            }
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
        // Reserve the replacement before touching the current iterator. A
        // quota/collection failure must leave the existing iterator and its
        // owner lease usable for a retry.
        let lease = self.acquire_owner_lease(64 + entries.len() as u64 * 8)?;
        let iterators = &mut self.running.frames.last_mut().unwrap().map_iterators;
        iterators[local] = Some(frame::MapIterator {
            object,
            entries,
            position: 0,
            lease: Some(lease),
        });
        Ok(())
    }

    pub(super) fn next_map_entry(
        &mut self,
        local: usize,
    ) -> Result<(Value, Value, bool), RuntimeError> {
        let iterator = self
            .running
            .frames
            .last_mut()
            .unwrap()
            .map_iterators
            .get_mut(local)
            .and_then(Option::as_mut)
            .ok_or_else(|| {
                RuntimeError::new(
                    "invalid_iterator",
                    "iterator",
                    "iterator is not initialized",
                )
            })?;
        let mut found = None;
        if let Data::Map(root) = &iterator.object.data {
            {
                let (module, node) = self.types.node(&iterator.object.typ)?.ok_or_else(|| {
                    RuntimeError::new("type_error", "iterator", "missing map type")
                })?;
                let key_type = self.types.resolve(module, &node.key)?;
                let value_type = self.types.resolve(module, &node.elem)?;
                while iterator.position < iterator.entries.len() {
                    let packed = iterator.entries[iterator.position];
                    iterator.position += 1;
                    let identity = crate::managed_map::MapEntryIdentity {
                        index: packed as u32,
                        generation: (packed >> 32) as u32,
                    };
                    let Some(entry) = self
                        .managed_maps
                        .get(*root)
                        .expect("managed map owner remains registered")
                        .entry(identity)?
                    else {
                        continue;
                    };
                    let key =
                        self.heap
                            .decode_map_lane(&key_type, &entry.key, &entry.key_references)?;
                    let value = self.heap.decode_map_lane(
                        &value_type,
                        &entry.value,
                        &entry.value_references,
                    )?;
                    found = Some((key, value));
                    break;
                }
            }
        }
        let typ = iterator.object.typ.clone();
        let ok = found.is_some();
        if !ok {
            iterator.entries = Vec::new();
            iterator.lease.take();
        }
        let (key, value) = match found {
            Some(pair) => pair,
            None => {
                let (module, node) = self.types.node(&typ)?.ok_or_else(|| {
                    RuntimeError::new("type_error", "iterator", "missing map type")
                })?;
                let key = self.types.resolve(module, &node.key)?;
                let value = self.types.resolve(module, &node.elem)?;
                (self.zero(&key)?, self.zero(&value)?)
            }
        };
        Ok((key, value, ok))
    }
    pub(super) fn slice_value(
        &mut self,
        mut object: Value,
        low: usize,
        high: usize,
        maximum: Option<usize>,
    ) -> Result<Value, RuntimeError> {
        let mut storage = None;
        if let Data::Pointer(address) = &object.data {
            storage = Some(address.clone());
            object = self.read_address(address)?;
        }
        let full = maximum.is_some();
        let maximum = maximum.unwrap_or(high);
        if high < low || maximum < high {
            return Err(RuntimeError::new("panic", "slice", "invalid slice bounds"));
        }
        Ok(match object.data {
            Data::String(bytes) => {
                if full || high > bytes.len() {
                    return Err(RuntimeError::new(
                        "panic",
                        "slice",
                        "invalid string slice bounds",
                    ));
                }
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
            data if data.aggregate_shape().is_some_and(|(array, _)| array) => {
                let length = data.aggregate_shape().unwrap().1;
                if maximum > length {
                    return Err(RuntimeError::new(
                        "panic",
                        "slice",
                        "array slice bounds exceed length",
                    ));
                }
                let capacity = if full { maximum - low } else { length - low };
                let element = self.element_type(&object.typ)?;
                let storage = match storage {
                    Some(address) => Arc::unwrap_or_clone(address),
                    None => Address {
                        identity: std::sync::Arc::default(),
                        root: self.allocate(Value {
                            typ: object.typ,
                            data,
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

    pub(super) fn managed_map_layout(
        &self,
        typ: &TypeIdentity,
    ) -> Result<Option<crate::managed_map::MapValueLayout>, RuntimeError> {
        let layout = self.heap.layout_for_type(typ)?;
        let stride = layout.stride;
        // A dynamic descriptor owns its child reference at `dynamic + 8`.
        // The compiler layout exposes that slot in `references` for generic
        // storage consumers, while ManagedMap records dynamic lanes
        // separately so its validator and scanner do not count it twice.
        let references = layout
            .references
            .iter()
            .copied()
            .filter(|reference| {
                !layout
                    .dynamics
                    .iter()
                    .any(|dynamic| dynamic.checked_add(8) == Some(*reference))
            })
            .collect();
        Ok(Some(crate::managed_map::MapValueLayout {
            stride,
            references,
            dynamics: layout.dynamics.to_vec(),
            logical: self.types.storage_logical_bytes(typ)?,
        }))
    }

    pub(super) fn materialize_managed_map(
        &self,
        root: Handle,
        typ: &TypeIdentity,
    ) -> Result<Vec<(Value, Value)>, RuntimeError> {
        let map = self
            .managed_maps
            .get(root)
            .ok_or_else(|| RuntimeError::new("invalid_map", "map", "map owner is missing"))?;
        let (module, node) = self
            .types
            .node(typ)?
            .ok_or_else(|| RuntimeError::new("type_error", "map", "missing map type"))?;
        let key_type = self.types.resolve(module, &node.key)?;
        let value_type = self.types.resolve(module, &node.elem)?;
        let mut entries = Vec::with_capacity(map.len()? as usize);
        for identity in map.snapshot()? {
            if let Some(entry) = map.entry(identity)? {
                entries.push((
                    self.heap
                        .decode_map_lane(&key_type, &entry.key, &entry.key_references)?,
                    self.heap.decode_map_lane(
                        &value_type,
                        &entry.value,
                        &entry.value_references,
                    )?,
                ));
            }
        }
        Ok(entries)
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
        let new_length = length.checked_add(added).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "append", "slice length overflows")
        })?;
        let new_capacity = if new_length > capacity {
            new_length.max(capacity.checked_mul(2).ok_or_else(|| {
                RuntimeError::new("allocation_limit", "append", "slice capacity overflows")
            })?)
        } else {
            capacity
        };
        Ok((new_length, new_capacity))
    }

    pub(super) fn copy_values(
        &mut self,
        destination: Value,
        source: Value,
    ) -> Result<usize, RuntimeError> {
        if let Data::Slice(slice) = &destination.data
            && slice.storage.path.is_empty()
            && self
                .heap
                .read_byte_storage_range(slice.storage.root, 0, 0)?
                .is_some()
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
        if let (Data::Slice(destination), Data::Slice(source)) = (&destination.data, &source.data) {
            let copied = destination.length.min(source.length);
            if copied == 0 {
                return Ok(0);
            }
            let mut destination_address = destination.storage.clone();
            destination_address
                .path
                .push(PathElement::Index(destination.start));
            let mut source_address = source.storage.clone();
            source_address.path.push(PathElement::Index(source.start));
            if let (
                Some((destination_offset, destination_type, 0)),
                Some((source_offset, source_type, 0)),
            ) = (
                self.numeric_location(&destination_address)?,
                self.numeric_location(&source_address)?,
            ) && destination_type == source_type
            {
                let element = self
                    .types
                    .identity_for_storage_type_id(destination_type)
                    .ok_or_else(|| {
                        RuntimeError::new("unknown_type", "copy", "unknown element layout")
                    })?;
                let width = u64::from(self.types.storage_lane_layout(&element)?.stride);
                let length = width.checked_mul(copied as u64).ok_or_else(|| {
                    RuntimeError::new("invalid_slice", "copy", "copy length overflows")
                })?;
                self.heap.copy_prepared_range(
                    source_address.root,
                    source_offset,
                    destination_address.root,
                    destination_offset,
                    length,
                )?;
                return Ok(copied);
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
        let roots = self.running.allocation_roots.len();
        self.running.allocation_roots.push(destination.clone());
        self.running
            .allocation_roots
            .extend(source.iter().take(copied).cloned());
        let outcome = (|| {
            for (index, value) in source.into_iter().take(copied).enumerate() {
                self.store_index(destination.clone(), Value::int(index as i64), value)?;
            }
            Ok(copied)
        })();
        self.running.allocation_roots.truncate(roots);
        outcome
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::snapshot::HostData;

    #[test]
    fn prepared_slice_copy_preserves_overlap_and_old_to_young_string_edges() {
        let program = test_helpers::program_with_artifact(|_| {});
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        let typ = TypeIdentity::Slice(Arc::new(TypeIdentity::Primitive(wire::PrimitiveString)));
        let destination = vm.make_slice(typ.clone(), 3, 3, Vec::new()).unwrap();
        vm.results = vec![destination.clone()];
        vm.running.transient_roots.clear();
        vm.collect_rooted().unwrap();
        let source = vm
            .make_slice(
                typ,
                3,
                3,
                ["first", "second", "third"]
                    .into_iter()
                    .map(Value::string)
                    .collect(),
            )
            .unwrap();
        vm.heap.flush_worker_lease().unwrap();
        let allocations = vm.heap.stats().allocations;
        assert_eq!(vm.copy_values(destination.clone(), source).unwrap(), 3);
        assert_eq!(vm.heap.stats().allocations, allocations);
        vm.running.transient_roots.clear();
        vm.collect_minor_rooted().unwrap();
        vm.collect_rooted().unwrap();
        assert_eq!(
            vm.slice_values(&destination)
                .unwrap()
                .iter()
                .map(ToString::to_string)
                .collect::<Vec<_>>(),
            ["first", "second", "third"]
        );

        let source = vm.slice_value(destination.clone(), 0, 2, None).unwrap();
        let tail = vm.slice_value(destination.clone(), 1, 3, None).unwrap();
        assert_eq!(vm.copy_values(tail, source).unwrap(), 2);
        assert_eq!(
            vm.slice_values(&destination)
                .unwrap()
                .iter()
                .map(ToString::to_string)
                .collect::<Vec<_>>(),
            ["first", "first", "second"]
        );
        vm.close().unwrap();
    }

    #[test]
    fn map_literal_keeps_string_keys_across_collection() {
        use serde_json::json;
        let program = test_helpers::program_with_artifact(|artifact| {
            let integer = json!({"kind":3,"primitive":3});
            let string = json!({"kind":3,"primitive":2});
            let map = json!({"kind":7,"node":"stringMap"});
            artifact["type_table"] = json!({"nodes":[{
                "id":"stringMap","kind":7,"key":string,"elem":integer
            }]});
            artifact["constants"] = json!([
                {"id":"key","type":string,"value":"same"},
                {"id":"answer","type":integer,"value":42}
            ]);
            artifact["functions"] = json!([{
                "id":"fn.Main","signature":{"results":[integer]},
                "code":test_helpers::micro_code(json!([map,integer]), &[
                    ("make_map",json!({"type":map,"entry_count":1}),json!({"inputs":[[1,0],[1,1]],"outputs":[0]})),
                    ("load_index",json!({}),json!({"inputs":[[0,0],[1,0]],"outputs":[1],"release":[0]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,1]],"release":[1]}))
                ])
            }]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Running);
        vm.collect_garbage().unwrap();
        assert_eq!(vm.poll_steps(4).unwrap(), PollStatus::Ready);
        assert_eq!(vm.guest_results()[0].integer().unwrap(), 42);
        vm.close().unwrap();
    }

    #[test]
    fn snapshot_preserves_managed_map_aliases_and_entries() {
        let program = test_helpers::program_with_artifact(|_| {});
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        let integer = TypeIdentity::Primitive(wire::PrimitiveInt);
        let typ = vm
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
        let map = vm.make_empty_map(typ).unwrap();
        let Data::Map(handle) = map.data else {
            panic!("map reference")
        };
        let key_value = Value::int(7);
        let key = vm.heap.encode_map_lane(&key_value, &integer).unwrap();
        let value = vm.heap.encode_map_lane(&Value::int(42), &integer).unwrap();
        let key_bytes = key.bytes.clone();
        vm.managed_maps
            .get_mut(handle)
            .unwrap()
            .set_lane(
                crate::managed_map::hash_value(&key_value).unwrap(),
                crate::managed_map::MapValueBytes::from_prepared(key),
                crate::managed_map::MapValueBytes::from_prepared(value),
                |candidate| candidate.bytes == key_bytes,
            )
            .unwrap();
        vm.results = vec![
            map.clone(),
            map.clone(),
            Value {
                typ: TypeIdentity::Any,
                data: Data::Interface(Arc::new(map)),
            },
        ];
        let budget_error = vm
            .snapshot_results(crate::snapshot::SnapshotLimits {
                max_bytes: 1,
                ..Default::default()
            })
            .unwrap_err();
        assert_eq!(budget_error.code, "snapshot_limit");
        let snapshot = vm.snapshot_results(Default::default()).unwrap();
        let HostData::Map(first) = snapshot.roots[0].data else {
            panic!("map root must refer to snapshot object")
        };
        let HostData::Map(second) = snapshot.roots[1].data else {
            panic!("alias root must refer to snapshot object")
        };
        let HostData::Interface(inner) = &snapshot.roots[2].data else {
            panic!("interface root")
        };
        let HostData::Map(nested) = inner.data else {
            panic!("interface map reference")
        };
        assert_eq!(first, second);
        assert_eq!(first, nested);
        vm.close().unwrap();
        let HostData::MapEntries(entries) = &snapshot.objects[first].data else {
            panic!("map entries")
        };
        assert_eq!(entries.len(), 1);
        assert!(matches!(entries[0].0.data, HostData::Integer(7)));
        assert!(matches!(entries[0].1.data, HostData::Integer(42)));
    }

    #[test]
    fn snapshot_preserves_channel_order_without_consuming_messages() {
        let program = test_helpers::program_with_artifact(|_| {});
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        let element = TypeIdentity::Primitive(wire::PrimitiveInt);
        let typ = vm
            .types
            .construct(
                crate::types::ConstructedType::Channel {
                    direction: wire::ChannelBoth,
                    element: element.clone(),
                },
                100,
                32,
            )
            .unwrap();
        vm.heap.set_type_registry(vm.types.clone());
        let resource = vm.new_channel_resource(&element, 2).unwrap();
        let handle = vm
            .allocate(Value {
                typ: typ.clone(),
                data: Data::Resource(Box::new(resource.clone())),
            })
            .unwrap();
        vm.register_channel_resource(handle, &resource);
        let channel = Value {
            typ,
            data: Data::ResourceRef(handle),
        };
        assert!(vm.try_send(&channel, Value::int(41)).unwrap());
        assert!(vm.try_send(&channel, Value::int(42)).unwrap());
        assert_eq!(
            vm.try_receive(&channel)
                .unwrap()
                .unwrap()
                .0
                .integer()
                .unwrap(),
            41
        );
        assert!(vm.try_send(&channel, Value::int(43)).unwrap());
        vm.results = vec![channel.clone(), channel.clone()];
        let snapshot = vm.snapshot_results(Default::default()).unwrap();
        let HostData::Resource(first) = snapshot.roots[0].data else {
            panic!("channel snapshot reference")
        };
        let HostData::Resource(second) = snapshot.roots[1].data else {
            panic!("channel alias snapshot reference")
        };
        assert_eq!(first, second);
        for expected in [42, 43] {
            let (message, ok) = vm.try_receive(&channel).unwrap().unwrap();
            assert!(ok);
            assert_eq!(message.integer().unwrap(), expected);
        }
        assert!(vm.try_receive(&channel).unwrap().is_none());
        vm.close().unwrap();
        let HostData::Channel {
            capacity,
            closed,
            queued,
        } = &snapshot.objects[first].data
        else {
            panic!("detached channel snapshot")
        };
        assert_eq!(*capacity, 2);
        assert!(!closed);
        assert_eq!(queued.len(), 2);
        assert!(matches!(queued[0].data, HostData::Integer(42)));
        assert!(matches!(queued[1].data, HostData::Integer(43)));
    }

    #[test]
    fn slice_construction_reuses_admitted_page_capacity() {
        let program = test_helpers::program_with_artifact(|_| {});
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        let typ = TypeIdentity::Slice(Arc::new(TypeIdentity::Primitive(wire::PrimitiveInt)));
        let first = vm
            .make_slice(typ.clone(), 2, 2, vec![Value::int(7)])
            .unwrap();
        let Data::Slice(first) = first.data else {
            panic!("slice header")
        };
        let backing = vm.heap.read_typed_value(first.storage.root).unwrap();
        let resident = vm.heap.stats().resident_page_bytes;
        vm.limits.max_resident_bytes = resident;
        vm.heap.set_resident_limit(resident).unwrap();
        let zero = vm.zero(&backing.typ).unwrap();
        assert_eq!(zero.element(0).unwrap().integer().unwrap(), 0);
        let second = vm.make_slice(typ, 2, 2, vec![Value::int(9)]).unwrap();
        assert_eq!(vm.slice_values(&second).unwrap()[0].integer().unwrap(), 9);
        assert_eq!(vm.heap.stats().resident_page_bytes, resident);
        vm.close().unwrap();
    }

    #[test]
    fn internal_slice_and_channel_capacity_ignores_host_element_boundary() {
        let program = test_helpers::program_with_artifact(|_| {});
        let mut vm = Instance::new(
            program,
            ExecutionLimits {
                max_boundary_elements: 1,
                ..ExecutionLimits::default()
            },
        )
        .unwrap();
        let element = TypeIdentity::Primitive(wire::PrimitiveInt);
        let slice = vm
            .make_slice(
                TypeIdentity::Slice(Arc::new(element.clone())),
                0,
                2,
                Vec::new(),
            )
            .unwrap();
        assert_eq!(vm.container_length(slice, true).unwrap(), 2);

        vm.heap.set_type_registry(vm.types.clone());
        let resource = vm.new_channel_resource(&element, 2).unwrap();
        let scheduler::Resource::Channel { channel } = resource else {
            panic!("expected channel resource")
        };
        assert_eq!(channel.lock().unwrap().capacity(), 2);
        vm.close().unwrap();
    }
}
