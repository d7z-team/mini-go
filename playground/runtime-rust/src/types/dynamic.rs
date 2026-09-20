//! Instance-owned structural types. Construction stages metadata and commits
//! only after the complete shape and its referenced types have been validated.

use super::*;
use crate::contract::GoSlice;
use std::sync::atomic::{AtomicU64, Ordering};

static NEXT_CATALOG: AtomicU64 = AtomicU64::new(1);

pub struct ConstructedField {
    pub name: String,
    pub typ: TypeIdentity,
    pub tag: String,
    pub embedded: bool,
}

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
        self.dynamic_nodes = HashMap::new();
        self.dynamic_refs = HashMap::new();
        self.dynamic_module.clear();
        self.registered_dynamic.clear();
        self.dynamic_bytes = 0;
    }
    pub fn dynamic_stats(&self) -> (usize, u64) {
        (self.registered_dynamic.len(), self.dynamic_bytes)
    }

    pub(crate) fn register_dynamic(
        &mut self,
        typ: &TypeIdentity,
        max_types: usize,
        max_bytes: u64,
        max_depth: usize,
    ) -> Result<(), RuntimeError> {
        for existing in &self.registered_dynamic {
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
        self.registered_dynamic.insert(typ.clone());
        self.dynamic_bytes += size;
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
        let mut candidate = self.clone();
        if candidate.dynamic_module.is_empty() {
            let id = NEXT_CATALOG
                .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |id| id.checked_add(1))
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
                    let valid_name = !field.name.is_empty()
                        && field.name.chars().enumerate().all(|(index, ch)| {
                            ch == '_' || ch.is_alphabetic() || index > 0 && ch.is_numeric()
                        });
                    if !valid_name
                        || !field.name.chars().next().is_some_and(char::is_uppercase)
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
        node.id = format!("constructed.{}", candidate.dynamic_nodes.len());
        let identity = TypeIdentity::Structural {
            module: candidate.dynamic_module.clone().into(),
            node: node.id.clone().into(),
        };
        candidate.dynamic_nodes.insert(
            (
                candidate.dynamic_module.clone().into(),
                node.id.clone().into(),
            ),
            node,
        );
        for ((module, _), node) in &candidate.dynamic_nodes {
            candidate.validate_node(module, node)?;
        }
        for (module, node) in self.dynamic_nodes.keys() {
            let existing = TypeIdentity::Structural {
                module: module.clone(),
                node: node.clone(),
            };
            if candidate.identical(&existing, &identity)? {
                return Ok(existing);
            }
        }
        if candidate.dynamic_nodes.len() > max_nodes {
            return Err(RuntimeError::new(
                "type_limit",
                "dynamic",
                "dynamic type budget exceeded",
            ));
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
        node.id = format!("imported.{}", self.dynamic_refs.len());
        reference.kind = node.kind;
        reference.node = node.id.clone();
        self.dynamic_refs.insert(typ.clone(), reference.clone());
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
        self.dynamic_nodes.insert(
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
        let merged = types.overlay(&TypeRegistry::new([]).unwrap(), 32).unwrap();
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
}
