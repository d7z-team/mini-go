//! Guest zero values, assignment and conversions shared by bytecode and reflection.

use super::*;

impl Instance {
    pub(super) fn coerce(
        &self,
        mut value: Value,
        typ: &TypeIdentity,
    ) -> Result<Value, RuntimeError> {
        if let Data::String(bytes) = &value.data {
            self.check_string_size(bytes.len())?;
        }
        let registry = &self.types;
        if registry.identical(&value.typ, typ)? {
            value.typ = typ.clone();
            return Ok(value);
        }
        let interface = registry.is_interface(typ)?;
        if interface {
            if let Data::Interface(dynamic) = value.data {
                value = Arc::unwrap_or_clone(dynamic);
            }
            if matches!(value.data, Data::Nil) && registry.is_interface(&value.typ)? {
                return Ok(Value {
                    typ: typ.clone(),
                    data: Data::Nil,
                });
            }
            let assignment = (value.typ.clone(), typ.clone());
            let known = self
                .interface_assignments
                .lock()
                .unwrap()
                .contains(&assignment);
            if !known {
                if !registry.implements(&value.typ, typ)? {
                    return Err(RuntimeError::new(
                        "type_error",
                        "assignment",
                        "dynamic type does not implement interface",
                    ));
                }
                let mut assignments = self.interface_assignments.lock().unwrap();
                if assignments.len() == 256 {
                    assignments.clear();
                }
                assignments.insert(assignment);
            }
            return Ok(Value {
                typ: typ.clone(),
                data: Data::Interface(std::sync::Arc::new(value)),
            });
        }
        if registry.channel_assignable(&value.typ, typ)? {
            value.typ = typ.clone();
            return Ok(value);
        }
        if value.typ == TypeIdentity::Any
            && matches!(value.data, Data::Nil)
            && registry.nil_assignable(typ)?
        {
            return self.zero(typ, 0);
        }
        if let Data::Function(callee) = &value.data {
            let revision = callee.revision.as_ref().unwrap_or(&self.revision);
            let function = revision
                .program
                .function(&callee.module, &callee.function)?;
            if let Some((module, node)) = registry.node(typ)?
                && let Some(signature) = &node.signature
                && registry.signature_identical_from(
                    registry,
                    module,
                    signature,
                    revision.program.decoded.types(),
                    &callee.module,
                    &function.declaration.signature,
                )?
            {
                value.typ = typ.clone();
                return Ok(value);
            }
        }
        let source = registry.underlying(&value.typ)?;
        let destination = registry.underlying(typ)?;
        if (!matches!(value.typ, TypeIdentity::Named(_)) || !matches!(typ, TypeIdentity::Named(_)))
            && registry.identical(&source, &destination)?
        {
            value.typ = typ.clone();
            return Ok(value);
        }
        Err(RuntimeError::new(
            "type_error",
            "assignment",
            format!("cannot assign {:?} to {:?}", value.typ, typ),
        ))
    }

    pub(super) fn zero(&self, typ: &TypeIdentity, depth: usize) -> Result<Value, RuntimeError> {
        let mut remaining = self.limits.max_heap_bytes;
        Value::zero_with_budget(
            &self.types,
            typ,
            depth,
            self.limits.max_value_depth,
            self.limits.max_sequence_elements,
            &mut remaining,
        )
    }

    pub(super) fn retype_value(
        &self,
        mut value: Value,
        typ: &TypeIdentity,
        depth: usize,
    ) -> Result<Value, RuntimeError> {
        if self.types.identical(&value.typ, typ)? {
            value.typ = typ.clone();
            return Ok(value);
        }
        if depth >= self.limits.max_value_depth {
            return Err(RuntimeError::new(
                "value_limit",
                "convert",
                "typed view exceeds value depth",
            ));
        }
        match &mut value.data {
            Data::Struct(fields) => {
                let (module, node) = self
                    .types
                    .node(typ)?
                    .filter(|(_, node)| node.kind == wire::Struct)
                    .ok_or_else(|| {
                        RuntimeError::new(
                            "type_error",
                            "convert",
                            "typed view requires matching struct",
                        )
                    })?;
                for field in node.fields.iter() {
                    let destination = fields.get_mut(&field.name).ok_or_else(|| {
                        RuntimeError::new("type_error", "convert", "typed view field is missing")
                    })?;
                    let field_type = self.types.resolve(module, &field.r#type)?;
                    *destination =
                        self.retype_value(destination.clone(), &field_type, depth + 1)?;
                }
            }
            Data::Array(values) => {
                let element = self.element_type(typ)?;
                for value in Arc::make_mut(values) {
                    *value = self.retype_value(value.clone(), &element, depth + 1)?;
                }
            }
            Data::Pointer(address) => {
                let address = Arc::make_mut(address);
                let element = match self.types.underlying(typ)? {
                    TypeIdentity::Pointer(element) => (*element).clone(),
                    _ => self.element_type(typ)?,
                };
                let original = match &address.identity.original {
                    Some(original) => original.clone(),
                    None => {
                        let mut original = address.pointer_origin();
                        original.pointee = Some(match self.types.underlying(&value.typ)? {
                            TypeIdentity::Pointer(element) => (*element).clone(),
                            _ => self.element_type(&value.typ)?,
                        });
                        original
                    }
                };
                while matches!(address.path.last(), Some(PathElement::TypeView(_))) {
                    address.path.pop();
                }
                if self
                    .types
                    .identical(original.pointee.as_ref().unwrap(), &element)?
                {
                    address.identity = Arc::new(crate::value::PointerIdentity {
                        key: std::sync::OnceLock::from(original.key),
                        path_root: original.root.map(|root| (root, original.slots)),
                        array_capacity: original.array.map(|(_, _, capacity)| capacity),
                        ..Default::default()
                    });
                } else {
                    address.identity = Arc::new(crate::value::PointerIdentity {
                        original: Some(original),
                        ..Default::default()
                    });
                    address.path.push(PathElement::TypeView(element));
                }
            }
            _ => {}
        }
        value.typ = typ.clone();
        Ok(value)
    }

    pub(super) fn convert_value(
        &mut self,
        value: Value,
        typ: TypeIdentity,
    ) -> Result<Value, RuntimeError> {
        let pointee = self.types.pointer_element(&typ)?;
        if let Some(element) = &pointee
            && !matches!(typ, TypeIdentity::Named(_))
            && !matches!(value.typ, TypeIdentity::Named(_))
            && (matches!(value.data, Data::Pointer(_)) || matches!(value.data, Data::Nil))
        {
            let source_element = self.types.pointer_element(&value.typ)?;
            if let Some(source_element) = source_element
                && self
                    .types
                    .conversion_underlying_identical(&source_element, element)?
            {
                return self.retype_value(value, &typ, 0);
            }
        }
        if matches!(value.data, Data::Struct(_) | Data::Array(_))
            && self
                .types
                .conversion_underlying_identical(&value.typ, &typ)?
        {
            return self.retype_value(value, &typ, 0);
        }
        if let Some(element) = pointee
            && let Some((_, node)) = self
                .types
                .node(&element)?
                .filter(|(_, node)| node.kind == wire::Array)
            && (matches!(value.data, Data::Slice(_)) || matches!(value.data, Data::Nil))
        {
            let length = node.length as usize;
            if !self.types.identical(
                &self.element_type(&value.typ)?,
                &self.element_type(&element)?,
            )? {
                return Err(RuntimeError::new(
                    "type_error",
                    "convert",
                    "slice and array element types differ",
                ));
            }
            let data = match value.data {
                Data::Slice(slice) if slice.length >= length => {
                    let capacity = match &self.snapshot_address(&slice.storage)?.data {
                        Data::Bytes(_) => length,
                        Data::Array(values) => values.len() - slice.start,
                        _ => unreachable!("validated slice backing"),
                    };
                    let mut address = slice.storage.clone();
                    address.identity = Arc::new(crate::value::PointerIdentity {
                        array_capacity: Some(capacity),
                        ..Default::default()
                    });
                    address.path.push(PathElement::ArrayView {
                        start: slice.start,
                        length,
                        typ: element,
                    });
                    Data::Pointer(std::sync::Arc::new(address))
                }
                Data::Nil if length == 0 => Data::Nil,
                _ => {
                    return Err(RuntimeError::new(
                        "type_error",
                        "convert",
                        "slice is shorter than target array",
                    ));
                }
            };
            return Ok(Value { typ, data });
        }
        Ok(
            if self.types.is_interface(&typ)? || matches!(value.data, Data::Function(_)) {
                self.coerce(value, &typ)?
            } else if self
                .types
                .node(&typ)?
                .is_some_and(|(_, node)| node.kind == wire::Array)
                && (matches!(value.data, Data::Slice(_)) || matches!(value.data, Data::Nil))
            {
                let (_, node) = self.types.node(&typ)?.unwrap();
                let length = node.length as usize;
                if !self
                    .types
                    .identical(&self.element_type(&value.typ)?, &self.element_type(&typ)?)?
                {
                    return Err(RuntimeError::new(
                        "type_error",
                        "convert",
                        "slice and array element types differ",
                    ));
                }
                let mut elements = self.slice_values(&value)?;
                if elements.len() < length {
                    return Err(RuntimeError::new(
                        "type_error",
                        "convert",
                        "slice is shorter than target array",
                    ));
                }
                elements.truncate(length);
                Value {
                    typ,
                    data: Data::Array(elements.into()),
                }
            } else if matches!(value.data, Data::String(_))
                && (matches!(self.types.underlying(&typ)?, TypeIdentity::Slice(_))
                    || self
                        .types
                        .node(&typ)?
                        .is_some_and(|(_, node)| node.kind == wire::Slice))
            {
                let element = self.element_type(&typ)?;
                let Data::String(bytes) = value.data else {
                    unreachable!()
                };
                let mut elements = Vec::new();
                if element == TypeIdentity::Primitive(wire::PrimitiveUint8) {
                    return self.make_bytes(typ, bytes.len(), bytes.len(), &bytes);
                } else if element == TypeIdentity::Primitive(wire::PrimitiveInt32) {
                    let mut index = 0;
                    while index < bytes.len() {
                        let (rune, width) = crate::value::decode_rune(&bytes[index..]);
                        index += width;
                        elements.push(Value {
                            typ: element.clone(),
                            data: Data::Integer(i64::from(rune)),
                        });
                    }
                } else {
                    return Err(RuntimeError::new(
                        "type_error",
                        "convert",
                        "string conversion requires byte or rune slice",
                    ));
                }
                self.make_slice(typ, elements.len(), elements.len(), elements)?
            } else if self.types.underlying(&typ)? == TypeIdentity::Primitive(wire::PrimitiveString)
                && (matches!(value.data, Data::Slice(_)) || matches!(value.data, Data::Nil))
            {
                if self.types.underlying(&self.element_type(&value.typ)?)?
                    == TypeIdentity::Primitive(wire::PrimitiveUint8)
                {
                    if let Data::Slice(slice) = &value.data {
                        self.check_string_size(slice.length)?;
                    }
                    let bytes = self.slice_bytes(&value)?;
                    self.charge_guest(bytes.len() as u64)?;
                    return Ok(Value {
                        typ,
                        data: Data::String(bytes.into()),
                    });
                }
                let mut bytes = Vec::new();
                for value in self.slice_values(&value)? {
                    match value.data {
                        Data::Unsigned(byte) if byte <= 255 => bytes.push(byte as u8),
                        Data::Integer(rune) => {
                            let rune = u32::try_from(rune)
                                .ok()
                                .and_then(char::from_u32)
                                .unwrap_or(char::REPLACEMENT_CHARACTER);
                            let mut encoded = [0; 4];
                            let encoded = rune.encode_utf8(&mut encoded).as_bytes();
                            self.check_string_size(bytes.len().saturating_add(encoded.len()))?;
                            bytes.extend_from_slice(encoded);
                        }
                        _ => {
                            return Err(RuntimeError::new(
                                "type_error",
                                "convert",
                                "expected bytes or runes",
                            ));
                        }
                    }
                }
                self.charge_guest(bytes.len() as u64)?;
                Value {
                    typ,
                    data: Data::String(bytes.into()),
                }
            } else {
                crate::operators::convert(value, typ, &self.types)?
            },
        )
    }
}
