//! Owner roots, frame buffer capacities and managed allocation admission.

use super::*;

/// Temporary operand roots retain the values that are still owned by a frame
/// while a fallible owner transaction is resumed. The managed heap remains
/// authoritative for payload bytes and allocation accounting; this buffer is
/// a short-lived root list.
#[derive(Default)]
pub(super) struct OperandRoots {
    handles: Vec<Handle>,
}

impl OperandRoots {
    pub fn clear(&mut self) {
        self.handles.clear();
    }
    pub fn storage_bytes(&self) -> usize {
        self.handles.capacity() * size_of::<Handle>()
    }
    pub fn capture(&mut self, values: &[Value]) {
        self.clear();
        for value in values {
            value.trace(&mut |handle| self.handles.push(handle));
        }
    }

    pub(super) fn capture_handle(&mut self, handle: Handle) {
        if handle.valid() {
            self.handles.push(handle);
        }
    }

    pub(super) fn capture_bank_slot(
        &mut self,
        bank: &crate::instance::frame::AggregateBanks,
        slot: usize,
    ) {
        bank.trace_slot(slot, &mut |handle| self.capture_handle(handle));
    }
}

impl Trace for OperandRoots {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        for handle in &self.handles {
            visit(*handle);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn operand_roots_trace_prepared_lane_edges_without_decoding_value() {
        let types = TypeRegistry::new([]).unwrap();
        let heap = Heap::new(2, 4096).unwrap();
        heap.set_type_registry(types.clone());
        let string = TypeIdentity::Primitive(crate::contract_generated::PrimitiveString);
        let mut bank = crate::instance::frame::AggregateBanks::for_types(
            std::slice::from_ref(&string),
            &types,
        );
        bank.set(0, &Value::string("prepared root"), &heap, &types)
            .unwrap();

        let mut roots = OperandRoots::default();
        roots.capture_bank_slot(&bank, 0);
        let mut handles = Vec::new();
        roots.trace(&mut |handle| handles.push(handle));

        // A prepared string owns its immutable backing through the lane's
        // numeric reference. Capturing the operand root must retain that edge
        // directly; rebuilding an owning Value is both unnecessary and the
        // source of the old recursive aggregate materialization.
        assert_eq!(handles.len(), 1);
        assert!(heap.is_live(handles[0]));
    }
}

// Capacity belongs to this runtime's owner storage; logical accounting does
// not reproduce the host allocator or another backend's slice growth.
pub(super) fn grow_frame_buffer(capacity: usize, length: usize) -> Result<usize, RuntimeError> {
    if length <= capacity {
        return Ok(capacity);
    }
    let overflow = || {
        RuntimeError::new(
            "allocation_limit",
            "frame",
            "frame buffer capacity overflow",
        )
    };
    length.checked_next_power_of_two().ok_or_else(overflow)
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct MemoryStats {
    pub live_bytes: u64,
    pub total_allocated_bytes: u64,
    pub peak_bytes: u64,
}

/// Active buffer high-water marks; retained storage belongs to `FramePool`.
#[derive(Clone, Copy, Debug, Default)]
pub(super) struct FrameBufferCapacities {
    pub popped: usize,
    pub deferred: usize,
}

impl Instance {
    pub(super) fn ensure_allocated_capacity(&mut self, bytes: u64) -> Result<(), RuntimeError> {
        // Transfer private TLAB credit before admission. The shared ledger is
        // the only source of truth for the next owner; counting both an
        // active lease and its pending control reservation rejects values
        // that fit exactly at limits 159/160.
        let mut stats = self.heap.stats();
        if stats
            .guest_bytes
            .saturating_add(stats.reserved_bytes)
            .saturating_add(bytes)
            > self.limits.max_guest_bytes
        {
            self.collect_rooted()?;
            stats = self.heap.stats();
            if stats
                .guest_bytes
                .saturating_add(stats.reserved_bytes)
                .saturating_add(bytes)
                > self.limits.max_guest_bytes
            {
                return Err(RuntimeError::new(
                    "allocation_limit",
                    "guest",
                    "guest allocation byte limit exceeded",
                ));
            }
        }
        Ok(())
    }

    pub fn memory_stats(&self) -> MemoryStats {
        let heap = self.heap.stats();
        MemoryStats {
            live_bytes: heap.guest_bytes,
            total_allocated_bytes: heap.total_allocated_bytes,
            peak_bytes: heap.peak_bytes,
        }
    }

    pub(super) fn acquire_owner_lease(
        &mut self,
        bytes: u64,
    ) -> Result<crate::managed_storage::StorageControlLease, RuntimeError> {
        if bytes == 0 {
            return Err(RuntimeError::new(
                "invalid_allocation",
                "guest",
                "owner lease cannot be empty",
            ));
        }
        let reservation = match self.heap.reserve_control_capacity(bytes) {
            Ok(reservation) => reservation,
            Err(error) if error.code == "collection_required" => {
                self.collect_rooted()?;
                self.heap.reserve_control_capacity(bytes).map_err(|error| {
                    if error.code == "collection_required" {
                        RuntimeError::new(
                            "allocation_limit",
                            "guest",
                            "guest allocation byte limit exceeded",
                        )
                    } else {
                        error
                    }
                })?
            }
            Err(error) => return Err(error),
        };
        Ok(reservation.commit())
    }
}
