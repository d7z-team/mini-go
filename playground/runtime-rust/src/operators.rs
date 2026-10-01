use crate::{
    contract_generated as wire,
    error::RuntimeError,
    types::{TypeIdentity, TypeRegistry},
    value::{Data, Value},
};

#[path = "operators_complex.rs"]
mod complex;
use complex::complex_divide;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Operator {
    Add,
    Subtract,
    Multiply,
    Divide,
    Remainder,
    Not,
    Xor,
    And,
    Or,
    AndNot,
    ShiftLeft,
    ShiftRight,
    Less,
    LessEqual,
    Greater,
    GreaterEqual,
    Equal,
    NotEqual,
    LogicalAnd,
    LogicalOr,
    Real,
    Imaginary,
    Complex,
}

impl Operator {
    pub fn parse(text: &str) -> Result<Self, RuntimeError> {
        Ok(match text {
            "+" => Self::Add,
            "-" => Self::Subtract,
            "*" => Self::Multiply,
            "/" => Self::Divide,
            "%" => Self::Remainder,
            "!" => Self::Not,
            "^" => Self::Xor,
            "&" => Self::And,
            "|" => Self::Or,
            "&^" => Self::AndNot,
            "<<" => Self::ShiftLeft,
            ">>" => Self::ShiftRight,
            "<" => Self::Less,
            "<=" => Self::LessEqual,
            ">" => Self::Greater,
            ">=" => Self::GreaterEqual,
            "==" => Self::Equal,
            "!=" => Self::NotEqual,
            "&&" => Self::LogicalAnd,
            "||" => Self::LogicalOr,
            "real" => Self::Real,
            "imag" => Self::Imaginary,
            "complex" => Self::Complex,
            _ => {
                return Err(RuntimeError::new(
                    "invalid_operation",
                    text,
                    "unknown operator",
                ));
            }
        })
    }
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Add => "+",
            Self::Subtract => "-",
            Self::Multiply => "*",
            Self::Divide => "/",
            Self::Remainder => "%",
            Self::Not => "!",
            Self::Xor => "^",
            Self::And => "&",
            Self::Or => "|",
            Self::AndNot => "&^",
            Self::ShiftLeft => "<<",
            Self::ShiftRight => ">>",
            Self::Less => "<",
            Self::LessEqual => "<=",
            Self::Greater => ">",
            Self::GreaterEqual => ">=",
            Self::Equal => "==",
            Self::NotEqual => "!=",
            Self::LogicalAnd => "&&",
            Self::LogicalOr => "||",
            Self::Real => "real",
            Self::Imaginary => "imag",
            Self::Complex => "complex",
        }
    }
}

fn invalid(operator: &str) -> RuntimeError {
    RuntimeError::new(
        "invalid_operation",
        operator,
        "operand types do not support operator",
    )
}

/// Operand normalization belongs to the prepared function's type registry.
/// None keeps dynamic resolution; zero means a proven nonnumeric operand.
#[derive(Clone, Copy)]
pub(crate) struct PreparedOperator {
    pub operator: Operator,
    numeric: [Option<u8>; 2],
}

impl PreparedOperator {
    pub fn parse(text: &str) -> Result<Self, RuntimeError> {
        Ok(Self {
            operator: Operator::parse(text)?,
            numeric: [None; 2],
        })
    }

    pub fn bind(
        &mut self,
        index: usize,
        typ: &TypeIdentity,
        registry: &TypeRegistry,
    ) -> Result<(), RuntimeError> {
        self.numeric[index] = Some(match registry.underlying(typ)? {
            TypeIdentity::Primitive(kind)
                if (wire::PrimitiveInt..=wire::PrimitiveComplex128).contains(&kind) =>
            {
                kind
            }
            _ => 0,
        });
        Ok(())
    }
}

pub(crate) fn unary(
    operation: PreparedOperator,
    mut value: Value,
    registry: &TypeRegistry,
) -> Result<Value, RuntimeError> {
    let operator = operation.operator;
    let original_type = value.typ.clone();
    value.typ = numeric_operand_type(&value.typ, registry, operation.numeric[0])?;
    if let Data::Complex { real, imag } = value.data
        && (operator == Operator::Real || operator == Operator::Imaginary)
    {
        return Ok(Value {
            typ: TypeIdentity::Primitive(
                if value.typ == TypeIdentity::Primitive(wire::PrimitiveComplex64) {
                    wire::PrimitiveFloat32
                } else {
                    wire::PrimitiveFloat64
                },
            ),
            data: Data::Float(if operator == Operator::Real {
                real
            } else {
                imag
            }),
        });
    }
    value.data = match (operator, value.data) {
        (Operator::Not, Data::Bool(value)) => Data::Bool(!value),
        (
            Operator::Add,
            data @ (Data::Integer(_) | Data::Unsigned(_) | Data::Float(_) | Data::Complex { .. }),
        ) => data,
        (Operator::Subtract, Data::Complex { real, imag }) => Data::Complex {
            real: -real,
            imag: -imag,
        },
        (Operator::Subtract, Data::Integer(value)) => Data::Integer(value.wrapping_neg()),
        (Operator::Subtract, Data::Unsigned(value)) => Data::Unsigned(value.wrapping_neg()),
        (Operator::Subtract, Data::Float(value)) => Data::Float(-value),
        (Operator::Xor, Data::Integer(value)) => Data::Integer(!value),
        (Operator::Xor, Data::Unsigned(value)) => Data::Unsigned(!value),
        _ => return Err(invalid(operator.as_str())),
    };
    value = normalize(value);
    value.typ = original_type;
    Ok(value)
}

pub(crate) fn binary(
    operation: PreparedOperator,
    left: &Value,
    right: &Value,
    registry: &TypeRegistry,
) -> Result<Value, RuntimeError> {
    let operator = operation.operator;
    // The loader binds numeric kinds, including the underlying kind of named
    // types. Full-width arithmetic needs no conversion or width normalization.
    if operation.numeric == [Some(wire::PrimitiveInt), Some(wire::PrimitiveInt)]
        && let (&Data::Integer(a), &Data::Integer(b)) = (&left.data, &right.data)
    {
        let data = match operator {
            Operator::Add => Some(Data::Integer(a.wrapping_add(b))),
            Operator::Subtract => Some(Data::Integer(a.wrapping_sub(b))),
            Operator::Multiply => Some(Data::Integer(a.wrapping_mul(b))),
            Operator::Less => return Ok(Value::boolean(a < b)),
            Operator::LessEqual => return Ok(Value::boolean(a <= b)),
            Operator::Greater => return Ok(Value::boolean(a > b)),
            Operator::GreaterEqual => return Ok(Value::boolean(a >= b)),
            _ => None,
        };
        if let Some(data) = data {
            return Ok(Value {
                typ: left.typ.clone(),
                data,
            });
        }
    }
    if operator == Operator::Equal || operator == Operator::NotEqual {
        return Ok(Value::boolean(
            equal(left, right, registry)? == (operator == Operator::Equal),
        ));
    }
    let typ = numeric_operand_type(&left.typ, registry, operation.numeric[0])?;
    if operation.numeric[1].is_none() {
        numeric_operand_type(&right.typ, registry, None)?;
    }
    if operator == Operator::Complex {
        let (&Data::Float(real), &Data::Float(imag)) = (&left.data, &right.data) else {
            return Err(invalid(operator.as_str()));
        };
        return Ok(normalize(Value {
            typ: TypeIdentity::Primitive(
                if typ == TypeIdentity::Primitive(wire::PrimitiveFloat32) {
                    wire::PrimitiveComplex64
                } else {
                    wire::PrimitiveComplex128
                },
            ),
            data: Data::Complex { real, imag },
        }));
    }
    if operator == Operator::ShiftLeft || operator == Operator::ShiftRight {
        let shift = match right.data {
            Data::Integer(value) if value >= 0 => value as u64,
            Data::Unsigned(value) => value,
            Data::Integer(_) => {
                return Err(RuntimeError::new(
                    "panic",
                    operator.as_str(),
                    "negative shift count",
                ));
            }
            _ => return Err(invalid(operator.as_str())),
        };
        let data = match left.data {
            Data::Integer(value) if operator == Operator::ShiftLeft => {
                Data::Integer(if shift >= 64 { 0 } else { value << shift })
            }
            Data::Integer(value) => Data::Integer(if shift >= 64 {
                if value < 0 { -1 } else { 0 }
            } else {
                value >> shift
            }),
            Data::Unsigned(value) if operator == Operator::ShiftLeft => {
                Data::Unsigned(if shift >= 64 { 0 } else { value << shift })
            }
            Data::Unsigned(value) => Data::Unsigned(if shift >= 64 { 0 } else { value >> shift }),
            _ => return Err(invalid(operator.as_str())),
        };
        let mut value = normalize(Value { typ, data });
        value.typ = left.typ.clone();
        return Ok(value);
    }
    let data = match (&left.data, &right.data) {
        (&Data::Complex { real: a, imag: b }, &Data::Complex { real: c, imag: d }) => {
            let (real, imag) = match operator {
                Operator::Add => (a + c, b + d),
                Operator::Subtract => (a - c, b - d),
                Operator::Multiply => (a * c - b * d, a * d + b * c),
                Operator::Divide => complex_divide(a, b, c, d),
                _ => return Err(invalid(operator.as_str())),
            };
            Data::Complex { real, imag }
        }
        (&Data::Integer(a), &Data::Integer(b)) => match operator {
            Operator::Add => Data::Integer(a.wrapping_add(b)),
            Operator::Subtract => Data::Integer(a.wrapping_sub(b)),
            Operator::Multiply => Data::Integer(a.wrapping_mul(b)),
            Operator::Divide | Operator::Remainder if b == 0 => {
                return Err(RuntimeError::new(
                    "panic",
                    operator.as_str(),
                    "integer divide by zero",
                ));
            }
            Operator::Divide => Data::Integer(a.wrapping_div(b)),
            Operator::Remainder => Data::Integer(a.wrapping_rem(b)),
            Operator::And => Data::Integer(a & b),
            Operator::Or => Data::Integer(a | b),
            Operator::Xor => Data::Integer(a ^ b),
            Operator::AndNot => Data::Integer(a & !b),
            Operator::Less => return Ok(Value::boolean(a < b)),
            Operator::LessEqual => return Ok(Value::boolean(a <= b)),
            Operator::Greater => return Ok(Value::boolean(a > b)),
            Operator::GreaterEqual => return Ok(Value::boolean(a >= b)),
            _ => return Err(invalid(operator.as_str())),
        },
        (&Data::Unsigned(a), &Data::Unsigned(b)) => match operator {
            Operator::Add => Data::Unsigned(a.wrapping_add(b)),
            Operator::Subtract => Data::Unsigned(a.wrapping_sub(b)),
            Operator::Multiply => Data::Unsigned(a.wrapping_mul(b)),
            Operator::Divide | Operator::Remainder if b == 0 => {
                return Err(RuntimeError::new(
                    "panic",
                    operator.as_str(),
                    "integer divide by zero",
                ));
            }
            Operator::Divide => Data::Unsigned(a / b),
            Operator::Remainder => Data::Unsigned(a % b),
            Operator::And => Data::Unsigned(a & b),
            Operator::Or => Data::Unsigned(a | b),
            Operator::Xor => Data::Unsigned(a ^ b),
            Operator::AndNot => Data::Unsigned(a & !b),
            Operator::Less => return Ok(Value::boolean(a < b)),
            Operator::LessEqual => return Ok(Value::boolean(a <= b)),
            Operator::Greater => return Ok(Value::boolean(a > b)),
            Operator::GreaterEqual => return Ok(Value::boolean(a >= b)),
            _ => return Err(invalid(operator.as_str())),
        },
        (&Data::Float(a), &Data::Float(b)) => match operator {
            Operator::Add => Data::Float(a + b),
            Operator::Subtract => Data::Float(a - b),
            Operator::Multiply => Data::Float(a * b),
            Operator::Divide => Data::Float(a / b),
            Operator::Less => return Ok(Value::boolean(a < b)),
            Operator::LessEqual => return Ok(Value::boolean(a <= b)),
            Operator::Greater => return Ok(Value::boolean(a > b)),
            Operator::GreaterEqual => return Ok(Value::boolean(a >= b)),
            _ => return Err(invalid(operator.as_str())),
        },
        (&Data::Bool(a), &Data::Bool(b)) => {
            return Ok(Value::boolean(match operator {
                Operator::LogicalAnd => a && b,
                Operator::LogicalOr => a || b,
                _ => return Err(invalid(operator.as_str())),
            }));
        }
        (Data::String(a), Data::String(b)) => match operator {
            Operator::Add => {
                let mut bytes = a.to_vec();
                bytes.extend_from_slice(b);
                Data::String(bytes.into())
            }
            Operator::Less => return Ok(Value::boolean(a < b)),
            Operator::LessEqual => return Ok(Value::boolean(a <= b)),
            Operator::Greater => return Ok(Value::boolean(a > b)),
            Operator::GreaterEqual => return Ok(Value::boolean(a >= b)),
            _ => return Err(invalid(operator.as_str())),
        },
        _ => return Err(invalid(operator.as_str())),
    };
    let mut value = normalize(Value { typ, data });
    value.typ = left.typ.clone();
    Ok(value)
}

fn numeric_operand_type(
    typ: &TypeIdentity,
    registry: &TypeRegistry,
    prepared: Option<u8>,
) -> Result<TypeIdentity, RuntimeError> {
    if let Some(kind) = prepared {
        if kind != 0 {
            return Ok(TypeIdentity::Primitive(kind));
        }
        return Ok(typ.clone());
    }
    let underlying = registry.underlying(typ)?;
    if matches!(underlying, TypeIdentity::Primitive(primitive) if (wire::PrimitiveInt..=wire::PrimitiveComplex128).contains(&primitive))
    {
        return Ok(underlying);
    }
    Ok(typ.clone())
}

pub(crate) fn equal(
    left: &Value,
    right: &Value,
    registry: &TypeRegistry,
) -> Result<bool, RuntimeError> {
    Ok(match (&left.data, &right.data) {
        (Data::Interface(a), Data::Interface(b)) => {
            if !registry.identical(&a.typ, &b.typ)? {
                false
            } else {
                if !registry.comparable(&a.typ)? {
                    return Err(RuntimeError::new(
                        "panic",
                        "compare",
                        "uncomparable dynamic value",
                    ));
                }
                equal(a, b, registry)?
            }
        }
        (Data::Interface(a), data) if !matches!(data, Data::Nil) => {
            registry.identical(&a.typ, &right.typ)? && equal(a, right, registry)?
        }
        (data, Data::Interface(b)) if !matches!(data, Data::Nil) => {
            registry.identical(&left.typ, &b.typ)? && equal(left, b, registry)?
        }
        (Data::ResourceRef(a), Data::ResourceRef(b)) => a == b,
        (Data::Nil, Data::Nil) => true,
        (Data::Nil, _) | (_, Data::Nil) => false,
        (Data::Bool(a), Data::Bool(b)) => a == b,
        (Data::Integer(a), Data::Integer(b)) => a == b,
        (Data::Unsigned(a), Data::Unsigned(b)) => a == b,
        (Data::Float(a), Data::Float(b)) => a == b,
        (Data::Complex { real: a, imag: b }, Data::Complex { real: c, imag: d }) => {
            a == c && b == d
        }
        (Data::String(a), Data::String(b)) => a == b,
        (Data::Pointer(a), Data::Pointer(b)) => a == b,
        (Data::Array(a), Data::Array(b))
            if registry.identical(&left.typ, &right.typ)? && a.len() == b.len() =>
        {
            for (a, b) in a.iter().zip(b.iter()) {
                if !equal(a, b, registry)? {
                    return Ok(false);
                }
            }
            true
        }
        (Data::Struct(a), Data::Struct(b))
            if registry.identical(&left.typ, &right.typ)? && a.len() == b.len() =>
        {
            for (name, value) in a {
                if name != "_"
                    && !b
                        .get(name)
                        .map(|other| equal(value, other, registry))
                        .transpose()?
                        .unwrap_or(false)
                {
                    return Ok(false);
                }
            }
            true
        }
        (
            Data::Map(_)
            | Data::Slice(_)
            | Data::Function(_)
            | Data::DynamicFunction(_)
            | Data::Method { .. },
            _,
        )
        | (
            _,
            Data::Map(_)
            | Data::Slice(_)
            | Data::Function(_)
            | Data::DynamicFunction(_)
            | Data::Method { .. },
        ) => {
            return Err(RuntimeError::new(
                "panic",
                "compare",
                "uncomparable dynamic value",
            ));
        }
        _ => false,
    })
}

pub(crate) fn convert(
    value: Value,
    typ: TypeIdentity,
    registry: &TypeRegistry,
) -> Result<Value, RuntimeError> {
    if typ == TypeIdentity::Any {
        return Ok(value);
    }
    let underlying = registry.underlying(&typ)?;
    if registry.channel_assignable(&value.typ, &typ)? {
        return Ok(Value {
            typ,
            data: value.data,
        });
    }
    let data = match (&underlying, value.data) {
        (TypeIdentity::Primitive(primitive), data)
            if (wire::PrimitiveInt..=wire::PrimitiveInt64).contains(primitive) =>
        {
            Data::Integer(match data {
                Data::Integer(value) => value,
                Data::Unsigned(value) => value as i64,
                Data::Float(value) => value as i64,
                _ => return Err(invalid("convert")),
            })
        }
        (TypeIdentity::Primitive(primitive), data)
            if (wire::PrimitiveUint..=wire::PrimitiveUintptr).contains(primitive) =>
        {
            Data::Unsigned(match data {
                Data::Integer(value) => value as u64,
                Data::Unsigned(value) => value,
                Data::Float(value) => value as u64,
                _ => return Err(invalid("convert")),
            })
        }
        (TypeIdentity::Primitive(wire::PrimitiveFloat32 | wire::PrimitiveFloat64), data) => {
            Data::Float(match data {
                Data::Integer(value) => value as f64,
                Data::Unsigned(value) => value as f64,
                Data::Float(value) => value,
                _ => return Err(invalid("convert")),
            })
        }
        (
            TypeIdentity::Primitive(wire::PrimitiveComplex64 | wire::PrimitiveComplex128),
            data @ Data::Complex { .. },
        ) => data,
        (TypeIdentity::Primitive(wire::PrimitiveString), Data::Integer(value)) => Data::String(
            char::from_u32(u32::try_from(value).unwrap_or(u32::MAX))
                .unwrap_or(char::REPLACEMENT_CHARACTER)
                .to_string()
                .into_bytes()
                .into(),
        ),
        (TypeIdentity::Primitive(wire::PrimitiveString), Data::Unsigned(value)) => Data::String(
            char::from_u32(u32::try_from(value).unwrap_or(u32::MAX))
                .unwrap_or(char::REPLACEMENT_CHARACTER)
                .to_string()
                .into_bytes()
                .into(),
        ),
        (_, data) if registry.identical(&underlying, &registry.underlying(&value.typ)?)? => data,
        _ => return Err(invalid("convert")),
    };
    let mut converted = normalize(Value {
        typ: underlying,
        data,
    });
    converted.typ = typ;
    Ok(converted)
}

fn normalize(mut value: Value) -> Value {
    value.data = match (&value.typ, value.data) {
        (TypeIdentity::Primitive(wire::PrimitiveInt8), Data::Integer(value)) => {
            Data::Integer(value as i8 as i64)
        }
        (TypeIdentity::Primitive(wire::PrimitiveInt16), Data::Integer(value)) => {
            Data::Integer(value as i16 as i64)
        }
        (TypeIdentity::Primitive(wire::PrimitiveInt32), Data::Integer(value)) => {
            Data::Integer(value as i32 as i64)
        }
        (TypeIdentity::Primitive(wire::PrimitiveUint8), Data::Unsigned(value)) => {
            Data::Unsigned(value as u8 as u64)
        }
        (TypeIdentity::Primitive(wire::PrimitiveUint16), Data::Unsigned(value)) => {
            Data::Unsigned(value as u16 as u64)
        }
        (TypeIdentity::Primitive(wire::PrimitiveUint32), Data::Unsigned(value)) => {
            Data::Unsigned(value as u32 as u64)
        }
        (TypeIdentity::Primitive(wire::PrimitiveFloat32), Data::Float(value)) => {
            Data::Float(value as f32 as f64)
        }
        (TypeIdentity::Primitive(wire::PrimitiveComplex64), Data::Complex { real, imag }) => {
            Data::Complex {
                real: real as f32 as f64,
                imag: imag as f32 as f64,
            }
        }
        (_, data) => data,
    };
    value
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn prepared_numeric_operands_preserve_named_types_and_arithmetic_boundaries() {
        let cases = [
            (
                wire::PrimitiveInt,
                "+",
                Data::Integer(i64::MAX),
                Data::Integer(1),
                Data::Integer(i64::MIN),
            ),
            (
                wire::PrimitiveInt,
                "-",
                Data::Integer(i64::MIN),
                Data::Integer(1),
                Data::Integer(i64::MAX),
            ),
            (
                wire::PrimitiveInt,
                "*",
                Data::Integer(i64::MAX),
                Data::Integer(2),
                Data::Integer(-2),
            ),
            (
                wire::PrimitiveInt,
                "<",
                Data::Integer(-1),
                Data::Integer(1),
                Data::Bool(true),
            ),
            (
                wire::PrimitiveInt,
                "<=",
                Data::Integer(1),
                Data::Integer(1),
                Data::Bool(true),
            ),
            (
                wire::PrimitiveInt,
                ">",
                Data::Integer(-1),
                Data::Integer(1),
                Data::Bool(false),
            ),
            (
                wire::PrimitiveInt,
                ">=",
                Data::Integer(1),
                Data::Integer(1),
                Data::Bool(true),
            ),
            (
                wire::PrimitiveInt8,
                "+",
                Data::Integer(127),
                Data::Integer(1),
                Data::Integer(-128),
            ),
            (
                wire::PrimitiveInt8,
                "/",
                Data::Integer(-128),
                Data::Integer(-1),
                Data::Integer(-128),
            ),
            (
                wire::PrimitiveInt8,
                ">>",
                Data::Integer(-128),
                Data::Unsigned(64),
                Data::Integer(-1),
            ),
            (
                wire::PrimitiveUint8,
                "+",
                Data::Unsigned(255),
                Data::Unsigned(1),
                Data::Unsigned(0),
            ),
            (
                wire::PrimitiveUint8,
                "<<",
                Data::Unsigned(255),
                Data::Unsigned(64),
                Data::Unsigned(0),
            ),
            (
                wire::PrimitiveFloat32,
                "+",
                Data::Float(16_777_216.0),
                Data::Float(1.0),
                Data::Float(16_777_216.0),
            ),
            (
                wire::PrimitiveInt8,
                "<",
                Data::Integer(-1),
                Data::Integer(1),
                Data::Bool(true),
            ),
            (
                wire::PrimitiveComplex64,
                "+",
                Data::Complex {
                    real: 16_777_216.0,
                    imag: 1.0,
                },
                Data::Complex {
                    real: 1.0,
                    imag: 2.0,
                },
                Data::Complex {
                    real: 16_777_216.0,
                    imag: 3.0,
                },
            ),
        ];
        for (kind, operator, left, right, expected) in cases {
            let identity = wire::TypeKey {
                module_path: "sample".into(),
                decl_id: "Number".into(),
            };
            let artifact = wire::Artifact {
                module: wire::Module {
                    path: "sample".into(),
                    ..Default::default()
                },
                type_table: wire::TypeTable {
                    nodes: crate::contract::GoSlice(Some(vec![wire::TypeNode {
                        id: "number".into(),
                        kind: wire::Named,
                        identity: identity.clone(),
                        underlying: wire::TypeRef {
                            kind: wire::Primitive,
                            primitive: kind,
                            ..Default::default()
                        },
                        ..Default::default()
                    }])),
                },
                ..Default::default()
            };
            let registry = TypeRegistry::new([&artifact]).unwrap();
            let typ = TypeIdentity::Named(std::sync::Arc::new(identity));
            let right_type = if operator == "<<" || operator == ">>" {
                TypeIdentity::Primitive(wire::PrimitiveUint64)
            } else {
                typ.clone()
            };
            let left = Value {
                typ: typ.clone(),
                data: left,
            };
            let right = Value {
                typ: right_type,
                data: right,
            };
            let dynamic = PreparedOperator::parse(operator).unwrap();
            let mut prepared = dynamic;
            prepared.bind(0, &left.typ, &registry).unwrap();
            prepared.bind(1, &right.typ, &registry).unwrap();
            for operation in [dynamic, prepared] {
                let value = binary(operation, &left, &right, &registry).unwrap();
                assert_eq!(format!("{:?}", value.data), format!("{expected:?}"));
                assert_eq!(
                    value.typ,
                    if matches!(expected, Data::Bool(_)) {
                        TypeIdentity::Primitive(wire::PrimitiveBool)
                    } else {
                        typ.clone()
                    }
                );
            }
        }
    }

    #[test]
    fn prepared_numeric_failures_remain_guest_panics() {
        let registry = TypeRegistry::new(std::iter::empty()).unwrap();
        for (operator, right) in [("/", 0), ("%", 0), ("<<", -1), (">>", -1)] {
            let mut operation = PreparedOperator::parse(operator).unwrap();
            operation
                .bind(0, &TypeIdentity::Primitive(wire::PrimitiveInt), &registry)
                .unwrap();
            operation
                .bind(1, &TypeIdentity::Primitive(wire::PrimitiveInt), &registry)
                .unwrap();
            let error =
                binary(operation, &Value::int(1), &Value::int(right), &registry).unwrap_err();
            assert_eq!(error.code, "panic");
        }
        let typ = TypeIdentity::Primitive(wire::PrimitiveInt8);
        let mut operation = PreparedOperator::parse("-").unwrap();
        operation.bind(0, &typ, &registry).unwrap();
        let value = unary(
            operation,
            Value {
                typ: typ.clone(),
                data: Data::Integer(-128),
            },
            &registry,
        )
        .unwrap();
        assert_eq!(value.typ, typ);
        assert!(matches!(value.data, Data::Integer(-128)));
    }
}
