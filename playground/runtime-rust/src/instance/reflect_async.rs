//! Intrinsics that suspend a frame resume through the owner scheduler.

use super::reflect_value::ReflectedValue;
use super::scheduler::Resource;
use super::*;

pub(super) enum IntrinsicResume {
    Send,
    Receive,
    Call {
        count: usize,
    },
    Callback {
        results: Vec<TypeIdentity>,
        reflected: bool,
    },
}

impl Trace for IntrinsicResume {
    fn trace(&self, _visit: &mut dyn FnMut(Handle)) {}
}

impl Instance {
    pub(super) fn resume_reflect(&mut self, resume: IntrinsicResume) -> Result<(), RuntimeError> {
        let (values, reflected) = match resume {
            IntrinsicResume::Send => (vec![Value::string(""), Value::boolean(true)], false),
            IntrinsicResume::Receive => {
                let received = self.running.pop()?;
                let value = self.running.pop()?;
                (
                    vec![
                        self.reflect_snapshot(ReflectedValue::owned(value), 0)?,
                        received,
                        Value::string(""),
                        Value::boolean(true),
                    ],
                    false,
                )
            }
            IntrinsicResume::Call { count } => {
                (self.running.pop_values(&self.frame_pool, count)?, true)
            }
            IntrinsicResume::Callback { results, reflected } => {
                let values = self.running.pop()?;
                let values = self.slice_values(&values)?;
                if values.len() != results.len() {
                    return Err(RuntimeError::new(
                        "panic",
                        "MakeFunc",
                        "reflect: wrong result count from MakeFunc callback",
                    ));
                }
                let values = values
                    .iter()
                    .zip(results)
                    .map(|(value, typ)| {
                        let value = self.reflected_value(value)?.current.ok_or_else(|| {
                            RuntimeError::new(
                                "panic",
                                "MakeFunc",
                                "reflect: invalid callback result",
                            )
                        })?;
                        self.coerce(value, &typ)
                            .map_err(|error| RuntimeError::new("panic", "MakeFunc", error.message))
                    })
                    .collect::<Result<Vec<_>, _>>()?;
                (values, reflected)
            }
        };
        let results = if reflected {
            let values = values
                .into_iter()
                .map(|value| self.reflect_snapshot(ReflectedValue::owned(value), 0))
                .collect::<Result<Vec<_>, _>>()?;
            let typ = TypeIdentity::Slice(std::sync::Arc::new(TypeIdentity::Named(
                std::sync::Arc::new(wire::TypeKey {
                    module_path: "reflect".to_owned(),
                    decl_id: "Value".to_owned(),
                }),
            )));
            vec![
                self.make_slice(typ, values.len(), values.len(), values)?,
                Value::string(""),
                Value::boolean(true),
            ]
        } else {
            values
        };
        self.running
            .frames
            .last_mut()
            .unwrap()
            .stack
            .extend(results);
        Ok(())
    }

    pub(super) fn start_dynamic_callback(
        &mut self,
        function: Value,
        arguments: Vec<Value>,
        expected_results: usize,
        reflected: bool,
    ) -> Result<(), RuntimeError> {
        let (module, node) = self
            .types
            .node(&function.typ)?
            .filter(|(_, node)| node.kind == wire::Function)
            .ok_or_else(|| {
                RuntimeError::new("panic", "MakeFunc", "reflect: invalid function type")
            })?;
        let signature = node.signature.as_ref().unwrap();
        if signature.params.len() != arguments.len() || signature.results.len() != expected_results
        {
            return Err(RuntimeError::new(
                "panic",
                "MakeFunc",
                "reflect: call arity mismatch",
            ));
        }
        let params = signature
            .params
            .iter()
            .map(|parameter| self.types.resolve(module, &parameter.r#type))
            .collect::<Result<Vec<_>, _>>()?;
        let results = signature
            .results
            .iter()
            .map(|reference| self.types.resolve(module, reference))
            .collect::<Result<Vec<_>, _>>()?;
        let Data::DynamicFunction(callback) = function.data else {
            unreachable!()
        };
        let Data::Function(callee) = callback.data else {
            return Err(RuntimeError::new(
                "panic",
                "MakeFunc",
                "reflect: callback is not executable",
            ));
        };
        let mut snapshots = Vec::new();
        for (value, typ) in arguments.into_iter().zip(params) {
            let value = self.coerce(value, &typ)?;
            snapshots.push(self.reflect_snapshot(ReflectedValue::owned(value), 0)?);
        }
        let typ = TypeIdentity::Slice(std::sync::Arc::new(TypeIdentity::Named(
            std::sync::Arc::new(wire::TypeKey {
                module_path: "reflect".to_owned(),
                decl_id: "Value".to_owned(),
            }),
        )));
        let values = self.make_slice(typ, snapshots.len(), snapshots.len(), snapshots)?;
        self.running.frames.last_mut().unwrap().resume =
            Some(IntrinsicResume::Callback { results, reflected });
        self.push_frame(callee, vec![values], 1, false)
    }

    pub(super) fn execute_reflect_async(
        &mut self,
        id: &str,
        arguments: Vec<Value>,
    ) -> Result<(), RuntimeError> {
        let outcome = (|| -> Result<Option<Vec<Value>>, RuntimeError> {
            if id == "reflect.select" {
                let mut cases = Vec::new();
                let mut default = None;
                let mut indexes = Vec::new();
                let mut receives = Vec::new();
                for (index, value) in self.slice_values(&arguments[0])?.into_iter().enumerate() {
                    let Data::Struct(fields) = value.data else {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect.Select: invalid case",
                        ));
                    };
                    let direction = fields
                        .get("Dir")
                        .ok_or_else(|| {
                            RuntimeError::new("reflect", id, "reflect.Select: missing Dir")
                        })?
                        .integer()?;
                    if direction == 3 {
                        if default.replace(index).is_some() {
                            return Err(RuntimeError::new(
                                "reflect",
                                id,
                                "reflect.Select: multiple default cases",
                            ));
                        }
                        continue;
                    }
                    if direction != 1 && direction != 2 {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect.Select: invalid Dir",
                        ));
                    }
                    let snapshot = fields.get("Chan").ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect.Select: missing Chan")
                    })?;
                    let Some(channel) = self.reflected_value(snapshot)?.current else {
                        continue;
                    };
                    let (_, node) = self
                        .types
                        .node(&channel.typ)?
                        .filter(|(_, node)| node.kind == wire::Waitable)
                        .ok_or_else(|| {
                            RuntimeError::new("reflect", id, "reflect.Select: non-channel Value")
                        })?;
                    let sending = direction == 1;
                    if sending && node.direction == wire::ChannelReceive
                        || !sending && node.direction == wire::ChannelSend
                    {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect.Select: channel direction mismatch",
                        ));
                    }
                    let send = if sending {
                        let snapshot = fields.get("Send").ok_or_else(|| {
                            RuntimeError::new("reflect", id, "reflect.Select: missing Send")
                        })?;
                        Some(self.reflected_value(snapshot)?.current.ok_or_else(|| {
                            RuntimeError::new("reflect", id, "reflect.Select: invalid Send Value")
                        })?)
                    } else {
                        None
                    };
                    indexes.push(index);
                    receives.push(!sending);
                    cases.push(select::SelectCase {
                        channel,
                        send,
                        zero: None,
                    });
                }
                self.start_selection(
                    select::Selection {
                        cases,
                        destination: select::SelectDestination::Reflect {
                            indexes,
                            receives,
                            default,
                        },
                        outcome: None,
                    },
                    default.is_some(),
                )?;
                return Ok(None);
            }
            if id == "reflect.make_func" {
                let typ = self.reflected_type(&arguments[0])?;
                if !self
                    .types
                    .node(&typ)?
                    .is_some_and(|(_, node)| node.kind == wire::Function)
                {
                    return Err(RuntimeError::new(
                        "reflect",
                        id,
                        "reflect: MakeFunc requires function type",
                    ));
                }
                let Data::Function(callback) = &arguments[1].data else {
                    return Err(RuntimeError::new(
                        "reflect",
                        id,
                        "reflect: MakeFunc callback is not callable",
                    ));
                };
                let revision = callback.revision.as_ref().unwrap_or(&self.revision);
                let signature = &revision
                    .program
                    .function(&callback.module, &callback.function)?
                    .declaration
                    .signature;
                if signature.params.len() != 1 || signature.results.len() != 1 {
                    return Err(RuntimeError::new(
                        "reflect",
                        id,
                        "reflect: invalid MakeFunc callback signature",
                    ));
                }
                let value = Value {
                    typ,
                    data: Data::DynamicFunction(Box::new(arguments[1].clone())),
                };
                let value = self.reflect_snapshot(ReflectedValue::owned(value), 0)?;
                return Ok(Some(vec![value, Value::string(""), Value::boolean(true)]));
            }
            if id == "reflect.value_call" {
                let function = self
                    .reflected_value(&arguments[0])?
                    .current
                    .ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: Call of invalid Value")
                    })?;
                let mut values = self
                    .slice_values(&arguments[1])?
                    .iter()
                    .map(|value| {
                        self.reflected_value(value)?.current.ok_or_else(|| {
                            RuntimeError::new("reflect", id, "reflect: invalid Call argument")
                        })
                    })
                    .collect::<Result<Vec<_>, _>>()?;
                let Data::Bool(call_slice) = arguments[2].data else {
                    return Err(RuntimeError::new(
                        "reflect",
                        id,
                        "reflect: invalid CallSlice flag",
                    ));
                };
                let typ = self.reflect_value_type(&function)?;
                let (module, node) = self
                    .types
                    .node(&typ)?
                    .filter(|(_, node)| node.kind == wire::Function)
                    .ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect: Call of non-function Value")
                    })?;
                let signature = node.signature.as_ref().unwrap();
                let params = signature
                    .params
                    .iter()
                    .map(|parameter| self.types.resolve(module, &parameter.r#type))
                    .collect::<Result<Vec<_>, _>>()?;
                let result_count = signature.results.len();
                if call_slice && !signature.variadic {
                    return Err(RuntimeError::new(
                        "reflect",
                        id,
                        "reflect: CallSlice of non-variadic function",
                    ));
                }
                if signature.variadic && !call_slice {
                    let fixed = params.len() - 1;
                    if values.len() < fixed {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect: too few Call arguments",
                        ));
                    }
                    let rest = values.split_off(fixed);
                    values.push(self.make_slice(
                        params[fixed].clone(),
                        rest.len(),
                        rest.len(),
                        rest,
                    )?);
                }
                if values.len() != params.len() {
                    return Err(RuntimeError::new(
                        "reflect",
                        id,
                        "reflect: wrong Call argument count",
                    ));
                }
                let values = values
                    .into_iter()
                    .zip(params)
                    .map(|(value, typ)| self.coerce(value, &typ))
                    .collect::<Result<Vec<_>, _>>()?;
                match function.data {
                    Data::Method { function, receiver } => {
                        let mut values = values;
                        if let Some(receiver) = receiver {
                            let revision = function.revision.as_ref().unwrap_or(&self.revision);
                            let callee = revision
                                .program
                                .function(&function.module, &function.function)?;
                            let parameter =
                                callee.declaration.signature.params.first().ok_or_else(|| {
                                    RuntimeError::new(
                                        "reflect",
                                        id,
                                        "method has no receiver parameter",
                                    )
                                })?;
                            let expected = revision
                                .program
                                .types()
                                .resolve(&function.module, &parameter.r#type)?;
                            values.insert(0, self.method_receiver(*receiver, &expected)?);
                        }
                        self.running.frames.last_mut().unwrap().resume =
                            Some(IntrinsicResume::Call {
                                count: result_count,
                            });
                        self.push_frame(function, values, result_count, false)?;
                    }
                    Data::Function(callee) => {
                        self.running.frames.last_mut().unwrap().resume =
                            Some(IntrinsicResume::Call {
                                count: result_count,
                            });
                        self.push_frame(callee, values, result_count, false)?;
                    }
                    Data::DynamicFunction(_) => {
                        self.start_dynamic_callback(function, values, result_count, true)?
                    }
                    _ => {
                        return Err(RuntimeError::new(
                            "reflect",
                            id,
                            "reflect: Call of nil function",
                        ));
                    }
                }
                return Ok(None);
            }
            if id == "reflect.make_chan" {
                let typ = self.reflected_type(&arguments[0])?;
                if !self.types.node(&typ)?.is_some_and(|(_, node)| {
                    node.kind == wire::Waitable && node.direction == wire::ChannelBoth
                }) {
                    return Err(RuntimeError::new(
                        "reflect",
                        id,
                        "reflect.MakeChan requires bidirectional channel type",
                    ));
                }
                let capacity = usize::try_from(arguments[1].integer()?)
                    .ok()
                    .filter(|capacity| *capacity <= self.limits.max_sequence_elements)
                    .ok_or_else(|| {
                        RuntimeError::new("reflect", id, "reflect.MakeChan: invalid buffer size")
                    })?;
                self.charge_guest_object(capacity, 0)?;
                let handle = self.allocate(Value {
                    typ: TypeIdentity::Any,
                    data: Data::Resource(Box::new(Resource::Channel {
                        capacity,
                        closed: false,
                        values: VecDeque::new(),
                    })),
                })?;
                let value = self.reflect_snapshot(
                    ReflectedValue::owned(Value {
                        typ,
                        data: Data::ResourceRef(handle),
                    }),
                    0,
                )?;
                return Ok(Some(vec![value, Value::string(""), Value::boolean(true)]));
            }
            let channel = self
                .reflected_value(&arguments[0])?
                .current
                .ok_or_else(|| {
                    RuntimeError::new("reflect", id, "reflect: invalid channel Value")
                })?;
            let (_, node) = self
                .types
                .node(&channel.typ)?
                .filter(|(_, node)| node.kind == wire::Waitable)
                .ok_or_else(|| {
                    RuntimeError::new("reflect", id, "reflect: expected channel Value")
                })?;
            let receiving = matches!(id, "reflect.value_recv" | "reflect.value_try_recv");
            if receiving && node.direction == wire::ChannelSend
                || !receiving && node.direction == wire::ChannelReceive
            {
                return Err(RuntimeError::new(
                    "reflect",
                    id,
                    "reflect: operation conflicts with channel direction",
                ));
            }
            if id == "reflect.value_close" {
                self.close_channel(&channel)?;
                return Ok(Some(vec![Value::string(""), Value::boolean(true)]));
            }
            if receiving {
                if let Some((value, received)) = self.try_receive(&channel)? {
                    let value = self.reflect_snapshot(ReflectedValue::owned(value), 0)?;
                    return Ok(Some(vec![
                        value,
                        Value::boolean(received),
                        Value::string(""),
                        Value::boolean(true),
                    ]));
                }
                if id == "reflect.value_try_recv" {
                    return Ok(Some(vec![
                        self.reflect_struct("Value", [])?,
                        Value::boolean(false),
                        Value::string(""),
                        Value::boolean(true),
                    ]));
                }
                self.running.frames.last_mut().unwrap().resume = Some(IntrinsicResume::Receive);
                self.wait_channel(channel, None, true)?;
                return Ok(None);
            }
            let value = self
                .reflected_value(&arguments[1])?
                .current
                .ok_or_else(|| RuntimeError::new("reflect", id, "reflect: invalid send Value"))?;
            let value = self.coerce(value, &self.element_type(&channel.typ)?)?;
            let sent = self.try_send(&channel, value.clone())?;
            if id == "reflect.value_try_send" {
                return Ok(Some(vec![
                    Value::boolean(sent),
                    Value::string(""),
                    Value::boolean(true),
                ]));
            }
            if sent {
                return Ok(Some(vec![Value::string(""), Value::boolean(true)]));
            }
            self.running.frames.last_mut().unwrap().resume = Some(IntrinsicResume::Send);
            self.wait_channel(channel, Some(value), false)?;
            Ok(None)
        })();
        let results = match outcome {
            Ok(None) => return Ok(()),
            Ok(Some(results)) => results,
            Err(error) => {
                if !matches!(error.code, "reflect" | "type_error" | "panic") {
                    return Err(error);
                }
                let mut results = Vec::new();
                if id == "reflect.select" {
                    results.extend([
                        Value::int(0),
                        self.reflect_struct("Value", [])?,
                        Value::boolean(false),
                    ]);
                }
                if id == "reflect.value_call" {
                    results.push(Value {
                        typ: TypeIdentity::Slice(std::sync::Arc::new(TypeIdentity::Named(
                            std::sync::Arc::new(wire::TypeKey {
                                module_path: "reflect".to_owned(),
                                decl_id: "Value".to_owned(),
                            }),
                        ))),
                        data: Data::Nil,
                    });
                }
                if matches!(
                    id,
                    "reflect.make_chan"
                        | "reflect.make_func"
                        | "reflect.value_recv"
                        | "reflect.value_try_recv"
                ) {
                    results.push(self.reflect_struct("Value", [])?);
                }
                if matches!(
                    id,
                    "reflect.value_recv" | "reflect.value_try_recv" | "reflect.value_try_send"
                ) {
                    results.push(Value::boolean(false));
                }
                results.extend([Value::string(error.message), Value::boolean(false)]);
                results
            }
        };
        self.running
            .frames
            .last_mut()
            .unwrap()
            .stack
            .extend(results);
        Ok(())
    }
}
