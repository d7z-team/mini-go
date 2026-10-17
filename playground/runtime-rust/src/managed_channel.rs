//! Typed channel payload storage backed by one contiguous managed page object.
//!
//! The ring has one `StorageRef` for the complete allocation.  Every element
//! occupies the same prepared layout stride and is addressed by
//! `head * stride`; the channel never keeps a host-side ticket or `Value`
//! queue.  Reference offsets in the prepared layout are repeated for every
//! element by `StorageHeap`'s collector, so a ring remains a normal managed
//! root even when an element contains guest references.

use crate::managed_storage::{StorageLayout, StorageLayoutLease, StorageRef, StorageWorkerPool};
use crate::{error::RuntimeError, heap::PreparedPayload};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SendResult {
    /// The ticket is now owned by the channel ring.
    Sent,
    /// The channel remains unchanged and the caller retains ownership of the
    /// ticket.  A blocked operation can keep it as an exact root.
    Full,
}

#[derive(Debug)]
pub struct ManagedChannel {
    layout: u32,
    _layout_lease: StorageLayoutLease,
    reference_offsets: Box<[u32]>,
    stride: u32,
    logical_bytes: u64,
    capacity: usize,
    ring: StorageRef,
    head: usize,
    len: usize,
    closed: bool,
}

fn layout_reference_offsets(layout: &StorageLayout) -> Result<Box<[u32]>, RuntimeError> {
    let mut offsets = Vec::with_capacity(layout.references.len() + layout.dynamics.len());
    offsets.extend(layout.references.iter().copied());
    for dynamic in &layout.dynamics {
        offsets.push(dynamic.checked_add(8).ok_or_else(|| {
            RuntimeError::new(
                "typed_payload_invalid",
                "channel",
                "channel dynamic lane offset overflows",
            )
        })?);
    }
    Ok(offsets.into_boxed_slice())
}

impl ManagedChannel {
    // The serialized channel descriptor is intentionally flat so it can be
    // restored without a temporary owner object; its nine fields are the
    // complete channel state needed by the managed resource record.
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn from_parts(
        pool: &StorageWorkerPool,
        layout: u32,
        stride: u32,
        logical_bytes: u64,
        capacity: usize,
        ring: StorageRef,
        head: usize,
        len: usize,
        closed: bool,
    ) -> Result<Self, RuntimeError> {
        let layout_spec = pool.layout(layout)?;
        let expected_stride = layout_spec.stride;
        let reference_offsets = layout_reference_offsets(&layout_spec)?;
        if stride == 0 || stride != expected_stride {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "channel",
                "channel stride does not match its layout",
            ));
        }
        if capacity == 0 {
            if ring != StorageRef::NULL || head != 0 || len != 0 {
                return Err(RuntimeError::new(
                    "typed_payload_invalid",
                    "channel",
                    "rendezvous channel has buffered state",
                ));
            }
        } else if ring == StorageRef::NULL || head >= capacity || len > capacity {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "channel",
                "channel ring state is invalid",
            ));
        }
        Ok(Self {
            layout,
            _layout_lease: pool.pin_layout(layout)?,
            reference_offsets,
            stride,
            logical_bytes,
            capacity,
            ring,
            head,
            len,
            closed,
        })
    }

    /// Register the fixed message layout and allocate one contiguous ring.
    /// Capacity zero is valid and represents a rendezvous channel; it has no
    /// managed payload and transfers the sender's encoded value at the owner
    /// boundary.
    pub fn new(
        pool: &StorageWorkerPool,
        capacity: usize,
        layout: StorageLayout,
        logical_bytes: u64,
    ) -> Result<Self, RuntimeError> {
        let reference_offsets = layout_reference_offsets(&layout)?;
        let layout = pool.register_layout(layout)?;
        let layout_lease = pool.pin_layout(layout)?;
        let stride = pool.layout(layout)?.stride;
        let ring = if capacity == 0 {
            StorageRef::NULL
        } else {
            let count = u32::try_from(capacity).map_err(|_| {
                RuntimeError::new("value_limit", "channel", "channel capacity overflows")
            })?;
            // A non-empty ring is its own managed record: account for its
            // record header once, followed by the contiguous typed slots.
            // Rendezvous channels have no ring allocation at all.
            let logical = crate::contract_generated::STORAGE_RECORD_HEADER
                .checked_add(logical_bytes.checked_mul(count as u64).ok_or_else(|| {
                    RuntimeError::new("allocation_limit", "channel", "ring size overflow")
                })?)
                .ok_or_else(|| {
                    RuntimeError::new("allocation_limit", "channel", "ring size overflow")
                })?;
            let mut heap = pool.lock()?;
            let reservation = heap.reserve(layout, count, logical)?;
            reservation.commit()
        };
        Ok(Self {
            layout,
            _layout_lease: layout_lease,
            reference_offsets,
            stride,
            logical_bytes,
            capacity,
            ring,
            head: 0,
            len: 0,
            closed: false,
        })
    }

    pub fn layout(&self) -> u32 {
        self.layout
    }

    pub fn capacity(&self) -> usize {
        self.capacity
    }

    pub fn stride(&self) -> u32 {
        self.stride
    }

    pub fn logical_bytes(&self) -> u64 {
        self.logical_bytes
    }

    /// The complete ring allocation. The owning VM must publish this as an
    /// exact managed root while the channel resource is live.
    pub fn root(&self) -> StorageRef {
        self.ring
    }

    pub fn len(&self) -> usize {
        self.len
    }

    pub fn head(&self) -> usize {
        self.head
    }

    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    pub fn is_closed(&self) -> bool {
        self.closed
    }

    pub fn ready_to_send(&self) -> bool {
        !self.closed && self.len < self.capacity
    }

    pub fn ready_to_receive(&self) -> bool {
        self.len != 0 || self.closed
    }

    /// Write and enqueue one prepared element. Full leaves the caller's input
    /// untouched, which lets a blocked operation retain its normal VM root.
    #[cfg(test)]
    pub fn try_send(
        &mut self,
        pool: &StorageWorkerPool,
        payload: &[u8],
    ) -> Result<SendResult, RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new("panic", "send", "send on closed channel"));
        }
        if self.len == self.capacity {
            return Ok(SendResult::Full);
        }
        if payload.len() > self.stride as usize {
            return Err(RuntimeError::new(
                "allocation_limit",
                "channel",
                "message exceeds prepared channel layout",
            ));
        }
        let index = (self.head + self.len) % self.capacity;
        let offset = (index as u32).checked_mul(self.stride).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "channel", "ring offset overflows")
        })?;
        let mut bytes = vec![0; self.stride as usize];
        bytes[..payload.len()].copy_from_slice(payload);
        pool.lock()?.write(self.ring, offset, &bytes)?;
        self.len += 1;
        Ok(SendResult::Sent)
    }

    /// Write one prepared payload, including its managed reference lanes, into
    /// the contiguous ring slot. The channel retains bytes and numeric refs;
    /// no source `Value` or host graph crosses the owner boundary.
    pub(crate) fn try_send_prepared(
        &mut self,
        pool: &StorageWorkerPool,
        payload: &PreparedPayload,
    ) -> Result<SendResult, RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new("panic", "send", "send on closed channel"));
        }
        if self.len == self.capacity {
            return Ok(SendResult::Full);
        }
        let index = (self.head + self.len) % self.capacity;
        let offset = (index as u32).checked_mul(self.stride).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "channel", "ring offset overflows")
        })?;
        if payload.bytes.len() > self.stride as usize
            || payload
                .references
                .iter()
                .any(|(reference_offset, reference)| {
                    !self.reference_offsets.contains(reference_offset)
                        || (*reference_offset as usize).saturating_add(8) > self.stride as usize
                        || *reference == StorageRef::NULL
                })
            || payload
                .references
                .iter()
                .enumerate()
                .any(|(index, (offset, _))| {
                    payload.references[..index]
                        .iter()
                        .any(|(previous, _)| previous == offset)
                })
        {
            return Err(RuntimeError::new(
                "allocation_limit",
                "channel",
                "message exceeds prepared channel layout",
            ));
        }
        // The slot is cleared by `consume` before it can be reused, so write
        // the prepared bytes directly into the ring. Patching reference lanes
        // through the same locked heap avoids a temporary message-sized Vec
        // on every send while preserving the write barrier for each lane.
        let mut storage = pool.lock()?;
        // Validate every child before touching the destination slot. A stale
        // ticket must leave an empty slot zeroed; otherwise the collector
        // would scan bytes from a failed send even though `len` did not grow.
        for (_, reference) in &payload.references {
            storage.get(*reference)?;
        }
        storage.write(self.ring, offset, &payload.bytes)?;
        for (reference_offset, reference) in &payload.references {
            storage.write_reference(self.ring, offset + *reference_offset, *reference)?;
        }
        self.len += 1;
        Ok(SendResult::Sent)
    }

    /// Copy the next payload without changing ring ownership. Consumers must
    /// decode and validate this copy before calling `consume`; this keeps a
    /// failed decode atomic and leaves the message available for recovery.
    pub fn peek(&self, pool: &StorageWorkerPool) -> Result<Option<Vec<u8>>, RuntimeError> {
        self.peek_at(pool, 0)
    }

    /// Copy a queued payload at a logical FIFO offset without changing the
    /// channel. Snapshot readers use this to inspect every queued message
    /// while preserving head/len and ownership.
    pub fn peek_at(
        &self,
        pool: &StorageWorkerPool,
        logical_index: usize,
    ) -> Result<Option<Vec<u8>>, RuntimeError> {
        if logical_index >= self.len {
            return Ok(None);
        }
        let slot = (self.head + logical_index) % self.capacity;
        let offset = (slot as u32).checked_mul(self.stride).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "channel", "ring offset overflows")
        })?;
        let storage = pool.read_lock()?;
        let message = storage.read(self.ring, offset, self.stride)?;
        Ok(Some(message))
    }

    /// Commit consumption of the element previously returned by `peek`.
    pub fn consume(&mut self, pool: &StorageWorkerPool) -> Result<Option<Vec<u8>>, RuntimeError> {
        let Some(message) = self.peek(pool)? else {
            return Ok(None);
        };
        let offset = (self.head as u32).checked_mul(self.stride).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "channel", "ring offset overflows")
        })?;
        let mut storage = pool.lock()?;
        storage.clear(self.ring, offset, self.stride)?;
        self.head = (self.head + 1) % self.capacity;
        self.len -= 1;
        Ok(Some(message))
    }

    /// Consume the next validated fixed-width payload. New receive paths use
    /// `peek` + `consume` when decoding can fail and must remain atomic.
    pub fn receive(&mut self, pool: &StorageWorkerPool) -> Result<Option<Vec<u8>>, RuntimeError> {
        self.consume(pool)
    }

    /// Release all queued messages while preserving the channel's open/closed
    /// state. This is used by clear and failure cleanup paths.
    pub fn clear(&mut self, pool: &StorageWorkerPool) -> Result<(), RuntimeError> {
        if self.ring != StorageRef::NULL {
            let length = self
                .stride
                .checked_mul(u32::try_from(self.capacity).map_err(|_| {
                    RuntimeError::new("allocation_limit", "channel", "ring size overflows")
                })?)
                .ok_or_else(|| {
                    RuntimeError::new("allocation_limit", "channel", "ring size overflows")
                })?;
            let mut storage = pool.lock()?;
            storage.clear(self.ring, 0, length)?;
        }
        self.head = 0;
        self.len = 0;
        Ok(())
    }

    /// Release the contiguous ring when the owning resource is retired. The
    /// VM calls this only after no task or select operation can reference the
    /// channel resource.
    pub fn destroy(&mut self, pool: &StorageWorkerPool) -> Result<(), RuntimeError> {
        let clear_error = self.clear(pool).err();
        let release_error = if self.ring != StorageRef::NULL {
            let ring = self.ring;
            match pool.lock()?.release(ring) {
                Ok(()) => {
                    self.ring = StorageRef::NULL;
                    None
                }
                Err(error) => Some(error),
            }
        } else {
            None
        };
        clear_error.or(release_error).map_or(Ok(()), Err)
    }

    pub fn close(&mut self, pool: &StorageWorkerPool) -> Result<(), RuntimeError> {
        if self.closed {
            return Err(RuntimeError::new(
                "panic",
                "close",
                "close of closed channel",
            ));
        }
        self.closed = true;
        // Buffered messages remain receiveable after close. The owner calls
        // `clear` when the channel itself is discarded; close only changes
        // the send state and wakes blocked receivers.
        let _ = pool;
        Ok(())
    }

    /// The owner passes this one exact ring root to managed-heap collection.
    /// Empty slots contain zeroed bytes and therefore contribute no edges.
    pub fn roots(&self) -> impl Iterator<Item = StorageRef> {
        (self.ring != StorageRef::NULL)
            .then_some(self.ring)
            .into_iter()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::managed_storage::StorageHeap;

    fn channel(
        max_guest_bytes: Option<u64>,
        capacity: usize,
    ) -> (StorageWorkerPool, ManagedChannel) {
        let heap = StorageHeap::new(max_guest_bytes, None, 64 * 1024);
        let pool = heap.into_worker_pool();
        let channel = ManagedChannel::new(
            &pool,
            capacity,
            StorageLayout {
                stride: 16,
                references: vec![],
                dynamics: vec![],
            },
            16,
        )
        .unwrap();
        (pool, channel)
    }

    #[test]
    fn ring_cost_includes_one_record_header_and_rendezvous_has_no_ring() {
        let (pool, buffered) = channel(None, 2);
        assert_ne!(buffered.root(), StorageRef::NULL);
        assert_eq!(pool.stats().unwrap().occupied_bytes, 16 + 2 * 16);

        let (pool, rendezvous) = channel(None, 0);
        assert_eq!(rendezvous.root(), StorageRef::NULL);
        assert_eq!(pool.stats().unwrap().occupied_bytes, 0);
    }

    #[test]
    fn shared_channel_cost_vectors_cover_ring_and_rendezvous_allocations() {
        let vectors: Vec<serde_json::Value> =
            serde_json::from_str(include_str!("../../../testdata/runtime/storage_cost.json"))
                .unwrap();
        let vector = |name: &str| {
            vectors
                .iter()
                .find(|vector| vector["name"] == name)
                .unwrap()
        };
        let (pool, buffered) = {
            let heap = StorageHeap::new(None, None, 64 * 1024);
            let pool = heap.into_worker_pool();
            let channel = ManagedChannel::new(
                &pool,
                2,
                StorageLayout {
                    stride: 8,
                    references: vec![],
                    dynamics: vec![],
                },
                8,
            )
            .unwrap();
            (pool, channel)
        };
        assert_ne!(buffered.root(), StorageRef::NULL);
        assert_eq!(
            pool.stats().unwrap().occupied_bytes,
            vector("buffered_channel")["bytes"].as_u64().unwrap()
                - (crate::contract_generated::STORAGE_RECORD_HEADER
                    + crate::contract_generated::STORAGE_CHANNEL_HEADER)
        );

        let (pool, rendezvous) = channel(None, 0);
        assert_eq!(rendezvous.root(), StorageRef::NULL);
        assert_eq!(
            vector("rendezvous_channel")["bytes"].as_u64().unwrap(),
            crate::contract_generated::STORAGE_RECORD_HEADER
                + crate::contract_generated::STORAGE_CHANNEL_HEADER
        );
        assert_eq!(pool.stats().unwrap().occupied_bytes, 0);
    }

    #[test]
    fn ring_preserves_typed_payload_and_wraps() {
        let (pool, mut channel) = channel(None, 2);
        for payload in [[1u8; 16], [2u8; 16]] {
            assert_eq!(channel.try_send(&pool, &payload).unwrap(), SendResult::Sent);
        }
        assert_eq!(channel.receive(&pool).unwrap().unwrap(), vec![1; 16]);
        assert_eq!(channel.try_send(&pool, &[3; 16]).unwrap(), SendResult::Sent);
        assert_eq!(channel.receive(&pool).unwrap().unwrap(), vec![2; 16]);
        assert_eq!(channel.receive(&pool).unwrap().unwrap(), vec![3; 16]);
        assert!(channel.is_empty());
    }

    #[test]
    fn full_keeps_message_ticket_for_a_blocked_sender() {
        let (pool, mut channel) = channel(None, 1);
        assert_eq!(channel.try_send(&pool, &[1; 16]).unwrap(), SendResult::Sent);
        assert_eq!(channel.try_send(&pool, &[2; 16]).unwrap(), SendResult::Full);
        assert_eq!(channel.receive(&pool).unwrap().unwrap(), vec![1; 16]);
        assert_eq!(channel.try_send(&pool, &[2; 16]).unwrap(), SendResult::Sent);
        assert_eq!(channel.receive(&pool).unwrap().unwrap(), vec![2; 16]);
    }

    #[test]
    fn close_releases_queued_payloads_and_rejects_new_messages() {
        let (pool, mut channel) = channel(None, 2);
        assert_eq!(channel.try_send(&pool, &[1; 16]).unwrap(), SendResult::Sent);
        channel.close(&pool).unwrap();
        assert!(channel.is_closed());
        assert!(channel.ready_to_receive());
        let queued = channel.receive(&pool).unwrap();
        assert_eq!(queued.unwrap(), vec![1; 16]);
        assert!(channel.is_empty());
        let error = channel.try_send(&pool, &[2; 16]).unwrap_err();
        assert_eq!(error.code, "panic");
        channel.destroy(&pool).unwrap();
        assert_eq!(pool.stats().unwrap().occupied_bytes, 0);
    }

    #[test]
    fn failed_reservation_does_not_publish_a_message() {
        let heap = StorageHeap::new(Some(15), None, 64 * 1024);
        let pool = heap.into_worker_pool();
        let error = ManagedChannel::new(
            &pool,
            1,
            StorageLayout {
                stride: 16,
                references: vec![],
                dynamics: vec![],
            },
            16,
        )
        .unwrap_err();
        assert_eq!(error.code, "collection_required");
        assert_eq!(pool.stats().unwrap().allocations, 0);
    }

    #[test]
    fn ring_reference_slots_keep_old_to_young_payload_alive() {
        let heap = StorageHeap::new(None, None, 64 * 1024);
        let pool = heap.into_worker_pool();
        let child_layout = pool
            .register_layout(StorageLayout {
                stride: 8,
                references: vec![],
                dynamics: vec![],
            })
            .unwrap();
        let ring_layout = StorageLayout {
            stride: 16,
            references: vec![0],
            dynamics: vec![],
        };
        let mut channel = ManagedChannel::new(&pool, 2, ring_layout, 16).unwrap();
        pool.collect_minor(channel.roots()).unwrap();
        pool.collect_minor(channel.roots()).unwrap();
        let child = {
            let mut storage = pool.lock().unwrap();
            let mut reservation = storage.reserve(child_layout, 1, 8).unwrap();
            reservation.bytes().unwrap()[..8].copy_from_slice(&42u64.to_le_bytes());
            reservation.commit()
        };
        let mut payload = [0u8; 16];
        payload[..4].copy_from_slice(&child.slot.to_le_bytes());
        payload[4..8].copy_from_slice(&child.generation.to_le_bytes());
        channel.try_send(&pool, &payload).unwrap();
        pool.collect_minor(channel.roots()).unwrap();
        assert_eq!(
            pool.lock().unwrap().read(child, 0, 8).unwrap(),
            42u64.to_le_bytes()
        );

        assert_eq!(channel.receive(&pool).unwrap().unwrap(), payload);
        // Keeping an empty channel alive must not retain consumed messages.
        pool.collect_major(channel.roots()).unwrap();
        assert!(pool.lock().unwrap().read(child, 0, 8).is_err());
        assert_eq!(channel.len(), 0);
        channel.destroy(&pool).unwrap();
    }

    #[test]
    fn descriptor_ring_copies_reference_lane_without_extra_message_box() {
        let heap = StorageHeap::new(None, None, 64 * 1024);
        let pool = heap.into_worker_pool();
        let mut channel = ManagedChannel::new(
            &pool,
            1,
            StorageLayout {
                stride: 24,
                references: vec![],
                dynamics: vec![0],
            },
            24,
        )
        .unwrap();
        let child_layout = pool
            .register_layout(StorageLayout {
                stride: 8,
                references: vec![],
                dynamics: vec![],
            })
            .unwrap();
        let _child_layout_lease = pool.pin_layout(child_layout).unwrap();
        // Age the ring before publishing its first dynamic child. This
        // exercises the old-ring -> young-descriptor edge used by minor GC.
        pool.collect_major(channel.roots()).unwrap();
        pool.collect_major(channel.roots()).unwrap();
        let child = {
            let mut storage = pool.lock().unwrap();
            let reservation = storage.reserve(child_layout, 1, 8).unwrap();
            reservation.commit()
        };
        let mut bytes = vec![0; 24];
        bytes[4..8].copy_from_slice(&crate::managed_storage::VALUE_BOX.to_le_bytes());
        let payload = PreparedPayload {
            bytes,
            references: vec![(8, child)],
        };
        channel.try_send_prepared(&pool, &payload).unwrap();
        assert_eq!(pool.stats().unwrap().allocations, 2);
        pool.collect_minor(channel.roots()).unwrap();
        assert_eq!(pool.lock().unwrap().read(child, 0, 8).unwrap(), [0; 8]);
        let mut expected = payload.bytes.clone();
        expected[8..12].copy_from_slice(&child.slot.to_le_bytes());
        expected[12..16].copy_from_slice(&child.generation.to_le_bytes());
        assert_eq!(channel.peek(&pool).unwrap().unwrap(), expected);
        channel.consume(&pool).unwrap();
        pool.collect_major(channel.roots()).unwrap();
        assert!(pool.size(child).is_err());
        channel.destroy(&pool).unwrap();
    }

    #[test]
    fn failed_descriptor_send_leaves_ring_slot_empty() {
        let heap = StorageHeap::new(None, None, 64 * 1024);
        let pool = heap.into_worker_pool();
        let mut channel = ManagedChannel::new(
            &pool,
            1,
            StorageLayout {
                stride: 24,
                references: vec![],
                dynamics: vec![0],
            },
            24,
        )
        .unwrap();
        let child_layout = pool
            .register_layout(StorageLayout {
                stride: 8,
                references: vec![],
                dynamics: vec![],
            })
            .unwrap();
        let child = {
            let mut storage = pool.lock().unwrap();
            storage.reserve(child_layout, 1, 8).unwrap().commit()
        };
        pool.lock().unwrap().release(child).unwrap();
        let mut bytes = vec![0; 24];
        bytes[4..8].copy_from_slice(&crate::managed_storage::VALUE_BOX.to_le_bytes());
        let error = channel
            .try_send_prepared(
                &pool,
                &PreparedPayload {
                    bytes,
                    references: vec![(8, child)],
                },
            )
            .unwrap_err();
        assert_eq!(error.code, "stale_reference");
        assert_eq!(channel.len(), 0);
        assert!(channel.peek(&pool).unwrap().is_none());
        pool.collect_major(channel.roots()).unwrap();
        channel.destroy(&pool).unwrap();
    }

    #[test]
    fn invalid_descriptor_lane_leaves_ring_slot_empty() {
        let heap = StorageHeap::new(None, None, 64 * 1024);
        let pool = heap.into_worker_pool();
        let mut channel = ManagedChannel::new(
            &pool,
            1,
            StorageLayout {
                stride: 24,
                references: vec![],
                dynamics: vec![0],
            },
            24,
        )
        .unwrap();
        let child_layout = pool
            .register_layout(StorageLayout {
                stride: 8,
                references: vec![],
                dynamics: vec![],
            })
            .unwrap();
        let child = {
            let mut storage = pool.lock().unwrap();
            storage.reserve(child_layout, 1, 8).unwrap().commit()
        };
        let error = channel
            .try_send_prepared(
                &pool,
                &PreparedPayload {
                    bytes: vec![0; 24],
                    references: vec![(16, child)],
                },
            )
            .unwrap_err();
        assert_eq!(error.code, "allocation_limit");
        assert_eq!(channel.len(), 0);
        assert!(channel.peek(&pool).unwrap().is_none());
        channel.destroy(&pool).unwrap();
    }
}
