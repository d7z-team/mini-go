//! Address resolution, payload snapshots and writes with allocation accounting.

use super::*;

const POINTER_COMPACT_BYTES: u32 = 1;

impl Instance {
    pub(super) fn read_address(&mut self, address: &Address) -> Result<Value, RuntimeError> {
        self.heap.initialize_zero(address.root)?;
        if let Some((offset, type_id, flags)) = self.numeric_location(address)?
            && let Some(value) =
                self.heap
                    .read_prepared_location(address.root, offset, type_id, flags)?
        {
            return Ok(value);
        }
        if address.path.is_empty() {
            return self.load_slot(address.root);
        }
        if let Some(value) = self.read_prepared_scalar_address(address)? {
            return Ok(value);
        }
        self.snapshot_address(address)
    }

    fn read_prepared_scalar_address(
        &self,
        address: &Address,
    ) -> Result<Option<Value>, RuntimeError> {
        let Some(root_type) = self.heap.payload_type(address.root)? else {
            return Ok(None);
        };
        if !self.heap.payload_initialized(address.root)? {
            return Ok(None);
        }
        if self.types.storage_fixed_words(&root_type)?.is_none() {
            return Ok(None);
        }
        let Some(indices) = self.prepared_numeric_path(&root_type, &address.path)? else {
            return Ok(None);
        };
        self.heap
            .read_prepared_path(address.root, &root_type, &indices)
    }

    fn prepared_numeric_path(
        &self,
        root_type: &TypeIdentity,
        path: &[PathElement],
    ) -> Result<Option<Vec<usize>>, RuntimeError> {
        let mut current = root_type.clone();
        let mut indices = Vec::with_capacity(path.len());
        for segment in path {
            let layout_type = self.types.underlying(&current)?;
            match segment {
                PathElement::Field(name) => {
                    let Some(crate::types::StorageTypeLayout::Struct { fields }) =
                        self.types.storage_layout(&layout_type)?
                    else {
                        return Ok(None);
                    };
                    let Some((index, (_, field_type))) = fields
                        .iter()
                        .enumerate()
                        .find(|(_, (field, _))| field == name)
                    else {
                        return Ok(None);
                    };
                    indices.push(index);
                    current = field_type.clone();
                }
                PathElement::Index(index) => match self.types.storage_layout(&layout_type)? {
                    Some(crate::types::StorageTypeLayout::Array { element, length }) => {
                        if *index >= length {
                            return Ok(None);
                        }
                        indices.push(*index);
                        current = element;
                    }
                    Some(crate::types::StorageTypeLayout::Struct { fields }) => {
                        let Some((_, (_, field_type))) = fields
                            .iter()
                            .enumerate()
                            .find(|(field_index, _)| *field_index == *index)
                        else {
                            return Ok(None);
                        };
                        indices.push(*index);
                        current = field_type.clone();
                    }
                    _ => return Ok(None),
                },
                PathElement::ArrayView { .. } | PathElement::TypeView(_) => return Ok(None),
            }
        }
        Ok(Some(indices))
    }

    pub(super) fn snapshot_address(&self, address: &Address) -> Result<Value, RuntimeError> {
        if let Some((offset, type_id, flags)) = self.numeric_location(address)? {
            let prepared =
                self.heap
                    .read_prepared_location(address.root, offset, type_id, flags)?;
            if let Some(value) = prepared {
                return Ok(value);
            }
        }
        let value = self.heap.read_typed_value(address.root)?;
        if address.path.is_empty() && !matches!(value.data, Data::Uninitialized) {
            return Ok(value);
        }
        self.project_address(&value, &address.path)
    }

    fn project_address(
        &self,
        original: &Value,
        path: &[PathElement],
    ) -> Result<Value, RuntimeError> {
        let mut value = if matches!(original.data, Data::Uninitialized) {
            self.zero(&original.typ)?
        } else {
            original.clone()
        };
        if let Data::Bytes(bytes) = &value.data {
            if path.is_empty() {
                return Ok(value);
            }
            let (range, typ, scalar) = crate::value::byte_projection(path, bytes.len(), &value.typ)
                .ok_or_else(|| {
                    RuntimeError::new("invalid_address", "pointer", "byte path does not resolve")
                })?;
            let data = if scalar {
                Data::Unsigned(u64::from(bytes[range.start]))
            } else {
                Data::Array(
                    bytes[range]
                        .iter()
                        .map(|byte| Value {
                            typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                            data: Data::Unsigned(u64::from(*byte)),
                        })
                        .collect::<Vec<_>>()
                        .into(),
                )
            };
            return Ok(Value { typ, data });
        }
        let mut window: Option<(usize, usize, &TypeIdentity)> = None;
        for element in path {
            match element {
                PathElement::TypeView(typ) => {
                    if let Some((start, length, view_type)) = window.take() {
                        value = value.array_range(start, length, view_type)?;
                    }
                    value = self.retype_value(value, typ, 0)?;
                }
                PathElement::ArrayView { start, length, typ } => {
                    let Some((true, count)) = value.data.aggregate_shape() else {
                        return Err(RuntimeError::new(
                            "invalid_address",
                            "pointer",
                            "array view requires backing array",
                        ));
                    };
                    let (offset, available, _) = window.unwrap_or((0, count, &value.typ));
                    if *start > available || *length > available - start {
                        return Err(RuntimeError::new(
                            "invalid_address",
                            "pointer",
                            "array view exceeds backing array",
                        ));
                    }
                    window = Some((offset + start, *length, typ));
                }
                PathElement::Field(name) => value = value.field(name)?,
                PathElement::Index(index) => {
                    let index = if let Some((start, length, _)) = window.take() {
                        if *index >= length {
                            return Err(RuntimeError::new(
                                "invalid_address",
                                "pointer",
                                "index exceeds array view",
                            ));
                        }
                        start + index
                    } else {
                        *index
                    };
                    value = value.element(index)?;
                }
            }
        }
        if let Some((start, length, typ)) = window {
            value = value.array_range(start, length, typ)?;
        }
        Ok(value)
    }

    fn replace_projected_value(
        object: &Value,
        path: &[PathElement],
        value: Value,
    ) -> Result<Value, RuntimeError> {
        let mut window = None;
        for (position, element) in path.iter().enumerate() {
            let index = match element {
                PathElement::TypeView(_) => continue,
                PathElement::ArrayView { start, length, .. } => {
                    let offset = window.map_or(0, |(offset, _)| offset);
                    window = Some((offset + start, *length));
                    continue;
                }
                PathElement::Field(name) => {
                    let count = object.data.aggregate_shape().map_or(0, |(_, count)| count);
                    (0..count)
                        .find(|index| object.data.field_name(*index) == Some(name.as_str()))
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "invalid_address",
                                "pointer",
                                "field does not resolve",
                            )
                        })?
                }
                PathElement::Index(index) => {
                    window.take().map_or(*index, |(offset, _)| offset + index)
                }
            };
            let child = object.element(index)?;
            let replacement = Self::replace_projected_value(&child, &path[position + 1..], value)?;
            return object.with_element(index, replacement);
        }
        if let Some((start, length)) = window {
            if value.data.aggregate_shape() != Some((true, length)) {
                return Err(RuntimeError::new(
                    "type_error",
                    "pointer",
                    "array replacement has a different length",
                ));
            }
            if let (Data::Aggregate(aggregate_object), Data::Aggregate(replacement)) =
                (&object.data, &value.data)
            {
                return Ok(Value {
                    typ: object.typ.clone(),
                    data: Data::Aggregate(aggregate_object.with_array_range(start, replacement)?),
                });
            }
            if let (Data::Array(object_values), Data::Array(replacement_values)) =
                (&object.data, &value.data)
            {
                let mut values = (**object_values).clone();
                values[start..start + length].clone_from_slice(replacement_values);
                return Ok(Value {
                    typ: object.typ.clone(),
                    data: Data::Array(values.into()),
                });
            }
            let mut result = object.clone();
            for index in 0..length {
                result = result.with_element(start + index, value.element(index)?)?;
            }
            Ok(result)
        } else {
            Ok(value)
        }
    }

    pub(super) fn load_slot(&mut self, root: Handle) -> Result<Value, RuntimeError> {
        self.heap.initialize_zero(root)?;
        self.heap.read_typed_value(root)
    }

    pub(super) fn store_slot(&mut self, root: Handle, value: Value) -> Result<(), RuntimeError> {
        self.write_storage(root, &[], value)
    }

    pub(super) fn write_address(
        &mut self,
        address: &Address,
        value: Value,
    ) -> Result<(), RuntimeError> {
        let mut write = mutation::PendingWrite::Address {
            address: address.clone(),
            value,
        };
        let mut collection = mutation::WriteCollection::IMMEDIATE;
        while !self.attempt_write(&mut write, &mut collection)? {}
        Ok(())
    }

    fn write_storage(
        &mut self,
        root: Handle,
        path: &[PathElement],
        value: Value,
    ) -> Result<(), RuntimeError> {
        let roots = self.running.transient_roots.len();
        let mut collection = mutation::WriteCollection::IMMEDIATE;
        while !self.attempt_storage_write(root, path, value.clone(), &mut collection)? {
            self.running.transient_roots.truncate(roots);
        }
        Ok(())
    }

    pub(super) fn attempt_storage_write(
        &mut self,
        root: Handle,
        path: &[PathElement],
        value: Value,
        collection: &mut mutation::WriteCollection,
    ) -> Result<bool, RuntimeError> {
        let (snapshot, stored_bytes, version) = self.heap.read_typed_mutation_base(root)?;
        if path.is_empty()
            && snapshot.typ == value.typ
            && matches!(
                value.data,
                Data::Bool(_)
                    | Data::Integer(_)
                    | Data::Unsigned(_)
                    | Data::Float(_)
                    | Data::Complex { .. }
            )
            && matches!(
                snapshot.data,
                Data::Uninitialized
                    | Data::Bool(_)
                    | Data::Integer(_)
                    | Data::Unsigned(_)
                    | Data::Float(_)
                    | Data::Complex { .. }
            )
        {
            drop(snapshot);
            return self
                .heap
                .update_typed(root, version, stored_bytes, true, |slot| *slot = value);
        }
        if !path.is_empty()
            && !matches!(snapshot.data, Data::Uninitialized)
            && self.types.storage_fixed_words(&snapshot.typ)?.is_some()
            && let Some(indices) = self.prepared_numeric_path(&snapshot.typ, path)?
            && let Some(previous) = self
                .heap
                .read_prepared_path(root, &snapshot.typ, &indices)?
        {
            let value = self.coerce(value.clone(), &previous.typ)?;
            if self
                .heap
                .update_prepared_scalar_path_typed(root, &snapshot.typ, &indices, &value)?
            {
                return Ok(true);
            }
        }
        if path.is_empty() && matches!(snapshot.data, Data::Uninitialized) {
            let value = self.coerce(value.clone(), &snapshot.typ)?;
            let bytes = self.prepared_allocation_bytes(&value)?;
            value.trace(&mut |handle| self.running.transient_roots.push(handle));
            if !self.prepare_write_replacements(&[(root, bytes)], collection)? {
                return Ok(false);
            }
            drop(snapshot);
            return self
                .heap
                .update_typed(root, version, bytes, false, |slot| *slot = value);
        }
        let materialized = if matches!(snapshot.data, Data::Uninitialized) {
            Some(self.zero(&snapshot.typ)?)
        } else {
            None
        };
        let previous = self.project_address(&snapshot, path)?;
        let mut value = self.coerce(value.clone(), &previous.typ)?;
        let storage_path = if path
            .iter()
            .any(|segment| matches!(segment, PathElement::TypeView(_)))
        {
            std::borrow::Cow::Owned(
                path.iter()
                    .filter(|segment| !matches!(segment, PathElement::TypeView(_)))
                    .cloned()
                    .collect::<Vec<_>>(),
            )
        } else {
            std::borrow::Cow::Borrowed(path)
        };
        let previous = if matches!(storage_path, std::borrow::Cow::Owned(_)) {
            let storage = self.project_address(&snapshot, &storage_path)?;
            value = self.retype_value(value, &storage.typ, 0)?;
            storage
        } else {
            previous
        };
        if matches!(snapshot.data, Data::Bytes(_)) {
            let replacement_data = match &value.data {
                Data::Interface(inner) => &inner.data,
                data => data,
            };
            let replacement = match replacement_data {
                Data::Unsigned(byte) if *byte <= u64::from(u8::MAX) => vec![*byte as u8],
                Data::Unsigned(_) => {
                    return Err(RuntimeError::new(
                        "type_error",
                        "pointer",
                        "byte value is outside uint8 range",
                    ));
                }
                // A zero value for an array declared with an unsigned byte
                // type can still arrive as the interpreter's signed integer
                // representation after aggregate construction.  The type
                // coercion above has already checked the destination lane;
                // accept the canonical byte range here while retaining the
                // explicit bounds check for malformed host values.
                Data::Integer(byte) if (0..=i64::from(u8::MAX)).contains(byte) => {
                    vec![*byte as u8]
                }
                Data::Integer(_) => {
                    return Err(RuntimeError::new(
                        "type_error",
                        "pointer",
                        "byte value is outside uint8 range",
                    ));
                }
                Data::Array(values) => values
                    .iter()
                    .map(|value| match value.data {
                        Data::Unsigned(byte) if byte <= u64::from(u8::MAX) => Ok(byte as u8),
                        Data::Unsigned(_) => Err(RuntimeError::new(
                            "type_error",
                            "pointer",
                            "byte value is outside uint8 range",
                        )),
                        Data::Integer(byte) if (0..=i64::from(u8::MAX)).contains(&byte) => {
                            Ok(byte as u8)
                        }
                        Data::Integer(_) => Err(RuntimeError::new(
                            "type_error",
                            "pointer",
                            "byte value is outside uint8 range",
                        )),
                        _ => Err(RuntimeError::new(
                            "type_error",
                            "pointer",
                            "byte array requires byte elements",
                        )),
                    })
                    .collect::<Result<Vec<_>, _>>()?,
                _ => {
                    return Err(RuntimeError::new(
                        "type_error",
                        "pointer",
                        "invalid byte backing replacement",
                    ));
                }
            };
            let mut start = 0;
            for segment in storage_path.iter() {
                match segment {
                    PathElement::Index(index) => start += index,
                    PathElement::ArrayView { start: offset, .. } => start += offset,
                    _ => {}
                }
            }
            let end = start.checked_add(replacement.len()).ok_or_else(|| {
                RuntimeError::new("invalid_address", "pointer", "byte replacement overflows")
            })?;
            if u64::try_from(end).map_or(true, |end| end > stored_bytes) {
                return Err(RuntimeError::new(
                    "invalid_address",
                    "pointer",
                    "byte replacement is outside backing storage",
                ));
            }
            drop(previous);
            drop(snapshot);
            if self
                .heap
                .write_byte_storage_range_if_version(root, start, &replacement, version)?
            {
                return Ok(true);
            }
            return Ok(false);
        }
        let mut old_edges = Vec::new();
        let mut new_edges = Vec::new();
        previous.trace(&mut |handle| old_edges.push(handle));
        value.trace(&mut |handle| new_edges.push(handle));
        let replacement_bytes = self.prepared_allocation_bytes(&value)?;
        let base_bytes = match &materialized {
            Some(value) => self.prepared_allocation_bytes(value)?,
            None => stored_bytes,
        };
        let bytes = base_bytes
            .checked_sub(self.prepared_allocation_bytes(&previous)?)
            .and_then(|bytes| bytes.checked_add(replacement_bytes))
            .ok_or_else(|| {
                RuntimeError::new("allocation_limit", "value", "logical size overflow")
            })?;
        let heap = self.heap.clone();
        let allocations = heap.begin_allocation_scope();
        let replacement = Self::replace_projected_value(
            materialized.as_ref().unwrap_or(&snapshot),
            &storage_path,
            value,
        )?;
        replacement.trace(&mut |handle| self.running.transient_roots.push(handle));
        drop(previous);
        drop(snapshot);
        self.running
            .transient_roots
            .extend(new_edges.iter().copied());
        if !self.prepare_write_replacements(&[(root, bytes)], collection)? {
            return Ok(false);
        }
        if self
            .heap
            .update_typed(root, version, bytes, old_edges == new_edges, |object| {
                *object = replacement;
            })?
        {
            allocations.commit();
            return Ok(true);
        }
        Ok(false)
    }

    /// Extends a prepared numeric pointer through a view path without
    /// materializing its backing aggregate. The base descriptor is relative
    /// to the root storage object; every aggregate segment advances by the
    /// compiler-owned physical lane stride and changes the view type ID.
    pub(super) fn numeric_location(
        &self,
        address: &Address,
    ) -> Result<Option<(u64, u32, u32)>, RuntimeError> {
        let (mut offset, mut type_id, flags) = if let Some(numeric) = address.identity.numeric {
            // A decoded pointer may already carry a base descriptor while a
            // later address operation appends a field/index path.  Continue
            // projecting that path from the descriptor; returning the base
            // location here would make `&mutex.state` read the Mutex object.
            numeric
        } else if address.identity.path_root.is_some()
            || address.identity.original.is_some()
            || !address.path.is_empty()
        {
            // Converted pointers can retain only `original` (a TypeView),
            // while slice-to-array views deliberately carry a fresh identity
            // with just `array_capacity`.  Both still designate the same
            // managed root and can be prepared without reconstructing the
            // host aggregate.  The previous implementation required
            // `path_root`, sending these legitimate views to project_address.
            // After an indirect pointer segment, `address.root` is the
            // pointee's backing object while `path_root` still identifies the
            // slot that held the pointer.  Computing offsets from path_root
            // can therefore apply the pointer slot's layout to the pointee
            // (notably Mutex.state becomes a projection of the parent
            // struct).  The current root is authoritative; retain the
            // original pointee metadata only for descriptor-backed views
            // whose root has no typed payload of its own.
            let root_type = self
                .heap
                .payload_type(address.root)?
                .or_else(|| {
                    address
                        .identity
                        .original
                        .as_ref()
                        .and_then(|origin| origin.pointee.clone())
                })
                .or_else(|| {
                    address
                        .identity
                        .path_root
                        .map(|(root, _)| root)
                        .or_else(|| {
                            address
                                .identity
                                .original
                                .as_ref()
                                .and_then(|origin| origin.root)
                        })
                        .and_then(|root| self.heap.payload_type(root).ok().flatten())
                });
            let Some(mut root_type) = root_type else {
                return Ok(None);
            };
            if let Some(element) = self.types.pointer_element(&root_type)? {
                root_type = element;
            }
            let type_id = self.types.storage_type_id(&root_type)?;
            (0, type_id, 0)
        } else {
            return Ok(None);
        };
        if address.path.is_empty() {
            return Ok(Some((offset, type_id, flags)));
        }
        let mut current = self
            .types
            .identity_for_storage_type_id(type_id)
            .ok_or_else(|| {
                RuntimeError::new("unknown_type", "pointer", "unknown numeric view type")
            })?;
        for segment in &address.path {
            let layout_type = self.types.underlying(&current)?;
            match segment {
                PathElement::Field(name) => {
                    let Some(crate::types::StorageTypeLayout::Struct { fields }) =
                        self.types.storage_layout(&layout_type)?
                    else {
                        return Ok(None);
                    };
                    let Some(index) = fields.iter().position(|(field, _)| field == name) else {
                        return Ok(None);
                    };
                    for (_, field_type) in &fields[..index] {
                        offset = offset
                            .checked_add(u64::from(
                                self.types.storage_lane_layout(field_type)?.stride,
                            ))
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "allocation_limit",
                                    "pointer",
                                    "pointer offset overflows",
                                )
                            })?;
                    }
                    current = fields[index].1.clone();
                }
                PathElement::Index(index) => match self.types.storage_layout(&layout_type)? {
                    Some(crate::types::StorageTypeLayout::Array { element, length }) => {
                        if *index >= length {
                            return Ok(None);
                        }
                        let stride = if flags == POINTER_COMPACT_BYTES {
                            Heap::compact_byte_width(&element, &self.types)? as u64
                        } else {
                            u64::from(self.types.storage_lane_layout(&element)?.stride)
                        };
                        offset = offset
                            .checked_add((*index as u64).checked_mul(stride).ok_or_else(|| {
                                RuntimeError::new(
                                    "allocation_limit",
                                    "pointer",
                                    "pointer offset overflows",
                                )
                            })?)
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "allocation_limit",
                                    "pointer",
                                    "pointer offset overflows",
                                )
                            })?;
                        current = element;
                    }
                    Some(crate::types::StorageTypeLayout::Struct { fields }) => {
                        let Some((_, field_type)) = fields.get(*index) else {
                            return Ok(None);
                        };
                        for (_, preceding) in &fields[..*index] {
                            offset = offset
                                .checked_add(u64::from(
                                    self.types.storage_lane_layout(preceding)?.stride,
                                ))
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "allocation_limit",
                                        "pointer",
                                        "pointer offset overflows",
                                    )
                                })?;
                        }
                        current = field_type.clone();
                    }
                    _ => {
                        return Ok(None);
                    }
                },
                PathElement::ArrayView { start, typ, .. } => {
                    let element = match self.types.storage_layout(&layout_type)? {
                        Some(crate::types::StorageTypeLayout::Array { element, .. }) => element,
                        _ => match layout_type {
                            TypeIdentity::Slice(element) => (*element).clone(),
                            _ => current.clone(),
                        },
                    };
                    let stride = if flags == POINTER_COMPACT_BYTES {
                        Heap::compact_byte_width(&element, &self.types)? as u64
                    } else {
                        u64::from(self.types.storage_lane_layout(&element)?.stride)
                    };
                    offset = offset
                        .checked_add((*start as u64).checked_mul(stride).ok_or_else(|| {
                            RuntimeError::new(
                                "allocation_limit",
                                "pointer",
                                "pointer offset overflows",
                            )
                        })?)
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "allocation_limit",
                                "pointer",
                                "pointer offset overflows",
                            )
                        })?;
                    current = typ.clone();
                }
                PathElement::TypeView(typ) => current = typ.clone(),
            }
            type_id = self.types.storage_type_id(&current)?;
        }
        Ok(Some((offset, type_id, flags)))
    }

    pub(super) fn resolve_address(
        &mut self,
        payload: &wire::AddressPayload,
    ) -> Result<Address, RuntimeError> {
        if payload.kind == "local" {
            let local = {
                let frame = self.running.frames.last().unwrap();
                frame.prepared.locals[&payload.local]
            };
            if matches!(
                self.running.frames.last().unwrap().locals[local],
                frame::Local::Uninitialized
            ) {
                let typ = self.running.frames.last().unwrap().prepared.local_types[local].clone();
                self.store_local(local, self.zero(&typ)?, false)?;
            }
        }
        let frame = self.running.frames.last().unwrap();
        let function = frame.prepared.clone();
        let mut address = match payload.kind.as_str() {
            "local" => Address {
                identity: std::sync::Arc::default(),
                root: frame.locals[function.locals[&payload.local]].address()?,
                path: Vec::new(),
            },
            "upvalue" => frame.upvalues[function.upvalues[&payload.upvalue]].clone(),
            "global" => Address {
                identity: std::sync::Arc::default(),
                root: self.globals[&(frame.module.to_string(), payload.global.clone())],
                path: Vec::new(),
            },
            "export" => {
                let export = self
                    .revision
                    .program
                    .export(&payload.module_path, &payload.export)?;
                let root = self
                    .globals
                    .get(&(payload.module_path.clone(), export.id.clone()))
                    .copied()
                    .ok_or_else(|| {
                        RuntimeError::new("invalid_address", "export", "export is not a global")
                    })?;
                Address {
                    identity: std::sync::Arc::default(),
                    root,
                    path: Vec::new(),
                }
            }
            _ => {
                return Err(RuntimeError::new(
                    "invalid_address",
                    "pointer",
                    "invalid address kind",
                ));
            }
        };
        let path_root = address.root;
        for segment in payload.path.iter() {
            let value = self.read_address(&address)?;
            if matches!(value.data, Data::Nil) && self.types.pointer_element(&value.typ)?.is_some()
            {
                return Err(RuntimeError::new(
                    "panic",
                    "pointer",
                    "nil pointer dereference",
                ));
            }
            if let Data::Pointer(pointer) = value.data {
                address = Arc::unwrap_or_clone(pointer);
            } else if segment.kind == "indirect" {
                return Err(RuntimeError::new(
                    if matches!(value.data, Data::Nil) {
                        "panic"
                    } else {
                        "type_error"
                    },
                    "pointer",
                    "cannot dereference value",
                ));
            }
            match segment.kind.as_str() {
                "field" => address.path.push(PathElement::Field(segment.field.clone())),
                "index" => {
                    let index = self.load_local(function.locals[&segment.local])?.index()?;
                    if let Data::Slice(slice) = self.read_address(&address)?.data {
                        if index >= slice.length {
                            return Err(RuntimeError::new(
                                "panic",
                                "pointer",
                                "index outside slice",
                            ));
                        }
                        address = slice.storage.clone();
                        address.path.push(PathElement::Index(slice.start + index));
                    } else {
                        address.path.push(PathElement::Index(index));
                    }
                }
                "indirect" => {}
                _ => {
                    return Err(RuntimeError::new(
                        "invalid_address",
                        "pointer",
                        "unknown path segment",
                    ));
                }
            }
        }
        if !payload.path.is_empty() {
            let path_root = Some((
                path_root,
                payload.path.len()
                    + payload
                        .path
                        .iter()
                        .filter(|segment| segment.kind == "index")
                        .count(),
            ));
            Arc::make_mut(&mut address.identity).path_root = path_root;
            // A pointer can designate a field of an as-yet unmaterialized
            // aggregate. Numeric reads decline to decode such a payload;
            // materialize its zero value before publishing the field
            // descriptor so the descriptor cannot later decode as the parent.
            self.heap.initialize_zero(address.root)?;
            let numeric = self.numeric_location(&address)?;
            address.reset_identity(path_root);
            // A dynamic field can remain represented by its aggregate parent
            // until its prepared payload is published. Keep the logical path
            // in that case; clearing it would make AddressOf point at the
            // parent and change the pointer's declared type.
            let readable = numeric
                .map(|(offset, type_id, flags)| {
                    self.heap
                        .read_prepared_location(address.root, offset, type_id, flags)
                        .map(|value| value.is_some())
                })
                .transpose()?
                .unwrap_or(false);
            if readable && let Some(numeric) = numeric {
                Arc::make_mut(&mut address.identity).numeric = Some(numeric);
                // The descriptor now includes this path. Further projections
                // append only new segments relative to its pointee type.
                address.path.clear();
            }
        }
        if !address.path.is_empty() {
            self.snapshot_address(&address)?;
        }
        Ok(address)
    }
}
