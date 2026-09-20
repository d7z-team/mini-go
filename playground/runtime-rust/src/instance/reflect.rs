//! Reflection exchanges the ordinary structs/interfaces owned by the current
//! `.mgo` reflect package. Runtime type identities remain structural internally.

use super::*;

impl Instance {
    pub(super) fn publish_dynamic_type(
        &mut self,
        mut types: TypeRegistry,
        typ: &TypeIdentity,
    ) -> Result<(), RuntimeError> {
        types.register_dynamic(
            typ,
            self.limits.max_dynamic_types,
            self.limits.max_dynamic_type_bytes,
            self.limits.max_value_depth,
        )?;
        let bytes = types.dynamic_stats().1;
        let previous_bytes = self.types.dynamic_stats().1;
        if self.heap.set_external_bytes(bytes).is_err() {
            self.collect_rooted()?;
            self.heap.set_external_bytes(bytes)?;
        }
        if let Err(error) = self.charge_guest(bytes.saturating_sub(previous_bytes)) {
            self.heap.set_external_bytes(previous_bytes)?;
            return Err(error);
        }
        self.types = types;
        Ok(())
    }
    pub(super) fn reflect_value_type(
        &mut self,
        value: &Value,
    ) -> Result<TypeIdentity, RuntimeError> {
        let Data::Function(callee) = &value.data else {
            return Ok(value.typ.clone());
        };
        let revision = callee.revision.as_ref().unwrap_or(&self.revision);
        let signature = &revision
            .program
            .function(&callee.module, &callee.function)?
            .declaration
            .signature;
        let params = signature
            .params
            .iter()
            .map(|parameter| {
                revision
                    .program
                    .types()
                    .resolve(&callee.module, &parameter.r#type)
            })
            .collect::<Result<Vec<_>, _>>()?;
        let results = signature
            .results
            .iter()
            .map(|reference| revision.program.types().resolve(&callee.module, reference))
            .collect::<Result<Vec<_>, _>>()?;
        self.types.construct(
            crate::types::ConstructedType::Function {
                params,
                results,
                variadic: signature.variadic,
            },
            self.limits.max_sequence_elements,
            self.limits.max_value_depth,
        )
    }
    pub(super) fn reflect_struct(
        &self,
        name: &str,
        entries: impl IntoIterator<Item = (&'static str, Value)>,
    ) -> Result<Value, RuntimeError> {
        let typ = TypeIdentity::Named(std::sync::Arc::new(wire::TypeKey {
            module_path: "reflect".to_owned(),
            decl_id: name.to_owned(),
        }));
        let mut value = self.zero(&typ, 0)?;
        let Data::Struct(fields) = &mut value.data else {
            return Err(RuntimeError::new(
                "invalid_reflect_schema",
                name,
                "expected runtime struct",
            ));
        };
        for (name, value) in entries {
            let destination = fields.get_mut(name).ok_or_else(|| {
                RuntimeError::new("invalid_reflect_schema", name, "missing runtime field")
            })?;
            *destination = self.coerce(value, &destination.typ)?;
        }
        Ok(value)
    }

    pub(super) fn reflect_type(&mut self, typ: &TypeIdentity) -> Result<Value, RuntimeError> {
        let interface = TypeIdentity::Named(std::sync::Arc::new(wire::TypeKey {
            module_path: "reflect".to_owned(),
            decl_id: "Type".to_owned(),
        }));
        if *typ == TypeIdentity::Void {
            return self.zero(&interface, 0);
        }
        if let Some(index) = self.reflected_type_indices.get(typ) {
            return Ok(self.reflected_types[*index].1.clone());
        }
        for (existing, value) in &self.reflected_types {
            if self.types.identical(typ, existing)? {
                return Ok(value.clone());
            }
        }
        if self.reflected_types.len() >= self.limits.max_sequence_elements {
            return Err(RuntimeError::new(
                "type_limit",
                "reflect",
                "reflected type limit exceeded",
            ));
        }
        let key = self.types.canonical_text(
            typ,
            self.limits.max_value_depth,
            self.limits.max_string_bytes as u64,
        )?;
        let value = self.reflect_struct("runtimeType", [("key", Value::string(key.clone()))])?;
        let value = self.coerce(value, &interface)?;
        self.reflected_type_keys
            .insert(key, self.reflected_types.len());
        self.reflected_type_indices
            .insert(typ.clone(), self.reflected_types.len());
        self.reflected_types.push((typ.clone(), value.clone()));
        Ok(value)
    }

    pub(super) fn reflected_type(&self, value: &Value) -> Result<TypeIdentity, RuntimeError> {
        let value = match &value.data {
            Data::Interface(value) => &**value,
            _ => value,
        };
        let Data::Struct(fields) = &value.data else {
            return Err(RuntimeError::new(
                "reflect",
                "type",
                "reflect: invalid Type",
            ));
        };
        let Some(Value {
            data: Data::String(key),
            ..
        }) = fields.get("key")
        else {
            return Err(RuntimeError::new(
                "reflect",
                "type",
                "reflect: invalid type key",
            ));
        };
        std::str::from_utf8(key)
            .ok()
            .and_then(|key| self.reflected_type_keys.get(key))
            .and_then(|index| self.reflected_types.get(*index))
            .map(|(typ, _)| typ.clone())
            .ok_or_else(|| RuntimeError::new("reflect", "type", "reflect: type is unavailable"))
    }

    fn reflect_layout(
        &self,
        typ: &TypeIdentity,
        depth: usize,
    ) -> Result<(u64, u64, u64), RuntimeError> {
        if depth >= self.limits.max_value_depth {
            return Err(RuntimeError::new(
                "type_limit",
                "reflect",
                "layout depth exceeded",
            ));
        }
        let typ = self.types.underlying(typ)?;
        let layout = match typ {
            TypeIdentity::Void => (1, 0, 0),
            TypeIdentity::Any
            | TypeIdentity::Primitive(wire::PrimitiveString | wire::PrimitiveError) => (8, 16, 0),
            TypeIdentity::Primitive(wire::PrimitiveBool) => (1, 1, 0),
            TypeIdentity::Primitive(wire::PrimitiveInt8 | wire::PrimitiveUint8) => (1, 1, 8),
            TypeIdentity::Primitive(wire::PrimitiveInt16 | wire::PrimitiveUint16) => (2, 2, 16),
            TypeIdentity::Primitive(
                wire::PrimitiveInt32 | wire::PrimitiveUint32 | wire::PrimitiveFloat32,
            ) => (4, 4, 32),
            TypeIdentity::Primitive(wire::PrimitiveComplex64) => (4, 8, 64),
            TypeIdentity::Primitive(wire::PrimitiveComplex128) => (8, 16, 128),
            TypeIdentity::Primitive(
                wire::PrimitiveInt
                | wire::PrimitiveInt64
                | wire::PrimitiveUint
                | wire::PrimitiveUint64
                | wire::PrimitiveUintptr
                | wire::PrimitiveFloat64,
            ) => (8, 8, 64),
            TypeIdentity::Slice(_) => (8, 24, 0),
            TypeIdentity::Structural { .. } => {
                let (module, node) = self.types.node(&typ)?.unwrap();
                match node.kind {
                    wire::Slice => (8, 24, 0),
                    wire::Interface => (8, 16, 0),
                    wire::Array => {
                        let element = self.types.resolve(module, &node.elem)?;
                        let (align, size, bits) = self.reflect_layout(&element, depth + 1)?;
                        let size = size
                            .checked_next_multiple_of(align)
                            .and_then(|stride| stride.checked_mul(node.length as u64))
                            .ok_or_else(|| {
                                RuntimeError::new("type_limit", "reflect", "array layout overflow")
                            })?;
                        (align, size, bits)
                    }
                    wire::Struct => {
                        let mut size = 0u64;
                        let mut alignment = 1;
                        for field in node.fields.iter() {
                            let field_type = self.types.resolve(module, &field.r#type)?;
                            let (align, field_size, _) =
                                self.reflect_layout(&field_type, depth + 1)?;
                            alignment = alignment.max(align);
                            size = size
                                .checked_next_multiple_of(align)
                                .and_then(|size| size.checked_add(field_size))
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "type_limit",
                                        "reflect",
                                        "struct layout overflow",
                                    )
                                })?;
                        }
                        (
                            alignment,
                            size.checked_next_multiple_of(alignment).ok_or_else(|| {
                                RuntimeError::new(
                                    "type_limit",
                                    "reflect",
                                    "struct alignment overflow",
                                )
                            })?,
                            0,
                        )
                    }
                    _ => (8, 8, 0),
                }
            }
            _ => (8, 8, 0),
        };
        Ok(layout)
    }

    fn reflect_display(&self, typ: &TypeIdentity, depth: usize) -> Result<String, RuntimeError> {
        if depth >= self.limits.max_value_depth {
            return Err(RuntimeError::new(
                "type_limit",
                "reflect",
                "display depth exceeded",
            ));
        }
        Ok(match typ {
            TypeIdentity::Void => String::new(),
            TypeIdentity::Any => "any".to_owned(),
            TypeIdentity::Primitive(primitive) => match *primitive {
                wire::PrimitiveBool => "bool",
                wire::PrimitiveString => "string",
                wire::PrimitiveInt => "int",
                wire::PrimitiveInt8 => "int8",
                wire::PrimitiveInt16 => "int16",
                wire::PrimitiveInt32 => "int32",
                wire::PrimitiveInt64 => "int64",
                wire::PrimitiveUint => "uint",
                wire::PrimitiveUint8 => "uint8",
                wire::PrimitiveUint16 => "uint16",
                wire::PrimitiveUint32 => "uint32",
                wire::PrimitiveUint64 => "uint64",
                wire::PrimitiveUintptr => "uintptr",
                wire::PrimitiveFloat32 => "float32",
                wire::PrimitiveFloat64 => "float64",
                wire::PrimitiveComplex64 => "complex64",
                wire::PrimitiveComplex128 => "complex128",
                wire::PrimitiveError => "error",
                wire::PrimitiveFunction => "func()",
                _ => "invalid",
            }
            .to_owned(),
            TypeIdentity::Named(key) => format!(
                "{}.{}",
                self.revision
                    .program
                    .decoded
                    .artifacts()
                    .get(&key.module_path)
                    .map(|artifact| artifact.module.package.as_str())
                    .unwrap_or(&key.module_path),
                key.decl_id
            ),
            TypeIdentity::Pointer(element) => {
                format!("*{}", self.reflect_display(element, depth + 1)?)
            }
            TypeIdentity::Slice(element) => {
                format!("[]{}", self.reflect_display(element, depth + 1)?)
            }
            TypeIdentity::Structural { .. } => {
                let (module, node) = self.types.node(typ)?.unwrap();
                let render = |reference: &wire::TypeRef| {
                    self.reflect_display(&self.types.resolve(module, reference)?, depth + 1)
                };
                let signature_text =
                    |signature: &wire::FunctionSignature| -> Result<String, RuntimeError> {
                        let mut params = signature
                            .params
                            .iter()
                            .map(|parameter| render(&parameter.r#type))
                            .collect::<Result<Vec<_>, _>>()?;
                        if signature.variadic
                            && let Some(last) = params.last_mut()
                        {
                            *last = format!("...{}", last.strip_prefix("[]").unwrap_or(last));
                        }
                        let results = signature
                            .results
                            .iter()
                            .map(render)
                            .collect::<Result<Vec<_>, _>>()?;
                        Ok(format!(
                            "({}){}",
                            params.join(", "),
                            match results.len() {
                                0 => String::new(),
                                1 => format!(" {}", results[0]),
                                _ => format!(" ({})", results.join(", ")),
                            }
                        ))
                    };
                match node.kind {
                    wire::Pointer => format!("*{}", render(&node.elem)?),
                    wire::Slice => format!("[]{}", render(&node.elem)?),
                    wire::Array => format!("[{}]{}", node.length, render(&node.elem)?),
                    wire::Map => format!("map[{}]{}", render(&node.key)?, render(&node.elem)?),
                    wire::Waitable => format!(
                        "{}{}",
                        match node.direction {
                            wire::ChannelReceive => "<-chan ",
                            wire::ChannelSend => "chan<- ",
                            _ => "chan ",
                        },
                        render(&node.elem)?
                    ),
                    wire::Struct => {
                        let mut fields = Vec::new();
                        for field in node.fields.iter() {
                            let typ = render(&field.r#type)?;
                            let mut text = if field.embedded {
                                typ
                            } else {
                                format!("{} {typ}", field.name)
                            };
                            if !field.tag.is_empty() {
                                text.push(' ');
                                text.push_str(&serde_json::to_string(&field.tag)?);
                            }
                            fields.push(text);
                        }
                        if fields.is_empty() {
                            "struct {}".to_owned()
                        } else {
                            format!("struct {{ {} }}", fields.join("; "))
                        }
                    }
                    wire::Interface => {
                        let mut methods = node.methods.iter().collect::<Vec<_>>();
                        methods.sort_by(|left, right| left.name.cmp(&right.name));
                        let methods = methods
                            .into_iter()
                            .map(|method| {
                                Ok(format!(
                                    "{}{}",
                                    method.name,
                                    signature_text(&method.signature)?
                                ))
                            })
                            .collect::<Result<Vec<_>, RuntimeError>>()?;
                        if methods.is_empty() {
                            "interface {}".to_owned()
                        } else {
                            format!("interface {{ {} }}", methods.join("; "))
                        }
                    }
                    wire::Function => {
                        format!("func{}", signature_text(node.signature.as_ref().unwrap())?)
                    }
                    _ => {
                        return Err(RuntimeError::new(
                            "reflect",
                            "type",
                            "unsupported display type",
                        ));
                    }
                }
            }
        })
    }

    fn reflect_descriptor(&mut self, typ: &TypeIdentity) -> Result<Value, RuntimeError> {
        let underlying = self.types.underlying(typ)?;
        let mut elem = TypeIdentity::Void;
        let mut key = TypeIdentity::Void;
        let mut length = 0;
        let mut field_count = 0;
        let mut direction = 0;
        let mut variadic = false;
        let mut inputs = Vec::new();
        let mut outputs = Vec::new();
        let kind = match &underlying {
            TypeIdentity::Void => 0,
            TypeIdentity::Any | TypeIdentity::Primitive(wire::PrimitiveError) => 20,
            TypeIdentity::Primitive(wire::PrimitiveBool) => 1,
            TypeIdentity::Primitive(wire::PrimitiveString) => 24,
            TypeIdentity::Primitive(wire::PrimitiveFunction) => 19,
            TypeIdentity::Primitive(primitive)
                if (wire::PrimitiveInt..=wire::PrimitiveComplex128).contains(primitive) =>
            {
                u64::from(*primitive - 1)
            }
            TypeIdentity::Pointer(element) => {
                elem = (**element).clone();
                22
            }
            TypeIdentity::Slice(element) => {
                elem = (**element).clone();
                23
            }
            TypeIdentity::Structural { .. } => {
                let (module, node) = self.types.node(&underlying)?.unwrap();
                if matches!(
                    node.kind,
                    wire::Array | wire::Slice | wire::Map | wire::Pointer | wire::Waitable
                ) {
                    elem = self.types.resolve(module, &node.elem)?;
                }
                if node.kind == wire::Map {
                    key = self.types.resolve(module, &node.key)?;
                }
                length = node.length;
                field_count = node.fields.len();
                if node.kind == wire::Waitable {
                    direction = match node.direction {
                        wire::ChannelReceive => 1,
                        wire::ChannelSend => 2,
                        _ => 3,
                    };
                }
                if let Some(signature) = &node.signature {
                    variadic = signature.variadic;
                    inputs = signature
                        .params
                        .iter()
                        .map(|parameter| self.types.resolve(module, &parameter.r#type))
                        .collect::<Result<Vec<_>, _>>()?;
                    outputs = signature
                        .results
                        .iter()
                        .map(|reference| self.types.resolve(module, reference))
                        .collect::<Result<Vec<_>, _>>()?;
                }
                match node.kind {
                    wire::Array => 17,
                    wire::Waitable => 18,
                    wire::Function => 19,
                    wire::Interface => 20,
                    wire::Map => 21,
                    wire::Pointer => 22,
                    wire::Slice => 23,
                    wire::Struct => 25,
                    _ => 0,
                }
            }
            _ => 0,
        };
        let (package, name) = match typ {
            TypeIdentity::Named(key) => (key.module_path.clone(), key.decl_id.clone()),
            TypeIdentity::Primitive(_) => (String::new(), self.reflect_display(typ, 0)?),
            _ => (String::new(), String::new()),
        };
        let method_count = self
            .types
            .declared_methods(typ)?
            .iter()
            .filter(|(_, method)| {
                kind == 20 || method.name.chars().next().is_some_and(char::is_uppercase)
            })
            .count();
        let display = self.reflect_display(typ, 0)?;
        let (align, size, bits) = self.reflect_layout(typ, 0)?;
        let elem = self.reflect_type(&elem)?;
        let key = self.reflect_type(&key)?;
        let input_values = inputs
            .iter()
            .map(|typ| self.reflect_type(typ))
            .collect::<Result<Vec<_>, _>>()?;
        let output_values = outputs
            .iter()
            .map(|typ| self.reflect_type(typ))
            .collect::<Result<Vec<_>, _>>()?;
        let reflected_type = TypeIdentity::Named(std::sync::Arc::new(wire::TypeKey {
            module_path: "reflect".to_owned(),
            decl_id: "Type".to_owned(),
        }));
        let inputs = self.make_slice(
            TypeIdentity::Slice(std::sync::Arc::new(reflected_type.clone())),
            input_values.len(),
            input_values.len(),
            input_values,
        )?;
        let outputs = self.make_slice(
            TypeIdentity::Slice(std::sync::Arc::new(reflected_type)),
            output_values.len(),
            output_values.len(),
            output_values,
        )?;
        let own_type = self.reflect_type(typ)?;
        let Data::Interface(own_type) = own_type.data else {
            return Err(RuntimeError::new(
                "reflect",
                "type",
                "invalid descriptor type",
            ));
        };
        let Data::Struct(own_fields) = own_type.data else {
            unreachable!()
        };
        self.reflect_struct(
            "runtimeTypeData",
            [
                ("key", own_fields["key"].clone()),
                ("pkgPath", Value::string(package)),
                ("typeName", Value::string(name)),
                ("displayName", Value::string(display)),
                ("variadic", Value::boolean(variadic)),
                ("align", Value::int(align as i64)),
                ("fieldAlign", Value::int(align as i64)),
                (
                    "size",
                    Value {
                        typ: TypeIdentity::Primitive(wire::PrimitiveUintptr),
                        data: Data::Unsigned(size),
                    },
                ),
                ("bits", Value::int(bits as i64)),
                ("chanDir", Value::int(direction)),
                (
                    "kind",
                    Value {
                        typ: TypeIdentity::Primitive(wire::PrimitiveUint),
                        data: Data::Unsigned(kind),
                    },
                ),
                ("elem", elem),
                ("keyType", key),
                ("length", Value::int(length)),
                ("inputs", inputs),
                ("outputs", outputs),
                ("comparable", Value::boolean(self.types.comparable(typ)?)),
                ("fieldCount", Value::int(field_count as i64)),
                ("methodCount", Value::int(method_count as i64)),
            ],
        )
    }

    pub(super) fn execute_reflect(
        &mut self,
        id: &str,
        mut arguments: Vec<Value>,
    ) -> Result<Vec<Value>, RuntimeError> {
        match id {
            "reflect.array_of" | "reflect.map_of" | "reflect.chan_of" | "reflect.func_of"
            | "reflect.struct_of" => {
                use crate::types::{ConstructedField, ConstructedType};
                let outcome = (|| -> Result<Value, RuntimeError> {
                    let shape = match id {
                        "reflect.array_of" => ConstructedType::Array {
                            length: usize::try_from(arguments[0].integer()?).map_err(|_| {
                                RuntimeError::new("reflect", id, "reflect.ArrayOf: negative length")
                            })?,
                            element: self.reflected_type(&arguments[1])?,
                        },
                        "reflect.map_of" => ConstructedType::Map {
                            key: self.reflected_type(&arguments[0])?,
                            element: self.reflected_type(&arguments[1])?,
                        },
                        "reflect.chan_of" => ConstructedType::Channel {
                            direction: match arguments[0].integer()? {
                                1 => wire::ChannelReceive,
                                2 => wire::ChannelSend,
                                3 => wire::ChannelBoth,
                                _ => {
                                    return Err(RuntimeError::new(
                                        "reflect",
                                        id,
                                        "reflect.ChanOf: invalid direction",
                                    ));
                                }
                            },
                            element: self.reflected_type(&arguments[1])?,
                        },
                        "reflect.func_of" => {
                            let params = self
                                .slice_values(&arguments[0])?
                                .iter()
                                .map(|value| self.reflected_type(value))
                                .collect::<Result<Vec<_>, _>>()?;
                            let results = self
                                .slice_values(&arguments[1])?
                                .iter()
                                .map(|value| self.reflected_type(value))
                                .collect::<Result<Vec<_>, _>>()?;
                            let Data::Bool(variadic) = arguments[2].data else {
                                return Err(RuntimeError::new(
                                    "reflect",
                                    id,
                                    "reflect.FuncOf: invalid variadic flag",
                                ));
                            };
                            ConstructedType::Function {
                                params,
                                results,
                                variadic,
                            }
                        }
                        _ => {
                            let mut fields = Vec::new();
                            for value in self.slice_values(&arguments[0])? {
                                let Data::Struct(values) = value.data else {
                                    return Err(RuntimeError::new(
                                        "reflect",
                                        id,
                                        "reflect.StructOf: invalid field",
                                    ));
                                };
                                let text = |name: &str| -> Result<String, RuntimeError> {
                                    let Some(Value {
                                        data: Data::String(bytes),
                                        ..
                                    }) = values.get(name)
                                    else {
                                        return Err(RuntimeError::new(
                                            "reflect",
                                            id,
                                            "reflect.StructOf: invalid field metadata",
                                        ));
                                    };
                                    String::from_utf8(bytes.to_vec()).map_err(|_| {
                                        RuntimeError::new(
                                            "reflect",
                                            id,
                                            "reflect.StructOf: invalid field encoding",
                                        )
                                    })
                                };
                                if !text("PkgPath")?.is_empty() {
                                    return Err(RuntimeError::new(
                                        "reflect",
                                        id,
                                        "reflect.StructOf: unexported field",
                                    ));
                                }
                                let typ =
                                    self.reflected_type(values.get("Type").ok_or_else(|| {
                                        RuntimeError::new(
                                            "reflect",
                                            id,
                                            "reflect.StructOf: missing field type",
                                        )
                                    })?)?;
                                let Some(Value {
                                    data: Data::Bool(embedded),
                                    ..
                                }) = values.get("Anonymous")
                                else {
                                    return Err(RuntimeError::new(
                                        "reflect",
                                        id,
                                        "reflect.StructOf: invalid field flags",
                                    ));
                                };
                                fields.push(ConstructedField {
                                    name: text("Name")?,
                                    typ,
                                    tag: text("Tag")?,
                                    embedded: *embedded,
                                });
                            }
                            ConstructedType::Struct(fields)
                        }
                    };
                    let mut types = self.types.clone();
                    let typ = types.construct(
                        shape,
                        self.limits.max_sequence_elements,
                        self.limits.max_value_depth,
                    )?;
                    self.publish_dynamic_type(types, &typ)?;
                    self.reflect_type(&typ)
                })();
                match outcome {
                    Ok(value) => Ok(vec![value, Value::string(""), Value::boolean(true)]),
                    Err(error)
                        if matches!(error.code, "reflect" | "invalid_type" | "type_error") =>
                    {
                        Ok(vec![
                            self.reflect_type(&TypeIdentity::Void)?,
                            Value::string(error.message),
                            Value::boolean(false),
                        ])
                    }
                    Err(error) => Err(error),
                }
            }
            "reflect.implements" | "reflect.assignable_to" | "reflect.convertible_to" => {
                let source = self.reflected_type(&arguments[0])?;
                let target = self.reflected_type(&arguments[1])?;
                let answer = if id == "reflect.implements" {
                    self.types.implements(&source, &target)?
                } else {
                    let value = self.zero(&source, 0)?;
                    self.coerce(value.clone(), &target).is_ok()
                        || id == "reflect.convertible_to"
                            && self.convert_value(value, target).is_ok()
                };
                Ok(vec![Value::boolean(answer)])
            }
            "reflect.type_of" => {
                let mut value = arguments.remove(0);
                while let Data::Interface(inner) = value.data {
                    value = *inner;
                }
                let typ = if matches!(value.data, Data::Nil)
                    && (value.typ == TypeIdentity::Any
                        || self
                            .types
                            .node(&value.typ)?
                            .is_some_and(|(_, node)| node.kind == wire::Interface))
                {
                    TypeIdentity::Void
                } else {
                    self.reflect_value_type(&value)?
                };
                Ok(vec![self.reflect_type(&typ)?])
            }
            "reflect.type_descriptor" => {
                let outcome = self
                    .reflected_type(&arguments[0])
                    .and_then(|typ| self.reflect_descriptor(&typ));
                match outcome {
                    Ok(value) => Ok(vec![value, Value::string(""), Value::boolean(true)]),
                    Err(error) => Ok(vec![
                        self.reflect_struct("runtimeTypeData", [])?,
                        Value::string(error.to_string()),
                        Value::boolean(false),
                    ]),
                }
            }
            "reflect.type_field" => {
                let outcome = (|| -> Result<Value, RuntimeError> {
                    let typ = self.reflected_type(&arguments[0])?;
                    let index = usize::try_from(arguments[1].integer()?).map_err(|_| {
                        RuntimeError::new("reflect", id, "reflect: Field index out of bounds")
                    })?;
                    let (module, node) = self
                        .types
                        .node(&typ)?
                        .filter(|(_, node)| node.kind == wire::Struct)
                        .ok_or_else(|| {
                            RuntimeError::new("reflect", id, "reflect: Field of non-struct type")
                        })?;
                    let field = node.fields.get(index).cloned().ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: Field index out of bounds")
                    })?;
                    let field_type = self.types.resolve(module, &field.r#type)?;
                    let mut offset = 0u64;
                    for previous in node.fields.iter().take(index) {
                        let previous_type = self.types.resolve(module, &previous.r#type)?;
                        let (align, size, _) = self.reflect_layout(&previous_type, 0)?;
                        offset = offset
                            .checked_next_multiple_of(align)
                            .and_then(|offset| offset.checked_add(size))
                            .ok_or_else(|| {
                                RuntimeError::new("type_limit", id, "field offset overflow")
                            })?;
                    }
                    let (align, _, _) = self.reflect_layout(&field_type, 0)?;
                    offset = offset.checked_next_multiple_of(align).ok_or_else(|| {
                        RuntimeError::new("type_limit", id, "field alignment overflow")
                    })?;
                    let package = if field.name.chars().next().is_some_and(char::is_uppercase) {
                        String::new()
                    } else {
                        module.to_owned()
                    };
                    let field_type = self.reflect_type(&field_type)?;
                    let path = self.make_slice(
                        TypeIdentity::Slice(std::sync::Arc::new(TypeIdentity::Primitive(
                            wire::PrimitiveInt,
                        ))),
                        1,
                        1,
                        vec![Value::int(index as i64)],
                    )?;
                    self.reflect_struct(
                        "StructField",
                        [
                            ("Name", Value::string(field.name)),
                            ("PkgPath", Value::string(package)),
                            ("Type", field_type),
                            ("Tag", Value::string(field.tag)),
                            (
                                "Offset",
                                Value {
                                    typ: TypeIdentity::Primitive(wire::PrimitiveUintptr),
                                    data: Data::Unsigned(offset),
                                },
                            ),
                            ("Index", path),
                            ("Anonymous", Value::boolean(field.embedded)),
                        ],
                    )
                })();
                match outcome {
                    Ok(value) => Ok(vec![value, Value::string(""), Value::boolean(true)]),
                    Err(error) if error.code == "reflect" => Ok(vec![
                        self.reflect_struct("StructField", [])?,
                        Value::string(error.message),
                        Value::boolean(false),
                    ]),
                    Err(error) => Err(error),
                }
            }
            "reflect.type_method" | "reflect.value_method" => {
                self.execute_reflect_method(id, arguments)
            }
            _ => self.execute_reflect_value(id, arguments),
        }
    }
}
