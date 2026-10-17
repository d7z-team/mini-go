//! Bounded physical frame storage. The guest capacity ledger is independent.

use super::*;

/// Maximum logical payload size kept in a prepared aggregate frame bank.
/// Physical lane stride remains type-layout driven; this is only the policy
/// boundary deciding whether a logical aggregate is worth retaining inline.
const INLINE_AGGREGATE_LOGICAL_BYTES: u64 = 128;

pub(super) enum Local {
    // Non-addressable locals stay lazy until their first read or write. This
    // keeps an unused large aggregate from forcing a storage layout or heap
    // allocation during frame admission.
    Uninitialized,
    // Private locals are heap-backed but cannot be addressed by guest code.
    // Keeping the distinction from Shared preserves rebind/address semantics
    // while making every guest payload visible to the same collector.
    Private(Handle),
    Shared(Handle),
    Inline(usize),
    Aggregate(usize),
}

#[derive(Clone, Debug)]
pub(super) enum InlineScalar {
    Uninitialized,
    Bool(bool),
    Integer(i64),
    Unsigned(u64),
    Float(f64),
    Complex { real: f64, imag: f64 },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum ScalarClass {
    Bool,
    Integer,
    Unsigned,
    Float,
    Complex,
}

/// Columnar storage for scalar frame slots. Each logical slot has a stable
/// class and column offset prepared at frame activation; the tagged enum is
/// materialized only at the opcode boundary. The columns are retained by the
/// frame pool and only their initialization bitmap is reset between calls.
#[derive(Clone, Debug, Default)]
pub(super) struct ScalarBanks {
    classes: Vec<Option<(ScalarClass, usize)>>,
    initialized: Vec<bool>,
    bools: Vec<bool>,
    integers: Vec<i64>,
    unsigned: Vec<u64>,
    floats: Vec<f64>,
    complex_real: Vec<f64>,
    complex_imag: Vec<f64>,
}

impl ScalarBanks {
    fn class_for_type(typ: &TypeIdentity) -> Option<ScalarClass> {
        match typ {
            TypeIdentity::Primitive(crate::contract_generated::PrimitiveBool) => {
                Some(ScalarClass::Bool)
            }
            TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveInt
                | crate::contract_generated::PrimitiveInt8
                | crate::contract_generated::PrimitiveInt16
                | crate::contract_generated::PrimitiveInt32
                | crate::contract_generated::PrimitiveInt64,
            ) => Some(ScalarClass::Integer),
            TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveUint
                | crate::contract_generated::PrimitiveUint8
                | crate::contract_generated::PrimitiveUint16
                | crate::contract_generated::PrimitiveUint32
                | crate::contract_generated::PrimitiveUint64
                | crate::contract_generated::PrimitiveUintptr,
            ) => Some(ScalarClass::Unsigned),
            TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveFloat32
                | crate::contract_generated::PrimitiveFloat64,
            ) => Some(ScalarClass::Float),
            TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveComplex64
                | crate::contract_generated::PrimitiveComplex128,
            ) => Some(ScalarClass::Complex),
            _ => None,
        }
    }

    pub(super) fn for_types(types: &[TypeIdentity]) -> Self {
        let mut classes = Vec::with_capacity(types.len());
        let mut bools = 0;
        let mut integers = 0;
        let mut unsigned = 0;
        let mut floats = 0;
        let mut complex = 0;
        for typ in types {
            let class = match Self::class_for_type(typ) {
                Some(ScalarClass::Bool) => {
                    let slot = bools;
                    bools += 1;
                    Some((ScalarClass::Bool, slot))
                }
                Some(ScalarClass::Integer) => {
                    let slot = integers;
                    integers += 1;
                    Some((ScalarClass::Integer, slot))
                }
                Some(ScalarClass::Unsigned) => {
                    let slot = unsigned;
                    unsigned += 1;
                    Some((ScalarClass::Unsigned, slot))
                }
                Some(ScalarClass::Float) => {
                    let slot = floats;
                    floats += 1;
                    Some((ScalarClass::Float, slot))
                }
                Some(ScalarClass::Complex) => {
                    let slot = complex;
                    complex += 1;
                    Some((ScalarClass::Complex, slot))
                }
                None => None,
            };
            classes.push(class);
        }
        Self {
            initialized: vec![false; types.len()],
            classes,
            bools: vec![false; bools],
            integers: vec![0; integers],
            unsigned: vec![0; unsigned],
            floats: vec![0.; floats],
            complex_real: vec![0.; complex],
            complex_imag: vec![0.; complex],
        }
    }

    /// Prepare the bank for another activation of the same prepared
    /// function.  The column layout is stable for a pooled frame, so only
    /// initialization state needs to be reset on the hot path.  A changed
    /// function layout (which can happen after a patch) rebuilds the columns
    /// before the frame becomes visible.
    pub(super) fn prepare(&mut self, types: &[TypeIdentity]) {
        let same_layout = self.classes.len() == types.len()
            && self.classes.iter().zip(types).all(|(class, typ)| {
                let expected = Self::class_for_type(typ);
                class.as_ref().map(|(actual, _)| *actual) == expected
                    || class.is_none() && expected.is_none()
            });
        if !same_layout {
            *self = Self::for_types(types);
            return;
        }
        self.initialized.fill(false);
    }

    pub(super) fn is_uninitialized(&self, index: usize) -> bool {
        !self.initialized[index]
    }

    pub(super) fn clear(&mut self, index: usize) {
        self.initialized[index] = false;
    }

    pub(super) fn get(&self, index: usize, _typ: &TypeIdentity) -> InlineScalar {
        if !self.initialized[index] {
            return InlineScalar::Uninitialized;
        }
        let Some((class, slot)) = self.classes[index] else {
            return InlineScalar::Uninitialized;
        };
        match class {
            ScalarClass::Bool => InlineScalar::Bool(self.bools[slot]),
            ScalarClass::Integer => InlineScalar::Integer(self.integers[slot]),
            ScalarClass::Unsigned => InlineScalar::Unsigned(self.unsigned[slot]),
            ScalarClass::Float => InlineScalar::Float(self.floats[slot]),
            ScalarClass::Complex => InlineScalar::Complex {
                real: self.complex_real[slot],
                imag: self.complex_imag[slot],
            },
        }
    }

    pub(super) fn set(&mut self, index: usize, value: &Value) -> Result<(), RuntimeError> {
        let scalar = InlineScalar::from_value(value).ok_or_else(|| {
            RuntimeError::new(
                "invalid_local",
                "frame",
                "inline local requires a scalar value",
            )
        })?;
        if matches!(scalar, InlineScalar::Uninitialized) {
            self.clear(index);
            return Ok(());
        }
        let Some((class, slot)) = self.classes[index] else {
            return Err(RuntimeError::new(
                "invalid_local",
                "frame",
                "local is not an inline scalar",
            ));
        };
        match (class, scalar) {
            (ScalarClass::Bool, InlineScalar::Bool(value)) => self.bools[slot] = value,
            (ScalarClass::Integer, InlineScalar::Integer(value)) => self.integers[slot] = value,
            (ScalarClass::Unsigned, InlineScalar::Unsigned(value)) => self.unsigned[slot] = value,
            (ScalarClass::Float, InlineScalar::Float(value)) => self.floats[slot] = value,
            (ScalarClass::Complex, InlineScalar::Complex { real, imag }) => {
                self.complex_real[slot] = real;
                self.complex_imag[slot] = imag;
            }
            _ => {
                return Err(RuntimeError::new(
                    "invalid_local",
                    "frame",
                    "scalar class does not match local type",
                ));
            }
        }
        self.initialized[index] = true;
        Ok(())
    }

    /// Copy a prepared scalar lane without materializing a `Value`. The
    /// caller has already checked the exact TypeIdentity; matching the
    /// prepared scalar class here keeps this primitive safe for pooled banks
    /// and makes a mismatched destination a no-op.
    pub(super) fn copy_slot_from(
        &mut self,
        destination: usize,
        source: &Self,
        source_index: usize,
    ) -> bool {
        let Some(Some((destination_class, destination_slot))) = self.classes.get(destination)
        else {
            return false;
        };
        let Some(Some((source_class, source_slot))) = source.classes.get(source_index) else {
            return false;
        };
        if destination_class != source_class
            || !source
                .initialized
                .get(source_index)
                .copied()
                .unwrap_or(false)
        {
            return false;
        }
        match destination_class {
            ScalarClass::Bool => self.bools[*destination_slot] = source.bools[*source_slot],
            ScalarClass::Integer => {
                self.integers[*destination_slot] = source.integers[*source_slot]
            }
            ScalarClass::Unsigned => {
                self.unsigned[*destination_slot] = source.unsigned[*source_slot]
            }
            ScalarClass::Float => self.floats[*destination_slot] = source.floats[*source_slot],
            ScalarClass::Complex => {
                self.complex_real[*destination_slot] = source.complex_real[*source_slot];
                self.complex_imag[*destination_slot] = source.complex_imag[*source_slot];
            }
        }
        self.initialized[destination] = true;
        true
    }

    pub(super) fn bytes(&self) -> usize {
        self.classes.len()
            + self.initialized.len()
            + self.bools.len()
            + self.integers.len() * size_of::<i64>()
            + self.unsigned.len() * size_of::<u64>()
            + self.floats.len() * size_of::<f64>()
            + (self.complex_real.len() + self.complex_imag.len()) * size_of::<f64>()
    }
}

impl InlineScalar {
    pub(super) fn from_value(value: &Value) -> Option<Self> {
        Some(match &value.data {
            Data::Uninitialized => Self::Uninitialized,
            Data::Bool(value) => Self::Bool(*value),
            Data::Integer(value) => Self::Integer(*value),
            Data::Unsigned(value) => Self::Unsigned(*value),
            Data::Float(value) => Self::Float(*value),
            Data::Complex { real, imag } => Self::Complex {
                real: *real,
                imag: *imag,
            },
            _ => return None,
        })
    }

    pub(super) fn value(&self, typ: &TypeIdentity) -> Value {
        let data = match self {
            Self::Uninitialized => Data::Uninitialized,
            Self::Bool(value) => Data::Bool(*value),
            Self::Integer(value) => Data::Integer(*value),
            Self::Unsigned(value) => Data::Unsigned(*value),
            Self::Float(value) => Data::Float(*value),
            Self::Complex { real, imag } => Data::Complex {
                real: *real,
                imag: *imag,
            },
        };
        Value {
            typ: typ.clone(),
            data,
        }
    }
}

#[derive(Clone, Debug)]
struct AggregateSlotPlan {
    typ: TypeIdentity,
    layout: Arc<crate::types::StorageLaneLayout>,
    offset: usize,
}

/// A prepared string lane.  The lane keeps only the backing identity and the
/// visible byte range; consumers can read the few bytes needed by an opcode
/// without reconstructing a `Value` containing the complete string.
#[derive(Clone, Copy, Debug)]
pub(super) struct PreparedStringDescriptor {
    pub(super) root: crate::managed_storage::StorageRef,
    pub(super) offset: u32,
    pub(super) length: u32,
}

impl PreparedStringDescriptor {
    fn from_bytes(
        typ: &TypeIdentity,
        bytes: &[u8],
        types: &TypeRegistry,
    ) -> Result<Option<Self>, RuntimeError> {
        if types.underlying(typ)?
            != TypeIdentity::Primitive(crate::contract_generated::PrimitiveString)
            || bytes.len() < 24
        {
            return Ok(None);
        }
        let root = crate::managed_storage::StorageRef::from_payload_identity(u64::from_le_bytes(
            bytes[0..8].try_into().unwrap(),
        ));
        let offset = u32::try_from(u64::from_le_bytes(bytes[8..16].try_into().unwrap()))
            .map_err(|_| RuntimeError::new("invalid_value", "string", "string offset overflows"))?;
        let length = u32::try_from(u64::from_le_bytes(bytes[16..24].try_into().unwrap()))
            .map_err(|_| RuntimeError::new("invalid_value", "string", "string length overflows"))?;
        // A null backing is the canonical empty string representation.  A
        // non-empty null descriptor is malformed and must fall back to the
        // normal decoder so it preserves its existing error behavior.
        if !root.valid() && length != 0 {
            return Ok(None);
        }
        Ok(Some(Self {
            root,
            offset,
            length,
        }))
    }

    pub(super) fn read(
        self,
        heap: &Heap,
        index: usize,
        width: usize,
    ) -> Result<Option<Vec<u8>>, RuntimeError> {
        let length = self.length as usize;
        if index >= length {
            return Ok(None);
        }
        let width = width.min(length - index);
        if !self.root.valid() {
            return Ok(None);
        }
        let offset = self
            .offset
            .checked_add(u32::try_from(index).map_err(|_| {
                RuntimeError::new("invalid_value", "string", "string index overflows")
            })?)
            .ok_or_else(|| {
                RuntimeError::new("invalid_value", "string", "string offset overflows")
            })?;
        heap.read_storage(
            self.root,
            offset,
            u32::try_from(width).map_err(|_| {
                RuntimeError::new("invalid_value", "string", "string read width overflows")
            })?,
        )
        .map(Some)
    }
}

#[derive(Clone, Debug, Default)]
pub(super) struct AggregateBanks {
    plans: Vec<Option<AggregateSlotPlan>>,
    initialized: Vec<bool>,
    bytes: Vec<u8>,
}

impl AggregateBanks {
    pub(super) fn for_types(types: &[TypeIdentity], registry: &TypeRegistry) -> Self {
        let mut plans = Vec::with_capacity(types.len());
        let mut offset = 0usize;
        for typ in types {
            if inline_type(typ) {
                plans.push(None);
                continue;
            }
            let Some(layout) = registry.storage_lane_layout(typ).ok() else {
                plans.push(None);
                continue;
            };
            if registry
                .storage_logical_bytes(typ)
                .ok()
                .is_none_or(|bytes| bytes > INLINE_AGGREGATE_LOGICAL_BYTES)
            {
                plans.push(None);
                continue;
            }
            let width = layout.stride as usize;
            plans.push(Some(AggregateSlotPlan {
                typ: typ.clone(),
                layout,
                offset,
            }));
            offset = offset.saturating_add(width);
        }
        Self {
            plans,
            initialized: vec![false; types.len()],
            bytes: vec![0; offset],
        }
    }

    pub(super) fn prepare(&mut self, types: &[TypeIdentity], registry: &TypeRegistry) {
        let same_layout = self.plans.len() == types.len()
            && self
                .plans
                .iter()
                .zip(types)
                .all(|(plan, typ)| plan.as_ref().is_none_or(|plan| plan.typ == *typ));
        if !same_layout {
            *self = Self::for_types(types, registry);
            return;
        }
        self.initialized.fill(false);
        self.bytes.fill(0);
    }

    pub(super) fn is_uninitialized(&self, index: usize) -> bool {
        !self.initialized.get(index).copied().unwrap_or(false)
    }

    pub(super) fn has_plan(&self, index: usize) -> bool {
        self.plans.get(index).is_some_and(Option::is_some)
    }

    pub(super) fn string_descriptor(
        &self,
        index: usize,
        typ: &TypeIdentity,
        types: &TypeRegistry,
    ) -> Result<Option<PreparedStringDescriptor>, RuntimeError> {
        let Some(plan) = self.plans.get(index).and_then(Option::as_ref) else {
            return Ok(None);
        };
        if !self.initialized.get(index).copied().unwrap_or(false) {
            return Ok(None);
        }
        debug_assert_eq!(&plan.typ, typ);
        let start = plan.offset;
        let end = start + plan.layout.stride as usize;
        PreparedStringDescriptor::from_bytes(typ, &self.bytes[start..end], types)
    }

    pub(super) fn dynamic_type_ids(&self, output: &mut Vec<u32>) {
        for (index, plan) in self.plans.iter().enumerate() {
            let Some(plan) = plan else { continue };
            if !self.initialized.get(index).copied().unwrap_or(false) {
                continue;
            }
            for offset in &plan.layout.dynamics {
                let start = plan.offset.saturating_add(*offset as usize);
                if let Some(bytes) = self.bytes.get(start..start.saturating_add(4))
                    && bytes.len() == 4
                {
                    let id = u32::from_le_bytes(bytes.try_into().unwrap());
                    if id != 0 {
                        output.push(id);
                    }
                }
            }
        }
    }

    pub(super) fn clear(&mut self, index: usize) {
        let Some(plan) = self.plans.get(index).and_then(Option::as_ref) else {
            return;
        };
        let start = plan.offset;
        let end = start + plan.layout.stride as usize;
        if let Some(bytes) = self.bytes.get_mut(start..end) {
            bytes.fill(0);
        }
        if let Some(initialized) = self.initialized.get_mut(index) {
            *initialized = false;
        }
    }

    pub(super) fn set(
        &mut self,
        index: usize,
        value: &Value,
        heap: &Heap,
        types: &TypeRegistry,
    ) -> Result<(), RuntimeError> {
        let Some(plan) = self.plans.get(index).and_then(Option::as_ref) else {
            return Err(RuntimeError::new(
                "invalid_local",
                "frame",
                "aggregate slot has no prepared layout",
            ));
        };
        if value.typ != plan.typ
            && plan.typ != TypeIdentity::Any
            && !types.is_interface(&plan.typ)?
            && !types.identical(&value.typ, &plan.typ)?
        {
            return Err(RuntimeError::new(
                "invalid_local",
                "frame",
                "aggregate value type does not match slot layout",
            ));
        }
        // `Any` and interface slots are descriptor lanes. Their concrete
        // value must be encoded through the dynamic codec so functions,
        // aggregates and other non-scalar values keep the concrete type ID
        // and owned box edge. Concrete aggregate layouts keep their static
        // stride and use the ordinary lane codec, including nested dynamic
        // fields.
        let payload = if plan.typ == TypeIdentity::Any || types.is_interface(&plan.typ)? {
            heap.prepared_dynamic_payload(value)?
        } else {
            heap.prepared_payload(value)?
        };
        let width = plan.layout.stride as usize;
        if payload.bytes.len() != width {
            return Err(RuntimeError::new(
                "invalid_local",
                "frame",
                "prepared payload does not match slot layout",
            ));
        }
        for (offset, _) in &payload.references {
            let offset = usize::try_from(*offset).map_err(|_| {
                RuntimeError::new("invalid_local", "frame", "reference offset overflows")
            })?;
            let end = offset.checked_add(8).ok_or_else(|| {
                RuntimeError::new("invalid_local", "frame", "reference offset overflows")
            })?;
            if end > width {
                return Err(RuntimeError::new(
                    "invalid_local",
                    "frame",
                    "reference offset is outside lane",
                ));
            }
        }
        let start = plan.offset;
        let end = start + width;
        let destination = &mut self.bytes[start..end];
        destination.copy_from_slice(&payload.bytes);
        for (offset, reference) in payload.references {
            let offset = usize::try_from(offset).map_err(|_| {
                RuntimeError::new("invalid_local", "frame", "reference offset overflows")
            })?;
            let end = offset.checked_add(8).ok_or_else(|| {
                RuntimeError::new("invalid_local", "frame", "reference offset overflows")
            })?;
            let lane = &mut destination[offset..end];
            lane.copy_from_slice(&reference.identity_for_payload().to_le_bytes());
        }
        self.initialized[index] = true;
        Ok(())
    }

    /// Validate the static part of an aggregate write without asking the heap
    /// to encode or publish its nested references. Batch result publication
    /// uses this pass before preparing any aggregate payload, so a later type
    /// error cannot leave an earlier value's backing allocation behind.
    pub(super) fn validate_value(
        &self,
        index: usize,
        value: &Value,
        types: &TypeRegistry,
    ) -> Result<(), RuntimeError> {
        let Some(plan) = self.plans.get(index).and_then(Option::as_ref) else {
            return Err(RuntimeError::new(
                "invalid_local",
                "frame",
                "aggregate slot has no prepared layout",
            ));
        };
        if value.typ != plan.typ
            && plan.typ != TypeIdentity::Any
            && !types.is_interface(&plan.typ)?
            && !types.identical(&value.typ, &plan.typ)?
        {
            return Err(RuntimeError::new(
                "invalid_local",
                "frame",
                "aggregate value type does not match slot layout",
            ));
        }
        Ok(())
    }

    pub(super) fn get(
        &self,
        index: usize,
        typ: &TypeIdentity,
        heap: &Heap,
        _types: &TypeRegistry,
    ) -> Result<Option<Value>, RuntimeError> {
        let Some(plan) = self.plans.get(index).and_then(Option::as_ref) else {
            return Ok(None);
        };
        if !self.initialized.get(index).copied().unwrap_or(false) {
            return Ok(None);
        }
        debug_assert_eq!(&plan.typ, typ);
        let start = plan.offset;
        let end = start + plan.layout.stride as usize;
        let bytes = &self.bytes[start..end];
        heap.decode_prepared_lane(typ, &plan.layout, bytes)
            .map(Some)
    }

    /// Read one prepared field/element path while it is still in its lane.
    /// Fixed scalar leaves are decoded directly from bytes; aggregate parents
    /// therefore never become a recursive `Value` merely to select one leaf.
    pub(super) fn get_path(
        &self,
        slot: usize,
        path: &[usize],
        heap: &Heap,
        types: &TypeRegistry,
    ) -> Result<Option<Value>, RuntimeError> {
        let Some(plan) = self.plans.get(slot).and_then(Option::as_ref) else {
            return Ok(None);
        };
        if !self.initialized.get(slot).copied().unwrap_or(false) {
            return Ok(None);
        }
        let mut typ = plan.typ.clone();
        let mut offset = plan.offset as u32;
        for index in path {
            match types.storage_layout(&typ)? {
                Some(crate::types::StorageTypeLayout::Struct { fields }) => {
                    let Some((_, child)) = fields.get(*index) else {
                        return Ok(None);
                    };
                    for (_, preceding) in fields.iter().take(*index) {
                        offset = offset
                            .checked_add(types.storage_lane_layout(preceding)?.stride)
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "allocation_limit",
                                    "frame",
                                    "aggregate path offset overflows",
                                )
                            })?;
                    }
                    typ = child.clone();
                }
                Some(crate::types::StorageTypeLayout::Array { element, length })
                    if *index < length =>
                {
                    let stride = types.storage_lane_layout(&element)?.stride;
                    let delta = u32::try_from(*index)
                        .ok()
                        .and_then(|index| index.checked_mul(stride))
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "allocation_limit",
                                "frame",
                                "aggregate path offset overflows",
                            )
                        })?;
                    offset = offset.checked_add(delta).ok_or_else(|| {
                        RuntimeError::new(
                            "allocation_limit",
                            "frame",
                            "aggregate path offset overflows",
                        )
                    })?;
                    typ = element;
                }
                _ => return Ok(None),
            }
        }
        let layout = types.storage_lane_layout(&typ)?;
        let width = layout.stride as usize;
        let relative = offset.saturating_sub(plan.offset as u32) as usize;
        let start = plan.offset.saturating_add(relative);
        let end = start.saturating_add(width);
        let bytes = self.bytes.get(start..end).ok_or_else(|| {
            RuntimeError::new("invalid_local", "frame", "aggregate path exceeds lane")
        })?;
        if let Some(value) = decode_fixed_lane(&typ, bytes, types)? {
            return Ok(Some(value));
        }
        heap.decode_prepared_lane(&typ, &layout, bytes).map(Some)
    }

    pub(super) fn copy_slot_from(
        &mut self,
        destination: usize,
        source: &Self,
        source_index: usize,
    ) -> bool {
        if !self.can_copy_slot_from(destination, source, source_index) {
            return false;
        }
        let Some(destination_plan) = self.plans.get(destination).and_then(Option::as_ref) else {
            return false;
        };
        let source_plan = source.plans[source_index].as_ref().unwrap();
        let width = destination_plan.layout.stride as usize;
        let source_start = source_plan.offset;
        let destination_start = destination_plan.offset;
        let Some(source_bytes) = source.bytes.get(source_start..source_start + width) else {
            return false;
        };
        let Some(destination_bytes) = self
            .bytes
            .get_mut(destination_start..destination_start + width)
        else {
            return false;
        };
        destination_bytes.copy_from_slice(source_bytes);
        self.initialized[destination] = true;
        true
    }

    /// Copy one complete prepared lane from managed storage into a frame
    /// bank. The source is already in the declared numeric layout, so this
    /// preserves references and dynamic type IDs without rebuilding its
    /// owning `Value`.
    pub(super) fn copy_slot_from_heap(
        &mut self,
        destination: usize,
        source: Handle,
        source_type: &TypeIdentity,
        heap: &Heap,
        types: &TypeRegistry,
    ) -> Result<bool, RuntimeError> {
        let Some(destination_plan) = self.plans.get(destination).and_then(Option::as_ref) else {
            // Large or addressable aggregates intentionally remain managed;
            // the caller will use the ordinary Value publication boundary.
            return Ok(false);
        };
        let width = destination_plan.layout.stride as usize;
        let start = destination_plan.offset;
        let end = start.saturating_add(width);
        let Some(destination_bytes) = self.bytes.get_mut(start..end) else {
            return Err(RuntimeError::new(
                "invalid_local",
                "frame",
                "prepared destination exceeds aggregate bank",
            ));
        };
        // Metadata validation and payload copy share one storage read guard.
        // A rejected shape therefore leaves an already initialized lane
        // untouched, while an accepted source is copied without an owned
        // intermediate Vec.
        let plans = &self.plans;
        let accepted =
            heap.read_storage_snapshot_into_if(source, 0, destination_bytes, |metadata| {
                Self::accepts_heap_lane(plans, destination, source_type, metadata, types)
            })?;
        if accepted {
            self.initialized[destination] = true;
        }
        Ok(accepted)
    }

    pub(super) fn can_copy_slot_from_heap(
        &self,
        destination: usize,
        source: Handle,
        source_type: &TypeIdentity,
        heap: &Heap,
        types: &TypeRegistry,
    ) -> Result<bool, RuntimeError> {
        Self::accepts_heap_lane(
            &self.plans,
            destination,
            source_type,
            &heap.meta(source)?,
            types,
        )
    }

    fn accepts_heap_lane(
        plans: &[Option<AggregateSlotPlan>],
        destination: usize,
        source_type: &TypeIdentity,
        metadata: &crate::managed_storage::StorageObjectMeta,
        types: &TypeRegistry,
    ) -> Result<bool, RuntimeError> {
        let Some(destination_plan) = plans.get(destination).and_then(Option::as_ref) else {
            return Ok(false);
        };
        let Some(stored_type) = metadata.typ.as_ref() else {
            return Ok(false);
        };
        // A function record and a function lane have the same declared type,
        // but the lane contains a handle while the record contains code and
        // captures. Only complete typed lanes may be copied byte-for-byte.
        if !metadata.initialized
            || metadata.kind != crate::managed_storage::StoragePayloadKind::Typed
            || !types.identical(stored_type, source_type)?
        {
            return Ok(false);
        }
        if destination_plan.typ != TypeIdentity::Any
            && !types.is_interface(&destination_plan.typ)?
            && !types.identical(stored_type, &destination_plan.typ)?
        {
            return Ok(false);
        }
        let source_layout = types.storage_lane_layout(stored_type)?;
        let destination_layout = &destination_plan.layout;
        Ok(metadata.payload_len == destination_layout.stride
            && source_layout.kind == destination_layout.kind
            && source_layout.stride == destination_layout.stride
            && source_layout.references == destination_layout.references
            && source_layout.dynamics == destination_layout.dynamics)
    }

    pub(super) fn slice_bounds(
        &self,
        index: usize,
        typ: &TypeIdentity,
        types: &TypeRegistry,
    ) -> Result<Option<(usize, usize)>, RuntimeError> {
        let Some(plan) = self.plans.get(index).and_then(Option::as_ref) else {
            return Ok(None);
        };
        if !self.initialized.get(index).copied().unwrap_or(false)
            || !matches!(
                types.storage_layout(typ)?,
                Some(crate::types::StorageTypeLayout::Slice { .. })
            )
        {
            return Ok(None);
        }
        let start = plan.offset;
        let end = start.saturating_add(plan.layout.stride as usize);
        let Some(bytes) = self.bytes.get(start..end) else {
            return Ok(None);
        };
        if bytes.len() < 48 {
            return Ok(None);
        }
        let length = usize::try_from(u64::from_le_bytes(bytes[32..40].try_into().unwrap()))
            .map_err(|_| RuntimeError::new("invalid_value", "slice", "length overflows"))?;
        let capacity = usize::try_from(u64::from_le_bytes(bytes[40..48].try_into().unwrap()))
            .map_err(|_| RuntimeError::new("invalid_value", "slice", "capacity overflows"))?;
        Ok(Some((length, capacity)))
    }

    pub(super) fn can_copy_slot_from(
        &self,
        destination: usize,
        source: &Self,
        source_index: usize,
    ) -> bool {
        let Some(destination_plan) = self.plans.get(destination).and_then(Option::as_ref) else {
            return false;
        };
        let Some(source_plan) = source.plans.get(source_index).and_then(Option::as_ref) else {
            return false;
        };
        destination_plan.typ == source_plan.typ
            && destination_plan.layout == source_plan.layout
            && source
                .initialized
                .get(source_index)
                .copied()
                .unwrap_or(false)
    }

    pub(super) fn bytes(&self) -> usize {
        self.plans.capacity() * size_of::<Option<AggregateSlotPlan>>()
            + self.initialized.capacity() * size_of::<bool>()
            + self.bytes.capacity()
    }

    pub(super) fn trace_slot(&self, slot: usize, visit: &mut dyn FnMut(Handle)) {
        let Some(plan) = self.plans.get(slot).and_then(Option::as_ref) else {
            return;
        };
        if !self.initialized.get(slot).copied().unwrap_or(false) {
            return;
        }
        let start = plan.offset;
        let bytes = &self.bytes[start..start + plan.layout.stride as usize];
        for offset in plan.layout.references.iter().copied() {
            if let Some(reference) = decode_storage_reference(bytes, offset as usize) {
                visit(reference);
            }
        }
        for offset in plan.layout.dynamics.iter().copied() {
            let offset = offset as usize;
            let representation =
                u32::from_le_bytes(bytes[offset + 4..offset + 8].try_into().unwrap());
            if matches!(
                representation,
                crate::managed_storage::VALUE_REF | crate::managed_storage::VALUE_BOX
            ) && let Some(reference) = decode_storage_reference(bytes, offset + 8)
            {
                visit(reference);
            }
        }
    }

    pub(super) fn trace_all(&self, visit: &mut dyn FnMut(Handle)) {
        for slot in 0..self.plans.len() {
            self.trace_slot(slot, visit);
        }
    }
}

fn decode_fixed_lane(
    typ: &TypeIdentity,
    bytes: &[u8],
    types: &TypeRegistry,
) -> Result<Option<Value>, RuntimeError> {
    let underlying = types.underlying(typ)?;
    let data = match underlying {
        TypeIdentity::Primitive(crate::contract_generated::PrimitiveBool) => {
            Data::Bool(bytes.first().copied().unwrap_or_default() != 0)
        }
        TypeIdentity::Primitive(
            crate::contract_generated::PrimitiveInt
            | crate::contract_generated::PrimitiveInt8
            | crate::contract_generated::PrimitiveInt16
            | crate::contract_generated::PrimitiveInt32
            | crate::contract_generated::PrimitiveInt64,
        ) => {
            let Some(bytes) = bytes.get(..8) else {
                return Ok(None);
            };
            Data::Integer(i64::from_le_bytes(bytes.try_into().unwrap()))
        }
        TypeIdentity::Primitive(
            crate::contract_generated::PrimitiveUint
            | crate::contract_generated::PrimitiveUint8
            | crate::contract_generated::PrimitiveUint16
            | crate::contract_generated::PrimitiveUint32
            | crate::contract_generated::PrimitiveUint64
            | crate::contract_generated::PrimitiveUintptr,
        ) => {
            let Some(bytes) = bytes.get(..8) else {
                return Ok(None);
            };
            Data::Unsigned(u64::from_le_bytes(bytes.try_into().unwrap()))
        }
        TypeIdentity::Primitive(
            crate::contract_generated::PrimitiveFloat32
            | crate::contract_generated::PrimitiveFloat64,
        ) => {
            let Some(bytes) = bytes.get(..8) else {
                return Ok(None);
            };
            Data::Float(f64::from_bits(u64::from_le_bytes(
                bytes.try_into().unwrap(),
            )))
        }
        TypeIdentity::Primitive(
            crate::contract_generated::PrimitiveComplex64
            | crate::contract_generated::PrimitiveComplex128,
        ) => {
            let (Some(real), Some(imag)) = (bytes.get(..8), bytes.get(8..16)) else {
                return Ok(None);
            };
            Data::Complex {
                real: f64::from_bits(u64::from_le_bytes(real.try_into().unwrap())),
                imag: f64::from_bits(u64::from_le_bytes(imag.try_into().unwrap())),
            }
        }
        _ => return Ok(None),
    };
    Ok(Some(Value {
        typ: typ.clone(),
        data,
    }))
}

fn decode_storage_reference(bytes: &[u8], offset: usize) -> Option<Handle> {
    let bytes = bytes.get(offset..offset + 8)?;
    let identity = u64::from_le_bytes(bytes.try_into().ok()?);
    let reference = crate::managed_storage::StorageRef::from_payload_identity(identity);
    reference.valid().then_some(reference)
}

pub(super) fn inline_type(typ: &TypeIdentity) -> bool {
    matches!(
        typ,
        TypeIdentity::Primitive(
            crate::contract_generated::PrimitiveBool
                | crate::contract_generated::PrimitiveInt
                | crate::contract_generated::PrimitiveInt8
                | crate::contract_generated::PrimitiveInt16
                | crate::contract_generated::PrimitiveInt32
                | crate::contract_generated::PrimitiveInt64
                | crate::contract_generated::PrimitiveUint
                | crate::contract_generated::PrimitiveUint8
                | crate::contract_generated::PrimitiveUint16
                | crate::contract_generated::PrimitiveUint32
                | crate::contract_generated::PrimitiveUint64
                | crate::contract_generated::PrimitiveUintptr
                | crate::contract_generated::PrimitiveFloat32
                | crate::contract_generated::PrimitiveFloat64
                | crate::contract_generated::PrimitiveComplex64
                | crate::contract_generated::PrimitiveComplex128
        )
    )
}

fn can_inline_value(value: &Value, typ: &TypeIdentity) -> bool {
    inline_type(typ) && InlineScalar::from_value(value).is_some()
}

fn can_inline_aggregate(value: &Value, typ: &TypeIdentity, types: &TypeRegistry) -> bool {
    !inline_type(typ)
        && value.typ == *typ
        && types
            .storage_logical_bytes(typ)
            .ok()
            .is_some_and(|bytes| bytes <= INLINE_AGGREGATE_LOGICAL_BYTES)
        && types
            .storage_lane_layout(typ)
            .ok()
            .is_some_and(|layout| layout.stride > 0)
}

impl Local {
    pub(super) fn address(&self) -> Result<Handle, RuntimeError> {
        match self {
            Self::Shared(handle) => Ok(*handle),
            Self::Uninitialized | Self::Private(_) | Self::Inline(_) | Self::Aggregate(_) => Err(
                RuntimeError::new("invalid_address", "local", "local has no shared storage"),
            ),
        }
    }
}

impl Trace for Local {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        match self {
            Self::Private(handle) | Self::Shared(handle) => visit(*handle),
            Self::Uninitialized | Self::Inline(_) | Self::Aggregate(_) => {}
        }
    }
}

pub(super) enum Continuation {
    Intrinsic(reflect_async::IntrinsicResume),
    TailReturn(usize),
}

pub(super) enum FrameArguments {
    Owned(Vec<Value>),
    Caller {
        frame: usize,
        suspended: bool,
        start: usize,
        count: usize,
    },
}

// A suspended operation owns delivered values until its owner resumes it.
// They are independent of bytecode operands and remain GC roots on the frame.
#[derive(Default)]
pub(super) struct ResultBuffer {
    pub handles: Vec<Handle>,
    pub reading: bool,
}

pub(super) struct Frame {
    _control: crate::managed_storage::StorageControlLease,
    // Physical frame banks and root buffers are admitted through the shared
    // resident ledger for the lifetime of the active or pooled frame.
    _resident: Option<crate::managed_storage::StorageResidentLease>,
    pub(super) heap: Heap,
    pub(super) prepared: Arc<crate::program::PreparedFunction>,
    pub(super) memory: memory::FrameBufferCapacities,
    pub(super) revision: Arc<crate::program::Revision>,
    pub(super) module: Arc<str>,
    pub(super) function: Arc<str>,
    pub(super) pc: usize,
    pub(super) type_dispatch_index: usize,
    pub(super) type_dispatch_value: Option<Value>,
    pub(super) locals: Vec<Local>,
    /// Scalar locals and small aggregate lanes are stored in typed prepared
    /// frame banks. The aggregate bank retains only bytes and StorageRef edges;
    /// larger or addressable values continue to use managed cells.
    pub(super) scalar_values: ScalarBanks,
    pub(super) aggregate_values: AggregateBanks,
    pub(super) slot_scalar_values: ScalarBanks,
    pub(super) slot_aggregate_values: AggregateBanks,
    pub(super) result_scalar_values: ScalarBanks,
    pub(super) result_aggregate_values: AggregateBanks,
    pub(super) result_handles: Vec<Option<Handle>>,
    pub(super) globals: Vec<Handle>,
    pub(super) upvalues: Vec<Address>,
    // Slots without a prepared inline/aggregate lane keep only their managed
    // storage identity.  The frame must not become a second owning object
    // graph by retaining detached Values here; decoding is done on demand
    // through the heap and the handle is traced with the slot liveness map.
    pub(super) slot_handles: Vec<Option<Handle>>,
    pub(super) slot_constants: Vec<(usize, Value)>,
    pub(super) slot_pc: Option<usize>,
    pub(super) slot_input: usize,
    pub(super) slot_output: usize,
    pub(super) delivery: ResultBuffer,
    pub(super) map_iterators: Vec<Option<MapIterator>>,
    pub(super) popped_roots: memory::OperandRoots,
    pub(super) expected_results: usize,
    pub(super) initializing: bool,
    pub(super) defers: Vec<FunctionValue>,
    pub(super) returning: Option<usize>,
    /// Panic values live in the managed heap while unwinding. Keeping a
    /// detached `Arc<Value>` here made a deferred frame an untracked second
    /// object graph and let a recovery collection miss its edges.
    pub(super) panic: Option<Handle>,
    pub(super) recovered: Option<Handle>,
    pub(super) deferred: bool,
    pub(super) continuation: Option<Continuation>,
    pub(super) after_init: Option<usize>,
}

pub(super) struct MapIterator {
    pub object: Value,
    pub entries: Vec<u64>,
    pub position: usize,
    pub lease: Option<crate::managed_storage::StorageControlLease>,
}

impl Trace for Frame {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        self.trace_at(self.pc, false, visit);
    }
}

impl Frame {
    fn prepared_string_handle(
        &self,
        handle: Handle,
        typ: &TypeIdentity,
    ) -> Option<PreparedStringDescriptor> {
        if !self.heap.payload_initialized(handle).ok()? {
            return None;
        }
        if self.heap.payload_type(handle).ok()?.as_ref() != Some(typ) {
            return None;
        }
        let mut bytes = [0_u8; 24];
        self.heap.read_storage_into(handle, 0, &mut bytes).ok()?;
        PreparedStringDescriptor::from_bytes(typ, &bytes, self.revision.program.decoded.types())
            .ok()
            .flatten()
    }

    /// Return the prepared descriptor for an input string, if its storage is
    /// already in a typed lane.  Non-prepared constants and malformed lanes
    /// return `None` so the ordinary decoder retains its error semantics.
    pub(super) fn input_string_descriptor(
        &self,
        pc: usize,
        position: usize,
    ) -> Option<PreparedStringDescriptor> {
        let input = self.prepared.operands.inputs_at(pc).get(position)?;
        let types = self.revision.program.decoded.types();
        match input.kind {
            0 => {
                let index = input.index as usize;
                let typ = self.prepared.slot_types.get(index)?;
                if self.slot_aggregate_values.has_plan(index) {
                    return self
                        .slot_aggregate_values
                        .string_descriptor(index, typ, types)
                        .ok()
                        .flatten();
                }
                self.slot_handles
                    .get(index)
                    .and_then(|handle| handle.as_ref())
                    .and_then(|handle| self.prepared_string_handle(*handle, typ))
            }
            2 => {
                let index = input.index as usize;
                let typ = self.prepared.local_types.get(index)?;
                match self.locals.get(index)? {
                    frame::Local::Aggregate(slot) => self
                        .aggregate_values
                        .string_descriptor(*slot, typ, types)
                        .ok()
                        .flatten(),
                    frame::Local::Private(handle) | frame::Local::Shared(handle) => {
                        self.prepared_string_handle(*handle, typ)
                    }
                    frame::Local::Inline(_) | frame::Local::Uninitialized => None,
                }
            }
            _ => None,
        }
    }

    pub(super) fn dynamic_type_ids(&self, output: &mut Vec<u32>) {
        self.aggregate_values.dynamic_type_ids(output);
        self.slot_aggregate_values.dynamic_type_ids(output);
        self.result_aggregate_values.dynamic_type_ids(output);
    }

    pub(super) fn trace_at(
        &self,
        pc: usize,
        retain_all_slots: bool,
        visit: &mut dyn FnMut(Handle),
    ) {
        // `reference_at_pc` is an interned liveness map produced from the
        // prepared control-flow graph.  Only local, temporary and captured
        // cell slots are filtered by it.  The remaining values below belong
        // to suspended operations (selects, calls, returns and defers) and
        // remain roots for as long as their owner is present on the frame.
        // Program preparation rejects an incomplete plan. A missing entry is
        // therefore an internal invariant violation, and must not silently
        // turn into a global scan on a hot path.
        let live = |index: usize| {
            if retain_all_slots {
                return true;
            }
            // A completed return may leave pc pointing into another branch.
            // While defers and result publication finish, only named result
            // locals remain executable storage; result banks and deferred
            // captures are enumerated by their owners below.
            if self.returning.is_some() {
                return self.prepared.result_locals.contains(&index);
            }
            let Some(map_id) = self.prepared.reference_at_pc.get(pc) else {
                debug_assert!(false, "prepared function has no reference map for PC");
                return false;
            };
            let Some(map) = self.prepared.reference_maps.get(*map_id as usize) else {
                debug_assert!(false, "prepared function references an unknown root map");
                return false;
            };
            map.get(index / 64)
                .is_some_and(|word| word & (1_u64 << (index % 64)) != 0)
        };
        let local_count = self.prepared.local_types.len();
        let slot_base = local_count;

        for iterator in self.map_iterators.iter().flatten() {
            iterator.object.trace(visit);
        }
        for (index, local) in self.locals.iter().enumerate() {
            if live(index) {
                if let Local::Aggregate(slot) = local {
                    self.aggregate_values.trace_slot(*slot, visit);
                } else {
                    local.trace(visit);
                }
            }
        }
        // Globals are handles owned by the frame's module lookup and are not
        // part of the local liveness bitmap.  They are retained while the
        // frame is alive, including during a call suspension.
        for handle in &self.globals {
            visit(*handle);
        }
        for (index, handle) in self.slot_handles.iter().enumerate() {
            let Some(typ) = self.prepared.slot_types.get(index) else {
                continue;
            };
            if inline_type(typ) || self.slot_aggregate_values.has_plan(index) {
                continue;
            }
            if (retain_all_slots || live(slot_base + index))
                && let Some(handle) = handle
            {
                visit(*handle);
            }
        }
        for (index, typ) in self.prepared.slot_types.iter().enumerate() {
            if !(retain_all_slots || live(slot_base + index)) {
                continue;
            }
            if !inline_type(typ) {
                self.slot_aggregate_values.trace_slot(index, visit);
            }
        }
        for (index, address) in self.upvalues.iter().enumerate() {
            let _ = index;
            visit(address.root);
        }
        for handle in &self.delivery.handles {
            visit(*handle);
        }
        for (_, value) in &self.slot_constants {
            value.trace(visit);
        }
        if let Some(value) = &self.type_dispatch_value {
            value.trace(visit);
        }
        self.popped_roots.trace(visit);
        if let Some(Continuation::Intrinsic(resume)) = &self.continuation {
            resume.trace(visit);
        }
        for index in 0..self.prepared.result_types.len() {
            self.result_aggregate_values.trace_slot(index, visit);
        }
        for handle in self.result_handles.iter().flatten() {
            visit(*handle);
        }
        for function in &self.defers {
            if let Some(anchor) = function.revision_anchor {
                visit(anchor);
            }
            for address in &function.captures {
                visit(address.root);
            }
        }
        if let Some(handle) = self.panic {
            visit(handle);
        }
        if let Some(handle) = self.recovered {
            visit(handle);
        }
    }
}

impl Frame {
    fn resident_bytes(&self) -> usize {
        self.locals.capacity() * size_of::<Local>()
            + self.scalar_values.bytes()
            + self.aggregate_values.bytes()
            + self.slot_scalar_values.bytes()
            + self.slot_aggregate_values.bytes()
            + self.result_scalar_values.bytes()
            + self.result_aggregate_values.bytes()
            + self.result_handles.capacity() * size_of::<Option<Handle>>()
            + self.globals.capacity() * size_of::<Handle>()
            + self.upvalues.capacity() * size_of::<Address>()
            + self.slot_handles.capacity() * size_of::<Option<Handle>>()
            + self.slot_constants.capacity() * size_of::<(usize, Value)>()
            + self.delivery.handles.capacity() * size_of::<Handle>()
            + self.popped_roots.storage_bytes()
            + self.defers.capacity() * size_of::<FunctionValue>()
            + self.memory.popped * size_of::<Value>()
            + self.memory.deferred * size_of::<Value>()
            + self.map_iterators.capacity() * size_of::<Option<MapIterator>>()
    }

    /// Admit a vector capacity increase before the vector is grown. The
    /// resident lease is shared with pooled FrameStorage, so a cached frame
    /// retains exactly the capacity it contributed to the pool.
    pub(super) fn ensure_resident_growth(
        &mut self,
        old_capacity: usize,
        new_capacity: usize,
        element_size: usize,
    ) -> Result<(), RuntimeError> {
        if new_capacity <= old_capacity {
            return Ok(());
        }
        let delta = new_capacity
            .saturating_sub(old_capacity)
            .saturating_mul(element_size);
        let target = self.resident_bytes().saturating_add(delta);
        self._resident
            .as_mut()
            .ok_or_else(|| RuntimeError::new("invalid_frame", "frame", "missing resident lease"))?
            .ensure_capacity(target as u64)
    }

    pub(super) fn output_type(&self) -> Option<TypeIdentity> {
        if self.continuation.is_some() {
            return None;
        }
        let pc = self.slot_pc?;
        let output = *self
            .prepared
            .operands
            .outputs_at(pc)
            .get(self.slot_output)?;
        if output & crate::program::LOCAL_OUTPUT != 0 {
            self.prepared
                .local_types
                .get((output & !crate::program::LOCAL_OUTPUT) as usize)
                .cloned()
        } else {
            self.prepared.slot_types.get(output as usize).cloned()
        }
    }

    pub(super) fn slot_value(&self, index: usize) -> Option<Value> {
        self.read_slot_value(index).ok().flatten()
    }

    pub(super) fn read_slot_value(&self, index: usize) -> Result<Option<Value>, RuntimeError> {
        let Some(typ) = self.prepared.slot_types.get(index) else {
            return Ok(None);
        };
        if inline_type(typ) {
            let value = self.slot_scalar_values.get(index, typ).value(typ);
            return Ok((!matches!(value.data, Data::Uninitialized)).then_some(value));
        }
        if self.slot_aggregate_values.has_plan(index) {
            return self.slot_aggregate_values.get(
                index,
                typ,
                &self.heap,
                self.revision.program.decoded.types(),
            );
        }
        // Values without a prepared scalar or aggregate lane remain in the
        // managed slot storage. This covers large or dynamically shaped
        // values; prepared lanes have already returned above.
        let Some(handle) = self.slot_handles.get(index).and_then(|handle| *handle) else {
            return Ok(None);
        };
        self.heap.read_typed_value(handle).map(Some)
    }

    /// Retain the managed edges of one live input without rebuilding its
    /// owning `Value`. Prepared frame banks are already the source of truth
    /// for their reference lanes, so caller admission can root those lanes
    /// directly while a callee is being prepared.
    pub(super) fn capture_input_roots(&self, position: usize, roots: &mut memory::OperandRoots) {
        let Some(pc) = self.slot_pc else { return };
        let Some(input) = self.prepared.operands.inputs_at(pc).get(position) else {
            return;
        };
        match input.kind {
            0 => {
                let index = input.index as usize;
                if self.slot_aggregate_values.has_plan(index) {
                    roots.capture_bank_slot(&self.slot_aggregate_values, index);
                } else if let Some(handle) = self.slot_handles.get(index).and_then(|handle| *handle)
                {
                    roots.capture_handle(handle);
                }
            }
            1 => {
                let index = input.index as usize;
                if !matches!(
                    self.revision.program.constant_data.get(index),
                    Some(crate::program::PreparedConstant::Scalar(_))
                ) && self.slot_pc == Some(pc)
                    && let Some((_, value)) =
                        self.slot_constants.iter().find(|(key, _)| *key == index)
                {
                    value.trace(&mut |handle| roots.capture_handle(handle));
                }
            }
            2 => match self.locals.get(input.index as usize) {
                Some(Local::Private(handle) | Local::Shared(handle)) => {
                    roots.capture_handle(*handle);
                }
                Some(Local::Aggregate(slot)) => {
                    roots.capture_bank_slot(&self.aggregate_values, *slot);
                }
                _ => {}
            },
            _ => {}
        }
    }

    pub(super) fn put_slot_value(
        &mut self,
        index: usize,
        value: Value,
    ) -> Result<(), RuntimeError> {
        let typ =
            self.prepared.slot_types.get(index).ok_or_else(|| {
                RuntimeError::new("invalid_operand", "frame", "slot is out of range")
            })?;
        if inline_type(typ) {
            return self.slot_scalar_values.set(index, &value);
        }
        if self.slot_aggregate_values.has_plan(index) {
            return self.slot_aggregate_values.set(
                index,
                &value,
                &self.heap,
                self.revision.program.decoded.types(),
            );
        }
        let bytes = self.heap.logical_allocation_bytes(&value)?;
        if let Some(handle) = self.slot_handles.get(index).and_then(|handle| *handle) {
            self.heap
                .replace(handle, value, bytes)
                .map_err(|(error, _)| error)?;
            return Ok(());
        }
        if index >= self.slot_handles.len() {
            let old_capacity = self.slot_handles.capacity();
            let new_length = index.saturating_add(1);
            let new_capacity = old_capacity
                .max(new_length)
                .max(1)
                .checked_next_power_of_two()
                .ok_or_else(|| {
                    RuntimeError::new("allocation_limit", "frame", "slot capacity overflows")
                })?;
            self.ensure_resident_growth(old_capacity, new_capacity, size_of::<Option<Handle>>())?;
            self.slot_handles.resize_with(new_length, || None);
        }
        let handle = self
            .heap
            .allocate(value, bytes)
            .map_err(|(error, _)| error)?;
        self.slot_handles[index] = Some(handle);
        Ok(())
    }

    pub(super) fn clear_slot(&mut self, index: usize) {
        if let Some(typ) = self.prepared.slot_types.get(index) {
            if inline_type(typ) {
                self.slot_scalar_values.clear(index);
            } else if self.slot_aggregate_values.has_plan(index) {
                self.slot_aggregate_values.clear(index);
            }
        }
        if let Some(handle) = self.slot_handles.get_mut(index) {
            *handle = None;
        }
    }

    pub(super) fn local_value(&self, index: usize) -> Result<Value, RuntimeError> {
        match &self.locals[index] {
            Local::Uninitialized => Ok(Value {
                typ: self.prepared.local_types[index].clone(),
                data: Data::Uninitialized,
            }),
            Local::Inline(slot) => Ok(self
                .scalar_values
                .get(*slot, &self.prepared.local_types[index])
                .value(&self.prepared.local_types[index])),
            Local::Aggregate(slot) => Ok(self
                .aggregate_values
                .get(
                    *slot,
                    &self.prepared.local_types[index],
                    &self.heap,
                    self.revision.program.decoded.types(),
                )?
                .unwrap_or(Value {
                    typ: self.prepared.local_types[index].clone(),
                    data: Data::Uninitialized,
                })),
            Local::Private(handle) | Local::Shared(handle) => self.heap.read_typed_value(*handle),
        }
    }

    pub(super) fn result_value(&self, index: usize) -> Result<Value, RuntimeError> {
        let typ = self.prepared.result_types.get(index).ok_or_else(|| {
            RuntimeError::new("invalid_return", "frame", "result index is out of range")
        })?;
        if inline_type(typ) {
            return Ok(self.result_scalar_values.get(index, typ).value(typ));
        }
        if self.result_aggregate_values.has_plan(index) {
            return self
                .result_aggregate_values
                .get(
                    index,
                    typ,
                    &self.heap,
                    self.revision.program.decoded.types(),
                )?
                .ok_or_else(|| {
                    RuntimeError::new("invalid_return", "frame", "result lane is uninitialized")
                });
        }
        let handle = self
            .result_handles
            .get(index)
            .and_then(|handle| *handle)
            .ok_or_else(|| {
                RuntimeError::new("invalid_return", "frame", "result storage is missing")
            })?;
        self.heap.read_typed_value(handle)
    }

    /// Transfer one aggregate result into the caller's prepared destination.
    /// A false return means that the destination is a continuation, a managed
    /// local, or otherwise has a different prepared layout and must use the
    /// ordinary `Value` result boundary.
    pub(super) fn push_prepared_result_from(
        &mut self,
        source: &Frame,
        index: usize,
    ) -> Result<bool, RuntimeError> {
        if !self.can_push_prepared_result_from(source, index) {
            return Ok(false);
        }
        let output = self.prepared.operands.outputs_at(self.slot_pc.unwrap())[self.slot_output];
        let copied = if output & crate::program::LOCAL_OUTPUT != 0 {
            let destination = (output & !crate::program::LOCAL_OUTPUT) as usize;
            match self.locals.get(destination) {
                Some(Local::Aggregate(destination))
                    if source.result_aggregate_values.has_plan(index) =>
                {
                    self.aggregate_values.copy_slot_from(
                        *destination,
                        &source.result_aggregate_values,
                        index,
                    )
                }
                Some(Local::Aggregate(destination)) => {
                    let Some(handle) = source.result_handles.get(index).and_then(|handle| *handle)
                    else {
                        return Ok(false);
                    };
                    let Some(source_type) = source.prepared.result_types.get(index) else {
                        return Ok(false);
                    };
                    self.aggregate_values.copy_slot_from_heap(
                        *destination,
                        handle,
                        source_type,
                        &self.heap,
                        self.revision.program.decoded.types(),
                    )?
                }
                _ => false,
            }
        } else {
            let destination = output as usize;
            if source.result_aggregate_values.has_plan(index) {
                self.slot_aggregate_values.copy_slot_from(
                    destination,
                    &source.result_aggregate_values,
                    index,
                )
            } else {
                let Some(handle) = source.result_handles.get(index).and_then(|handle| *handle)
                else {
                    return Ok(false);
                };
                let Some(source_type) = source.prepared.result_types.get(index) else {
                    return Ok(false);
                };
                self.slot_aggregate_values.copy_slot_from_heap(
                    destination,
                    handle,
                    source_type,
                    &self.heap,
                    self.revision.program.decoded.types(),
                )?
            }
        };
        if copied {
            self.slot_output += 1;
        }
        Ok(copied)
    }

    /// Publish a small aggregate local directly into the current output slot.
    /// The private worker uses this for `load_local`; returning a Value here
    /// would decode the whole local only to encode the same prepared lane
    /// again in `push_result`.
    pub(super) fn push_prepared_local_result(
        &mut self,
        index: usize,
    ) -> Result<bool, RuntimeError> {
        let managed_source = match self.locals.get(index) {
            Some(Local::Private(handle) | Local::Shared(handle)) => Some(*handle),
            _ => None,
        };
        if let Some(handle) = managed_source {
            let source_type = self.prepared.local_types[index].clone();
            let Some(pc) = self.slot_pc else {
                return Ok(false);
            };
            let Some(output) = self
                .prepared
                .operands
                .outputs_at(pc)
                .get(self.slot_output)
                .copied()
            else {
                return Ok(false);
            };
            if output & crate::program::LOCAL_OUTPUT != 0 {
                return Ok(false);
            }
            let copied = self.slot_aggregate_values.copy_slot_from_heap(
                output as usize,
                handle,
                &source_type,
                &self.heap,
                self.revision.program.decoded.types(),
            )?;
            if copied {
                self.slot_output += 1;
            }
            return Ok(copied);
        }
        let Some(source) = self.locals.get(index).and_then(|local| match local {
            Local::Aggregate(source) => Some(*source),
            _ => None,
        }) else {
            return Ok(false);
        };
        let Some(pc) = self.slot_pc else {
            return Ok(false);
        };
        let Some(output) = self
            .prepared
            .operands
            .outputs_at(pc)
            .get(self.slot_output)
            .copied()
        else {
            return Ok(false);
        };
        // A local output aliases the same aggregate bank and needs a separate
        // within-bank copy primitive; leave that uncommon shape to the owner
        // fallback until it can be staged without overlapping borrows.
        if output & crate::program::LOCAL_OUTPUT != 0 {
            return Ok(false);
        }
        let destination = output as usize;
        if !self.slot_aggregate_values.can_copy_slot_from(
            destination,
            &self.aggregate_values,
            source,
        ) {
            return Ok(false);
        }
        if self
            .slot_aggregate_values
            .copy_slot_from(destination, &self.aggregate_values, source)
        {
            self.slot_output += 1;
            Ok(true)
        } else {
            Ok(false)
        }
    }

    pub(super) fn can_push_prepared_result_from(&self, source: &Frame, index: usize) -> bool {
        self.can_push_prepared_result_from_at(source, index, self.slot_output)
    }

    fn can_push_prepared_result_from_at(
        &self,
        source: &Frame,
        index: usize,
        output_position: usize,
    ) -> bool {
        if self.continuation.is_some() || self.slot_pc.is_none() {
            return false;
        }
        let source_type = match source.prepared.result_types.get(index) {
            Some(source_type) => source_type,
            None => return false,
        };
        let Some(output) = self
            .prepared
            .operands
            .outputs_at(self.slot_pc.unwrap())
            .get(output_position)
        else {
            return false;
        };
        if *output & crate::program::LOCAL_OUTPUT != 0 {
            let destination = (*output & !crate::program::LOCAL_OUTPUT) as usize;
            match self.locals.get(destination) {
                Some(Local::Aggregate(destination))
                    if source.result_aggregate_values.has_plan(index) =>
                {
                    Ok(self.aggregate_values.can_copy_slot_from(
                        *destination,
                        &source.result_aggregate_values,
                        index,
                    ))
                }
                Some(Local::Aggregate(destination)) => source
                    .result_handles
                    .get(index)
                    .and_then(|handle| *handle)
                    .map_or(Ok(false), |handle| {
                        self.aggregate_values.can_copy_slot_from_heap(
                            *destination,
                            handle,
                            source_type,
                            &self.heap,
                            self.revision.program.decoded.types(),
                        )
                    }),
                _ => Ok(false),
            }
        } else if source.result_aggregate_values.has_plan(index) {
            Ok(self.slot_aggregate_values.can_copy_slot_from(
                *output as usize,
                &source.result_aggregate_values,
                index,
            ))
        } else {
            source
                .result_handles
                .get(index)
                .and_then(|handle| *handle)
                .map_or(Ok(false), |handle| {
                    self.slot_aggregate_values.can_copy_slot_from_heap(
                        *output as usize,
                        handle,
                        source_type,
                        &self.heap,
                        self.revision.program.decoded.types(),
                    )
                })
        }
        .unwrap_or(false)
    }

    fn set_inline(&mut self, index: usize, value: &Value) -> Result<(), RuntimeError> {
        let slot = match self.locals[index] {
            Local::Inline(slot) => slot,
            Local::Uninitialized | Local::Private(_) | Local::Shared(_) | Local::Aggregate(_) => {
                return Err(RuntimeError::new(
                    "invalid_local",
                    "frame",
                    "local is not an inline scalar",
                ));
            }
        };
        self.scalar_values.set(slot, value)
    }
}

#[derive(Default)]
pub(super) struct FramePool {
    state: std::sync::Mutex<FramePoolState>,
}

#[derive(Default)]
struct FramePoolState {
    frames: HashMap<(u64, usize), Vec<FrameStorage>>,
    operands: Vec<Vec<Value>>,
    bytes: usize,
}

#[derive(Default)]
pub(super) struct FrameStorage {
    pub resident: Option<crate::managed_storage::StorageResidentLease>,
    pub locals: Vec<Local>,
    pub scalar_values: ScalarBanks,
    pub aggregate_values: AggregateBanks,
    pub slot_scalar_values: ScalarBanks,
    pub slot_aggregate_values: AggregateBanks,
    pub result_scalar_values: ScalarBanks,
    pub result_aggregate_values: AggregateBanks,
    pub result_handles: Vec<Option<Handle>>,
    pub globals: Vec<Handle>,
    pub delivery: Vec<Handle>,
    pub slot_handles: Vec<Option<Handle>>,
    pub popped: memory::OperandRoots,
    pub defers: Vec<FunctionValue>,
    pub map_iterators: Vec<Option<MapIterator>>,
}

impl FrameStorage {
    fn bytes(&self) -> usize {
        self.locals.capacity() * size_of::<Local>()
            + self.scalar_values.bytes()
            + self.aggregate_values.bytes()
            + self.slot_scalar_values.bytes()
            + self.slot_aggregate_values.bytes()
            + self.result_scalar_values.bytes()
            + self.result_aggregate_values.bytes()
            + self.result_handles.capacity() * size_of::<Option<Handle>>()
            + self.globals.capacity() * size_of::<Handle>()
            + self.delivery.capacity() * size_of::<Handle>()
            + self.slot_handles.capacity() * size_of::<Option<Handle>>()
            + self.popped.storage_bytes()
            + self.defers.capacity() * size_of::<FunctionValue>()
            + self.map_iterators.capacity() * size_of::<Option<MapIterator>>()
    }

    fn planned_bytes(
        &self,
        local_types: &[TypeIdentity],
        slot_types: &[TypeIdentity],
        result_types: &[TypeIdentity],
        registry: &TypeRegistry,
        globals: usize,
        map_iterators: usize,
    ) -> usize {
        let local_banks = ScalarBanks::for_types(local_types).bytes();
        let local_aggregates = AggregateBanks::for_types(local_types, registry).bytes();
        let slot_banks = ScalarBanks::for_types(slot_types).bytes();
        let slot_aggregates = AggregateBanks::for_types(slot_types, registry).bytes();
        let result_banks = ScalarBanks::for_types(result_types).bytes();
        let result_aggregates = AggregateBanks::for_types(result_types, registry).bytes();
        self.locals
            .capacity()
            .max(local_types.len())
            .saturating_mul(size_of::<Local>())
            .saturating_add(self.scalar_values.bytes().max(local_banks))
            .saturating_add(self.aggregate_values.bytes().max(local_aggregates))
            .saturating_add(self.slot_scalar_values.bytes().max(slot_banks))
            .saturating_add(self.slot_aggregate_values.bytes().max(slot_aggregates))
            .saturating_add(self.result_scalar_values.bytes().max(result_banks))
            .saturating_add(self.result_aggregate_values.bytes().max(result_aggregates))
            .saturating_add(
                self.result_handles.capacity().max(result_types.len())
                    * size_of::<Option<Handle>>(),
            )
            .saturating_add(self.globals.capacity().max(globals) * size_of::<Handle>())
            .saturating_add(self.delivery.capacity() * size_of::<Handle>())
            .saturating_add(
                self.slot_handles
                    .capacity()
                    .max(slot_types.len())
                    .saturating_mul(size_of::<Option<Handle>>()),
            )
            .saturating_add(self.popped.storage_bytes())
            .saturating_add(self.defers.capacity() * size_of::<FunctionValue>())
            .saturating_add(
                self.map_iterators.capacity().max(map_iterators) * size_of::<Option<MapIterator>>(),
            )
    }

    pub(super) fn dynamic_type_ids(&self, output: &mut Vec<u32>) {
        self.aggregate_values.dynamic_type_ids(output);
        self.slot_aggregate_values.dynamic_type_ids(output);
        self.result_aggregate_values.dynamic_type_ids(output);
    }
}

impl FramePool {
    pub fn take_operands(&self, count: usize) -> Vec<Value> {
        let mut state = self.state.lock().unwrap();
        if let Some(index) = state
            .operands
            .iter()
            .position(|values| values.capacity() >= count)
        {
            let values = state.operands.swap_remove(index);
            state.bytes -= values.capacity() * size_of::<Value>();
            values
        } else {
            drop(state);
            Vec::with_capacity(count)
        }
    }

    pub fn recycle_operands(&self, mut values: Vec<Value>, limit: usize) {
        values.clear();
        let bytes = values.capacity() * size_of::<Value>();
        let mut state = self.state.lock().unwrap();
        if bytes != 0 && state.operands.len() < 8 && bytes <= limit.saturating_sub(state.bytes) {
            state.bytes += bytes;
            state.operands.push(values);
        }
    }

    pub fn take(&self, generation: u64, function: usize) -> FrameStorage {
        let key = (generation, function);
        let mut state = self.state.lock().unwrap();
        let storage = state
            .frames
            .get_mut(&key)
            .and_then(Vec::pop)
            .unwrap_or_default();
        state.bytes -= storage.bytes();
        storage
    }

    fn recycle(&self, generation: u64, function: usize, storage: FrameStorage, limit: usize) {
        let bytes = storage.bytes();
        let key = (generation, function);
        let mut state = self.state.lock().unwrap();
        if bytes <= limit.saturating_sub(state.bytes)
            && state.frames.get(&key).map_or(0, Vec::len) < 8
        {
            state.frames.entry(key).or_default().push(storage);
            state.bytes += bytes;
        }
    }

    pub fn clear(&self) {
        let previous = std::mem::take(&mut *self.state.lock().unwrap());
        drop(previous);
    }

    pub(super) fn dynamic_type_ids(&self, output: &mut Vec<u32>) {
        let state = self.state.lock().unwrap();
        for storage in state.frames.values().flatten() {
            storage.dynamic_type_ids(output);
        }
    }
}

impl Trace for FramePool {
    fn trace(&self, visit: &mut dyn FnMut(Handle)) {
        let mut roots = Vec::new();
        let state = self.state.lock().unwrap();
        for storage in state.frames.values().flatten() {
            for local in &storage.locals {
                local.trace(&mut |handle| roots.push(handle));
            }
            storage
                .aggregate_values
                .trace_all(&mut |handle| roots.push(handle));
            storage
                .slot_aggregate_values
                .trace_all(&mut |handle| roots.push(handle));
        }
        drop(state);
        for handle in roots {
            visit(handle);
        }
    }
}

impl Instance {
    pub(super) fn load_local(&mut self, index: usize) -> Result<Value, RuntimeError> {
        let value = {
            let frame = self.running.frames.last().unwrap();
            if let Local::Shared(handle) = frame.locals[index] {
                return self.load_slot(handle);
            }
            frame.local_value(index)?
        };
        if matches!(value.data, Data::Uninitialized) {
            let value = self.zero(&value.typ)?;
            self.store_local(index, value, false)?;
        }
        self.running.frames.last().unwrap().local_value(index)
    }

    pub(super) fn store_local(
        &mut self,
        index: usize,
        value: Value,
        rebind: bool,
    ) -> Result<(), RuntimeError> {
        if let Some(handle) = self.shared_local_target(index, rebind)? {
            return self.store_slot(handle, value);
        }
        let typ = self.running.frames.last().unwrap().prepared.local_types[index].clone();
        let value = self.coerce(value, &typ)?;
        if matches!(
            self.running.frames.last().unwrap().locals[index],
            Local::Uninitialized
        ) {
            let addressable = self
                .running
                .frames
                .last()
                .unwrap()
                .prepared
                .addressable_locals[index];
            if addressable {
                let bytes = self.prepared_allocation_bytes(&value)?;
                value.trace(&mut |handle| self.running.transient_roots.push(handle));
                let handle = self.allocate_private_storage(value, bytes)?;
                self.running.frames.last_mut().unwrap().locals[index] = Local::Shared(handle);
                return Ok(());
            }
            if can_inline_value(&value, &typ) {
                let frame = self.running.frames.last_mut().unwrap();
                frame.scalar_values.set(index, &value)?;
                frame.locals[index] = Local::Inline(index);
                return Ok(());
            }
            if can_inline_aggregate(&value, &typ, &self.types) {
                let frame = self.running.frames.last_mut().unwrap();
                frame
                    .aggregate_values
                    .set(index, &value, &self.heap, &self.types)?;
                frame.locals[index] = Local::Aggregate(index);
                return Ok(());
            }
            let bytes = self.prepared_allocation_bytes(&value)?;
            value.trace(&mut |handle| self.running.transient_roots.push(handle));
            let handle = self.allocate_private_storage(value, bytes)?;
            self.running.frames.last_mut().unwrap().locals[index] = Local::Private(handle);
            return Ok(());
        }
        if matches!(
            self.running.frames.last().unwrap().locals[index],
            Local::Inline(_)
        ) {
            return self
                .running
                .frames
                .last_mut()
                .unwrap()
                .set_inline(index, &value);
        }
        if let Local::Aggregate(slot) = self.running.frames.last().unwrap().locals[index] {
            return self
                .running
                .frames
                .last_mut()
                .unwrap()
                .aggregate_values
                .set(slot, &value, &self.heap, &self.types);
        }
        let bytes = self.prepared_allocation_bytes(&value)?;
        value.trace(&mut |handle| self.running.transient_roots.push(handle));
        let Local::Private(handle) = self.running.frames.last().unwrap().locals[index] else {
            unreachable!()
        };
        if let Err((error, value)) = self.heap.replace(handle, value, bytes) {
            if error.code == "stale_reference" {
                let replacement = self.allocate_private_storage(value, bytes)?;
                self.running.frames.last_mut().unwrap().locals[index] = Local::Private(replacement);
                return Ok(());
            }
            self.frame_pool.clear();
            self.collect_rooted()?;
            match self.heap.replace(handle, value, bytes) {
                Ok(()) => {}
                Err((error, value)) if error.code == "stale_reference" => {
                    let replacement = self.allocate_private_storage(value, bytes)?;
                    self.running.frames.last_mut().unwrap().locals[index] =
                        Local::Private(replacement);
                }
                Err((error, _)) => return Err(error),
            }
        }
        Ok(())
    }

    pub(super) fn shared_local_target(
        &mut self,
        index: usize,
        rebind: bool,
    ) -> Result<Option<Handle>, RuntimeError> {
        let Local::Shared(handle) = &self.running.frames.last().unwrap().locals[index] else {
            return Ok(None);
        };
        if !rebind {
            return Ok(Some(*handle));
        }
        let typ = self.heap.read_typed_value(*handle)?.typ.clone();
        let handle = self.allocate(self.zero(&typ)?)?;
        self.running.frames.last_mut().unwrap().locals[index] = Local::Shared(handle);
        Ok(Some(handle))
    }

    pub(super) fn cache_frame(&mut self, frame: &mut Frame) -> Result<(), RuntimeError> {
        if frame.revision.generation != self.revision.generation
            || self.limits.max_frame_cache_bytes == 0
        {
            return Ok(());
        }
        // Precise frame roots may reclaim a private local before the frame
        // returns. Such a frame cannot be pooled: reading or resetting its
        // dead handle would turn normal liveness into a stale-reference
        // failure, and retaining it in the pool would publish an invalid root.
        if frame
            .locals
            .iter()
            .any(|local| matches!(local, Local::Private(handle) if !self.heap.is_live(*handle)))
        {
            return Ok(());
        }
        for local in &mut frame.locals {
            if matches!(local, Local::Shared(_)) {
                // Escaped cells belong to their pointers/closures, never the
                // recycled frame. A new activation allocates its own cell.
                *local = Local::Uninitialized;
                continue;
            }
            if let Local::Private(handle) = local {
                if !self.heap.payload_initialized(*handle)? {
                    continue;
                }
                let typ = self.heap.payload_type(*handle)?.ok_or_else(|| {
                    RuntimeError::new("invalid_local", "frame", "local storage has no type")
                })?;
                let value = Value {
                    typ,
                    data: Data::Uninitialized,
                };
                let bytes = self.prepared_allocation_bytes(&value)?;
                self.heap
                    .replace(*handle, value, bytes)
                    .map_err(|(error, _)| error)?;
            }
        }
        for (index, local) in frame.locals.iter().enumerate() {
            if let Local::Inline(slot) = local {
                debug_assert_eq!(*slot, index);
                frame.scalar_values.clear(*slot);
            }
            if let Local::Aggregate(slot) = local {
                frame.aggregate_values.clear(*slot);
            }
        }
        for (index, typ) in frame.prepared.slot_types.iter().enumerate() {
            if inline_type(typ) {
                frame.slot_scalar_values.clear(index);
            } else {
                frame.slot_aggregate_values.clear(index);
            }
        }
        frame.delivery.handles.clear();
        for index in 0..frame.prepared.result_types.len() {
            frame.result_scalar_values.clear(index);
            frame.result_aggregate_values.clear(index);
        }
        frame.result_handles.fill(None);
        frame.slot_handles.fill(None);
        frame.slot_constants.clear();
        frame.type_dispatch_value = None;
        frame.popped_roots.clear();
        frame.defers.clear();
        frame.map_iterators.clear();
        let storage = FrameStorage {
            resident: frame._resident.take(),
            locals: std::mem::take(&mut frame.locals),
            scalar_values: std::mem::take(&mut frame.scalar_values),
            aggregate_values: std::mem::take(&mut frame.aggregate_values),
            slot_scalar_values: std::mem::take(&mut frame.slot_scalar_values),
            slot_aggregate_values: std::mem::take(&mut frame.slot_aggregate_values),
            result_scalar_values: std::mem::take(&mut frame.result_scalar_values),
            result_aggregate_values: std::mem::take(&mut frame.result_aggregate_values),
            result_handles: std::mem::take(&mut frame.result_handles),
            globals: std::mem::take(&mut frame.globals),
            delivery: std::mem::take(&mut frame.delivery.handles),
            slot_handles: std::mem::take(&mut frame.slot_handles),
            popped: std::mem::take(&mut frame.popped_roots),
            defers: std::mem::take(&mut frame.defers),
            map_iterators: std::mem::take(&mut frame.map_iterators),
        };
        self.frame_pool.recycle(
            frame.revision.generation,
            frame.prepared.index,
            storage,
            self.limits.max_frame_cache_bytes,
        );
        Ok(())
    }
}

impl Instance {
    pub(super) fn call_with_continuation(
        &mut self,
        callee: FunctionValue,
        arguments: Vec<Value>,
        expected_results: usize,
        continuation: Continuation,
    ) -> Result<(), RuntimeError> {
        let caller_index = self.running.frames.len() - 1;
        self.push_frame(callee, arguments, expected_results, false, 0)?;
        // Frame preparation may fail or request collection. Publish the resume
        // state only once a callee owns the call, so retry reads the operands.
        self.running.frames[caller_index].continuation = Some(continuation);
        Ok(())
    }

    pub(super) fn push_frame(
        &mut self,
        callee: FunctionValue,
        arguments: Vec<Value>,
        expected_results: usize,
        initializing: bool,
        task_charge: u64,
    ) -> Result<(), RuntimeError> {
        self.push_frame_arguments(
            callee,
            FrameArguments::Owned(arguments),
            expected_results,
            initializing,
            task_charge,
        )
    }

    pub(super) fn push_frame_arguments(
        &mut self,
        callee: FunctionValue,
        mut arguments: FrameArguments,
        expected_results: usize,
        initializing: bool,
        task_charge: u64,
    ) -> Result<(), RuntimeError> {
        let heap = self.heap.clone();
        let allocations = heap.begin_allocation_scope();
        let prior_roots = self.running.transient_roots.len();
        let result = (|| {
            self.running
                .transient_roots
                .extend(callee.captures.iter().map(|address| address.root));
            let argument_count = match &arguments {
                FrameArguments::Owned(values) => {
                    for value in values {
                        value.trace(&mut |handle| self.running.transient_roots.push(handle));
                    }
                    values.len()
                }
                FrameArguments::Caller {
                    frame,
                    suspended,
                    start,
                    count,
                } => {
                    // A borrowed call transfers values from the caller only after
                    // all callee preparation has succeeded.  Keep those source
                    // slots rooted across preparation: the current PC may already
                    // mark them dead because the call consumes them, while a
                    // collection can still run before the new frame is published.
                    {
                        let caller = if *suspended {
                            &mut self.running.suspended_frames[*frame]
                        } else {
                            &mut self.running.frames[*frame]
                        };
                        let mut captured = memory::OperandRoots::default();
                        for index in 0..*count {
                            caller.capture_input_roots(*start + index, &mut captured);
                        }
                        caller.popped_roots = captured;
                    }
                    let mut roots = Vec::new();
                    {
                        let caller = if *suspended {
                            &self.running.suspended_frames[*frame]
                        } else {
                            &self.running.frames[*frame]
                        };
                        caller.popped_roots.trace(&mut |handle| roots.push(handle));
                    }
                    self.running.transient_roots.extend(roots);
                    *count
                }
            };
            if self.running.frames.len() >= self.limits.max_call_depth {
                return Err(RuntimeError::new(
                    "frame_limit",
                    &callee.function,
                    "frame budget exhausted",
                ));
            }
            let revision = if let Some(revision) = &callee.revision {
                revision.clone()
            } else if let Some(generation) = callee.revision_generation {
                if generation == self.revision.generation {
                    self.revision.clone()
                } else if let Some(revision) = self.retired_revisions.get(&generation) {
                    revision.clone()
                } else {
                    return Err(RuntimeError::new(
                        "stale_revision",
                        &callee.function,
                        "closure revision is no longer available",
                    ));
                }
            } else {
                self.revision.clone()
            };
            self.types.use_context(revision.program.decoded.types());
            let function = self.resolve_function_binding(&callee, &revision)?;
            // Numeric FN02 records intentionally omit display names. Resolve the
            // names from Program metadata before loading globals or publishing a
            // frame, otherwise executor calls reach an empty module key.
            let function_module = function.module.clone();
            let function_name = function.name.clone();
            if argument_count != function.declaration.signature.params.len()
                || expected_results != function.declaration.signature.results.len()
                || callee.captures.len() != function.upvalues.len()
            {
                return Err(RuntimeError::new(
                    "invalid_call",
                    &callee.function,
                    "argument, result or capture count mismatch",
                ));
            }
            let mut storage = self.frame_pool.take(revision.generation, function.index);
            for local in &storage.locals {
                local.trace(&mut |handle| self.running.transient_roots.push(handle));
            }
            // Any private handles prepared below belong to this not-yet-published
            // frame. If the final guest charge fails, dropping the storage is not
            // enough: the handles must leave the transient root set so the normal
            // numeric collector can reclaim them before the caller retries.
            let frame_prepare_roots = self.running.transient_roots.len();
            // Pooled storage is keyed by revision and function and has already
            // prepared these banks. Its resident lease distinguishes it from
            // cold storage; account its current capacities without constructing
            // six temporary banks merely to measure the same layout again.
            let planned_bytes = if storage.resident.is_some() {
                storage.bytes()
            } else {
                storage.planned_bytes(
                    &function.local_types,
                    &function.slot_types,
                    &function.result_types,
                    &self.types,
                    function.globals.len(),
                    function.map_iterator_count,
                )
            } as u64;
            let mut resident = match storage.resident.take() {
                Some(resident) => resident,
                None => match self.heap.reserve_resident(0) {
                    Ok(resident) => resident,
                    Err(error) => {
                        self.running.transient_roots.truncate(frame_prepare_roots);
                        return Err(error);
                    }
                },
            };
            if let Err(error) = resident.ensure_capacity(planned_bytes) {
                self.running.transient_roots.truncate(frame_prepare_roots);
                return Err(error);
            }
            storage.resident = Some(resident);
            storage
                .map_iterators
                .reserve_exact(function.map_iterator_count);
            storage
                .map_iterators
                .resize_with(function.map_iterator_count, || None);
            storage.locals.reserve_exact(
                function
                    .local_types
                    .len()
                    .saturating_sub(storage.locals.len()),
            );
            // FramePool keys storage by revision and function. Rebuild plans only
            // when a new prepared function is encountered; normal activations
            // clear initialized bits and reuse the fixed typed banks.
            storage.scalar_values.prepare(&function.local_types);
            storage
                .aggregate_values
                .prepare(&function.local_types, &self.types);
            storage.slot_scalar_values.prepare(&function.slot_types);
            storage
                .slot_aggregate_values
                .prepare(&function.slot_types, &self.types);
            storage.result_scalar_values.prepare(&function.result_types);
            storage
                .result_aggregate_values
                .prepare(&function.result_types, &self.types);
            storage
                .result_handles
                .resize(function.result_types.len(), None);
            for (index, typ) in function.local_types.iter().enumerate() {
                // Recycled private locals and frame-bank scalars already hold this
                // function's typed, uninitialized value. Only parameters and
                // shared cells need preparation on every call.
                if index >= argument_count
                    && !function.addressable_locals[index]
                    && storage.locals.get(index).is_some_and(|slot| match slot {
                        Local::Private(handle) => self
                            .heap
                            .read_typed_value(*handle)
                            .is_ok_and(|value| matches!(&value.data, Data::Uninitialized)),
                        Local::Inline(slot) => storage.scalar_values.is_uninitialized(*slot),
                        Local::Aggregate(slot) => storage.aggregate_values.is_uninitialized(*slot),
                        Local::Shared(_) => false,
                        // A lazy local has no storage to reset.  Let primitive
                        // and prepared small aggregates pass through setup so
                        // they can reclaim their typed bank; only a value with
                        // no fixed frame representation stays lazy below.
                        Local::Uninitialized => false,
                    })
                {
                    continue;
                }
                // Caller arguments that already reside in a prepared scalar or
                // aggregate lane can enter the callee bank byte-for-byte. The
                // source was rooted above for the admission transaction; this
                // avoids the second Value decode/re-encode at frame creation.
                let direct_scalar_source =
                    if index < argument_count && !function.addressable_locals[index] {
                        match &arguments {
                            FrameArguments::Caller {
                                frame,
                                suspended,
                                start,
                                ..
                            } => {
                                let caller = if *suspended {
                                    &self.running.suspended_frames[*frame]
                                } else {
                                    &self.running.frames[*frame]
                                };
                                caller.scalar_input(*start + index, typ)
                            }
                            FrameArguments::Owned(_) => None,
                        }
                    } else {
                        None
                    };
                if let Some((source_bank, source)) = direct_scalar_source {
                    let slot = match storage.locals.get(index) {
                        Some(Local::Inline(slot)) => *slot,
                        _ => index,
                    };
                    let copied = storage
                        .scalar_values
                        .copy_slot_from(slot, source_bank, source);
                    if copied {
                        let local = Local::Inline(slot);
                        if index == storage.locals.len() {
                            storage.locals.push(local);
                        } else {
                            storage.locals[index] = local;
                        }
                        continue;
                    }
                }
                let direct_aggregate_source =
                    if index < argument_count && !function.addressable_locals[index] {
                        match &arguments {
                            FrameArguments::Caller {
                                frame,
                                suspended,
                                start,
                                ..
                            } => {
                                let caller = if *suspended {
                                    &self.running.suspended_frames[*frame]
                                } else {
                                    &self.running.frames[*frame]
                                };
                                caller.transferable_aggregate_input(*start + index, typ)
                            }
                            FrameArguments::Owned(_) => None,
                        }
                    } else {
                        None
                    };
                if let Some(source) = direct_aggregate_source {
                    let slot = match storage.locals.get(index) {
                        Some(Local::Aggregate(slot)) => *slot,
                        _ => index,
                    };
                    let copied = match &arguments {
                        FrameArguments::Caller {
                            frame, suspended, ..
                        } => {
                            let caller = if *suspended {
                                &self.running.suspended_frames[*frame]
                            } else {
                                &self.running.frames[*frame]
                            };
                            storage.aggregate_values.copy_slot_from(
                                slot,
                                &caller.slot_aggregate_values,
                                source,
                            )
                        }
                        FrameArguments::Owned(_) => false,
                    };
                    if copied {
                        let local = Local::Aggregate(slot);
                        if index == storage.locals.len() {
                            storage.locals.push(local);
                        } else {
                            storage.locals[index] = local;
                        }
                        continue;
                    }
                }
                let value = if index >= argument_count {
                    Value {
                        typ: typ.clone(),
                        data: Data::Uninitialized,
                    }
                } else {
                    match &mut arguments {
                        FrameArguments::Owned(values) => {
                            std::mem::replace(&mut values[index], Value::int(0))
                        }
                        FrameArguments::Caller {
                            frame,
                            suspended,
                            start,
                            ..
                        } => {
                            let caller = if *suspended {
                                &self.running.suspended_frames[*frame]
                            } else {
                                &self.running.frames[*frame]
                            };
                            caller.input_value(*start + index)?
                        }
                    }
                };
                let value = self.coerce(value, typ)?;
                if function.addressable_locals[index] && matches!(value.data, Data::Uninitialized) {
                    // Addressable locals get a managed cell when first read or
                    // written. Keeping an uninitialized cell for every activation
                    // makes addressable-but-unused locals dominate small budgets.
                    let local = Local::Uninitialized;
                    if index == storage.locals.len() {
                        storage.locals.push(local);
                    } else {
                        storage.locals[index] = local;
                    }
                    continue;
                }
                if !function.addressable_locals[index]
                    && matches!(value.data, Data::Uninitialized)
                    && !can_inline_value(&value, typ)
                    && !can_inline_aggregate(&value, typ, &self.types)
                {
                    let local = Local::Uninitialized;
                    if index == storage.locals.len() {
                        storage.locals.push(local);
                    } else {
                        storage.locals[index] = local;
                    }
                    continue;
                }
                if !function.addressable_locals[index] && can_inline_value(&value, typ) {
                    let local = match storage.locals.get(index) {
                        Some(Local::Inline(slot)) => Local::Inline(*slot),
                        _ => Local::Inline(index),
                    };
                    let slot = match local {
                        Local::Inline(slot) => slot,
                        _ => unreachable!(),
                    };
                    storage.scalar_values.set(slot, &value)?;
                    if index == storage.locals.len() {
                        storage.locals.push(local);
                    } else {
                        storage.locals[index] = local;
                    }
                    continue;
                }
                if !function.addressable_locals[index]
                    && can_inline_aggregate(&value, typ, &self.types)
                {
                    let slot = match storage.locals.get(index) {
                        Some(Local::Aggregate(slot)) => *slot,
                        _ => index,
                    };
                    if !matches!(value.data, Data::Uninitialized) {
                        if let Err(error) =
                            storage
                                .aggregate_values
                                .set(slot, &value, &self.heap, &self.types)
                        {
                            if error.code != "collection_required" {
                                return Err(error);
                            }
                            value.trace(&mut |handle| self.running.transient_roots.push(handle));
                            self.collect_rooted()?;
                            storage
                                .aggregate_values
                                .set(slot, &value, &self.heap, &self.types)?;
                        }
                        // These lanes belong to the callee under construction,
                        // before it is present in the task frame list. Later
                        // parameters and frame admission may trigger collection.
                        storage.aggregate_values.trace_slot(slot, &mut |handle| {
                            self.running.transient_roots.push(handle);
                        });
                    }
                    let local = Local::Aggregate(slot);
                    if index == storage.locals.len() {
                        storage.locals.push(local);
                    } else {
                        storage.locals[index] = local;
                    }
                    continue;
                }
                let local = if function.addressable_locals[index] {
                    Local::Shared(self.allocate(value)?)
                } else if let Some(Local::Private(slot)) = storage.locals.get_mut(index) {
                    value.trace(&mut |handle| self.running.transient_roots.push(handle));
                    let bytes = self.prepared_allocation_bytes(&value)?;
                    let handle = *slot;
                    if let Err((error, value)) = self.heap.replace(handle, value, bytes) {
                        if error.code == "stale_reference" {
                            *slot = self.allocate_private_storage(value, bytes)?;
                            continue;
                        }
                        self.frame_pool.clear();
                        self.collect_rooted()?;
                        match self.heap.replace(handle, value, bytes) {
                            Ok(()) => {}
                            Err((error, value)) if error.code == "stale_reference" => {
                                *slot = self.allocate_private_storage(value, bytes)?;
                            }
                            Err((error, _)) => return Err(error),
                        }
                    }
                    continue;
                } else {
                    Local::Private(self.allocate_private(value)?)
                };
                // A freshly allocated local is not visible from a task until the
                // frame commit below. Keep it rooted while later locals and frame
                // accounting are prepared, so recovery GC cannot reclaim a
                // partially built frame.
                local.trace(&mut |handle| self.running.transient_roots.push(handle));
                if index == storage.locals.len() {
                    storage.locals.push(local);
                } else {
                    storage.locals[index] = local;
                }
            }
            // Each destination is prepared exactly once above. Remember only the
            // uniquely consumed sources; clearing them is the infallible commit
            // after frame admission, including when several inputs alias a slot.
            let moved_sources = match &arguments {
                FrameArguments::Caller {
                    frame,
                    suspended,
                    start,
                    count,
                } => {
                    let caller = if *suspended {
                        &self.running.suspended_frames[*frame]
                    } else {
                        &self.running.frames[*frame]
                    };
                    let mut sources = Vec::new();
                    for index in 0..*count {
                        if function.addressable_locals[index] {
                            continue;
                        }
                        let Some(source) =
                            caller.transferable_input(*start + index, &function.local_types[index])
                        else {
                            continue;
                        };
                        sources.push(source as usize);
                    }
                    sources
                }
                FrameArguments::Owned(_) => Vec::new(),
            };
            if storage.slot_handles.len() < function.slot_types.len() {
                storage
                    .slot_handles
                    .reserve_exact(function.slot_types.len() - storage.slot_handles.len());
                storage
                    .slot_handles
                    .resize_with(function.slot_types.len(), || None);
            }
            if storage.globals.is_empty() {
                storage.globals.reserve_exact(function.globals.len());
                storage.globals.extend(
                    function
                        .globals
                        .iter()
                        .map(|id| self.globals[&(function_module.to_string(), id.clone())]),
                );
            }
            let memory = memory::FrameBufferCapacities::default();
            // Captures are transferred into the active frame after the pooled
            // storage admission. Include their backing capacity before publishing
            // the frame so the first closure frame cannot grow its resident lease
            // after the Vec has already been moved.
            let resident_bytes = storage
                .bytes()
                .saturating_add(callee.captures.capacity() * size_of::<Address>())
                as u64;
            let resident = match storage.resident.take() {
                Some(mut resident) => {
                    if let Err(error) = resident.ensure_capacity(resident_bytes) {
                        self.running.transient_roots.truncate(frame_prepare_roots);
                        return Err(error);
                    }
                    resident
                }
                None => match self.heap.reserve_resident(resident_bytes) {
                    Ok(resident) => resident,
                    Err(error) => {
                        self.running.transient_roots.truncate(frame_prepare_roots);
                        return Err(error);
                    }
                },
            };
            let control_charge = if task_charge != 0 {
                crate::contract_generated::STORAGE_CONTROL_RECORD
            } else {
                0
            };
            let type_bytes = |typ: &TypeIdentity| self.types.storage_logical_bytes(typ);
            let mut frame_charge = crate::contract_generated::STORAGE_FRAME_HEADER;
            for (index, typ) in function.local_types.iter().enumerate() {
                let bytes = match storage.locals.get(index) {
                    Some(Local::Inline(_) | Local::Aggregate(_)) => type_bytes(typ)?,
                    // Heap-backed locals and lazy locals occupy a single frame
                    // reference. Their payload is charged by the managed heap
                    // when materialized, so the frame itself never charges the
                    // declared aggregate width.
                    Some(Local::Private(_) | Local::Shared(_) | Local::Uninitialized) | None => {
                        crate::contract_generated::STORAGE_REFERENCE
                    }
                };
                frame_charge = frame_charge.saturating_add(bytes);
            }
            frame_charge = frame_charge.saturating_add(
                (callee.captures.len() as u64)
                    .saturating_mul(crate::contract_generated::STORAGE_REFERENCE),
            );
            for (types, banks) in [
                (&function.slot_types[..], &storage.slot_aggregate_values),
                (
                    &function.result_types[..expected_results],
                    &storage.result_aggregate_values,
                ),
            ] {
                for (index, typ) in types.iter().enumerate() {
                    let inline_aggregate = banks.plans[index]
                        .as_ref()
                        .is_some_and(|plan| plan.layout.stride > 0);
                    let bytes = if inline_type(typ) || inline_aggregate {
                        type_bytes(typ)?
                    } else {
                        crate::contract_generated::STORAGE_REFERENCE
                    };
                    frame_charge = frame_charge.saturating_add(bytes);
                }
            }
            let charge = control_charge.saturating_add(frame_charge);
            self.ensure_allocated_capacity(charge)?;
            let reservation = match self.heap.reserve_control_capacity(charge) {
                Ok(reservation) => reservation,
                Err(error) if error.code == "collection_required" => {
                    // The temporary frame owns any private locals prepared above.
                    // Keep those handles in the transient root set while the
                    // recovery collection runs; truncating here would let it
                    // reclaim storage that is still held by `storage` and leave a
                    // stale local in the frame that is about to be published.
                    self.collect_rooted()?;
                    self.ensure_allocated_capacity(charge)?;
                    match self.heap.reserve_control_capacity(charge) {
                        Ok(reservation) => reservation,
                        Err(error) => {
                            let error = if error.code == "collection_required" {
                                RuntimeError::new(
                                    "allocation_limit",
                                    "frame",
                                    "guest allocation byte limit exceeded",
                                )
                            } else {
                                error
                            };
                            self.running.transient_roots.truncate(frame_prepare_roots);
                            return Err(error);
                        }
                    }
                }
                Err(error) => {
                    self.running.transient_roots.truncate(frame_prepare_roots);
                    return Err(error);
                }
            };
            // All fallible preparation is complete. A failed allocation or collection
            // above leaves caller operands intact; only this commit consumes them.
            match arguments {
                FrameArguments::Owned(values) => self
                    .frame_pool
                    .recycle_operands(values, self.limits.max_frame_cache_bytes),
                FrameArguments::Caller {
                    frame, suspended, ..
                } => {
                    let caller = if suspended {
                        &mut self.running.suspended_frames[frame]
                    } else {
                        &mut self.running.frames[frame]
                    };
                    for source in moved_sources {
                        caller.clear_slot(source);
                    }
                    caller.slot_input = 0;
                }
            }
            let frame_control = reservation.commit();
            self.running.frames.push(Frame {
                _control: frame_control,
                _resident: Some(resident),
                heap: self.heap.clone(),
                globals: storage.globals,
                prepared: function,
                memory,
                revision,
                module: function_module,
                function: function_name,
                pc: 0,
                type_dispatch_index: 0,
                type_dispatch_value: None,
                locals: storage.locals,
                scalar_values: storage.scalar_values,
                aggregate_values: storage.aggregate_values,
                slot_scalar_values: storage.slot_scalar_values,
                slot_aggregate_values: storage.slot_aggregate_values,
                result_scalar_values: storage.result_scalar_values,
                result_aggregate_values: storage.result_aggregate_values,
                result_handles: storage.result_handles,
                upvalues: callee.captures,
                slot_handles: storage.slot_handles,
                slot_constants: Vec::new(),
                slot_pc: None,
                slot_input: 0,
                slot_output: 0,
                delivery: ResultBuffer {
                    handles: storage.delivery,
                    reading: false,
                },
                map_iterators: storage.map_iterators,
                popped_roots: storage.popped,
                expected_results,
                initializing,
                defers: storage.defers,
                returning: None,
                panic: None,
                recovered: None,
                deferred: false,
                continuation: None,
                after_init: None,
            });
            Ok(())
        })();
        if result.is_ok() {
            allocations.commit();
        } else {
            self.running.transient_roots.truncate(prior_roots);
        }
        result
    }

    pub(super) fn finish_frame(&mut self) -> Result<(), RuntimeError> {
        let frame = self.running.frames.last_mut().unwrap();
        if let Some(callee) = frame.defers.pop() {
            let index = self.running.frames.len();
            self.push_frame(callee, Vec::new(), 0, false, 0)?;
            self.running.frames[index].deferred = true;
            return Ok(());
        }
        let frame = self.running.frames.last().unwrap();
        let function = &frame.prepared;
        if frame.panic.is_none() && !function.result_locals.is_empty() {
            let values = function
                .result_locals
                .iter()
                .map(|&index| {
                    let value = frame.local_value(index)?;
                    if matches!(value.data, Data::Uninitialized) {
                        self.zero(&value.typ)
                    } else {
                        Ok(value)
                    }
                })
                .collect::<Result<Vec<_>, _>>()?;
            self.store_return_values(values)?;
        }
        let (result_count, frame_panic, frame_deferred, expected_results) = {
            let frame = self.running.frames.last().unwrap();
            (
                frame.returning.unwrap_or(0),
                frame.panic.is_some(),
                frame.deferred,
                frame.expected_results,
            )
        };
        let mut values = self.frame_pool.take_operands(result_count);
        let mut delivered_to_caller = false;
        if !frame_panic && !frame_deferred && self.running.frames.len() >= 2 {
            if result_count != expected_results {
                return Err(RuntimeError::new(
                    "invalid_return",
                    "frame",
                    "result count mismatch",
                ));
            }
            let source_index = self.running.frames.len() - 1;
            // A mixed direct/fallback group cannot publish incrementally: a
            // later Value fallback may fail its charge after earlier prepared
            // lanes have already changed the caller. Preflight the complete
            // group and use the atomic Value publisher for mixed groups.
            let output_start = self.running.frames[source_index - 1].slot_output;
            let all_prepared = (0..result_count).all(|index| {
                let Some(output_position) = output_start.checked_add(index) else {
                    return false;
                };
                let (prefix, suffix) = self.running.frames.split_at(source_index);
                prefix[source_index - 1].can_push_prepared_result_from_at(
                    &suffix[0],
                    index,
                    output_position,
                )
            });
            if all_prepared {
                for index in 0..result_count {
                    let (prefix, suffix) = self.running.frames.split_at_mut(source_index);
                    prefix[source_index - 1].push_prepared_result_from(&suffix[0], index)?;
                }
                delivered_to_caller = true;
            } else {
                let source = self.running.frames.last().unwrap();
                for index in 0..result_count {
                    values.push(source.result_value(index)?);
                }
            }
        } else {
            let frame = self.running.frames.last().unwrap();
            for index in 0..result_count {
                values.push(frame.result_value(index)?);
            }
        }
        let frame = self.running.frames.pop().unwrap();
        let panic = frame.panic;
        let recovered = frame.recovered;
        let (deferred, initializing, expected_results) =
            (frame.deferred, frame.initializing, frame.expected_results);
        let module = frame.module.clone();
        let function_id = frame.function.clone();
        let task_id = self.running.id;
        let completion_index = self.running.suspended_frames.len();
        self.running.suspended_frames.push(frame);
        let outcome = (|| {
            if let Some(panic) = panic {
                if let Some(owner) = self.running.frames.last_mut() {
                    owner.returning = Some(0);
                    owner.panic = Some(panic);
                    return Ok(());
                }
                let value = self.heap.read_typed_value(panic)?;
                return Err(RuntimeError::new("panic", "guest", value.to_string()));
            }
            if deferred {
                let owner = self.running.frames.last().ok_or_else(|| {
                    RuntimeError::new("invalid_defer", "frame", "deferred frame lost owner")
                })?;
                if recovered
                    .zip(owner.panic)
                    .is_some_and(|(recovered, panic)| recovered == panic)
                {
                    let function = owner
                        .revision
                        .program
                        .function(&owner.module, &owner.function)?;
                    let values = function
                        .declaration
                        .signature
                        .results
                        .iter()
                        .map(|typ| self.types.resolve(&owner.module, typ))
                        .collect::<Result<Vec<_>, _>>()?
                        .iter()
                        .map(|typ| self.zero(typ))
                        .collect::<Result<Vec<_>, _>>()?;
                    self.store_return_values(values)?;
                    self.running.frames.last_mut().unwrap().panic = None;
                }
                return Ok(());
            }
            if !delivered_to_caller && values.len() != expected_results {
                return Err(RuntimeError::new(
                    "invalid_return",
                    &function_id,
                    "result count mismatch",
                ));
            }
            if initializing {
                self.initializing.remove(module.as_ref());
                self.initialized.insert(module.to_string());
                self.blocked.notify_module(module.as_ref());
            }
            if !self.running.frames.is_empty() {
                if !delivered_to_caller {
                    let publication = self
                        .running
                        .frames
                        .last_mut()
                        .unwrap()
                        .publish_results_atomic(values.clone());
                    match publication {
                        Err(error) if error.code == "collection_required" => {
                            // The completed callee remains in suspended_frames
                            // until publication finishes, keeping every result
                            // rooted while the owner replenishes storage.
                            self.frame_pool.clear();
                            self.collect_rooted()?;
                            self.running
                                .frames
                                .last_mut()
                                .unwrap()
                                .publish_results_atomic(values.clone())?;
                        }
                        outcome => outcome?,
                    }
                }
                self.frame_pool
                    .recycle_operands(values, self.limits.max_frame_cache_bytes);
            } else if self.foreground == Some(self.running.id) {
                self.results = values;
                self.foreground = None;
            }
            if self.running.frames.is_empty() {
                self.scope_work.entry(self.running.scope).or_default().tasks -= 1;
                self.changed_scopes.insert(self.running.scope);
            }
            if initializing
                && self
                    .running
                    .frames
                    .last()
                    .is_some_and(|caller| caller.after_init.is_some())
            {
                // Complete the suspended owner action after initialization, without
                // executing or charging its bytecode instruction a second time. Any
                // allocation pressure now belongs to the caller continuation rather
                // than the initializer's return instruction.
                let pc = self.running.frames.last().unwrap().after_init.unwrap();
                self.running.begin_instruction(pc);
                return self.step();
            }
            Ok(())
        })();
        let task = std::iter::once(&mut self.running)
            .chain(self.preparing_task.iter_mut())
            .chain(self.resuming_task.iter_mut())
            .chain(self.runnable.iter_mut())
            .chain(self.blocked.iter_mut())
            .find(|task| task.id == task_id)
            .expect("completion retains its task");
        let mut frame = task.suspended_frames.remove(completion_index);
        self.cache_frame(&mut frame)?;
        outcome
    }

    pub(super) fn begin_return(&mut self, values: Vec<Value>) -> Result<(), RuntimeError> {
        let frame = self.running.frames.last().unwrap();
        let function = &frame.prepared;
        if values.len() != function.result_types.len() {
            return Err(RuntimeError::new(
                "invalid_return",
                &frame.function,
                "result count mismatch",
            ));
        }
        let values = values
            .into_iter()
            .zip(function.result_types.iter())
            .map(|(value, typ)| self.coerce(value, typ))
            .collect::<Result<Vec<_>, _>>()?;
        self.store_return_values(values)?;
        self.finish_frame()
    }

    pub(super) fn store_return_values(&mut self, values: Vec<Value>) -> Result<(), RuntimeError> {
        // Source operands and each published lane remain roots while later
        // lanes allocate. The suspended frame keeps this region through
        // defer execution and delivery into the caller.
        for value in &values {
            value.trace(&mut |handle| self.running.transient_roots.push(handle));
        }
        let count = values.len();
        for (index, value) in values.into_iter().enumerate() {
            self.store_return_value_at(index, value)?;
        }
        self.running.frames.last_mut().unwrap().returning = Some(count);
        Ok(())
    }

    fn store_return_value_at(&mut self, index: usize, value: Value) -> Result<(), RuntimeError> {
        let frame = self.running.frames.last_mut().unwrap();
        if inline_type(&frame.prepared.result_types[index]) {
            frame.result_scalar_values.set(index, &value)?;
        } else if frame.result_aggregate_values.has_plan(index) {
            let outcome = frame
                .result_aggregate_values
                .set(index, &value, &self.heap, &self.types);
            match outcome {
                Err(error) if error.code == "collection_required" => {
                    self.collect_rooted()?;
                    self.running
                        .frames
                        .last_mut()
                        .unwrap()
                        .result_aggregate_values
                        .set(index, &value, &self.heap, &self.types)?;
                }
                outcome => outcome?,
            }
        } else {
            let handle = self.allocate_private(value)?;
            self.running.frames.last_mut().unwrap().result_handles[index] = Some(handle);
        }
        Ok(())
    }

    pub(super) fn return_slot_values(&mut self, count: usize) -> Result<(), RuntimeError> {
        let frame = self.running.frames.last().unwrap();
        let function = frame.prepared.clone();
        if count != function.result_types.len() {
            return Err(RuntimeError::new(
                "invalid_return",
                &frame.function,
                "result count mismatch",
            ));
        }
        let start = frame
            .slot_input
            .checked_sub(count)
            .ok_or_else(|| RuntimeError::new("invalid_operand", "frame", "input underflow"))?;
        for (index, typ) in function.result_types.iter().enumerate() {
            let direct =
                {
                    let frame = self.running.frames.last_mut().unwrap();
                    let input =
                        &frame.prepared.operands.inputs_at(frame.slot_pc.unwrap())[start + index];
                    match input.kind {
                        0 => {
                            let source = input.index as usize;
                            if frame.slot_aggregate_values.has_plan(source) {
                                frame.result_aggregate_values.copy_slot_from(
                                    index,
                                    &frame.slot_aggregate_values,
                                    source,
                                )
                            } else if let Some(handle) =
                                frame.slot_handles.get(source).and_then(|handle| *handle)
                            {
                                let source_type = frame.prepared.slot_types[source].clone();
                                frame.result_aggregate_values.copy_slot_from_heap(
                                    index,
                                    handle,
                                    &source_type,
                                    &frame.heap,
                                    &self.types,
                                )?
                            } else {
                                false
                            }
                        }
                        2 => match frame.locals[input.index as usize] {
                            Local::Aggregate(source) => frame
                                .result_aggregate_values
                                .copy_slot_from(index, &frame.aggregate_values, source),
                            Local::Private(handle) | Local::Shared(handle) => {
                                let source_type =
                                    frame.prepared.local_types[input.index as usize].clone();
                                frame.result_aggregate_values.copy_slot_from_heap(
                                    index,
                                    handle,
                                    &source_type,
                                    &frame.heap,
                                    &self.types,
                                )?
                            }
                            _ => false,
                        },
                        _ => false,
                    }
                };
            if direct {
                continue;
            }
            let value = {
                let frame = self.running.frames.last().unwrap();
                self.coerce(frame.input_value(start + index)?, typ)?
            };
            value.trace(&mut |handle| self.running.transient_roots.push(handle));
            self.store_return_value_at(index, value)?;
        }
        self.running.frames.last_mut().unwrap().returning = Some(count);
        self.running.frames.last_mut().unwrap().slot_input = start;
        self.finish_frame()
    }
}

impl scheduler::Task {
    pub(super) fn begin_instruction(&mut self, pc: usize) {
        self.instruction_active = true;
        self.instruction_pc = pc;
        self.instruction_frame = self.frames.len().checked_sub(1);
        self.instruction_frame_suspended = false;
    }

    pub(super) fn finish_instruction(&mut self) {
        self.instruction_active = false;
        self.instruction_frame = None;
        self.instruction_frame_suspended = false;
    }

    pub(super) fn pop(&mut self) -> Result<Value, RuntimeError> {
        if let Some(frame) = self.frames.last_mut()
            && !frame.delivery.reading
        {
            frame.slot_input = frame
                .slot_input
                .checked_sub(1)
                .ok_or_else(|| RuntimeError::new("invalid_operand", "frame", "input underflow"))?;
            // Slot inputs and materialized constants remain frame roots until
            // the instruction completes; its disjoint outputs cannot destroy
            // retry operands. No second root buffer is needed here.
            return frame.input_value(frame.slot_input);
        }
        let frame = self
            .frames
            .last_mut()
            .ok_or_else(|| RuntimeError::new("invalid_operand", "frame", "missing result frame"))?;
        let old_capacity = frame.memory.popped;
        let new_capacity = memory::grow_frame_buffer(old_capacity, 1)?;
        frame.ensure_resident_growth(old_capacity, new_capacity, size_of::<Value>())?;
        frame.memory.popped = new_capacity;
        let handle = frame.delivery.handles.pop().ok_or_else(|| {
            RuntimeError::new("invalid_operand", "frame", "missing continuation result")
        })?;
        let value = frame.heap.read_typed_value(handle)?;
        value.trace(&mut |handle| self.transient_roots.push(handle));
        Ok(value)
    }

    pub(super) fn pop_values(
        &mut self,
        pool: &frame::FramePool,
        count: usize,
    ) -> Result<Vec<Value>, RuntimeError> {
        let mut values = pool.take_operands(count);
        self.popped_frame = Some(self.frames.len() - 1);
        let frame = self.frames.last_mut().unwrap();
        let old_capacity = frame.memory.popped;
        let new_capacity = memory::grow_frame_buffer(old_capacity, count)?;
        frame.ensure_resident_growth(old_capacity, new_capacity, size_of::<Value>())?;
        frame.memory.popped = new_capacity;
        if !frame.delivery.reading {
            let start = frame
                .slot_input
                .checked_sub(count)
                .ok_or_else(|| RuntimeError::new("invalid_operand", "frame", "input underflow"))?;
            for index in start..frame.slot_input {
                values.push(frame.input_value(index)?);
            }
            frame.slot_input = start;
        } else {
            let results = &mut frame.delivery.handles;
            let start = results.len().checked_sub(count).ok_or_else(|| {
                RuntimeError::new("invalid_operand", "frame", "missing continuation results")
            })?;
            let handles: Vec<_> = results.drain(start..).collect();
            for handle in handles {
                values.push(frame.heap.read_typed_value(handle)?);
            }
        }
        frame.popped_roots.capture(&values);
        for value in &values {
            value.trace(&mut |handle| self.transient_roots.push(handle));
        }
        Ok(values)
    }
}

#[cfg(test)]
mod pool_tests {
    use super::*;
    use crate::types::{ConstructedField, ConstructedType};

    #[test]
    fn scalar_frame_bank_does_not_publish_a_heap_entry() {
        let heap: Heap = Heap::new(4, 512).unwrap();
        let typ = TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt);
        let mut storage = FrameStorage {
            locals: vec![Local::Inline(0)],
            scalar_values: ScalarBanks::for_types(std::slice::from_ref(&typ)),
            ..FrameStorage::default()
        };
        storage.scalar_values.set(0, &Value::int(41)).unwrap();
        let mut roots = Vec::new();
        for local in &storage.locals {
            local.trace(&mut |handle| roots.push(handle));
        }
        assert!(roots.is_empty());
        assert_eq!(heap.stats().live_objects, 0);
        assert_eq!(
            storage
                .scalar_values
                .get(0, &typ)
                .value(&typ)
                .integer()
                .unwrap(),
            41
        );
        storage
            .scalar_values
            .set(
                0,
                &Value {
                    typ: typ.clone(),
                    data: Data::Unsigned(9),
                },
            )
            .expect_err("signed bank rejects unsigned value");
        storage.scalar_values.set(0, &Value::int(9)).unwrap();
        assert!(matches!(
            storage.scalar_values.get(0, &typ).value(&typ).data,
            Data::Integer(9)
        ));
    }

    #[test]
    fn scalar_frame_bank_accepts_uninitialized_and_rebuilds_type_columns() {
        let int = TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt);
        let boolean = TypeIdentity::Primitive(crate::contract_generated::PrimitiveBool);
        let mut bank = ScalarBanks::for_types(std::slice::from_ref(&int));
        bank.set(0, &Value::int(7)).unwrap();
        let integers = bank.integers.as_ptr();
        bank.prepare(std::slice::from_ref(&int));
        assert!(bank.is_uninitialized(0));
        assert_eq!(bank.integers.as_ptr(), integers);
        bank.set(
            0,
            &Value {
                typ: int.clone(),
                data: Data::Uninitialized,
            },
        )
        .unwrap();
        assert!(bank.is_uninitialized(0));

        // Frame storage is pooled across functions. A same-sized frame with a
        // different scalar class must install a new column map before its
        // first parameter is written.
        bank = ScalarBanks::for_types(std::slice::from_ref(&boolean));
        bank.set(
            0,
            &Value {
                typ: boolean,
                data: Data::Bool(true),
            },
        )
        .unwrap();
        assert!(matches!(bank.get(0, &int), InlineScalar::Bool(true)));
    }

    #[test]
    fn scalar_frame_bank_copies_exact_lanes_without_materializing_values() {
        let integer = TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt);
        let unsigned = TypeIdentity::Primitive(crate::contract_generated::PrimitiveUint);
        let mut source = ScalarBanks::for_types(&[integer.clone(), unsigned.clone()]);
        source.set(0, &Value::int(41)).unwrap();
        source
            .set(
                1,
                &Value {
                    typ: unsigned.clone(),
                    data: Data::Unsigned(9),
                },
            )
            .unwrap();
        let mut destination = ScalarBanks::for_types(&[integer.clone(), unsigned.clone()]);
        assert!(destination.copy_slot_from(0, &source, 0));
        assert!(matches!(
            destination.get(0, &integer),
            InlineScalar::Integer(41)
        ));
        assert!(destination.copy_slot_from(1, &source, 1));
        assert!(matches!(
            destination.get(1, &unsigned),
            InlineScalar::Unsigned(9)
        ));

        // A scalar class mismatch and an uninitialized source never publish a
        // destination lane, which keeps the caller release path retryable.
        assert!(!destination.copy_slot_from(0, &source, 1));
        assert!(matches!(
            destination.get(0, &integer),
            InlineScalar::Integer(41)
        ));
        let mut empty = ScalarBanks::for_types(std::slice::from_ref(&integer));
        assert!(!destination.copy_slot_from(0, &empty, 0));
        empty.set(0, &Value::int(7)).unwrap();
        assert!(destination.copy_slot_from(0, &empty, 0));
        assert!(matches!(
            destination.get(0, &integer),
            InlineScalar::Integer(7)
        ));
    }

    #[test]
    fn function_record_roundtrip_preserves_captures_in_frame_lane() {
        let mut types = TypeRegistry::new([]).unwrap();
        let typ = types
            .construct(
                ConstructedType::Function {
                    params: Vec::new(),
                    results: Vec::new(),
                    variadic: false,
                },
                32,
                16,
            )
            .unwrap();
        types.register_dynamic(&typ, 32, 1 << 20, 16).unwrap();
        let heap = Heap::new(32, 8192).unwrap();
        heap.set_type_registry(types.clone());
        let captured = heap.allocate(Value::int(42), 8).unwrap();
        let function = Value {
            typ: typ.clone(),
            data: Data::Function(Arc::new(FunctionValue {
                index: Some(7),
                revision: None,
                revision_generation: Some(1),
                revision_anchor: None,
                module: Arc::from("main"),
                function: Arc::from("callback"),
                captures: vec![Address {
                    root: captured,
                    path: Vec::new(),
                    identity: Arc::default(),
                }],
            })),
        };
        let record = heap
            .allocate(
                function.clone(),
                heap.logical_allocation_bytes(&function).unwrap(),
            )
            .unwrap();
        let mut bank = AggregateBanks::for_types(std::slice::from_ref(&typ), &types);
        // Complete records have code/capture bytes; a frame lane contains a
        // reference to a record. Exercise the same admission and fallback as
        // load_local and return publication.
        let copied = bank
            .can_copy_slot_from_heap(0, record, &typ, &heap, &types)
            .unwrap();
        if copied {
            assert!(
                bank.copy_slot_from_heap(0, record, &typ, &heap, &types)
                    .unwrap()
            );
        } else {
            bank.set(0, &heap.read_typed_value(record).unwrap(), &heap, &types)
                .unwrap();
        }
        let mut roots = Vec::new();
        bank.trace_slot(0, &mut |handle| roots.push(handle));
        heap.flush_worker_lease().unwrap();
        heap.collect_minor(roots.iter().copied()).unwrap();
        heap.collect(roots).unwrap();
        let decoded = bank.get(0, &typ, &heap, &types).unwrap().unwrap();
        let Data::Function(function) = decoded.data else {
            panic!("expected function")
        };
        assert_eq!(function.index, Some(7));
        assert_eq!(
            heap.read_typed_value(function.captures[0].root)
                .unwrap()
                .integer()
                .unwrap(),
            42
        );
    }

    #[test]
    fn rejected_heap_lane_copy_preserves_initialized_destination() {
        let mut types = TypeRegistry::new([]).unwrap();
        let integer = TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt);
        let array = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: integer.clone(),
                },
                64,
                16,
            )
            .unwrap();
        types.register_dynamic(&array, 64, 1 << 20, 16).unwrap();
        let heap = Heap::new(8, 4096).unwrap();
        heap.set_type_registry(types.clone());
        let source = heap.allocate(Value::int(7), 8).unwrap();
        let mut bank = AggregateBanks::for_types(std::slice::from_ref(&array), &types);
        bank.set(
            0,
            &Value {
                typ: array.clone(),
                data: Data::Array(vec![Value::int(11), Value::int(22)].into()),
            },
            &heap,
            &types,
        )
        .unwrap();
        let before = bank.bytes.clone();
        assert!(
            !bank
                .copy_slot_from_heap(0, source, &integer, &heap, &types)
                .unwrap()
        );
        assert_eq!(bank.bytes, before);
        assert!(!bank.is_uninitialized(0));
        heap.flush_worker_lease().unwrap();
    }

    #[test]
    fn prepared_string_descriptor_reads_only_requested_bytes() {
        let types = TypeRegistry::new([]).unwrap();
        let typ = TypeIdentity::Primitive(crate::contract_generated::PrimitiveString);
        let heap = Heap::new(4, 4096).unwrap();
        heap.set_type_registry(types.clone());
        let mut bank = AggregateBanks::for_types(std::slice::from_ref(&typ), &types);
        bank.set(
            0,
            &Value::string(b"prefix\xF0\x9F\x8C\x8D".to_vec()),
            &heap,
            &types,
        )
        .unwrap();
        let descriptor = bank.string_descriptor(0, &typ, &types).unwrap().unwrap();
        assert_eq!(descriptor.length, 10);
        assert_eq!(descriptor.read(&heap, 0, 1).unwrap().unwrap(), b"p");
        assert_eq!(
            descriptor.read(&heap, 6, 4).unwrap().unwrap(),
            b"\xF0\x9F\x8C\x8D"
        );
        assert!(descriptor.read(&heap, 10, 1).unwrap().is_none());
    }

    #[test]
    fn aggregate_frame_bank_keeps_small_scalar_arrays_off_heap() {
        let mut types = TypeRegistry::new([]).unwrap();
        let typ = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt),
                },
                128,
                32,
            )
            .unwrap();
        types.register_dynamic(&typ, 128, 1 << 20, 32).unwrap();
        let heap = Heap::new(4, 4096).unwrap();
        heap.set_type_registry(types.clone());
        let uninitialized = Value {
            typ: typ.clone(),
            data: Data::Uninitialized,
        };
        assert!(can_inline_aggregate(&uninitialized, &typ, &types));
        let mut bank = AggregateBanks::for_types(std::slice::from_ref(&typ), &types);
        let value = Value {
            typ: typ.clone(),
            data: Data::Array(vec![Value::int(4), Value::int(9)].into()),
        };
        bank.set(0, &value, &heap, &types).unwrap();
        assert_eq!(
            bank.get_path(0, &[1], &heap, &types)
                .unwrap()
                .unwrap()
                .integer()
                .unwrap(),
            9
        );
        let mut moved = AggregateBanks::for_types(std::slice::from_ref(&typ), &types);
        assert!(moved.can_copy_slot_from(0, &bank, 0));
        assert!(moved.copy_slot_from(0, &bank, 0));
        assert_eq!(
            moved
                .get_path(0, &[0], &heap, &types)
                .unwrap()
                .unwrap()
                .integer()
                .unwrap(),
            4
        );
        let copy = bank.get(0, &typ, &heap, &types).unwrap().unwrap();
        assert_eq!(copy.element(0).unwrap().integer().unwrap(), 4);
        assert_eq!(copy.element(1).unwrap().integer().unwrap(), 9);
        let mut roots = Vec::new();
        copy.trace(&mut |handle| roots.push(handle));
        assert!(roots.is_empty());
    }

    #[test]
    fn aggregate_frame_bank_keeps_small_scalar_structs_off_heap() {
        let mut types = TypeRegistry::new([]).unwrap();
        let typ = types
            .construct(
                ConstructedType::Struct(vec![
                    ConstructedField {
                        name: "Count".into(),
                        typ: TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt),
                        tag: String::new(),
                        embedded: false,
                    },
                    ConstructedField {
                        name: "Enabled".into(),
                        typ: TypeIdentity::Primitive(crate::contract_generated::PrimitiveBool),
                        tag: String::new(),
                        embedded: false,
                    },
                ]),
                128,
                32,
            )
            .unwrap();
        types.register_dynamic(&typ, 128, 1 << 20, 32).unwrap();
        let heap = Heap::new(4, 4096).unwrap();
        heap.set_type_registry(types.clone());
        let value = Value {
            typ: typ.clone(),
            data: Data::Struct(
                BTreeMap::from([
                    ("Count".into(), Value::int(4)),
                    (
                        "Enabled".into(),
                        Value {
                            typ: TypeIdentity::Primitive(crate::contract_generated::PrimitiveBool),
                            data: Data::Bool(true),
                        },
                    ),
                ])
                .into(),
            ),
        };
        let mut bank = AggregateBanks::for_types(std::slice::from_ref(&typ), &types);
        bank.set(0, &value, &heap, &types).unwrap();
        assert_eq!(
            bank.get_path(0, &[0], &heap, &types)
                .unwrap()
                .unwrap()
                .integer()
                .unwrap(),
            4
        );
        let copy = bank.get(0, &typ, &heap, &types).unwrap().unwrap();
        assert_eq!(copy.field("Count").unwrap().integer().unwrap(), 4);
        assert!(matches!(
            copy.field("Enabled").unwrap().data,
            Data::Bool(true)
        ));
        let mut roots = Vec::new();
        copy.trace(&mut |handle| roots.push(handle));
        assert!(roots.is_empty());
    }

    #[test]
    fn aggregate_frame_bank_dynamic_roots_follow_the_representation_tag() {
        let types = TypeRegistry::new([]).unwrap();
        let heap = Heap::new(64, 1 << 20).unwrap();
        heap.set_type_registry(types.clone());
        let mut bank = AggregateBanks::for_types(&[TypeIdentity::Any], &types);
        // These scalar words have nonzero low and high halves; interpreting
        // them as a storage reference would invent a slot and generation.
        for value in [
            Value::int((7_i64 << 32) | 19),
            Value {
                typ: TypeIdentity::Primitive(wire::PrimitiveFloat64),
                data: Data::Float(f64::from_bits((11_u64 << 32) | 23)),
            },
        ] {
            bank.set(0, &value, &heap, &types).unwrap();
            let mut roots = Vec::new();
            bank.trace_slot(0, &mut |handle| roots.push(handle));
            assert!(roots.is_empty());
            heap.flush_worker_lease().unwrap();
            heap.collect(roots).unwrap();
        }
        bank.set(0, &Value::string("rooted"), &heap, &types)
            .unwrap();
        let mut roots = Vec::new();
        bank.trace_slot(0, &mut |handle| roots.push(handle));
        assert!(!roots.is_empty());
        heap.flush_worker_lease().unwrap();
        heap.collect(roots).unwrap();
        assert_eq!(
            bank.get(0, &TypeIdentity::Any, &heap, &types)
                .unwrap()
                .unwrap()
                .to_string(),
            "rooted"
        );
    }

    #[test]
    fn aggregate_frame_bank_traces_reference_lanes() {
        let mut types = TypeRegistry::new([]).unwrap();
        let typ = types
            .construct(
                ConstructedType::Struct(vec![ConstructedField {
                    name: "Text".into(),
                    typ: TypeIdentity::Primitive(crate::contract_generated::PrimitiveString),
                    tag: String::new(),
                    embedded: false,
                }]),
                128,
                32,
            )
            .unwrap();
        types.register_dynamic(&typ, 128, 1 << 20, 32).unwrap();
        let string = Value::string("prepared");
        let heap = Heap::new(2, 1024).unwrap();
        heap.set_type_registry(types.clone());
        let value = Value {
            typ: typ.clone(),
            data: Data::Struct(BTreeMap::from([("Text".into(), string)]).into()),
        };
        let mut bank = AggregateBanks::for_types(std::slice::from_ref(&typ), &types);
        bank.set(0, &value, &heap, &types).unwrap();
        let target = decode_storage_reference(&bank.bytes[..], 0).unwrap();

        let mut roots = Vec::new();
        bank.trace_slot(0, &mut |handle| roots.push(handle));
        assert_eq!(roots.len(), 1);
        assert_eq!(roots[0], target);
        assert!(heap.is_live(roots[0]));
        let copy = bank.get(0, &typ, &heap, &types).unwrap().unwrap();
        assert!(
            matches!(copy.field("Text").unwrap().data, Data::String(ref bytes) if **bytes == *b"prepared")
        );
    }

    #[test]
    fn aggregate_frame_bank_prepares_strings_and_nested_values_without_host_graph() {
        let mut types = TypeRegistry::new([]).unwrap();
        let nested_type = types
            .construct(
                ConstructedType::Array {
                    length: 2,
                    element: TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt),
                },
                128,
                32,
            )
            .unwrap();
        types
            .register_dynamic(&nested_type, 128, 1 << 20, 32)
            .unwrap();
        let typ = types
            .construct(
                ConstructedType::Struct(vec![
                    ConstructedField {
                        name: "Text".into(),
                        typ: TypeIdentity::Primitive(crate::contract_generated::PrimitiveString),
                        tag: String::new(),
                        embedded: false,
                    },
                    ConstructedField {
                        name: "Nested".into(),
                        typ: nested_type.clone(),
                        tag: String::new(),
                        embedded: false,
                    },
                ]),
                128,
                32,
            )
            .unwrap();
        types.register_dynamic(&typ, 128, 1 << 20, 32).unwrap();
        let heap = Heap::new(4, 4096).unwrap();
        heap.set_type_registry(types.clone());
        let value = Value {
            typ: typ.clone(),
            data: Data::Struct(
                BTreeMap::from([
                    (
                        "Text".into(),
                        Value {
                            typ: TypeIdentity::Primitive(
                                crate::contract_generated::PrimitiveString,
                            ),
                            data: Data::String(b"prepared"[..].into()),
                        },
                    ),
                    (
                        "Nested".into(),
                        Value {
                            typ: nested_type,
                            data: Data::Array(vec![Value::int(3), Value::int(5)].into()),
                        },
                    ),
                ])
                .into(),
            ),
        };
        let mut bank = AggregateBanks::for_types(std::slice::from_ref(&typ), &types);
        bank.set(0, &value, &heap, &types).unwrap();
        let copy = bank.get(0, &typ, &heap, &types).unwrap().unwrap();
        assert!(
            matches!(copy.field("Text").unwrap().data, Data::String(ref bytes) if **bytes == *b"prepared")
        );
        let nested = copy.field("Nested").unwrap();
        assert_eq!(nested.element(0).unwrap().integer().unwrap(), 3);
        assert_eq!(nested.element(1).unwrap().integer().unwrap(), 5);
    }

    #[test]
    fn aggregate_frame_bank_reclaims_replaced_ranges() {
        let mut types = TypeRegistry::new([]).unwrap();
        let typ = types
            .construct(
                ConstructedType::Array {
                    length: 1,
                    element: TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt),
                },
                128,
                32,
            )
            .unwrap();
        types.register_dynamic(&typ, 128, 4096, 32).unwrap();
        let heap = Heap::new(2, 4096).unwrap();
        heap.set_type_registry(types.clone());
        let mut bank = AggregateBanks::for_types(std::slice::from_ref(&typ), &types);
        for value in 0..128 {
            let value = Value {
                typ: typ.clone(),
                data: Data::Array(vec![Value::int(value)].into()),
            };
            bank.set(0, &value, &heap, &types).unwrap();
        }
        assert!(bank.bytes() < 4096, "replaced aggregate ranges leaked");
    }

    #[test]
    fn call_parameter_transfer_commits_after_preflight_and_preserves_duplicate_inputs() {
        use serde_json::json;
        let contract: serde_json::Value = serde_json::from_str(wire::CONTRACT_JSON).unwrap();
        let operation = |name: &str, descriptor: usize, operands: usize| {
            let opcode = contract["spec"]["opcodes"]
                .as_array()
                .unwrap()
                .iter()
                .position(|entry| entry["op"] == name)
                .unwrap()
                + 1;
            json!([opcode, descriptor, operands])
        };
        for (count, suspended) in [(1, false), (2, false), (16, false), (1, true), (2, true)] {
            let program = test_helpers::program_with_artifact(|artifact| {
                let string = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveString});
                artifact["functions"] = json!([
                    {"id":"fn.Main","code":{
                        "types":[string],
                        "descriptors":{"type":[{"type":string}],"call":[{"module_path":artifact["module"]["path"],"function":"callee","arg_count":count}],"return":[{}]},
                        "instructions":[operation("zero",0,0),operation("call_direct",0,1),operation("return",0,2)],
                        "operands":[{"outputs":[0]},{"inputs":vec![json!([0,0]);count],"release":[0]},{}]
                    }},
                    {"id":"callee","signature":{"params":vec![json!({"type":string});count]},
                        "locals":(0..count).map(|index|json!({"id":format!("p{index}"),"type":string})).collect::<Vec<_>>(),
                        "code":{"descriptors":{}}}
                ]);
            });
            let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
            vm.start("default", &[]).unwrap();
            assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Running);
            let backing: Arc<[u8]> = Arc::from(&b"owned argument"[..]);
            let caller = &mut vm.running.frames[0];
            caller.begin_slots(1);
            caller
                .put_slot_value(
                    0,
                    Value {
                        typ: TypeIdentity::Primitive(wire::PrimitiveString),
                        data: Data::String(backing.clone().into()),
                    },
                )
                .unwrap();
            caller.pc = 2;
            let callee = FunctionValue {
                module: caller.module.clone(),
                function: "callee".into(),
                index: None,
                revision: None,
                revision_generation: None,
                revision_anchor: None,
                captures: vec![],
            };
            vm.running.begin_instruction(1);
            if suspended {
                vm.running
                    .suspended_frames
                    .push(vm.running.frames.pop().unwrap());
            }
            vm.push_frame_arguments(
                callee,
                FrameArguments::Caller {
                    frame: 0,
                    suspended,
                    start: 0,
                    count,
                },
                0,
                false,
                0,
            )
            .unwrap();
            let frame = vm.running.frames.last().unwrap();
            for (index, local) in frame.locals.iter().enumerate() {
                let Local::Aggregate(slot) = local else {
                    panic!("string transfer should use the prepared aggregate bank")
                };
                let value = frame
                    .aggregate_values
                    .get(
                        *slot,
                        &frame.prepared.local_types[index],
                        &frame.heap,
                        &vm.types,
                    )
                    .unwrap()
                    .unwrap();
                assert!(matches!(value.data, Data::String(bytes) if &*bytes == b"owned argument"));
            }
            let caller = if suspended {
                &vm.running.suspended_frames[0]
            } else {
                &vm.running.frames[0]
            };
            assert_eq!(caller.slot_value(0).is_none(), count == 1);
            if !suspended {
                assert_eq!(vm.poll_steps(10).unwrap(), PollStatus::Ready);
            }
            vm.close().unwrap();
        }
    }

    #[test]
    fn prepared_return_uses_operand_identity_and_completes_only_the_entry_frame() {
        use serde_json::json;
        let string = json!({"kind": wire::Primitive, "primitive": wire::PrimitiveString});
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = json!([
                {"id":"fn.Main", "signature":{"params":[{"type":string.clone()}],"results":[string.clone()]},
                 "locals":[{"id":"argument","type":string.clone()}],
                 "code":test_helpers::slot_code(json!([string.clone(),string.clone()]), &[
                    ("zero",json!({"type":string.clone()}),json!({"outputs":[0]})),
                    ("call_direct",json!({"function":"callee","arg_count":1,"result_count":1}),json!({"inputs":[[2,0]],"outputs":[1]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,1]]}))
                 ])},
                {"id":"callee", "signature":{"params":[{"type":string.clone()}],"results":[string.clone()]},
                 "locals":[{"id":"argument","type":string.clone()}],
                 "code":test_helpers::slot_code(json!([string.clone()]), &[
                    ("zero",json!({"type":string.clone()}),json!({"outputs":[0]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[2,0]]}))
                 ])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[crate::snapshot::HostValue::string("returned")])
            .unwrap();
        while vm.poll_steps(1).unwrap() != PollStatus::Ready {}
        let result = vm.snapshot_results(Default::default()).unwrap();
        assert_eq!(result.roots.len(), 1);
        assert!(
            matches!(&result.roots[0].data, crate::snapshot::HostData::String(bytes) if bytes == b"returned")
        );
        assert!(vm.running.frames.is_empty());
        vm.close().unwrap();
    }

    #[test]
    fn prepared_multi_result_return_preflights_each_destination_lane() {
        use serde_json::json;

        let string = json!({
            "kind": wire::Primitive,
            "primitive": wire::PrimitiveString
        });
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = json!([
                {
                    "id":"fn.Main",
                    "signature": {
                        "params":[{"type":string.clone()},{"type":string.clone()}],
                        "results":[string.clone(),string.clone()]
                    },
                    "locals":[
                        {"id":"first","type":string.clone()},
                        {"id":"second","type":string.clone()}
                    ],
                    "code":test_helpers::slot_code(
                        json!([
                            string.clone(), string.clone(), string.clone(), string.clone()
                        ]),
                        &[
                            (
                                "call_direct",
                                json!({"function":"callee","arg_count":2,"result_count":2}),
                                json!({"inputs":[[2,0],[2,1]],"outputs":[2,3]})
                            ),
                            (
                                "return",
                                json!({"result_count":2}),
                                json!({"inputs":[[0,2],[0,3]]})
                            )
                        ]
                    )
                },
                {
                    "id":"callee",
                    "signature": {
                        "params":[{"type":string.clone()},{"type":string.clone()}],
                        "results":[string.clone(),string.clone()]
                    },
                    "locals":[
                        {"id":"first","type":string.clone()},
                        {"id":"second","type":string.clone()}
                    ],
                    "code":test_helpers::slot_code(
                        json!([string.clone(), string.clone()]),
                        &[
                            (
                                "return",
                                json!({"result_count":2}),
                                json!({"inputs":[[2,0],[2,1]]})
                            )
                        ]
                    )
                }
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start(
            "default",
            &[
                crate::snapshot::HostValue::string("first result"),
                crate::snapshot::HostValue::string("second result"),
            ],
        )
        .unwrap();
        while vm.poll_steps(1).unwrap() != PollStatus::Ready {}

        let result = vm.snapshot_results(Default::default()).unwrap();
        assert_eq!(result.roots.len(), 2);
        assert!(matches!(
            &result.roots[0].data,
            crate::snapshot::HostData::String(bytes) if bytes == b"first result"
        ));
        assert!(matches!(
            &result.roots[1].data,
            crate::snapshot::HostData::String(bytes) if bytes == b"second result"
        ));
        assert!(vm.running.frames.is_empty());
        vm.close().unwrap();
    }

    #[test]
    fn mixed_prepared_and_managed_results_retry_as_one_publication() {
        use serde_json::json;

        for direct_locals in [false, true] {
            let string = json!({"kind": wire::Primitive, "primitive": wire::PrimitiveString});
            let integer = json!({"kind": wire::Primitive, "primitive": wire::PrimitiveInt});
            let wide = json!({"kind": wire::Array, "node": "wide"});
            let program = test_helpers::program_with_artifact(|artifact| {
                artifact["constants"] = json!([
                    {"id":"child", "type":string.clone(), "value":"prepared child"}
                ]);
                artifact["type_table"] = json!({
                    "nodes": [{"id":"wide", "kind":wire::Array, "elem":integer.clone(), "length":64}]
                });
                artifact["functions"] = json!([
                    {
                        "id":"fn.Main",
                        "signature":{"results":[string.clone(),wide.clone()]},
                        "code":test_helpers::slot_code(
                            json!([string.clone(),wide.clone(),integer.clone()]),
                            &[
                                (
                                    "call_direct",
                                    json!({"function":"callee","arg_count":0,"result_count":2}),
                                    json!({"outputs":[0,1]})
                                ),
                                (
                                    "zero",
                                    json!({"type":integer.clone()}),
                                    json!({"outputs":[2]})
                                ),
                                (
                                    "return",
                                    json!({"result_count":2}),
                                    json!({"inputs":[[0,0],[0,1]]})
                                )
                            ]
                        )
                    },
                    {
                        "id":"callee",
                        "signature":{"results":[string.clone(),wide.clone()]},
                        "code":test_helpers::slot_code(
                            json!([string.clone(),wide.clone()]),
                            &[
                                ("const",json!({"constant":"child"}),json!({"outputs":[0]})),
                                ("zero",json!({"type":wide.clone()}),json!({"outputs":[1]})),
                                (
                                    "return",
                                    json!({"result_count":2}),
                                    json!({"inputs":[[0,0],[0,1]]})
                                )
                            ]
                        )
                    }
                ]);
                if direct_locals {
                    let caller = &mut artifact["functions"][0];
                    caller["locals"] = json!([
                        {"id":"text","type":string.clone()},
                        {"id":"array","type":wide.clone()}
                    ]);
                    caller["code"]["operands"][0]["outputs"] = json!([
                        crate::program::LOCAL_OUTPUT,
                        crate::program::LOCAL_OUTPUT | 1
                    ]);
                    caller["code"]["operands"][2]["inputs"] = json!([[2, 0], [2, 1]]);
                }
            });
            let max_guest_bytes = 2 << 20;
            let limits = ExecutionLimits {
                max_guest_bytes,
                ..ExecutionLimits::default()
            };
            let mut vm = Instance::new(program, limits).unwrap();
            vm.start("default", &[]).unwrap();
            // Prepare both callee results before saturating the quota. The test
            // isolates delivery into the caller from allocations inside the callee.
            while vm.running.frames.len() < 2 || vm.running.frames[1].pc < 2 {
                assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Running);
            }
            let values = (0..2)
                .map(|index| vm.running.frames[1].slot_value(index).unwrap())
                .collect();
            vm.store_return_values(values).unwrap();
            // The following filler is a
            // published, unreachable cell: its precise quota charge leaves less
            // than one managed result can consume, so the mixed publication must
            // request collection and then retry after the owner has traced the
            // suspended callee. This exercises the actual allocation failure,
            // rather than using a generally tiny execution budget as a proxy.
            vm.collect_after_bytes = u64::MAX;
            vm.collect_major_after_bytes = u64::MAX;
            let before = vm.heap_stats();
            let available = max_guest_bytes.saturating_sub(before.guest_bytes);
            assert!(available > 1024, "test setup left no quota for the filler");
            let filler = vm
                .heap
                .allocate(Value::int(0), available.saturating_sub(1))
                .map_err(|(error, _)| error)
                .unwrap();
            vm.heap.flush_worker_lease().unwrap();
            let saturated = vm.heap_stats();
            assert_eq!(saturated.collections, before.collections);
            assert!(saturated.guest_bytes >= max_guest_bytes - 1);
            // Keep the handle only long enough to make it explicit that the cell
            // is not a VM root. Collection must reclaim it through the heap graph.
            assert!(vm.heap.is_live(filler));

            vm.finish_frame().unwrap();
            assert!(!vm.heap.is_live(filler));
            assert_eq!(vm.running.frames.len(), 1);
            assert_eq!(vm.running.frames[0].slot_output, 2);
            assert_eq!(vm.heap_stats().collections, before.collections + 1);
            while vm.poll_steps(1).unwrap() != PollStatus::Ready {}
            let result = vm.snapshot_results(Default::default()).unwrap();
            assert_eq!(result.roots.len(), 2);
            assert!(matches!(
                &result.roots[0].data,
                crate::snapshot::HostData::String(bytes) if bytes == b"prepared child"
            ));
            assert!(matches!(
                &result.roots[1].data,
                crate::snapshot::HostData::Array(values) if values.len() == 64
            ));
            assert!(vm.running.frames.is_empty());
            vm.close().unwrap();
        }
    }

    #[test]
    fn call_parameter_transfer_keeps_source_until_frame_admission() {
        use serde_json::json;

        let string = json!({"kind": wire::Primitive, "primitive": wire::PrimitiveString});
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = json!([
                {"id":"fn.Main","code": test_helpers::slot_code(
                    json!([string.clone()]),
                    &[
                        ("zero", json!({"type": string.clone()}), json!({"outputs":[0]})),
                        ("call_direct", json!({"function":"callee","arg_count":1,"result_count":0}), json!({"inputs":[[0,0]],"release":[0]})),
                        ("return", json!({}), json!({}))
                    ]
                )},
                {"id":"callee","signature":{"params":[{"type":string.clone()}]},
                    "locals":[{"id":"argument","type":string.clone()}],"code":{"descriptors":{}}}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Running);

        let caller = &mut vm.running.frames[0];
        caller.begin_slots(1);
        caller
            .put_slot_value(0, Value::string("caller-owned"))
            .unwrap();
        caller.pc = 2;
        let callee = FunctionValue {
            module: caller.module.clone(),
            function: "callee".into(),
            index: None,
            revision: None,
            revision_generation: None,
            revision_anchor: None,
            captures: vec![],
        };
        vm.running.begin_instruction(1);
        vm.collect_rooted().unwrap();
        let callee_bytes = crate::contract_generated::STORAGE_FRAME_HEADER
            + crate::contract_generated::STORAGE_STRING;
        // The argument reuses its immutable backing. Leave exactly one byte
        // less than the callee activation needs; failure must retain the
        // caller's argument and restore the admission reservation.
        let before = vm.heap.stats();
        let held = vm
            .heap
            .reserve_control_capacity(
                vm.limits.max_guest_bytes - before.live_bytes - callee_bytes + 1,
            )
            .unwrap();
        let reserved = vm.heap.stats().reserved_bytes;
        let cursor = vm.running.frames[0].slot_input;
        let error = vm
            .push_frame_arguments(
                callee.clone(),
                FrameArguments::Caller {
                    frame: 0,
                    suspended: false,
                    start: 0,
                    count: 1,
                },
                0,
                false,
                0,
            )
            .unwrap_err();
        assert_eq!(error.code, "allocation_limit");
        assert_eq!(vm.running.frames.len(), 1);
        assert_eq!(vm.running.frames[0].slot_input, cursor);
        assert_eq!(vm.heap.stats().live_bytes, before.live_bytes);
        assert_eq!(vm.heap.stats().reserved_bytes, reserved);
        assert!(matches!(
            vm.running.frames[0].slot_value(0).map(|value| value.data),
            Some(Data::String(bytes)) if &*bytes == b"caller-owned"
        ));
        drop(held);
        vm.push_frame_arguments(
            callee,
            FrameArguments::Caller {
                frame: 0,
                suspended: false,
                start: 0,
                count: 1,
            },
            0,
            false,
            0,
        )
        .unwrap();
        assert_eq!(vm.running.frames.len(), 2);
        assert!(vm.running.frames[0].slot_value(0).is_none());
        assert!(matches!(
            vm.running.frames[1].local_value(0).unwrap().data,
            Data::String(bytes) if &*bytes == b"caller-owned"
        ));
        vm.close().unwrap();
    }

    #[test]
    fn continuation_is_published_only_after_callee_preparation() {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":{"descriptors":{}}},
                {"id":"callee", "locals":[{"id":"value","type":{"kind":3,"primitive":3}}], "code":{"descriptors":{}}}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        let callee = FunctionValue {
            module: vm.running.frames[0].module.clone(),
            function: "callee".into(),
            index: None,
            revision: None,
            revision_generation: None,
            revision_anchor: None,
            captures: vec![],
        };
        let frame_limit = vm.limits.max_call_depth;
        vm.limits.max_call_depth = 1;
        assert_eq!(
            vm.call_with_continuation(callee.clone(), vec![], 0, Continuation::TailReturn(0))
                .unwrap_err()
                .code,
            "frame_limit"
        );
        assert_eq!(vm.running.frames.len(), 1);
        assert!(vm.running.frames[0].continuation.is_none());
        vm.limits.max_call_depth = frame_limit;

        vm.call_with_continuation(callee, vec![], 0, Continuation::TailReturn(0))
            .unwrap();
        assert_eq!(vm.running.frames.len(), 2);
        assert!(matches!(
            vm.running.frames[0].continuation,
            Some(Continuation::TailReturn(0))
        ));
        assert!(vm.running.frames[1].continuation.is_none());
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
        assert_eq!(vm.steps(), 0);
        vm.close().unwrap();
    }

    #[test]
    fn pooled_frame_releases_shared_cells_without_resetting_escaped_values() {
        use serde_json::json;
        for escaped in [false, true] {
            let program = test_helpers::program_with_artifact(|artifact| {
                let string = json!({"kind": wire::Primitive, "primitive": wire::PrimitiveString});
                let pointer = json!({"kind": wire::Pointer, "node": "string_pointer"});
                artifact["type_table"] = json!({"nodes": [{"id": "string_pointer", "kind": wire::Pointer, "elem": string}]});
                artifact["functions"] = json!([{
                    "id": "fn.Main",
                    "signature": {"results": if escaped { vec![pointer.clone()] } else { vec![] }},
                    "locals": [{"id": "text", "type": string}],
                    "code": test_helpers::slot_code(json!([pointer]), &[
                        ("address_of", json!({"kind": "local", "local": "text"}), json!({"outputs": [0]})),
                        ("return", json!({"result_count": usize::from(escaped)}), if escaped { json!({"inputs": [[0, 0]], "release": [0]}) } else { json!({"release": [0]}) }),
                    ])
                }]);
            });
            let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
            vm.start("default", &[]).unwrap();
            assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Running);
            let Local::Shared(cell) = vm.running.frames[0].locals[0] else {
                panic!("addressable local must use a shared cell");
            };
            let value = Value::string("retained through returned pointer");
            vm.heap
                .replace(
                    cell,
                    value.clone(),
                    vm.heap.logical_allocation_bytes(&value).unwrap(),
                )
                .unwrap();
            assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
            assert!(vm.frame_pool.state.lock().unwrap().bytes > 0);
            vm.collect_rooted().unwrap();
            assert_eq!(vm.heap.is_live(cell), escaped);
            if escaped {
                let Data::String(text) = vm.heap.read_typed_value(cell).unwrap().data else {
                    panic!("escaped cell lost its string")
                };
                assert_eq!(&*text, b"retained through returned pointer");
            }
            vm.close().unwrap();
        }
    }

    #[test]
    fn concurrent_frame_and_operand_reuse_preserves_ownership_and_capacity() {
        let pool = FramePool::default();
        let heap = Heap::new(128, 128 * 1024).unwrap();
        heap.set_type_registry(TypeRegistry::new([]).unwrap());
        // Worker leases reserve storage in fixed-size batches. Allocate one
        // root for each worker before entering the scoped threads, then flush
        // that lease. The concurrent part exercises frame and operand reuse;
        // it does not make the test depend on four simultaneous lease
        // reservations or a larger heap budget.
        let worker_roots = (0..4)
            .map(|_| heap.allocate(Value::int(0), 128).unwrap())
            .collect::<Vec<_>>();
        heap.flush_worker_lease().unwrap();
        let limit = 16 * 1024;
        std::thread::scope(|scope| {
            for (worker, &root) in worker_roots.iter().enumerate() {
                let pool = &pool;
                let heap = &heap;
                scope.spawn(move || {
                    for iteration in 0..200 {
                        let mut storage = pool.take(1, worker);
                        if storage.locals.is_empty() {
                            storage.locals.push(Local::Private(root));
                        }
                        let Local::Private(local) = &storage.locals[0] else {
                            unreachable!()
                        };
                        assert_eq!(heap.get(*local).unwrap().integer().unwrap(), 0);
                        let marker = (worker * 200 + iteration + 1) as i64;
                        heap.replace(*local, Value::int(marker), 128).unwrap();
                        let mut operands = pool.take_operands(4);
                        assert!(operands.is_empty());
                        operands.push(Value::int(marker));
                        assert_eq!(
                            heap.get(*local).unwrap().integer().unwrap(),
                            operands[0].integer().unwrap()
                        );
                        heap.replace(*local, Value::int(0), 128).unwrap();
                        pool.recycle(1, worker, storage, limit);
                        pool.recycle_operands(operands, limit);
                    }
                });
            }
        });
        let state = pool.state.lock().unwrap();
        let retained = state
            .frames
            .values()
            .flatten()
            .map(FrameStorage::bytes)
            .sum::<usize>()
            + state
                .operands
                .iter()
                .map(|values| values.capacity() * size_of::<Value>())
                .sum::<usize>();
        assert_eq!(state.bytes, retained);
        assert!(retained <= limit);
        drop(state);
        pool.clear();
        heap.flush_worker_lease().unwrap();
        heap.collect([]).unwrap();
        assert_eq!(heap.stats().live_objects, 0);
        assert_eq!(heap.stats().live_bytes, 0);
    }

    #[test]
    fn frame_pool_visitors_run_after_releasing_storage_lock() {
        let pool = FramePool::default();
        let heap = Heap::new(4, 1024).unwrap();
        heap.set_type_registry(TypeRegistry::new([]).unwrap());
        let root = heap.allocate(Value::int(42), 128).unwrap();
        let pointer = Value {
            typ: TypeIdentity::Any,
            data: Data::Pointer(std::sync::Arc::new(Address {
                identity: Arc::default(),
                root,
                path: Vec::new(),
            })),
        };
        let handle = heap.allocate(pointer, 128).unwrap();
        let storage = FrameStorage {
            locals: vec![Local::Private(handle)],
            ..FrameStorage::default()
        };
        pool.recycle(1, 0, storage, 1024);
        let mut roots = Vec::new();
        pool.trace(&mut |handle| {
            pool.clear();
            roots.push(handle);
        });
        assert_eq!(roots, vec![handle]);
        heap.flush_worker_lease().unwrap();
        heap.collect([root]).unwrap();
        assert_eq!(heap.stats().live_objects, 1);
    }
}
