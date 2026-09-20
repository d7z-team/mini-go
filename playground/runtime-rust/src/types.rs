//! Structural type lookup. Text formatting is deliberately separate from
//! resolution so execution can retain stable named and module-local identities.

use crate::{contract_generated as wire, error::RuntimeError};
use std::collections::{HashMap, HashSet};
use std::sync::{Arc, RwLock};

mod dynamic;
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

#[derive(Clone, Debug)]
pub struct TypeRegistry {
    cache: Arc<RwLock<TypeCache>>,
    nodes: Arc<HashMap<NodeIdentity, wire::TypeNode>>,
    named: Arc<HashMap<wire::TypeKey, NodeIdentity>>,
    node_aliases: Arc<HashMap<(String, String), String>>,
    dynamic_nodes: HashMap<NodeIdentity, wire::TypeNode>,
    dynamic_refs: HashMap<TypeIdentity, wire::TypeRef>,
    dynamic_module: String,
    registered_dynamic: HashSet<TypeIdentity>,
    dynamic_bytes: u64,
}

type NodeIdentity = (Arc<str>, Arc<str>);

#[derive(Debug, Default)]
struct TypeCache {
    resolved: HashMap<String, HashMap<wire::TypeRef, TypeIdentity>>,
    resolved_count: usize,
    underlying: HashMap<TypeIdentity, TypeIdentity>,
}

const TYPE_CACHE_LIMIT: usize = 16_384;

impl TypeRegistry {
    pub(crate) fn use_context(&mut self, source: &Self) {
        if !Arc::ptr_eq(&self.node_aliases, &source.node_aliases) {
            self.node_aliases = source.node_aliases.clone();
            self.cache = source.cache.clone();
        }
    }
    pub(crate) fn overlay(&self, next: &Self, max_nodes: usize) -> Result<Self, RuntimeError> {
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
        if merged.nodes.len() + merged.dynamic_nodes.len() > max_nodes {
            return Err(RuntimeError::new(
                "type_limit",
                "patch",
                "type metadata limit exceeded",
            ));
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
            if !expected.name.chars().next().is_some_and(char::is_uppercase)
                && source_module != target_module
            {
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
            nodes: Arc::new(HashMap::new()),
            named: Arc::new(HashMap::new()),
            node_aliases: Arc::new(HashMap::new()),
            dynamic_nodes: HashMap::new(),
            dynamic_refs: HashMap::new(),
            dynamic_module: String::new(),
            registered_dynamic: HashSet::new(),
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
                "invalid or unresolved runtime type reference",
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
