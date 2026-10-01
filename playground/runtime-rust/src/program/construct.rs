//! Validated aggregate layouts shared by every instance of a program.

use crate::{
    contract_generated as wire,
    error::RuntimeError,
    types::{TypeIdentity, TypeRegistry},
};
use std::{
    collections::{BTreeMap, HashMap, HashSet},
    sync::Arc,
};

#[derive(Clone)]
pub(crate) enum AggregateLayout {
    Struct(StructLayout),
    Sequence(ValueLayout),
}

#[derive(Clone, Copy, Default)]
pub(crate) struct ValueLayout {
    pub depth: usize,
    pub max_sequence: usize,
}

impl ValueLayout {
    pub fn prepare(types: &TypeRegistry, typ: &TypeIdentity) -> Result<Self, RuntimeError> {
        enum Step {
            Enter(TypeIdentity),
            Exit(TypeIdentity, Vec<TypeIdentity>, usize),
        }
        let mut pending = vec![Step::Enter(typ.clone())];
        let mut active = HashSet::new();
        let mut complete = HashMap::<TypeIdentity, Self>::new();
        while let Some(step) = pending.pop() {
            match step {
                Step::Enter(current) => {
                    if complete.contains_key(&current) {
                        continue;
                    }
                    if !active.insert(current.clone()) {
                        return Err(RuntimeError::new(
                            "invalid_type",
                            "value layout",
                            "recursive aggregate layout",
                        ));
                    }
                    let mut children = Vec::new();
                    let mut sequence = 0;
                    if let Some((module, node)) = types.node(&current)? {
                        match node.kind {
                            wire::Struct => {
                                for field in node.fields.iter() {
                                    children.push(types.resolve(module, &field.r#type)?);
                                }
                            }
                            wire::Array => {
                                sequence = usize::try_from(node.length).map_err(|_| {
                                    RuntimeError::new(
                                        "invalid_type",
                                        "array",
                                        "invalid array length",
                                    )
                                })?;
                                if sequence != 0 {
                                    children.push(types.resolve(module, &node.elem)?);
                                }
                            }
                            _ => {}
                        }
                    }
                    pending.push(Step::Exit(current, children.clone(), sequence));
                    pending.extend(children.into_iter().rev().map(Step::Enter));
                }
                Step::Exit(current, children, sequence) => {
                    let mut layout = Self {
                        depth: 1,
                        max_sequence: sequence,
                    };
                    for child in children {
                        let child = complete[&child];
                        layout.depth = layout.depth.max(child.depth.saturating_add(1));
                        layout.max_sequence = layout.max_sequence.max(child.max_sequence);
                    }
                    active.remove(&current);
                    complete.insert(current, layout);
                }
            }
        }
        Ok(complete[typ])
    }
}

#[derive(Clone)]
pub(crate) struct StructLayout {
    pub names: Arc<Vec<String>>,
    pub fields: Vec<(TypeIdentity, Option<usize>)>,
    pub declared_fields: usize,
    pub limits: ValueLayout,
}

impl StructLayout {
    pub fn prepare(
        types: &TypeRegistry,
        typ: &TypeIdentity,
        supplied: &[String],
    ) -> Result<Self, RuntimeError> {
        let (module, node) = types
            .node(typ)?
            .filter(|(_, node)| node.kind == wire::Struct)
            .ok_or_else(|| {
                RuntimeError::new("invalid_operand", "make_struct", "expected struct type")
            })?;
        let mut fields = BTreeMap::new();
        for field in node.fields.iter() {
            fields.insert(
                field.name.clone(),
                (types.resolve(module, &field.r#type)?, None),
            );
        }
        for (index, name) in supplied.iter().enumerate() {
            let (_, input) = fields
                .get_mut(name)
                .ok_or_else(|| RuntimeError::new("missing_field", "make_struct", name))?;
            if input.replace(index).is_some() {
                return Err(RuntimeError::new(
                    "invalid_operand",
                    "make_struct",
                    "duplicate field initializer",
                ));
            }
        }
        let (names, fields) = fields.into_iter().unzip();
        Ok(Self {
            names: Arc::new(names),
            fields,
            declared_fields: node.fields.len(),
            limits: ValueLayout::prepare(types, typ)?,
        })
    }
}
