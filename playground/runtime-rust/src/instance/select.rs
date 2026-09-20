use super::*;
use scheduler::Resource;

#[derive(Clone)]
pub(super) struct SelectCase {
    pub channel: Value,
    pub send: Option<Value>,
    pub zero: Option<Value>,
}

#[derive(Clone)]
pub(super) enum SelectDestination {
    Locals(wire::SelectPayload),
    Receive(bool),
    Send,
    Reflect {
        indexes: Vec<usize>,
        receives: Vec<bool>,
        default: Option<usize>,
    },
}

#[derive(Clone)]
pub(super) struct Selection {
    pub cases: Vec<SelectCase>,
    pub destination: SelectDestination,
    pub outcome: Option<SelectOutcome>,
}

#[derive(Clone)]
pub(super) struct SelectOutcome {
    pub index: i64,
    pub value: Option<Value>,
    pub received: bool,
    pub error: Option<RuntimeError>,
}

impl Trace for Selection {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        for case in &self.cases {
            case.channel.trace(visit);
            if let Some(value) = &case.send {
                value.trace(visit);
            }
            if let Some(value) = &case.zero {
                value.trace(visit);
            }
        }
        if let Some(outcome) = &self.outcome
            && let Some(value) = &outcome.value
        {
            value.trace(visit);
        }
    }
}

impl Instance {
    pub(super) fn execute_select(
        &mut self,
        payload: wire::SelectPayload,
    ) -> Result<(), RuntimeError> {
        let locals = self.running.frames.last().unwrap().prepared.locals.clone();
        let mut cases = Vec::with_capacity(payload.cases.len());
        for case in payload.cases.iter() {
            let channel = self.load_local(locals[&case.channel])?;
            let send = if case.send.is_empty() {
                None
            } else {
                Some(self.load_local(locals[&case.send])?)
            };
            cases.push(SelectCase {
                channel,
                send,
                zero: None,
            });
        }
        let fallback = payload.default;
        let selection = Selection {
            cases,
            destination: SelectDestination::Locals(payload),
            outcome: None,
        };
        self.start_selection(selection, fallback)
    }

    pub(super) fn start_selection(
        &mut self,
        selection: Selection,
        fallback: bool,
    ) -> Result<(), RuntimeError> {
        let mut selection = self.prepare_selection(selection)?;
        self.running.selection_completion = Some(Box::new(selection.clone()));
        let ready = self.try_selection(&mut selection);
        self.running.selection_completion = None;
        if !ready? {
            if fallback {
                selection.outcome = Some(SelectOutcome {
                    index: -1,
                    value: None,
                    received: false,
                    error: None,
                });
            } else {
                self.park(scheduler::Blocked::Select(Box::new(selection)));
                return Ok(());
            }
        }
        self.running.selection_completion = Some(Box::new(selection));
        self.finish_selection()
    }

    pub(super) fn wait_channel(
        &mut self,
        channel: Value,
        send: Option<Value>,
        with_ok: bool,
    ) -> Result<(), RuntimeError> {
        let destination = if send.is_some() {
            SelectDestination::Send
        } else {
            SelectDestination::Receive(with_ok)
        };
        self.start_selection(
            Selection {
                cases: vec![SelectCase {
                    channel,
                    send,
                    zero: None,
                }],
                destination,
                outcome: None,
            },
            false,
        )
    }

    pub(super) fn finish_selection(&mut self) -> Result<(), RuntimeError> {
        let selection = self.running.selection_completion.as_ref().unwrap();
        let outcome = selection.outcome.clone().expect("committed selection");
        let destination = selection.destination.clone();
        let result = (|| {
            if let SelectDestination::Reflect {
                indexes,
                receives,
                default,
            } = destination
            {
                let (index, value, received, message, success) = match outcome.error {
                    Some(error) if matches!(error.code, "panic" | "type_error") => {
                        (0, None, false, error.message, false)
                    }
                    Some(error) => return Err(error),
                    None if outcome.index < 0 => {
                        (default.unwrap(), None, false, String::new(), true)
                    }
                    None => {
                        let index = outcome.index as usize;
                        (
                            indexes[index],
                            if receives[index] { outcome.value } else { None },
                            outcome.received,
                            String::new(),
                            true,
                        )
                    }
                };
                let value = match value {
                    Some(value) => {
                        self.reflect_snapshot(reflect_value::ReflectedValue::owned(value), 0)?
                    }
                    None => self.reflect_struct("Value", [])?,
                };
                self.running.frames.last_mut().unwrap().stack.extend([
                    Value::int(index as i64),
                    value,
                    Value::boolean(received),
                    Value::string(message),
                    Value::boolean(success),
                ]);
                return Ok(());
            }
            if let Some(error) = outcome.error {
                return Err(error);
            }
            match destination {
                SelectDestination::Locals(payload) => {
                    let locals = self.running.frames.last().unwrap().prepared.locals.clone();
                    self.store_local(locals[&payload.index], Value::int(outcome.index), false)?;
                    if outcome.index >= 0 {
                        let case = &payload.cases[outcome.index as usize];
                        if case.send.is_empty() {
                            self.store_local(locals[&case.value], outcome.value.unwrap(), false)?;
                            self.store_local(
                                locals[&case.ok],
                                Value::boolean(outcome.received),
                                false,
                            )?;
                        }
                    }
                }
                SelectDestination::Receive(with_ok) => {
                    let frame = self.running.frames.last_mut().unwrap();
                    frame.stack.push(outcome.value.unwrap());
                    if with_ok {
                        frame.stack.push(Value::boolean(outcome.received));
                    }
                }
                SelectDestination::Send => {}
                SelectDestination::Reflect { .. } => unreachable!(),
            }
            Ok(())
        })();
        self.running.selection_completion = None;
        result
    }

    pub(super) fn prepare_selection(
        &mut self,
        mut selection: Selection,
    ) -> Result<Selection, RuntimeError> {
        if selection.cases.len() > self.limits.max_sequence_elements {
            return Err(RuntimeError::new(
                "value_limit",
                "select",
                "too many select cases",
            ));
        }
        let root_start = self.running.allocation_roots.len();
        for case in &selection.cases {
            self.running.allocation_roots.push(case.channel.clone());
            if let Some(value) = &case.send {
                self.running.allocation_roots.push(value.clone());
            }
        }
        let prepared = (|| {
            self.charge_guest(128 + selection.cases.len() as u64 * 176)?;
            for case in &mut selection.cases {
                let (_, node) = self
                    .types
                    .node(&case.channel.typ)?
                    .filter(|(_, node)| node.kind == wire::Waitable)
                    .ok_or_else(|| RuntimeError::new("type_error", "select", "expected channel"))?;
                if case.send.is_some() && node.direction == wire::ChannelReceive
                    || case.send.is_none() && node.direction == wire::ChannelSend
                {
                    return Err(RuntimeError::new(
                        "type_error",
                        "select",
                        "channel direction mismatch",
                    ));
                }
                let element = self.element_type(&case.channel.typ)?;
                if let Some(value) = case.send.take() {
                    let value = self.coerce(value, &element)?;
                    self.running.allocation_roots.push(value.clone());
                    case.send = Some(value);
                } else {
                    let value = self.zero(&element, 0)?;
                    self.running.allocation_roots.push(value.clone());
                    case.zero = Some(value);
                }
            }
            Ok::<_, RuntimeError>(())
        })();
        self.running.allocation_roots.truncate(root_start);
        prepared?;
        Ok(selection)
    }

    pub(super) fn try_selection(
        &mut self,
        selection: &mut Selection,
    ) -> Result<bool, RuntimeError> {
        if selection.outcome.is_some() {
            return Ok(true);
        }
        let mut ready = Vec::new();
        for (index, case) in selection.cases.iter().enumerate() {
            if matches!(case.channel.data, Data::Nil) {
                continue;
            }
            let handle = Self::resource_handle(&case.channel)?;
            let resource = self.resource(handle)?;
            let Resource::Channel {
                closed,
                capacity,
                values,
                ..
            } = &*resource
            else {
                return Err(RuntimeError::new(
                    "type_error",
                    "select",
                    "expected channel",
                ));
            };
            if *closed
                || self
                    .blocked
                    .select_peer(handle, case.send.is_some())
                    .is_some()
                || if case.send.is_some() {
                    values.len() < *capacity
                } else {
                    !values.is_empty()
                }
            {
                ready.push(index);
            }
        }
        let index = self.choose_ready(&ready);
        if index < 0 {
            return Ok(false);
        }
        let case = &selection.cases[index as usize];
        let handle = Self::resource_handle(&case.channel)?;
        let mut resource = self.resource(handle)?.clone();
        let Resource::Channel { closed, values, .. } = &mut resource else {
            unreachable!()
        };
        let mut outcome = SelectOutcome {
            index,
            value: None,
            received: false,
            error: None,
        };
        if let Some(value) = &case.send {
            if *closed {
                outcome.error = Some(RuntimeError::new("panic", "send", "send on closed channel"));
            } else if let Some((key, peer, _)) = self.blocked.select_peer(handle, true) {
                self.blocked.complete_selection(
                    key,
                    SelectOutcome {
                        index: peer as i64,
                        value: Some(value.clone()),
                        received: true,
                        error: None,
                    },
                );
            } else {
                values.push_back(value.clone());
                self.store_resource(handle, resource)?;
            }
        } else if let Some(value) = values.pop_front() {
            value.trace(&mut |handle| self.running.transient_roots.push(handle));
            self.store_resource(handle, resource)?;
            outcome.value = Some(value);
            outcome.received = true;
        } else if *closed {
            outcome.value = case.zero.clone();
        } else if let Some((key, peer, value)) = self.blocked.select_peer(handle, false) {
            outcome.value = value;
            outcome.received = true;
            self.blocked.complete_selection(
                key,
                SelectOutcome {
                    index: peer as i64,
                    value: None,
                    received: false,
                    error: None,
                },
            );
        }
        selection.outcome = Some(outcome);
        Ok(true)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pending_selection_cannot_match_itself_and_cancellation_removes_all_roots() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let scope = vm.running.scope;
        let handle = vm
            .allocate(Value {
                typ: TypeIdentity::Any,
                data: Data::Resource(Box::new(Resource::Channel {
                    capacity: 0,
                    closed: false,
                    values: VecDeque::new(),
                })),
            })
            .unwrap();
        let value = vm.heap.allocate(Value::int(42), 128).unwrap();
        let channel = Value {
            typ: TypeIdentity::Any,
            data: Data::ResourceRef(handle),
        };
        let sent = Value {
            typ: TypeIdentity::Any,
            data: Data::Pointer(Address {
                identity: Arc::default(),
                root: value,
                path: vec![],
            }),
        };
        vm.park(scheduler::Blocked::Select(Box::new(Selection {
            cases: vec![
                SelectCase {
                    channel: channel.clone(),
                    send: Some(sent),
                    zero: None,
                },
                SelectCase {
                    channel,
                    send: None,
                    zero: Some(Value::int(0)),
                },
            ],
            destination: SelectDestination::Send,
            outcome: None,
        })));
        vm.resume_blocked().unwrap();
        assert_eq!(vm.blocked.len(), 1);
        assert!(vm.runnable.is_empty());
        vm.collect_at_boundary().unwrap();
        assert!(vm.heap.get(value).is_ok());
        vm.cancel_scope(scope).unwrap();
        assert!(vm.blocked.is_empty());
        assert!(vm.blocked.select_peer(handle, true).is_none());
        assert!(vm.blocked.select_peer(handle, false).is_none());
        assert!(vm.heap.get(value).is_err());
        assert!(vm.heap.get(handle).is_err());
        vm.close().unwrap();
    }

    #[test]
    fn crossing_selections_commit_both_winners_and_withdraw_every_other_case() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let mut channels = Vec::new();
        for _ in 0..2 {
            let handle = vm
                .allocate(Value {
                    typ: TypeIdentity::Any,
                    data: Data::Resource(Box::new(Resource::Channel {
                        capacity: 0,
                        closed: false,
                        values: VecDeque::new(),
                    })),
                })
                .unwrap();
            channels.push(Value {
                typ: TypeIdentity::Any,
                data: Data::ResourceRef(handle),
            });
        }
        let first = Selection {
            cases: vec![
                SelectCase {
                    channel: channels[0].clone(),
                    send: Some(Value::int(41)),
                    zero: None,
                },
                SelectCase {
                    channel: channels[1].clone(),
                    send: None,
                    zero: Some(Value::int(0)),
                },
            ],
            destination: SelectDestination::Send,
            outcome: None,
        };
        vm.park(scheduler::Blocked::Select(Box::new(first)));
        vm.collect_at_boundary().unwrap();
        let mut second = Selection {
            cases: vec![
                SelectCase {
                    channel: channels[1].clone(),
                    send: Some(Value::int(42)),
                    zero: None,
                },
                SelectCase {
                    channel: channels[0].clone(),
                    send: None,
                    zero: Some(Value::int(0)),
                },
            ],
            destination: SelectDestination::Send,
            outcome: None,
        };
        assert!(vm.try_selection(&mut second).unwrap());
        let second = second.outcome.unwrap();
        let scheduler::Blocked::Select(first) =
            vm.blocked.iter().next().unwrap().blocked.as_ref().unwrap()
        else {
            panic!("selection")
        };
        let first = first.outcome.as_ref().unwrap();
        assert_eq!(first.index + second.index, 1);
        let received = if first.received {
            first.value.as_ref().unwrap()
        } else {
            second.value.as_ref().unwrap()
        };
        assert_eq!(
            received.integer().unwrap(),
            if first.received { 42 } else { 41 }
        );
        for channel in &channels {
            let handle = Instance::resource_handle(channel).unwrap();
            assert!(vm.blocked.select_peer(handle, true).is_none());
            assert!(vm.blocked.select_peer(handle, false).is_none());
            assert!(vm.try_receive(channel).unwrap().is_none());
        }
        vm.close().unwrap();
        assert_eq!(vm.heap_stats().live_objects, 0);
    }
}
