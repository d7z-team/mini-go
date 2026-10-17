//! Immutable aggregate operands keep prepared bytes, never a recursive Value tree.

use super::{Data, Value};
use crate::{
    contract_generated as wire,
    error::RuntimeError,
    heap::{Heap, PreparedPayload, Trace},
    managed_storage::{StorageRef, VALUE_BOX, VALUE_REF},
    types::{StorageLaneLayout, TypeIdentity},
};
use std::sync::Arc;

#[derive(Clone)]
pub struct PreparedAggregate(Arc<AggregateSnapshot>);

struct AggregateSnapshot {
    heap: Heap,
    typ: TypeIdentity,
    layout: Arc<StorageLaneLayout>,
    bytes: Arc<[u8]>,
    offset: usize,
}

impl std::fmt::Debug for PreparedAggregate {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("PreparedAggregate")
            .field("typ", &self.0.typ)
            .field("length", &self.len())
            .field("bytes", &self.0.layout.stride)
            .finish()
    }
}

impl PreparedAggregate {
    pub(crate) fn zero(
        heap: Heap,
        typ: TypeIdentity,
        layout: Arc<StorageLaneLayout>,
    ) -> Result<Self, RuntimeError> {
        let width = usize::try_from(layout.stride).map_err(|_| {
            RuntimeError::new(
                "allocation_limit",
                "aggregate",
                "zero aggregate width overflows host size",
            )
        })?;
        let mut bytes = Vec::new();
        bytes.try_reserve_exact(width).map_err(|_| {
            RuntimeError::new(
                "allocation_limit",
                "aggregate",
                "zero aggregate allocation failed",
            )
        })?;
        bytes.resize(width, 0);
        Self::new(heap, typ, layout, bytes.into(), 0)
    }

    /// Build one prepared array backing in a single pass. Missing elements are
    /// already represented by zeroed lanes; initial values replace only their
    /// own lane, so construction stays O(stride + initial.len()) instead of
    /// copying the complete backing once per value.
    pub(crate) fn new(
        heap: Heap,
        typ: TypeIdentity,
        layout: Arc<StorageLaneLayout>,
        bytes: Arc<[u8]>,
        offset: usize,
    ) -> Result<Self, RuntimeError> {
        if !matches!(layout.kind, wire::Array | wire::Struct)
            || offset
                .checked_add(layout.stride as usize)
                .is_none_or(|end| end > bytes.len())
        {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "aggregate",
                "aggregate range does not match its prepared layout",
            ));
        }
        Ok(Self(Arc::new(AggregateSnapshot {
            heap,
            typ,
            layout,
            bytes,
            offset,
        })))
    }

    pub(crate) fn typ(&self) -> &TypeIdentity {
        &self.0.typ
    }
    pub(crate) fn layout(&self) -> &Arc<StorageLaneLayout> {
        &self.0.layout
    }
    pub(crate) fn bytes(&self) -> &[u8] {
        &self.0.bytes[self.0.offset..self.0.offset + self.0.layout.stride as usize]
    }
    pub(crate) fn owner_id(&self) -> u64 {
        self.0.heap.owner_id()
    }
    pub(crate) fn is_array(&self) -> bool {
        self.0.layout.kind == wire::Array
    }
    pub(crate) fn len(&self) -> usize {
        self.0
            .layout
            .element
            .as_ref()
            .map_or(self.0.layout.fields.len(), |(_, _, length)| *length)
    }
    pub(crate) fn field_name(&self, index: usize) -> Option<&str> {
        self.0
            .layout
            .fields
            .get(index)
            .map(|field| field.name.as_str())
    }
    pub(crate) fn field_index(&self, name: &str) -> Option<usize> {
        self.0
            .layout
            .fields
            .iter()
            .position(|field| field.name == name)
    }
    pub(crate) fn child_layout(
        &self,
        index: usize,
    ) -> Result<(&TypeIdentity, &Arc<StorageLaneLayout>, usize), RuntimeError> {
        if let Some((typ, layout, length)) = &self.0.layout.element {
            if index < *length {
                return Ok((typ, layout, index * layout.stride as usize));
            }
        } else if let Some(field) = self.0.layout.fields.get(index) {
            return Ok((&field.typ, &field.layout, field.offset as usize));
        }
        Err(RuntimeError::new(
            "index_out_of_range",
            "aggregate",
            "aggregate index is outside its layout",
        ))
    }
    pub(crate) fn get(&self, index: usize) -> Result<Value, RuntimeError> {
        let (typ, layout, offset) = self.child_layout(index)?;
        if matches!(layout.kind, wire::Array | wire::Struct) {
            return Ok(Value {
                typ: typ.clone(),
                data: Data::Aggregate(Self::new(
                    self.0.heap.clone(),
                    typ.clone(),
                    layout.clone(),
                    self.0.bytes.clone(),
                    self.0.offset + offset,
                )?),
            });
        }
        self.0.heap.decode_prepared_lane(
            typ,
            layout,
            &self.bytes()[offset..offset + layout.stride as usize],
        )
    }
    pub(crate) fn get_field(&self, name: &str) -> Result<Value, RuntimeError> {
        let index = self.field_index(name).ok_or_else(|| {
            RuntimeError::new(
                "field_not_found",
                "aggregate",
                format!("unknown field {name}"),
            )
        })?;
        self.get(index)
    }
    pub(crate) fn values(&self) -> impl ExactSizeIterator<Item = Result<Value, RuntimeError>> + '_ {
        (0..self.len()).map(|index| self.get(index))
    }
    pub(crate) fn with_value(&self, index: usize, value: &Value) -> Result<Self, RuntimeError> {
        let (typ, layout, offset) = self.child_layout(index)?;
        let payload = self.0.heap.prepared_payload_for_type(value, typ)?;
        if payload.bytes.len() != layout.stride as usize {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "aggregate",
                "replacement width does not match field layout",
            ));
        }
        let mut bytes = self.bytes().to_vec();
        bytes[offset..offset + payload.bytes.len()].copy_from_slice(&payload.bytes);
        Self::new(
            self.0.heap.clone(),
            self.0.typ.clone(),
            self.0.layout.clone(),
            bytes.into(),
            0,
        )
    }
    pub(crate) fn with_values(
        &self,
        values: impl IntoIterator<Item = (usize, Value)>,
    ) -> Result<Self, RuntimeError> {
        // Keep nested typed-box allocations provisional until every field has
        // been encoded and the replacement aggregate has been validated.  A
        // failed later field must not publish an earlier dynamic payload.
        let scope = self.0.heap.begin_allocation_scope();
        let mut bytes = self.bytes().to_vec();
        for (index, value) in values {
            let (typ, layout, offset) = self.child_layout(index)?;
            let payload = self.0.heap.prepared_payload_for_type(&value, typ)?;
            if payload.bytes.len() != layout.stride as usize {
                return Err(RuntimeError::new(
                    "typed_payload_invalid",
                    "aggregate",
                    "replacement width does not match field layout",
                ));
            }
            bytes[offset..offset + payload.bytes.len()].copy_from_slice(&payload.bytes);
        }
        let result = Self::new(
            self.0.heap.clone(),
            self.0.typ.clone(),
            self.0.layout.clone(),
            bytes.into(),
            0,
        )?;
        scope.commit();
        Ok(result)
    }
    pub(crate) fn retype(&self, typ: &TypeIdentity) -> Result<Self, RuntimeError> {
        let layout = self.0.heap.layout_for_type(typ)?;
        if layout.stride != self.0.layout.stride
            || layout.kind != self.0.layout.kind
            || layout.references != self.0.layout.references
            || layout.dynamics != self.0.layout.dynamics
        {
            return Err(RuntimeError::new(
                "type_error",
                "aggregate",
                "aggregate conversion changes its storage layout",
            ));
        }
        Self::new(
            self.0.heap.clone(),
            typ.clone(),
            layout,
            self.0.bytes.clone(),
            self.0.offset,
        )
    }
    pub(crate) fn array_range(
        &self,
        start: usize,
        length: usize,
        typ: &TypeIdentity,
    ) -> Result<Self, RuntimeError> {
        let Some((source_element_type, element, count)) = &self.0.layout.element else {
            return Err(RuntimeError::new(
                "type_error",
                "aggregate",
                "expected array",
            ));
        };
        let layout = self.0.heap.layout_for_type(typ)?;
        let Some((target_element_type, target_element, target_count)) = &layout.element else {
            return Err(RuntimeError::new(
                "type_error",
                "aggregate",
                "array view requires an array layout",
            ));
        };
        let element_types_match = if source_element_type == target_element_type {
            true
        } else if let Some(types) = self.0.heap.type_registry_snapshot() {
            types.identical(source_element_type, target_element_type)?
        } else {
            false
        };
        if start.checked_add(length).is_none_or(|end| end > *count)
            || *target_count != length
            || !element_types_match
            || !Self::physical_layout_compatible(target_element, element)
        {
            return Err(RuntimeError::new(
                "index_out_of_range",
                "aggregate",
                "array view exceeds its backing or has an incompatible type",
            ));
        }
        Self::new(
            self.0.heap.clone(),
            typ.clone(),
            layout,
            self.0.bytes.clone(),
            self.0.offset + start * element.stride as usize,
        )
    }
    /// Replace one prepared array range in a single linear pass. Calling
    /// `with_value` once per element would copy the complete packed backing on
    /// every iteration and turn an ArrayView write into O(n²).
    pub(crate) fn with_array_range(
        &self,
        start: usize,
        replacement: &Self,
    ) -> Result<Self, RuntimeError> {
        let Some((source_element_type, source_element, source_count)) = &self.0.layout.element
        else {
            return Err(RuntimeError::new(
                "type_error",
                "aggregate",
                "expected array",
            ));
        };
        let Some((replacement_element_type, replacement_element, replacement_count)) =
            &replacement.0.layout.element
        else {
            return Err(RuntimeError::new(
                "type_error",
                "aggregate",
                "array replacement requires an array",
            ));
        };
        let element_types_match = if source_element_type == replacement_element_type {
            true
        } else if let Some(types) = self.0.heap.type_registry_snapshot() {
            types.identical(source_element_type, replacement_element_type)?
        } else {
            false
        };
        if start
            .checked_add(*replacement_count)
            .is_none_or(|end| end > *source_count)
            || self.owner_id() != replacement.owner_id()
            || !element_types_match
            || !Self::physical_layout_compatible(replacement_element, source_element)
        {
            return Err(RuntimeError::new(
                "type_error",
                "aggregate",
                "array replacement has an incompatible layout",
            ));
        }
        let mut bytes = self.bytes().to_vec();
        let offset = start * source_element.stride as usize;
        let replacement_bytes = replacement.bytes();
        bytes[offset..offset + replacement_bytes.len()].copy_from_slice(replacement_bytes);
        Self::new(
            self.0.heap.clone(),
            self.0.typ.clone(),
            self.0.layout.clone(),
            bytes.into(),
            0,
        )
    }

    fn physical_layout_compatible(left: &StorageLaneLayout, right: &StorageLaneLayout) -> bool {
        left.kind == right.kind
            && left.primitive == right.primitive
            && left.stride == right.stride
            && left.references == right.references
            && left.dynamics == right.dynamics
            && left.fixed == right.fixed
            && left.fields.len() == right.fields.len()
            && left.fields.iter().zip(&right.fields).all(|(left, right)| {
                left.name == right.name
                    && left.offset == right.offset
                    && Self::physical_layout_compatible(&left.layout, &right.layout)
            })
            && match (&left.element, &right.element) {
                (None, None) => true,
                (Some((_, left, left_count)), Some((_, right, right_count))) => {
                    left_count == right_count && Self::physical_layout_compatible(left, right)
                }
                _ => false,
            }
    }

    pub(crate) fn payload(&self) -> PreparedPayload {
        let mut references = Vec::new();
        self.reference_lanes(&mut |offset, handle| references.push((offset, handle)));
        PreparedPayload {
            bytes: self.bytes().to_vec(),
            references,
        }
    }
    fn reference_lanes(&self, visit: &mut dyn FnMut(u32, StorageRef)) {
        let bytes = self.bytes();
        let read = |offset: usize| {
            StorageRef::from_payload_identity(u64::from_le_bytes(
                bytes[offset..offset + 8].try_into().unwrap(),
            ))
        };
        for &offset in &self.0.layout.references {
            let handle = read(offset as usize);
            if handle.valid() {
                visit(offset, handle);
            }
        }
        for &offset in &self.0.layout.dynamics {
            let tag = u32::from_le_bytes(
                bytes[offset as usize + 4..offset as usize + 8]
                    .try_into()
                    .unwrap(),
            );
            if matches!(tag, VALUE_REF | VALUE_BOX) {
                let handle = read(offset as usize + 8);
                if handle.valid() {
                    visit(offset + 8, handle);
                }
            }
        }
    }
    pub(crate) fn type_roots(&self, roots: &mut Vec<TypeIdentity>) {
        roots.push(self.0.typ.clone());
        for &offset in &self.0.layout.dynamics {
            let id = u32::from_le_bytes(
                self.bytes()[offset as usize..offset as usize + 4]
                    .try_into()
                    .unwrap(),
            );
            if let Some(typ) = self.0.heap.storage_type_identity(id) {
                roots.push(typ);
            }
        }
    }
}

impl Trace for PreparedAggregate {
    fn trace(&self, visit: &mut dyn FnMut(StorageRef)) {
        self.reference_lanes(&mut |_, handle| visit(handle));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{ConstructedField, ConstructedType, TypeRegistry};

    #[test]
    fn zero_aggregate_uses_prepared_bytes_instead_of_value_tree() {
        let mut types = TypeRegistry::new([]).unwrap();
        let typ = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: TypeIdentity::Primitive(wire::PrimitiveInt),
                },
                128,
                32,
            )
            .unwrap();
        let heap = Heap::new(64, 1 << 20).unwrap();
        heap.set_type_registry(types.clone());
        let layout = heap.layout_for_type(&typ).unwrap();
        let zero = PreparedAggregate::zero(heap.clone(), typ.clone(), layout.clone()).unwrap();
        assert_eq!(zero.bytes().len(), layout.stride as usize);
        assert!(zero.bytes().iter().all(|byte| *byte == 0));
        assert!(matches!(zero.get(0).unwrap().data, Data::Integer(0)));
    }

    #[test]
    fn nested_value_copy_preserves_shared_references_through_collection() {
        let mut types = TypeRegistry::new([]).unwrap();
        let numbers = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: TypeIdentity::Primitive(wire::PrimitiveInt),
                },
                128,
                32,
            )
            .unwrap();
        let typ = types
            .construct(
                ConstructedType::Struct(vec![
                    ConstructedField {
                        name: "Numbers".into(),
                        typ: numbers.clone(),
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
        let heap = Heap::new(64, 1 << 20).unwrap();
        heap.set_type_registry(types.clone());
        let source = Value {
            typ: typ.clone(),
            data: Data::Struct(super::super::StructStorage::with_layout(
                Arc::new(vec!["Numbers".into(), "Text".into()]),
                vec![
                    Value {
                        typ: numbers,
                        data: Data::Array(vec![Value::int(7), Value::int(11)].into()),
                    },
                    Value::string(b"shared"),
                ],
            )),
        };
        let payload = heap.prepared_payload(&source).unwrap();
        let original = heap.decode_prepared_value(&typ, &payload.bytes).unwrap();
        assert!(crate::operators::equal(&source, &original, &types).unwrap());
        assert_eq!(
            crate::managed_map::hash_value(&source).unwrap(),
            crate::managed_map::hash_value(&original).unwrap()
        );
        let numbers = original
            .field("Numbers")
            .unwrap()
            .with_element(0, Value::int(99))
            .unwrap();
        let updated = original.with_element(0, numbers).unwrap();
        let root = heap
            .allocate(updated, heap.logical_allocation_bytes(&source).unwrap())
            .unwrap();
        heap.flush_worker_lease().unwrap();
        heap.collect_minor([root]).unwrap();
        heap.collect([root]).unwrap();
        let updated = heap.read_typed_value(root).unwrap();
        assert_eq!(
            original
                .field("Numbers")
                .unwrap()
                .element(0)
                .unwrap()
                .integer()
                .unwrap(),
            7
        );
        assert_eq!(
            updated
                .field("Numbers")
                .unwrap()
                .element(0)
                .unwrap()
                .integer()
                .unwrap(),
            99
        );
        assert_eq!(
            updated
                .field("Numbers")
                .unwrap()
                .element(1)
                .unwrap()
                .integer()
                .unwrap(),
            11
        );
        assert_eq!(original.field("Text").unwrap().to_string(), "shared");
        assert_eq!(updated.field("Text").unwrap().to_string(), "shared");
    }

    #[test]
    fn array_range_requires_element_type_and_layout_match() {
        let mut types = TypeRegistry::new([]).unwrap();
        let integers = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: TypeIdentity::Primitive(wire::PrimitiveInt),
                },
                128,
                32,
            )
            .unwrap();
        let booleans = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: TypeIdentity::Primitive(wire::PrimitiveBool),
                },
                128,
                32,
            )
            .unwrap();
        let heap = Heap::new(64, 1 << 20).unwrap();
        heap.set_type_registry(types);
        let source = Value {
            typ: integers.clone(),
            data: Data::Array(vec![Value::int(1), Value::int(2)].into()),
        };
        let payload = heap.prepared_payload(&source).unwrap();
        let aggregate = PreparedAggregate::new(
            heap.clone(),
            integers.clone(),
            heap.layout_for_type(&integers).unwrap(),
            payload.bytes.into(),
            0,
        )
        .unwrap();
        assert!(aggregate.array_range(0, 2, &booleans).is_err());
        let replacement = PreparedAggregate::new(
            heap.clone(),
            integers.clone(),
            heap.layout_for_type(&integers).unwrap(),
            heap.prepared_payload(&Value {
                typ: integers.clone(),
                data: Data::Array(vec![Value::int(7), Value::int(9)].into()),
            })
            .unwrap()
            .bytes
            .into(),
            0,
        )
        .unwrap();
        let updated = aggregate.with_array_range(0, &replacement).unwrap();
        assert_eq!(updated.get(0).unwrap().integer().unwrap(), 7);
        assert_eq!(updated.get(1).unwrap().integer().unwrap(), 9);
    }
}
