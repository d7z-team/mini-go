//! Bounded materialization of structured zero values.

use super::{Data, Value};
use crate::{
    contract_generated as wire,
    error::RuntimeError,
    types::{TypeIdentity, TypeRegistry},
};
use std::collections::BTreeMap;

impl Value {
    pub(crate) fn zero_with_budget(
        types: &TypeRegistry,
        typ: &TypeIdentity,
        depth: usize,
        max_depth: usize,
        max_elements: usize,
        remaining: &mut u64,
    ) -> Result<Value, RuntimeError> {
        if depth >= max_depth {
            return Err(RuntimeError::new(
                "value_limit",
                "type",
                "recursive value layout",
            ));
        }
        let underlying = types.underlying(typ)?;
        let data = match underlying {
            TypeIdentity::Primitive(wire::PrimitiveBool) => Data::Bool(false),
            TypeIdentity::Primitive(wire::PrimitiveString) => Data::String((&[][..]).into()),
            TypeIdentity::Primitive(primitive)
                if (wire::PrimitiveInt..=wire::PrimitiveInt64).contains(&primitive) =>
            {
                Data::Integer(0)
            }
            TypeIdentity::Primitive(primitive)
                if (wire::PrimitiveUint..=wire::PrimitiveUintptr).contains(&primitive) =>
            {
                Data::Unsigned(0)
            }
            TypeIdentity::Primitive(wire::PrimitiveFloat32 | wire::PrimitiveFloat64) => {
                Data::Float(0.0)
            }
            TypeIdentity::Primitive(wire::PrimitiveComplex64 | wire::PrimitiveComplex128) => {
                Data::Complex {
                    real: 0.0,
                    imag: 0.0,
                }
            }
            TypeIdentity::Structural { .. } => {
                let (module, node) = types.node(typ)?.unwrap();
                match node.kind {
                    wire::Struct => {
                        if let Some((mut value, construction_bytes)) =
                            types.cached_zero(typ, max_depth - depth, max_elements)
                        {
                            *remaining =
                                remaining.checked_sub(construction_bytes).ok_or_else(|| {
                                    RuntimeError::new(
                                        "allocation_limit",
                                        "zero",
                                        "struct layout exceeds heap budget",
                                    )
                                })?;
                            if let Data::Struct(fields) = &mut value.data {
                                // Sharing zero storage must not merge two guest
                                // objects in the live-memory census.
                                *fields = fields.fresh_zero();
                            }
                            return Ok(value);
                        }
                        let budget_before = *remaining;
                        *remaining = (node.fields.len() as u64)
                            .checked_mul(16)
                            .and_then(|bytes| bytes.checked_add(128))
                            .and_then(|bytes| remaining.checked_sub(bytes))
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "allocation_limit",
                                    "zero",
                                    "struct layout exceeds heap budget",
                                )
                            })?;
                        let mut fields = BTreeMap::new();
                        for field in node.fields.iter() {
                            let typ = types.resolve(module, &field.r#type)?;
                            fields.insert(
                                field.name.clone(),
                                Self::zero_with_budget(
                                    types,
                                    &typ,
                                    depth + 1,
                                    max_depth,
                                    max_elements,
                                    remaining,
                                )?,
                            );
                        }
                        let value = Value {
                            typ: typ.clone(),
                            data: Data::Struct(super::StructStorage::zero(fields)),
                        };
                        value.logical_bytes()?;
                        types.cache_zero(
                            &value,
                            max_depth - depth,
                            max_elements,
                            budget_before - *remaining,
                        );
                        return Ok(value);
                    }
                    wire::Array => {
                        let length = usize::try_from(node.length)
                            .ok()
                            .filter(|length| *length <= max_elements)
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "value_limit",
                                    "array",
                                    "array length exceeds limit",
                                )
                            })?;
                        let element = types.resolve(module, &node.elem)?;
                        *remaining = (length as u64)
                            .checked_mul(16)
                            .and_then(|bytes| bytes.checked_add(128))
                            .and_then(|bytes| remaining.checked_sub(bytes))
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "allocation_limit",
                                    "zero",
                                    "array layout exceeds heap budget",
                                )
                            })?;
                        if length == 0 {
                            Data::Array(Vec::new().into())
                        } else {
                            let zero = Self::zero_with_budget(
                                types,
                                &element,
                                depth + 1,
                                max_depth,
                                max_elements,
                                remaining,
                            )?;
                            let copies = zero
                                .logical_bytes()?
                                .saturating_sub(16)
                                .checked_mul(length as u64 - 1)
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "allocation_limit",
                                        "zero",
                                        "array layout overflow",
                                    )
                                })?;
                            if copies > *remaining {
                                return Err(RuntimeError::new(
                                    "allocation_limit",
                                    "zero",
                                    "array layout exceeds heap budget",
                                ));
                            }
                            let mut values = Vec::new();
                            values.try_reserve_exact(length).map_err(|_| {
                                RuntimeError::new(
                                    "allocation_limit",
                                    "zero",
                                    "array layout allocation failed",
                                )
                            })?;
                            if matches!(zero.data, Data::Struct(_) | Data::Array(_)) {
                                values.push(zero);
                                for _ in 1..length {
                                    values.push(Self::zero_with_budget(
                                        types,
                                        &element,
                                        depth + 1,
                                        max_depth,
                                        max_elements,
                                        remaining,
                                    )?);
                                }
                            } else {
                                *remaining -= copies;
                                values.resize(length, zero);
                            }
                            Data::Array(values.into())
                        }
                    }
                    _ => Data::Nil,
                }
            }
            _ => Data::Nil,
        };
        Ok(Value {
            typ: typ.clone(),
            data,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{ConstructedField, ConstructedType};

    #[test]
    fn cached_zero_charges_every_declared_blank_field() {
        let artifact: wire::Artifact = serde_json::from_str(
            r#"{"module":{"path":"example"},"type_table":{"nodes":[
                {"id":"record","kind":12,"fields":[
                    {"name":"_","type":{"kind":3,"primitive":3}},
                    {"name":"_","type":{"kind":3,"primitive":3}},
                    {"name":"Value","type":{"kind":3,"primitive":3}}]}]}}"#,
        )
        .unwrap();
        let types = TypeRegistry::new([&artifact]).unwrap();
        let typ = types
            .resolve(
                "example",
                &wire::TypeRef {
                    kind: wire::Struct,
                    node: "record".into(),
                    ..Default::default()
                },
            )
            .unwrap();
        let mut cold_remaining = 4096;
        Value::zero_with_budget(&types, &typ, 0, 16, 32, &mut cold_remaining).unwrap();
        let mut warm_remaining = 4096;
        Value::zero_with_budget(&types, &typ, 0, 16, 32, &mut warm_remaining).unwrap();
        assert_eq!(cold_remaining, warm_remaining);
        assert_eq!(4096 - cold_remaining, 128 + 3 * 16);
        let mut budget = 4096 - cold_remaining - 1;
        assert_eq!(
            Value::zero_with_budget(&types, &typ, 0, 16, 32, &mut budget)
                .unwrap_err()
                .code,
            "allocation_limit"
        );
    }

    #[test]
    fn cached_zero_preserves_budget_depth_elements_and_copy_isolation() {
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
        let typ = types
            .construct(
                ConstructedType::Struct(vec![ConstructedField {
                    name: "Items".into(),
                    typ: array,
                    tag: String::new(),
                    embedded: false,
                }]),
                32,
                16,
            )
            .unwrap();
        let mut cold_remaining = 4096;
        let mut cold =
            Value::zero_with_budget(&types, &typ, 0, 16, 32, &mut cold_remaining).unwrap();
        let mut warm_remaining = 4096;
        let warm = Value::zero_with_budget(&types, &typ, 0, 16, 32, &mut warm_remaining).unwrap();
        assert_eq!(cold_remaining, warm_remaining);
        assert_eq!(cold.logical_bytes().unwrap(), 4096 - cold_remaining + 16);
        let Data::Struct(fields) = &mut cold.data else {
            unreachable!()
        };
        let Data::Array(items) = &mut fields.get_mut("Items").unwrap().data else {
            unreachable!()
        };
        std::sync::Arc::make_mut(items)[0].data = Data::Integer(7);
        let Data::Struct(fields) = &warm.data else {
            unreachable!()
        };
        let Data::Array(items) = &fields["Items"].data else {
            unreachable!()
        };
        assert!(matches!(items[0].data, Data::Integer(0)));
        for (depth, elements, mut budget, code) in [
            (16, 32, 4096 - cold_remaining - 1, "allocation_limit"),
            (2, 32, 4096, "value_limit"),
            (16, 1, 4096, "value_limit"),
        ] {
            assert_eq!(
                Value::zero_with_budget(&types, &typ, 0, depth, elements, &mut budget)
                    .unwrap_err()
                    .code,
                code
            );
        }
    }
}
