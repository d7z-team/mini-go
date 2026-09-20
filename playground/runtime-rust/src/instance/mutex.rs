use super::*;
use scheduler::{Blocked, Resource};

impl Instance {
    pub(super) fn execute_mutex(&mut self, id: &str, pointer: &Value) -> Result<(), RuntimeError> {
        let Data::Pointer(address) = &pointer.data else {
            return Err(RuntimeError::new("panic", id, "nil mutex pointer"));
        };
        let mut state = self.read_address(address)?;
        if matches!(state.data, Data::Nil) {
            if id == "sync.mutex_unlock" {
                return Err(RuntimeError::new(
                    "panic",
                    id,
                    "sync: unlock of unlocked mutex",
                ));
            }
            self.charge_guest_object(0, 0)?;
            let handle = self.allocate(Value {
                typ: TypeIdentity::Any,
                data: Data::Resource(Box::new(Resource::Mutex {
                    locked: false,
                    grant: None,
                    waiters: VecDeque::new(),
                })),
            })?;
            self.running.transient_roots.push(handle);
            state.data = Data::ResourceRef(handle);
            self.write_address(address, state.clone())?;
        }
        let handle = Self::resource_handle(&state)?;
        self.running.transient_roots.push(handle);
        let mut resource = self.resource(handle)?.clone();
        let Resource::Mutex {
            locked,
            grant,
            waiters,
        } = &mut resource
        else {
            return Err(RuntimeError::new(
                "type_error",
                id,
                "invalid mutex resource",
            ));
        };
        let available = !*locked && grant.is_none();
        match id {
            "sync.mutex_lock" => {
                if available {
                    *locked = true;
                    self.store_resource(handle, resource)?;
                } else {
                    self.charge_guest_object(1, 0)?;
                    waiters.push_back(self.running.id);
                    self.store_resource(handle, resource)?;
                    self.park(Blocked::Mutex(handle));
                }
            }
            "sync.mutex_try_lock" => {
                if available {
                    *locked = true;
                    self.store_resource(handle, resource)?;
                }
                self.running
                    .frames
                    .last_mut()
                    .unwrap()
                    .stack
                    .push(Value::boolean(available));
            }
            "sync.mutex_unlock" => {
                if !*locked {
                    return Err(RuntimeError::new(
                        "panic",
                        id,
                        "sync: unlock of unlocked mutex",
                    ));
                }
                *locked = false;
                *grant = waiters.pop_front();
                self.store_resource(handle, resource)?;
            }
            _ => unreachable!(),
        }
        Ok(())
    }

    pub(super) fn cancel_mutex_wait(
        &mut self,
        handle: Handle,
        task: u64,
    ) -> Result<(), RuntimeError> {
        let mut resource = self.resource(handle)?.clone();
        let Resource::Mutex { grant, waiters, .. } = &mut resource else {
            unreachable!()
        };
        if *grant == Some(task) {
            *grant = waiters.pop_front();
        } else {
            waiters.retain(|waiting| *waiting != task);
        }
        self.store_resource(handle, resource)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cancellation_transfers_undelivered_grant_and_preserves_delivered_lock() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let task = vm.running.id;
        let first = task + 1;
        let removed = task + 2;
        let handle = vm
            .allocate(Value {
                typ: TypeIdentity::Any,
                data: Data::Resource(Box::new(Resource::Mutex {
                    locked: false,
                    grant: Some(first),
                    waiters: VecDeque::from([removed, task]),
                })),
            })
            .unwrap();
        vm.park(Blocked::Mutex(handle));
        vm.collect_at_boundary().unwrap();
        vm.cancel_mutex_wait(handle, removed).unwrap();
        vm.cancel_mutex_wait(handle, first).unwrap();
        assert!(
            matches!(&*vm.resource(handle).unwrap(), Resource::Mutex { locked: false, grant: Some(owner), waiters } if *owner == task && waiters.is_empty())
        );
        vm.resume_blocked().unwrap();
        assert!(vm.schedule_next());
        vm.cancel_mutex_wait(handle, task).unwrap();
        assert!(
            matches!(&*vm.resource(handle).unwrap(), Resource::Mutex { locked: true, grant: None, waiters } if waiters.is_empty())
        );
        vm.close().unwrap();
        assert_eq!(vm.heap_stats().live_objects, 0);
    }
}
