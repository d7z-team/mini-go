//! Guest values use value copies for structs and arrays, and arena addresses
//! for shared storage. A pointer never borrows a Rust frame or vector element.

use crate::{
    error::RuntimeError,
    heap::{Handle, Trace},
    types::TypeIdentity,
};

mod map;
pub use map::MapStorage;
mod string;
pub use string::ByteString;
mod structure;
pub use structure::StructStorage;
mod zero;

pub(crate) enum ValueRead<'a> {
    Borrowed(&'a Value),
    Owned(Value),
    Shared(std::sync::Arc<Value>),
}

impl std::ops::Deref for ValueRead<'_> {
    type Target = Value;
    fn deref(&self) -> &Value {
        match self {
            Self::Borrowed(value) => value,
            Self::Owned(value) => value,
            Self::Shared(value) => value,
        }
    }
}

impl ValueRead<'_> {
    pub(crate) fn into_owned(self) -> Value {
        match self {
            Self::Borrowed(value) => value.clone(),
            Self::Owned(value) => value,
            Self::Shared(value) => std::sync::Arc::unwrap_or_clone(value),
        }
    }
}

pub(crate) fn decode_rune(bytes: &[u8]) -> (u32, usize) {
    let bytes = &bytes[..bytes.len().min(4)];
    let prefix = match std::str::from_utf8(bytes) {
        Ok(text) => text,
        Err(error) => std::str::from_utf8(&bytes[..error.valid_up_to()]).unwrap(),
    };
    match prefix.chars().next() {
        Some(rune) => (u32::from(rune), rune.len_utf8()),
        None => (u32::from(char::REPLACEMENT_CHARACTER), 1),
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Hash, serde::Serialize, serde::Deserialize)]
pub enum PathElement {
    Field(String),
    Index(usize),
    ArrayView {
        start: usize,
        length: usize,
        typ: TypeIdentity,
    },
    TypeView(TypeIdentity),
}

/// Resolve a path into compact backing without expanding it into scalar slots.
pub(crate) fn byte_projection(
    path: &[PathElement],
    length: usize,
    typ: &TypeIdentity,
) -> Option<(std::ops::Range<usize>, TypeIdentity, bool)> {
    let mut range = 0..length;
    let mut typ = typ.clone();
    let mut scalar = false;
    for segment in path {
        match segment {
            PathElement::ArrayView {
                start,
                length,
                typ: view,
            } if !scalar && *start <= range.len() && *length <= range.len() - start => {
                range.start += start;
                range.end = range.start + length;
                typ = view.clone();
            }
            PathElement::Index(index) if !scalar && *index < range.len() => {
                range.start += index;
                range.end = range.start + 1;
                scalar = true;
                typ = TypeIdentity::Primitive(crate::contract_generated::PrimitiveUint8);
            }
            PathElement::TypeView(view) => typ = view.clone(),
            _ => return None,
        }
    }
    Some((range, typ, scalar))
}

#[derive(Clone, Debug)]
pub struct Address {
    pub(crate) root: Handle,
    pub(crate) path: Vec<PathElement>,
    pub(crate) identity: std::sync::Arc<PointerIdentity>,
}

#[derive(Clone, Debug, Default)]
pub(crate) struct PointerIdentity {
    pub key: std::sync::Arc<()>,
    pub original: Option<PointerOrigin>,
    pub path_root: Option<(Handle, usize)>,
    pub array_capacity: Option<usize>,
}

#[derive(Clone, Debug)]
pub(crate) struct PointerOrigin {
    pub key: std::sync::Arc<()>,
    pub root: Option<Handle>,
    pub slots: usize,
    pub array: Option<(Handle, Vec<PathElement>, usize)>,
    pub pointee: Option<TypeIdentity>,
}

impl Address {
    pub(crate) fn pointer_origin(&self) -> PointerOrigin {
        let mut origin = PointerOrigin {
            key: self.identity.key.clone(),
            root: None,
            slots: 0,
            array: None,
            pointee: None,
        };
        if self.identity.original.is_some() {
            return origin;
        }
        if let Some(capacity) = self.identity.array_capacity {
            origin.array = Some((self.root, self.path.clone(), capacity));
        } else {
            let (root, slots) = self.identity.path_root.unwrap_or((
                self.root,
                self.path
                    .iter()
                    .map(|segment| match segment {
                        PathElement::Index(_) => 2,
                        PathElement::Field(_) => 1,
                        _ => 0,
                    })
                    .sum(),
            ));
            origin.root = Some(root);
            origin.slots = slots;
        }
        origin
    }
    fn location_path(&self) -> Vec<PathElement> {
        let mut path = Vec::with_capacity(self.path.len());
        for segment in &self.path {
            match segment {
                PathElement::ArrayView { start, .. } => path.push(PathElement::ArrayView {
                    start: *start,
                    length: 0,
                    typ: TypeIdentity::Any,
                }),
                PathElement::Index(index) => path.push(PathElement::Index(*index)),
                PathElement::Field(name) => path.push(PathElement::Field(name.clone())),
                PathElement::TypeView(_) => {}
            }
        }
        path
    }
}

impl PartialEq for Address {
    fn eq(&self, other: &Self) -> bool {
        self.root == other.root
            && (self.path == other.path || self.location_path() == other.location_path())
    }
}
impl Eq for Address {}
impl std::hash::Hash for Address {
    fn hash<H: std::hash::Hasher>(&self, state: &mut H) {
        self.root.hash(state);
        self.location_path().hash(state);
    }
}

#[derive(Clone, Debug)]
pub struct FunctionValue {
    pub(crate) index: Option<usize>,
    pub(crate) revision: Option<std::sync::Arc<crate::program::Revision>>,
    pub(crate) module: std::sync::Arc<str>,
    pub(crate) function: std::sync::Arc<str>,
    pub(crate) captures: Vec<Address>,
}

#[derive(Clone, Debug)]
pub struct SliceValue {
    pub(crate) storage: Address,
    pub(crate) start: usize,
    pub(crate) length: usize,
    pub(crate) capacity: usize,
    pub(crate) identity: std::sync::Arc<()>,
}

#[derive(Clone, Debug)]
pub enum Data {
    /// Typed slot whose zero layout has not been read or written yet.
    Uninitialized,
    Nil,
    Bool(bool),
    Integer(i64),
    Unsigned(u64),
    Float(f64),
    Complex {
        real: f64,
        imag: f64,
    },
    String(ByteString),
    OpaqueJSON(std::sync::Arc<[u8]>),
    Struct(StructStorage),
    Array(Vec<Value>),
    /// Mutable byte backing, referenced through slice and pointer addresses.
    Bytes(Vec<u8>),
    Slice(SliceValue),
    Map(Handle),
    MapEntries(MapStorage),
    ResourceRef(Handle),
    Resource(Box<crate::instance::scheduler::Resource>),
    Interface(Box<Value>),
    Pointer(Address),
    Function(FunctionValue),
    DynamicFunction(Box<Value>),
    Method {
        function: FunctionValue,
        receiver: Option<Box<Value>>,
    },
}

#[derive(Clone, Debug)]
pub struct Value {
    pub(crate) typ: TypeIdentity,
    pub(crate) data: Data,
}

impl std::fmt::Display for Value {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match &self.data {
            Data::Nil => formatter.write_str("<nil>"),
            Data::Bool(value) => value.fmt(formatter),
            Data::Integer(value) => value.fmt(formatter),
            Data::Unsigned(value) => value.fmt(formatter),
            Data::Float(value) => value.fmt(formatter),
            Data::Complex { real, imag } => write!(formatter, "({real}{imag:+}i)"),
            Data::String(value) => formatter.write_str(&String::from_utf8_lossy(value)),
            Data::Interface(value) => value.fmt(formatter),
            data => write!(formatter, "{data:?}"),
        }
    }
}

impl Value {
    pub fn string(value: impl Into<Vec<u8>>) -> Self {
        Self {
            typ: TypeIdentity::Primitive(crate::contract_generated::PrimitiveString),
            data: Data::String(value.into().into()),
        }
    }
    pub fn int(value: i64) -> Self {
        Self {
            typ: TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt),
            data: Data::Integer(value),
        }
    }
    pub fn typ(&self) -> &TypeIdentity {
        &self.typ
    }
    pub fn data(&self) -> &Data {
        &self.data
    }
    pub(crate) fn logical_bytes(&self) -> Result<u64, RuntimeError> {
        if matches!(
            self.data,
            Data::Uninitialized
                | Data::Nil
                | Data::Bool(_)
                | Data::Integer(_)
                | Data::Unsigned(_)
                | Data::Float(_)
                | Data::Complex { .. }
                | Data::Map(_)
                | Data::ResourceRef(_)
        ) {
            return Ok(16);
        }
        let mut bytes = 16u64;
        let mut pending = vec![self];
        while let Some(value) = pending.pop() {
            let extra = match &value.data {
                Data::String(bytes) => bytes.len() as u64,
                Data::OpaqueJSON(bytes) => bytes.len() as u64,
                Data::Bytes(bytes) => 128 + bytes.len() as u64,
                Data::Struct(fields) => {
                    pending.extend(fields.values());
                    128 + fields.len() as u64 * 16
                }
                Data::Array(values) => {
                    pending.extend(values);
                    128 + values.len() as u64 * 16
                }
                Data::MapEntries(entries) => {
                    for (key, value) in entries {
                        pending.push(key);
                        pending.push(value);
                    }
                    128 + entries.len() as u64 * 32
                }
                Data::Function(function) => function.captures.len() as u64 * 16,
                Data::Pointer(address) => {
                    128 + address.path.len() as u64 * 16
                        + u64::from(address.identity.original.is_some()) * 128
                }
                Data::Method { function, receiver } => {
                    if let Some(receiver) = receiver {
                        pending.push(receiver);
                    }
                    function.captures.len() as u64 * 16
                }
                Data::Resource(resource) => resource.logical_bytes(),
                Data::Interface(value) | Data::DynamicFunction(value) => {
                    pending.push(value);
                    16
                }
                _ => 0,
            };
            bytes = bytes.checked_add(extra).ok_or_else(|| {
                RuntimeError::new("allocation_limit", "value", "logical size overflow")
            })?;
        }
        Ok(bytes)
    }
    pub fn integer(&self) -> Result<i64, RuntimeError> {
        match self.data {
            Data::Integer(value) => Ok(value),
            _ => Err(RuntimeError::new(
                "type_error",
                "value",
                "expected signed integer",
            )),
        }
    }
    pub(crate) fn boolean(value: bool) -> Self {
        Self {
            typ: TypeIdentity::Primitive(crate::contract_generated::PrimitiveBool),
            data: Data::Bool(value),
        }
    }
}

impl Trace for Value {
    fn stable_edges(&self) -> bool {
        true
    }
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        self.data.trace(visit);
    }
}

impl Trace for Data {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        match self {
            Data::Pointer(address) => {
                visit(address.root);
                if let Some((root, _)) = address.identity.path_root {
                    visit(root);
                }
                if let Some(origin) = &address.identity.original {
                    if let Some(root) = origin.root {
                        visit(root);
                    }
                    if let Some((root, _, _)) = &origin.array {
                        visit(*root);
                    }
                }
            }
            Data::Slice(slice) => visit(slice.storage.root),
            Data::Map(handle) => visit(*handle),
            Data::ResourceRef(handle) => visit(*handle),
            Data::Resource(resource) => resource.trace(visit),
            Data::Interface(value) | Data::DynamicFunction(value) => value.trace(visit),
            Data::MapEntries(entries) => {
                for (key, value) in entries {
                    key.trace(visit);
                    value.trace(visit);
                }
            }
            Data::Function(function) => {
                for address in &function.captures {
                    visit(address.root);
                }
            }
            Data::Method { function, receiver } => {
                for address in &function.captures {
                    visit(address.root);
                }
                if let Some(receiver) = receiver {
                    receiver.trace(visit);
                }
            }
            Data::Struct(fields) => {
                for value in fields.values() {
                    value.trace(visit);
                }
            }
            Data::Array(values) => {
                for value in values {
                    value.trace(visit);
                }
            }
            _ => {}
        }
    }
}
