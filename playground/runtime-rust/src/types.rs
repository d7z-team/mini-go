//! Structural type lookup. Text formatting is deliberately separate from
//! resolution so execution can retain stable named and module-local identities.

use crate::{contract_generated as wire, error::RuntimeError};
use std::collections::{HashMap, HashSet};
use std::sync::{Arc, RwLock};

mod dynamic;
pub(crate) mod names;
pub use dynamic::{ConstructedField, ConstructedType};

#[derive(Clone, Debug, PartialEq, Eq, Hash, serde::Serialize, serde::Deserialize)]
pub enum TypeIdentity {
    Void,
    Any,
    Primitive(u8),
    Pointer(Arc<TypeIdentity>),
    Slice(Arc<TypeIdentity>),
    Named(Arc<wire::TypeKey>),
    Structural { module: Arc<str>, node: Arc<str> },
}

/// Compiler-owned aggregate storage shape. Runtime payload writers consume
/// this descriptor instead of rediscovering field names or sequence bounds
/// from a guest value. Offsets are logical word offsets in a prepared record;
/// they are stable across value instances and hot-patch revisions that retain
/// the same type fingerprint.
#[derive(Clone, Debug)]
pub(crate) enum StorageTypeLayout {
    Struct {
        fields: Arc<Vec<(String, TypeIdentity)>>,
    },
    Array {
        element: TypeIdentity,
        length: usize,
    },
    Slice {
        _element: TypeIdentity,
    },
}

/// Physical byte layout shared by records, containers and frame banks.
/// Aggregate fields are inline; reference and dynamic offsets describe their
/// actual payload lanes. `fixed` identifies layouts containing only scalars.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct StorageLaneLayout {
    pub kind: u8,
    pub primitive: u8,
    pub stride: u32,
    pub references: Vec<u32>,
    pub dynamics: Vec<u32>,
    pub fixed: bool,
    pub fields: Vec<StorageField>,
    pub element: Option<(TypeIdentity, Arc<StorageLaneLayout>, usize)>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct StorageField {
    pub name: String,
    pub typ: TypeIdentity,
    pub offset: u32,
    pub layout: Arc<StorageLaneLayout>,
}

#[derive(Clone, Debug)]
pub struct TypeRegistry {
    cache: Arc<RwLock<TypeCache>>,
    storage_ids: Arc<RwLock<StorageTypeIdentities>>,
    nodes: Arc<HashMap<NodeIdentity, wire::TypeNode>>,
    named: Arc<HashMap<wire::TypeKey, NodeIdentity>>,
    node_aliases: Arc<HashMap<(String, String), String>>,
    dynamic_nodes: Arc<HashMap<NodeIdentity, wire::TypeNode>>,
    dynamic_refs: Arc<HashMap<TypeIdentity, wire::TypeRef>>,
    dynamic_module: String,
    // Construction identities are lineage-local and remain monotonic even
    // after unreachable dynamic nodes are pruned. Reusing a node suffix can
    // make an old structural identity resolve to a newly constructed shape.
    next_dynamic_sequence: u64,
    next_imported_sequence: u64,
    registered_dynamic: Arc<HashSet<TypeIdentity>>,
    dynamic_costs: Arc<HashMap<TypeIdentity, u64>>,
    // Shape interning is a derived catalog. It is cleared with dynamic nodes
    // so a retired identity can never be handed back to a later caller.
    dynamic_shapes: Arc<HashMap<ConstructedType, TypeIdentity>>,
    dynamic_bytes: u64,
}

type NodeIdentity = (Arc<str>, Arc<str>);

/// Numeric identities in managed dynamic lanes belong to the instance's
/// registry lineage. Revision overlays share the catalog, while preparation
/// caches may change independently. IDs are never recycled after pruning.
#[derive(Debug, Default)]
struct StorageTypeIdentities {
    by_identity: HashMap<TypeIdentity, u32>,
    by_id: HashMap<u32, (TypeIdentity, Arc<StorageLaneLayout>)>,
    next: u32,
}

#[derive(Debug, Default)]
struct TypeCache {
    resolved: HashMap<String, HashMap<wire::TypeRef, TypeIdentity>>,
    resolved_count: usize,
    underlying: HashMap<TypeIdentity, TypeIdentity>,
    identical: HashMap<(TypeIdentity, TypeIdentity, bool), bool>,
    zeros: HashMap<(TypeIdentity, usize, usize), ZeroTemplate>,
    zero_bytes: u64,
    /// Prepared storage descriptors are immutable for a registry revision.
    /// Keeping them in the shared cache avoids rebuilding field names and
    /// resolved child identities for every aggregate access.
    storage: HashMap<TypeIdentity, Option<StorageTypeLayout>>,
    lanes: HashMap<TypeIdentity, Arc<StorageLaneLayout>>,
    fixed_words: HashMap<TypeIdentity, Option<usize>>,
    logical_bytes: HashMap<TypeIdentity, Option<u64>>,
}

#[derive(Debug)]
struct ZeroTemplate {
    value: crate::value::Value,
    construction_bytes: u64,
}

const TYPE_CACHE_LIMIT: usize = 16_384;

impl TypeRegistry {
    /// Traversal includes loaded types as well as the candidate dynamic graph.
    /// A dynamic publication quota does not restrict existing static types.
    pub(crate) fn traversal_limit(&self, additional_nodes: usize) -> usize {
        self.nodes
            .len()
            .saturating_add(self.dynamic_nodes.len())
            .saturating_add(self.dynamic_refs.len())
            .saturating_add(additional_nodes)
            .saturating_add(1)
    }

    pub(crate) fn fork_storage_catalog(&mut self) {
        self.storage_ids = Arc::default();
    }

    pub(crate) fn storage_type_id(&self, identity: &TypeIdentity) -> Result<u32, RuntimeError> {
        if let Some(id) = self.storage_ids.read().unwrap().by_identity.get(identity) {
            return Ok(*id);
        }
        // Reject identities outside this registry before publishing a lane
        // that would subsequently have no prepared decoder.
        let layout = self.storage_lane_layout(identity)?;
        let mut ids = self.storage_ids.write().unwrap();
        if let Some(id) = ids.by_identity.get(identity) {
            return Ok(*id);
        }
        let id = ids.next.checked_add(1).ok_or_else(|| {
            RuntimeError::new("type_limit", "storage type", "numeric type IDs exhausted")
        })?;
        ids.next = id;
        ids.by_identity.insert(identity.clone(), id);
        ids.by_id.insert(id, (identity.clone(), layout));
        Ok(id)
    }

    pub(crate) fn identity_for_storage_type_id(&self, id: u32) -> Option<TypeIdentity> {
        self.storage_ids
            .read()
            .unwrap()
            .by_id
            .get(&id)
            .map(|(identity, _)| identity.clone())
    }

    pub(crate) fn prepared_storage_type(
        &self,
        id: u32,
    ) -> Option<(TypeIdentity, Arc<StorageLaneLayout>)> {
        self.storage_ids.read().unwrap().by_id.get(&id).cloned()
    }

    pub(crate) fn storage_lane_layout(
        &self,
        identity: &TypeIdentity,
    ) -> Result<Arc<StorageLaneLayout>, RuntimeError> {
        fn prepare(
            registry: &TypeRegistry,
            identity: &TypeIdentity,
            active: &mut HashSet<TypeIdentity>,
        ) -> Result<Arc<StorageLaneLayout>, RuntimeError> {
            if let Some(layout) = registry.cache.read().unwrap().lanes.get(identity) {
                return Ok(layout.clone());
            }
            let underlying = registry.underlying(identity)?;
            if !active.insert(underlying.clone()) {
                return Err(RuntimeError::new(
                    "type_cycle",
                    "storage layout",
                    "recursive inline value layout",
                ));
            }
            let overflow =
                || RuntimeError::new("allocation_limit", "storage layout", "lane width overflows");
            let (kind, primitive) = match &underlying {
                TypeIdentity::Any | TypeIdentity::Primitive(wire::PrimitiveError) => {
                    (wire::Interface, 0)
                }
                TypeIdentity::Primitive(primitive) => (wire::Primitive, *primitive),
                TypeIdentity::Pointer(_) => (wire::Pointer, 0),
                TypeIdentity::Slice(_) => (wire::Slice, 0),
                TypeIdentity::Structural { .. } => (registry.node(&underlying)?.unwrap().1.kind, 0),
                TypeIdentity::Void => (wire::Invalid, 0),
                TypeIdentity::Named(_) => unreachable!("underlying resolves named types"),
            };
            let mut layout = StorageLaneLayout {
                kind,
                primitive,
                stride: 0,
                references: Vec::new(),
                dynamics: Vec::new(),
                fixed: true,
                fields: Vec::new(),
                element: None,
            };
            match &underlying {
                TypeIdentity::Void => {}
                TypeIdentity::Any | TypeIdentity::Primitive(wire::PrimitiveError) => {
                    layout.stride = 24;
                    layout.dynamics.push(0);
                    layout.fixed = false;
                }
                TypeIdentity::Primitive(wire::PrimitiveString) | TypeIdentity::Pointer(_) => {
                    layout.stride = 24;
                    layout.references.push(0);
                    layout.fixed = false;
                }
                TypeIdentity::Slice(_) => {
                    layout.stride = 48;
                    layout.references.push(0);
                    layout.fixed = false;
                }
                TypeIdentity::Primitive(wire::PrimitiveFunction) => {
                    layout.stride = 8;
                    layout.references.push(0);
                    layout.fixed = false;
                }
                TypeIdentity::Primitive(wire::PrimitiveComplex64 | wire::PrimitiveComplex128) => {
                    layout.stride = 16;
                }
                TypeIdentity::Primitive(primitive)
                    if *primitive != wire::PrimitiveInvalid
                        && usize::from(*primitive) < wire::STORAGE_PRIMITIVE_BYTES.len() =>
                {
                    layout.stride = 8;
                }
                TypeIdentity::Structural { .. } => {
                    let (module, node) = registry.node(&underlying)?.ok_or_else(|| {
                        RuntimeError::new(
                            "unknown_type",
                            "storage layout",
                            "missing structural type",
                        )
                    })?;
                    match node.kind {
                        wire::Struct => {
                            for field in node.fields.iter() {
                                let typ = registry.resolve(module, &field.r#type)?;
                                let child = prepare(registry, &typ, active)?;
                                let offset = layout.stride;
                                layout.stride =
                                    offset.checked_add(child.stride).ok_or_else(overflow)?;
                                layout
                                    .references
                                    .extend(child.references.iter().map(|lane| offset + *lane));
                                layout
                                    .dynamics
                                    .extend(child.dynamics.iter().map(|lane| offset + *lane));
                                layout.fixed &= child.fixed;
                                layout.fields.push(StorageField {
                                    name: field.name.clone(),
                                    typ,
                                    offset,
                                    layout: child,
                                });
                            }
                        }
                        wire::Array => {
                            let element_type = registry.resolve(module, &node.elem)?;
                            let element = prepare(registry, &element_type, active)?;
                            let count = u32::try_from(node.length).map_err(|_| overflow())?;
                            layout.stride =
                                element.stride.checked_mul(count).ok_or_else(overflow)?;
                            layout.fixed = element.fixed;
                            for (source, target) in [
                                (&element.references, &mut layout.references),
                                (&element.dynamics, &mut layout.dynamics),
                            ] {
                                if source.is_empty() {
                                    continue;
                                }
                                let capacity = source
                                    .len()
                                    .checked_mul(count as usize)
                                    .ok_or_else(overflow)?;
                                target.try_reserve_exact(capacity).map_err(|_| overflow())?;
                                for index in 0..count {
                                    let offset = index * element.stride;
                                    target.extend(source.iter().map(|lane| offset + lane));
                                }
                            }
                            layout.element = Some((element_type, element, count as usize));
                        }
                        wire::Pointer => {
                            layout.stride = 24;
                            layout.references.push(0);
                            layout.fixed = false;
                        }
                        wire::Slice => {
                            layout.stride = 48;
                            layout.references.push(0);
                            layout.fixed = false;
                        }
                        wire::Interface => {
                            layout.stride = 24;
                            layout.dynamics.push(0);
                            layout.fixed = false;
                        }
                        wire::Map | wire::Waitable | wire::Function => {
                            layout.stride = 8;
                            layout.references.push(0);
                            layout.fixed = false;
                        }
                        _ => {
                            return Err(RuntimeError::new(
                                "invalid_type",
                                "storage layout",
                                "type has no executable storage layout",
                            ));
                        }
                    }
                }
                TypeIdentity::Primitive(_) => {
                    return Err(RuntimeError::new(
                        "invalid_type",
                        "storage layout",
                        "invalid primitive storage type",
                    ));
                }
                TypeIdentity::Named(_) => unreachable!("underlying resolves named types"),
            }
            active.remove(&underlying);
            let layout = Arc::new(layout);
            let mut cache = registry.cache.write().unwrap();
            if cache.lanes.len() < TYPE_CACHE_LIMIT {
                cache.lanes.insert(identity.clone(), layout.clone());
            }
            Ok(layout)
        }
        prepare(self, identity, &mut HashSet::new())
    }

    pub(crate) fn layout_for_type(
        &self,
        identity: &TypeIdentity,
    ) -> Result<Arc<StorageLaneLayout>, RuntimeError> {
        self.storage_lane_layout(identity)
    }

    /// Returns the number of 64-bit numeric lanes used by an aggregate whose
    /// complete representation is statically addressable. Scalar values use
    /// one lane, while complex values use two. A dynamic value has no fixed
    /// width and is deliberately represented by a typed descriptor/box.
    pub(crate) fn storage_fixed_words(
        &self,
        identity: &TypeIdentity,
    ) -> Result<Option<usize>, RuntimeError> {
        if let Some(width) = self.cache.read().unwrap().fixed_words.get(identity) {
            return Ok(*width);
        }
        fn visit(
            registry: &TypeRegistry,
            identity: &TypeIdentity,
            active: &mut HashSet<TypeIdentity>,
        ) -> Result<Option<usize>, RuntimeError> {
            let identity = registry.underlying(identity)?;
            if !active.insert(identity.clone()) {
                return Ok(None);
            }
            let result = match registry.storage_layout(&identity)? {
                Some(StorageTypeLayout::Struct { fields }) => {
                    let mut words = 0usize;
                    for (_, field) in fields.iter() {
                        let Some(width) = visit(registry, field, active)? else {
                            active.remove(&identity);
                            return Ok(None);
                        };
                        words = words.checked_add(width).ok_or_else(|| {
                            RuntimeError::new(
                                "allocation_limit",
                                "storage layout",
                                "fixed aggregate width overflow",
                            )
                        })?;
                    }
                    Some(words)
                }
                Some(StorageTypeLayout::Array { element, length }) => {
                    let Some(width) = visit(registry, &element, active)? else {
                        active.remove(&identity);
                        return Ok(None);
                    };
                    Some(width.checked_mul(length).ok_or_else(|| {
                        RuntimeError::new(
                            "allocation_limit",
                            "storage layout",
                            "fixed aggregate width overflow",
                        )
                    })?)
                }
                Some(StorageTypeLayout::Slice { .. }) | None => match identity {
                    TypeIdentity::Primitive(
                        wire::PrimitiveBool
                        | wire::PrimitiveInt
                        | wire::PrimitiveInt8
                        | wire::PrimitiveInt16
                        | wire::PrimitiveInt32
                        | wire::PrimitiveInt64
                        | wire::PrimitiveUint
                        | wire::PrimitiveUint8
                        | wire::PrimitiveUint16
                        | wire::PrimitiveUint32
                        | wire::PrimitiveUint64
                        | wire::PrimitiveUintptr
                        | wire::PrimitiveFloat32
                        | wire::PrimitiveFloat64,
                    ) => Some(1),
                    TypeIdentity::Primitive(
                        wire::PrimitiveComplex64 | wire::PrimitiveComplex128,
                    ) => Some(2),
                    _ => None,
                },
            };
            active.remove(&identity);
            Ok(result)
        }

        let result = visit(self, identity, &mut HashSet::new())?;
        let mut cache = self.cache.write().unwrap();
        if cache.fixed_words.len() < TYPE_CACHE_LIMIT {
            cache.fixed_words.insert(identity.clone(), result);
        }
        Ok(result)
    }

    /// Returns the guest logical width of a value using the shared storage
    /// contract. This is distinct from the numeric lane width: a bool can
    /// cost one byte, while a string, slice, pointer, interface, or dynamic
    /// value has a descriptor width even when its physical page lane is
    /// rounded or boxed.
    pub(crate) fn storage_logical_bytes(
        &self,
        identity: &TypeIdentity,
    ) -> Result<u64, RuntimeError> {
        if let Some(bytes) = self.cache.read().unwrap().logical_bytes.get(identity) {
            return bytes.ok_or_else(|| {
                RuntimeError::new("type_cycle", "storage layout", "recursive value layout")
            });
        }
        fn visit(
            registry: &TypeRegistry,
            identity: &TypeIdentity,
            active: &mut HashSet<TypeIdentity>,
        ) -> Result<u64, RuntimeError> {
            let identity = registry.underlying(identity)?;
            if !active.insert(identity.clone()) {
                return Err(RuntimeError::new(
                    "type_cycle",
                    "storage layout",
                    "recursive value layout",
                ));
            }
            let result = match &identity {
                TypeIdentity::Void => 0,
                TypeIdentity::Any => wire::STORAGE_DYNAMIC,
                TypeIdentity::Primitive(primitive) => wire::STORAGE_PRIMITIVE_BYTES
                    .get(*primitive as usize)
                    .copied()
                    .unwrap_or(wire::STORAGE_REFERENCE),
                TypeIdentity::Pointer(_) => wire::STORAGE_POINTER,
                TypeIdentity::Slice(_) => wire::STORAGE_SLICE,
                TypeIdentity::Structural { .. } => {
                    let (module, node) = registry.node(&identity)?.ok_or_else(|| {
                        RuntimeError::new(
                            "unknown_type",
                            "storage layout",
                            "missing structural type",
                        )
                    })?;
                    match node.kind {
                        wire::Struct => {
                            let mut bytes = 0u64;
                            for field in node.fields.iter() {
                                bytes = bytes
                                    .checked_add(visit(
                                        registry,
                                        &registry.resolve(module, &field.r#type)?,
                                        active,
                                    )?)
                                    .ok_or_else(|| {
                                        RuntimeError::new(
                                            "allocation_limit",
                                            "storage layout",
                                            "logical width overflows",
                                        )
                                    })?;
                            }
                            bytes
                        }
                        wire::Array => {
                            let element =
                                visit(registry, &registry.resolve(module, &node.elem)?, active)?;
                            element
                                .checked_mul(u64::try_from(node.length).map_err(|_| {
                                    RuntimeError::new(
                                        "allocation_limit",
                                        "storage layout",
                                        "array length overflows",
                                    )
                                })?)
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "allocation_limit",
                                        "storage layout",
                                        "logical width overflows",
                                    )
                                })?
                        }
                        wire::Pointer => wire::STORAGE_POINTER,
                        wire::Slice => wire::STORAGE_SLICE,
                        wire::Interface => wire::STORAGE_DYNAMIC,
                        wire::Map | wire::Waitable | wire::Function => wire::STORAGE_REFERENCE,
                        _ => wire::STORAGE_REFERENCE,
                    }
                }
                TypeIdentity::Named(_) => unreachable!("underlying resolves named types"),
            };
            active.remove(&identity);
            Ok(result)
        }

        let result = visit(self, identity, &mut HashSet::new());
        if let Ok(bytes) = result {
            let mut cache = self.cache.write().unwrap();
            if cache.logical_bytes.len() < TYPE_CACHE_LIMIT {
                cache.logical_bytes.insert(identity.clone(), Some(bytes));
            }
            Ok(bytes)
        } else {
            result
        }
    }

    pub(crate) fn storage_layout(
        &self,
        identity: &TypeIdentity,
    ) -> Result<Option<StorageTypeLayout>, RuntimeError> {
        if let Some(layout) = self.cache.read().unwrap().storage.get(identity) {
            return Ok(layout.clone());
        }
        let Some((module, node)) = self.node(identity)? else {
            let mut cache = self.cache.write().unwrap();
            if cache.storage.len() < TYPE_CACHE_LIMIT {
                cache.storage.insert(identity.clone(), None);
            }
            return Ok(None);
        };
        let layout = match node.kind {
            wire::Struct => Some(StorageTypeLayout::Struct {
                fields: Arc::new(
                    node.fields
                        .iter()
                        .map(|field| Ok((field.name.clone(), self.resolve(module, &field.r#type)?)))
                        .collect::<Result<Vec<_>, RuntimeError>>()?,
                ),
            }),
            wire::Array => Some(StorageTypeLayout::Array {
                element: self.resolve(module, &node.elem)?,
                length: usize::try_from(node.length).map_err(|_| {
                    RuntimeError::new("invalid_type", "storage layout", "array length overflows")
                })?,
            }),
            wire::Slice => Some(StorageTypeLayout::Slice {
                _element: self.resolve(module, &node.elem)?,
            }),
            _ => None,
        };
        let mut cache = self.cache.write().unwrap();
        if cache.storage.len() < TYPE_CACHE_LIMIT {
            cache.storage.insert(identity.clone(), layout.clone());
        }
        Ok(layout)
    }

    pub(crate) fn cached_zero(
        &self,
        typ: &TypeIdentity,
        depth: usize,
        elements: usize,
    ) -> Option<(crate::value::Value, u64)> {
        self.cache
            .read()
            .unwrap()
            .zeros
            .get(&(typ.clone(), depth, elements))
            .map(|template| (template.value.clone(), template.construction_bytes))
    }

    pub(crate) fn cache_zero(
        &self,
        value: &crate::value::Value,
        depth: usize,
        elements: usize,
        construction_bytes: u64,
    ) {
        let mut cache = self.cache.write().unwrap();
        // Zero layouts contain no guest handles. Their budget and the entry
        // count bound metadata retention independently of an instance heap.
        let bytes = construction_bytes.saturating_add(16);
        if cache.zeros.len() >= 256 || bytes > (1 << 20) - cache.zero_bytes {
            return;
        }
        if let std::collections::hash_map::Entry::Vacant(entry) =
            cache.zeros.entry((value.typ.clone(), depth, elements))
        {
            entry.insert(ZeroTemplate {
                value: value.clone(),
                construction_bytes,
            });
            cache.zero_bytes += bytes;
        }
    }
    pub(crate) fn use_context(&mut self, source: &Self) {
        if !Arc::ptr_eq(&self.node_aliases, &source.node_aliases) {
            self.node_aliases = source.node_aliases.clone();
            self.cache = source.cache.clone();
        }
    }
    pub(crate) fn overlay(&self, next: &Self) -> Result<Self, RuntimeError> {
        for (key, node) in next.nodes.iter() {
            if let Some(old) = self.nodes.get(key)
                && crate::contract::canonical_json(old)? != crate::contract::canonical_json(node)?
            {
                return Err(RuntimeError::new(
                    "type_shape_changed",
                    key.0.as_ref(),
                    format!("type node {} changed shape", key.1),
                ));
            }
        }
        for identity in self.named.keys() {
            if !next.named.contains_key(identity) {
                return Err(RuntimeError::new(
                    "type_shape_changed",
                    &identity.module_path,
                    "named declaration was removed or rebound",
                ));
            }
        }
        let mut merged = self.clone();
        Arc::make_mut(&mut merged.nodes).extend(
            next.nodes
                .iter()
                .map(|(key, node)| (key.clone(), node.clone())),
        );
        Arc::make_mut(&mut merged.named).extend(
            next.named
                .iter()
                .map(|(key, node)| (key.clone(), node.clone())),
        );
        merged.use_context(next);
        for (identity, old) in self.named.iter() {
            let new = &next.named[identity];
            let old = &self.nodes[old];
            let new = &next.nodes[new];
            let module = &identity.module_path;
            if old.alias != new.alias
                || old.methods.len() != new.methods.len()
                || !merged.identical(
                    &self.resolve(
                        module,
                        if old.alias {
                            &old.alias_target
                        } else {
                            &old.underlying
                        },
                    )?,
                    &next.resolve(
                        module,
                        if new.alias {
                            &new.alias_target
                        } else {
                            &new.underlying
                        },
                    )?,
                )?
            {
                return Err(RuntimeError::new(
                    "type_shape_changed",
                    module,
                    "named declaration changed shape",
                ));
            }
            for (old_method, new_method) in old.methods.iter().zip(new.methods.iter()) {
                if old_method.name != new_method.name
                    || old_method.function_id != new_method.function_id
                    || old_method.module_path != new_method.module_path
                    || !merged.identical(
                        &self.resolve(module, &old_method.receiver)?,
                        &next.resolve(module, &new_method.receiver)?,
                    )?
                    || !merged.signature_identical_from(
                        self,
                        module,
                        &old_method.signature,
                        next,
                        module,
                        &new_method.signature,
                    )?
                {
                    return Err(RuntimeError::new(
                        "type_shape_changed",
                        module,
                        "named declaration method changed shape",
                    ));
                }
            }
        }
        Ok(merged)
    }
    pub fn signature_identical(
        &self,
        left_module: &str,
        left: &wire::FunctionSignature,
        right_module: &str,
        right: &wire::FunctionSignature,
    ) -> Result<bool, RuntimeError> {
        self.signature_identical_from(self, left_module, left, self, right_module, right)
    }

    pub(crate) fn signature_identical_from(
        &self,
        left_registry: &Self,
        left_module: &str,
        left: &wire::FunctionSignature,
        right_registry: &Self,
        right_module: &str,
        right: &wire::FunctionSignature,
    ) -> Result<bool, RuntimeError> {
        if left.params.len() != right.params.len()
            || left.results.len() != right.results.len()
            || left.variadic != right.variadic
        {
            return Ok(false);
        }
        for (left, right) in left
            .params
            .iter()
            .map(|parameter| &parameter.r#type)
            .chain(left.results.iter())
            .zip(
                right
                    .params
                    .iter()
                    .map(|parameter| &parameter.r#type)
                    .chain(right.results.iter()),
            )
        {
            if !self.identical(
                &left_registry.resolve(left_module, left)?,
                &right_registry.resolve(right_module, right)?,
            )? {
                return Ok(false);
            }
        }
        Ok(true)
    }

    pub fn implements(
        &self,
        source: &TypeIdentity,
        target: &TypeIdentity,
    ) -> Result<bool, RuntimeError> {
        if self.underlying(target)? == TypeIdentity::Any {
            return Ok(true);
        }
        let error_method = || wire::Method {
            name: "Error".to_owned(),
            signature: wire::FunctionSignature {
                results: crate::contract::GoSlice(Some(vec![wire::TypeRef {
                    kind: wire::Primitive,
                    primitive: wire::PrimitiveString,
                    ..Default::default()
                }])),
                ..Default::default()
            },
            ..Default::default()
        };
        let required = if *target == TypeIdentity::Primitive(wire::PrimitiveError) {
            vec![(String::new(), error_method())]
        } else if let Some((_, node)) = self.node(target)?
            && node.kind == wire::Interface
        {
            self.declared_methods(target)?
        } else {
            return Ok(false);
        };
        for (target_module, expected) in required {
            let actual = if *source == TypeIdentity::Primitive(wire::PrimitiveError)
                && expected.name == "Error"
            {
                Some((String::new(), error_method()))
            } else if let Some((_, node)) = self.node(source)?
                && node.kind == wire::Interface
            {
                self.declared_methods(source)?
                    .into_iter()
                    .find(|(_, method)| method.name == expected.name)
            } else {
                self.method(source, &expected.name)?
            };
            let Some((source_module, actual)) = actual else {
                return Ok(false);
            };
            if !crate::types::names::is_exported(&expected.name) && source_module != target_module {
                return Ok(false);
            }
            if !self.signature_identical(
                &source_module,
                &actual.signature,
                &target_module,
                &expected.signature,
            )? {
                return Ok(false);
            }
        }
        Ok(true)
    }

    pub fn is_interface(&self, typ: &TypeIdentity) -> Result<bool, RuntimeError> {
        Ok(matches!(
            self.underlying(typ)?,
            TypeIdentity::Any | TypeIdentity::Primitive(wire::PrimitiveError)
        ) || self
            .node(typ)?
            .is_some_and(|(_, node)| node.kind == wire::Interface))
    }

    pub fn nil_assignable(&self, typ: &TypeIdentity) -> Result<bool, RuntimeError> {
        Ok(self.is_interface(typ)?
            || matches!(
                self.underlying(typ)?,
                TypeIdentity::Pointer(_)
                    | TypeIdentity::Slice(_)
                    | TypeIdentity::Primitive(wire::PrimitiveFunction)
            )
            || self.node(typ)?.is_some_and(|(_, node)| {
                matches!(
                    node.kind,
                    wire::Pointer | wire::Slice | wire::Map | wire::Function | wire::Waitable
                )
            }))
    }

    /// Resolves both runtime pointer identities and artifact pointer nodes.
    pub(crate) fn pointer_element(
        &self,
        typ: &TypeIdentity,
    ) -> Result<Option<TypeIdentity>, RuntimeError> {
        if let TypeIdentity::Pointer(element) = self.underlying(typ)? {
            return Ok(Some((*element).clone()));
        }
        self.node(typ)?
            .filter(|(_, node)| node.kind == wire::Pointer)
            .map(|(module, node)| self.resolve(module, &node.elem))
            .transpose()
    }

    pub fn channel_assignable(
        &self,
        source: &TypeIdentity,
        target: &TypeIdentity,
    ) -> Result<bool, RuntimeError> {
        let (Some((source_module, source_node)), Some((target_module, target_node))) =
            (self.node(source)?, self.node(target)?)
        else {
            return Ok(false);
        };
        Ok(source_node.kind == wire::Waitable
            && target_node.kind == wire::Waitable
            && source_node.direction == wire::ChannelBoth
            && (!matches!(source, TypeIdentity::Named(_))
                || !matches!(target, TypeIdentity::Named(_)))
            && self.identical(
                &self.resolve(source_module, &source_node.elem)?,
                &self.resolve(target_module, &target_node.elem)?,
            )?)
    }

    pub fn comparable(&self, typ: &TypeIdentity) -> Result<bool, RuntimeError> {
        let mut pending = vec![typ.clone()];
        let mut seen = HashSet::new();
        while let Some(typ) = pending.pop() {
            let typ = self.underlying(&typ)?;
            if !seen.insert(typ.clone()) {
                continue;
            }
            match typ {
                TypeIdentity::Void
                | TypeIdentity::Slice(_)
                | TypeIdentity::Primitive(wire::PrimitiveFunction) => return Ok(false),
                TypeIdentity::Structural { .. } => {
                    let (module, node) = self.node(&typ)?.unwrap();
                    match node.kind {
                        wire::Slice | wire::Map | wire::Function => return Ok(false),
                        wire::Array => pending.push(self.resolve(module, &node.elem)?),
                        wire::Struct => {
                            for field in node.fields.iter() {
                                pending.push(self.resolve(module, &field.r#type)?);
                            }
                        }
                        _ => {}
                    }
                }
                _ => {}
            }
        }
        Ok(true)
    }
    pub fn method(
        &self,
        typ: &TypeIdentity,
        name: &str,
    ) -> Result<Option<(String, wire::Method)>, RuntimeError> {
        let mut typ = typ.clone();
        let mut pointer = false;
        let mut visited = HashSet::new();
        while visited.insert(typ.clone()) {
            if let Some((module, node)) = self.declaration(&typ) {
                if let Some(method) = node.methods.iter().find(|method| method.name == name) {
                    if method.receiver.kind == wire::Pointer && !pointer {
                        return Ok(None);
                    }
                    return Ok(Some((module.to_owned(), method.clone())));
                }
                if node.kind == wire::Named && node.alias {
                    typ = self.resolve(module, &node.alias_target)?;
                    continue;
                }
            }
            if let Some(element) = self.pointer_element(&typ)? {
                typ = element;
                pointer = true;
            } else {
                break;
            }
        }
        Ok(None)
    }

    pub fn declared_methods(
        &self,
        typ: &TypeIdentity,
    ) -> Result<Vec<(String, wire::Method)>, RuntimeError> {
        if let Some((module, node)) = self.declaration(typ)
            && node.kind == wire::Named
            && !node.methods.is_empty()
        {
            let mut methods: Vec<_> = node
                .methods
                .iter()
                .map(|method| (module.to_owned(), method.clone()))
                .collect();
            methods.sort_by(|left, right| left.1.name.cmp(&right.1.name));
            return Ok(methods);
        }
        if let Some((module, node)) = self.node(typ)?
            && node.kind == wire::Interface
        {
            let mut methods: Vec<_> = node
                .methods
                .iter()
                .map(|method| (module.to_owned(), method.clone()))
                .collect();
            methods.sort_by(|left, right| left.1.name.cmp(&right.1.name));
            return Ok(methods);
        }
        let mut current = typ.clone();
        let mut methods = Vec::new();
        let mut seen = HashSet::new();
        while seen.insert(current.clone()) {
            if let Some((module, node)) = self.declaration(&current) {
                methods.extend(
                    node.methods
                        .iter()
                        .map(|method| (module.to_owned(), method.clone())),
                );
                if node.kind == wire::Named && node.alias {
                    current = self.resolve(module, &node.alias_target)?;
                    continue;
                }
            }
            if let Some(element) = self.pointer_element(&current)? {
                current = element;
            } else {
                break;
            }
        }
        methods.sort_by(|left, right| left.1.name.cmp(&right.1.name));
        Ok(methods)
    }
    pub fn declaration(&self, identity: &TypeIdentity) -> Option<(&str, &wire::TypeNode)> {
        let key = match identity {
            TypeIdentity::Named(key) => self.named.get(key)?,
            TypeIdentity::Structural { module, node } => {
                let key = (module.clone(), node.clone());
                let (key, value) = self
                    .nodes
                    .get_key_value(&key)
                    .or_else(|| self.dynamic_nodes.get_key_value(&key))?;
                return Some((&key.0, value));
            }
            _ => return None,
        };
        Some((&key.0, &self.nodes[key]))
    }

    pub fn identical(
        &self,
        left: &TypeIdentity,
        right: &TypeIdentity,
    ) -> Result<bool, RuntimeError> {
        self.identical_with_tags(left, right, false)
    }

    pub(crate) fn conversion_underlying_identical(
        &self,
        left: &TypeIdentity,
        right: &TypeIdentity,
    ) -> Result<bool, RuntimeError> {
        self.identical_with_tags(&self.underlying(left)?, &self.underlying(right)?, true)
    }

    fn identical_with_tags(
        &self,
        left: &TypeIdentity,
        right: &TypeIdentity,
        ignore_tags: bool,
    ) -> Result<bool, RuntimeError> {
        if left == right {
            return Ok(true);
        }
        if matches!(
            left,
            TypeIdentity::Void | TypeIdentity::Any | TypeIdentity::Primitive(_)
        ) && matches!(
            right,
            TypeIdentity::Void | TypeIdentity::Any | TypeIdentity::Primitive(_)
        ) {
            return Ok(false);
        }
        // A registry cache belongs to immutable program metadata. Dynamic
        // constructions remain instance-owned and must not enter that cache.
        let static_identity = |mut typ: &TypeIdentity| loop {
            match typ {
                TypeIdentity::Pointer(element) | TypeIdentity::Slice(element) => typ = element,
                TypeIdentity::Structural { module, node } => {
                    break self.nodes.contains_key(&(module.clone(), node.clone()));
                }
                TypeIdentity::Named(key) => break self.named.contains_key(key.as_ref()),
                _ => break true,
            }
        };
        let key = (static_identity(left) && static_identity(right))
            .then(|| (left.clone(), right.clone(), ignore_tags));
        if let Some(key) = &key
            && let Some(result) = self.cache.read().unwrap().identical.get(key)
        {
            return Ok(*result);
        }
        let result = self.compare_type_graphs(left, right, ignore_tags)?;
        if let Some(key) = key {
            let mut cache = self.cache.write().unwrap();
            if cache.identical.len() < 4096 {
                cache.identical.insert(key, result);
            }
        }
        Ok(result)
    }

    fn compare_type_graphs(
        &self,
        left: &TypeIdentity,
        right: &TypeIdentity,
        ignore_tags: bool,
    ) -> Result<bool, RuntimeError> {
        let mut pending = vec![(left.clone(), right.clone())];
        let mut seen = HashSet::new();
        while let Some((left, right)) = pending.pop() {
            if left == right || !seen.insert((left.clone(), right.clone())) {
                continue;
            }
            let left_alias = self
                .declaration(&left)
                .filter(|(_, node)| node.kind == wire::Named && node.alias);
            let right_alias = self
                .declaration(&right)
                .filter(|(_, node)| node.kind == wire::Named && node.alias);
            if left_alias.is_some() || right_alias.is_some() {
                pending.push((
                    match left_alias {
                        Some((module, node)) => self.resolve(module, &node.alias_target)?,
                        None => left,
                    },
                    match right_alias {
                        Some((module, node)) => self.resolve(module, &node.alias_target)?,
                        None => right,
                    },
                ));
                continue;
            }
            if matches!(left, TypeIdentity::Named(_)) || matches!(right, TypeIdentity::Named(_)) {
                return Ok(false);
            }
            let (dynamic, other, kind) = match (&left, &right) {
                (TypeIdentity::Pointer(a), TypeIdentity::Pointer(b))
                | (TypeIdentity::Slice(a), TypeIdentity::Slice(b)) => {
                    pending.push(((**a).clone(), (**b).clone()));
                    continue;
                }
                (TypeIdentity::Pointer(element), other)
                | (other, TypeIdentity::Pointer(element)) => (Some(element), other, wire::Pointer),
                (TypeIdentity::Slice(element), other) | (other, TypeIdentity::Slice(element)) => {
                    (Some(element), other, wire::Slice)
                }
                _ => (None, &right, wire::Invalid),
            };
            if let Some(element) = dynamic {
                let Some((module, node)) = self.node(other)? else {
                    return Ok(false);
                };
                if node.kind != kind {
                    return Ok(false);
                }
                pending.push(((**element).clone(), self.resolve(module, &node.elem)?));
                continue;
            }
            let (Some((left_module, a)), Some((right_module, b))) =
                (self.node(&left)?, self.node(&right)?)
            else {
                return Ok(false);
            };
            if a.kind != b.kind
                || a.length != b.length
                || a.direction != b.direction
                || a.fields.len() != b.fields.len()
                || a.tuple.len() != b.tuple.len()
                || a.methods.len() != b.methods.len()
            {
                return Ok(false);
            }
            if matches!(
                a.kind,
                wire::Slice | wire::Array | wire::Pointer | wire::Map | wire::Waitable
            ) {
                pending.push((
                    self.resolve(left_module, &a.elem)?,
                    self.resolve(right_module, &b.elem)?,
                ));
            }
            if a.kind == wire::Map {
                pending.push((
                    self.resolve(left_module, &a.key)?,
                    self.resolve(right_module, &b.key)?,
                ));
            }
            for (a, b) in a.fields.iter().zip(b.fields.iter()) {
                if a.name != b.name || (!ignore_tags && a.tag != b.tag) || a.embedded != b.embedded
                {
                    return Ok(false);
                }
                pending.push((
                    self.resolve(left_module, &a.r#type)?,
                    self.resolve(right_module, &b.r#type)?,
                ));
            }
            for (a, b) in a.tuple.iter().zip(b.tuple.iter()) {
                pending.push((
                    self.resolve(left_module, a)?,
                    self.resolve(right_module, b)?,
                ));
            }
            let signatures = a
                .signature
                .iter()
                .map(|signature| &**signature)
                .zip(b.signature.iter().map(|signature| &**signature));
            if a.signature.is_some() != b.signature.is_some() {
                return Ok(false);
            }
            for (a, b) in signatures.chain(
                a.methods
                    .iter()
                    .zip(b.methods.iter())
                    .map(|(a, b)| (&a.signature, &b.signature)),
            ) {
                if a.variadic != b.variadic
                    || a.params.len() != b.params.len()
                    || a.results.len() != b.results.len()
                {
                    return Ok(false);
                }
                for (a, b) in a.params.iter().zip(b.params.iter()) {
                    pending.push((
                        self.resolve(left_module, &a.r#type)?,
                        self.resolve(right_module, &b.r#type)?,
                    ));
                }
                for (a, b) in a.results.iter().zip(b.results.iter()) {
                    pending.push((
                        self.resolve(left_module, a)?,
                        self.resolve(right_module, b)?,
                    ));
                }
            }
            if a.methods
                .iter()
                .zip(b.methods.iter())
                .any(|(a, b)| a.name != b.name)
            {
                return Ok(false);
            }
        }
        Ok(true)
    }
    pub fn node(
        &self,
        identity: &TypeIdentity,
    ) -> Result<Option<(&str, &wire::TypeNode)>, RuntimeError> {
        let identity = self.underlying(identity)?;
        match identity {
            TypeIdentity::Structural { module, node } => {
                let key = (module, node);
                let (key, value) = self
                    .nodes
                    .get_key_value(&key)
                    .or_else(|| self.dynamic_nodes.get_key_value(&key))
                    .ok_or_else(|| {
                        RuntimeError::new("unknown_type", "type", "missing structural node")
                    })?;
                Ok(Some((&key.0, value)))
            }
            _ => Ok(None),
        }
    }
    /// Copies only type metadata, then validates references against the complete
    /// module closure. Named definitions can reference later dependencies.
    pub fn new<'a>(
        artifacts: impl IntoIterator<Item = &'a wire::Artifact>,
    ) -> Result<Self, RuntimeError> {
        let mut registry = Self {
            cache: Arc::default(),
            storage_ids: Arc::default(),
            nodes: Arc::new(HashMap::new()),
            named: Arc::new(HashMap::new()),
            node_aliases: Arc::new(HashMap::new()),
            dynamic_nodes: Arc::default(),
            dynamic_refs: Arc::default(),
            dynamic_module: String::new(),
            next_dynamic_sequence: 0,
            next_imported_sequence: 0,
            registered_dynamic: Arc::default(),
            dynamic_costs: Arc::default(),
            dynamic_shapes: Arc::default(),
            dynamic_bytes: 0,
        };
        for artifact in artifacts {
            let module = &artifact.module.path;
            let namespace = crate::contract::canonical_hash(&artifact.type_table)?;
            for original in artifact.type_table.nodes.iter() {
                let mut node = original.clone();
                let qualify = |reference: &mut wire::TypeRef| {
                    if !reference.node.is_empty() {
                        reference.node = format!("{namespace}/{}", reference.node);
                    }
                };
                Arc::make_mut(&mut registry.node_aliases).insert(
                    (module.clone(), node.id.clone()),
                    format!("{namespace}/{}", node.id),
                );
                if node.id.is_empty() {
                    return Err(RuntimeError::new("invalid_type", module, "empty type node"));
                }
                node.id = format!("{namespace}/{}", node.id);
                for reference in [
                    &mut node.alias_target,
                    &mut node.underlying,
                    &mut node.elem,
                    &mut node.key,
                    &mut node.constraint,
                    &mut node.base,
                ] {
                    qualify(reference);
                }
                for reference in node.tuple.iter_mut().chain(node.type_args.iter_mut()) {
                    qualify(reference);
                }
                for field in node.fields.iter_mut() {
                    qualify(&mut field.r#type);
                }
                for term in node.terms.iter_mut() {
                    qualify(&mut term.r#type);
                }
                for method in node.methods.iter_mut() {
                    qualify(&mut method.receiver);
                }
                for signature in node
                    .signature
                    .iter_mut()
                    .map(|signature| &mut **signature)
                    .chain(node.methods.iter_mut().map(|method| &mut method.signature))
                {
                    for parameter in signature.params.iter_mut() {
                        qualify(&mut parameter.r#type);
                    }
                    for reference in signature.results.iter_mut() {
                        qualify(reference);
                    }
                }
                let key = (
                    Arc::<str>::from(module.as_str()),
                    Arc::<str>::from(node.id.as_str()),
                );
                if node.id.is_empty()
                    || Arc::make_mut(&mut registry.nodes)
                        .insert(key.clone(), node.clone())
                        .is_some()
                {
                    return Err(RuntimeError::new(
                        "invalid_type",
                        module,
                        "empty or duplicate type node",
                    ));
                }
                if node.kind == wire::Named {
                    if node.identity.module_path.is_empty() || node.identity.decl_id.is_empty() {
                        return Err(RuntimeError::new(
                            "invalid_type",
                            &node.id,
                            "named definition has invalid owner",
                        ));
                    }
                    // Importers carry local metadata copies. The defining
                    // module alone owns the global named identity.
                    if node.identity.module_path == *module
                        && Arc::make_mut(&mut registry.named)
                            .insert(node.identity.clone(), key)
                            .is_some()
                    {
                        return Err(RuntimeError::new(
                            "invalid_type",
                            &node.id,
                            "duplicate named identity",
                        ));
                    }
                }
            }
        }
        for ((module, _), node) in registry.nodes.iter() {
            registry.validate_node(module, node)?;
        }
        Ok(registry)
    }

    pub fn resolve(
        &self,
        module: &str,
        reference: &wire::TypeRef,
    ) -> Result<TypeIdentity, RuntimeError> {
        if reference.kind <= wire::Primitive || module == self.dynamic_module {
            return self.resolve_ref(module, reference);
        }
        if let Some(identity) = self
            .cache
            .read()
            .unwrap()
            .resolved
            .get(module)
            .and_then(|types| types.get(reference))
        {
            return Ok(identity.clone());
        }
        let identity = self.resolve_ref(module, reference)?;
        let mut cache = self.cache.write().unwrap();
        if cache.resolved_count < TYPE_CACHE_LIMIT
            && cache
                .resolved
                .entry(module.to_owned())
                .or_default()
                .insert(reference.clone(), identity.clone())
                .is_none()
        {
            cache.resolved_count += 1;
        }
        Ok(identity)
    }

    fn resolve_ref(
        &self,
        module: &str,
        reference: &wire::TypeRef,
    ) -> Result<TypeIdentity, RuntimeError> {
        let empty_named =
            reference.named.module_path.is_empty() && reference.named.decl_id.is_empty();
        if reference.kind == wire::Named {
            if reference.named.module_path.is_empty() != reference.named.decl_id.is_empty()
                || reference.primitive != 0
            {
                return Err(RuntimeError::new(
                    "invalid_type",
                    module,
                    "malformed named reference",
                ));
            }
            let identity = if empty_named {
                self.node_by_ref(module, reference)?.identity.clone()
            } else {
                reference.named.clone()
            };
            if !self.named.contains_key(&identity) {
                return Err(RuntimeError::new(
                    "unknown_type",
                    module,
                    format!(
                        "unresolved named type {}.{}",
                        identity.module_path, identity.decl_id
                    ),
                ));
            }
            if !reference.node.is_empty()
                && self.node_by_ref(module, reference)?.identity != identity
            {
                return Err(RuntimeError::new(
                    "invalid_type",
                    module,
                    "named node and identity disagree",
                ));
            }
            return Ok(TypeIdentity::Named(identity.into()));
        }
        if !empty_named {
            return Err(RuntimeError::new(
                "invalid_type",
                module,
                "non-named reference contains named identity",
            ));
        }
        match reference.kind {
            wire::Void | wire::Any if reference.primitive == 0 && reference.node.is_empty() => {
                Ok(if reference.kind == wire::Void {
                    TypeIdentity::Void
                } else {
                    TypeIdentity::Any
                })
            }
            wire::Primitive
                if reference.primitive > wire::PrimitiveInvalid
                    && reference.primitive <= wire::PrimitiveFunction
                    && reference.node.is_empty() =>
            {
                Ok(TypeIdentity::Primitive(reference.primitive))
            }
            wire::Slice
            | wire::Array
            | wire::Map
            | wire::Pointer
            | wire::Waitable
            | wire::Function
            | wire::Tuple
            | wire::Struct
            | wire::Interface
                if reference.primitive == 0 =>
            {
                self.node_by_ref(module, reference)?;
                Ok(TypeIdentity::Structural {
                    module: module.into(),
                    node: self.node_by_ref(module, reference)?.id.clone().into(),
                })
            }
            _ => Err(RuntimeError::new(
                "invalid_type",
                module,
                format!(
                    "invalid or unresolved runtime type reference: kind={} primitive={} node={:?} named={:?}",
                    reference.kind, reference.primitive, reference.node, reference.named
                ),
            )),
        }
    }

    fn node_by_ref(
        &self,
        module: &str,
        reference: &wire::TypeRef,
    ) -> Result<&wire::TypeNode, RuntimeError> {
        let key = (module.to_owned(), reference.node.clone());
        let node = self.node_aliases.get(&key).unwrap_or(&reference.node);
        self.nodes
            .get(&(module.into(), node.as_str().into()))
            .or_else(|| {
                self.dynamic_nodes
                    .get(&(module.into(), reference.node.as_str().into()))
            })
            .filter(|node| node.kind == reference.kind)
            .ok_or_else(|| {
                RuntimeError::new(
                    "unknown_type",
                    module,
                    format!("missing or mismatched type node {}", reference.node),
                )
            })
    }

    pub fn underlying(&self, identity: &TypeIdentity) -> Result<TypeIdentity, RuntimeError> {
        if !matches!(identity, TypeIdentity::Named(_)) {
            return Ok(identity.clone());
        }
        if let Some(underlying) = self.cache.read().unwrap().underlying.get(identity) {
            return Ok(underlying.clone());
        }
        let mut current = identity.clone();
        let mut visited = HashSet::new();
        while let TypeIdentity::Named(key) = &current {
            if !visited.insert(key.clone()) {
                return Err(RuntimeError::new(
                    "type_cycle",
                    "type",
                    "named underlying cycle",
                ));
            }
            let (module, node_id) = self.named.get(key).ok_or_else(|| {
                RuntimeError::new("unknown_type", "type", "missing named definition")
            })?;
            let node = &self.nodes[&(module.clone(), node_id.clone())];
            current = self.resolve(
                module,
                if node.alias {
                    &node.alias_target
                } else {
                    &node.underlying
                },
            )?;
        }
        let mut cache = self.cache.write().unwrap();
        if cache.underlying.len() < TYPE_CACHE_LIMIT {
            cache.underlying.insert(identity.clone(), current.clone());
        }
        Ok(current)
    }

    fn validate_node(&self, module: &str, node: &wire::TypeNode) -> Result<(), RuntimeError> {
        if node.constraint.kind != wire::Invalid
            || node.base.kind != wire::Invalid
            || !node.type_args.is_empty()
        {
            return Err(RuntimeError::new(
                "invalid_type",
                &node.id,
                "unresolved compiler type metadata",
            ));
        }
        match node.kind {
            wire::Named => {
                self.underlying(&TypeIdentity::Named(node.identity.clone().into()))?;
            }
            wire::Slice | wire::Pointer | wire::Waitable | wire::Array => {
                self.resolve(module, &node.elem)?;
                if node.kind == wire::Array && node.length < 0 {
                    return Err(RuntimeError::new(
                        "invalid_type",
                        &node.id,
                        "negative array length",
                    ));
                }
                if node.kind == wire::Waitable
                    && !(wire::ChannelBoth..=wire::ChannelSend).contains(&node.direction)
                {
                    return Err(RuntimeError::new(
                        "invalid_type",
                        &node.id,
                        "invalid channel direction",
                    ));
                }
            }
            wire::Map => {
                self.resolve(module, &node.key)?;
                self.resolve(module, &node.elem)?;
            }
            wire::Struct => {
                let mut names = HashSet::new();
                for field in node.fields.iter() {
                    if field.name.is_empty() || (field.name != "_" && !names.insert(&field.name)) {
                        return Err(RuntimeError::new(
                            "invalid_type",
                            &node.id,
                            "empty or duplicate struct field",
                        ));
                    }
                    self.resolve(module, &field.r#type)?;
                }
            }
            wire::Tuple => {
                for element in node.tuple.iter() {
                    self.resolve(module, element)?;
                }
            }
            wire::Function => {
                let signature = node.signature.as_ref().ok_or_else(|| {
                    RuntimeError::new("invalid_type", &node.id, "missing function signature")
                })?;
                self.validate_signature(module, signature)?;
            }
            wire::Interface => {}
            _ => {
                return Err(RuntimeError::new(
                    "invalid_type",
                    &node.id,
                    "unsupported runtime node kind",
                ));
            }
        }
        for method in node.methods.iter() {
            self.validate_signature(module, &method.signature)?;
            if method.receiver.kind != wire::Invalid {
                self.resolve(module, &method.receiver)?;
            }
        }
        Ok(())
    }

    pub fn validate_signature(
        &self,
        module: &str,
        signature: &wire::FunctionSignature,
    ) -> Result<(), RuntimeError> {
        for parameter in signature.params.iter() {
            self.resolve(module, &parameter.r#type)?;
        }
        for result in signature.results.iter() {
            self.resolve(module, result)?;
        }
        if signature.variadic {
            let parameter = signature.params.last().ok_or_else(|| {
                RuntimeError::new(
                    "invalid_type",
                    module,
                    "variadic function has no parameters",
                )
            })?;
            let underlying = self.underlying(&self.resolve(module, &parameter.r#type)?)?;
            let is_slice = match underlying {
                TypeIdentity::Structural { module, node } => self
                    .node_by_ref(
                        &module,
                        &wire::TypeRef {
                            kind: wire::Slice,
                            node: node.to_string(),
                            ..Default::default()
                        },
                    )
                    .is_ok(),
                TypeIdentity::Slice(_) => true,
                _ => false,
            };
            if !is_slice {
                return Err(RuntimeError::new(
                    "invalid_type",
                    module,
                    "variadic parameter is not a slice",
                ));
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod storage_layout_tests {
    use super::*;

    #[test]
    fn dynamic_storage_ids_survive_revision_overlays_and_expire_with_their_type_owner() {
        let mut types = TypeRegistry::new([]).unwrap();
        let scalar = TypeIdentity::Primitive(wire::PrimitiveInt);
        let scalar_id = types.storage_type_id(&scalar).unwrap();
        let aggregate = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: scalar.clone(),
                },
                128,
                32,
            )
            .unwrap();
        types
            .register_dynamic(&aggregate, 128, 1 << 20, 32)
            .unwrap();
        let aggregate_id = types.storage_type_id(&aggregate).unwrap();
        assert_ne!(aggregate_id, scalar_id);
        assert_ne!(aggregate_id, 0);
        let pointer = TypeIdentity::Pointer(Arc::new(aggregate.clone()));
        types.register_dynamic(&pointer, 128, 1 << 20, 32).unwrap();
        let pointer_id = types.storage_type_id(&pointer).unwrap();
        let mut overlay = types.overlay(&TypeRegistry::new([]).unwrap()).unwrap();
        assert_eq!(
            overlay.identity_for_storage_type_id(aggregate_id),
            Some(aggregate.clone())
        );
        assert_eq!(overlay.storage_type_id(&aggregate).unwrap(), aggregate_id);
        overlay.prune_dynamic(std::slice::from_ref(&pointer));
        assert_eq!(
            overlay.identity_for_storage_type_id(pointer_id),
            Some(pointer)
        );
        assert_eq!(overlay.storage_logical_bytes(&aggregate).unwrap(), 16);
        overlay.prune_dynamic(&[]);
        assert_eq!(overlay.identity_for_storage_type_id(aggregate_id), None);
        assert_eq!(overlay.identity_for_storage_type_id(pointer_id), None);
        assert_eq!(
            overlay.identity_for_storage_type_id(scalar_id),
            Some(scalar)
        );
        let next = overlay
            .storage_type_id(&TypeIdentity::Primitive(wire::PrimitiveBool))
            .unwrap();
        assert!(next > pointer_id);
        let mut independent = overlay.clone();
        independent.fork_storage_catalog();
        assert_eq!(independent.identity_for_storage_type_id(next), None);
        assert!(overlay.identity_for_storage_type_id(next).is_some());
    }

    #[test]
    fn nested_layouts_inline_descriptors_and_trace_their_actual_offsets() {
        let mut types = TypeRegistry::new([]).unwrap();
        let inner = types
            .construct(
                ConstructedType::Struct(vec![
                    ConstructedField {
                        name: "Flag".into(),
                        typ: TypeIdentity::Primitive(wire::PrimitiveBool),
                        tag: String::new(),
                        embedded: false,
                    },
                    ConstructedField {
                        name: "Text".into(),
                        typ: TypeIdentity::Primitive(wire::PrimitiveString),
                        tag: String::new(),
                        embedded: false,
                    },
                ]),
                128,
                32,
            )
            .unwrap();
        let rows = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: inner,
                },
                128,
                32,
            )
            .unwrap();
        let aggregate = types
            .construct(
                ConstructedType::Struct(vec![
                    ConstructedField {
                        name: "Head".into(),
                        typ: TypeIdentity::Pointer(Arc::new(TypeIdentity::Primitive(
                            wire::PrimitiveInt,
                        ))),
                        tag: String::new(),
                        embedded: false,
                    },
                    ConstructedField {
                        name: "Rows".into(),
                        typ: rows,
                        tag: String::new(),
                        embedded: false,
                    },
                    ConstructedField {
                        name: "Value".into(),
                        typ: TypeIdentity::Any,
                        tag: String::new(),
                        embedded: false,
                    },
                ]),
                128,
                32,
            )
            .unwrap();
        let layout = types.storage_lane_layout(&aggregate).unwrap();
        assert_eq!(layout.stride, 112);
        assert_eq!(layout.references, [0, 32, 64]);
        assert_eq!(layout.dynamics, [88]);
        assert!(!layout.fixed);
        assert_eq!(types.storage_logical_bytes(&aggregate).unwrap(), 98);

        // The collector consumes the prepared offsets directly, independently
        // of any Value decoder or root graph reconstructed by the codec.
        let mut heap = crate::managed_storage::StorageHeap::new(None, None, 4 << 20);
        let child_layout = heap
            .register_layout(crate::managed_storage::StorageLayout {
                stride: 8,
                references: Vec::new(),
                dynamics: Vec::new(),
            })
            .unwrap();
        let child = heap.reserve(child_layout, 1, 8).unwrap().commit();
        let record_layout = heap
            .register_layout(crate::managed_storage::StorageLayout {
                stride: layout.stride,
                references: layout.references.clone(),
                dynamics: layout.dynamics.clone(),
            })
            .unwrap();
        let record = heap.reserve(record_layout, 1, 98).unwrap().commit();
        heap.write_reference(record, 64, child).unwrap();
        heap.collect_major([record]).unwrap();
        assert_eq!(heap.read_reference(record, 64).unwrap(), child);
        heap.write_reference(record, 64, crate::managed_storage::StorageRef::NULL)
            .unwrap();
        heap.collect_major([record]).unwrap();
        assert!(heap.read(child, 0, 8).is_err());
    }

    #[test]
    fn container_and_function_lanes_are_references_and_interfaces_are_dynamic() {
        let mut types = TypeRegistry::new([]).unwrap();
        let int = TypeIdentity::Primitive(wire::PrimitiveInt);
        let map = types
            .construct(
                ConstructedType::Map {
                    key: int.clone(),
                    element: TypeIdentity::Any,
                },
                128,
                32,
            )
            .unwrap();
        let channel = types
            .construct(
                ConstructedType::Channel {
                    direction: wire::ChannelBoth,
                    element: int,
                },
                128,
                32,
            )
            .unwrap();
        for typ in [
            map,
            channel,
            TypeIdentity::Primitive(wire::PrimitiveFunction),
        ] {
            let layout = types.storage_lane_layout(&typ).unwrap();
            assert_eq!(layout.stride, 8);
            assert_eq!(layout.references, [0]);
            assert!(layout.dynamics.is_empty());
        }
        for typ in [
            TypeIdentity::Any,
            TypeIdentity::Primitive(wire::PrimitiveError),
        ] {
            let layout = types.storage_lane_layout(&typ).unwrap();
            assert_eq!(layout.stride, 24);
            assert!(layout.references.is_empty());
            assert_eq!(layout.dynamics, [0]);
        }
    }
}
