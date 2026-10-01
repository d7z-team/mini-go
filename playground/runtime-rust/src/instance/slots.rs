use super::*;

impl Frame {
    pub(super) fn begin_slots(&mut self, pc: usize) {
        let operands = &self.prepared.operands;
        if self.slot_pc != Some(pc) {
            if let Some(previous) = self.slot_pc {
                for &slot in operands.release_at(previous) {
                    let value = &mut self.slot_values[slot as usize];
                    if value.is_some() {
                        *value = None;
                    }
                }
            }
            self.slot_constants.clear();
            for &slot in operands.release_before_at(pc) {
                let value = &mut self.slot_values[slot as usize];
                if value.is_some() {
                    *value = None;
                }
            }
        }
        self.slot_pc = Some(pc);
        self.slot_input = operands.inputs_at(pc).len();
        self.slot_output = 0;
        self.delivery.reading = false;
    }

    pub(super) fn operand_count(&self, pc: usize) -> usize {
        self.prepared.operands.inputs_at(pc).len()
    }

    pub(super) fn operand_from_end(&self, pc: usize, offset: usize) -> Option<&Value> {
        let position = self.operand_count(pc).checked_sub(offset + 1)?;
        self.input_at(pc, position)
    }

    pub(super) fn input_at(&self, pc: usize, position: usize) -> Option<&Value> {
        let input = self.prepared.operands.inputs_at(pc).get(position)?;
        if input.kind == 0 {
            return self.slot_values[input.index as usize].as_ref();
        }
        if input.kind == 2 {
            return match &self.locals[input.index as usize] {
                frame::Local::Private(value)
                    if !matches!(value.get().data, Data::Uninitialized) =>
                {
                    Some(value.get())
                }
                _ => None,
            };
        }
        let index = input.index as usize;
        if let crate::program::PreparedConstant::Scalar(value) =
            &self.revision.program.constant_data[index]
        {
            return Some(value);
        }
        if self.slot_pc != Some(pc) {
            return None;
        }
        self.slot_constants
            .iter()
            .find(|(key, _)| *key == index)
            .map(|(_, value)| value)
    }

    pub(super) fn input_value(&self, position: usize) -> Result<Value, RuntimeError> {
        self.input_at(self.slot_pc.unwrap(), position)
            .cloned()
            .ok_or_else(|| {
                RuntimeError::new("invalid_operand", "frame", "input is not initialized")
            })
    }

    // Release and uniqueness are checked during loading. Runtime identity is
    // still checked against the current callee, which may come from a patch.
    pub(super) fn transferable_input(&self, position: usize, typ: &TypeIdentity) -> Option<u32> {
        let pc = self.slot_pc?;
        let source = self.prepared.operands.transfers_at(pc)[position];
        (source != u32::MAX && self.slot_values[source as usize].as_ref()?.typ == *typ)
            .then_some(source)
    }

    pub(super) fn push_result(&mut self, value: Value) {
        if self.continuation.is_some() {
            self.delivery.values.push(value);
            return;
        }
        let outputs = self.prepared.operands.outputs_at(self.slot_pc.unwrap());
        let output = outputs[self.slot_output];
        if output & crate::program::LOCAL_OUTPUT != 0 {
            let frame::Local::Private(local) =
                &mut self.locals[(output & !crate::program::LOCAL_OUTPUT) as usize]
            else {
                unreachable!("validated private output")
            };
            // The loader allows only fixed-size scalar results here. Both an
            // uninitialized scalar and its initialized value charge 144 bytes.
            local
                .replace(value, 144)
                .expect("fixed-size scalar output cannot grow");
        } else {
            self.slot_values[output as usize] = Some(value);
        }
        self.slot_output += 1;
    }

    pub(super) fn extend_results(&mut self, values: impl IntoIterator<Item = Value>) {
        for value in values {
            self.push_result(value);
        }
    }
}

impl Instance {
    pub(super) fn begin_slot_instruction(&mut self, pc: usize) -> Result<(), RuntimeError> {
        let frame = self.running.frames.last_mut().unwrap();
        frame.begin_slots(pc);
        if !frame.prepared.lazy_slot_inputs[pc] {
            return Ok(());
        }
        // Scalar constants and initialized private locals can be borrowed
        // directly. Retain program/revision handles only when materialization
        // actually needs to reborrow the instance.
        let Some(first_missing) = frame
            .prepared
            .operands
            .inputs_at(pc)
            .iter()
            .enumerate()
            .find_map(|(position, _)| frame.input_at(pc, position).is_none().then_some(position))
        else {
            return Ok(());
        };
        let function = frame.prepared.clone();
        let operands = &function.operands;
        let revision = frame.revision.clone();
        for input in operands.inputs_at(pc)[first_missing..].iter() {
            if input.kind == 2 {
                let local = &self.running.frames.last().unwrap().locals[input.index as usize];
                if matches!(local, frame::Local::Private(value) if matches!(value.get().data, Data::Uninitialized))
                {
                    self.load_local(input.index as usize)?;
                }
                continue;
            }
            if input.kind != 1
                || matches!(
                    revision.program.constant_data[input.index as usize],
                    crate::program::PreparedConstant::Scalar(_)
                )
            {
                continue;
            }
            let index = input.index as usize;
            if self
                .running
                .frames
                .last()
                .unwrap()
                .slot_constants
                .iter()
                .any(|(key, _)| *key == index)
            {
                continue;
            }
            let value = self.load_prepared_constant(&revision, index)?;
            self.running
                .frames
                .last_mut()
                .unwrap()
                .slot_constants
                .push((index, value));
        }
        Ok(())
    }
}
