//! Instance-owned structural types. Construction stages metadata and commits
//! only after the complete shape and its referenced types have been validated.

use super::*;
use crate::contract::GoSlice;
use std::sync::atomic::{AtomicU64, Ordering};

static NEXT_CATALOG: AtomicU64 = AtomicU64::new(1);
const DYNAMIC_SHAPE_CACHE_LIMIT: usize = 4096;

#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct ConstructedField {
    pub name: String,
    pub typ: TypeIdentity,
    pub tag: String,
    pub embedded: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub enum ConstructedType {
    Array {
        length: usize,
        element: TypeIdentity,
    },
    Map {
        key: TypeIdentity,
        element: TypeIdentity,
    },
    Channel {
        direction: u8,
        element: TypeIdentity,
    },
    Function {
        params: Vec<TypeIdentity>,
        results: Vec<TypeIdentity>,
        variadic: bool,
    },
    Struct(Vec<ConstructedField>),
}

impl TypeRegistry {
    pub fn clear_dynamic(&mut self) {
        let removed = self.dynamic_nodes.keys().cloned().collect::<HashSet<_>>();
        self.dynamic_nodes = Arc::default();
        self.dynamic_refs = Arc::default();
        self.dynamic_module.clear();
        self.registered_dynamic = Arc::default();
        self.dynamic_costs = Arc::default();
        self.dynamic_shapes = Arc::default();
        self.dynamic_bytes = 0;
        self.prune_storage_type_ids(&removed);
        // Prepared layouts can contain dynamic structural identities. They
        // must not survive a catalog reset and accidentally retain a removed
        // node graph.
        let mut cache = self.cache.write().unwrap();
        cache.storage.clear();
        cache.lanes.clear();
        cache.fixed_words.clear();
        cache.logical_bytes.clear();
        cache.underlying.clear();
        cache.identical.clear();
        cache.zeros.clear();
        cache.zero_bytes = 0;
    }
    pub fn dynamic_stats(&self) -> (usize, u64) {
        (self.registered_dynamic.len(), self.dynamic_bytes)
    }

    /// Remove dynamic metadata that is no longer owned by a managed payload.
    /// The roots are the published dynamic identities; imported nodes are
    /// retained only when reachable from one of those roots.
    pub(crate) fn prune_dynamic(&mut self, roots: &[TypeIdentity]) -> (usize, u64) {
        let mut keep = HashSet::new();
        for root in roots {
            self.mark_dynamic_identity(root, &mut keep);
        }
        let before = self.registered_dynamic.len();
        let removed = self
            .dynamic_nodes
            .keys()
            .filter(|identity| !keep.contains(*identity))
            .cloned()
            .collect::<HashSet<_>>();
        if !removed.is_empty() {
            Arc::make_mut(&mut self.dynamic_nodes).retain(|identity, _| keep.contains(identity));
        }
        self.prune_storage_type_ids(&removed);
        let dynamic_module = self.dynamic_module.clone();
        let stale_refs = self.dynamic_refs.iter().any(|(_, reference)| {
            reference.node.is_empty()
                || !keep.contains(&(dynamic_module.clone().into(), reference.node.clone().into()))
        });
        if stale_refs {
            Arc::make_mut(&mut self.dynamic_refs).retain(|_, reference| {
                !reference.node.is_empty()
                    && keep
                        .contains(&(dynamic_module.clone().into(), reference.node.clone().into()))
            });
        }
        let root_set = roots.iter().collect::<HashSet<_>>();
        let stale_registered = self
            .registered_dynamic
            .iter()
            .any(|typ| !root_set.contains(typ));
        if stale_registered {
            Arc::make_mut(&mut self.registered_dynamic).retain(|typ| root_set.contains(typ));
        }
        let registered_dynamic = self.registered_dynamic.clone();
        let stale_costs = self
            .dynamic_costs
            .keys()
            .any(|typ| !registered_dynamic.contains(typ));
        if stale_costs {
            Arc::make_mut(&mut self.dynamic_costs)
                .retain(|typ, _| registered_dynamic.contains(typ));
        }
        let dynamic_nodes = self.dynamic_nodes.clone();
        let stale_shapes = self.dynamic_shapes.iter().any(|(_, typ)| {
            matches!(typ, TypeIdentity::Structural { module, node }
                if !dynamic_nodes.contains_key(&(module.clone(), node.clone())))
        });
        if stale_shapes {
            Arc::make_mut(&mut self.dynamic_shapes).retain(|_, typ| {
                matches!(typ, TypeIdentity::Structural { module, node }
                    if dynamic_nodes.contains_key(&(module.clone(), node.clone())))
            });
        }
        self.dynamic_bytes = self.dynamic_costs.values().copied().sum();
        // Dependency nodes may be reclaimed while the set of published roots
        // stays the same. Clear every derived layout cache whenever the node
        // graph changed, and reset the byte ledger for zero templates along
        // with the entries themselves.
        if !removed.is_empty() || before != self.registered_dynamic.len() {
            let mut cache = self.cache.write().unwrap();
            cache.storage.clear();
            cache.lanes.clear();
            cache.fixed_words.clear();
            cache.logical_bytes.clear();
            cache.underlying.clear();
            cache.identical.clear();
            cache.zeros.clear();
            cache.zero_bytes = 0;
        }
        (
            before.saturating_sub(self.registered_dynamic.len()),
            self.dynamic_bytes,
        )
    }

    fn prune_storage_type_ids(&self, removed: &HashSet<NodeIdentity>) {
        if removed.is_empty() {
            return;
        }
        let mut ids = self.storage_ids.write().unwrap();
        ids.by_identity.retain(|identity, _| {
            let mut target = identity;
            while let TypeIdentity::Pointer(element) | TypeIdentity::Slice(element) = target {
                target = element;
            }
            !matches!(target, TypeIdentity::Structural { module, node }
                if removed.contains(&(module.clone(), node.clone())))
        });
        let StorageTypeIdentities {
            by_identity, by_id, ..
        } = &mut *ids;
        by_id.retain(|_, (identity, _)| by_identity.contains_key(identity));
    }

    fn mark_dynamic_identity(&self, typ: &TypeIdentity, keep: &mut HashSet<NodeIdentity>) {
        if let TypeIdentity::Pointer(element) | TypeIdentity::Slice(element) = typ {
            self.mark_dynamic_identity(element, keep);
            return;
        }
        let TypeIdentity::Structural { module, node } = typ else {
            return;
        };
        let identity = (module.clone(), node.clone());
        if !keep.insert(identity.clone()) {
            return;
        }
        let Some(node_data) = self.dynamic_nodes.get(&identity) else {
            return;
        };
        let node_data = node_data.clone();
        let mut mark_reference = |reference: &wire::TypeRef| {
            if reference.kind != wire::Invalid
                && let Ok(resolved) = self.resolve(module, reference)
            {
                self.mark_dynamic_identity(&resolved, keep);
            }
        };
        mark_reference(&node_data.elem);
        mark_reference(&node_data.key);
        mark_reference(&node_data.underlying);
        mark_reference(&node_data.alias_target);
        for field in node_data.fields.iter() {
            mark_reference(&field.r#type);
        }
        for item in node_data.tuple.iter() {
            mark_reference(item);
        }
        if let Some(signature) = &node_data.signature {
            for parameter in signature.params.iter() {
                mark_reference(&parameter.r#type);
            }
            for result in signature.results.iter() {
                mark_reference(result);
            }
        }
        for method in node_data.methods.iter() {
            mark_reference(&method.receiver);
            for parameter in method.signature.params.iter() {
                mark_reference(&parameter.r#type);
            }
            for result in method.signature.results.iter() {
                mark_reference(result);
            }
        }
    }

    pub(crate) fn register_dynamic(
        &mut self,
        typ: &TypeIdentity,
        max_types: usize,
        max_bytes: u64,
        max_depth: usize,
    ) -> Result<(), RuntimeError> {
        if self.registered_dynamic.contains(typ) {
            return Ok(());
        }
        for existing in self.registered_dynamic.iter() {
            if self.identical(existing, typ)? {
                return Ok(());
            }
        }
        if self.registered_dynamic.len() >= max_types {
            return Err(RuntimeError::new(
                "dynamic_type_limit",
                "reflect",
                "dynamic reflection type limit exceeded",
            ));
        }
        let fields = self
            .node(typ)?
            .filter(|(_, node)| node.kind == wire::Struct)
            .map_or(0, |(_, node)| node.fields.len() as u64);
        let size = self
            .canonical_size(typ, max_depth, &mut HashMap::new())?
            .saturating_mul(64)
            .saturating_add(512)
            .saturating_add(fields.saturating_mul(128));
        if size > max_bytes.saturating_sub(self.dynamic_bytes) {
            return Err(RuntimeError::new(
                "dynamic_type_bytes_limit",
                "reflect",
                "dynamic reflection metadata byte limit exceeded",
            ));
        }
        Arc::make_mut(&mut self.registered_dynamic).insert(typ.clone());
        Arc::make_mut(&mut self.dynamic_costs).insert(typ.clone(), size);
        self.dynamic_bytes += size;
        let mut cache = self.cache.write().unwrap();
        cache.storage.remove(typ);
        cache.lanes.remove(typ);
        cache.fixed_words.remove(typ);
        cache.logical_bytes.remove(typ);
        Ok(())
    }

    pub(crate) fn canonical_text(
        &self,
        typ: &TypeIdentity,
        max_depth: usize,
        max_bytes: u64,
    ) -> Result<String, RuntimeError> {
        let size = self.canonical_size(typ, max_depth, &mut HashMap::new())?;
        if size > max_bytes {
            return Err(RuntimeError::new(
                "type_limit",
                "reflect",
                "type spelling exceeds byte limit",
            ));
        }
        let mut text = String::new();
        text.try_reserve_exact(size as usize).map_err(|_| {
            RuntimeError::new(
                "allocation_limit",
                "reflect",
                "cannot reserve type spelling",
            )
        })?;
        self.write_canonical(typ, &mut text)?;
        Ok(text)
    }

    fn write_canonical(&self, typ: &TypeIdentity, text: &mut String) -> Result<(), RuntimeError> {
        match typ {
            TypeIdentity::Void => text.push_str("Void"),
            TypeIdentity::Any => text.push_str("Any"),
            TypeIdentity::Primitive(primitive) => {
                const NAMES: [&str; 20] = [
                    "",
                    "Bool",
                    "String",
                    "Int",
                    "Int8",
                    "Int16",
                    "Int32",
                    "Int64",
                    "Uint",
                    "Uint8",
                    "Uint16",
                    "Uint32",
                    "Uint64",
                    "Uintptr",
                    "Float32",
                    "Float64",
                    "Complex64",
                    "Complex128",
                    "Error",
                    "Function",
                ];
                text.push_str(NAMES[*primitive as usize]);
            }
            TypeIdentity::Named(key) => {
                text.push_str(&key.module_path);
                text.push('.');
                text.push_str(&key.decl_id);
            }
            TypeIdentity::Pointer(element) | TypeIdentity::Slice(element) => {
                text.push_str(if matches!(typ, TypeIdentity::Pointer(_)) {
                    "Ptr<"
                } else {
                    "Slice<"
                });
                self.write_canonical(element, text)?;
                text.push('>');
            }
            TypeIdentity::Structural { .. } => {
                let (module, node) = self.node(typ)?.unwrap();
                match node.kind {
                    wire::Pointer | wire::Slice | wire::Array | wire::Map | wire::Waitable => {
                        text.push_str(match node.kind {
                            wire::Pointer => "Ptr<",
                            wire::Slice => "Slice<",
                            wire::Array => "Array<",
                            wire::Map => "Map<",
                            _ => match node.direction {
                                wire::ChannelReceive => "ReceiveWaitable<",
                                wire::ChannelSend => "SendWaitable<",
                                _ => "Waitable<",
                            },
                        });
                        if node.kind == wire::Array {
                            text.push_str(&node.length.to_string());
                            text.push_str(", ");
                        }
                        if node.kind == wire::Map {
                            self.write_canonical(&self.resolve(module, &node.key)?, text)?;
                            text.push_str(", ");
                        }
                        self.write_canonical(&self.resolve(module, &node.elem)?, text)?;
                        text.push('>');
                    }
                    wire::Struct => {
                        text.push_str("struct{");
                        for (index, field) in node.fields.iter().enumerate() {
                            if index != 0 {
                                text.push(',');
                            }
                            if field.embedded {
                                text.push_str("embedded ");
                            }
                            text.push_str(&field.name);
                            text.push(':');
                            self.write_canonical(&self.resolve(module, &field.r#type)?, text)?;
                            if !field.tag.is_empty() {
                                text.push_str(" `");
                                text.push_str(&field.tag);
                                text.push('`');
                            }
                        }
                        text.push('}');
                    }
                    wire::Function => {
                        self.write_signature(module, node.signature.as_ref().unwrap(), text)?
                    }
                    wire::Interface => {
                        text.push_str("interface{");
                        for (index, method) in node.methods.iter().enumerate() {
                            if index != 0 {
                                text.push(',');
                            }
                            text.push_str(&method.name);
                            text.push(':');
                            self.write_signature(module, &method.signature, text)?;
                        }
                        text.push('}');
                    }
                    _ => unreachable!("canonical size validated the runtime type"),
                }
            }
        }
        Ok(())
    }

    fn write_signature(
        &self,
        module: &str,
        signature: &wire::FunctionSignature,
        text: &mut String,
    ) -> Result<(), RuntimeError> {
        text.push_str("function(");
        for (index, parameter) in signature.params.iter().enumerate() {
            if index != 0 {
                text.push_str(", ");
            }
            if signature.variadic && index + 1 == signature.params.len() {
                text.push_str("variadic ");
            }
            self.write_canonical(&self.resolve(module, &parameter.r#type)?, text)?;
        }
        text.push(')');
        if signature.results.len() == 1 {
            text.push(' ');
        } else if signature.results.len() > 1 {
            text.push_str(" tuple(");
        }
        for (index, result) in signature.results.iter().enumerate() {
            if index != 0 {
                text.push_str(", ");
            }
            self.write_canonical(&self.resolve(module, result)?, text)?;
        }
        if signature.results.len() > 1 {
            text.push(')');
        }
        Ok(())
    }

    // Length of the current Go canonical type spelling, used only at the
    // metadata accounting boundary. Type relations remain structural.
    fn canonical_size(
        &self,
        typ: &TypeIdentity,
        depth: usize,
        memo: &mut HashMap<TypeIdentity, u64>,
    ) -> Result<u64, RuntimeError> {
        if let Some(size) = memo.get(typ) {
            return Ok(*size);
        }
        if depth == 0 {
            return Err(RuntimeError::new(
                "type_limit",
                "reflect",
                "type metadata depth exceeded",
            ));
        }
        let size = match typ {
            TypeIdentity::Void => 4,
            TypeIdentity::Any => 3,
            TypeIdentity::Primitive(primitive) => match *primitive {
                wire::PrimitiveBool | wire::PrimitiveInt8 | wire::PrimitiveUint => 4,
                wire::PrimitiveInt => 3,
                wire::PrimitiveString
                | wire::PrimitiveUint16
                | wire::PrimitiveUint32
                | wire::PrimitiveUint64 => 6,
                wire::PrimitiveInt16
                | wire::PrimitiveInt32
                | wire::PrimitiveInt64
                | wire::PrimitiveUint8
                | wire::PrimitiveError => 5,
                wire::PrimitiveUintptr | wire::PrimitiveFloat32 | wire::PrimitiveFloat64 => 7,
                wire::PrimitiveFunction => 8,
                wire::PrimitiveComplex64 => 9,
                wire::PrimitiveComplex128 => 10,
                _ => {
                    return Err(RuntimeError::new(
                        "invalid_type",
                        "reflect",
                        "unknown primitive",
                    ));
                }
            },
            TypeIdentity::Named(key) => (key.module_path.len() + 1 + key.decl_id.len()) as u64,
            TypeIdentity::Pointer(element) => {
                5_u64.saturating_add(self.canonical_size(element, depth - 1, memo)?)
            }
            TypeIdentity::Slice(element) => {
                7_u64.saturating_add(self.canonical_size(element, depth - 1, memo)?)
            }
            TypeIdentity::Structural { .. } => {
                let (module, node) = self.node(typ)?.ok_or_else(|| {
                    RuntimeError::new("unknown_type", "reflect", "missing type metadata")
                })?;
                let mut reference_size = |reference: &wire::TypeRef| {
                    self.canonical_size(&self.resolve(module, reference)?, depth - 1, memo)
                };
                match node.kind {
                    wire::Pointer => 5_u64.saturating_add(reference_size(&node.elem)?),
                    wire::Slice => 7_u64.saturating_add(reference_size(&node.elem)?),
                    wire::Array => (9 + node.length.to_string().len() as u64)
                        .saturating_add(reference_size(&node.elem)?),
                    wire::Map => 7_u64
                        .saturating_add(reference_size(&node.key)?)
                        .saturating_add(reference_size(&node.elem)?),
                    wire::Waitable => {
                        let prefix: u64 = match node.direction {
                            wire::ChannelReceive => 17,
                            wire::ChannelSend => 14,
                            _ => 10,
                        };
                        prefix.saturating_add(reference_size(&node.elem)?)
                    }
                    wire::Struct => {
                        let mut size =
                            8_u64.saturating_add(node.fields.len().saturating_sub(1) as u64);
                        for field in node.fields.iter() {
                            size = size
                                .saturating_add(field.name.len() as u64 + 1)
                                .saturating_add(if field.embedded { 9 } else { 0 })
                                .saturating_add(if field.tag.is_empty() {
                                    0
                                } else {
                                    field.tag.len() as u64 + 3
                                })
                                .saturating_add(reference_size(&field.r#type)?);
                        }
                        size
                    }
                    wire::Function => self.signature_size(
                        module,
                        node.signature.as_ref().unwrap(),
                        depth - 1,
                        memo,
                    )?,
                    wire::Interface => {
                        let mut size =
                            11_u64.saturating_add(node.methods.len().saturating_sub(1) as u64);
                        for method in node.methods.iter() {
                            size = size
                                .saturating_add(method.name.len() as u64 + 1)
                                .saturating_add(self.signature_size(
                                    module,
                                    &method.signature,
                                    depth - 1,
                                    memo,
                                )?);
                        }
                        size
                    }
                    _ => {
                        return Err(RuntimeError::new(
                            "invalid_type",
                            "reflect",
                            "unsupported runtime type metadata",
                        ));
                    }
                }
            }
        };
        memo.insert(typ.clone(), size);
        Ok(size)
    }

    fn signature_size(
        &self,
        module: &str,
        signature: &wire::FunctionSignature,
        depth: usize,
        memo: &mut HashMap<TypeIdentity, u64>,
    ) -> Result<u64, RuntimeError> {
        let mut size = 10_u64
            .saturating_add((signature.params.len().saturating_sub(1) as u64).saturating_mul(2));
        if signature.variadic {
            size = size.saturating_add(9);
        }
        for parameter in signature.params.iter() {
            size = size.saturating_add(self.canonical_size(
                &self.resolve(module, &parameter.r#type)?,
                depth,
                memo,
            )?);
        }
        if signature.results.len() == 1 {
            size = size.saturating_add(1);
        } else if signature.results.len() > 1 {
            size = size.saturating_add(8).saturating_add(
                (signature.results.len().saturating_sub(1) as u64).saturating_mul(2),
            );
        }
        for result in signature.results.iter() {
            size = size.saturating_add(self.canonical_size(
                &self.resolve(module, result)?,
                depth,
                memo,
            )?);
        }
        Ok(size)
    }
    pub fn construct(
        &mut self,
        shape: ConstructedType,
        max_nodes: usize,
        max_depth: usize,
    ) -> Result<TypeIdentity, RuntimeError> {
        if let Some(identity) = self.dynamic_shapes.get(&shape)
            && let TypeIdentity::Structural { module, node } = identity
            && self
                .dynamic_nodes
                .contains_key(&(module.clone(), node.clone()))
        {
            return Ok(identity.clone());
        }
        let shape_key = shape.clone();
        let mut candidate = self.clone();
        if candidate.dynamic_module.is_empty() {
            let id = NEXT_CATALOG
                .try_update(Ordering::Relaxed, Ordering::Relaxed, |id| id.checked_add(1))
                .map_err(|_| {
                    RuntimeError::new("type_limit", "dynamic", "type catalog identity exhausted")
                })?;
            candidate.dynamic_module = format!("\0runtime.{id}");
            while candidate
                .nodes
                .keys()
                .any(|(module, _)| module.as_ref() == candidate.dynamic_module)
            {
                candidate.dynamic_module.push('.');
            }
        }
        let mut node = wire::TypeNode::default();
        match shape {
            ConstructedType::Array { length, element } => {
                node.kind = wire::Array;
                node.length = i64::try_from(length).map_err(|_| {
                    RuntimeError::new("type_limit", "array", "array length overflow")
                })?;
                node.elem = candidate.import_type(&element, max_nodes, max_depth)?;
            }
            ConstructedType::Map { key, element } => {
                if !candidate.comparable(&key)? {
                    return Err(RuntimeError::new(
                        "reflect",
                        "MapOf",
                        "reflect: invalid map key type",
                    ));
                }
                node.kind = wire::Map;
                node.key = candidate.import_type(&key, max_nodes, max_depth)?;
                node.elem = candidate.import_type(&element, max_nodes, max_depth)?;
            }
            ConstructedType::Channel { direction, element } => {
                if !matches!(
                    direction,
                    wire::ChannelBoth | wire::ChannelSend | wire::ChannelReceive
                ) {
                    return Err(RuntimeError::new(
                        "reflect",
                        "ChanOf",
                        "reflect: invalid channel direction",
                    ));
                }
                node.kind = wire::Waitable;
                node.direction = direction;
                node.elem = candidate.import_type(&element, max_nodes, max_depth)?;
            }
            ConstructedType::Function {
                params,
                results,
                variadic,
            } => {
                node.kind = wire::Function;
                let mut signature = wire::FunctionSignature {
                    variadic,
                    ..Default::default()
                };
                signature.params = GoSlice(Some(
                    params
                        .iter()
                        .map(|typ| {
                            Ok(wire::TypeParam {
                                r#type: candidate.import_type(typ, max_nodes, max_depth)?,
                            })
                        })
                        .collect::<Result<_, RuntimeError>>()?,
                ));
                signature.results = GoSlice(Some(
                    results
                        .iter()
                        .map(|typ| candidate.import_type(typ, max_nodes, max_depth))
                        .collect::<Result<_, _>>()?,
                ));
                node.signature = Some(Box::new(signature));
            }
            ConstructedType::Struct(fields) => {
                node.kind = wire::Struct;
                let mut names = HashSet::new();
                let mut output = Vec::new();
                for field in fields {
                    let valid_name = super::names::is_identifier(&field.name);
                    if !valid_name
                        || !crate::types::names::is_exported(&field.name)
                        || field.tag.contains('`')
                        || !names.insert(field.name.clone())
                    {
                        return Err(RuntimeError::new(
                            "reflect",
                            "StructOf",
                            "reflect: invalid or duplicate field name",
                        ));
                    }
                    output.push(wire::Field {
                        name: field.name,
                        r#type: candidate.import_type(&field.typ, max_nodes, max_depth)?,
                        tag: field.tag,
                        embedded: field.embedded,
                    });
                }
                node.fields = GoSlice(Some(output));
            }
        }
        // The number of live nodes is not an identity: pruning can make it
        // smaller while old structural identities remain in bytecode, frame
        // metadata, or a staged revision. Keep the sequence in the registry
        // lineage and advance it even when a node is later reclaimed.
        let mut sequence = candidate.next_dynamic_sequence;
        loop {
            let id = format!("constructed.{sequence}");
            if !candidate
                .dynamic_nodes
                .keys()
                .any(|(_, existing)| existing.as_ref() == id)
            {
                node.id = id;
                break;
            }
            sequence = sequence.checked_add(1).ok_or_else(|| {
                RuntimeError::new("type_limit", "dynamic", "dynamic type identity exhausted")
            })?;
        }
        candidate.next_dynamic_sequence = sequence.checked_add(1).ok_or_else(|| {
            RuntimeError::new("type_limit", "dynamic", "dynamic type identity exhausted")
        })?;
        let identity = TypeIdentity::Structural {
            module: candidate.dynamic_module.clone().into(),
            node: node.id.clone().into(),
        };
        Arc::make_mut(&mut candidate.dynamic_nodes).insert(
            (
                candidate.dynamic_module.clone().into(),
                node.id.clone().into(),
            ),
            node,
        );
        for ((module, _), node) in candidate.dynamic_nodes.iter() {
            candidate.validate_node(module, node)?;
        }
        let existing_identities = self
            .dynamic_nodes
            .keys()
            .map(|(module, node)| TypeIdentity::Structural {
                module: module.clone(),
                node: node.clone(),
            })
            .collect::<Vec<_>>();
        for existing in existing_identities {
            if !candidate.identical(&existing, &identity)? {
                continue;
            }
            if self.dynamic_shapes.len() < DYNAMIC_SHAPE_CACHE_LIMIT {
                Arc::make_mut(&mut self.dynamic_shapes).insert(shape_key.clone(), existing.clone());
            }
            return Ok(existing);
        }
        if candidate.dynamic_nodes.len() > max_nodes {
            return Err(RuntimeError::new(
                "type_limit",
                "dynamic",
                "dynamic type budget exceeded",
            ));
        }
        if candidate.dynamic_shapes.len() < DYNAMIC_SHAPE_CACHE_LIMIT {
            Arc::make_mut(&mut candidate.dynamic_shapes).insert(shape_key, identity.clone());
        }
        *self = candidate;
        Ok(identity)
    }

    fn import_type(
        &mut self,
        typ: &TypeIdentity,
        max_nodes: usize,
        depth: usize,
    ) -> Result<wire::TypeRef, RuntimeError> {
        if let Some(reference) = self.dynamic_refs.get(typ) {
            return Ok(reference.clone());
        }
        if depth == 0 {
            return Err(RuntimeError::new(
                "type_limit",
                "dynamic",
                "dynamic type depth exceeded",
            ));
        }
        let mut reference = wire::TypeRef::default();
        match typ {
            TypeIdentity::Void => {
                return Err(RuntimeError::new(
                    "reflect",
                    "type",
                    "reflect: invalid element type",
                ));
            }
            TypeIdentity::Any => {
                reference.kind = wire::Any;
                return Ok(reference);
            }
            TypeIdentity::Primitive(primitive) => {
                reference.kind = wire::Primitive;
                reference.primitive = *primitive;
                return Ok(reference);
            }
            TypeIdentity::Named(key) => {
                reference.kind = wire::Named;
                reference.named = (**key).clone();
                return Ok(reference);
            }
            TypeIdentity::Structural { module, node } if module.as_ref() == self.dynamic_module => {
                reference.kind = self.dynamic_nodes[&(module.clone(), node.clone())].kind;
                reference.node = node.to_string();
                return Ok(reference);
            }
            _ => (),
        }
        if self.dynamic_refs.len() + self.dynamic_nodes.len() >= max_nodes {
            return Err(RuntimeError::new(
                "type_limit",
                "dynamic",
                "dynamic type budget exceeded",
            ));
        }
        let (source_module, mut node) = match typ {
            TypeIdentity::Pointer(_) => (
                String::new(),
                wire::TypeNode {
                    kind: wire::Pointer,
                    ..Default::default()
                },
            ),
            TypeIdentity::Slice(_) => (
                String::new(),
                wire::TypeNode {
                    kind: wire::Slice,
                    ..Default::default()
                },
            ),
            _ => {
                let (module, node) = self.node(typ)?.ok_or_else(|| {
                    RuntimeError::new("unknown_type", "dynamic", "missing structural type")
                })?;
                (module.to_owned(), node.clone())
            }
        };
        // Imported nodes share the dynamic catalog with constructed nodes.
        // Their live count is not an identity either: pruning an unreachable
        // import must not let a stale reference resolve to a later shape.
        let mut sequence = self.next_imported_sequence;
        loop {
            let id = format!("imported.{sequence}");
            if !self
                .dynamic_nodes
                .keys()
                .any(|(_, existing)| existing.as_ref() == id)
            {
                node.id = id;
                break;
            }
            sequence = sequence.checked_add(1).ok_or_else(|| {
                RuntimeError::new("type_limit", "dynamic", "dynamic type identity exhausted")
            })?;
        }
        self.next_imported_sequence = sequence.checked_add(1).ok_or_else(|| {
            RuntimeError::new("type_limit", "dynamic", "dynamic type identity exhausted")
        })?;
        reference.kind = node.kind;
        reference.node = node.id.clone();
        Arc::make_mut(&mut self.dynamic_refs).insert(typ.clone(), reference.clone());
        if let TypeIdentity::Pointer(element) | TypeIdentity::Slice(element) = typ {
            node.elem = self.import_type(element, max_nodes, depth - 1)?;
        } else {
            for reference in [
                &mut node.elem,
                &mut node.key,
                &mut node.underlying,
                &mut node.alias_target,
            ] {
                if reference.kind != wire::Invalid {
                    *reference = self.import_type(
                        &self.resolve(&source_module, reference)?,
                        max_nodes,
                        depth - 1,
                    )?;
                }
            }
            let mut fields = Vec::new();
            for mut field in node.fields.iter().cloned() {
                field.r#type = self.import_type(
                    &self.resolve(&source_module, &field.r#type)?,
                    max_nodes,
                    depth - 1,
                )?;
                fields.push(field);
            }
            node.fields = GoSlice(Some(fields));
            node.tuple = GoSlice(Some(
                node.tuple
                    .iter()
                    .map(|reference| {
                        self.import_type(
                            &self.resolve(&source_module, reference)?,
                            max_nodes,
                            depth - 1,
                        )
                    })
                    .collect::<Result<_, _>>()?,
            ));
            if let Some(signature) = &mut node.signature {
                self.import_signature(&source_module, signature, max_nodes, depth - 1)?;
            }
            let mut methods = Vec::new();
            for mut method in node.methods.iter().cloned() {
                if method.receiver.kind != wire::Invalid {
                    method.receiver = self.import_type(
                        &self.resolve(&source_module, &method.receiver)?,
                        max_nodes,
                        depth - 1,
                    )?;
                }
                self.import_signature(&source_module, &mut method.signature, max_nodes, depth - 1)?;
                methods.push(method);
            }
            node.methods = GoSlice(Some(methods));
        }
        Arc::make_mut(&mut self.dynamic_nodes).insert(
            (self.dynamic_module.clone().into(), node.id.clone().into()),
            node,
        );
        Ok(reference)
    }

    fn import_signature(
        &mut self,
        module: &str,
        signature: &mut wire::FunctionSignature,
        max_nodes: usize,
        depth: usize,
    ) -> Result<(), RuntimeError> {
        signature.params = GoSlice(Some(
            signature
                .params
                .iter()
                .map(|parameter| {
                    Ok(wire::TypeParam {
                        r#type: self.import_type(
                            &self.resolve(module, &parameter.r#type)?,
                            max_nodes,
                            depth,
                        )?,
                    })
                })
                .collect::<Result<_, RuntimeError>>()?,
        ));
        signature.results = GoSlice(Some(
            signature
                .results
                .iter()
                .map(|reference| {
                    self.import_type(&self.resolve(module, reference)?, max_nodes, depth)
                })
                .collect::<Result<_, _>>()?,
        ));
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn repeated_shape_construction_reuses_catalog_identity() {
        let mut types = TypeRegistry::new([]).unwrap();
        let shape = ConstructedType::Array {
            length: 4,
            element: TypeIdentity::Primitive(wire::PrimitiveInt),
        };
        let first = types.construct(shape.clone(), 32, 16).unwrap();
        let node_count = types.dynamic_nodes.len();
        // Reusing a live shape does not consume another node admission.
        let second = types.construct(shape.clone(), node_count, 16).unwrap();
        assert_eq!(first, second);
        assert_eq!(types.dynamic_nodes.len(), node_count);

        let snapshot = types.clone();
        types.prune_dynamic(&[]);
        let replacement = types.construct(shape, 32, 16).unwrap();
        assert_ne!(first, replacement);
        assert_eq!(snapshot.node(&first).unwrap().unwrap().1.length, 4);
        assert!(types.node(&first).is_err());
        assert_eq!(types.node(&replacement).unwrap().unwrap().1.length, 4);
    }

    #[test]
    fn dynamic_construction_snapshot_isolated_from_later_catalog_writes() {
        let mut types = TypeRegistry::new([]).unwrap();
        let snapshot = types.clone();
        let shape = ConstructedType::Array {
            length: 3,
            element: TypeIdentity::Primitive(wire::PrimitiveInt),
        };
        let created = types.construct(shape, 32, 16).unwrap();
        assert!(snapshot.node(&created).is_err());
        assert!(types.node(&created).unwrap().is_some());

        let before_nodes = types.dynamic_nodes.len();
        let before_refs = types.dynamic_refs.len();
        let failed = types.construct(
            ConstructedType::Array {
                length: 5,
                element: TypeIdentity::Primitive(wire::PrimitiveInt),
            },
            0,
            16,
        );
        assert_eq!(failed.unwrap_err().code, "type_limit");
        assert_eq!(types.dynamic_nodes.len(), before_nodes);
        assert_eq!(types.dynamic_refs.len(), before_refs);
    }

    #[test]
    fn clear_dynamic_releases_zero_cache_budget_for_the_next_catalog() {
        let mut types = TypeRegistry::new([]).unwrap();
        let shape = ConstructedType::Struct(vec![ConstructedField {
            name: "Value".into(),
            typ: TypeIdentity::Primitive(wire::PrimitiveInt),
            tag: String::new(),
            embedded: false,
        }]);
        let first = types.construct(shape.clone(), 32, 16).unwrap();
        let mut budget = 4096;
        crate::value::Value::zero_with_budget(&types, &first, 0, 16, 32, &mut budget).unwrap();
        {
            let cache = types.cache.read().unwrap();
            assert_eq!(cache.zeros.len(), 1);
            assert!(cache.zero_bytes > 0);
        }

        types.clear_dynamic();
        {
            let cache = types.cache.read().unwrap();
            assert!(cache.zeros.is_empty());
            assert_eq!(cache.zero_bytes, 0);
        }

        let replacement = types.construct(shape, 32, 16).unwrap();
        let mut budget = 4096;
        crate::value::Value::zero_with_budget(&types, &replacement, 0, 16, 32, &mut budget)
            .unwrap();
        let cache = types.cache.read().unwrap();
        assert_eq!(cache.zeros.len(), 1);
        assert!(cache.zero_bytes > 0);
    }

    #[test]
    fn metadata_budgets_charge_canonical_shapes_once_and_preserve_state_on_failure() {
        let mut types = TypeRegistry::new([]).unwrap();
        let array = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: TypeIdentity::Primitive(wire::PrimitiveInt),
                },
                32,
                16,
            )
            .unwrap();
        let array_bytes = 512 + 64 * "Array<2, Int>".len() as u64;
        types.register_dynamic(&array, 1, array_bytes, 16).unwrap();
        assert_eq!(types.dynamic_stats(), (1, array_bytes));
        types.register_dynamic(&array, 1, array_bytes, 16).unwrap();
        let pointer = TypeIdentity::Pointer(std::sync::Arc::new(array.clone()));
        assert_eq!(
            types
                .register_dynamic(&pointer, 1, u64::MAX, 16)
                .unwrap_err()
                .code,
            "dynamic_type_limit"
        );
        assert_eq!(
            types
                .register_dynamic(&pointer, 2, array_bytes, 16)
                .unwrap_err()
                .code,
            "dynamic_type_bytes_limit"
        );
        assert_eq!(types.dynamic_stats(), (1, array_bytes));
        let pointer_bytes = 512 + 64 * "Ptr<Array<2, Int>>".len() as u64;
        types
            .register_dynamic(&pointer, 2, array_bytes + pointer_bytes, 16)
            .unwrap();
        assert_eq!(types.dynamic_stats(), (2, array_bytes + pointer_bytes));
        let merged = types.overlay(&TypeRegistry::new([]).unwrap()).unwrap();
        assert_eq!(merged.dynamic_stats(), types.dynamic_stats());
        types.clear_dynamic();
        assert_eq!(types.dynamic_stats(), (0, 0));
    }

    #[test]
    fn canonical_metadata_costs_include_tags_directions_and_variadic_signatures() {
        let int = TypeIdentity::Primitive(wire::PrimitiveInt);
        let mut types = TypeRegistry::new([]).unwrap();
        let cases = [
            (
                ConstructedType::Map {
                    key: int.clone(),
                    element: TypeIdentity::Primitive(wire::PrimitiveString),
                },
                "Map<Int, String>",
                0,
            ),
            (
                ConstructedType::Channel {
                    direction: wire::ChannelReceive,
                    element: int.clone(),
                },
                "ReceiveWaitable<Int>",
                0,
            ),
            (
                ConstructedType::Channel {
                    direction: wire::ChannelSend,
                    element: int.clone(),
                },
                "SendWaitable<Int>",
                0,
            ),
            (
                ConstructedType::Channel {
                    direction: wire::ChannelBoth,
                    element: int.clone(),
                },
                "Waitable<Int>",
                0,
            ),
            (
                ConstructedType::Function {
                    params: vec![
                        int.clone(),
                        TypeIdentity::Slice(std::sync::Arc::new(int.clone())),
                    ],
                    results: vec![int.clone(), TypeIdentity::Primitive(wire::PrimitiveBool)],
                    variadic: true,
                },
                "function(Int, variadic Slice<Int>) tuple(Int, Bool)",
                0,
            ),
            (
                ConstructedType::Struct(vec![ConstructedField {
                    name: "Field".into(),
                    typ: int,
                    tag: "json:\"字段\"".into(),
                    embedded: false,
                }]),
                "struct{Field:Int `json:\"字段\"`}",
                1,
            ),
        ];
        for (shape, text, fields) in cases {
            let typ = types.construct(shape, 128, 32).unwrap();
            assert_eq!(
                types.canonical_text(&typ, 32, text.len() as u64).unwrap(),
                text
            );
            assert_eq!(
                types
                    .canonical_text(&typ, 32, text.len() as u64 - 1)
                    .unwrap_err()
                    .code,
                "type_limit"
            );
            let before = types.dynamic_stats();
            types.register_dynamic(&typ, 32, 1 << 20, 32).unwrap();
            assert_eq!(
                types.dynamic_stats(),
                (
                    before.0 + 1,
                    before.1 + 512 + 64 * text.len() as u64 + 128 * fields
                ),
                "{text}"
            );
        }
    }

    #[test]
    fn pruning_dynamic_metadata_keeps_dependency_closure_only() {
        let mut types = TypeRegistry::new([]).unwrap();
        let array = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: TypeIdentity::Primitive(wire::PrimitiveInt),
                },
                32,
                16,
            )
            .unwrap();
        types.register_dynamic(&array, 32, 1 << 20, 16).unwrap();
        assert_eq!(types.storage_logical_bytes(&array).unwrap(), 16);
        let pointer = types
            .construct(
                ConstructedType::Array {
                    length: 3,
                    element: array.clone(),
                },
                32,
                16,
            )
            .unwrap();
        types.register_dynamic(&pointer, 32, 1 << 20, 16).unwrap();
        // The outer array stores three inline two-int arrays, so its logical
        // width is 3 * (2 * 8), independent of its physical descriptor.
        assert_eq!(types.storage_logical_bytes(&pointer).unwrap(), 48);
        assert_eq!(types.dynamic_stats().0, 2);
        types
            .construct(
                ConstructedType::Array {
                    length: 1,
                    element: TypeIdentity::Pointer(Arc::new(TypeIdentity::Primitive(
                        wire::PrimitiveInt,
                    ))),
                },
                32,
                16,
            )
            .unwrap();
        let old_imported = types
            .dynamic_refs
            .values()
            .map(|reference| reference.node.clone())
            .collect::<Vec<_>>();
        assert!(!old_imported.is_empty());

        types.prune_dynamic(std::slice::from_ref(&pointer));
        assert_eq!(types.dynamic_stats().0, 1);
        assert!(types.node(&pointer).unwrap().is_some());
        assert!(types.node(&array).unwrap().is_some());

        types.prune_dynamic(&[]);
        assert_eq!(types.dynamic_stats(), (0, 0));
        assert!(types.node(&pointer).is_err());
        assert!(types.node(&array).is_err());

        // The retired identities remain unavailable to later construction;
        // a stale frame or staged revision must never resolve to a new shape
        // merely because the live node count became smaller.
        let replacement = types
            .construct(
                ConstructedType::Array {
                    length: 4,
                    element: TypeIdentity::Primitive(wire::PrimitiveInt),
                },
                32,
                16,
            )
            .unwrap();
        assert_ne!(replacement, array);
        assert_eq!(types.node(&replacement).unwrap().unwrap().1.length, 4);

        let pointer_after_prune = types
            .construct(
                ConstructedType::Array {
                    length: 1,
                    element: TypeIdentity::Pointer(Arc::new(TypeIdentity::Primitive(
                        wire::PrimitiveInt,
                    ))),
                },
                32,
                16,
            )
            .unwrap();
        let new_imported = types
            .dynamic_refs
            .values()
            .map(|reference| reference.node.clone())
            .collect::<Vec<_>>();
        assert!(new_imported.iter().all(|node| !old_imported.contains(node)));
        assert!(types.node(&pointer_after_prune).is_ok());
    }
}
