use super::*;

impl Frame {
    pub(super) fn begin_slots(&mut self, pc: usize) {
        if self.slot_pc != Some(pc) {
            if let Some(previous) = self.slot_pc {
                let release_count = self.prepared.operands.release_at(previous).len();
                for release_index in 0..release_count {
                    let slot = self.prepared.operands.release_at(previous)[release_index];
                    let index = slot as usize;
                    if let Some(typ) = self.prepared.slot_types.get(index) {
                        if super::frame::inline_type(typ) {
                            self.slot_scalar_values.clear(index);
                        } else {
                            self.slot_aggregate_values.clear(index);
                        }
                    }
                    if let Some(handle) = self.slot_handles.get_mut(index) {
                        *handle = None;
                    }
                }
            }
            self.slot_constants.clear();
            let release_count = self.prepared.operands.release_before_at(pc).len();
            for release_index in 0..release_count {
                let slot = self.prepared.operands.release_before_at(pc)[release_index];
                let index = slot as usize;
                if let Some(typ) = self.prepared.slot_types.get(index) {
                    if super::frame::inline_type(typ) {
                        self.slot_scalar_values.clear(index);
                    } else {
                        self.slot_aggregate_values.clear(index);
                    }
                }
                if let Some(handle) = self.slot_handles.get_mut(index) {
                    *handle = None;
                }
            }
        }
        self.slot_pc = Some(pc);
        self.slot_input = self.prepared.operands.inputs_at(pc).len();
        self.slot_output = 0;
        self.delivery.reading = false;
    }

    pub(super) fn operand_count(&self, pc: usize) -> usize {
        self.prepared.operands.inputs_at(pc).len()
    }

    pub(super) fn operand_from_end(&self, pc: usize, offset: usize) -> Option<Value> {
        let position = self.operand_count(pc).checked_sub(offset + 1)?;
        self.input_at(pc, position)
    }

    pub(super) fn input_type_at(&self, pc: usize, position: usize) -> Option<&TypeIdentity> {
        let input = self.prepared.operands.inputs_at(pc).get(position)?;
        match input.kind {
            0 => self.prepared.slot_types.get(input.index as usize),
            2 => self.prepared.local_types.get(input.index as usize),
            _ => None,
        }
    }

    /// Select a field or element from its prepared storage, decoding only the
    /// selected value. Both temporary slots and addressable locals use this
    /// path; reading one leaf never materializes its aggregate ancestors.
    pub(super) fn input_prepared_path(
        &self,
        pc: usize,
        position: usize,
        path: &[usize],
        types: &TypeRegistry,
    ) -> Result<Option<Value>, RuntimeError> {
        let Some(input) = self.prepared.operands.inputs_at(pc).get(position) else {
            return Ok(None);
        };
        let index = input.index as usize;
        let handle = match input.kind {
            0 => {
                if self.slot_aggregate_values.has_plan(index) {
                    return self
                        .slot_aggregate_values
                        .get_path(index, path, &self.heap, types);
                }
                self.slot_handles.get(index).copied().flatten()
            }
            2 => match self.locals.get(index) {
                Some(frame::Local::Aggregate(slot)) => {
                    return self
                        .aggregate_values
                        .get_path(*slot, path, &self.heap, types);
                }
                Some(frame::Local::Private(handle) | frame::Local::Shared(handle)) => Some(*handle),
                _ => None,
            },
            _ => None,
        };
        match (handle, self.input_type_at(pc, position)) {
            (Some(handle), Some(typ)) => self.heap.read_prepared_path(handle, typ, path),
            _ => Ok(None),
        }
    }

    /// Read a sequence bound from its prepared header or static type. This
    /// keeps Len/Cap on managed locals from decoding all elements into a
    /// recursive `Value`.
    pub(super) fn input_prepared_len_cap(
        &self,
        pc: usize,
        position: usize,
        capacity: bool,
        types: &TypeRegistry,
    ) -> Result<Option<usize>, RuntimeError> {
        let Some(typ) = self.input_type_at(pc, position) else {
            return Ok(None);
        };
        if let Some(crate::types::StorageTypeLayout::Array { length, .. }) =
            types.storage_layout(typ)?
        {
            return Ok(Some(length));
        }
        if let Some(element) = types.pointer_element(typ)?
            && let Some(crate::types::StorageTypeLayout::Array { length, .. }) =
                types.storage_layout(&element)?
        {
            return Ok(Some(length));
        }
        if !matches!(
            types.storage_layout(typ)?,
            Some(crate::types::StorageTypeLayout::Slice { .. })
        ) {
            return Ok(None);
        }
        let input = self.prepared.operands.inputs_at(pc).get(position).unwrap();
        let bounds = match input.kind {
            0 => {
                let index = input.index as usize;
                if self.slot_aggregate_values.has_plan(index) {
                    self.slot_aggregate_values.slice_bounds(index, typ, types)?
                } else {
                    self.slot_handles
                        .get(index)
                        .and_then(|handle| *handle)
                        .map(|handle| {
                            let mut bytes = [0_u8; 48];
                            self.heap.read_storage_into(handle, 0, &mut bytes)?;
                            Ok::<_, RuntimeError>(Self::slice_bounds_from_bytes(&bytes))
                        })
                        .transpose()?
                        .flatten()
                }
            }
            2 => match self.locals.get(input.index as usize) {
                Some(frame::Local::Aggregate(slot)) => {
                    self.aggregate_values.slice_bounds(*slot, typ, types)?
                }
                Some(frame::Local::Private(handle) | frame::Local::Shared(handle)) => {
                    let mut bytes = [0_u8; 48];
                    self.heap.read_storage_into(*handle, 0, &mut bytes)?;
                    Self::slice_bounds_from_bytes(&bytes)
                }
                _ => None,
            },
            _ => None,
        };
        Ok(bounds.map(|(length, bound)| if capacity { bound } else { length }))
    }

    fn slice_bounds_from_bytes(bytes: &[u8]) -> Option<(usize, usize)> {
        if bytes.len() < 48 {
            return None;
        }
        let length = usize::try_from(u64::from_le_bytes(bytes[32..40].try_into().ok()?)).ok()?;
        let capacity = usize::try_from(u64::from_le_bytes(bytes[40..48].try_into().ok()?)).ok()?;
        Some((length, capacity))
    }

    /// Check operand readiness from prepared liveness metadata.  This keeps
    /// slot setup from decoding a complete aggregate/string just to discover
    /// whether its input is initialized.
    pub(super) fn input_initialized_at(&self, pc: usize, position: usize) -> bool {
        let Some(input) = self.prepared.operands.inputs_at(pc).get(position) else {
            return false;
        };
        match input.kind {
            0 => {
                let index = input.index as usize;
                let Some(typ) = self.prepared.slot_types.get(index) else {
                    return false;
                };
                if super::frame::inline_type(typ) {
                    return !self.slot_scalar_values.is_uninitialized(index);
                }
                if self.slot_aggregate_values.has_plan(index) {
                    return !self.slot_aggregate_values.is_uninitialized(index);
                }
                self.slot_handles
                    .get(index)
                    .and_then(|handle| *handle)
                    .is_some_and(|handle| self.heap.payload_initialized(handle).unwrap_or(false))
            }
            1 => {
                let index = input.index as usize;
                matches!(
                    self.revision.program.constant_data.get(index),
                    Some(crate::program::PreparedConstant::Scalar(_))
                ) || (self.slot_pc == Some(pc)
                    && self.slot_constants.iter().any(|(key, _)| *key == index))
            }
            2 => match self.locals.get(input.index as usize) {
                Some(frame::Local::Private(handle) | frame::Local::Shared(handle)) => {
                    self.heap.payload_initialized(*handle).unwrap_or(false)
                }
                Some(frame::Local::Inline(slot)) => !self.scalar_values.is_uninitialized(*slot),
                Some(frame::Local::Aggregate(slot)) => {
                    !self.aggregate_values.is_uninitialized(*slot)
                }
                Some(frame::Local::Uninitialized) | None => false,
            },
            _ => false,
        }
    }

    /// Copy a prepared aggregate input into an addressable aggregate local
    /// without decoding the source into a recursive `Value`. The ordinary
    /// store-local path uses this before its Value fallback; scalar locals and
    /// managed private destinations retain their existing semantics.
    pub(super) fn copy_prepared_input_to_aggregate_local(
        &mut self,
        pc: usize,
        local: usize,
        types: &TypeRegistry,
    ) -> Result<bool, RuntimeError> {
        let destination = match self.locals.get(local) {
            Some(frame::Local::Aggregate(destination)) => *destination,
            _ => return Ok(false),
        };
        let Some(input) = self.prepared.operands.inputs_at(pc).last().cloned() else {
            return Ok(false);
        };
        match input.kind {
            0 => {
                let source = input.index as usize;
                if self.slot_aggregate_values.has_plan(source) {
                    if self.slot_aggregate_values.is_uninitialized(source) {
                        return Ok(false);
                    }
                    return Ok(self.aggregate_values.copy_slot_from(
                        destination,
                        &self.slot_aggregate_values,
                        source,
                    ));
                }
                let Some(handle) = self.slot_handles.get(source).and_then(|handle| *handle) else {
                    return Ok(false);
                };
                let Some(source_type) = self.prepared.slot_types.get(source).cloned() else {
                    return Ok(false);
                };
                self.aggregate_values.copy_slot_from_heap(
                    destination,
                    handle,
                    &source_type,
                    &self.heap,
                    types,
                )
            }
            2 => match self.locals.get(input.index as usize) {
                Some(frame::Local::Private(handle) | frame::Local::Shared(handle)) => {
                    let Some(source_type) =
                        self.prepared.local_types.get(input.index as usize).cloned()
                    else {
                        return Ok(false);
                    };
                    self.aggregate_values.copy_slot_from_heap(
                        destination,
                        *handle,
                        &source_type,
                        &self.heap,
                        types,
                    )
                }
                // A local-to-local aggregate move has a separate move_source
                // fast path in the executor; leave ordinary aliasing stores to
                // the Value path until they can be staged without overlap.
                _ => Ok(false),
            },
            _ => Ok(false),
        }
    }

    pub(super) fn input_at(&self, pc: usize, position: usize) -> Option<Value> {
        self.read_input_at(pc, position).ok().flatten()
    }

    fn read_input_at(&self, pc: usize, position: usize) -> Result<Option<Value>, RuntimeError> {
        let Some(input) = self.prepared.operands.inputs_at(pc).get(position) else {
            return Ok(None);
        };
        if input.kind == 0 {
            return self.read_slot_value(input.index as usize);
        }
        if input.kind == 2 {
            let value = match &self.locals[input.index as usize] {
                frame::Local::Private(handle) | frame::Local::Shared(handle) => {
                    Some(self.heap.read_typed_value(*handle)?)
                }
                frame::Local::Inline(slot) => Some(
                    self.scalar_values
                        .get(*slot, &self.prepared.local_types[input.index as usize])
                        .value(&self.prepared.local_types[input.index as usize]),
                ),
                frame::Local::Aggregate(slot) => self.aggregate_values.get(
                    *slot,
                    &self.prepared.local_types[input.index as usize],
                    &self.heap,
                    self.revision.program.decoded.types(),
                )?,
                frame::Local::Uninitialized => None,
            };
            return Ok(value.filter(|value| !matches!(value.data, Data::Uninitialized)));
        }
        let index = input.index as usize;
        if let crate::program::PreparedConstant::Scalar(value) =
            &self.revision.program.constant_data[index]
        {
            return Ok(Some(value.clone()));
        }
        if self.slot_pc != Some(pc) {
            return Ok(None);
        }
        Ok(self
            .slot_constants
            .iter()
            .find(|(key, _)| *key == index)
            .map(|(_, value)| value.clone()))
    }

    pub(super) fn input_value(&self, position: usize) -> Result<Value, RuntimeError> {
        self.read_input_at(self.slot_pc.unwrap(), position)?
            .ok_or_else(|| {
                RuntimeError::new("invalid_operand", "frame", "input is not initialized")
            })
    }

    // Release and uniqueness are checked during loading. Runtime identity is
    // still checked against the current callee, which may come from a patch.
    pub(super) fn transferable_input(&self, position: usize, typ: &TypeIdentity) -> Option<u32> {
        let pc = self.slot_pc?;
        let source = self.prepared.operands.transfers_at(pc)[position];
        if let Some(source) = self.transferable_aggregate_input(position, typ) {
            return Some(source as u32);
        }
        if let Some(source) = self.transferable_scalar_input(position, typ) {
            return Some(source as u32);
        }
        (source != u32::MAX && self.slot_value(source as usize)?.typ == *typ).then_some(source)
    }

    pub(super) fn transferable_aggregate_input(
        &self,
        position: usize,
        typ: &TypeIdentity,
    ) -> Option<usize> {
        let pc = self.slot_pc?;
        let source = *self.prepared.operands.transfers_at(pc).get(position)?;
        if source == u32::MAX {
            return None;
        }
        let source = source as usize;
        (self.prepared.slot_types.get(source)? == typ
            && self.slot_aggregate_values.has_plan(source)
            && !self.slot_aggregate_values.is_uninitialized(source))
        .then_some(source)
    }

    /// Return the prepared scalar source lane for a caller argument when the
    /// destination type is exact. The caller owns the transfer/release
    /// decision; this method only proves that the source is initialized and
    /// represented by the scalar bank.
    pub(super) fn transferable_scalar_input(
        &self,
        position: usize,
        typ: &TypeIdentity,
    ) -> Option<usize> {
        let pc = self.slot_pc?;
        let source = *self.prepared.operands.transfers_at(pc).get(position)?;
        if source == u32::MAX {
            return None;
        }
        let source = source as usize;
        (self.prepared.slot_types.get(source)? == typ
            && super::frame::inline_type(typ)
            && !self.slot_scalar_values.is_uninitialized(source))
        .then_some(source)
    }

    pub(super) fn scalar_input(
        &self,
        position: usize,
        typ: &TypeIdentity,
    ) -> Option<(&frame::ScalarBanks, usize)> {
        if !frame::inline_type(typ) {
            return None;
        }
        let input = self
            .prepared
            .operands
            .inputs_at(self.slot_pc?)
            .get(position)?;
        let index = input.index as usize;
        let (bank, slot, source_type) = match input.kind {
            0 => (
                &self.slot_scalar_values,
                index,
                self.prepared.slot_types.get(index)?,
            ),
            2 => match self.locals.get(index)? {
                frame::Local::Inline(slot) => (
                    &self.scalar_values,
                    *slot,
                    self.prepared.local_types.get(index)?,
                ),
                _ => return None,
            },
            _ => return None,
        };
        (source_type == typ && !bank.is_uninitialized(slot)).then_some((bank, slot))
    }

    pub(super) fn push_result(&mut self, value: &Value) -> Result<(), RuntimeError> {
        if self.continuation.is_some() {
            let old_capacity = self.delivery.handles.capacity();
            let new_length = self.delivery.handles.len().saturating_add(1);
            let new_capacity = old_capacity
                .max(new_length)
                .max(1)
                .checked_next_power_of_two()
                .ok_or_else(|| {
                    RuntimeError::new(
                        "allocation_limit",
                        "frame",
                        "result buffer capacity overflows",
                    )
                })?;
            self.ensure_resident_growth(old_capacity, new_capacity, size_of::<Handle>())?;
            let bytes = self.heap.logical_allocation_bytes(value)?;
            let handle = self
                .heap
                .allocate(value.clone(), bytes)
                .map_err(|(error, _)| error)?;
            self.delivery.handles.push(handle);
            return Ok(());
        }
        let outputs = self.prepared.operands.outputs_at(self.slot_pc.unwrap());
        let output = outputs[self.slot_output];
        if output & crate::program::LOCAL_OUTPUT != 0 {
            let index = (output & !crate::program::LOCAL_OUTPUT) as usize;
            match &self.locals[index] {
                frame::Local::Inline(slot) => {
                    self.scalar_values.set(*slot, value).map_err(|error| {
                        RuntimeError::new(
                            error.code,
                            "frame",
                            format!("failed to write scalar result: {}", error.message),
                        )
                    })?;
                }
                frame::Local::Aggregate(slot) => {
                    self.aggregate_values.set(
                        *slot,
                        value,
                        &self.heap,
                        self.revision.program.decoded.types(),
                    )?;
                }
                frame::Local::Private(local) => {
                    // Large aggregates retain a stable managed identity;
                    // small values use the prepared frame banks above.
                    let bytes = self.heap.allocation_bytes(*local)?;
                    self.heap
                        .replace(*local, value.clone(), bytes)
                        .map_err(|(error, _)| error)?;
                }
                frame::Local::Shared(_) => unreachable!("validated private output"),
                frame::Local::Uninitialized => {
                    return Err(RuntimeError::new(
                        "invalid_local",
                        "frame",
                        "uninitialized local requires a frame write",
                    ));
                }
            }
        } else {
            self.put_slot_value(output as usize, value.clone())?;
        }
        self.slot_output += 1;
        Ok(())
    }

    pub(super) fn extend_results(
        &mut self,
        values: impl IntoIterator<Item = Value>,
    ) -> Result<(), RuntimeError> {
        for value in values {
            self.push_result(&value)?;
        }
        Ok(())
    }

    /// Publish a non-continuation result group atomically. FFI replies are
    /// delivered outside the instruction executor, so writing them one by one
    /// can otherwise leave the output cursor and the first managed slot
    /// changed when a later allocation fails.
    pub(super) fn publish_results_atomic(
        &mut self,
        values: Vec<Value>,
    ) -> Result<(), RuntimeError> {
        if self.continuation.is_some() {
            return self.extend_results(values);
        }
        if values.is_empty() {
            return Ok(());
        }
        let pc = self.slot_pc.ok_or_else(|| {
            RuntimeError::new("invalid_frame", "frame", "result has no active instruction")
        })?;
        let outputs = self.prepared.operands.outputs_at(pc);
        let end = self.slot_output.checked_add(values.len()).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "frame", "result count overflows")
        })?;
        let output_slice = outputs.get(self.slot_output..end).ok_or_else(|| {
            RuntimeError::new(
                "invalid_result",
                "frame",
                "result count exceeds instruction outputs",
            )
        })?;

        // Frame slot storage is prepared at activation. Do not grow it after
        // staging: a resident admission followed by a failed heap batch would
        // otherwise change accounting even though publication failed.
        let mut staged_slot_handles = self.slot_handles.clone();
        let mut staged_scalar_values = self.scalar_values.clone();
        let mut staged_aggregate_values = self.aggregate_values.clone();
        let mut staged_slot_scalar_values = self.slot_scalar_values.clone();
        let mut staged_slot_aggregate_values = self.slot_aggregate_values.clone();
        let mut replacements = Vec::new();
        let mut allocations = Vec::new();
        let mut allocation_targets = Vec::new();
        let mut local_updates = Vec::new();

        // Run all side-effect-free shape checks first. Aggregate encoding can
        // allocate nested backing records, so a later type error must be
        // reported before the first payload asks the heap to publish anything.
        for (output, value) in output_slice.iter().copied().zip(values.iter()) {
            if output & crate::program::LOCAL_OUTPUT != 0 {
                let index = (output & !crate::program::LOCAL_OUTPUT) as usize;
                if let Some(frame::Local::Aggregate(slot)) = self.locals.get(index) {
                    staged_aggregate_values.validate_value(
                        *slot,
                        value,
                        self.revision.program.decoded.types(),
                    )?;
                } else if matches!(self.locals.get(index), Some(frame::Local::Uninitialized))
                    && staged_aggregate_values.has_plan(index)
                {
                    staged_aggregate_values.validate_value(
                        index,
                        value,
                        self.revision.program.decoded.types(),
                    )?;
                }
                continue;
            }
            let index = output as usize;
            if let Some(typ) = self.prepared.slot_types.get(index)
                && !super::frame::inline_type(typ)
                && staged_slot_aggregate_values.has_plan(index)
            {
                staged_slot_aggregate_values.validate_value(
                    index,
                    value,
                    self.revision.program.decoded.types(),
                )?;
            }
        }

        // Aggregate encoders may allocate nested backing records before the
        // destination lane is copied. Keep those allocations in the same
        // transaction as the output group; a later codec or quota failure
        // must release both direct and nested records.
        let allocation_scope = self.heap.begin_allocation_scope();

        for (output, value) in output_slice.iter().copied().zip(values) {
            if output & crate::program::LOCAL_OUTPUT != 0 {
                let index = (output & !crate::program::LOCAL_OUTPUT) as usize;
                match self.locals.get(index) {
                    Some(frame::Local::Inline(slot)) => {
                        staged_scalar_values.set(*slot, &value).map_err(|error| {
                            RuntimeError::new(
                                error.code,
                                "frame",
                                format!("failed to write scalar result: {}", error.message),
                            )
                        })?;
                    }
                    Some(frame::Local::Aggregate(slot)) => {
                        staged_aggregate_values.set(
                            *slot,
                            &value,
                            &self.heap,
                            self.revision.program.decoded.types(),
                        )?;
                    }
                    Some(frame::Local::Private(handle)) => {
                        let bytes = self.heap.allocation_bytes(*handle)?;
                        replacements.push((*handle, value, bytes));
                    }
                    Some(frame::Local::Uninitialized) => {
                        if super::frame::inline_type(&self.prepared.local_types[index]) {
                            staged_scalar_values.set(index, &value)?;
                            local_updates.push((index, frame::Local::Inline(index)));
                        } else if staged_aggregate_values.has_plan(index) {
                            staged_aggregate_values.set(
                                index,
                                &value,
                                &self.heap,
                                self.revision.program.decoded.types(),
                            )?;
                            local_updates.push((index, frame::Local::Aggregate(index)));
                        } else {
                            let bytes = self.heap.logical_allocation_bytes(&value)?;
                            allocations.push((value, bytes));
                            allocation_targets.push(output);
                        }
                    }
                    Some(frame::Local::Shared(_)) | None => {
                        return Err(RuntimeError::new(
                            "invalid_local",
                            "frame",
                            "result target is not a writable local",
                        ));
                    }
                }
                continue;
            }

            let index = output as usize;
            let typ = self.prepared.slot_types.get(index).ok_or_else(|| {
                RuntimeError::new("invalid_operand", "frame", "slot is out of range")
            })?;
            if super::frame::inline_type(typ) {
                staged_slot_scalar_values
                    .set(index, &value)
                    .map_err(|error| {
                        RuntimeError::new(
                            error.code,
                            "frame",
                            format!("failed to write scalar result: {}", error.message),
                        )
                    })?;
            } else if staged_slot_aggregate_values.has_plan(index) {
                staged_slot_aggregate_values.set(
                    index,
                    &value,
                    &self.heap,
                    self.revision.program.decoded.types(),
                )?;
            } else {
                let bytes = self.heap.logical_allocation_bytes(&value)?;
                if let Some(handle) = staged_slot_handles.get(index).and_then(|handle| *handle) {
                    replacements.push((handle, value, bytes));
                } else {
                    if index >= staged_slot_handles.len() {
                        return Err(RuntimeError::new(
                            "invalid_frame",
                            "frame",
                            "result slot storage is not prepared",
                        ));
                    }
                    allocations.push((value, bytes));
                    allocation_targets.push(output);
                }
            }
        }

        if !replacements.is_empty() {
            let replacement_sizes = replacements
                .iter()
                .map(|(handle, _, bytes)| (*handle, *bytes))
                .collect::<Vec<_>>();
            if !self.heap.replacements_fit(&replacement_sizes)? {
                return Err(RuntimeError::new(
                    "allocation_limit",
                    "heap",
                    "managed result replacement exceeds allocation limit",
                ));
            }
        }

        let batch = self.heap.prepare_allocations(allocations)?;
        for (output, handle) in allocation_targets
            .into_iter()
            .zip(batch.handles().iter().copied())
        {
            if output & crate::program::LOCAL_OUTPUT != 0 {
                let index = (output & !crate::program::LOCAL_OUTPUT) as usize;
                local_updates.push((index, frame::Local::Private(handle)));
            } else {
                staged_slot_handles[output as usize] = Some(handle);
            }
        }

        // Existing destinations use the fixed-frame rewrite transaction. It
        // encodes and validates every replacement before changing any old
        // payload, so a later codec or storage failure cannot partially publish
        // an earlier result.
        let prepared_replacements = self.heap.prepare_replacements(replacements)?;
        prepared_replacements.commit()?;

        self.scalar_values = staged_scalar_values;
        self.aggregate_values = staged_aggregate_values;
        self.slot_scalar_values = staged_slot_scalar_values;
        self.slot_aggregate_values = staged_slot_aggregate_values;
        self.slot_handles = staged_slot_handles;
        for (index, local) in local_updates {
            self.locals[index] = local;
        }
        self.slot_output = end;
        batch.commit();
        allocation_scope.commit();
        Ok(())
    }
}

impl Instance {
    pub(super) fn coerce_result_for_frame(
        &self,
        frame: &Frame,
        value: Value,
    ) -> Result<Value, RuntimeError> {
        match frame.output_type() {
            Some(typ) => self.coerce(value, &typ),
            None => Ok(value),
        }
    }

    pub(super) fn push_result(&mut self, value: Value) -> Result<(), RuntimeError> {
        if let Some(mutation::PendingWrite::Results(values)) = &mut self.running.pending_write {
            values.push_back(value);
            return Ok(());
        }
        match self.publish_result(value.clone()) {
            Err(error) if error.code == "collection_required" => {
                // Preserve the evaluated value and retry only publication
                // after the owner has collected. This also covers lazy
                // addressable locals, whose first result allocates a cell.
                self.running.pending_write =
                    Some(mutation::PendingWrite::Results(VecDeque::from([value])));
                self.running.write_collection = mutation::WriteCollection {
                    request: Some(mutation::CollectionRequest {
                        clear_cache: true,
                        major: !self.allocation_failure_prefers_minor()?,
                    }),
                    ..Default::default()
                };
                Ok(())
            }
            outcome => outcome,
        }
    }

    pub(super) fn publish_result(&mut self, value: Value) -> Result<(), RuntimeError> {
        let value = {
            let frame = self.running.frames.last().ok_or_else(|| {
                RuntimeError::new("invalid_frame", "frame", "result has no destination frame")
            })?;
            self.coerce_result_for_frame(frame, value)?
        };
        let uninitialized_local = {
            let frame = self.running.frames.last().unwrap();
            if frame.continuation.is_some() {
                None
            } else {
                let output =
                    frame.prepared.operands.outputs_at(frame.slot_pc.unwrap())[frame.slot_output];
                (output & crate::program::LOCAL_OUTPUT != 0)
                    .then_some((output & !crate::program::LOCAL_OUTPUT) as usize)
                    .filter(|index| matches!(frame.locals[*index], frame::Local::Uninitialized))
            }
        };
        if let Some(index) = uninitialized_local {
            self.store_local(index, value, false)?;
            self.running.frames.last_mut().unwrap().slot_output += 1;
            return Ok(());
        }
        self.running
            .frames
            .last_mut()
            .ok_or_else(|| {
                RuntimeError::new("invalid_frame", "frame", "result has no destination frame")
            })?
            .push_result(&value)
    }

    pub(super) fn extend_results(
        &mut self,
        values: impl IntoIterator<Item = Value>,
    ) -> Result<(), RuntimeError> {
        for value in values {
            self.push_result(value)?;
        }
        Ok(())
    }

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
            .find_map(|(position, _)| {
                (!frame.input_initialized_at(pc, position)).then_some(position)
            })
        else {
            return Ok(());
        };
        let function = frame.prepared.clone();
        let operands = &function.operands;
        let revision = frame.revision.clone();
        for input in operands.inputs_at(pc)[first_missing..].iter() {
            if input.kind == 2 {
                let value = self
                    .running
                    .frames
                    .last()
                    .unwrap()
                    .local_value(input.index as usize)?;
                if matches!(value.data, Data::Uninitialized) {
                    self.load_local(input.index as usize)?;
                }
                continue;
            }
            if input.kind == 0 {
                let frame = self.running.frames.last().unwrap();
                let index = input.index as usize;
                // Preserve decoder failures instead of reporting a live,
                // initialized aggregate as an absent operand.
                frame.slot_aggregate_values.get(
                    index,
                    &frame.prepared.slot_types[index],
                    &self.heap,
                    &self.types,
                )?;
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
            let frame = self.running.frames.last_mut().unwrap();
            let old_capacity = frame.slot_constants.capacity();
            let new_length = frame.slot_constants.len().saturating_add(1);
            let new_capacity = old_capacity
                .max(new_length)
                .max(1)
                .checked_next_power_of_two()
                .ok_or_else(|| {
                    RuntimeError::new(
                        "allocation_limit",
                        "frame",
                        "constant cache capacity overflows",
                    )
                })?;
            frame.ensure_resident_growth(
                old_capacity,
                new_capacity,
                size_of::<(usize, Value)>(),
            )?;
            frame.slot_constants.push((index, value));
        }
        Ok(())
    }
}
