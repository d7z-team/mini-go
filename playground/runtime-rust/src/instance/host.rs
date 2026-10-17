//! Typed host arguments are copied and validated before publishing an invocation.

use super::*;
use crate::snapshot::{HostData, HostValue};

impl Instance {
    /// Imports host-owned value trees. Slice inputs use Array or compact byte
    /// String data; map inputs use MapEntries. Live resource handles are not inputs.
    pub fn start(&mut self, entry: &str, arguments: &[HostValue]) -> Result<(), RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new(
                "closed",
                "argument",
                "instance is closed",
            ));
        }
        if self.faulted {
            return Err(RuntimeError::new(
                "faulted",
                "argument",
                "instance execution failed",
            ));
        }
        if self.foreground.is_some() {
            return Err(RuntimeError::new("busy", "argument", "execution is active"));
        }
        if arguments.len() > self.limits.max_boundary_elements {
            return Err(RuntimeError::new(
                "boundary_limit",
                "argument",
                "argument count exceeds limit",
            ));
        }
        self.types
            .use_context(self.revision.program.decoded.types());
        let mut remaining = self.limits.max_boundary_bytes as u64;
        let result = (|| {
            let values = arguments
                .iter()
                .map(|value| self.import_host_value(value, 0, &mut remaining))
                .collect::<Result<Vec<_>, _>>()?;
            self.start_values(entry, values)
        })();
        if result.is_err() {
            self.collect_at_boundary()?;
        }
        result
    }

    fn import_host_value(
        &mut self,
        input: &HostValue,
        depth: usize,
        remaining: &mut u64,
    ) -> Result<Value, RuntimeError> {
        let invalid = || {
            RuntimeError::new(
                "host_type",
                "argument",
                "host data does not match its declared type",
            )
        };
        if depth >= self.limits.max_boundary_depth {
            return Err(RuntimeError::new(
                "boundary_limit",
                "argument",
                "host value depth exceeds limit",
            ));
        }
        let underlying = self.types.underlying(&input.typ)?;
        *remaining = remaining.checked_sub(16).ok_or_else(|| {
            RuntimeError::new(
                "boundary_limit",
                "argument",
                "host value size exceeds limit",
            )
        })?;
        let primitive = match underlying {
            TypeIdentity::Primitive(primitive) => Some(primitive),
            _ => None,
        };
        let value = match &input.data {
            HostData::Nil if self.types.nil_assignable(&input.typ)? => {
                return self.zero(&input.typ);
            }
            HostData::Bool(value) if primitive == Some(wire::PrimitiveBool) => {
                Value::boolean(*value)
            }
            HostData::Integer(value)
                if primitive.is_some_and(|primitive| {
                    (wire::PrimitiveInt..=wire::PrimitiveInt64).contains(&primitive)
                }) =>
            {
                Value::int(*value)
            }
            HostData::Unsigned(value)
                if primitive.is_some_and(|primitive| {
                    (wire::PrimitiveUint..=wire::PrimitiveUintptr).contains(&primitive)
                }) =>
            {
                Value {
                    typ: TypeIdentity::Primitive(wire::PrimitiveUint64),
                    data: Data::Unsigned(*value),
                }
            }
            HostData::FloatBits(value)
                if matches!(
                    primitive,
                    Some(wire::PrimitiveFloat32 | wire::PrimitiveFloat64)
                ) =>
            {
                Value {
                    typ: TypeIdentity::Primitive(wire::PrimitiveFloat64),
                    data: Data::Float(f64::from_bits(*value)),
                }
            }
            HostData::ComplexBits { real, imag }
                if matches!(
                    primitive,
                    Some(wire::PrimitiveComplex64 | wire::PrimitiveComplex128)
                ) =>
            {
                Value {
                    typ: TypeIdentity::Primitive(wire::PrimitiveComplex128),
                    data: Data::Complex {
                        real: f64::from_bits(*real),
                        imag: f64::from_bits(*imag),
                    },
                }
            }
            HostData::String(bytes) => {
                let slice = matches!(underlying, TypeIdentity::Slice(_))
                    || self
                        .types
                        .node(&input.typ)?
                        .is_some_and(|(_, node)| node.kind == wire::Slice);
                if slice {
                    if !self.types.identical(
                        &self.element_type(&input.typ)?,
                        &TypeIdentity::Primitive(wire::PrimitiveUint8),
                    )? {
                        return Err(invalid());
                    }
                    *remaining = remaining.checked_sub(bytes.len() as u64).ok_or_else(|| {
                        RuntimeError::new(
                            "boundary_limit",
                            "argument",
                            "host byte slice exceeds limit",
                        )
                    })?;
                    return self.make_bytes(input.typ.clone(), bytes.len(), bytes.len(), bytes);
                }
                *remaining = remaining.checked_sub(bytes.len() as u64).ok_or_else(|| {
                    RuntimeError::new("boundary_limit", "argument", "host string exceeds limit")
                })?;
                self.check_string_size(bytes.len())?;
                Value::string(bytes.clone())
            }
            HostData::Interface(inner) if self.types.is_interface(&input.typ)? => {
                let value = self.import_host_value(inner, depth + 1, remaining)?;
                return self.coerce(value, &input.typ);
            }
            HostData::Array(values) => {
                if values.len() > self.limits.max_boundary_elements {
                    return Err(RuntimeError::new(
                        "boundary_limit",
                        "argument",
                        "host sequence exceeds limit",
                    ));
                }
                let slice = matches!(underlying, TypeIdentity::Slice(_))
                    || self
                        .types
                        .node(&input.typ)?
                        .is_some_and(|(_, node)| node.kind == wire::Slice);
                if !slice
                    && !self.types.node(&input.typ)?.is_some_and(|(_, node)| {
                        node.kind == wire::Array && node.length as usize == values.len()
                    })
                {
                    return Err(invalid());
                }
                let element = self.element_type(&input.typ)?;
                let mut elements = Vec::with_capacity(values.len());
                for value in values {
                    let value = self.import_host_value(value, depth + 1, remaining)?;
                    elements.push(self.coerce(value, &element)?);
                }
                return if slice {
                    self.make_slice(input.typ.clone(), elements.len(), elements.len(), elements)
                } else {
                    Ok(Value {
                        typ: input.typ.clone(),
                        data: Data::Array(elements.into()),
                    })
                };
            }
            HostData::Struct(fields) => {
                if fields.len() > self.limits.max_boundary_elements {
                    return Err(RuntimeError::new(
                        "boundary_limit",
                        "argument",
                        "host struct exceeds limit",
                    ));
                }
                if !self
                    .types
                    .node(&input.typ)?
                    .is_some_and(|(_, node)| node.kind == wire::Struct)
                {
                    return Err(invalid());
                }
                let layout = self.heap.layout_for_type(&input.typ)?;
                if layout.kind != wire::Struct
                    || fields.keys().any(|name| {
                        !layout
                            .fields
                            .iter()
                            .any(|field| field.name.as_str() == name.as_str())
                    })
                {
                    return Err(invalid());
                }
                let mut replacements = Vec::with_capacity(fields.len());
                for (name, input) in fields {
                    let index = layout
                        .fields
                        .iter()
                        .position(|field| field.name.as_str() == name.as_str())
                        .ok_or_else(invalid)?;
                    let value = self.import_host_value(input, depth + 1, remaining)?;
                    let value = self.coerce(value, &layout.fields[index].typ)?;
                    replacements.push((index, value));
                }
                return self.construct_aggregate(input.typ.clone(), replacements);
            }
            HostData::MapEntries(entries) => {
                if !self
                    .types
                    .node(&input.typ)?
                    .is_some_and(|(_, node)| node.kind == wire::Map)
                {
                    return Err(invalid());
                }
                if entries.len() > self.limits.max_boundary_elements {
                    return Err(RuntimeError::new(
                        "boundary_limit",
                        "argument",
                        "host map exceeds limit",
                    ));
                }
                let map = self.make_empty_map(input.typ.clone())?;
                for (key, value) in entries {
                    let key = self.import_host_value(key, depth + 1, remaining)?;
                    let value = self.import_host_value(value, depth + 1, remaining)?;
                    self.store_index(map.clone(), key, value)?;
                }
                return Ok(map);
            }
            _ => return Err(invalid()),
        };
        self.convert_value(value, input.typ.clone())
    }
}
