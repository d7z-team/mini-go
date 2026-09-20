//! Reflection snapshots retain explicit guest addresses for live, writable views.

use super::*;

pub(super) struct ReflectedValue {
    pub(super) current: Option<Value>,
    target: Option<Address>,
    addressable: bool,
    settable: bool,
    interfaceable: bool,
    embedded_read_only: bool,
}

impl Instance {
    fn reflect_deep_equal(&mut self, left: Value, right: Value) -> Result<bool, RuntimeError> {
        let mut pending = vec![(left, right)];
        let mut pointers = HashSet::new();
        let mut maps = HashSet::new();
        let mut slices = HashSet::new();
        while let Some((left, right)) = pending.pop() {
            if !self.types.identical(&left.typ, &right.typ)? {
                return Ok(false);
            }
            match (left.data, right.data) {
                (Data::Interface(left), Data::Interface(right)) => pending.push((*left, *right)),
                (Data::Array(left), Data::Array(right)) => {
                    if left.len() != right.len() {
                        return Ok(false);
                    }
                    pending.extend(left.into_iter().zip(right));
                }
                (Data::Struct(left), Data::Struct(right)) => {
                    if left.len() != right.len() {
                        return Ok(false);
                    }
                    for (name, value) in &left {
                        let Some(other) = right.get(name) else {
                            return Ok(false);
                        };
                        pending.push((value.clone(), other.clone()));
                    }
                }
                (Data::Pointer(left), Data::Pointer(right)) => {
                    if left != right && pointers.insert((left.clone(), right.clone())) {
                        pending.push((self.read_address(&left)?, self.read_address(&right)?));
                    }
                }
                (Data::Map(left), Data::Map(right)) => {
                    if left == right || !maps.insert((left, right)) {
                        continue;
                    }
                    let (Data::MapEntries(left), Data::MapEntries(right)) =
                        (&self.heap.get(left)?.data, &self.heap.get(right)?.data)
                    else {
                        unreachable!()
                    };
                    if left.len() != right.len() {
                        return Ok(false);
                    }
                    for (key, value) in left {
                        let Some(index) = right.find(key, &self.types)? else {
                            return Ok(false);
                        };
                        pending.push((value.clone(), right[index].1.clone()));
                    }
                }
                (Data::Slice(left), Data::Slice(right)) => {
                    if left.length != right.length {
                        return Ok(false);
                    }
                    let left_backing = self.snapshot_address(&left.storage)?;
                    let right_backing = self.snapshot_address(&right.storage)?;
                    if let (Data::Bytes(a), Data::Bytes(b)) =
                        (&left_backing.data, &right_backing.data)
                    {
                        let invalid = || {
                            RuntimeError::new(
                                "invalid_slice",
                                "reflect",
                                "byte view exceeds backing",
                            )
                        };
                        let a = a
                            .get(left.start..left.start + left.length)
                            .ok_or_else(invalid)?;
                        let b = b
                            .get(right.start..right.start + right.length)
                            .ok_or_else(invalid)?;
                        if a != b {
                            return Ok(false);
                        }
                        continue;
                    }
                    let pair = (
                        (left.storage.clone(), left.start, left.length),
                        (right.storage.clone(), right.start, right.length),
                    );
                    if pair.0 == pair.1 || !slices.insert(pair) {
                        continue;
                    }
                    for index in 0..left.length {
                        let mut left_address = left.storage.clone();
                        left_address
                            .path
                            .push(PathElement::Index(left.start + index));
                        let mut right_address = right.storage.clone();
                        right_address
                            .path
                            .push(PathElement::Index(right.start + index));
                        pending.push((
                            self.read_address(&left_address)?,
                            self.read_address(&right_address)?,
                        ));
                    }
                }
                (Data::Function(_) | Data::DynamicFunction(_) | Data::Method { .. }, _)
                | (_, Data::Function(_) | Data::DynamicFunction(_) | Data::Method { .. }) => {
                    return Ok(false);
                }
                (left_data, right_data) => {
                    if !crate::operators::equal(
                        &Value {
                            typ: left.typ,
                            data: left_data,
                        },
                        &Value {
                            typ: right.typ,
                            data: right_data,
                        },
                        &self.types,
                    )? {
                        return Ok(false);
                    }
                }
            }
        }
        Ok(true)
    }

    pub(super) fn reflected_value(
        &mut self,
        value: &Value,
    ) -> Result<ReflectedValue, RuntimeError> {
        if !matches!(&value.typ, TypeIdentity::Named(key) if key.module_path == "reflect" && key.decl_id == "Value")
        {
            return Err(RuntimeError::new(
                "reflect",
                "Value",
                "reflect: expected Value",
            ));
        }
        let Data::Struct(fields) = &value.data else {
            return Err(RuntimeError::new(
                "reflect",
                "Value",
                "reflect: invalid Value",
            ));
        };
        for (name, primitive) in [
            ("valid", wire::PrimitiveBool),
            ("addressable", wire::PrimitiveBool),
            ("settable", wire::PrimitiveBool),
            ("interfaceable", wire::PrimitiveBool),
            ("embeddedReadOnly", wire::PrimitiveBool),
            ("zero", wire::PrimitiveBool),
            ("nilValue", wire::PrimitiveBool),
            ("length", wire::PrimitiveInt),
            ("capacity", wire::PrimitiveInt),
            ("boolValue", wire::PrimitiveBool),
            ("intValue", wire::PrimitiveInt64),
            ("uintValue", wire::PrimitiveUint64),
            ("floatValue", wire::PrimitiveFloat64),
            ("complexValue", wire::PrimitiveComplex128),
            ("stringValue", wire::PrimitiveString),
        ] {
            if fields
                .get(name)
                .is_some_and(|value| value.typ != TypeIdentity::Primitive(primitive))
            {
                return Err(RuntimeError::new(
                    "reflect",
                    "Value",
                    format!("reflect: Value payload field {name} has invalid type"),
                ));
            }
        }
        if fields
            .get("data")
            .is_none_or(|value| value.typ != TypeIdentity::Any)
            || fields
                .get("target")
                .is_some_and(|value| value.typ != TypeIdentity::Any)
        {
            return Err(RuntimeError::new(
                "reflect",
                "Value",
                "reflect: invalid interface payload",
            ));
        }
        let type_value = fields
            .get("valueType")
            .ok_or_else(|| RuntimeError::new("reflect", "Value", "reflect: missing valueType"))?;
        if !matches!(&type_value.typ, TypeIdentity::Named(key) if key.module_path == "reflect" && key.decl_id == "Type")
            && !self
                .types
                .node(&type_value.typ)?
                .is_some_and(|(_, node)| node.kind == wire::Interface)
        {
            return Err(RuntimeError::new(
                "reflect",
                "Value",
                "reflect: invalid valueType",
            ));
        }
        if let Some(entries) = fields.get("mapEntries")
            && !matches!(entries.typ, TypeIdentity::Slice(_))
            && !self
                .types
                .node(&entries.typ)?
                .is_some_and(|(_, node)| node.kind == wire::Slice)
        {
            return Err(RuntimeError::new(
                "reflect",
                "Value",
                "reflect: invalid mapEntries",
            ));
        }
        let flag = |name: &str| {
            fields
                .get(name)
                .is_some_and(|value| matches!(value.data, Data::Bool(true)))
        };
        let target = match fields.get("target").map(|value| &value.data) {
            Some(Data::Interface(value)) => match &value.data {
                Data::Pointer(address) => Some(address.clone()),
                _ => {
                    return Err(RuntimeError::new(
                        "reflect",
                        "Value",
                        "reflect: invalid target",
                    ));
                }
            },
            _ => None,
        };
        let current = if !flag("valid") {
            None
        } else if let Some(target) = &target {
            Some(self.read_address(target)?)
        } else {
            match fields.get("data").map(|value| &value.data) {
                Some(Data::Interface(value)) => Some((**value).clone()),
                Some(Data::Nil) => Some(self.zero(&self.reflected_type(&fields["valueType"])?, 0)?),
                _ => {
                    return Err(RuntimeError::new(
                        "reflect",
                        "Value",
                        "reflect: invalid data",
                    ));
                }
            }
        };
        Ok(ReflectedValue {
            current,
            target,
            addressable: flag("addressable"),
            settable: flag("settable"),
            interfaceable: flag("interfaceable"),
            embedded_read_only: flag("embeddedReadOnly"),
        })
    }

    pub(super) fn reflect_snapshot(
        &mut self,
        view: ReflectedValue,
        depth: usize,
    ) -> Result<Value, RuntimeError> {
        let Some(value) = view.current else {
            return self.reflect_struct("Value", []);
        };
        if depth >= self.limits.max_value_depth {
            return Err(RuntimeError::new(
                "value_limit",
                "reflect",
                "snapshot depth exceeded",
            ));
        }
        let mut pending = vec![&value];
        let mut zero = true;
        while let Some(value) = pending.pop() {
            match &value.data {
                Data::Nil => (),
                Data::Bool(value) => zero &= !value,
                Data::Integer(value) => zero &= *value == 0,
                Data::Unsigned(value) => zero &= *value == 0,
                Data::Float(value) => zero &= *value == 0.0,
                Data::Complex { real, imag } => zero &= *real == 0.0 && *imag == 0.0,
                Data::String(value) => zero &= value.is_empty(),
                Data::Array(values) => pending.extend(values),
                Data::Struct(fields) => pending.extend(
                    fields
                        .iter()
                        .filter(|(name, _)| *name != "_")
                        .map(|(_, value)| value),
                ),
                _ => zero = false,
            }
            if !zero {
                break;
            }
        }
        let (length, capacity) = match &value.data {
            Data::String(value) => (value.len(), 0),
            Data::Array(value) => (value.len(), value.len()),
            Data::Struct(value) => (value.len(), 0),
            Data::Slice(value) => (value.length, value.capacity),
            Data::Map(handle) => match &self.heap.get(*handle)?.data {
                Data::MapEntries(entries) => (entries.len(), 0),
                _ => unreachable!(),
            },
            _ => (0, 0),
        };
        let mut entries = Vec::new();
        if let Data::Map(handle) = &value.data {
            let Data::MapEntries(map) = self.heap.get(*handle)?.data.clone() else {
                unreachable!()
            };
            for (key, value) in map {
                let key = self.reflect_snapshot(ReflectedValue::owned(key), depth + 1)?;
                let value = self.reflect_snapshot(ReflectedValue::owned(value), depth + 1)?;
                entries
                    .push(self.reflect_struct("valueMapEntry", [("Key", key), ("Value", value)])?);
            }
        }
        let typ = self.reflect_value_type(&value)?;
        let value_type = self.reflect_type(&typ)?;
        let target = match view.target {
            Some(address) => Value {
                typ: TypeIdentity::Any,
                data: Data::Interface(Box::new(Value {
                    typ: TypeIdentity::Pointer(std::sync::Arc::new(value.typ.clone())),
                    data: Data::Pointer(address),
                })),
            },
            None => self.zero(&TypeIdentity::Any, 0)?,
        };
        let mut fields = vec![
            ("valid", Value::boolean(true)),
            ("valueType", value_type),
            (
                "data",
                Value {
                    typ: TypeIdentity::Any,
                    data: Data::Interface(Box::new(value.clone())),
                },
            ),
            ("target", target),
            ("addressable", Value::boolean(view.addressable)),
            ("settable", Value::boolean(view.settable)),
            ("interfaceable", Value::boolean(view.interfaceable)),
            ("embeddedReadOnly", Value::boolean(view.embedded_read_only)),
            ("zero", Value::boolean(zero)),
            ("nilValue", Value::boolean(matches!(value.data, Data::Nil))),
            ("length", Value::int(length as i64)),
            ("capacity", Value::int(capacity as i64)),
        ];
        let scalar = match value.data {
            Data::Bool(_) => Some(("boolValue", wire::PrimitiveBool)),
            Data::Integer(_) => Some(("intValue", wire::PrimitiveInt64)),
            Data::Unsigned(_) => Some(("uintValue", wire::PrimitiveUint64)),
            Data::Float(_) => Some(("floatValue", wire::PrimitiveFloat64)),
            Data::Complex { .. } => Some(("complexValue", wire::PrimitiveComplex128)),
            Data::String(_) => Some(("stringValue", wire::PrimitiveString)),
            _ => None,
        };
        if let Some((name, primitive)) = scalar {
            fields.push((
                name,
                Value {
                    typ: TypeIdentity::Primitive(primitive),
                    data: value.data,
                },
            ));
        }
        if !entries.is_empty() {
            let typ = TypeIdentity::Slice(std::sync::Arc::new(TypeIdentity::Named(
                std::sync::Arc::new(wire::TypeKey {
                    module_path: "reflect".to_owned(),
                    decl_id: "valueMapEntry".to_owned(),
                }),
            )));
            fields.push((
                "mapEntries",
                self.make_slice(typ, entries.len(), entries.len(), entries)?,
            ));
        }
        self.reflect_struct("Value", fields)
    }

    pub(super) fn execute_reflect_value(
        &mut self,
        id: &str,
        mut arguments: Vec<Value>,
    ) -> Result<Vec<Value>, RuntimeError> {
        if id == "reflect.deep_equal" {
            let right = arguments.pop().unwrap();
            let left = arguments.pop().unwrap();
            return Ok(vec![Value::boolean(self.reflect_deep_equal(left, right)?)]);
        }
        if id == "reflect.same_reference" || id == "reflect.value_equal" {
            let left = self.reflected_value(&arguments[0])?.current;
            let right = self.reflected_value(&arguments[1])?.current;
            let answer = match (left, right) {
                (Some(left), Some(right)) if self.types.identical(&left.typ, &right.typ)? => {
                    if id == "reflect.value_equal" {
                        crate::operators::equal(&left, &right, &self.types)
                    } else {
                        Ok(match (&left.data, &right.data) {
                            (Data::Pointer(left), Data::Pointer(right)) => left == right,
                            (Data::Map(left), Data::Map(right)) => left == right,
                            (Data::Slice(left), Data::Slice(right)) => {
                                left.storage == right.storage
                                    && left.start == right.start
                                    && left.length == right.length
                            }
                            (Data::Nil, Data::Nil) => {
                                matches!(
                                    left.typ,
                                    TypeIdentity::Pointer(_) | TypeIdentity::Slice(_)
                                ) || self.types.node(&left.typ)?.is_some_and(|(_, node)| {
                                    matches!(node.kind, wire::Pointer | wire::Slice | wire::Map)
                                })
                            }
                            _ => false,
                        })
                    }
                }
                (None, None) => Ok(id == "reflect.value_equal"),
                _ => Ok(false),
            };
            if id == "reflect.same_reference" {
                return Ok(vec![Value::boolean(answer?)]);
            }
            return match answer {
                Ok(answer) => Ok(vec![
                    Value::boolean(answer),
                    Value::string(""),
                    Value::boolean(true),
                ]),
                Err(error) => Ok(vec![
                    Value::boolean(false),
                    Value::string(error.message),
                    Value::boolean(false),
                ]),
            };
        }
        if id == "reflect.value_of" {
            let mut value = arguments.remove(0);
            while let Data::Interface(inner) = value.data {
                value = *inner;
            }
            let view = if matches!(value.data, Data::Nil)
                && (value.typ == TypeIdentity::Any
                    || self
                        .types
                        .node(&value.typ)?
                        .is_some_and(|(_, node)| node.kind == wire::Interface))
            {
                ReflectedValue {
                    current: None,
                    ..ReflectedValue::owned(value)
                }
            } else {
                ReflectedValue::owned(value)
            };
            return Ok(vec![self.reflect_snapshot(view, 0)?]);
        }
        let mutation = matches!(
            id,
            "reflect.value_set"
                | "reflect.value_set_len"
                | "reflect.value_set_cap"
                | "reflect.value_swap"
                | "reflect.value_set_map_index"
                | "reflect.value_grow"
        );
        let type_result = matches!(id, "reflect.pointer_to" | "reflect.slice_of");
        let outcome = (|| -> Result<Option<Value>, RuntimeError> {
            if type_result {
                let element = self.reflected_type(&arguments[0])?;
                let typ = if id == "reflect.pointer_to" {
                    TypeIdentity::Pointer(std::sync::Arc::new(element))
                } else {
                    TypeIdentity::Slice(std::sync::Arc::new(element))
                };
                self.publish_dynamic_type(self.types.clone(), &typ)?;
                return Ok(Some(self.reflect_type(&typ)?));
            }
            if matches!(
                id,
                "reflect.zero" | "reflect.new" | "reflect.make_slice" | "reflect.make_map"
            ) {
                let typ = self.reflected_type(&arguments[0])?;
                let mut value = self.zero(&typ, 0)?;
                let mut target = None;
                if id == "reflect.make_slice" {
                    let length = usize::try_from(arguments[1].integer()?).map_err(|_| {
                        RuntimeError::new("reflect", id, "reflect: negative length")
                    })?;
                    let capacity = usize::try_from(arguments[2].integer()?).map_err(|_| {
                        RuntimeError::new("reflect", id, "reflect: negative capacity")
                    })?;
                    if length > capacity || capacity > self.limits.max_sequence_elements {
                        return Err(RuntimeError::new(
                            "value_limit",
                            id,
                            "invalid slice dimensions",
                        ));
                    }
                    self.charge_guest_object(capacity, 0)?;
                    value = self.make_slot_slice(typ.clone(), length, capacity, Vec::new())?;
                    target = Some(Address {
                        identity: std::sync::Arc::default(),
                        root: self.allocate(value.clone())?,
                        path: Vec::new(),
                    });
                } else if id == "reflect.make_map" {
                    if !self
                        .types
                        .node(&typ)?
                        .is_some_and(|(_, node)| node.kind == wire::Map)
                    {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect: MakeMap of non-map type",
                        ));
                    }
                    self.charge_guest_object(0, 0)?;
                    value = Value {
                        typ,
                        data: Data::Map(self.allocate(Value {
                            typ: TypeIdentity::Any,
                            data: Data::MapEntries(Default::default()),
                        })?),
                    };
                    target = Some(Address {
                        identity: std::sync::Arc::default(),
                        root: self.allocate(value.clone())?,
                        path: Vec::new(),
                    });
                } else if id == "reflect.new" {
                    let pointer = TypeIdentity::Pointer(std::sync::Arc::new(typ));
                    self.publish_dynamic_type(self.types.clone(), &pointer)?;
                    value = Value {
                        typ: pointer,
                        data: Data::Pointer(Address {
                            identity: std::sync::Arc::default(),
                            root: self.allocate(value)?,
                            path: Vec::new(),
                        }),
                    };
                }
                let mut view = ReflectedValue::owned(value);
                if let Some(target) = target {
                    view.target = Some(target);
                    view.addressable = true;
                    view.settable = true;
                }
                return Ok(Some(self.reflect_snapshot(view, 0)?));
            }
            let mut view = self.reflected_value(&arguments[0])?;
            match id {
                "reflect.value_current" => (),
                "reflect.value_slice" | "reflect.value_slice3" => {
                    let mut current = view.current.take().ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: Slice of invalid Value")
                    })?;
                    if matches!(current.data, Data::Array(_)) {
                        let address = view.target.take().ok_or_else(|| {
                            RuntimeError::new(
                                "reflect",
                                id,
                                "reflect: Slice of unaddressable array",
                            )
                        })?;
                        current = Value {
                            typ: TypeIdentity::Pointer(std::sync::Arc::new(current.typ)),
                            data: Data::Pointer(address),
                        };
                    }
                    let max = if id == "reflect.value_slice3" {
                        arguments[3].clone()
                    } else {
                        Value {
                            typ: TypeIdentity::Void,
                            data: Data::Nil,
                        }
                    };
                    let value = self.slice_value(
                        current,
                        arguments[1].integer()?,
                        arguments[2].integer()?,
                        max,
                    )?;
                    view.target = None;
                    view.addressable = false;
                    view.settable = false;
                    view.current = Some(value);
                }
                "reflect.value_set_map_index" => {
                    if !view.interfaceable {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect: SetMapIndex using unexported field",
                        ));
                    }
                    let current = view.current.ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: invalid map Value")
                    })?;
                    let key = self
                        .reflected_value(&arguments[1])?
                        .current
                        .ok_or_else(|| {
                            RuntimeError::new("reflect", id, "reflect: invalid map key Value")
                        })?;
                    match self.reflected_value(&arguments[2])?.current {
                        Some(value) => self.store_index(current, key, value)?,
                        None => self.delete_key(current, key)?,
                    }
                    return Ok(None);
                }
                "reflect.value_grow" => {
                    if !view.settable {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect: Grow of unsettable Value",
                        ));
                    }
                    let current = view.current.ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: Grow of invalid Value")
                    })?;
                    let count = usize::try_from(arguments[1].integer()?).map_err(|_| {
                        RuntimeError::new("reflect", id, "reflect: Grow: negative len")
                    })?;
                    let (length, capacity) = match &current.data {
                        Data::Slice(slice) => (slice.length, slice.capacity),
                        Data::Nil
                            if self
                                .types
                                .node(&current.typ)?
                                .is_some_and(|(_, node)| node.kind == wire::Slice)
                                || matches!(current.typ, TypeIdentity::Slice(_)) =>
                        {
                            (0, 0)
                        }
                        _ => {
                            return Err(RuntimeError::new(
                                "reflect",
                                id,
                                "reflect: Grow of non-slice Value",
                            ));
                        }
                    };
                    if count > capacity - length {
                        let new_capacity = length
                            .checked_add(count)
                            .map(|needed| needed.max(capacity.saturating_mul(2)))
                            .ok_or_else(|| {
                                RuntimeError::new("value_limit", id, "slice capacity overflow")
                            })?;
                        if new_capacity > self.limits.max_sequence_elements {
                            return Err(RuntimeError::new(
                                "value_limit",
                                id,
                                "slice capacity exceeds limit",
                            ));
                        }
                        self.charge_guest((new_capacity - capacity) as u64 * 16)?;
                        let compact = match &current.data {
                            Data::Slice(slice) => {
                                matches!(
                                    self.snapshot_address(&slice.storage)?.data,
                                    Data::Bytes(_)
                                )
                            }
                            _ => false,
                        };
                        let grown = if compact {
                            let bytes = self.slice_bytes(&current)?;
                            self.make_bytes(current.typ, length, new_capacity, &bytes)?
                        } else {
                            let initial = self.slice_values(&current)?;
                            self.make_slot_slice(current.typ, length, new_capacity, initial)?
                        };
                        self.write_address(
                            &view.target.ok_or_else(|| {
                                RuntimeError::new("reflect", id, "reflect: missing target")
                            })?,
                            grown,
                        )?;
                    }
                    return Ok(None);
                }
                "reflect.value_append" | "reflect.value_append_slice" | "reflect.value_copy" => {
                    let current = view.current.take().ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: invalid collection Value")
                    })?;
                    if id == "reflect.value_copy" {
                        let source =
                            self.reflected_value(&arguments[1])?
                                .current
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "reflect",
                                        id,
                                        "reflect: invalid source Value",
                                    )
                                })?;
                        return Ok(Some(Value::int(self.copy_values(current, source)? as i64)));
                    }
                    let values = if id == "reflect.value_append_slice" {
                        let source =
                            self.reflected_value(&arguments[1])?
                                .current
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "reflect",
                                        id,
                                        "reflect: invalid source Value",
                                    )
                                })?;
                        self.slice_values(&source)?
                    } else {
                        self.slice_values(&arguments[1])?
                            .iter()
                            .map(|value| {
                                self.reflected_value(value)?.current.ok_or_else(|| {
                                    RuntimeError::new(
                                        "reflect",
                                        id,
                                        "reflect: invalid append Value",
                                    )
                                })
                            })
                            .collect::<Result<Vec<_>, _>>()?
                    };
                    view = ReflectedValue::owned(self.append_values(current, values)?);
                }
                "reflect.value_swap" => {
                    let current = view.current.ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: Swapper of invalid Value")
                    })?;
                    if !matches!(current.data, Data::Slice(_)) {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect: Swapper of non-slice Value",
                        ));
                    }
                    let (left, _) = self.index_value(&current, &arguments[1])?;
                    let (right, _) = self.index_value(&current, &arguments[2])?;
                    self.store_index(current.clone(), arguments[1].clone(), right)?;
                    self.store_index(current, arguments[2].clone(), left)?;
                    return Ok(None);
                }
                "reflect.value_field_by_index" => {
                    let path = self.slice_values(&arguments[1])?;
                    if path.is_empty() {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect: empty field index",
                        ));
                    }
                    let mut current = view.current.take().ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: FieldByIndex of invalid Value")
                    })?;
                    let mut accessible = view.interfaceable || view.embedded_read_only;
                    for index in path {
                        if let Data::Pointer(address) = current.data {
                            current = self.read_address(&address)?;
                            view.target = Some(address);
                        }
                        let index = usize::try_from(index.integer()?).map_err(|_| {
                            RuntimeError::new("reflect", id, "reflect: negative field index")
                        })?;
                        let (_, node) = self
                            .types
                            .node(&current.typ)?
                            .filter(|(_, node)| node.kind == wire::Struct)
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "reflect",
                                    id,
                                    "reflect: non-struct field in index path",
                                )
                            })?;
                        let field = node.fields.get(index).ok_or_else(|| {
                            RuntimeError::new("reflect", id, "reflect: field index out of range")
                        })?;
                        let exported = field.name.chars().next().is_some_and(char::is_uppercase);
                        view.interfaceable = accessible && exported;
                        view.embedded_read_only = accessible && !exported && field.embedded;
                        accessible &= exported || field.embedded;
                        let Data::Struct(fields) = current.data else {
                            unreachable!()
                        };
                        current = fields.get(&field.name).cloned().ok_or_else(|| {
                            RuntimeError::new("reflect", id, "reflect: missing field storage")
                        })?;
                        if let Some(target) = &mut view.target {
                            target.path.push(PathElement::Field(field.name.clone()));
                        }
                    }
                    view.addressable = view.target.is_some();
                    view.settable = view.addressable && view.interfaceable;
                    view.current = Some(current);
                }
                "reflect.value_index" => {
                    let current = view.current.take().ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: Index of invalid Value")
                    })?;
                    let index = usize::try_from(arguments[1].integer()?)
                        .map_err(|_| RuntimeError::new("reflect", id, "reflect: negative index"))?;
                    if !matches!(
                        current.data,
                        Data::Array(_) | Data::Slice(_) | Data::String(_)
                    ) {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect: Index of non-array/slice/string Value",
                        ));
                    }
                    let (item, _) = self.index_value(&current, &arguments[1])?;
                    match current.data {
                        Data::Slice(slice) => {
                            let mut target = slice.storage;
                            target.path.push(PathElement::Index(slice.start + index));
                            view.target = Some(target);
                        }
                        Data::Array(_) => {
                            if let Some(target) = &mut view.target {
                                target.path.push(PathElement::Index(index));
                            }
                        }
                        _ => view.target = None,
                    }
                    view.addressable = view.target.is_some();
                    view.settable = view.addressable && view.interfaceable;
                    view.current = Some(item);
                }
                "reflect.value_convert" => {
                    let typ = self.reflected_type(&arguments[1])?;
                    let value = view.current.take().ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: Convert of invalid Value")
                    })?;
                    let converted = self.convert_value(value, typ)?;
                    view.current = Some(converted);
                    view.target = None;
                    view.addressable = false;
                    view.settable = false;
                }
                "reflect.value_elem" => {
                    let value = view.current.take().ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: Elem of invalid Value")
                    })?;
                    match value.data {
                        Data::Pointer(address) => {
                            view.current = Some(self.read_address(&address)?);
                            view.target = Some(address);
                            view.addressable = true;
                            view.settable = view.interfaceable;
                        }
                        Data::Interface(value) => {
                            view.current = Some(*value);
                            view.target = None;
                            view.addressable = false;
                            view.settable = false;
                        }
                        Data::Nil => {
                            view.current = None;
                            view.target = None;
                            view.addressable = false;
                            view.settable = false;
                        }
                        _ => {
                            return Err(RuntimeError::new(
                                "reflect",
                                id,
                                "reflect: Elem of non-pointer Value",
                            ));
                        }
                    }
                }
                "reflect.value_addr" => {
                    if !view.addressable {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect: Addr of unaddressable Value",
                        ));
                    }
                    let target = view.target.take().ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: missing target")
                    })?;
                    let current = view.current.take().ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: invalid Value")
                    })?;
                    view.current = Some(Value {
                        typ: TypeIdentity::Pointer(std::sync::Arc::new(current.typ)),
                        data: Data::Pointer(target),
                    });
                    view.addressable = false;
                    view.settable = false;
                }
                "reflect.value_set" | "reflect.value_set_len" | "reflect.value_set_cap" => {
                    if !view.settable {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect: mutation of unsettable Value",
                        ));
                    }
                    let target = view.target.ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: missing target")
                    })?;
                    let mut current = view.current.ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: invalid Value")
                    })?;
                    if id == "reflect.value_set" {
                        let source =
                            self.reflected_value(&arguments[1])?
                                .current
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "reflect",
                                        id,
                                        "reflect: Set from invalid Value",
                                    )
                                })?;
                        current = crate::operators::convert(
                            source.clone(),
                            current.typ.clone(),
                            &self.types,
                        )
                        .or_else(|_| self.coerce(source, &current.typ))?;
                    } else {
                        let count = usize::try_from(arguments[1].integer()?).map_err(|_| {
                            RuntimeError::new("reflect", id, "reflect: negative count")
                        })?;
                        if matches!(current.data, Data::Nil)
                            && (matches!(current.typ, TypeIdentity::Slice(_))
                                || self
                                    .types
                                    .node(&current.typ)?
                                    .is_some_and(|(_, node)| node.kind == wire::Slice))
                        {
                            return if count == 0 {
                                Ok(None)
                            } else {
                                Err(RuntimeError::new(
                                    "reflect",
                                    id,
                                    "reflect: slice bounds out of range",
                                ))
                            };
                        }
                        let Data::Slice(slice) = &mut current.data else {
                            return Err(RuntimeError::new(
                                "reflect",
                                id,
                                "reflect: mutation of non-slice Value",
                            ));
                        };
                        if count > slice.capacity
                            || id == "reflect.value_set_cap" && count < slice.length
                        {
                            return Err(RuntimeError::new(
                                "reflect",
                                id,
                                "reflect: slice bounds out of range",
                            ));
                        }
                        if id == "reflect.value_set_len" {
                            slice.length = count;
                        } else {
                            slice.capacity = count;
                        }
                        slice.identity = Arc::default();
                    }
                    self.write_address(&target, current)?;
                    return Ok(None);
                }
                _ => {
                    return Err(RuntimeError::new(
                        "unsupported_intrinsic",
                        id,
                        "unrecognized reflection operation",
                    ));
                }
            }
            Ok(Some(self.reflect_snapshot(view, 0)?))
        })();
        let mut results = Vec::new();
        match outcome {
            Ok(value) => {
                results.extend(value);
                results.push(Value::string(""));
                results.push(Value::boolean(true));
            }
            Err(error) => {
                if !matches!(
                    error.code,
                    "reflect" | "type_error" | "invalid_operation" | "panic"
                ) {
                    return Err(error);
                }
                if !mutation {
                    results.push(if id == "reflect.value_copy" {
                        Value::int(0)
                    } else if type_result {
                        self.reflect_type(&TypeIdentity::Void)?
                    } else {
                        self.reflect_struct("Value", [])?
                    });
                }
                results.push(Value::string(error.message));
                results.push(Value::boolean(false));
            }
        }
        Ok(results)
    }
}

impl ReflectedValue {
    pub(super) fn owned(value: Value) -> Self {
        Self {
            current: Some(value),
            target: None,
            addressable: false,
            settable: false,
            interfaceable: true,
            embedded_read_only: false,
        }
    }
}
