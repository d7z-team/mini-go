//! Typed host arguments are copied and validated before publishing an invocation.

use super::*;
use crate::snapshot::{HostData, HostValue};

impl Instance {
    /// Imports host-owned value trees. Slice inputs use Array or compact byte
    /// String data; map inputs use MapEntries. Live resource handles are not inputs.
    pub fn start_host(&mut self, entry: &str, arguments: &[HostValue]) -> Result<(), RuntimeError> {
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
        if arguments.len() > self.limits.max_sequence_elements {
            return Err(RuntimeError::new(
                "boundary_limit",
                "argument",
                "argument count exceeds limit",
            ));
        }
        self.types
            .use_context(self.revision.program.decoded.types());
        let mut remaining = self.limits.max_heap_bytes;
        let result = (|| {
            let values = arguments
                .iter()
                .map(|value| self.import_host_value(value, 0, &mut remaining))
                .collect::<Result<Vec<_>, _>>()?;
            let mut pending: Vec<_> = arguments.iter().collect();
            let mut bytes = 0u64;
            while let Some(value) = pending.pop() {
                bytes = bytes.saturating_add(128);
                match &value.data {
                    HostData::String(text) => bytes = bytes.saturating_add(text.len() as u64),
                    HostData::Array(items) => {
                        bytes = bytes.saturating_add(items.len() as u64 * 16);
                        pending.extend(items);
                    }
                    HostData::Struct(fields) => {
                        bytes = bytes.saturating_add(fields.len() as u64 * 16);
                        pending.extend(fields.values());
                    }
                    HostData::MapEntries(entries) => {
                        bytes = bytes.saturating_add(entries.len() as u64 * 32);
                        for (key, value) in entries {
                            pending.push(key);
                            pending.push(value);
                        }
                    }
                    HostData::Interface(value) => pending.push(value),
                    _ => {}
                }
            }
            self.charge_guest(bytes)?;
            self.start(entry, values)
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
        if depth >= self.limits.max_value_depth {
            return Err(RuntimeError::new(
                "boundary_limit",
                "argument",
                "host value depth exceeds limit",
            ));
        }
        *remaining = remaining.checked_sub(16).ok_or_else(|| {
            RuntimeError::new(
                "boundary_limit",
                "argument",
                "host value size exceeds limit",
            )
        })?;
        let underlying = self.types.underlying(&input.typ)?;
        let primitive = match underlying {
            TypeIdentity::Primitive(primitive) => Some(primitive),
            _ => None,
        };
        let value = match &input.data {
            HostData::Nil if self.types.nil_assignable(&input.typ)? => {
                return self.zero(&input.typ, 0);
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
                *remaining = remaining.checked_sub(bytes.len() as u64).ok_or_else(|| {
                    RuntimeError::new("boundary_limit", "argument", "host string exceeds limit")
                })?;
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
                    return self.make_bytes(input.typ.clone(), bytes.len(), bytes.len(), bytes);
                }
                self.check_string_size(bytes.len())?;
                Value::string(bytes.clone())
            }
            HostData::Interface(inner) if self.types.is_interface(&input.typ)? => {
                let value = self.import_host_value(inner, depth + 1, remaining)?;
                return self.coerce(value, &input.typ);
            }
            HostData::Array(values) => {
                if values.len() > self.limits.max_sequence_elements {
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
                let mut value = self.zero(&input.typ, 0)?;
                let Data::Struct(destinations) = &mut value.data else {
                    return Err(invalid());
                };
                if fields.len() > self.limits.max_sequence_elements {
                    return Err(RuntimeError::new(
                        "boundary_limit",
                        "argument",
                        "host struct exceeds limit",
                    ));
                }
                for (name, input) in fields {
                    let destination = destinations.get_mut(name).ok_or_else(invalid)?;
                    let value = self.import_host_value(input, depth + 1, remaining)?;
                    *destination = self.coerce(value, &destination.typ)?;
                }
                return Ok(value);
            }
            HostData::MapEntries(entries) => {
                if !self
                    .types
                    .node(&input.typ)?
                    .is_some_and(|(_, node)| node.kind == wire::Map)
                {
                    return Err(invalid());
                }
                if entries.len() > self.limits.max_sequence_elements {
                    return Err(RuntimeError::new(
                        "boundary_limit",
                        "argument",
                        "host map exceeds limit",
                    ));
                }
                let root = self.allocate(Value {
                    typ: TypeIdentity::Any,
                    data: Data::MapEntries(Default::default()),
                })?;
                let map = Value {
                    typ: input.typ.clone(),
                    data: Data::Map(root),
                };
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
