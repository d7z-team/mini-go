//! Method descriptors and bound callable values share the ordinary frame dispatcher.

use super::{reflect_value::ReflectedValue, *};

impl Instance {
    pub(super) fn method_receiver(
        &mut self,
        receiver: Value,
        expected: &TypeIdentity,
    ) -> Result<Value, RuntimeError> {
        let mut level = vec![(receiver, None::<Address>, HashSet::new())];
        for _ in 0..self.limits.max_value_depth {
            let mut next = Vec::new();
            let mut found = None;
            for (mut value, mut address, mut seen) in level {
                if !seen.insert(value.typ.clone()) {
                    continue;
                }
                let candidate = if self.types.identical(&value.typ, expected)? {
                    Some(value.clone())
                } else if let Some(address) = &address
                    && self.types.identical(
                        &TypeIdentity::Pointer(std::sync::Arc::new(value.typ.clone())),
                        expected,
                    )?
                {
                    Some(Value {
                        typ: expected.clone(),
                        data: Data::Pointer(address.clone()),
                    })
                } else {
                    None
                };
                if let Some(candidate) = candidate {
                    if found.replace(candidate).is_some() {
                        return Err(RuntimeError::new(
                            "panic",
                            "method",
                            "ambiguous embedded receiver",
                        ));
                    }
                    continue;
                }
                if let Data::Pointer(pointer) = &value.data {
                    address = Some(pointer.clone());
                    value = self.read_address(pointer)?;
                    if self.types.identical(&value.typ, expected)? {
                        if found.replace(value).is_some() {
                            return Err(RuntimeError::new(
                                "panic",
                                "method",
                                "ambiguous embedded receiver",
                            ));
                        }
                        continue;
                    }
                }
                if let Data::Struct(fields) = &value.data
                    && let Some((_, node)) = self.types.node(&value.typ)?
                {
                    for field in node.fields.iter().filter(|field| field.embedded) {
                        if let Some(value) = fields.get(&field.name) {
                            let address = address.as_ref().map(|address| {
                                let mut address = address.clone();
                                address.path.push(PathElement::Field(field.name.clone()));
                                address
                            });
                            next.push((value.clone(), address, seen.clone()));
                        }
                    }
                }
            }
            if let Some(value) = found {
                return self.coerce(value, expected);
            }
            if next.is_empty() {
                break;
            }
            level = next;
        }
        Err(RuntimeError::new(
            "panic",
            "method",
            "nil or incompatible method receiver",
        ))
    }

    pub(super) fn execute_reflect_method(
        &mut self,
        id: &str,
        arguments: Vec<Value>,
    ) -> Result<Vec<Value>, RuntimeError> {
        let binding = id == "reflect.value_method";
        let outcome = (|| -> Result<Value, RuntimeError> {
            if binding {
                let mut receiver =
                    self.reflected_value(&arguments[0])?
                        .current
                        .ok_or_else(|| {
                            RuntimeError::new("reflect", id, "reflect: Method of invalid Value")
                        })?;
                let Data::Struct(fields) = &arguments[1].data else {
                    return Err(RuntimeError::new("reflect", id, "reflect: invalid Method"));
                };
                let Some(Value {
                    data: Data::String(name),
                    ..
                }) = fields.get("Name")
                else {
                    return Err(RuntimeError::new(
                        "reflect",
                        id,
                        "reflect: missing method name",
                    ));
                };
                let name = std::str::from_utf8(name).map_err(|_| {
                    RuntimeError::new("reflect", id, "reflect: invalid method name")
                })?;
                if let Data::Interface(value) = receiver.data {
                    receiver = *value;
                }
                let (module, method) = self
                    .types
                    .declared_methods(&receiver.typ)?
                    .into_iter()
                    .find(|(_, method)| method.name == name)
                    .ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: method unavailable")
                    })?;
                let typ = self.reflect_method_signature(&module, &method, false)?;
                let function = FunctionValue {
                    index: None,
                    revision: Some(
                        self.running
                            .frames
                            .last()
                            .map_or_else(|| self.revision.clone(), |frame| frame.revision.clone()),
                    ),
                    module: if method.module_path.is_empty() {
                        module.into()
                    } else {
                        method.module_path.into()
                    },
                    function: method.function_id.into(),
                    captures: Vec::new(),
                };
                let value = Value {
                    typ,
                    data: Data::Method {
                        function,
                        receiver: Some(Box::new(receiver)),
                    },
                };
                return self.reflect_snapshot(ReflectedValue::owned(value), 0);
            }
            let typ = self.reflected_type(&arguments[0])?;
            let index = usize::try_from(arguments[1].integer()?).map_err(|_| {
                RuntimeError::new("reflect", id, "reflect: Method index out of range")
            })?;
            let interface = self
                .types
                .node(&typ)?
                .is_some_and(|(_, node)| node.kind == wire::Interface);
            let (module, method) = self
                .types
                .declared_methods(&typ)?
                .into_iter()
                .filter(|(_, method)| {
                    interface || method.name.chars().next().is_some_and(char::is_uppercase)
                })
                .nth(index)
                .ok_or_else(|| {
                    RuntimeError::new("reflect", id, "reflect: Method index out of range")
                })?;
            let signature = self.reflect_method_signature(&module, &method, false)?;
            let callable_type = if interface {
                signature.clone()
            } else {
                self.reflect_method_signature(&module, &method, true)?
            };
            let receiver = if interface {
                typ
            } else {
                self.types.resolve(&module, &method.receiver)?
            };
            let function_module = if method.module_path.is_empty() {
                module.clone()
            } else {
                method.module_path.clone()
            };
            let function = if interface {
                self.reflect_struct("Value", [])?
            } else {
                self.reflect_snapshot(
                    ReflectedValue::owned(Value {
                        typ: callable_type.clone(),
                        data: Data::Method {
                            function: FunctionValue {
                                index: None,
                                revision: Some(self.running.frames.last().map_or_else(
                                    || self.revision.clone(),
                                    |frame| frame.revision.clone(),
                                )),
                                module: function_module.clone().into(),
                                function: method.function_id.clone().into(),
                                captures: Vec::new(),
                            },
                            receiver: None,
                        },
                    }),
                    0,
                )?
            };
            let callable_type = self.reflect_type(&callable_type)?;
            let signature = self.reflect_type(&signature)?;
            let receiver = self.reflect_type(&receiver)?;
            let exported = method.name.chars().next().is_some_and(char::is_uppercase);
            self.reflect_struct(
                "Method",
                [
                    ("Name", Value::string(method.name)),
                    (
                        "PkgPath",
                        Value::string(if exported { String::new() } else { module }),
                    ),
                    ("Type", callable_type),
                    ("Func", function),
                    ("Index", Value::int(index as i64)),
                    ("modulePath", Value::string(function_module)),
                    ("receiverType", receiver),
                    ("signatureType", signature),
                    ("functionID", Value::string(method.function_id)),
                    ("variadic", Value::boolean(method.signature.variadic)),
                    ("exported", Value::boolean(exported)),
                ],
            )
        })();
        match outcome {
            Ok(value) => Ok(vec![value, Value::string(""), Value::boolean(true)]),
            Err(error) if matches!(error.code, "reflect" | "type_error") => Ok(vec![
                self.reflect_struct(if binding { "Value" } else { "Method" }, [])?,
                Value::string(error.message),
                Value::boolean(false),
            ]),
            Err(error) => Err(error),
        }
    }

    fn reflect_method_signature(
        &mut self,
        module: &str,
        method: &wire::Method,
        receiver: bool,
    ) -> Result<TypeIdentity, RuntimeError> {
        let mut params = Vec::new();
        if receiver {
            params.push(self.types.resolve(module, &method.receiver)?);
        }
        for parameter in method.signature.params.iter() {
            params.push(self.types.resolve(module, &parameter.r#type)?);
        }
        let results = method
            .signature
            .results
            .iter()
            .map(|reference| self.types.resolve(module, reference))
            .collect::<Result<Vec<_>, _>>()?;
        self.types.construct(
            crate::types::ConstructedType::Function {
                params,
                results,
                variadic: method.signature.variadic,
            },
            self.limits.max_sequence_elements,
            self.limits.max_value_depth,
        )
    }
}
