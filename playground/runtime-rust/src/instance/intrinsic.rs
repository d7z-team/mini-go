use super::*;

use super::timer::Timer;

impl Instance {
    pub(super) fn execute_intrinsic(
        &mut self,
        intrinsic: wire::Intrinsic,
    ) -> Result<(), RuntimeError> {
        use wire::Intrinsic::*;
        let (arg_count, result_count) = intrinsic.arity();
        let id = intrinsic.id();
        if intrinsic == FfiCall {
            return self.execute_wait(
                Instruction::CallFfi(wire::CallFFIPayload {
                    arg_count: 2,
                    result_count: 3,
                }),
                "",
            );
        }
        let mut arguments = self.running.pop_values(&self.frame_pool, arg_count)?;
        if matches!(
            intrinsic,
            SyncMutexLock | SyncMutexTryLock | SyncMutexUnlock
        ) {
            return self.execute_mutex(id, &arguments[0]);
        }
        if matches!(
            intrinsic,
            ReflectMakeChan
                | ReflectValueSend
                | ReflectValueRecv
                | ReflectValueTrySend
                | ReflectValueTryRecv
                | ReflectValueClose
                | ReflectValueCall
                | ReflectMakeFunc
                | ReflectSelect
        ) {
            return self.execute_reflect_async(id, arguments);
        }
        if intrinsic.is_reflect() {
            let results = self.execute_reflect(id, arguments)?;
            if results.len() != result_count {
                return Err(RuntimeError::new(
                    "invalid_intrinsic",
                    id,
                    "result count mismatch",
                ));
            }
            self.running
                .frames
                .last_mut()
                .unwrap()
                .stack
                .extend(results);
            return Ok(());
        }
        let mut results = Vec::new();
        match intrinsic {
            MathFloat32Bits | MathFloat64Bits => {
                let Data::Float(value) = arguments[0].data else {
                    return Err(RuntimeError::new("type_error", id, "expected float"));
                };
                let narrow = intrinsic == MathFloat32Bits;
                results.push(Value {
                    typ: TypeIdentity::Primitive(if narrow {
                        wire::PrimitiveUint32
                    } else {
                        wire::PrimitiveUint64
                    }),
                    data: Data::Unsigned(if narrow {
                        u64::from((value as f32).to_bits())
                    } else {
                        value.to_bits()
                    }),
                });
            }
            MathFloat32FromBits | MathFloat64FromBits => {
                let Data::Unsigned(value) = arguments[0].data else {
                    return Err(RuntimeError::new(
                        "type_error",
                        id,
                        "expected unsigned integer",
                    ));
                };
                let narrow = intrinsic == MathFloat32FromBits;
                results.push(Value {
                    typ: TypeIdentity::Primitive(if narrow {
                        wire::PrimitiveFloat32
                    } else {
                        wire::PrimitiveFloat64
                    }),
                    data: Data::Float(if narrow {
                        f64::from(f32::from_bits(value as u32))
                    } else {
                        f64::from_bits(value)
                    }),
                });
            }
            CryptoSha256Block => {
                let Data::Array(state) = &arguments[0].data else {
                    return Err(RuntimeError::new("type_error", id, "expected digest state"));
                };
                if state.len() != 8 {
                    return Err(RuntimeError::new(
                        "type_error",
                        id,
                        "SHA256 state requires eight words",
                    ));
                }
                let mut words = [0u32; 8];
                for (word, value) in words.iter_mut().zip(state) {
                    let Data::Unsigned(value) = value.data else {
                        return Err(RuntimeError::new(
                            "type_error",
                            id,
                            "expected unsigned state word",
                        ));
                    };
                    *word = value as u32;
                }
                let values = self.slice_values(&arguments[1])?;
                if !values.len().is_multiple_of(64) {
                    return Err(RuntimeError::new(
                        "type_error",
                        id,
                        "SHA256 input requires complete blocks",
                    ));
                }
                for block in values.as_chunks::<64>().0 {
                    let mut bytes = [0u8; 64];
                    for (byte, value) in bytes.iter_mut().zip(block) {
                        let Data::Unsigned(value) = value.data else {
                            return Err(RuntimeError::new("type_error", id, "expected byte"));
                        };
                        *byte = u8::try_from(value)
                            .map_err(|_| RuntimeError::new("type_error", id, "byte overflow"))?;
                    }
                    sha2::compress256(&mut words, &[bytes.into()]);
                }
                self.charge_guest_object(words.len(), 0)?;
                results.push(Value {
                    typ: arguments[0].typ.clone(),
                    data: Data::Array(
                        words
                            .into_iter()
                            .map(|word| Value {
                                typ: TypeIdentity::Primitive(wire::PrimitiveUint32),
                                data: Data::Unsigned(u64::from(word)),
                            })
                            .collect(),
                    ),
                });
            }
            CryptoRandRead => {
                let value = arguments.pop().unwrap();
                let length = match &value.data {
                    Data::Slice(slice) => slice.length,
                    Data::Nil => 0,
                    _ => {
                        return Err(RuntimeError::new("type_error", id, "expected byte slice"));
                    }
                };
                let mut bytes = vec![0; length];
                let (mut count, mut error) = if length == 0 {
                    (0, None)
                } else {
                    self.entropy.read(&mut bytes)
                };
                if count > length {
                    count = 0;
                    error = Some(RuntimeError::new("entropy", "read", "invalid read count"));
                } else if count == 0 && length > 0 && error.is_none() {
                    error = Some(RuntimeError::new("entropy", "read", "no progress"));
                }
                for (index, byte) in bytes.into_iter().take(count).enumerate() {
                    self.store_index(
                        value.clone(),
                        Value::int(index as i64),
                        Value {
                            typ: TypeIdentity::Primitive(wire::PrimitiveUint8),
                            data: Data::Unsigned(u64::from(byte)),
                        },
                    )?;
                }
                results.push(Value::int(count as i64));
                results.push(Value {
                    typ: TypeIdentity::Primitive(wire::PrimitiveString),
                    data: Data::String(
                        error
                            .as_ref()
                            .map(ToString::to_string)
                            .unwrap_or_default()
                            .into_bytes()
                            .into(),
                    ),
                });
                results.push(Value::boolean(error.is_none()));
            }
            TimeNow => {
                let (seconds, nanos) = self.clock.unix_time();
                results.extend([seconds, i64::from(nanos)].map(|value| Value {
                    typ: TypeIdentity::Primitive(wire::PrimitiveInt64),
                    data: Data::Integer(value),
                }));
            }
            TimeTimerStart => {
                let period = arguments[2].integer()?.max(0) as u64;
                let delay = arguments[1].integer()?.max(0) as u64;
                let channel = arguments.remove(0);
                let Data::ResourceRef(handle) = channel.data else {
                    return Err(RuntimeError::new(
                        "type_error",
                        id,
                        "expected timer channel",
                    ));
                };
                if let Some(previous) = self.timers.remove(handle) {
                    self.scope_work.entry(previous.scope).or_default().timers -= 1;
                    self.changed_scopes.insert(previous.scope);
                }
                if self.timers.len() >= self.limits.max_tasks {
                    return Err(RuntimeError::new(
                        "task_limit",
                        "timer",
                        "timer limit exceeded",
                    ));
                }
                self.timers.push(Timer {
                    order: 0,
                    scope: self.running.scope,
                    channel,
                    deadline: self.clock.monotonic_ns().saturating_add(delay),
                    period,
                });
                self.scope_work
                    .entry(self.running.scope)
                    .or_default()
                    .timers += 1;
                self.changed_scopes.insert(self.running.scope);
            }
            TimeTimerStop => {
                let Data::ResourceRef(handle) = arguments[0].data else {
                    return Err(RuntimeError::new(
                        "type_error",
                        id,
                        "expected timer channel",
                    ));
                };
                let previous = self.timers.remove(handle);
                if let Some(timer) = &previous {
                    self.scope_work.entry(timer.scope).or_default().timers -= 1;
                    self.changed_scopes.insert(timer.scope);
                    self.close_channel(&arguments[0])?;
                }
                results.push(Value::boolean(previous.is_some()));
            }
            _ => {
                return Err(RuntimeError::new(
                    "unsupported_intrinsic",
                    id,
                    "unrecognized intrinsic dispatch",
                ));
            }
        }
        if results.len() != result_count {
            return Err(RuntimeError::new(
                "invalid_intrinsic",
                id,
                "result count mismatch",
            ));
        }
        self.running
            .frames
            .last_mut()
            .unwrap()
            .stack
            .append(&mut results);
        self.frame_pool
            .recycle_operands(arguments, self.limits.max_frame_cache_bytes);
        self.frame_pool
            .recycle_operands(results, self.limits.max_frame_cache_bytes);
        Ok(())
    }
}
