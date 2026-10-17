//! Validated aggregate layouts shared by every instance of a program.

use crate::{
    contract_generated as wire,
    error::RuntimeError,
    types::{TypeIdentity, TypeRegistry},
};
use std::{collections::HashSet, sync::Arc};

pub(crate) fn validate_aggregate_layout(
    types: &TypeRegistry,
    typ: &TypeIdentity,
) -> Result<(), RuntimeError> {
    enum Step {
        Enter(TypeIdentity),
        Exit(TypeIdentity),
    }
    let mut pending = vec![Step::Enter(typ.clone())];
    let mut active = HashSet::new();
    let mut complete = HashSet::new();
    while let Some(step) = pending.pop() {
        match step {
            Step::Enter(current) => {
                if complete.contains(&current) {
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
                if let Some((module, node)) = types.node(&current)? {
                    match node.kind {
                        wire::Struct => {
                            for field in node.fields.iter() {
                                children.push(types.resolve(module, &field.r#type)?);
                            }
                        }
                        wire::Array => {
                            let length = usize::try_from(node.length).map_err(|_| {
                                RuntimeError::new("invalid_type", "array", "invalid array length")
                            })?;
                            if length != 0 {
                                children.push(types.resolve(module, &node.elem)?);
                            }
                        }
                        _ => {}
                    }
                }
                pending.push(Step::Exit(current));
                pending.extend(children.into_iter().rev().map(Step::Enter));
            }
            Step::Exit(current) => {
                active.remove(&current);
                complete.insert(current);
            }
        }
    }
    Ok(())
}

#[derive(Clone)]
pub(crate) struct StructLayout {
    pub names: Arc<Vec<String>>,
    pub fields: Vec<(TypeIdentity, Option<usize>)>,
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
        let mut names = Vec::with_capacity(node.fields.len());
        let mut fields = Vec::with_capacity(node.fields.len());
        for field in node.fields.iter() {
            names.push(field.name.clone());
            fields.push((types.resolve(module, &field.r#type)?, None));
        }
        for (index, name) in supplied.iter().enumerate() {
            let field_index = names
                .iter()
                .position(|candidate| candidate == name)
                .ok_or_else(|| RuntimeError::new("missing_field", "make_struct", name))?;
            let (_, input) = &mut fields[field_index];
            if input.replace(index).is_some() {
                return Err(RuntimeError::new(
                    "invalid_operand",
                    "make_struct",
                    "duplicate field initializer",
                ));
            }
        }
        validate_aggregate_layout(types, typ)?;
        Ok(Self {
            names: Arc::new(names),
            fields,
        })
    }
}
