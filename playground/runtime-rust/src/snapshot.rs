//! Detached host values. Object indices belong only to this snapshot, so cycles
//! and aliases survive without exposing a live guest heap handle.

use crate::{
    contract_generated as wire,
    error::RuntimeError,
    heap::{Handle, Heap},
    instance::scheduler::Resource,
    types::{TypeIdentity, TypeRegistry},
    value::{Address, Data, PathElement, Value},
};
use std::collections::{BTreeMap, HashMap};

#[derive(Clone, Debug, serde::Serialize, serde::Deserialize)]
pub struct HostValue {
    pub typ: TypeIdentity,
    pub data: HostData,
}

impl HostValue {
    pub fn int(value: i64) -> Self {
        Self {
            typ: TypeIdentity::Primitive(wire::PrimitiveInt),
            data: HostData::Integer(value),
        }
    }
    pub fn boolean(value: bool) -> Self {
        Self {
            typ: TypeIdentity::Primitive(wire::PrimitiveBool),
            data: HostData::Bool(value),
        }
    }
    pub fn string(value: impl Into<Vec<u8>>) -> Self {
        Self {
            typ: TypeIdentity::Primitive(wire::PrimitiveString),
            data: HostData::String(value.into()),
        }
    }
    pub fn bytes(value: impl Into<Vec<u8>>) -> Self {
        Self {
            typ: TypeIdentity::Slice(std::sync::Arc::new(TypeIdentity::Primitive(
                wire::PrimitiveUint8,
            ))),
            data: HostData::String(value.into()),
        }
    }
    pub fn any(value: Self) -> Self {
        Self {
            typ: TypeIdentity::Any,
            data: HostData::Interface(Box::new(value)),
        }
    }
}

#[derive(Clone, Debug, serde::Serialize, serde::Deserialize)]
pub struct HostAddress {
    pub object: usize,
    pub path: Vec<PathElement>,
}

#[derive(Clone, Debug, serde::Serialize, serde::Deserialize)]
pub enum HostData {
    Nil,
    Bool(bool),
    Integer(i64),
    Unsigned(u64),
    FloatBits(u64),
    ComplexBits {
        real: u64,
        imag: u64,
    },
    String(#[serde(with = "serde_bytes")] Vec<u8>),
    Array(Vec<HostValue>),
    Struct(BTreeMap<String, HostValue>),
    Interface(Box<HostValue>),
    Pointer(HostAddress),
    Slice {
        storage: HostAddress,
        start: usize,
        length: usize,
        capacity: usize,
    },
    Map(usize),
    MapEntries(Vec<(HostValue, HostValue)>),
    Function {
        module: String,
        function: String,
        captures: Vec<HostAddress>,
    },
    DynamicFunction(Box<HostValue>),
    Method {
        function: Box<HostValue>,
        receiver: Option<Box<HostValue>>,
    },
    Resource(usize),
    Mutex {
        locked: bool,
        waiting: usize,
        granted: bool,
    },
    Channel {
        capacity: usize,
        closed: bool,
        queued: Vec<HostValue>,
    },
}

#[derive(Clone, Copy, Debug)]
pub struct SnapshotLimits {
    pub max_bytes: usize,
    pub max_objects: usize,
    pub max_depth: usize,
}

impl Default for SnapshotLimits {
    fn default() -> Self {
        Self {
            max_bytes: 64 << 20,
            max_objects: 100_000,
            max_depth: 128,
        }
    }
}

#[derive(Clone, Debug)]
pub struct HostSnapshot {
    pub roots: Vec<HostValue>,
    pub objects: Vec<HostValue>,
    types: TypeRegistry,
}

struct SnapshotBuilder<'a> {
    types: &'a TypeRegistry,
    limits: SnapshotLimits,
    bytes: usize,
    indices: HashMap<Handle, usize>,
    pending: Vec<Handle>,
}

impl HostSnapshot {
    pub(crate) fn capture(
        heap: &Heap<Value>,
        types: &TypeRegistry,
        roots: &[Value],
        limits: SnapshotLimits,
    ) -> Result<Self, RuntimeError> {
        let mut builder = SnapshotBuilder {
            types,
            limits,
            bytes: 0,
            indices: HashMap::new(),
            pending: Vec::new(),
        };
        let roots = roots
            .iter()
            .map(|value| builder.value(value, 0))
            .collect::<Result<Vec<_>, _>>()?;
        let mut objects = Vec::new();
        while objects.len() < builder.pending.len() {
            let value = heap.get(builder.pending[objects.len()])?;
            objects.push(builder.value(&value, 0)?);
        }
        Ok(Self {
            roots,
            objects,
            types: types.clone(),
        })
    }

    pub fn types(&self) -> &TypeRegistry {
        &self.types
    }

    pub fn resolve_address(
        &self,
        address: &HostAddress,
    ) -> Result<std::borrow::Cow<'_, HostValue>, RuntimeError> {
        let value = self.objects.get(address.object).ok_or_else(|| {
            RuntimeError::new("snapshot_address", "snapshot", "object does not exist")
        })?;
        self.project_address(value, &address.path)
    }

    fn project_address<'a>(
        &self,
        mut value: &'a HostValue,
        path: &[PathElement],
    ) -> Result<std::borrow::Cow<'a, HostValue>, RuntimeError> {
        let invalid = || {
            RuntimeError::new(
                "snapshot_address",
                "snapshot",
                "object path does not resolve",
            )
        };
        if path.len() > 256 {
            return Err(invalid());
        }
        if let HostData::String(bytes) = &value.data
            && !path.is_empty()
        {
            let (range, typ, scalar) =
                crate::value::byte_projection(path, bytes.len(), &value.typ).ok_or_else(invalid)?;
            return Ok(std::borrow::Cow::Owned(HostValue {
                typ,
                data: if scalar {
                    HostData::Unsigned(u64::from(bytes[range.start]))
                } else {
                    HostData::Array(
                        bytes[range]
                            .iter()
                            .map(|byte| HostValue {
                                typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                                data: HostData::Unsigned(u64::from(*byte)),
                            })
                            .collect(),
                    )
                },
            }));
        }
        let mut window: Option<(usize, usize, &TypeIdentity)> = None;
        for (index, segment) in path.iter().enumerate() {
            if let PathElement::TypeView(typ) = segment {
                let value = if let Some((start, length, typ)) = window {
                    let HostData::Array(values) = &value.data else {
                        return Err(invalid());
                    };
                    HostValue {
                        typ: typ.clone(),
                        data: HostData::Array(values[start..start + length].to_vec()),
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
            if let PathElement::ArrayView { start, length, typ } = segment {
                let HostData::Array(values) = &value.data else {
                    return Err(invalid());
                };
                let (offset, available, _) = window.unwrap_or((0, values.len(), &value.typ));
                if *start > available || *length > available - start {
                    return Err(invalid());
                }
                window = Some((offset + start, *length, typ));
                continue;
            }
            value = match (segment, &value.data) {
                (PathElement::Field(name), HostData::Struct(fields)) => fields.get(name),
                (PathElement::Index(index), HostData::Array(values)) => {
                    let (start, length, _) = window.take().unwrap_or((0, values.len(), &value.typ));
                    if *index < length {
                        values.get(start + index)
                    } else {
                        None
                    }
                }
                _ => None,
            }
            .ok_or_else(invalid)?;
        }
        if let Some((start, length, typ)) = window {
            let HostData::Array(values) = &value.data else {
                unreachable!()
            };
            Ok(std::borrow::Cow::Owned(HostValue {
                typ: typ.clone(),
                data: HostData::Array(values[start..start + length].to_vec()),
            }))
        } else {
            Ok(std::borrow::Cow::Borrowed(value))
        }
    }

    fn retype_value(
        &self,
        mut value: HostValue,
        typ: &TypeIdentity,
        depth: usize,
    ) -> Result<HostValue, RuntimeError> {
        if depth > 256 {
            return Err(RuntimeError::new(
                "snapshot_limit",
                "snapshot",
                "typed view depth exceeded",
            ));
        }
        if !self.types.identical(&value.typ, typ)? {
            match &mut value.data {
                HostData::Struct(fields) => {
                    let (module, node) = self
                        .types
                        .node(typ)?
                        .filter(|(_, node)| node.kind == wire::Struct)
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "snapshot_type",
                                "snapshot",
                                "typed view requires struct",
                            )
                        })?;
                    for field in node.fields.iter() {
                        let previous = fields.remove(&field.name).ok_or_else(|| {
                            RuntimeError::new(
                                "snapshot_type",
                                "snapshot",
                                "typed view field missing",
                            )
                        })?;
                        fields.insert(
                            field.name.clone(),
                            self.retype_value(
                                previous,
                                &self.types.resolve(module, &field.r#type)?,
                                depth + 1,
                            )?,
                        );
                    }
                }
                HostData::Array(values) => {
                    let (module, node) = self
                        .types
                        .node(typ)?
                        .filter(|(_, node)| node.kind == wire::Array)
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "snapshot_type",
                                "snapshot",
                                "typed view requires array",
                            )
                        })?;
                    let element = self.types.resolve(module, &node.elem)?;
                    for value in values {
                        *value = self.retype_value(value.clone(), &element, depth + 1)?;
                    }
                }
                HostData::Pointer(address) => {
                    let element = match self.types.underlying(typ)? {
                        TypeIdentity::Pointer(element) => (*element).clone(),
                        _ => {
                            let (module, node) = self
                                .types
                                .node(typ)?
                                .filter(|(_, node)| node.kind == wire::Pointer)
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "snapshot_type",
                                        "snapshot",
                                        "typed view requires pointer",
                                    )
                                })?;
                            self.types.resolve(module, &node.elem)?
                        }
                    };
                    while matches!(address.path.last(), Some(PathElement::TypeView(_))) {
                        address.path.pop();
                    }
                    address.path.push(PathElement::TypeView(element));
                }
                _ => {}
            }
        }
        value.typ = typ.clone();
        Ok(value)
    }

    /// Copies the visible bytes of a string or []byte snapshot value.
    pub fn bytes(&self, value: &HostValue) -> Result<Vec<u8>, RuntimeError> {
        let invalid =
            || RuntimeError::new("snapshot_type", "bytes", "expected string or byte slice");
        if !matches!(value.data, HostData::String(_)) {
            let element = match self.types.underlying(&value.typ)? {
                TypeIdentity::Slice(element) => (*element).clone(),
                typ => {
                    let (module, node) = self
                        .types
                        .node(&typ)?
                        .filter(|(_, node)| node.kind == crate::contract_generated::Slice)
                        .ok_or_else(invalid)?;
                    self.types.resolve(module, &node.elem)?
                }
            };
            if self.types.underlying(&element)?
                != TypeIdentity::Primitive(crate::contract_generated::PrimitiveUint8)
            {
                return Err(invalid());
            }
        }
        match &value.data {
            HostData::String(bytes) => Ok(bytes.clone()),
            HostData::Nil => Ok(Vec::new()),
            HostData::Slice {
                storage,
                start,
                length,
                ..
            } => {
                let backing = self.resolve_address(storage)?;
                if let HostData::String(bytes) = &backing.data {
                    let end = start.checked_add(*length).ok_or_else(invalid)?;
                    return bytes
                        .get(*start..end)
                        .map(<[u8]>::to_vec)
                        .ok_or_else(invalid);
                }
                let HostData::Array(values) = &backing.data else {
                    return Err(invalid());
                };
                let end = start.checked_add(*length).ok_or_else(invalid)?;
                values
                    .get(*start..end)
                    .ok_or_else(invalid)?
                    .iter()
                    .map(|value| match value.data {
                        HostData::Unsigned(byte) => u8::try_from(byte).map_err(|_| invalid()),
                        _ => Err(invalid()),
                    })
                    .collect()
            }
            _ => Err(invalid()),
        }
    }
}

impl SnapshotBuilder<'_> {
    fn charge(&mut self, count: usize) -> Result<(), RuntimeError> {
        self.bytes = self
            .bytes
            .checked_add(count)
            .filter(|bytes| *bytes <= self.limits.max_bytes)
            .ok_or_else(|| {
                RuntimeError::new(
                    "snapshot_limit",
                    "snapshot",
                    "snapshot byte budget exceeded",
                )
            })?;
        Ok(())
    }

    fn object(&mut self, handle: Handle) -> Result<usize, RuntimeError> {
        if let Some(index) = self.indices.get(&handle) {
            return Ok(*index);
        }
        if self.pending.len() >= self.limits.max_objects {
            return Err(RuntimeError::new(
                "snapshot_limit",
                "snapshot",
                "snapshot object budget exceeded",
            ));
        }
        self.charge(128)?;
        let index = self.pending.len();
        self.indices.insert(handle, index);
        self.pending.push(handle);
        Ok(index)
    }

    fn address(&mut self, address: &Address) -> Result<HostAddress, RuntimeError> {
        for segment in &address.path {
            self.charge(
                16 + match segment {
                    PathElement::Field(name) => name.len(),
                    _ => 0,
                },
            )?;
        }
        Ok(HostAddress {
            object: self.object(address.root)?,
            path: address.path.clone(),
        })
    }

    fn value(&mut self, value: &Value, depth: usize) -> Result<HostValue, RuntimeError> {
        if depth >= self.limits.max_depth {
            return Err(RuntimeError::new(
                "snapshot_limit",
                "snapshot",
                "snapshot value depth exceeded",
            ));
        }
        if matches!(value.data, Data::Uninitialized) {
            let mut remaining = self.limits.max_bytes.saturating_sub(self.bytes) as u64;
            let zero = Value::zero_with_budget(
                self.types,
                &value.typ,
                depth,
                self.limits.max_depth,
                self.limits.max_bytes / 16,
                &mut remaining,
            )?;
            return self.value(&zero, depth);
        }
        self.charge(16)?;
        let data = match &value.data {
            Data::Uninitialized => unreachable!("zero value was materialized above"),
            Data::OpaqueJSON(_) => {
                return Err(RuntimeError::new(
                    "snapshot_type",
                    "snapshot",
                    "opaque JSON constant cannot cross the host value boundary",
                ));
            }
            Data::Nil => HostData::Nil,
            Data::Bool(value) => HostData::Bool(*value),
            Data::Integer(value) => HostData::Integer(*value),
            Data::Unsigned(value) => HostData::Unsigned(*value),
            Data::Float(value) => HostData::FloatBits(value.to_bits()),
            Data::Complex { real, imag } => HostData::ComplexBits {
                real: real.to_bits(),
                imag: imag.to_bits(),
            },
            Data::String(bytes) => {
                self.charge(bytes.len())?;
                HostData::String(bytes.to_vec())
            }
            Data::Bytes(bytes) => {
                self.charge(bytes.len())?;
                HostData::String(bytes.clone())
            }
            Data::Array(values) => HostData::Array(
                values
                    .iter()
                    .map(|value| self.value(value, depth + 1))
                    .collect::<Result<_, _>>()?,
            ),
            Data::Struct(fields) => {
                let mut output = BTreeMap::new();
                for (name, value) in fields {
                    self.charge(name.len())?;
                    output.insert(name.clone(), self.value(value, depth + 1)?);
                }
                HostData::Struct(output)
            }
            Data::Interface(value) => HostData::Interface(Box::new(self.value(value, depth + 1)?)),
            Data::Pointer(address) => HostData::Pointer(self.address(address)?),
            Data::Slice(slice) => HostData::Slice {
                storage: self.address(&slice.storage)?,
                start: slice.start,
                length: slice.length,
                capacity: slice.capacity,
            },
            Data::Map(handle) => HostData::Map(self.object(*handle)?),
            Data::MapEntries(entries) => HostData::MapEntries(
                entries
                    .iter()
                    .map(|(key, value)| {
                        Ok((self.value(key, depth + 1)?, self.value(value, depth + 1)?))
                    })
                    .collect::<Result<_, RuntimeError>>()?,
            ),
            Data::Function(function) => {
                self.charge(function.module.len() + function.function.len())?;
                HostData::Function {
                    module: function.module.to_string(),
                    function: function.function.to_string(),
                    captures: function
                        .captures
                        .iter()
                        .map(|address| self.address(address))
                        .collect::<Result<_, _>>()?,
                }
            }
            Data::DynamicFunction(callback) => {
                HostData::DynamicFunction(Box::new(self.value(callback, depth + 1)?))
            }
            Data::Method { function, receiver } => HostData::Method {
                function: Box::new(self.value(
                    &Value {
                        typ: TypeIdentity::Primitive(wire::PrimitiveFunction),
                        data: Data::Function(function.clone()),
                    },
                    depth + 1,
                )?),
                receiver: receiver
                    .as_ref()
                    .map(|value| self.value(value, depth + 1).map(Box::new))
                    .transpose()?,
            },
            Data::ResourceRef(handle) => HostData::Resource(self.object(*handle)?),
            Data::Resource(resource) => match &**resource {
                Resource::Mutex {
                    locked,
                    grant,
                    waiters,
                } => HostData::Mutex {
                    locked: *locked,
                    waiting: waiters.len(),
                    granted: grant.is_some(),
                },
                Resource::Channel {
                    capacity,
                    closed,
                    values,
                    ..
                } => HostData::Channel {
                    capacity: *capacity,
                    closed: *closed,
                    queued: values
                        .iter()
                        .map(|value| self.value(value, depth + 1))
                        .collect::<Result<_, _>>()?,
                },
            },
        };
        Ok(HostValue {
            typ: if value.typ == TypeIdentity::Any {
                match value.data {
                    Data::Bool(_) => TypeIdentity::Primitive(wire::PrimitiveBool),
                    Data::String(_) => TypeIdentity::Primitive(wire::PrimitiveString),
                    _ => value.typ.clone(),
                }
            } else {
                value.typ.clone()
            },
            data,
        })
    }
}
