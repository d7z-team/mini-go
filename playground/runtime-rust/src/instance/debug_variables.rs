//! Paused inspection traverses only the requested page and retains no guest copy.

use super::{
    debug::{FrameRef, VariableInfo, VariablePath, VariableRef},
    *,
};

impl Instance {
    pub fn debug_variables(
        &self,
        frame: &FrameRef,
        offset: usize,
        limit: usize,
    ) -> Result<Vec<VariableInfo>, RuntimeError> {
        if limit > self.limits.max_sequence_elements {
            return Err(RuntimeError::new(
                "debug_limit",
                "variables",
                "page size exceeds limit",
            ));
        }
        self.debug_binding_roots(frame)?
            .into_iter()
            .skip(offset)
            .take(limit)
            .map(|(name, root)| {
                self.debug_variable_info(
                    name,
                    VariableRef {
                        epoch: self.debug.epoch,
                        root,
                        path: Vec::new(),
                    },
                )
            })
            .collect()
    }

    pub fn debug_children(
        &self,
        reference: &VariableRef,
        offset: usize,
        limit: usize,
    ) -> Result<Vec<VariableInfo>, RuntimeError> {
        if limit > self.limits.max_sequence_elements
            || reference.path.len() >= self.limits.max_value_depth
        {
            return Err(RuntimeError::new(
                "debug_limit",
                "variables",
                "page size or path exceeds limit",
            ));
        }
        let value = self.debug_variable_value(reference)?;
        let mut paths = Vec::new();
        match &value.data {
            Data::Struct(fields) => {
                paths.extend(
                    fields
                        .keys()
                        .skip(offset)
                        .take(limit)
                        .map(|name| (name.clone(), VariablePath::Field(name.clone()))),
                );
            }
            Data::Array(values) => paths.extend(
                (0..values.len())
                    .skip(offset)
                    .take(limit)
                    .map(|index| (format!("[{index}]"), VariablePath::Index(index))),
            ),
            Data::Slice(slice) => paths.extend(
                (0..slice.length)
                    .skip(offset)
                    .take(limit)
                    .map(|index| (format!("[{index}]"), VariablePath::Index(index))),
            ),
            Data::Map(handle) => {
                let Data::MapEntries(entries) = &self.heap.get(*handle)?.data else {
                    unreachable!()
                };
                paths.extend(
                    (0..entries.len().saturating_mul(2))
                        .skip(offset)
                        .take(limit)
                        .map(|index| {
                            if index % 2 == 0 {
                                (
                                    format!("key[{}]", index / 2),
                                    VariablePath::MapKey(index / 2),
                                )
                            } else {
                                (
                                    format!("value[{}]", index / 2),
                                    VariablePath::MapValue(index / 2),
                                )
                            }
                        }),
                );
            }
            Data::Pointer(_) | Data::Interface(_) if offset == 0 && limit != 0 => {
                paths.push(("*".to_owned(), VariablePath::Dereference))
            }
            _ => {}
        }
        paths
            .into_iter()
            .map(|(name, path)| {
                let mut child = reference.clone();
                child.path.push(path);
                self.debug_variable_info(name, child)
            })
            .collect()
    }

    fn debug_variable_value(
        &self,
        reference: &VariableRef,
    ) -> Result<crate::value::ValueRead<'_>, RuntimeError> {
        let stale = || {
            RuntimeError::new(
                "stale_reference",
                "debug",
                "variable inspection epoch or path has expired",
            )
        };
        if self.closed
            || !self.debug.paused
            || reference.epoch != self.debug.epoch
            || reference.path.len() > self.limits.max_value_depth
        {
            return Err(stale());
        }
        let mut value = self.debug_root_value(&reference.root)?;
        for path in &reference.path {
            let previous = value;
            let data = &previous.data;
            let child = match (path, data) {
                (VariablePath::Field(name), Data::Struct(fields)) => {
                    fields.get(name).ok_or_else(stale)?
                }
                (VariablePath::Index(index), Data::Array(values)) => {
                    values.get(*index).ok_or_else(stale)?
                }
                (VariablePath::Index(index), Data::Slice(slice)) if *index < slice.length => {
                    let mut address = slice.storage.clone();
                    address.path.push(PathElement::Index(slice.start + index));
                    value = self.snapshot_address(&address)?;
                    continue;
                }
                (
                    VariablePath::MapKey(index) | VariablePath::MapValue(index),
                    Data::Map(handle),
                ) => {
                    let snapshot = self.heap.get(*handle)?;
                    let Data::MapEntries(entries) = &snapshot.data else {
                        return Err(stale());
                    };
                    let entry = entries.get(*index).ok_or_else(stale)?;
                    value = crate::value::ValueRead::Owned(
                        if matches!(path, VariablePath::MapKey(_)) {
                            entry.0.clone()
                        } else {
                            entry.1.clone()
                        },
                    );
                    continue;
                }
                (VariablePath::Dereference, Data::Pointer(address)) => {
                    value = self.snapshot_address(address)?;
                    continue;
                }
                (VariablePath::Dereference, Data::Interface(inner)) => inner,
                _ => return Err(stale()),
            };
            value = match previous {
                crate::value::ValueRead::Borrowed(parent) => {
                    let child = match (path, &parent.data) {
                        (VariablePath::Field(name), Data::Struct(fields)) => &fields[name],
                        (VariablePath::Index(index), Data::Array(values)) => &values[*index],
                        (VariablePath::Dereference, Data::Interface(inner)) => inner,
                        _ => {
                            value = crate::value::ValueRead::Owned(child.clone());
                            continue;
                        }
                    };
                    crate::value::ValueRead::Borrowed(child)
                }
                crate::value::ValueRead::Owned(_) | crate::value::ValueRead::Shared(_) => {
                    crate::value::ValueRead::Owned(child.clone())
                }
            };
        }
        Ok(value)
    }

    fn debug_variable_info(
        &self,
        name: String,
        reference: VariableRef,
    ) -> Result<VariableInfo, RuntimeError> {
        let value = self.debug_variable_value(&reference)?;
        let (summary, children) = match &value.data {
            Data::Array(values) => (format!("array len={}", values.len()), values.len()),
            Data::Struct(fields) => (format!("struct fields={}", fields.len()), fields.len()),
            Data::Slice(slice) => (
                format!("slice len={} cap={}", slice.length, slice.capacity),
                slice.length,
            ),
            Data::Map(handle) => {
                let Data::MapEntries(entries) = &self.heap.get(*handle)?.data else {
                    unreachable!()
                };
                (
                    format!("map len={}", entries.len()),
                    entries.len().saturating_mul(2),
                )
            }
            Data::Pointer(_) => ("pointer".to_owned(), 1),
            Data::Interface(_) => ("interface".to_owned(), 1),
            Data::String(bytes) => {
                let mut text = format!(
                    "{:?}",
                    String::from_utf8_lossy(&bytes[..bytes.len().min(128)])
                );
                if bytes.len() > 128 {
                    text.push('…');
                }
                (text, 0)
            }
            Data::Nil
            | Data::Bool(_)
            | Data::Integer(_)
            | Data::Unsigned(_)
            | Data::Float(_)
            | Data::Complex { .. } => (value.to_string(), 0),
            Data::Function(_) | Data::DynamicFunction(_) | Data::Method { .. } => {
                ("function".to_owned(), 0)
            }
            _ => ("resource".to_owned(), 0),
        };
        Ok(VariableInfo {
            name,
            typ: value.typ.clone(),
            summary,
            children,
            reference: (children != 0).then_some(reference),
        })
    }
}
