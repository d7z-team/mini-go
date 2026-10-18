//! Guest values use value copies for structs and arrays, and arena addresses
//! for shared storage. A pointer never borrows a Rust frame or vector element.

use crate::{
    error::RuntimeError,
    heap::{Handle, Trace},
    types::TypeIdentity,
};
mod string;
pub use string::ByteString;
mod structure;
pub use structure::StructStorage;
mod aggregate;
pub use aggregate::PreparedAggregate;
mod zero;

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

pub(crate) use crate::snapshot::PathElement;

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
    // Most address temporaries disappear before a snapshot or typed view needs
    // an origin. Publish the stable key only when that origin is observed.
    pub key: std::sync::OnceLock<std::sync::Arc<()>>,
    pub original: Option<PointerOrigin>,
    pub path_root: Option<(Handle, usize)>,
    pub array_capacity: Option<usize>,
    /// Prepared pointer location retained across decode.  The host-facing
    /// path remains optional boundary data; execution uses this compact
    /// offset/type descriptor directly.
    pub numeric: Option<(u64, u32, u32)>,
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
    pub(crate) fn reset_identity(&mut self, path_root: Option<(Handle, usize)>) {
        let identity = std::sync::Arc::make_mut(&mut self.identity);
        // No external pointer origin can observe an exclusively owned key.
        // Shared origins retain their identity while this address gets a new one.
        if identity
            .key
            .get()
            .is_some_and(|key| std::sync::Arc::strong_count(key) != 1)
        {
            identity.key = std::sync::OnceLock::new();
        }
        identity.original = None;
        identity.path_root = path_root;
        identity.array_capacity = None;
        identity.numeric = None;
    }

    pub(crate) fn pointer_origin(&self) -> PointerOrigin {
        let mut origin = PointerOrigin {
            key: self.identity.key.get_or_init(Default::default).clone(),
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
                PathElement::Index(index) => {
                    let absolute = match path.last() {
                        Some(PathElement::ArrayView { start, .. }) => start.checked_add(*index),
                        _ => None,
                    };
                    if let Some(index) = absolute {
                        path.pop();
                        path.push(PathElement::Index(index));
                    } else {
                        path.push(PathElement::Index(*index));
                    }
                }
                PathElement::Field(name) => path.push(PathElement::Field(name.clone())),
                PathElement::TypeView(_) => {}
            }
        }
        path
    }
}

impl PartialEq for Address {
    fn eq(&self, other: &Self) -> bool {
        if self.root != other.root {
            return false;
        }
        match (self.identity.numeric, other.identity.numeric) {
            (Some(left), Some(right)) => left == right,
            // A pointer loaded from a prepared record keeps its numeric view
            // alongside the detached path used by an address operation. A
            // path-only origin is equivalent when both still identify the
            // same derived location; an empty path cannot prove that a
            // numeric view targets the complete root value.
            (Some(_), None) | (None, Some(_)) => {
                !self.path.is_empty()
                    && !other.path.is_empty()
                    && (self.path == other.path || self.location_path() == other.location_path())
            }
            (None, None) => {
                self.path == other.path || self.location_path() == other.location_path()
            }
        }
    }
}
impl Eq for Address {}
impl std::hash::Hash for Address {
    fn hash<H: std::hash::Hasher>(&self, state: &mut H) {
        self.root.hash(state);
        // A prepared pointer may carry both its compact numeric location and
        // a detached path. When it is compared with a path-only address, the
        // path is the equality key, so use the same key for hashing.
        if self.identity.numeric.is_some() && !self.path.is_empty() {
            self.location_path().hash(state);
        } else if let Some(numeric) = self.identity.numeric {
            numeric.hash(state);
        } else {
            self.location_path().hash(state);
        }
    }
}

#[derive(Clone, Debug)]
pub struct FunctionValue {
    pub(crate) index: Option<usize>,
    pub(crate) revision: Option<std::sync::Arc<crate::program::Revision>>,
    /// Numeric revision identity persisted in managed payloads. The Arc is an
    /// owner-side execution cache; the managed anchor below retains the code
    /// generation without making the revision table an implicit GC root.
    pub(crate) revision_generation: Option<u64>,
    /// Managed edge to the revision anchor. The collector follows this edge
    /// directly, so retired revisions do not require a graph-wide decode.
    pub(crate) revision_anchor: Option<Handle>,
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
    Aggregate(PreparedAggregate),
    /// Value copies share storage; writes detach through `Arc::make_mut`.
    /// Logical allocation accounting counts each guest array independently.
    Array(std::sync::Arc<Vec<Value>>),
    /// Mutable byte backing, referenced through slice and pointer addresses.
    Bytes(Vec<u8>),
    Slice(std::sync::Arc<SliceValue>),
    Map(Handle),
    ResourceRef(Handle),
    Resource(Box<crate::instance::scheduler::Resource>),
    Interface(std::sync::Arc<Value>),
    Pointer(std::sync::Arc<Address>),
    Function(std::sync::Arc<FunctionValue>),
    DynamicFunction(std::sync::Arc<Value>),
    Method {
        function: std::sync::Arc<FunctionValue>,
        receiver: Option<std::sync::Arc<Value>>,
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
    pub(crate) fn field(&self, name: &str) -> Result<Value, RuntimeError> {
        match &self.data {
            Data::Aggregate(value) if !value.is_array() => value.get_field(name),
            Data::Struct(fields) => fields.get(name).cloned().ok_or_else(|| {
                RuntimeError::new("field_not_found", "value", format!("unknown field {name}"))
            }),
            _ => Err(RuntimeError::new("type_error", "value", "expected struct")),
        }
    }

    pub(crate) fn element(&self, index: usize) -> Result<Value, RuntimeError> {
        match &self.data {
            Data::Aggregate(value) => value.get(index),
            Data::Array(values) => values.get(index).cloned().ok_or_else(|| {
                RuntimeError::new(
                    "index_out_of_range",
                    "value",
                    "array index exceeds its length",
                )
            }),
            Data::Struct(fields) => fields.field_at(index).cloned().ok_or_else(|| {
                RuntimeError::new(
                    "index_out_of_range",
                    "value",
                    "field ordinal exceeds its layout",
                )
            }),
            _ => Err(RuntimeError::new(
                "type_error",
                "value",
                "expected aggregate",
            )),
        }
    }

    pub(crate) fn with_element(&self, index: usize, value: Value) -> Result<Value, RuntimeError> {
        let mut result = self.clone();
        match &mut result.data {
            Data::Aggregate(aggregate) => *aggregate = aggregate.with_value(index, &value)?,
            Data::Array(values) => {
                *std::sync::Arc::make_mut(values)
                    .get_mut(index)
                    .ok_or_else(|| {
                        RuntimeError::new(
                            "index_out_of_range",
                            "value",
                            "array index exceeds its length",
                        )
                    })? = value
            }
            Data::Struct(fields) => {
                *fields.field_at_mut(index).ok_or_else(|| {
                    RuntimeError::new(
                        "index_out_of_range",
                        "value",
                        "field ordinal exceeds its layout",
                    )
                })? = value
            }
            _ => {
                return Err(RuntimeError::new(
                    "type_error",
                    "value",
                    "expected aggregate",
                ));
            }
        }
        Ok(result)
    }

    pub(crate) fn array_range(
        &self,
        start: usize,
        length: usize,
        typ: &TypeIdentity,
    ) -> Result<Value, RuntimeError> {
        let data = match &self.data {
            Data::Aggregate(value) => Data::Aggregate(value.array_range(start, length, typ)?),
            Data::Array(values) => Data::Array(
                values
                    .get(
                        start..start.checked_add(length).ok_or_else(|| {
                            RuntimeError::new("invalid_address", "array", "range overflows")
                        })?,
                    )
                    .ok_or_else(|| {
                        RuntimeError::new("invalid_address", "array", "range exceeds backing")
                    })?
                    .to_vec()
                    .into(),
            ),
            _ => return Err(RuntimeError::new("type_error", "value", "expected array")),
        };
        Ok(Value {
            typ: typ.clone(),
            data,
        })
    }
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
    pub(crate) fn index(&self) -> Result<usize, RuntimeError> {
        match self.data {
            Data::Integer(value) => usize::try_from(value),
            Data::Unsigned(value) => usize::try_from(value),
            _ => {
                return Err(RuntimeError::new("type_error", "index", "expected integer"));
            }
        }
        .map_err(|_| RuntimeError::new("panic", "index", "index is outside addressable range"))
    }
    pub(crate) fn boolean(value: bool) -> Self {
        Self {
            typ: TypeIdentity::Primitive(crate::contract_generated::PrimitiveBool),
            data: Data::Bool(value),
        }
    }
}

impl Data {
    pub(crate) fn aggregate_shape(&self) -> Option<(bool, usize)> {
        match self {
            Self::Aggregate(value) => Some((value.is_array(), value.len())),
            Self::Array(values) => Some((true, values.len())),
            Self::Struct(fields) => Some((false, fields.len())),
            _ => None,
        }
    }
    pub(crate) fn field_name(&self, index: usize) -> Option<&str> {
        match self {
            Self::Aggregate(value) => value.field_name(index),
            Self::Struct(fields) => fields.keys().nth(index).map(String::as_str),
            _ => None,
        }
    }
}

impl Trace for Value {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        self.data.trace(visit);
    }

    fn payload_type(&self) -> Option<TypeIdentity> {
        Some(self.typ.clone())
    }

    fn payload_initialized(&self) -> bool {
        !matches!(self.data, Data::Uninitialized)
    }
}

impl Trace for Data {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        match self {
            Data::String(bytes) => {
                if let Some((_, handle, _)) = bytes.managed_backing() {
                    visit(handle);
                }
            }
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
            Data::Function(function) => {
                if let Some(anchor) = function.revision_anchor {
                    visit(anchor);
                }
                for address in &function.captures {
                    visit(address.root);
                }
            }
            Data::Method { function, receiver } => {
                if let Some(anchor) = function.revision_anchor {
                    visit(anchor);
                }
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
                for value in values.iter() {
                    value.trace(visit);
                }
            }
            Data::Aggregate(value) => value.trace(visit),
            _ => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    #[test]
    fn array_view_element_identity_matches_the_backing_element() {
        use std::hash::{Hash, Hasher};
        let root = crate::managed_storage::StorageRef {
            slot: 1,
            generation: 1,
        };
        let direct = Address {
            root,
            path: vec![PathElement::Index(3)],
            identity: Arc::default(),
        };
        let view = Address {
            root,
            path: vec![
                PathElement::ArrayView {
                    start: 2,
                    length: 2,
                    typ: TypeIdentity::Any,
                },
                PathElement::TypeView(TypeIdentity::Any),
                PathElement::Index(1),
            ],
            identity: Arc::default(),
        };
        assert_eq!(direct, view);
        let mut direct_hash = std::collections::hash_map::DefaultHasher::new();
        let mut view_hash = std::collections::hash_map::DefaultHasher::new();
        direct.hash(&mut direct_hash);
        view.hash(&mut view_hash);
        assert_eq!(direct_hash.finish(), view_hash.finish());
    }

    #[test]
    fn numeric_and_detached_pointer_paths_share_an_equality_key() {
        use std::hash::{Hash, Hasher};
        let root = crate::managed_storage::StorageRef {
            slot: 1,
            generation: 1,
        };
        let path = vec![PathElement::Index(3)];
        let numeric = Address {
            root,
            path: path.clone(),
            identity: Arc::new(PointerIdentity {
                numeric: Some((12, 7, 0)),
                ..PointerIdentity::default()
            }),
        };
        let detached = Address {
            root,
            path,
            identity: Arc::default(),
        };
        assert_eq!(numeric, detached);
        let mut numeric_hash = std::collections::hash_map::DefaultHasher::new();
        let mut detached_hash = std::collections::hash_map::DefaultHasher::new();
        numeric.hash(&mut numeric_hash);
        detached.hash(&mut detached_hash);
        assert_eq!(numeric_hash.finish(), detached_hash.finish());
    }

    #[test]
    fn pointer_origins_survive_shared_address_reset() {
        let root = crate::managed_storage::StorageRef {
            slot: 1,
            generation: 1,
        };
        let mut address = Address {
            root,
            path: Vec::new(),
            identity: Arc::default(),
        };
        let retained = address.clone();
        let original = address.pointer_origin();
        assert!(Arc::ptr_eq(&original.key, &retained.pointer_origin().key));
        address.reset_identity(Some((root, 2)));
        let updated = address.pointer_origin();
        assert!(!Arc::ptr_eq(&original.key, &updated.key));
        assert!(Arc::ptr_eq(&original.key, &retained.pointer_origin().key));
        assert_eq!(original.root, Some(root));
        assert_eq!(original.slots, 0);
        assert_eq!(updated.slots, 2);
    }

    #[test]
    fn nested_array_copies_detach_on_mutation() {
        let nested = Value {
            typ: TypeIdentity::Any,
            data: Data::Array(vec![Value::string(b"old".to_vec())].into()),
        };
        let original = Value {
            typ: TypeIdentity::Any,
            data: Data::Array(vec![nested; 2].into()),
        };
        let mut copied = original.clone();
        let (Data::Array(original_items), Data::Array(copied_items)) =
            (&original.data, &mut copied.data)
        else {
            unreachable!()
        };
        assert!(Arc::ptr_eq(original_items, copied_items));
        let Data::Array(nested) = &mut Arc::make_mut(copied_items)[0].data else {
            unreachable!()
        };
        Arc::make_mut(nested)[0] = Value::string(b"changed".to_vec());
        assert!(matches!(&nested[0].data, Data::String(text) if &text[..] == b"changed"));
        for value in original_items.iter() {
            let Data::Array(nested) = &value.data else {
                unreachable!()
            };
            let Data::String(text) = &nested[0].data else {
                unreachable!()
            };
            assert_eq!(&text[..], b"old");
        }
        let Data::Array(items) = &copied.data else {
            unreachable!()
        };
        let Data::Array(untouched) = &items[1].data else {
            unreachable!()
        };
        let Data::String(text) = &untouched[0].data else {
            unreachable!()
        };
        assert_eq!(&text[..], b"old");
    }
}
