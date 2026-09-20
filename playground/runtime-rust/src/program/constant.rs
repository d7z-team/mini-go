//! Constants are validated and decoded once, before publishing a Program.

use crate::{
    contract_generated as wire,
    error::RuntimeError,
    types::{TypeIdentity, TypeRegistry},
    value::{Data, Value},
};
use base64::{Engine, engine::general_purpose::STANDARD};

pub(crate) enum PreparedConstant {
    Scalar(Value),
    Bytes { typ: TypeIdentity, bytes: Vec<u8> },
    Metadata,
    Failure(RuntimeError),
}

impl PreparedConstant {
    pub(super) fn decode(
        registry: &TypeRegistry,
        module: &str,
        constant: &wire::Constant,
    ) -> Result<Self, RuntimeError> {
        let typ = registry.resolve(module, &constant.r#type)?;
        let underlying = registry.underlying(&typ)?;
        let raw = constant
            .value
            .as_ref()
            .map(|value| value.get())
            .unwrap_or("null");
        let invalid = || {
            RuntimeError::new(
                "invalid_constant",
                format!("{module}/{}", constant.id),
                "constant value does not match its declared type",
            )
        };
        let text = if raw.starts_with('"') {
            serde_json::from_str::<String>(raw)?
        } else {
            raw.to_owned()
        };
        if constant.untyped {
            let valid = match underlying {
                TypeIdentity::Primitive(primitive)
                    if (wire::PrimitiveInt..=wire::PrimitiveUintptr).contains(&primitive) =>
                {
                    let text = text.trim();
                    let digits = text
                        .strip_prefix('-')
                        .or_else(|| text.strip_prefix('+'))
                        .unwrap_or(text);
                    !digits.is_empty()
                        && digits.bytes().all(|byte| byte.is_ascii_digit())
                        && (primitive < wire::PrimitiveUint || !text.starts_with('-'))
                }
                TypeIdentity::Primitive(wire::PrimitiveFloat32 | wire::PrimitiveFloat64) => {
                    raw.starts_with('"') && exact_rational(&text)
                }
                TypeIdentity::Primitive(wire::PrimitiveComplex64 | wire::PrimitiveComplex128) => {
                    #[derive(serde::Deserialize)]
                    #[serde(deny_unknown_fields)]
                    struct Parts {
                        real: String,
                        imag: String,
                    }
                    serde_json::from_str::<Parts>(raw).is_ok_and(|parts| {
                        exact_rational(&parts.real) && exact_rational(&parts.imag)
                    })
                }
                TypeIdentity::Primitive(wire::PrimitiveBool) => {
                    serde_json::from_str::<bool>(raw).is_ok()
                }
                TypeIdentity::Primitive(wire::PrimitiveString) => raw.starts_with('"'),
                TypeIdentity::Any => true,
                _ => false,
            };
            return if valid {
                Ok(Self::Metadata)
            } else {
                Err(invalid())
            };
        }
        if raw == "null" {
            let nilable = matches!(
                underlying,
                TypeIdentity::Any
                    | TypeIdentity::Pointer(_)
                    | TypeIdentity::Slice(_)
                    | TypeIdentity::Primitive(wire::PrimitiveError | wire::PrimitiveFunction)
            ) || registry.node(&typ)?.is_some_and(|(_, node)| {
                matches!(
                    node.kind,
                    wire::Slice
                        | wire::Map
                        | wire::Pointer
                        | wire::Waitable
                        | wire::Function
                        | wire::Interface
                )
            });
            return if nilable {
                Ok(Self::Scalar(Value {
                    typ,
                    data: Data::Nil,
                }))
            } else {
                Err(invalid())
            };
        }
        if underlying == TypeIdentity::Any {
            let parsed: serde_json::Value = serde_json::from_str(raw)?;
            let number = match &parsed {
                serde_json::Value::Number(number) => Some(number.to_string()),
                serde_json::Value::String(text)
                    if serde_json::from_str::<serde_json::Number>(text).is_ok() =>
                {
                    Some(text.clone())
                }
                _ => None,
            };
            let data = if let Some(number) = number {
                match number.parse::<i64>() {
                    Ok(number) => Data::Integer(number),
                    Err(_) => return Ok(Self::Failure(invalid())),
                }
            } else {
                match parsed {
                    serde_json::Value::Bool(value) => Data::Bool(value),
                    serde_json::Value::String(value) => Data::String(value.into_bytes().into()),
                    parsed => {
                        let mut pending = vec![&parsed];
                        while let Some(value) = pending.pop() {
                            match value {
                                serde_json::Value::Number(number)
                                    if !number.as_f64().is_some_and(f64::is_finite) =>
                                {
                                    return Ok(Self::Failure(invalid()));
                                }
                                serde_json::Value::Array(values) => pending.extend(values),
                                serde_json::Value::Object(values) => {
                                    pending.extend(values.values())
                                }
                                _ => {}
                            }
                        }
                        Data::OpaqueJSON(raw.as_bytes().into())
                    }
                }
            };
            return Ok(Self::Scalar(Value { typ, data }));
        }
        let data = match underlying {
            TypeIdentity::Primitive(wire::PrimitiveBool) => {
                Data::Bool(serde_json::from_str(raw).map_err(|_| invalid())?)
            }
            TypeIdentity::Primitive(wire::PrimitiveString) => Data::String(
                serde_json::from_str::<String>(raw)
                    .map_err(|_| invalid())?
                    .into_bytes()
                    .into(),
            ),
            TypeIdentity::Primitive(primitive)
                if (wire::PrimitiveInt..=wire::PrimitiveInt64).contains(&primitive) =>
            {
                let value: i64 = text.trim().parse().map_err(|_| invalid())?;
                let valid = match primitive {
                    wire::PrimitiveInt8 => i8::try_from(value).is_ok(),
                    wire::PrimitiveInt16 => i16::try_from(value).is_ok(),
                    wire::PrimitiveInt32 => i32::try_from(value).is_ok(),
                    _ => true,
                };
                if !valid {
                    return Err(invalid());
                }
                Data::Integer(value)
            }
            TypeIdentity::Primitive(primitive)
                if (wire::PrimitiveUint..=wire::PrimitiveUintptr).contains(&primitive) =>
            {
                let value: u64 = text.trim().parse().map_err(|_| invalid())?;
                let valid = match primitive {
                    wire::PrimitiveUint8 => u8::try_from(value).is_ok(),
                    wire::PrimitiveUint16 => u16::try_from(value).is_ok(),
                    wire::PrimitiveUint32 => u32::try_from(value).is_ok(),
                    _ => true,
                };
                if !valid {
                    return Err(invalid());
                }
                Data::Unsigned(value)
            }
            TypeIdentity::Primitive(
                primitive @ (wire::PrimitiveFloat32 | wire::PrimitiveFloat64),
            ) => {
                let mut value: f64 = text.trim().parse().map_err(|_| invalid())?;
                if primitive == wire::PrimitiveFloat32 {
                    value = value as f32 as f64;
                }
                if !value.is_finite() {
                    return Err(invalid());
                }
                Data::Float(value)
            }
            TypeIdentity::Primitive(
                primitive @ (wire::PrimitiveComplex64 | wire::PrimitiveComplex128),
            ) => {
                #[derive(serde::Deserialize)]
                #[serde(deny_unknown_fields)]
                struct Parts {
                    real: f64,
                    imag: f64,
                }
                let mut parts: Parts = serde_json::from_str(raw).map_err(|_| invalid())?;
                if primitive == wire::PrimitiveComplex64 {
                    parts.real = parts.real as f32 as f64;
                    parts.imag = parts.imag as f32 as f64;
                }
                if !parts.real.is_finite() || !parts.imag.is_finite() {
                    return Err(invalid());
                }
                Data::Complex {
                    real: parts.real,
                    imag: parts.imag,
                }
            }
            _ => {
                if let Some((module, node)) = registry.node(&typ)?
                    && node.kind == wire::Slice
                    && registry.resolve(module, &node.elem)?
                        == TypeIdentity::Primitive(wire::PrimitiveUint8)
                {
                    let encoded: String = serde_json::from_str(raw).map_err(|_| invalid())?;
                    let bytes = STANDARD.decode(&encoded).map_err(|_| invalid())?;
                    if STANDARD.encode(&bytes) != encoded {
                        return Err(invalid());
                    }
                    return Ok(Self::Bytes { typ, bytes });
                }
                return Err(invalid());
            }
        };
        Ok(Self::Scalar(Value { typ, data }))
    }
}

fn exact_rational(text: &str) -> bool {
    let Some((numerator, denominator)) = text.split_once('/') else {
        return false;
    };
    let unsigned = |text: &str| {
        !text.is_empty()
            && (text.len() == 1 || !text.starts_with('0'))
            && text.bytes().all(|byte| byte.is_ascii_digit())
    };
    let numerator_valid = if let Some(digits) = numerator.strip_prefix('-') {
        digits != "0" && unsigned(digits)
    } else {
        unsigned(numerator)
    };
    numerator_valid
        && unsigned(denominator)
        && denominator != "0"
        && (numerator != "0" || denominator == "1")
}
