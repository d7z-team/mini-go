//! Address resolution, payload snapshots and writes with allocation accounting.

use super::*;

impl Instance {
    pub(super) fn read_address(&mut self, address: &Address) -> Result<Value, RuntimeError> {
        if address.path.is_empty() {
            return self.load_slot(address.root);
        }
        if matches!(self.heap.get(address.root)?.data, Data::Uninitialized) {
            self.load_slot(address.root)?;
        }
        Ok(self.snapshot_address(address)?.into_owned())
    }

    pub(super) fn snapshot_address(
        &self,
        address: &Address,
    ) -> Result<crate::value::ValueRead<'static>, RuntimeError> {
        let value = self.heap.get(address.root)?;
        if address.path.is_empty() && !matches!(value.data, Data::Uninitialized) {
            return Ok(crate::value::ValueRead::Shared(value));
        }
        Ok(crate::value::ValueRead::Owned(
            self.project_address(&value, &address.path)?.into_owned(),
        ))
    }

    fn project_address<'a>(
        &self,
        mut value: &'a Value,
        path: &[PathElement],
    ) -> Result<std::borrow::Cow<'a, Value>, RuntimeError> {
        if matches!(value.data, Data::Uninitialized) {
            let zero = self.zero(&value.typ, 0)?;
            return Ok(std::borrow::Cow::Owned(
                self.project_address(&zero, path)?.into_owned(),
            ));
        }
        if let Data::Bytes(bytes) = &value.data {
            if path.is_empty() {
                return Ok(std::borrow::Cow::Borrowed(value));
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
                        .collect(),
                )
            };
            return Ok(std::borrow::Cow::Owned(Value { typ, data }));
        }
        let mut window: Option<(usize, usize, &TypeIdentity)> = None;
        for (index, element) in path.iter().enumerate() {
            if let PathElement::TypeView(typ) = element {
                let value = if let Some((start, length, typ)) = window {
                    let Data::Array(values) = &value.data else {
                        unreachable!()
                    };
                    Value {
                        typ: typ.clone(),
                        data: Data::Array(values[start..start + length].to_vec()),
                    }
                } else {
                    value.clone()
                };
                let value = self.retype_value(value, typ, 0)?;
                return Ok(std::borrow::Cow::Owned(
                    self.project_address(&value, &path[index + 1..])?
                        .into_owned(),
                ));
            }
            if let PathElement::ArrayView { start, length, typ } = element {
                let Data::Array(values) = &value.data else {
                    return Err(RuntimeError::new(
                        "invalid_address",
                        "pointer",
                        "array view requires backing array",
                    ));
                };
                let (offset, available, _) = window.unwrap_or((0, values.len(), &value.typ));
                if *start > available || *length > available - start {
                    return Err(RuntimeError::new(
                        "invalid_address",
                        "pointer",
                        "array view exceeds backing array",
                    ));
                }
                window = Some((offset + start, *length, typ));
                continue;
            }
            value = match (element, &value.data) {
                (PathElement::Field(name), Data::Struct(fields)) => fields.get(name),
                (PathElement::Index(index), Data::Array(values)) => {
                    let (start, length, _) = window.take().unwrap_or((0, values.len(), &value.typ));
                    if *index < length {
                        values.get(start + index)
                    } else {
                        None
                    }
                }
                _ => None,
            }
            .ok_or_else(|| {
                RuntimeError::new("invalid_address", "pointer", "path does not resolve")
            })?;
        }
        if let Some((start, length, typ)) = window {
            let Data::Array(values) = &value.data else {
                unreachable!()
            };
            Ok(std::borrow::Cow::Owned(Value {
                typ: typ.clone(),
                data: Data::Array(values[start..start + length].to_vec()),
            }))
        } else {
            Ok(std::borrow::Cow::Borrowed(value))
        }
    }

    pub(super) fn load_slot(&mut self, root: Handle) -> Result<Value, RuntimeError> {
        loop {
            let snapshot = self.heap.get(root)?;
            if !matches!(snapshot.data, Data::Uninitialized) {
                return Ok(Arc::unwrap_or_clone(snapshot));
            }
            let value = self.zero(&snapshot.typ, 0)?;
            let bytes = value.logical_bytes()?.checked_add(128).ok_or_else(|| {
                RuntimeError::new("allocation_limit", "slot", "slot size overflow")
            })?;
            value.trace(&mut |handle| self.running.transient_roots.push(handle));
            self.prepare_heap_replacements(&[(root, bytes)])?;
            if let Some(mutation) = self.heap.prepare_mutation(root, &snapshot, value, bytes)? {
                drop(snapshot);
                mutation.commit();
            }
        }
    }

    pub(super) fn store_slot(&mut self, root: Handle, value: Value) -> Result<(), RuntimeError> {
        self.write_storage(root, &[], value)
    }

    pub(super) fn write_address(
        &mut self,
        address: &Address,
        value: Value,
    ) -> Result<(), RuntimeError> {
        if address.path.is_empty() {
            self.store_slot(address.root, value)
        } else {
            self.write_storage(address.root, &address.path, value)
        }
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
        let (snapshot, stored_bytes) = self.heap.read_mutation_base(root)?;
        let expected = Arc::downgrade(&snapshot);
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
                .update(root, &expected, stored_bytes, true, |slot| *slot = value);
        }
        if path.is_empty() && matches!(snapshot.data, Data::Uninitialized) {
            let value = self.coerce(value.clone(), &snapshot.typ)?;
            let bytes = value.logical_bytes()?.checked_add(128).ok_or_else(|| {
                RuntimeError::new("allocation_limit", "slot", "slot size overflow")
            })?;
            value.trace(&mut |handle| self.running.transient_roots.push(handle));
            if !self.prepare_write_replacements(&[(root, bytes)], collection)? {
                return Ok(false);
            }
            if let Some(mutation) = self.heap.prepare_mutation(root, &snapshot, value, bytes)? {
                drop(snapshot);
                if mutation.commit() {
                    return Ok(true);
                }
            }
            return Ok(false);
        }
        let materialized = if matches!(snapshot.data, Data::Uninitialized) {
            Some(self.zero(&snapshot.typ, 0)?)
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
            let replacement = match &value.data {
                Data::Unsigned(byte) => vec![*byte as u8],
                Data::Array(values) => values
                    .iter()
                    .map(|value| match value.data {
                        Data::Unsigned(byte) => Ok(byte as u8),
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
            drop(previous);
            drop(snapshot);
            if self
                .heap
                .update(root, &expected, stored_bytes, true, |object| {
                    let Data::Bytes(bytes) = &mut object.data else {
                        unreachable!()
                    };
                    bytes[start..start + replacement.len()].copy_from_slice(&replacement);
                })?
            {
                return Ok(true);
            }
            return Ok(false);
        }
        let mut old_edges = Vec::new();
        let mut new_edges = Vec::new();
        previous.trace(&mut |handle| old_edges.push(handle));
        value.trace(&mut |handle| new_edges.push(handle));
        let replacement_bytes = value.logical_bytes()?;
        let base_bytes = match &materialized {
            Some(value) => value.logical_bytes()?.saturating_add(128),
            None => stored_bytes,
        };
        let bytes = base_bytes
            .checked_sub(previous.logical_bytes()?)
            .and_then(|bytes| bytes.checked_add(replacement_bytes))
            .ok_or_else(|| {
                RuntimeError::new("allocation_limit", "value", "logical size overflow")
            })?;
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
            .update(root, &expected, bytes, old_edges == new_edges, |object| {
                if let Some(value) = materialized {
                    *object = value;
                }
                let mut destination = object;
                let mut window = None;
                for element in storage_path.iter() {
                    if let PathElement::ArrayView { start, length, .. } = element {
                        let offset = window.map_or(0, |(offset, _)| offset);
                        window = Some((offset + start, *length));
                        continue;
                    }
                    destination = match (element, &mut destination.data) {
                        (PathElement::Field(name), Data::Struct(fields)) => {
                            fields.get_mut(name).unwrap()
                        }
                        (PathElement::Index(index), Data::Array(values)) => {
                            let offset = window.take().map_or(0, |(offset, _)| offset);
                            values.get_mut(offset + index).unwrap()
                        }
                        _ => unreachable!("path was validated before mutation"),
                    };
                }
                if let Some((start, length)) = window {
                    let (Data::Array(destination), Data::Array(values)) =
                        (&mut destination.data, value.data)
                    else {
                        unreachable!()
                    };
                    destination[start..start + length].clone_from_slice(&values);
                } else {
                    *destination = value;
                }
            })?
        {
            return Ok(true);
        }
        Ok(false)
    }

    pub(super) fn resolve_address(
        &mut self,
        payload: &wire::AddressPayload,
    ) -> Result<Address, RuntimeError> {
        let frame = self.running.frames.last().unwrap();
        let function = &frame.prepared;
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
        let index_slots: Vec<_> = payload
            .path
            .iter()
            .map(|segment| (segment.kind == "index").then(|| function.locals[&segment.local]))
            .collect();
        for (segment, index_slot) in payload.path.iter().zip(index_slots) {
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
                address = pointer;
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
                    let index = self.load_local(index_slot.unwrap())?.integer()?;
                    let index = usize::try_from(index)
                        .map_err(|_| RuntimeError::new("panic", "pointer", "negative index"))?;
                    if let Data::Slice(slice) = self.read_address(&address)?.data {
                        if index >= slice.length {
                            return Err(RuntimeError::new(
                                "panic",
                                "pointer",
                                "index outside slice",
                            ));
                        }
                        address = slice.storage;
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
        if !address.path.is_empty() {
            self.snapshot_address(&address)?;
        }
        if !payload.path.is_empty() {
            address.identity = Arc::new(crate::value::PointerIdentity {
                path_root: Some((
                    path_root,
                    payload.path.len()
                        + payload
                            .path
                            .iter()
                            .filter(|segment| segment.kind == "index")
                            .count(),
                )),
                ..Default::default()
            });
        }
        Ok(address)
    }
}
