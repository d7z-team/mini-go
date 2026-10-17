//! Managed guest storage backed directly by `StorageHeap` slot generations.

use crate::error::RuntimeError;
use crate::managed_storage::{
    StorageHeap, StorageLayout, StorageNurseryPressure, StorageObjectMeta, StorageRef,
    StorageRewrite, StorageSnapshot, StorageWorkerPool,
};
use crate::types::TypeRegistry;
use crate::value::Value;
use std::cell::RefCell;
use std::collections::HashMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, RwLock};

thread_local! {
    /// Worker-local TLABs.  The VM runs each instance on one executor worker;
    /// leases therefore stay in TLS and allocation does not take a shared
    /// mutex.  The small map permits multiple independent heaps in tests and
    /// embedded hosts while keeping the hot path local to the worker.
    static LOCAL_LEASES: RefCell<HashMap<usize, crate::managed_storage::StorageWorkerLease>> = RefCell::new(HashMap::new());
    // A scope records allocations, not a snapshot of the heap. Nested
    // encoders retain their entries until the enclosing publication commits,
    // including objects that crossed a TLAB flush during preparation.
    static ALLOCATION_JOURNAL: RefCell<AllocationJournal> = RefCell::new(AllocationJournal::default());
}

#[derive(Default)]
struct AllocationJournal {
    objects: Vec<(usize, usize, Handle)>,
    next: usize,
    depth: usize,
}

pub(crate) struct AllocationScope<'a> {
    heap: &'a Heap,
    start: usize,
    had_lease: bool,
    committed: bool,
    // The journal and nursery lease belong to the current executor thread.
    _worker: std::marker::PhantomData<std::rc::Rc<()>>,
}

impl AllocationScope<'_> {
    pub(crate) fn commit(mut self) {
        self.committed = true;
    }
}

impl Drop for AllocationScope<'_> {
    fn drop(&mut self) {
        let discarded = ALLOCATION_JOURNAL.with(|journal| {
            let mut journal = journal.borrow_mut();
            let discarded = if self.committed {
                Vec::new()
            } else {
                let mut discarded = Vec::new();
                journal.objects.retain(|&(owner, sequence, handle)| {
                    let remove = owner == self.heap.worker_id && sequence >= self.start;
                    if remove {
                        discarded.push(handle);
                    }
                    !remove
                });
                discarded
            };
            journal.depth -= 1;
            if journal.depth == 0 {
                journal.objects.clear();
                journal.next = 0;
                // Do not retain a large one-off publication buffer forever
                // on an executor thread shared by many instances.
                if journal.objects.capacity() > 1024 {
                    journal.objects.shrink_to(128);
                }
            }
            discarded
        });
        if self.committed {
            return;
        }
        LOCAL_LEASES.with(|leases| {
            if let Some(lease) = leases.borrow_mut().get_mut(&self.heap.worker_id) {
                let retained: Vec<_> = lease
                    .objects()
                    .filter_map(|(handle, _)| (!discarded.contains(&handle)).then_some(handle))
                    .collect();
                lease.rollback_since(&retained);
            }
        });
        if let Ok(mut storage) = self.heap.storage.lock() {
            for handle in discarded.into_iter().rev() {
                // Private tickets have already been returned to the lease;
                // only objects published by a refill/direct allocation are
                // present in the shared directory.
                let _ = storage.release(handle);
            }
        }
        if !self.had_lease {
            let _ = self.heap.flush_worker_lease();
        }
    }
}

// A byte slice backing is physically compact: each uint8 occupies one byte,
// while the normal typed lane is eight-byte aligned. Numeric pointers into
// this backing carry the flag so their view uses byte offsets and byte-width
// aggregate decoding.
const POINTER_COMPACT_BYTES: u32 = 1;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::types::{ConstructedType, TypeIdentity, TypeRegistry};
    use crate::value::{Address, Data, FunctionValue, Value};
    use std::collections::VecDeque;

    fn allocation_delta(heap: &Heap, value: Value) -> (u64, u64) {
        let charge = heap.logical_allocation_bytes(&value).unwrap();
        let before = heap.stats();
        heap.allocate(value, charge).unwrap();
        heap.flush_worker_lease().unwrap();
        let after = heap.stats();
        (
            after.total_allocated_bytes - before.total_allocated_bytes,
            after.allocations - before.allocations,
        )
    }

    fn function_value(captures: Vec<Address>) -> Value {
        Value {
            typ: TypeIdentity::Any,
            data: Data::Function(Arc::new(FunctionValue {
                index: Some(0),
                revision: None,
                revision_generation: None,
                revision_anchor: None,
                module: Arc::from("main"),
                function: Arc::from("entry"),
                captures,
            })),
        }
    }

    #[test]
    fn allocation_refills_private_tickets_without_requesting_collection() {
        let heap = Heap::new(1024, 1 << 20).unwrap();
        heap.set_type_registry(TypeRegistry::new([]).unwrap());
        let mut handles = Vec::new();
        for value in 0..300 {
            let value = Value::int(value);
            let bytes = heap.logical_allocation_bytes(&value).unwrap();
            handles.push(heap.allocate(value, bytes).unwrap());
        }
        for (index, handle) in handles.iter().copied().enumerate() {
            assert_eq!(heap.get(handle).unwrap().integer().unwrap(), index as i64);
        }
        heap.flush_worker_lease().unwrap();
        assert_eq!(heap.stats().allocations, handles.len() as u64);
        assert_eq!(heap.stats().collections, 0);
        assert_eq!(heap.stats().reserved_bytes, 0);
    }

    #[test]
    fn uninitialized_aggregate_publishes_zero_lanes_without_backing_allocations() {
        let mut registry = TypeRegistry::new([]).unwrap();
        let typ = registry
            .construct(
                crate::types::ConstructedType::Array {
                    length: 2048,
                    element: TypeIdentity::Primitive(crate::contract_generated::PrimitiveString),
                },
                128,
                32,
            )
            .unwrap();
        let heap = Heap::new(4096, 1 << 20).unwrap();
        heap.set_type_registry(registry);
        let value = Value {
            typ: typ.clone(),
            data: Data::Uninitialized,
        };
        let bytes = heap.logical_allocation_bytes(&value).unwrap();
        let root = heap.allocate(value, bytes).unwrap();
        heap.flush_worker_lease().unwrap();
        let allocations = heap.stats().allocations;
        heap.initialize_zero(root).unwrap();
        assert_eq!(heap.stats().allocations, allocations);
        assert_eq!(
            heap.read_prepared_path(root, &typ, &[2047])
                .unwrap()
                .unwrap()
                .to_string(),
            ""
        );
    }

    #[test]
    fn stored_string_copies_and_substrings_reuse_their_managed_backing() {
        let heap = Heap::new(1024, 1 << 20).unwrap();
        heap.set_type_registry(TypeRegistry::new([]).unwrap());
        let original = Value::string(b"prefix answer suffix".to_vec());
        let bytes = heap.logical_allocation_bytes(&original).unwrap();
        let root = heap.allocate(original, bytes).unwrap();
        let original = heap.get(root).unwrap();
        let Data::String(string) = &original.data else {
            panic!("string expected")
        };
        let substring = Value {
            typ: original.typ.clone(),
            data: Data::String(string.slice(7..13)),
        };
        heap.flush_worker_lease().unwrap();
        let before = heap.stats().allocations;
        let bytes = heap.logical_allocation_bytes(&substring).unwrap();
        let copied = heap.allocate(substring, bytes).unwrap();
        heap.flush_worker_lease().unwrap();
        // The copied cell owns a descriptor, while both descriptors reference
        // the same immutable backing object.
        assert_eq!(heap.stats().allocations - before, 1);
        heap.collect([copied]).unwrap();
        assert!(!heap.is_live(root));
        assert_eq!(heap.get(copied).unwrap().to_string(), "answer");

        let copied_value = heap.get(copied).unwrap();
        let destination = Heap::new(1024, 1 << 20).unwrap();
        destination.set_type_registry(TypeRegistry::new([]).unwrap());
        let bytes = destination.logical_allocation_bytes(&copied_value).unwrap();
        let imported = destination.allocate(copied_value.clone(), bytes).unwrap();
        destination.flush_worker_lease().unwrap();
        heap.collect([]).unwrap();
        assert_eq!(destination.get(imported).unwrap().to_string(), "answer");
        assert_eq!(destination.stats().allocations, 2);
    }

    #[test]
    fn allocation_batch_failure_reclaims_backing_across_private_refills() {
        let heap = Heap::new(1024, 1 << 20).unwrap();
        heap.set_type_registry(TypeRegistry::new([]).unwrap());
        let retained = heap.allocate(Value::int(42), 24).unwrap();
        heap.flush_worker_lease().unwrap();
        let before = heap.stats();
        let mut values = Vec::new();
        for index in 0..140 {
            let value = Value::string(format!("value {index}").into_bytes());
            let bytes = heap.logical_allocation_bytes(&value).unwrap();
            values.push((value, bytes));
        }
        // The last parent exceeds quota only after earlier strings and their
        // backing objects have crossed multiple private page publications.
        values.push((Value::int(0), 2 << 20));
        assert!(heap.prepare_allocations(values).is_err());
        heap.flush_worker_lease().unwrap();
        assert_eq!(heap.get(retained).unwrap().integer().unwrap(), 42);
        assert_eq!(heap.stats().live_objects, before.live_objects);
        assert_eq!(heap.stats().live_bytes, before.live_bytes);
        assert_eq!(heap.stats().reserved_bytes, before.reserved_bytes);

        let value = Value::string(b"retry".to_vec());
        let bytes = heap.logical_allocation_bytes(&value).unwrap();
        let batch = heap.prepare_allocations(vec![(value, bytes)]).unwrap();
        let handle = batch.handles()[0];
        batch.commit();
        let value = heap.get(handle).unwrap();
        let Data::String(bytes) = &value.data else {
            panic!("expected the committed string result");
        };
        assert_eq!(bytes.as_ref(), b"retry");
    }

    #[test]
    fn shared_storage_cost_vectors_match_actual_heap_allocations() {
        let vectors: Vec<serde_json::Value> =
            serde_json::from_str(include_str!("../../../testdata/runtime/storage_cost.json"))
                .unwrap();
        let expected_names = [
            "bool_cell",
            "int_cell",
            "empty_array_cell",
            "string_cell",
            "function",
            "closure",
            "bound_method",
        ];
        assert!(vectors.len() >= expected_names.len());

        for name in expected_names {
            let vector = vectors
                .iter()
                .find(|vector| vector["name"] == name)
                .unwrap();
            let heap = Heap::new(64, 1 << 20).unwrap();
            let mut types = TypeRegistry::new([]).unwrap();
            // Install the primitive registry before preparing a capture root;
            // the final install below publishes any dynamic array metadata.
            heap.set_type_registry(types.clone());
            let value = match name {
                "bool_cell" => Value::boolean(true),
                "int_cell" => Value::int(42),
                "empty_array_cell" => {
                    let array = types
                        .construct(
                            ConstructedType::Array {
                                length: 0,
                                element: TypeIdentity::Primitive(
                                    crate::contract_generated::PrimitiveInt,
                                ),
                            },
                            32,
                            16,
                        )
                        .unwrap();
                    types.register_dynamic(&array, 32, 1 << 20, 16).unwrap();
                    Value {
                        typ: array,
                        data: Data::Array(Arc::new(Vec::new())),
                    }
                }
                "string_cell" => Value::string(b"abc".to_vec()),
                "function" => function_value(Vec::new()),
                "closure" => {
                    let capture = Value::int(7);
                    let capture_charge = heap.logical_allocation_bytes(&capture).unwrap();
                    let root = heap.allocate(capture, capture_charge).unwrap();
                    heap.flush_worker_lease().unwrap();
                    function_value(vec![Address {
                        root,
                        path: Vec::new(),
                        identity: Arc::default(),
                    }])
                }
                "bound_method" => Value {
                    typ: TypeIdentity::Any,
                    data: Data::Method {
                        function: match function_value(Vec::new()).data {
                            Data::Function(function) => function,
                            _ => unreachable!(),
                        },
                        receiver: Some(Arc::new(Value::int(42))),
                    },
                },
                _ => unreachable!(),
            };
            heap.set_type_registry(types);
            let (bytes, allocations) = allocation_delta(&heap, value);
            assert_eq!(
                bytes,
                vector["bytes"].as_u64().unwrap(),
                "{name} allocations={allocations}"
            );
            assert_eq!(
                allocations,
                vector["allocations"].as_u64().unwrap(),
                "{name}"
            );
        }

        for name in ["rendezvous_channel", "buffered_channel"] {
            let vector = vectors
                .iter()
                .find(|vector| vector["name"] == name)
                .unwrap();
            let heap = Heap::new(64, 1 << 20).unwrap();
            heap.set_type_registry(TypeRegistry::new([]).unwrap());
            let capacity = if name == "buffered_channel" { 2 } else { 0 };
            let before = heap.stats();
            let channel = crate::managed_channel::ManagedChannel::new(
                &heap.storage_pool(),
                capacity,
                crate::managed_storage::StorageLayout {
                    stride: 8,
                    references: Vec::new(),
                    dynamics: Vec::new(),
                },
                8,
            )
            .unwrap();
            let resource = crate::instance::scheduler::Resource::Channel {
                channel: Arc::new(std::sync::Mutex::new(channel)),
            };
            let value = Value {
                typ: TypeIdentity::Any,
                data: Data::Resource(Box::new(resource)),
            };
            let charge = heap.logical_allocation_bytes(&value).unwrap();
            let handle = heap.allocate(value, charge).unwrap();
            assert!(matches!(heap.get(handle).unwrap().data, Data::Resource(_)));
            heap.flush_worker_lease().unwrap();
            let after = heap.stats();
            assert_eq!(
                after.total_allocated_bytes - before.total_allocated_bytes,
                vector["bytes"].as_u64().unwrap(),
                "{name}"
            );
            assert_eq!(
                after.allocations - before.allocations,
                vector["allocations"].as_u64().unwrap(),
                "{name}"
            );
        }
    }

    #[test]
    fn any_aggregate_backing_and_interface_boxes_have_distinct_flags() {
        let heap = Heap::new(16, 4096).unwrap();
        heap.set_type_registry(TypeRegistry::new([]).unwrap());
        let value = Value {
            typ: TypeIdentity::Any,
            data: Data::Array(vec![Value::int(3), Value::int(5)].into()),
        };

        let backing = heap.prepared_payload(&value).unwrap();
        assert_eq!(backing.bytes.len(), 24);
        assert_eq!(
            u32::from_le_bytes(backing.bytes[20..24].try_into().unwrap()),
            crate::managed_storage::VALUE_BOX_BACKING
        );
        let decoded = heap
            .decode_prepared_value(&TypeIdentity::Any, &backing.bytes)
            .unwrap();
        assert_eq!(decoded.data.aggregate_shape(), Some((true, 2)));
        assert_eq!(decoded.element(1).unwrap().integer().unwrap(), 5);

        let interface = heap.prepared_dynamic_payload(&value).unwrap();
        assert_eq!(interface.bytes.len(), 24);
        assert_eq!(
            u32::from_le_bytes(interface.bytes[20..24].try_into().unwrap()),
            0
        );
        let decoded = heap
            .decode_prepared_value(&TypeIdentity::Any, &interface.bytes)
            .unwrap();
        assert!(matches!(decoded.data, Data::Interface(_)));

        let method = Value {
            typ: TypeIdentity::Any,
            data: Data::Method {
                function: Arc::new(FunctionValue {
                    index: Some(1),
                    revision: None,
                    revision_generation: None,
                    revision_anchor: None,
                    module: Arc::from("main"),
                    function: Arc::from("method"),
                    captures: Vec::new(),
                }),
                receiver: Some(Arc::new(Value::int(7))),
            },
        };
        let method_payload = heap.prepared_dynamic_payload(&method).unwrap();
        assert_eq!(method_payload.bytes.len(), 24);
        let decoded_method = heap
            .decode_prepared_value(&TypeIdentity::Any, &method_payload.bytes)
            .unwrap();
        assert!(matches!(decoded_method.data, Data::Method { .. }));

        let resource = Value {
            typ: TypeIdentity::Any,
            data: Data::Resource(Box::new(
                crate::instance::scheduler::Resource::from_page_mutex(false, None, VecDeque::new()),
            )),
        };
        let resource_payload = heap.prepared_dynamic_payload(&resource).unwrap();
        assert_eq!(resource_payload.bytes.len(), 24);
        let decoded_resource = heap
            .decode_prepared_value(&TypeIdentity::Any, &resource_payload.bytes)
            .unwrap();
        assert!(matches!(decoded_resource.data, Data::Resource(_)));

        let function = function_value(Vec::new());
        let function_payload = heap.prepared_dynamic_payload(&function).unwrap();
        assert_eq!(function_payload.bytes.len(), 24);
        let decoded_function = heap
            .decode_prepared_value(&TypeIdentity::Any, &function_payload.bytes)
            .unwrap();
        assert!(matches!(decoded_function.data, Data::Function(_)));

        let typed_method = Value {
            typ: TypeIdentity::Primitive(crate::contract_generated::PrimitiveFunction),
            data: Data::Method {
                function: Arc::new(FunctionValue {
                    index: Some(2),
                    revision: None,
                    revision_generation: None,
                    revision_anchor: None,
                    module: Arc::from("main"),
                    function: Arc::from("typed_method"),
                    captures: Vec::new(),
                }),
                receiver: None,
            },
        };
        let typed_method_payload = heap.prepared_payload(&typed_method).unwrap();
        assert_eq!(typed_method_payload.bytes.len(), 8);
        let decoded_typed_method = heap
            .decode_prepared_value(&typed_method.typ, &typed_method_payload.bytes)
            .unwrap();
        assert!(matches!(decoded_typed_method.data, Data::Method { .. }));
    }

    #[test]
    fn named_interface_decodes_boxed_concrete_payload_with_outer_type() {
        let artifact: crate::contract_generated::Artifact =
            serde_json::from_value(serde_json::json!({
                "module": {"path": "test", "package": "main"},
                "type_table": {"nodes": [{
                    "id": "reader",
                    "kind": crate::contract_generated::Interface,
                    "identity": {"module_path": "test", "decl_id": "Reader"}
                }]}
            }))
            .unwrap();
        let mut types = TypeRegistry::new([&artifact]).unwrap();
        let interface = types
            .resolve(
                "test",
                &crate::contract_generated::TypeRef {
                    kind: crate::contract_generated::Interface,
                    node: "reader".to_owned(),
                    ..Default::default()
                },
            )
            .unwrap();
        let concrete = types
            .construct(
                ConstructedType::Array {
                    length: 3,
                    element: TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt),
                },
                32,
                16,
            )
            .unwrap();
        types.register_dynamic(&concrete, 32, 1 << 20, 16).unwrap();
        let heap = Heap::new(16, 4096).unwrap();
        heap.set_type_registry(types);
        let value = Value {
            typ: interface.clone(),
            data: Data::Interface(Arc::new(Value {
                typ: concrete.clone(),
                data: Data::Array(Arc::new(vec![Value::int(3), Value::int(5), Value::int(7)])),
            })),
        };
        let payload = heap.prepared_dynamic_payload(&value).unwrap();
        assert_eq!(
            u32::from_le_bytes(payload.bytes[4..8].try_into().unwrap()),
            crate::managed_storage::VALUE_BOX
        );
        let decoded = heap
            .decode_prepared_value(&interface, &payload.bytes)
            .unwrap();
        let Data::Interface(inner) = decoded.data else {
            panic!("named interface lost its outer wrapper")
        };
        assert_eq!(decoded.typ, interface);
        assert_eq!(inner.data.aggregate_shape(), Some((true, 3)));
        assert_eq!(inner.element(2).unwrap().integer().unwrap(), 7);
        assert_eq!(inner.typ, concrete);
    }

    #[test]
    fn dynamic_function_record_preserves_outer_callback_semantics() {
        let mut types = TypeRegistry::new([]).unwrap();
        let function_type = types
            .construct(
                ConstructedType::Function {
                    params: vec![TypeIdentity::Primitive(
                        crate::contract_generated::PrimitiveInt,
                    )],
                    results: vec![TypeIdentity::Primitive(
                        crate::contract_generated::PrimitiveInt,
                    )],
                    variadic: false,
                },
                32,
                16,
            )
            .unwrap();
        types
            .register_dynamic(&function_type, 32, 1 << 20, 16)
            .unwrap();
        let heap = Heap::new(16, 4096).unwrap();
        heap.set_type_registry(types);
        let callback = Value {
            typ: function_type.clone(),
            data: Data::Function(Arc::new(FunctionValue {
                index: Some(7),
                revision: None,
                revision_generation: Some(1),
                revision_anchor: None,
                module: Arc::from("main"),
                function: Arc::from("callback"),
                captures: Vec::new(),
            })),
        };
        let value = Value {
            typ: function_type.clone(),
            data: Data::DynamicFunction(Arc::new(callback)),
        };
        let payload = heap.prepared_dynamic_payload(&value).unwrap();
        let decoded = heap
            .decode_prepared_value(&TypeIdentity::Any, &payload.bytes)
            .unwrap();
        let Data::DynamicFunction(callback) = decoded.data else {
            panic!("dynamic function record lost its outer callback wrapper")
        };
        assert_eq!(callback.typ, function_type);
        let Data::Function(callback) = &callback.data else {
            panic!("dynamic function record lost executable callback")
        };
        assert_eq!(callback.index, Some(7));

        // A statically typed function lane stores only the managed record
        // reference.  It must still decode as DynamicFunction and retain the
        // callback wrapper; the 24-byte dynamic descriptor is reserved for
        // erased Any/interface destinations.
        let typed_payload = heap.prepared_payload(&value).unwrap();
        assert_eq!(typed_payload.bytes.len(), 8);
        let typed_decoded = heap
            .decode_prepared_value(&function_type, &typed_payload.bytes)
            .unwrap();
        let Data::DynamicFunction(callback) = typed_decoded.data else {
            panic!("typed dynamic function lane lost its outer callback wrapper")
        };
        assert_eq!(typed_decoded.typ, function_type);
        assert_eq!(callback.typ, function_type);
        assert!(matches!(callback.data, Data::Function(_)));
    }

    #[test]
    fn empty_void_payload_is_the_zero_value() {
        let heap = Heap::new(16, 4096).unwrap();
        heap.set_type_registry(TypeRegistry::new([]).unwrap());
        let value = heap
            .decode_prepared_value(&TypeIdentity::Void, &[])
            .unwrap();
        assert_eq!(value.typ, TypeIdentity::Void);
        assert!(matches!(value.data, Data::Nil));
    }

    #[test]
    fn dynamic_nil_preserves_empty_interface_and_concrete_pointer_type() {
        let heap = Heap::new(16, 4096).unwrap();
        heap.set_type_registry(TypeRegistry::new([]).unwrap());
        let nil = Value {
            typ: TypeIdentity::Any,
            data: Data::Nil,
        };
        let payload = heap.prepared_dynamic_payload(&nil).unwrap();
        assert!(payload.bytes.iter().all(|byte| *byte == 0));
        let decoded = heap
            .decode_prepared_value(&TypeIdentity::Any, &payload.bytes)
            .unwrap();
        assert!(matches!(decoded.data, Data::Nil));
        let pointer = TypeIdentity::Pointer(Arc::new(TypeIdentity::Primitive(
            crate::contract_generated::PrimitiveInt,
        )));
        let typed_nil = Value {
            typ: pointer.clone(),
            data: Data::Nil,
        };
        let payload = heap.prepared_dynamic_payload(&typed_nil).unwrap();
        let decoded = heap
            .decode_prepared_value(&TypeIdentity::Any, &payload.bytes)
            .unwrap();
        let Data::Interface(inner) = decoded.data else {
            panic!("typed nil must retain its dynamic type")
        };
        assert_eq!(inner.typ, pointer);
        assert!(matches!(inner.data, Data::Nil));
        heap.flush_worker_lease().unwrap();
        assert_eq!(heap.stats().allocations, 0);
    }

    #[test]
    fn raw_bytes_with_record_tag_are_not_decoded_as_records() {
        let heap = Heap::new(16, 4096).unwrap();
        let before = heap.stats();
        let handle = heap.allocate_raw_bytes(b"FN02payload").unwrap();
        heap.flush_worker_lease().unwrap();
        let value = heap.read_typed_value(handle).unwrap();
        assert!(matches!(&value.data, Data::Bytes(bytes) if bytes.as_slice() == b"FN02payload"));
        assert_eq!(
            heap.read_byte_storage_range(handle, 0, 4).unwrap().unwrap(),
            b"FN02"
        );
        let after = heap.stats();
        assert_eq!(
            after.total_allocated_bytes - before.total_allocated_bytes,
            27
        );
        assert_eq!(after.allocations - before.allocations, 1);
    }

    #[test]
    fn typed_scalar_lane_with_record_prefix_uses_declared_type() {
        let heap = Heap::new(16, 4096).unwrap();
        heap.set_type_registry(TypeRegistry::new([]).unwrap());
        let typ = TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt);
        let mut payload = [0_u8; 8];
        payload[..4].copy_from_slice(b"RS01");
        // A valid eight-byte integer lane can begin with the resource tag;
        // the declared lane type must decide the decoder.
        let decoded = heap.decode_prepared_value(&typ, &payload).unwrap();
        assert_eq!(decoded.typ, typ);
        assert!(matches!(decoded.data, Data::Integer(value) if value == 0x31305352));
    }

    #[test]
    fn fixed_aggregate_bytes_do_not_select_dynamic_box_decoder() {
        let heap = Heap::new(16, 4096).unwrap();
        let mut registry = TypeRegistry::new([]).unwrap();
        let typ = registry
            .construct(
                crate::types::ConstructedType::Array {
                    length: 3,
                    element: TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt),
                },
                32,
                16,
            )
            .unwrap();
        heap.set_type_registry(registry);
        let expected = [5_i64 << 32, 17, 29];
        let payload: Vec<_> = expected
            .iter()
            .flat_map(|value| value.to_le_bytes())
            .collect();
        let decoded = heap.decode_prepared_value(&typ, &payload).unwrap();
        assert_eq!(decoded.typ, typ);
        assert_eq!(
            (0..expected.len())
                .map(|index| decoded.element(index).unwrap().integer().unwrap())
                .collect::<Vec<_>>(),
            expected
        );
    }

    #[test]
    fn prepared_slice_preserves_nonzero_offset_after_minor_collection() {
        let heap = Heap::new(32, 4096).unwrap();
        let registry = TypeRegistry::new([]).unwrap();
        heap.set_type_registry(registry.clone());
        let root = heap.allocate_raw_bytes(&[10, 20, 30, 40]).unwrap();
        let typ = TypeIdentity::Slice(Arc::new(TypeIdentity::Primitive(
            crate::contract_generated::PrimitiveUint8,
        )));
        let value = Value {
            typ: typ.clone(),
            data: Data::Slice(Arc::new(crate::value::SliceValue {
                storage: Address {
                    root,
                    path: Vec::new(),
                    identity: Arc::default(),
                },
                start: 1,
                length: 2,
                capacity: 3,
                identity: Arc::default(),
            })),
        };
        let handle = heap
            .allocate(
                value,
                heap.logical_allocation_bytes(&Value {
                    typ: typ.clone(),
                    data: Data::Slice(Arc::new(crate::value::SliceValue {
                        storage: Address {
                            root,
                            path: Vec::new(),
                            identity: Arc::default(),
                        },
                        start: 1,
                        length: 2,
                        capacity: 3,
                        identity: Arc::default(),
                    })),
                })
                .unwrap(),
            )
            .unwrap();
        heap.flush_worker_lease().unwrap();
        heap.collect_minor([handle]).unwrap();
        let decoded = heap.read_typed_value(handle).unwrap();
        let Data::Slice(slice) = &decoded.data else {
            panic!("expected prepared slice")
        };
        assert_eq!((slice.start, slice.length, slice.capacity), (1, 2, 3));
        assert_eq!(
            heap.read_byte_storage_range(slice.storage.root, 1, 2)
                .unwrap(),
            Some(vec![20, 30])
        );

        let mut types = TypeRegistry::new([]).unwrap();
        let integer = TypeIdentity::Primitive(crate::contract_generated::PrimitiveInt);
        let array = types
            .construct(
                ConstructedType::Array {
                    length: 4,
                    element: integer.clone(),
                },
                32,
                16,
            )
            .unwrap();
        types.register_dynamic(&array, 32, 1 << 20, 16).unwrap();
        heap.set_type_registry(types.clone());
        let array_root = heap
            .allocate(
                Value {
                    typ: array.clone(),
                    data: Data::Array(
                        vec![Value::int(7), Value::int(20), Value::int(22), Value::int(9)].into(),
                    ),
                },
                32,
            )
            .unwrap();
        let integer_id = types.storage_type_id(&integer).unwrap();
        let offset_slice = Value {
            typ: TypeIdentity::Slice(Arc::new(integer.clone())),
            data: Data::Slice(Arc::new(crate::value::SliceValue {
                storage: Address {
                    root: array_root,
                    path: Vec::new(),
                    identity: Arc::new(crate::value::PointerIdentity {
                        numeric: Some((8, integer_id, 0)),
                        ..Default::default()
                    }),
                },
                start: 0,
                length: 2,
                capacity: 2,
                identity: Arc::default(),
            })),
        };
        let offset_slice_handle = heap.allocate(offset_slice, 32).unwrap();
        heap.flush_worker_lease().unwrap();
        heap.collect_minor([offset_slice_handle]).unwrap();
        let decoded = heap.read_typed_value(offset_slice_handle).unwrap();
        let Data::Slice(slice) = &decoded.data else {
            panic!("expected typed array slice")
        };
        assert_eq!(slice.storage.identity.numeric, Some((8, integer_id, 0)));
        assert_eq!(
            heap.read_prepared_location(array_root, 8, integer_id, 0)
                .unwrap()
                .unwrap()
                .integer()
                .unwrap(),
            20
        );
        assert!(
            heap.write_prepared_location(array_root, 8, integer_id, 0, &Value::int(21))
                .unwrap()
        );
        assert_eq!(
            heap.read_prepared_location(array_root, 8, integer_id, 0)
                .unwrap()
                .unwrap()
                .integer()
                .unwrap(),
            21
        );
        assert_eq!(
            heap.read_prepared_location(array_root, 0, integer_id, 0)
                .unwrap()
                .unwrap()
                .integer()
                .unwrap(),
            7
        );
        assert_eq!(
            heap.read_prepared_location(array_root, 24, integer_id, 0)
                .unwrap()
                .unwrap()
                .integer()
                .unwrap(),
            9
        );
    }
}

static NEXT_WORKER_HEAP_ID: AtomicUsize = AtomicUsize::new(1);

pub type Handle = StorageRef;

pub trait Trace {
    fn trace(&self, visit: &mut dyn FnMut(Handle));
    fn payload_type(&self) -> Option<crate::types::TypeIdentity> {
        None
    }
    fn payload_initialized(&self) -> bool {
        true
    }
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct HeapStats {
    pub collections: u64,
    pub scanned_objects: u64,
    pub traced_objects: u64,
    pub reclaimed_bytes: u64,
    pub collection_nanos: u64,
    pub live_objects: usize,
    pub allocations: u64,
    pub live_bytes: u64,
    pub young_bytes: u64,
    pub old_bytes: u64,
    pub total_allocated_bytes: u64,
    pub gc_pressure_bytes: u64,
    pub peak_bytes: u64,
    pub minor_collections: u64,
    pub major_collections: u64,
    pub promoted_objects: u64,
    pub promoted_bytes: u64,
    pub copied_bytes: u64,
    pub dirty_cards: u64,
    pub scanned_roots: u64,
    pub scanned_references: u64,
    pub guest_bytes: u64,
    pub reserved_bytes: u64,
    pub last_major_reachable_bytes: u64,
    pub resident_page_bytes: u64,
}

#[derive(Clone, Debug)]
pub(crate) struct PreparedPayload {
    pub bytes: Vec<u8>,
    pub references: Vec<(u32, StorageRef)>,
}

#[derive(Clone)]
pub struct Heap {
    state: Arc<HeapState>,
}

pub struct HeapState {
    storage: StorageWorkerPool,
    type_registry: RwLock<Option<Arc<TypeRegistry>>>,
    type_registry_revision: AtomicUsize,
    worker_id: usize,
    max_objects: usize,
    max_bytes: u64,
    dynamic_type_limits: (usize, u64),
}

impl std::ops::Deref for Heap {
    type Target = HeapState;

    fn deref(&self) -> &Self::Target {
        &self.state
    }
}

impl Heap {
    pub fn new(max_objects: usize, max_bytes: u64) -> Result<Self, RuntimeError> {
        let limits = crate::instance::ExecutionLimits::default();
        Ok(Self {
            state: Arc::new(HeapState {
                storage: StorageHeap::new(Some(max_bytes), None, 4 << 20).into_worker_pool(),
                type_registry: RwLock::new(None),
                type_registry_revision: AtomicUsize::new(0),
                worker_id: NEXT_WORKER_HEAP_ID.fetch_add(1, Ordering::Relaxed),
                max_objects,
                max_bytes,
                dynamic_type_limits: (limits.max_dynamic_types, limits.max_dynamic_type_bytes),
            }),
        })
    }

    pub(crate) fn with_dynamic_type_limits(mut self, nodes: usize, bytes: u64) -> Self {
        Arc::get_mut(&mut self.state)
            .expect("heap limits are configured before sharing")
            .dynamic_type_limits = (nodes, bytes);
        self
    }

    pub(crate) fn set_type_registry(&self, types: TypeRegistry) {
        *self.type_registry.write().unwrap() = Some(Arc::new(types));
        self.type_registry_revision.fetch_add(1, Ordering::Release);
    }

    pub(crate) fn allocate_revision_anchor(
        &self,
        globals: impl ExactSizeIterator<Item = Handle>,
    ) -> Result<Handle, RuntimeError> {
        let count = u32::try_from(globals.len()).map_err(|_| {
            RuntimeError::new("allocation_limit", "revision", "too many global cells")
        })?;
        let payload_bytes = count.checked_mul(8).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "revision", "global storage overflows")
        })?;
        let logical = crate::contract_generated::STORAGE_RECORD_HEADER
            + u64::from(count) * crate::contract_generated::STORAGE_REFERENCE;
        self.flush_worker_lease()?;
        if self.storage.live_objects()? >= self.max_objects {
            return Err(RuntimeError::new(
                "allocation_limit",
                "revision",
                "object storage exhausted",
            ));
        }
        let layout = self.storage.register_layout(StorageLayout {
            stride: 8,
            references: vec![0],
            dynamics: Vec::new(),
        })?;
        let mut bytes = Vec::with_capacity(payload_bytes as usize);
        for handle in globals {
            if !self.is_live(handle) {
                return Err(RuntimeError::new(
                    "stale_reference",
                    "revision",
                    "global cell is not live",
                ));
            }
            bytes.extend_from_slice(&handle.slot.to_le_bytes());
            bytes.extend_from_slice(&handle.generation.to_le_bytes());
        }
        self.storage
            .reserve_direct(
                layout,
                count,
                logical,
                &bytes,
                StorageObjectMeta {
                    logical_bytes: logical,
                    typ: None,
                    initialized: true,
                    payload_len: payload_bytes,
                    kind: crate::managed_storage::StoragePayloadKind::Typed,
                    version: 1,
                },
            )
            .map(|handle| self.record_allocation(handle))
            .map_err(|error| {
                if error.code == "collection_required" {
                    RuntimeError::new(
                        "allocation_limit",
                        "revision",
                        "guest allocation byte limit exceeded",
                    )
                } else {
                    error
                }
            })
    }

    pub(crate) fn type_registry_revision(&self) -> usize {
        self.type_registry_revision.load(Ordering::Acquire)
    }

    fn mark_type_registry_changed(&self) {
        self.type_registry_revision.fetch_add(1, Ordering::Release);
    }
    /// Return the registry snapshot used by prepared storage.  Untyped
    /// aggregate payloads may materialize a structural type while encoding;
    /// the instance owner must adopt that snapshot before the next frame or
    /// decoder consults its own catalog.
    pub(crate) fn type_registry_snapshot(&self) -> Option<TypeRegistry> {
        self.type_registry.read().unwrap().as_deref().cloned()
    }
    pub(crate) fn live_dynamic_type_roots(
        &self,
    ) -> Result<Vec<crate::types::TypeIdentity>, RuntimeError> {
        let Some(registry) = self.type_registry_snapshot() else {
            return Ok(Vec::new());
        };
        let mut roots = Vec::new();
        for id in self.storage.live_dynamic_type_ids()? {
            let identity = registry.identity_for_storage_type_id(id).ok_or_else(|| {
                RuntimeError::new("unknown_type", "heap", "live dynamic type ID is missing")
            })?;
            roots.push(identity);
        }
        Ok(roots)
    }

    pub(crate) fn live_payload_types(
        &self,
    ) -> Result<Vec<crate::types::TypeIdentity>, RuntimeError> {
        self.storage.live_payload_types()
    }
    pub(crate) fn set_resident_limit(&self, limit: u64) -> Result<(), RuntimeError> {
        self.storage
            .set_resident_limit((limit != 0).then_some(limit))
    }
    pub(crate) fn reserve_resident(
        &self,
        bytes: u64,
    ) -> Result<crate::managed_storage::StorageResidentLease, RuntimeError> {
        self.storage.reserve_resident(bytes)
    }
    pub(crate) fn flush_worker_lease(&self) -> Result<(), RuntimeError> {
        let lease = LOCAL_LEASES.with(|leases| leases.borrow_mut().remove(&self.worker_id));
        if let Some(lease) = lease {
            lease.flush()?;
        }
        Ok(())
    }
    pub(crate) fn nursery_pressure(&self) -> Result<StorageNurseryPressure, RuntimeError> {
        self.storage.nursery_pressure()
    }
    pub(crate) fn close(&self) -> Result<(), RuntimeError> {
        self.flush_worker_lease()?;
        self.storage.close()
    }
    pub(crate) fn active_worker_leases(&self) -> Result<usize, RuntimeError> {
        self.storage.active_workers()
    }

    /// Publish the current worker page only when a prepared payload crosses
    /// into a shared owner (map, channel, resource or host result).  Private
    /// nursery handles are valid for VM-local reads, but the shared storage
    /// directory cannot resolve them until the lease is flushed.  Checking
    /// the payload's explicit reference lanes keeps scalar sends on the
    /// bump-pointer fast path while preserving the publication boundary for
    /// boxes, strings, pointers and nested aggregates.
    pub(crate) fn publish_payload(&self, payload: &PreparedPayload) -> Result<(), RuntimeError> {
        if payload
            .references
            .iter()
            .any(|(_, handle)| self.local_lease_contains(*handle))
        {
            self.flush_worker_lease()?;
        }
        Ok(())
    }
    pub(crate) fn owner_id(&self) -> u64 {
        // Patch plans are instance-owned.  The worker-local heap identity is
        // allocated when the Instance is created and must participate in the
        // plan check; a process-wide constant would let any instance apply a
        // plan prepared by another instance.
        self.worker_id as u64
    }
    pub(crate) fn is_live(&self, handle: Handle) -> bool {
        self.storage.size(handle).is_ok() || self.local_lease_contains(handle)
    }
    pub(crate) fn storage_pool(&self) -> StorageWorkerPool {
        self.storage.clone()
    }
    pub(crate) fn reserve_control_capacity(
        &self,
        bytes: u64,
    ) -> Result<crate::managed_storage::StorageControlLease, RuntimeError> {
        // A worker lease already owns a reservation in the shared ledger. Move
        // only its unused credit to the control owner; this keeps the TLAB
        // page private for ordinary frame allocations and avoids a global
        // publish on every task admission.
        let transferred = LOCAL_LEASES.with(|leases| {
            let mut leases = leases.borrow_mut();
            let Some(lease) = leases.get_mut(&self.worker_id) else {
                return false;
            };
            lease.transfer_unused_credit(bytes)
        });
        if transferred {
            return Ok(
                crate::managed_storage::StorageControlLease::from_transferred(&self.storage, bytes),
            );
        }
        self.flush_worker_lease()?;
        self.storage.reserve_control(bytes)
    }
    pub(crate) fn layout_for_type(
        &self,
        typ: &crate::types::TypeIdentity,
    ) -> Result<Arc<crate::types::StorageLaneLayout>, RuntimeError> {
        self.type_registry
            .read()
            .unwrap()
            .as_deref()
            .ok_or_else(|| RuntimeError::new("invalid_type", "heap", "missing type registry"))?
            .layout_for_type(typ)
    }
    pub(crate) fn storage_type_identity(&self, id: u32) -> Option<crate::types::TypeIdentity> {
        self.type_registry
            .read()
            .unwrap()
            .as_ref()?
            .identity_for_storage_type_id(id)
    }
    pub(crate) fn storage_ref(&self, handle: Handle) -> Result<StorageRef, RuntimeError> {
        self.is_live(handle).then_some(handle).ok_or_else(|| {
            RuntimeError::new("stale_reference", "heap", "object handle is not live")
        })
    }
    fn storage_size(&self, handle: StorageRef) -> Result<u32, RuntimeError> {
        if let Some(size) = self.local_lease_size(handle) {
            return Ok(size);
        }
        self.storage.size(handle)
    }
    pub(crate) fn meta(&self, handle: Handle) -> Result<StorageObjectMeta, RuntimeError> {
        if let Some(metadata) = self.local_lease_metadata(handle) {
            return Ok(metadata);
        }
        self.storage
            .metadata(handle)
            .map_err(|_| RuntimeError::new("stale_reference", "heap", "object handle is not live"))
    }
    pub(crate) fn read_storage(
        &self,
        handle: StorageRef,
        offset: u32,
        length: u32,
    ) -> Result<Vec<u8>, RuntimeError> {
        let mut bytes = vec![0; length as usize];
        self.read_storage_into(handle, offset, &mut bytes)?;
        Ok(bytes)
    }

    /// Read a prepared lane into caller-owned storage. Worker-local objects
    /// use one lease lookup for ownership and copying; published objects use
    /// the shared heap read guard. This is the hot path for fixed-width typed
    /// operands and avoids allocating a temporary Vec for every read.
    pub(crate) fn read_storage_into(
        &self,
        handle: StorageRef,
        offset: u32,
        destination: &mut [u8],
    ) -> Result<(), RuntimeError> {
        let local = LOCAL_LEASES.with(|leases| {
            leases
                .borrow()
                .get(&self.worker_id)
                .and_then(|lease| lease.read_into_if_present(handle, offset, destination))
        });
        if let Some(result) = local {
            return result;
        }
        self.storage
            .read_lock()?
            .read_into(handle, offset, destination)
    }

    pub(crate) fn read_storage_snapshot(
        &self,
        handle: StorageRef,
    ) -> Result<StorageSnapshot, RuntimeError> {
        if let Some(snapshot) = LOCAL_LEASES.with(|leases| {
            let leases = leases.borrow();
            let lease = leases.get(&self.worker_id)?;
            let metadata = lease.metadata(handle)?;
            let bytes = match lease.read(handle, 0, metadata.payload_len) {
                Ok(bytes) => bytes,
                Err(error) => return Some(Err(error)),
            };
            Some(Ok(StorageSnapshot { metadata, bytes }))
        }) {
            return snapshot;
        }
        self.storage.read_snapshot(handle)
    }

    /// Validate a metadata-dependent prepared lane before copying any bytes
    /// into the destination bank. The worker lease/shared heap path keeps the
    /// metadata guard through validation and the subsequent page copy.
    pub(crate) fn read_storage_snapshot_into_if<F>(
        &self,
        handle: StorageRef,
        offset: u32,
        destination: &mut [u8],
        mut validate: F,
    ) -> Result<bool, RuntimeError>
    where
        F: FnMut(&StorageObjectMeta) -> Result<bool, RuntimeError>,
    {
        let local = LOCAL_LEASES.with(|leases| {
            let leases = leases.borrow();
            leases.get(&self.worker_id).and_then(|lease| {
                lease.read_snapshot_into_if(handle, offset, destination, &mut validate)
            })
        });
        if let Some(result) = local {
            return result;
        }
        self.storage
            .read_lock()?
            .read_snapshot_into_if(handle, offset, destination, validate)
    }

    fn read_storage_snapshot_range(
        &self,
        handle: StorageRef,
        offset: u32,
        length: u32,
    ) -> Result<StorageSnapshot, RuntimeError> {
        if let Some(snapshot) = LOCAL_LEASES.with(|leases| {
            let leases = leases.borrow();
            let lease = leases.get(&self.worker_id)?;
            let metadata = lease.metadata(handle)?;
            if u64::from(offset).saturating_add(u64::from(length)) > u64::from(metadata.payload_len)
            {
                return Some(Err(RuntimeError::new(
                    "invalid_operation",
                    "managed_storage",
                    "snapshot range exceeds payload",
                )));
            }
            let bytes = match lease.read(handle, offset, length) {
                Ok(bytes) => bytes,
                Err(error) => return Some(Err(error)),
            };
            Some(Ok(StorageSnapshot { metadata, bytes }))
        }) {
            return snapshot;
        }
        self.storage.read_snapshot_range(handle, offset, length)
    }

    /// Writes a prepared field lane without materializing the containing
    /// value.  A handle can still be owned by the current worker's nursery
    /// lease; route that case through the lease so its private metadata and
    /// version remain authoritative until publication.  Published objects
    /// use the shared storage directory's versioned write-barrier path.
    pub(crate) fn write_storage_range_if_version(
        &self,
        handle: StorageRef,
        offset: u32,
        bytes: &[u8],
        expected_version: u64,
    ) -> Result<bool, RuntimeError> {
        if let Some(result) = LOCAL_LEASES.with(|leases| {
            let mut leases = leases.borrow_mut();
            leases.get_mut(&self.worker_id).and_then(|lease| {
                lease.write_range_if_version_if_present(handle, offset, bytes, expected_version)
            })
        }) {
            return result;
        }
        self.storage
            .write_range_if_version(handle, offset, bytes, expected_version)
    }

    /// Return the version of a serialized RS01 channel record after checking
    /// its fixed header. Channel send/receive uses this as the fallible
    /// preflight; the subsequent state update writes only the fixed fields
    /// and does not rebuild or allocate a resource value.
    pub(crate) fn channel_record_version(&self, handle: StorageRef) -> Result<u64, RuntimeError> {
        let metadata = self.meta(handle)?;
        if metadata.payload_len < 64 {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "resource",
                "channel resource record is truncated",
            ));
        }
        let mut header = [0_u8; 8];
        self.read_storage_into(handle, 0, &mut header)?;
        if header[..4] != *b"RS01" || u32::from_le_bytes(header[4..8].try_into().unwrap()) != 1 {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "resource",
                "resource record is not a channel",
            ));
        }
        Ok(metadata.version)
    }

    /// Commit the mutable head/length/closed fields of a fixed RS01 channel
    /// record. The channel owner has already performed all fallible layout
    /// and allocation work; this only updates existing bytes through the
    /// storage version barrier.
    pub(crate) fn update_channel_record(
        &self,
        handle: StorageRef,
        expected_version: u64,
        head: usize,
        len: usize,
        closed: bool,
    ) -> Result<bool, RuntimeError> {
        let head = u64::try_from(head).map_err(|_| {
            RuntimeError::new("allocation_limit", "resource", "channel head overflows")
        })?;
        let len = u64::try_from(len).map_err(|_| {
            RuntimeError::new("allocation_limit", "resource", "channel length overflows")
        })?;
        let mut fields = [0_u8; 24];
        fields[..8].copy_from_slice(&head.to_le_bytes());
        fields[8..16].copy_from_slice(&len.to_le_bytes());
        fields[16..24].copy_from_slice(&u64::from(closed).to_le_bytes());
        self.write_storage_range_if_version(handle, 40, &fields, expected_version)
    }

    fn local_lease_contains(&self, handle: Handle) -> bool {
        LOCAL_LEASES.with(|leases| {
            leases
                .borrow()
                .get(&self.worker_id)
                .is_some_and(|lease| lease.contains(handle))
        })
    }

    fn local_lease_size(&self, handle: Handle) -> Option<u32> {
        LOCAL_LEASES.with(|leases| {
            leases
                .borrow()
                .get(&self.worker_id)
                .and_then(|lease| lease.size(handle))
        })
    }

    fn local_lease_metadata(&self, handle: Handle) -> Option<StorageObjectMeta> {
        LOCAL_LEASES.with(|leases| {
            leases
                .borrow()
                .get(&self.worker_id)
                .and_then(|lease| lease.metadata(handle))
        })
    }

    pub(crate) fn begin_allocation_scope(&self) -> AllocationScope<'_> {
        let start = ALLOCATION_JOURNAL.with(|journal| {
            let mut journal = journal.borrow_mut();
            journal.depth += 1;
            journal.next
        });
        AllocationScope {
            heap: self,
            start,
            had_lease: LOCAL_LEASES.with(|leases| leases.borrow().contains_key(&self.worker_id)),
            committed: false,
            _worker: std::marker::PhantomData,
        }
    }

    fn record_allocation(&self, handle: Handle) -> Handle {
        ALLOCATION_JOURNAL.with(|journal| {
            let mut journal = journal.borrow_mut();
            if journal.depth != 0 {
                let sequence = journal.next;
                journal.next += 1;
                journal.objects.push((self.worker_id, sequence, handle));
            }
        });
        handle
    }

    fn allocate_storage_box(
        &self,
        bytes: &[u8],
        references: &[(u32, StorageRef)],
        kind: crate::managed_storage::StoragePayloadKind,
        typ: Option<crate::types::TypeIdentity>,
    ) -> Result<StorageRef, RuntimeError> {
        let stride = u32::try_from(bytes.len().max(1))
            .map_err(|_| RuntimeError::new("allocation_limit", "heap", "prepared box too large"))?;
        let layout = self.storage.register_layout(StorageLayout {
            stride,
            references: references.iter().map(|(offset, _)| *offset).collect(),
            dynamics: Vec::new(),
        })?;
        let logical = if kind == crate::managed_storage::StoragePayloadKind::Record
            && bytes.len() >= 16
            && bytes[..4] == *b"DF02"
        {
            let callback_len = u32::from_le_bytes(bytes[8..12].try_into().unwrap()) as usize;
            let captures = if callback_len >= 32 && bytes.len() >= 16 + callback_len {
                u32::from_le_bytes(bytes[16 + 28..16 + 32].try_into().unwrap()) as u64
            } else {
                0
            };
            crate::contract_generated::STORAGE_RECORD_HEADER
                .saturating_add(crate::contract_generated::STORAGE_DYNAMIC)
                .saturating_add(
                    captures.saturating_mul(crate::contract_generated::STORAGE_REFERENCE),
                )
        } else if kind == crate::managed_storage::StoragePayloadKind::Record
            && bytes.len() >= 32
            && bytes[..4] == *b"FN02"
        {
            // FN02 keeps the numeric code/revision header and 24-byte
            // capture descriptors in the physical payload, while the shared
            // logical contract charges one record header, one code anchor,
            // and one reference lane per capture.
            let captures = u64::from(u32::from_le_bytes(bytes[28..32].try_into().unwrap()));
            crate::contract_generated::STORAGE_RECORD_HEADER
                .saturating_add(crate::contract_generated::STORAGE_REFERENCE)
                .saturating_add(
                    captures.saturating_mul(crate::contract_generated::STORAGE_REFERENCE),
                )
        } else if kind == crate::managed_storage::StoragePayloadKind::Record
            && bytes.len() >= 16
            && bytes[..4] == *b"MD02"
        {
            // A dynamic descriptor may own a bound-method record as its
            // backing object.  Charge the same compact logical shape as a
            // direct method record; the physical MD02 bytes also contain
            // embedded prepared lanes and are intentionally wider.
            let function_len = u32::from_le_bytes(bytes[4..8].try_into().unwrap()) as usize;
            let receiver_len = u32::from_le_bytes(bytes[8..12].try_into().unwrap()) as usize;
            let captures = if function_len >= 32 && bytes.len() >= 16 + function_len {
                u32::from_le_bytes(bytes[16 + 28..16 + 32].try_into().unwrap()) as u64
            } else {
                0
            };
            crate::contract_generated::STORAGE_RECORD_HEADER
                .saturating_add(crate::contract_generated::STORAGE_REFERENCE)
                .saturating_add(
                    captures.saturating_mul(crate::contract_generated::STORAGE_REFERENCE),
                )
                .saturating_add(if receiver_len != 0 {
                    crate::contract_generated::STORAGE_DYNAMIC
                } else {
                    0
                })
        } else if kind == crate::managed_storage::StoragePayloadKind::Record
            && bytes.len() >= 8
            && bytes[..4] == *b"RS01"
        {
            // RS01 is the resource identity.  Its channel state and ring
            // metadata are physical implementation details; the shared
            // value contract charges one dynamic descriptor payload.
            crate::contract_generated::STORAGE_RECORD_HEADER
                .saturating_add(crate::contract_generated::STORAGE_DYNAMIC)
        } else {
            crate::contract_generated::STORAGE_RECORD_HEADER
                .saturating_add(u64::try_from(bytes.len()).unwrap_or(u64::MAX))
        };
        let metadata = StorageObjectMeta {
            logical_bytes: logical,
            typ,
            initialized: true,
            payload_len: bytes.len() as u32,
            kind,
            version: 1,
        };
        if stride > crate::managed_storage::TLAB_MAX_OBJECT_BYTES {
            self.flush_worker_lease()?;
            return self
                .storage
                .reserve_direct(layout, 1, logical, bytes, metadata)
                .map(|handle| self.record_allocation(handle));
        }
        let available = self.max_bytes.saturating_sub(self.stats().guest_bytes);
        let lease_credit = available.min(64 << 10).max(logical);
        let mut lease = LOCAL_LEASES
            .with(|leases| leases.borrow_mut().remove(&self.worker_id))
            .map(Ok)
            .unwrap_or_else(|| self.storage.acquire(lease_credit))?;
        let handle = match lease.reserve_prepared(layout, stride, 1, logical) {
            Ok(handle) => handle,
            Err(error) if error.code == "collection_required" => {
                lease.flush()?;
                lease = self.storage.acquire(lease_credit)?;
                lease.reserve_prepared(layout, stride, 1, logical)?
            }
            Err(error) => {
                LOCAL_LEASES.with(|leases| {
                    leases.borrow_mut().insert(self.worker_id, lease);
                });
                return Err(error);
            }
        };
        if let Err(error) = lease
            .write(handle, 0, bytes)
            .and_then(|()| lease.set_metadata(handle, metadata))
            .and_then(|()| lease.commit(handle))
        {
            let _ = lease.abort(handle);
            LOCAL_LEASES.with(|leases| {
                leases.borrow_mut().insert(self.worker_id, lease);
            });
            return Err(error);
        }
        LOCAL_LEASES.with(|leases| {
            leases.borrow_mut().insert(self.worker_id, lease);
        });
        Ok(self.record_allocation(handle))
    }

    fn allocate_prepared_box(
        &self,
        bytes: &[u8],
        references: &[(u32, StorageRef)],
    ) -> Result<StorageRef, RuntimeError> {
        self.allocate_storage_box(
            bytes,
            references,
            crate::managed_storage::StoragePayloadKind::Typed,
            None,
        )
    }

    pub(crate) fn allocate_raw_bytes(&self, bytes: &[u8]) -> Result<StorageRef, RuntimeError> {
        self.allocate_storage_box(
            bytes,
            &[],
            crate::managed_storage::StoragePayloadKind::RawBytes,
            Some(crate::types::TypeIdentity::Any),
        )
    }

    fn encode_function_record(
        &self,
        function: &crate::value::FunctionValue,
        registry: &TypeRegistry,
    ) -> Result<PreparedPayload, RuntimeError> {
        let captures = u32::try_from(function.captures.len()).map_err(|_| {
            RuntimeError::new("allocation_limit", "heap", "too many function captures")
        })?;
        let index = function.index.ok_or_else(|| {
            RuntimeError::new(
                "invalid_function",
                "heap",
                "managed function has no numeric code id",
            )
        })?;
        let mut bytes = Vec::with_capacity(32 + function.captures.len() * 24);
        bytes.extend_from_slice(b"FN02");
        bytes.extend_from_slice(&(index as u64).to_le_bytes());
        bytes.extend_from_slice(
            &function
                .revision_generation
                .unwrap_or(u64::MAX)
                .to_le_bytes(),
        );
        bytes.extend_from_slice(
            &function
                .revision_anchor
                .map(StorageRef::identity_for_payload)
                .unwrap_or_default()
                .to_le_bytes(),
        );
        bytes.extend_from_slice(&captures.to_le_bytes());
        let mut references = Vec::with_capacity(function.captures.len() + 1);
        if let Some(anchor) = function.revision_anchor.filter(|handle| handle.valid()) {
            references.push((20, anchor));
        }
        for capture in &function.captures {
            let pointer = self.prepared_pointer_payload(capture, registry)?;
            if pointer.bytes.len() != 24 {
                return Err(RuntimeError::new(
                    "typed_payload_invalid",
                    "heap",
                    "function capture pointer has invalid width",
                ));
            }
            let offset = u32::try_from(bytes.len()).map_err(|_| {
                RuntimeError::new("allocation_limit", "heap", "function record is too large")
            })?;
            bytes.extend_from_slice(&pointer.bytes);
            references.push((offset, capture.root));
        }
        Ok(PreparedPayload { bytes, references })
    }

    fn prepared_function_payload(
        &self,
        function: &crate::value::FunctionValue,
        registry: &TypeRegistry,
    ) -> Result<PreparedPayload, RuntimeError> {
        self.encode_function_record(function, registry)
    }

    fn prepared_dynamic_function_payload(
        &self,
        value: &crate::value::Value,
        registry: &TypeRegistry,
    ) -> Result<PreparedPayload, RuntimeError> {
        let crate::value::Data::DynamicFunction(callback) = &value.data else {
            return Err(RuntimeError::new(
                "typed_payload_unsupported",
                "heap",
                "value is not a dynamic function",
            ));
        };
        let crate::value::Data::Function(function) = &callback.data else {
            return Err(RuntimeError::new(
                "typed_payload_unsupported",
                "heap",
                "dynamic function callback is not executable",
            ));
        };
        let callback_type = callback.typ.clone();
        let callback = self.prepared_function_payload(function, registry)?;
        // An erased Any/interface lane keeps its declaration in `value.typ`,
        // while the callback carries the MakeFunc signature. Persist that
        // nested signature so a later snapshot can round-trip the dynamic
        // function without changing the slot's erased type.
        let outer_type = if value.typ == crate::types::TypeIdentity::Any {
            &callback_type
        } else {
            &value.typ
        };
        let outer_type_id = registry.storage_type_id(outer_type)?;
        let callback_len = u32::try_from(callback.bytes.len()).map_err(|_| {
            RuntimeError::new(
                "allocation_limit",
                "heap",
                "dynamic function callback is too large",
            )
        })?;
        let mut bytes = Vec::with_capacity(16 + callback.bytes.len());
        bytes.extend_from_slice(b"DF02");
        bytes.extend_from_slice(&outer_type_id.to_le_bytes());
        bytes.extend_from_slice(&callback_len.to_le_bytes());
        bytes.extend_from_slice(&0_u32.to_le_bytes());
        let callback_base = bytes.len() as u32;
        bytes.extend_from_slice(&callback.bytes);
        let references = callback
            .references
            .into_iter()
            .map(|(offset, handle)| (callback_base + offset, handle))
            .collect();
        Ok(PreparedPayload { bytes, references })
    }

    fn prepared_method_payload(
        &self,
        function: &crate::value::FunctionValue,
        receiver: Option<&crate::value::Value>,
        registry: &TypeRegistry,
    ) -> Result<PreparedPayload, RuntimeError> {
        // Keep the function and receiver descriptors in one method record.
        // MD02 embeds both prepared lanes and shifts their managed edges into
        // this record, so a bound method has one owner allocation.
        let function_payload = self.encode_function_record(function, registry)?;
        let receiver_payload = receiver
            .map(|receiver| self.prepared_payload_inner(receiver, false))
            .transpose()?;
        let receiver_type = receiver
            .map(|receiver| registry.storage_type_id(&receiver.typ))
            .transpose()?
            .unwrap_or_default();
        let function_len = u32::try_from(function_payload.bytes.len())
            .map_err(|_| RuntimeError::new("allocation_limit", "heap", "method is too large"))?;
        // MD02 uses a zero receiver length for an unbound method.  Preserve
        // the distinction for a bound zero-sized receiver (for example a
        // method on `struct{}`), otherwise decoding silently turns the bound
        // method into an unbound function that requires an extra argument.
        let receiver_len =
            receiver_payload
                .as_ref()
                .map_or(Ok::<u32, RuntimeError>(0), |payload| {
                    let length = u32::try_from(payload.bytes.len()).map_err(|_| {
                        RuntimeError::new(
                            "allocation_limit",
                            "heap",
                            "method receiver is too large",
                        )
                    })?;
                    Ok(if length == 0 { u32::MAX } else { length })
                })?;
        let receiver_bytes = receiver_payload
            .as_ref()
            .map_or(0, |payload| payload.bytes.len());
        let mut bytes = Vec::with_capacity(16 + function_payload.bytes.len() + receiver_bytes);
        bytes.extend_from_slice(b"MD02");
        bytes.extend_from_slice(&function_len.to_le_bytes());
        bytes.extend_from_slice(&receiver_len.to_le_bytes());
        bytes.extend_from_slice(&receiver_type.to_le_bytes());
        let function_base = bytes.len() as u32;
        bytes.extend_from_slice(&function_payload.bytes);
        let receiver_base = bytes.len() as u32;
        if let Some(payload) = receiver_payload {
            bytes.extend_from_slice(&payload.bytes);
            let mut references = function_payload
                .references
                .into_iter()
                .map(|(offset, handle)| (function_base + offset, handle))
                .collect::<Vec<_>>();
            references.extend(
                payload
                    .references
                    .into_iter()
                    .map(|(offset, handle)| (receiver_base + offset, handle)),
            );
            return Ok(PreparedPayload { bytes, references });
        }
        Ok(PreparedPayload {
            bytes,
            references: function_payload
                .references
                .into_iter()
                .map(|(offset, handle)| (function_base + offset, handle))
                .collect(),
        })
    }

    fn prepared_resource_payload(
        &self,
        resource: &crate::instance::scheduler::Resource,
    ) -> Result<PreparedPayload, RuntimeError> {
        let (bytes, references) = match resource {
            crate::instance::scheduler::Resource::Mutex {
                locked,
                grant,
                waiters,
            } => {
                let mut bytes = Vec::with_capacity(24 + waiters.len() * 8);
                bytes.extend_from_slice(b"RS01");
                bytes.extend_from_slice(&0_u32.to_le_bytes());
                bytes.extend_from_slice(&u64::from(*locked).to_le_bytes());
                bytes.extend_from_slice(&grant.unwrap_or(u64::MAX).to_le_bytes());
                bytes.extend_from_slice(&(waiters.len() as u32).to_le_bytes());
                bytes.extend_from_slice(&0_u32.to_le_bytes());
                for waiter in waiters {
                    bytes.extend_from_slice(&waiter.to_le_bytes());
                }
                (bytes, Vec::new())
            }
            crate::instance::scheduler::Resource::Channel { channel } => {
                let channel = channel
                    .lock()
                    .map_err(|_| RuntimeError::new("runtime", "heap", "channel lock poisoned"))?;
                let mut bytes = Vec::with_capacity(64);
                bytes.extend_from_slice(b"RS01");
                bytes.extend_from_slice(&1_u32.to_le_bytes());
                bytes.extend_from_slice(&(channel.layout() as u64).to_le_bytes());
                bytes.extend_from_slice(&(channel.stride() as u64).to_le_bytes());
                bytes.extend_from_slice(&channel.logical_bytes().to_le_bytes());
                bytes.extend_from_slice(&(channel.capacity() as u64).to_le_bytes());
                bytes.extend_from_slice(&(channel.head() as u64).to_le_bytes());
                bytes.extend_from_slice(&(channel.len() as u64).to_le_bytes());
                bytes.extend_from_slice(&u64::from(channel.is_closed()).to_le_bytes());
                bytes.extend_from_slice(&channel.root().identity_for_payload().to_le_bytes());
                let references = if channel.root().valid() {
                    vec![(bytes.len() as u32 - 8, channel.root())]
                } else {
                    Vec::new()
                };
                (bytes, references)
            }
        };
        // RS01 is the resource identity itself. Keeping it as the outer
        // managed record avoids an RS01 box plus an Any wrapper, and leaves
        // the channel ring as the sole separate payload allocation.
        Ok(PreparedPayload { bytes, references })
    }

    fn prepared_record_payload(
        &self,
        value: &crate::value::Value,
        registry: &TypeRegistry,
    ) -> Result<PreparedPayload, RuntimeError> {
        match &value.data {
            crate::value::Data::Function(function) => {
                self.prepared_function_payload(function, registry)
            }
            crate::value::Data::Method { function, receiver } => {
                self.prepared_method_payload(function, receiver.as_deref(), registry)
            }
            crate::value::Data::Resource(resource) => self.prepared_resource_payload(resource),
            _ => Err(RuntimeError::new(
                "typed_payload_unsupported",
                "heap",
                "value is not a managed record",
            )),
        }
    }

    fn prepared_pointer_payload(
        &self,
        address: &crate::value::Address,
        registry: &TypeRegistry,
    ) -> Result<PreparedPayload, RuntimeError> {
        // A pointer is an address into an already managed prepared object. Its
        // persistent representation never owns a second path record: the
        // ticket, physical byte offset and view type are enough for the
        // direct field/index readers.  Host paths stay at the boundary only.
        if address.path.is_empty()
            && let Some((offset, type_id, flags)) = address.identity.numeric
        {
            let mut bytes = vec![0_u8; 24];
            bytes[0..8].copy_from_slice(&address.root.identity_for_payload().to_le_bytes());
            bytes[8..16].copy_from_slice(&offset.to_le_bytes());
            bytes[16..20].copy_from_slice(&type_id.to_le_bytes());
            bytes[20..24].copy_from_slice(&flags.to_le_bytes());
            return Ok(PreparedPayload {
                bytes,
                references: address
                    .root
                    .valid()
                    .then_some((0, address.root))
                    .into_iter()
                    .collect(),
            });
        }
        let pointer_root = address.root;
        let mut compact_bytes = false;
        if let Ok(metadata) = self.meta(address.root) {
            compact_bytes = metadata.kind == crate::managed_storage::StoragePayloadKind::RawBytes;
            if !compact_bytes
                && metadata.typ == Some(crate::types::TypeIdentity::Any)
                && metadata.payload_len == 24
            {
                let mut descriptor = [0_u8; 24];
                self.read_storage_into(address.root, 0, &mut descriptor)?;
                compact_bytes = descriptor[4..8] == crate::managed_storage::VALUE_REF.to_le_bytes()
                    && descriptor[20..24] == crate::managed_storage::VALUE_REF_BYTES.to_le_bytes();
            }
        }
        let mut offset = 0_u64;
        let mut view_type = self
            .meta(address.root)
            .ok()
            .and_then(|meta| meta.typ)
            .unwrap_or(crate::types::TypeIdentity::Any);
        // A reflected field keeps the compact numeric location of its
        // parent pointer and appends the new path relative to that lane.
        // Starting the path walk from the root type would interpret
        // `Field("Number")` as a field of the outer Record after the
        // `embedded` lane has already been encoded, which produces a false
        // unknown-field error.  Continue from the stored offset/type ID.
        if let Some((base_offset, type_id, flags)) = address.identity.numeric {
            offset = base_offset;
            view_type = registry
                .identity_for_storage_type_id(type_id)
                .ok_or_else(|| {
                    RuntimeError::new("unknown_type", "pointer", "unknown numeric view type")
                })?;
            compact_bytes = flags == POINTER_COMPACT_BYTES;
        }
        for segment in &address.path {
            match segment {
                crate::value::PathElement::Field(name) => {
                    let layout_type = registry.underlying(&view_type)?;
                    let Some(crate::types::StorageTypeLayout::Struct { fields }) =
                        registry.storage_layout(&layout_type)?
                    else {
                        return Err(RuntimeError::new(
                            "invalid_address",
                            "pointer",
                            "field path does not resolve to a struct",
                        ));
                    };
                    let Some(index) = fields.iter().position(|(field, _)| field == name) else {
                        return Err(RuntimeError::new(
                            "invalid_address",
                            "pointer",
                            "unknown struct field",
                        ));
                    };
                    for (_, preceding) in &fields[..index] {
                        offset = offset
                            .checked_add(u64::from(registry.storage_lane_layout(preceding)?.stride))
                            .ok_or_else(|| {
                                RuntimeError::new(
                                    "allocation_limit",
                                    "pointer",
                                    "pointer offset overflows",
                                )
                            })?;
                    }
                    view_type = fields[index].1.clone();
                }
                crate::value::PathElement::Index(index) => {
                    if compact_bytes && view_type == crate::types::TypeIdentity::Any {
                        offset = offset.checked_add(*index as u64).ok_or_else(|| {
                            RuntimeError::new(
                                "allocation_limit",
                                "pointer",
                                "pointer offset overflows",
                            )
                        })?;
                        view_type = crate::types::TypeIdentity::Primitive(
                            crate::contract_generated::PrimitiveUint8,
                        );
                        continue;
                    }
                    let layout_type = registry.underlying(&view_type)?;
                    match registry.storage_layout(&layout_type)? {
                        Some(crate::types::StorageTypeLayout::Array { element, length }) => {
                            if *index >= length {
                                return Err(RuntimeError::new(
                                    "invalid_address",
                                    "pointer",
                                    "array index is outside the backing value",
                                ));
                            }
                            let stride = if compact_bytes {
                                Self::compact_byte_width(&element, registry)? as u64
                            } else {
                                u64::from(registry.storage_lane_layout(&element)?.stride)
                            };
                            offset = offset
                                .checked_add((*index as u64).checked_mul(stride).ok_or_else(
                                    || {
                                        RuntimeError::new(
                                            "allocation_limit",
                                            "pointer",
                                            "pointer offset overflows",
                                        )
                                    },
                                )?)
                                .ok_or_else(|| {
                                    RuntimeError::new(
                                        "allocation_limit",
                                        "pointer",
                                        "pointer offset overflows",
                                    )
                                })?;
                            view_type = element;
                        }
                        Some(crate::types::StorageTypeLayout::Struct { fields }) => {
                            let Some((_, field_type)) = fields.get(*index) else {
                                return Err(RuntimeError::new(
                                    "invalid_address",
                                    "pointer",
                                    "struct index is outside the backing value",
                                ));
                            };
                            for (_, preceding) in &fields[..*index] {
                                offset = offset
                                    .checked_add(u64::from(
                                        registry.storage_lane_layout(preceding)?.stride,
                                    ))
                                    .ok_or_else(|| {
                                        RuntimeError::new(
                                            "allocation_limit",
                                            "pointer",
                                            "pointer offset overflows",
                                        )
                                    })?;
                            }
                            view_type = field_type.clone();
                        }
                        _ => {
                            return Err(RuntimeError::new(
                                "invalid_address",
                                "pointer",
                                "index path does not resolve to an aggregate",
                            ));
                        }
                    }
                }
                crate::value::PathElement::ArrayView { start, typ, .. } => {
                    let stride = if compact_bytes {
                        1
                    } else {
                        let element = match registry
                            .storage_layout(&registry.underlying(&view_type)?)?
                        {
                            Some(crate::types::StorageTypeLayout::Array { element, .. }) => element,
                            _ => view_type.clone(),
                        };
                        u64::from(registry.storage_lane_layout(&element)?.stride)
                    };
                    offset = offset
                        .checked_add((*start as u64).checked_mul(stride).ok_or_else(|| {
                            RuntimeError::new(
                                "allocation_limit",
                                "pointer",
                                "pointer offset overflows",
                            )
                        })?)
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "allocation_limit",
                                "pointer",
                                "pointer offset overflows",
                            )
                        })?;
                    view_type = typ.clone();
                }
                crate::value::PathElement::TypeView(typ) => view_type = typ.clone(),
            }
        }
        let type_id = registry.storage_type_id(&view_type)?;
        let mut bytes = vec![0_u8; 24];
        bytes[0..8].copy_from_slice(&pointer_root.identity_for_payload().to_le_bytes());
        bytes[8..16].copy_from_slice(&offset.to_le_bytes());
        bytes[16..20].copy_from_slice(&type_id.to_le_bytes());
        if compact_bytes {
            bytes[20..24].copy_from_slice(&POINTER_COMPACT_BYTES.to_le_bytes());
        }
        Ok(PreparedPayload {
            bytes,
            references: pointer_root
                .valid()
                .then_some((0, pointer_root))
                .into_iter()
                .collect(),
        })
    }

    pub(crate) fn compact_byte_width(
        typ: &crate::types::TypeIdentity,
        registry: &TypeRegistry,
    ) -> Result<usize, RuntimeError> {
        let underlying = registry.underlying(typ)?;
        match underlying {
            crate::types::TypeIdentity::Primitive(crate::contract_generated::PrimitiveUint8) => {
                Ok(1)
            }
            crate::types::TypeIdentity::Structural { .. } => {
                let Some(crate::types::StorageTypeLayout::Array { element, length }) =
                    registry.storage_layout(&underlying)?
                else {
                    return Err(RuntimeError::new(
                        "typed_payload_invalid",
                        "pointer",
                        "compact byte pointer requires an uint8 array",
                    ));
                };
                Self::compact_byte_width(&element, registry)?
                    .checked_mul(length)
                    .ok_or_else(|| {
                        RuntimeError::new("allocation_limit", "pointer", "byte view overflows")
                    })
            }
            _ => Err(RuntimeError::new(
                "typed_payload_invalid",
                "pointer",
                "compact byte pointer requires an uint8 view",
            )),
        }
    }

    fn decode_compact_byte_value(
        &self,
        typ: &crate::types::TypeIdentity,
        bytes: &[u8],
        registry: &TypeRegistry,
    ) -> Result<Value, RuntimeError> {
        let underlying = registry.underlying(typ)?;
        match underlying {
            crate::types::TypeIdentity::Primitive(crate::contract_generated::PrimitiveUint8) => {
                if bytes.len() != 1 {
                    return Err(RuntimeError::new(
                        "typed_payload_invalid",
                        "pointer",
                        "compact byte lane width mismatch",
                    ));
                }
                Ok(Value {
                    typ: typ.clone(),
                    data: crate::value::Data::Unsigned(u64::from(bytes[0])),
                })
            }
            crate::types::TypeIdentity::Structural { .. } => {
                let Some(crate::types::StorageTypeLayout::Array { element, length }) =
                    registry.storage_layout(&underlying)?
                else {
                    return Err(RuntimeError::new(
                        "typed_payload_invalid",
                        "pointer",
                        "compact byte pointer requires an uint8 array",
                    ));
                };
                let element_width = Self::compact_byte_width(&element, registry)?;
                if bytes.len() != element_width.saturating_mul(length) {
                    return Err(RuntimeError::new(
                        "typed_payload_invalid",
                        "pointer",
                        "compact byte aggregate width mismatch",
                    ));
                }
                let layout = registry.storage_lane_layout(typ)?;
                let mut lanes = Vec::with_capacity(layout.stride as usize);
                for byte in bytes {
                    lanes.extend(u64::from(*byte).to_le_bytes());
                }
                self.decode_prepared_lane(typ, &layout, &lanes)
            }
            _ => Err(RuntimeError::new(
                "typed_payload_invalid",
                "pointer",
                "compact byte pointer requires an uint8 view",
            )),
        }
    }

    fn encode_compact_byte_value(
        typ: &crate::types::TypeIdentity,
        value: &Value,
        registry: &TypeRegistry,
    ) -> Result<Vec<u8>, RuntimeError> {
        let underlying = registry.underlying(typ)?;
        match underlying {
            crate::types::TypeIdentity::Primitive(crate::contract_generated::PrimitiveUint8) => {
                match value.data {
                    crate::value::Data::Unsigned(byte) if byte <= u64::from(u8::MAX) => {
                        Ok(vec![byte as u8])
                    }
                    crate::value::Data::Integer(byte)
                        if (0..=i64::from(u8::MAX)).contains(&byte) =>
                    {
                        Ok(vec![byte as u8])
                    }
                    _ => Err(RuntimeError::new(
                        "type_error",
                        "pointer",
                        "compact byte pointer requires uint8 values",
                    )),
                }
            }
            crate::types::TypeIdentity::Structural { .. } => {
                let Some(crate::types::StorageTypeLayout::Array { element, length }) =
                    registry.storage_layout(&underlying)?
                else {
                    return Err(RuntimeError::new(
                        "type_error",
                        "pointer",
                        "compact byte pointer requires an uint8 array",
                    ));
                };
                let Some((true, count)) = value.data.aggregate_shape() else {
                    return Err(RuntimeError::new(
                        "type_error",
                        "pointer",
                        "compact byte pointer requires an array value",
                    ));
                };
                if count != length {
                    return Err(RuntimeError::new(
                        "type_error",
                        "pointer",
                        "compact byte array length mismatch",
                    ));
                }
                let mut bytes = Vec::with_capacity(Self::compact_byte_width(typ, registry)?);
                for index in 0..length {
                    bytes.extend(Self::encode_compact_byte_value(
                        &element,
                        &value.element(index)?,
                        registry,
                    )?);
                }
                Ok(bytes)
            }
            _ => Err(RuntimeError::new(
                "type_error",
                "pointer",
                "compact byte pointer requires an uint8 view",
            )),
        }
    }

    fn decode_function_bytes(
        &self,
        bytes: &[u8],
        typ: crate::types::TypeIdentity,
    ) -> Result<crate::value::Value, RuntimeError> {
        if bytes.len() < 32 || bytes[..4] != *b"FN02" {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "heap",
                "function record is invalid",
            ));
        }
        let index = u64::from_le_bytes(bytes[4..12].try_into().unwrap());
        let generation = u64::from_le_bytes(bytes[12..20].try_into().unwrap());
        let anchor = StorageRef::from_payload_identity(u64::from_le_bytes(
            bytes[20..28].try_into().unwrap(),
        ));
        let captures = u32::from_le_bytes(bytes[28..32].try_into().unwrap()) as usize;
        let mut cursor = 32usize;
        let mut captures_out = Vec::with_capacity(captures);
        for _ in 0..captures {
            let capture_end = cursor.checked_add(24).ok_or_else(|| {
                RuntimeError::new(
                    "typed_payload_invalid",
                    "heap",
                    "function capture is truncated",
                )
            })?;
            let payload = bytes.get(cursor..capture_end).ok_or_else(|| {
                RuntimeError::new(
                    "typed_payload_invalid",
                    "heap",
                    "function capture is truncated",
                )
            })?;
            let root = StorageRef::from_payload_identity(u64::from_le_bytes(
                payload[0..8].try_into().unwrap(),
            ));
            let offset = u64::from_le_bytes(payload[8..16].try_into().unwrap());
            let type_id = u32::from_le_bytes(payload[16..20].try_into().unwrap());
            let flags = u32::from_le_bytes(payload[20..24].try_into().unwrap());
            cursor = capture_end;
            captures_out.push(crate::value::Address {
                root,
                path: Vec::new(),
                identity: Arc::default(),
            });
            if let Some(address) = captures_out.last_mut() {
                Arc::make_mut(&mut address.identity).numeric = Some((offset, type_id, flags));
            }
        }
        Ok(crate::value::Value {
            typ,
            data: crate::value::Data::Function(Arc::new(crate::value::FunctionValue {
                index: (index != u64::MAX).then_some(index as usize),
                revision: None,
                revision_generation: (generation != u64::MAX).then_some(generation),
                revision_anchor: anchor.valid().then_some(anchor),
                // Names are immutable Program metadata.  Managed records carry
                // only the numeric code id and revision generation; the owner
                // resolves these fields before dispatch or reflection.
                module: Arc::default(),
                function: Arc::default(),
                captures: captures_out,
            })),
        })
    }

    /// Decode a MakeFunc value.  Its public function signature and its
    /// executable callback are separate values: losing the outer wrapper and
    /// decoding only the callback turns a dynamic callback into an ordinary
    /// function and bypasses `start_dynamic_callback` on the next Call.
    fn decode_dynamic_function_bytes(
        &self,
        bytes: &[u8],
        typ: crate::types::TypeIdentity,
    ) -> Result<crate::value::Value, RuntimeError> {
        if bytes.len() < 16 || bytes[..4] != *b"DF02" {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "heap",
                "dynamic function record is invalid",
            ));
        }
        let outer_type_id = u32::from_le_bytes(bytes[4..8].try_into().unwrap());
        let callback_len = u32::from_le_bytes(bytes[8..12].try_into().unwrap()) as usize;
        let callback_end = 16usize.checked_add(callback_len).ok_or_else(|| {
            RuntimeError::new(
                "typed_payload_invalid",
                "heap",
                "dynamic function callback overflows",
            )
        })?;
        let callback = bytes.get(16..callback_end).ok_or_else(|| {
            RuntimeError::new(
                "typed_payload_invalid",
                "heap",
                "dynamic function callback is truncated",
            )
        })?;
        let registry =
            self.type_registry.read().unwrap().clone().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
        let outer_type = registry
            .identity_for_storage_type_id(outer_type_id)
            .ok_or_else(|| {
                RuntimeError::new(
                    "unknown_type",
                    "heap",
                    format!("dynamic function type ID {outer_type_id} is missing"),
                )
            })?;
        let callback = self.decode_function_bytes(callback, outer_type.clone())?;
        Ok(crate::value::Value {
            // Keep the declared lane type. The callback's concrete signature
            // is retained by the nested Function value and is used whenever
            // an erased Any lane is asserted or re-encoded.
            typ,
            data: crate::value::Data::DynamicFunction(Arc::new(callback)),
        })
    }

    fn decode_method_payload(
        &self,
        bytes: &[u8],
        typ: crate::types::TypeIdentity,
    ) -> Result<crate::value::Value, RuntimeError> {
        if bytes.len() >= 16 && bytes[..4] == *b"MD02" {
            let function_len = u32::from_le_bytes(bytes[4..8].try_into().unwrap()) as usize;
            let encoded_receiver_len = u32::from_le_bytes(bytes[8..12].try_into().unwrap());
            let receiver_present = encoded_receiver_len != 0;
            let receiver_len = if encoded_receiver_len == u32::MAX {
                0
            } else {
                encoded_receiver_len as usize
            };
            let receiver_type = u32::from_le_bytes(bytes[12..16].try_into().unwrap());
            let function_end = 16usize.checked_add(function_len).ok_or_else(|| {
                RuntimeError::new("typed_payload_invalid", "heap", "method function overflows")
            })?;
            let receiver_end = function_end.checked_add(receiver_len).ok_or_else(|| {
                RuntimeError::new("typed_payload_invalid", "heap", "method receiver overflows")
            })?;
            if receiver_end > bytes.len() || function_len < 32 {
                return Err(RuntimeError::new(
                    "typed_payload_invalid",
                    "heap",
                    "method record is truncated",
                ));
            }
            let function = self.decode_function_bytes(&bytes[16..function_end], typ.clone())?;
            let crate::value::Data::Function(function) = function.data else {
                unreachable!()
            };
            let receiver = if !receiver_present {
                None
            } else {
                let receiver_type = self
                    .type_registry
                    .read()
                    .unwrap()
                    .as_ref()
                    .and_then(|registry| registry.identity_for_storage_type_id(receiver_type))
                    .ok_or_else(|| {
                        RuntimeError::new("typed_payload_invalid", "heap", "unknown receiver type")
                    })?;
                Some(Arc::new(self.decode_prepared_value(
                    &receiver_type,
                    &bytes[function_end..receiver_end],
                )?))
            };
            return Ok(crate::value::Value {
                typ,
                data: crate::value::Data::Method { function, receiver },
            });
        }
        if bytes.len() < 16 || bytes[..4] != *b"MD02" {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "heap",
                "method record is invalid",
            ));
        }
        Err(RuntimeError::new(
            "typed_payload_invalid",
            "heap",
            "method record is truncated",
        ))
    }

    fn decode_resource_payload(
        &self,
        bytes: &[u8],
        typ: crate::types::TypeIdentity,
    ) -> Result<crate::value::Value, RuntimeError> {
        if bytes.len() < 8 || bytes[..4] != *b"RS01" {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "heap",
                "resource record is invalid",
            ));
        }
        let variant = u32::from_le_bytes(bytes[4..8].try_into().unwrap());
        let resource = match variant {
            0 => {
                if bytes.len() < 32 {
                    return Err(RuntimeError::new(
                        "typed_payload_invalid",
                        "heap",
                        "mutex record is truncated",
                    ));
                }
                let locked = u64::from_le_bytes(bytes[8..16].try_into().unwrap()) != 0;
                let grant = u64::from_le_bytes(bytes[16..24].try_into().unwrap());
                let count = u32::from_le_bytes(bytes[24..28].try_into().unwrap()) as usize;
                let end = 32usize
                    .checked_add(count.checked_mul(8).ok_or_else(|| {
                        RuntimeError::new("allocation_limit", "heap", "mutex record is too large")
                    })?)
                    .ok_or_else(|| {
                        RuntimeError::new("allocation_limit", "heap", "mutex record is too large")
                    })?;
                let mut waiters = std::collections::VecDeque::with_capacity(count);
                let waiters_bytes = bytes.get(32..end).ok_or_else(|| {
                    RuntimeError::new("typed_payload_invalid", "heap", "mutex record is truncated")
                })?;
                for chunk in waiters_bytes.as_chunks::<8>().0 {
                    waiters.push_back(u64::from_le_bytes(*chunk));
                }
                crate::instance::scheduler::Resource::from_page_mutex(
                    locked,
                    (grant != u64::MAX).then_some(grant),
                    waiters,
                )
            }
            1 => {
                if bytes.len() < 72 {
                    return Err(RuntimeError::new(
                        "typed_payload_invalid",
                        "heap",
                        "channel record is truncated",
                    ));
                }
                let ring = StorageRef::from_payload_identity(u64::from_le_bytes(
                    bytes[64..72].try_into().unwrap(),
                ));
                crate::instance::scheduler::Resource::from_page_channel(
                    &self.storage_pool(),
                    u64::from_le_bytes(bytes[8..16].try_into().unwrap()) as u32,
                    u64::from_le_bytes(bytes[16..24].try_into().unwrap()) as u32,
                    u64::from_le_bytes(bytes[24..32].try_into().unwrap()),
                    u64::from_le_bytes(bytes[32..40].try_into().unwrap()) as usize,
                    ring,
                    u64::from_le_bytes(bytes[40..48].try_into().unwrap()) as usize,
                    u64::from_le_bytes(bytes[48..56].try_into().unwrap()) as usize,
                    u64::from_le_bytes(bytes[56..64].try_into().unwrap()) != 0,
                )?
            }
            _ => {
                return Err(RuntimeError::new(
                    "typed_payload_invalid",
                    "heap",
                    "resource record variant is invalid",
                ));
            }
        };
        Ok(crate::value::Value {
            typ,
            data: crate::value::Data::Resource(Box::new(resource)),
        })
    }

    fn decode_record_lane(
        &self,
        typ: crate::types::TypeIdentity,
        lane: &[u8],
    ) -> Result<Option<crate::value::Value>, RuntimeError> {
        if lane.len() != 8 {
            return Ok(None);
        }
        let record =
            StorageRef::from_payload_identity(u64::from_le_bytes(lane.try_into().unwrap()));
        if !record.valid() {
            return Ok(None);
        }
        let snapshot = self.read_storage_snapshot(record)?;
        if snapshot.metadata.kind != crate::managed_storage::StoragePayloadKind::Record {
            return Ok(None);
        }
        Ok(Some(self.decode_record_payload(&typ, &snapshot.bytes)?))
    }

    fn prepared_inline_payload(
        &self,
        value: &crate::value::Value,
    ) -> Result<Option<PreparedPayload>, RuntimeError> {
        let registry =
            self.type_registry.read().unwrap().clone().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
        let layout = registry.storage_lane_layout(&value.typ)?;
        if let crate::value::Data::Aggregate(aggregate) = &value.data {
            if aggregate.owner_id() != self.owner_id() {
                return Err(RuntimeError::new(
                    "heap_owner",
                    "aggregate",
                    "aggregate belongs to a different heap",
                ));
            }
            if layout.kind == aggregate.layout().kind
                && layout.stride == aggregate.layout().stride
                && layout.references == aggregate.layout().references
                && layout.dynamics == aggregate.layout().dynamics
            {
                return Ok(Some(aggregate.payload()));
            }
            return Ok(None);
        }
        let mut bytes = vec![0; layout.stride as usize];
        let mut references = Vec::new();
        if matches!(
            &value.data,
            crate::value::Data::Nil | crate::value::Data::Uninitialized
        ) {
            return Ok(Some(PreparedPayload { bytes, references }));
        }
        let underlying = registry.underlying(&value.typ)?;
        let child_lane = |heap: &Heap,
                          child: &crate::value::Value,
                          declared: &crate::types::TypeIdentity,
                          registry: &TypeRegistry|
         -> Result<Option<PreparedPayload>, RuntimeError> {
            if *declared == crate::types::TypeIdentity::Any {
                if matches!(
                    child.data,
                    crate::value::Data::Array(_)
                        | crate::value::Data::Struct(_)
                        | crate::value::Data::Aggregate(_)
                ) {
                    return heap.prepared_any_aggregate_payload(child, true).map(Some);
                }
                return Ok(Some(heap.prepared_payload_inner(child, true)?));
            }
            if registry.is_interface(declared)? {
                Ok(Some(heap.prepared_payload_inner(child, true)?))
            } else {
                heap.prepared_inline_payload(child)
            }
        };
        // A uint8 lane has one canonical representation.  Host-created
        // integer data with the uint8 type is malformed input; reject it at
        // the prepared boundary instead of silently changing its runtime
        // kind while decoding the lane.
        if underlying
            == crate::types::TypeIdentity::Primitive(crate::contract_generated::PrimitiveUint8)
            && matches!(value.data, crate::value::Data::Integer(_))
        {
            return Ok(None);
        }
        match (&underlying, &value.data) {
            (
                crate::types::TypeIdentity::Primitive(crate::contract_generated::PrimitiveString),
                crate::value::Data::String(data),
            ) => {
                if data.is_empty() {
                    return Ok(Some(PreparedPayload { bytes, references }));
                }
                let (backing, offset) = match data.managed_backing() {
                    Some((owner, backing, offset))
                        if owner == self.owner_id() && self.is_live(backing) =>
                    {
                        (backing, offset)
                    }
                    _ => (self.allocate_prepared_box(data, &[])?, 0),
                };
                bytes[0..4].copy_from_slice(&backing.slot.to_le_bytes());
                bytes[4..8].copy_from_slice(&backing.generation.to_le_bytes());
                bytes[8..16].copy_from_slice(&offset.to_le_bytes());
                bytes[16..24].copy_from_slice(&(data.len() as u64).to_le_bytes());
                references.push((0, backing));
            }
            (_, crate::value::Data::OpaqueJSON(data)) if layout.references.contains(&0) => {
                let backing = self.allocate_prepared_box(data, &[])?;
                bytes[0..4].copy_from_slice(&backing.slot.to_le_bytes());
                bytes[4..8].copy_from_slice(&backing.generation.to_le_bytes());
                if bytes.len() >= 24 {
                    bytes[16..24].copy_from_slice(&(data.len() as u64).to_le_bytes());
                }
                references.push((0, backing));
            }
            (_, crate::value::Data::OpaqueJSON(data)) if !layout.dynamics.is_empty() => {
                let backing = self.allocate_prepared_box(data, &[])?;
                let type_id = registry.storage_type_id(&value.typ)?;
                bytes[0..4].copy_from_slice(&type_id.to_le_bytes());
                bytes[4..8].copy_from_slice(&crate::managed_storage::VALUE_REF.to_le_bytes());
                bytes[8..12].copy_from_slice(&backing.slot.to_le_bytes());
                bytes[12..16].copy_from_slice(&backing.generation.to_le_bytes());
                bytes[16..20].copy_from_slice(&(data.len() as u32).to_le_bytes());
                bytes[20..24]
                    .copy_from_slice(&crate::managed_storage::VALUE_REF_OPAQUE.to_le_bytes());
                references.push((8, backing));
            }
            (_, crate::value::Data::Bytes(data))
                if matches!(
                    registry.storage_layout(&value.typ)?,
                    Some(crate::types::StorageTypeLayout::Slice { .. })
                ) =>
            {
                let element = match registry.storage_layout(&value.typ)? {
                    Some(crate::types::StorageTypeLayout::Slice { _element }) => _element,
                    _ => unreachable!("slice layout was checked above"),
                };
                if registry.underlying(&element)?
                    != crate::types::TypeIdentity::Primitive(
                        crate::contract_generated::PrimitiveUint8,
                    )
                {
                    return Ok(None);
                }
                let backing = self.allocate_raw_bytes(data)?;
                bytes[0..8].copy_from_slice(&backing.identity_for_payload().to_le_bytes());
                bytes[24..32].fill(0);
                bytes[32..40].copy_from_slice(&(data.len() as u64).to_le_bytes());
                bytes[40..48].copy_from_slice(&(data.len() as u64).to_le_bytes());
                references.push((0, backing));
            }
            (_, crate::value::Data::Bytes(data)) if !layout.dynamics.is_empty() => {
                let backing = self.allocate_prepared_box(data, &[])?;
                let type_id = registry.storage_type_id(&value.typ)?;
                bytes[0..4].copy_from_slice(&type_id.to_le_bytes());
                bytes[4..8].copy_from_slice(&crate::managed_storage::VALUE_REF.to_le_bytes());
                bytes[8..12].copy_from_slice(&backing.slot.to_le_bytes());
                bytes[12..16].copy_from_slice(&backing.generation.to_le_bytes());
                bytes[16..20].copy_from_slice(&(data.len() as u32).to_le_bytes());
                bytes[20..24]
                    .copy_from_slice(&crate::managed_storage::VALUE_REF_BYTES.to_le_bytes());
                references.push((8, backing));
            }
            (_, crate::value::Data::Bytes(data)) if layout.references.contains(&0) => {
                let backing = self.allocate_prepared_box(data, &[])?;
                bytes[0..4].copy_from_slice(&backing.slot.to_le_bytes());
                bytes[4..8].copy_from_slice(&backing.generation.to_le_bytes());
                if bytes.len() >= 24 {
                    bytes[16..24].copy_from_slice(&(data.len() as u64).to_le_bytes());
                }
                references.push((0, backing));
            }
            (_, crate::value::Data::Slice(slice))
                if matches!(underlying, crate::types::TypeIdentity::Slice(_))
                    || matches!(
                        registry.storage_layout(&value.typ)?,
                        Some(crate::types::StorageTypeLayout::Slice { .. })
                    ) =>
            {
                // A slice can view an array inside another aggregate. Preserve
                // the base address before applying its element start/length.
                if slice.storage.identity.numeric.is_some() || !slice.storage.path.is_empty() {
                    let pointer = self.prepared_pointer_payload(&slice.storage, &registry)?;
                    bytes[..24].copy_from_slice(&pointer.bytes);
                    references.extend(pointer.references);
                } else {
                    // No projection means the complete backing object. A
                    // scalar element TypeID here would turn snapshot_address
                    // into a scalar load instead of an array/bytes view.
                    bytes[..8]
                        .copy_from_slice(&slice.storage.root.identity_for_payload().to_le_bytes());
                    references.push((0, slice.storage.root));
                }
                bytes[24..32].copy_from_slice(&(slice.start as u64).to_le_bytes());
                bytes[32..40].copy_from_slice(&(slice.length as u64).to_le_bytes());
                bytes[40..48].copy_from_slice(&(slice.capacity as u64).to_le_bytes());
            }
            (crate::types::TypeIdentity::Pointer(_), crate::value::Data::Pointer(address)) => {
                let pointer = self.prepared_pointer_payload(address, &registry)?;
                bytes[..pointer.bytes.len()].copy_from_slice(&pointer.bytes);
                references.extend(pointer.references);
            }
            (_, crate::value::Data::Pointer(address))
                if registry.pointer_element(&value.typ)?.is_some()
                    || matches!(
                        registry.storage_layout(&value.typ)?,
                        Some(crate::types::StorageTypeLayout::Slice { .. })
                    ) =>
            {
                let pointer = self.prepared_pointer_payload(address, &registry)?;
                bytes[..pointer.bytes.len()].copy_from_slice(&pointer.bytes);
                references.extend(pointer.references);
            }
            (_, crate::value::Data::Map(handle) | crate::value::Data::ResourceRef(handle)) => {
                if !layout.dynamics.is_empty() {
                    let flags = if matches!(value.data, crate::value::Data::Map(_)) {
                        crate::managed_storage::VALUE_REF_MAP
                    } else {
                        crate::managed_storage::VALUE_REF_RESOURCE
                    };
                    let type_id = registry.storage_type_id(&value.typ)?;
                    bytes[0..4].copy_from_slice(&type_id.to_le_bytes());
                    bytes[4..8].copy_from_slice(&crate::managed_storage::VALUE_REF.to_le_bytes());
                    bytes[8..12].copy_from_slice(&handle.slot.to_le_bytes());
                    bytes[12..16].copy_from_slice(&handle.generation.to_le_bytes());
                    bytes[20..24].copy_from_slice(&flags.to_le_bytes());
                    references.push((8, *handle));
                } else {
                    bytes[0..4].copy_from_slice(&handle.slot.to_le_bytes());
                    bytes[4..8].copy_from_slice(&handle.generation.to_le_bytes());
                    references.push((0, *handle));
                }
            }
            (crate::types::TypeIdentity::Structural { .. }, crate::value::Data::Struct(fields)) => {
                let Some(crate::types::StorageTypeLayout::Struct {
                    fields: layout_fields,
                }) = registry.storage_layout(&value.typ)?
                else {
                    return Ok(None);
                };
                let duplicate_names = layout_fields.iter().enumerate().any(|(index, (name, _))| {
                    layout_fields[..index]
                        .iter()
                        .any(|(previous, _)| previous == name)
                });
                let mut offset = 0usize;
                for (index, (name, field_type)) in layout_fields.iter().enumerate() {
                    // Values assembled from a host map are name ordered,
                    // while compiler-produced values preserve declaration
                    // order.  Use names whenever they are unique and fall
                    // back to the ordinal for repeated blank fields.
                    let field = if duplicate_names {
                        fields.field_at(index)
                    } else {
                        fields.get(name)
                    };
                    let Some(field) = field else {
                        return Ok(None);
                    };
                    let lane = child_lane(self, field, field_type, &registry)?;
                    let Some(lane) = lane else { return Ok(None) };
                    if lane.bytes.len() != registry.storage_lane_layout(field_type)?.stride as usize
                    {
                        return Ok(None);
                    }
                    let end = offset.checked_add(lane.bytes.len()).ok_or_else(|| {
                        RuntimeError::new("allocation_limit", "heap", "prepared lane too large")
                    })?;
                    if end > bytes.len() {
                        return Ok(None);
                    }
                    bytes[offset..end].copy_from_slice(&lane.bytes);
                    references.extend(
                        lane.references
                            .into_iter()
                            .map(|(child_offset, handle)| ((offset as u32) + child_offset, handle)),
                    );
                    offset = end;
                }
            }
            (crate::types::TypeIdentity::Structural { .. }, crate::value::Data::Array(values)) => {
                let Some(crate::types::StorageTypeLayout::Array { element, length }) =
                    registry.storage_layout(&value.typ)?
                else {
                    return Ok(None);
                };
                if length != values.len() {
                    return Ok(None);
                }
                let mut offset = 0usize;
                for child in values.iter() {
                    let lane = child_lane(self, child, &element, &registry)?;
                    let Some(lane) = lane else {
                        return Ok(None);
                    };
                    if lane.bytes.len() != registry.storage_lane_layout(&element)?.stride as usize {
                        return Ok(None);
                    }
                    let end = offset.checked_add(lane.bytes.len()).ok_or_else(|| {
                        RuntimeError::new("allocation_limit", "heap", "prepared lane too large")
                    })?;
                    if end > bytes.len() {
                        return Ok(None);
                    }
                    bytes[offset..end].copy_from_slice(&lane.bytes);
                    references.extend(
                        lane.references
                            .into_iter()
                            .map(|(child_offset, handle)| ((offset as u32) + child_offset, handle)),
                    );
                    offset = end;
                }
            }
            (_, crate::value::Data::Function(function))
                if layout.dynamics.is_empty() && layout.references.contains(&0) =>
            {
                let function = self.prepared_function_payload(function, &registry)?;
                let record = self.allocate_storage_box(
                    &function.bytes,
                    &function.references,
                    crate::managed_storage::StoragePayloadKind::Record,
                    Some(value.typ.clone()),
                )?;
                return Ok(Some(PreparedPayload {
                    bytes: record.identity_for_payload().to_le_bytes().to_vec(),
                    references: vec![(0, record)],
                }));
            }
            (_, crate::value::Data::DynamicFunction(_))
                if layout.dynamics.is_empty() && layout.references.contains(&0) =>
            {
                let dynamic = self.prepared_dynamic_function_payload(value, &registry)?;
                let record = self.allocate_storage_box(
                    &dynamic.bytes,
                    &dynamic.references,
                    crate::managed_storage::StoragePayloadKind::Record,
                    Some(value.typ.clone()),
                )?;
                return Ok(Some(PreparedPayload {
                    bytes: record.identity_for_payload().to_le_bytes().to_vec(),
                    references: vec![(0, record)],
                }));
            }
            (_, crate::value::Data::Method { function, receiver })
                if layout.dynamics.is_empty() && layout.references.contains(&0) =>
            {
                let record = self.prepared_record_payload(
                    &crate::value::Value {
                        typ: value.typ.clone(),
                        data: crate::value::Data::Method {
                            function: function.clone(),
                            receiver: receiver.clone(),
                        },
                    },
                    &registry,
                )?;
                let record = self.allocate_storage_box(
                    &record.bytes,
                    &record.references,
                    crate::managed_storage::StoragePayloadKind::Record,
                    Some(value.typ.clone()),
                )?;
                return Ok(Some(PreparedPayload {
                    bytes: record.identity_for_payload().to_le_bytes().to_vec(),
                    references: vec![(0, record)],
                }));
            }
            (_, crate::value::Data::Resource(resource))
                if layout.dynamics.is_empty() && layout.references.contains(&0) =>
            {
                let record = self.prepared_record_payload(
                    &crate::value::Value {
                        typ: value.typ.clone(),
                        data: crate::value::Data::Resource(resource.clone()),
                    },
                    &registry,
                )?;
                let record = self.allocate_storage_box(
                    &record.bytes,
                    &record.references,
                    crate::managed_storage::StoragePayloadKind::Record,
                    Some(value.typ.clone()),
                )?;
                return Ok(Some(PreparedPayload {
                    bytes: record.identity_for_payload().to_le_bytes().to_vec(),
                    references: vec![(0, record)],
                }));
            }
            _ if layout.fixed => {
                let (words, count) = match value.data {
                    crate::value::Data::Bool(value) => ([u64::from(value), 0], 1),
                    crate::value::Data::Integer(value) => ([value as u64, 0], 1),
                    crate::value::Data::Unsigned(value) => ([value, 0], 1),
                    crate::value::Data::Float(value) => ([value.to_bits(), 0], 1),
                    crate::value::Data::Complex { real, imag } => {
                        ([real.to_bits(), imag.to_bits()], 2)
                    }
                    _ => return Ok(None),
                };
                if count * 8 != bytes.len() {
                    return Ok(None);
                }
                for (index, word) in words[..count].iter().enumerate() {
                    bytes[index * 8..index * 8 + 8].copy_from_slice(&word.to_le_bytes());
                }
            }
            _ => return Ok(None),
        }
        Ok(Some(PreparedPayload { bytes, references }))
    }

    fn prepared_storage_payload(
        &self,
        value: &crate::value::Value,
    ) -> Result<Option<PreparedPayload>, RuntimeError> {
        let registry =
            self.type_registry.read().unwrap().clone().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
        if matches!(
            &value.data,
            crate::value::Data::Uninitialized | crate::value::Data::Nil
        ) {
            return self.prepared_inline_payload(value);
        }
        if matches!(
            &value.data,
            crate::value::Data::Function(_)
                | crate::value::Data::Method { .. }
                | crate::value::Data::Resource(_)
        ) {
            // Top-level managed records are published directly.  Dynamic
            // lanes use `prepared_dynamic_payload` below and retain their
            // 24-byte descriptor instead of leaking the physical record.
            return Ok(Some(self.prepared_record_payload(value, &registry)?));
        }
        if matches!(&value.data, crate::value::Data::DynamicFunction(_)) {
            return Ok(Some(
                self.prepared_dynamic_function_payload(value, &registry)?,
            ));
        }
        if value.typ == crate::types::TypeIdentity::Any
            && matches!(
                &value.data,
                crate::value::Data::Bytes(_) | crate::value::Data::OpaqueJSON(_)
            )
        {
            return self.prepared_inline_payload(value);
        }
        if value.typ == crate::types::TypeIdentity::Any
            && matches!(
                &value.data,
                crate::value::Data::Struct(_)
                    | crate::value::Data::Array(_)
                    | crate::value::Data::Aggregate(_)
            )
        {
            return self.prepared_any_aggregate_payload(value, true).map(Some);
        }
        if value.typ == crate::types::TypeIdentity::Any {
            return Ok(Some(self.prepared_payload_inner(value, true)?));
        }
        if registry.is_interface(&value.typ)? {
            return Ok(Some(self.prepared_payload_inner(value, true)?));
        }
        self.prepared_inline_payload(value)
    }

    #[expect(
        clippy::result_large_err,
        reason = "allocation failure must return the unpublished value without allocating"
    )]
    fn write_prepared_new(
        &self,
        value: Value,
        logical: u64,
        payload: PreparedPayload,
    ) -> Result<Handle, (RuntimeError, Value)> {
        let registry = self.type_registry.read().unwrap().clone();
        let lane = registry
            .as_deref()
            .ok_or_else(|| RuntimeError::new("invalid_type", "heap", "missing type registry"))
            .and_then(|types| types.storage_lane_layout(&value.typ))
            .map_err(|error| (error, value.clone()))?;
        for (_, child) in payload.references.iter().copied() {
            if child.valid() && !self.is_live(child) {
                return Err((
                    RuntimeError::new("stale_reference", "heap", "payload edge is not live"),
                    value,
                ));
            }
        }
        let direct_record = matches!(
            value.data,
            crate::value::Data::Function(_)
                | crate::value::Data::DynamicFunction(_)
                | crate::value::Data::Method { .. }
                | crate::value::Data::Resource(_)
        );
        let stride = if direct_record {
            u32::try_from(payload.bytes.len()).map_err(|_| {
                (
                    RuntimeError::new("allocation_limit", "heap", "function record is too large"),
                    value.clone(),
                )
            })?
        } else {
            lane.stride.max(8)
        };
        if payload.bytes.len() > stride as usize {
            return Err((
                RuntimeError::new(
                    "typed_payload_invalid",
                    "heap",
                    "prepared lane exceeds layout",
                ),
                value,
            ));
        }
        let layout = self
            .storage
            .register_layout(StorageLayout {
                stride,
                references: if direct_record {
                    payload
                        .references
                        .iter()
                        .map(|(offset, _)| *offset)
                        .collect()
                } else {
                    lane.references.clone()
                },
                dynamics: if direct_record {
                    Vec::new()
                } else {
                    lane.dynamics.clone()
                },
            })
            .map_err(|error| (error, value.clone()))?;
        let mut bytes = vec![0; stride as usize];
        bytes[..payload.bytes.len()].copy_from_slice(&payload.bytes);
        let metadata = StorageObjectMeta {
            logical_bytes: logical,
            typ: value.payload_type(),
            initialized: value.payload_initialized(),
            payload_len: payload.bytes.len() as u32,
            kind: if direct_record {
                crate::managed_storage::StoragePayloadKind::Record
            } else {
                crate::managed_storage::StoragePayloadKind::Typed
            },
            version: 1,
        };
        if stride > crate::managed_storage::TLAB_MAX_OBJECT_BYTES {
            self.flush_worker_lease()
                .map_err(|error| (error, value.clone()))?;
            return self
                .storage
                .reserve_direct(layout, 1, logical, &bytes, metadata)
                .map(|handle| self.record_allocation(handle))
                .map_err(|error| (error, value));
        }
        let available = self.max_bytes.saturating_sub(self.stats().guest_bytes);
        // Bound worker reservations to one small TLAB.  The storage pool
        // clamps this request to the remaining guest quota, so tiny-budget
        // instances are admitted instead of being rejected by a fixed batch
        // size; a full page simply rolls over to another lease.
        let lease_credit = available.min(64 << 10).max(logical);
        let mut lease = LOCAL_LEASES
            .with(|leases| leases.borrow_mut().remove(&self.worker_id))
            .map(Ok)
            .unwrap_or_else(|| self.storage.acquire(lease_credit))
            .map_err(|error| (error, value.clone()))?;
        let handle = match lease.reserve_prepared(layout, stride, 1, logical) {
            Ok(handle) => handle,
            Err(error) if error.code == "collection_required" => {
                // Refilling a private page does not yield the mutator or run
                // GC. Publish its initialized children, then continue the
                // parent on the next page. Only a failed refill requests an
                // owner safepoint; a full ticket batch is not a heap limit.
                lease.flush().map_err(|error| (error, value.clone()))?;
                lease = self
                    .storage
                    .acquire(lease_credit)
                    .map_err(|error| (error, value.clone()))?;
                match lease.reserve_prepared(layout, stride, 1, logical) {
                    Ok(handle) => handle,
                    Err(error) => {
                        LOCAL_LEASES.with(|leases| {
                            leases.borrow_mut().insert(self.worker_id, lease);
                        });
                        return Err((error, value));
                    }
                }
            }
            Err(error) => {
                LOCAL_LEASES.with(|leases| {
                    leases.borrow_mut().insert(self.worker_id, lease);
                });
                return Err((error, value));
            }
        };
        if let Err(error) = lease
            .write(handle, 0, &bytes)
            .and_then(|()| lease.set_metadata(handle, metadata))
            .and_then(|()| lease.commit(handle))
        {
            let _ = lease.abort(handle);
            LOCAL_LEASES.with(|leases| {
                leases.borrow_mut().insert(self.worker_id, lease);
            });
            return Err((error, value));
        }
        LOCAL_LEASES.with(|leases| {
            leases.borrow_mut().insert(self.worker_id, lease);
        });
        Ok(self.record_allocation(handle))
    }

    #[expect(
        clippy::result_large_err,
        reason = "allocation failure must return the unpublished value without allocating"
    )]
    fn write_new(&self, value: Value, logical: u64) -> Result<Handle, (RuntimeError, Value)> {
        let active_objects = LOCAL_LEASES.with(|leases| {
            leases
                .borrow()
                .get(&self.worker_id)
                .map(crate::managed_storage::StorageWorkerLease::committed_objects)
                .unwrap_or_default()
        });
        if self
            .storage
            .live_objects()
            .map_err(|error| (error, value.clone()))?
            .saturating_add(active_objects)
            >= self.max_objects
        {
            return Err((
                RuntimeError::new("allocation_limit", "heap", "object storage exhausted"),
                value,
            ));
        }
        let scope = self.begin_allocation_scope();
        let payload = match self.prepared_storage_payload(&value) {
            Ok(Some(payload)) => payload,
            Ok(None) => {
                return Err((
                    RuntimeError::new(
                        "typed_payload_unsupported",
                        "heap",
                        format!("value has no prepared storage layout: {:?}", value.typ),
                    ),
                    value,
                ));
            }
            Err(error) => return Err((error, value)),
        };
        let handle = self.write_prepared_new(value, logical, payload)?;
        scope.commit();
        Ok(handle)
    }

    #[expect(
        clippy::result_large_err,
        reason = "failed writes return their value for a rooted retry without allocating"
    )]
    fn replace_prepared(
        &self,
        handle: Handle,
        value: Value,
        charge: u64,
        payload: PreparedPayload,
        expected_version: Option<u64>,
    ) -> Result<bool, (RuntimeError, Value)> {
        let registry = self.type_registry.read().unwrap().clone();
        let lane = registry
            .as_deref()
            .ok_or_else(|| RuntimeError::new("invalid_type", "heap", "missing type registry"))
            .and_then(|types| types.storage_lane_layout(&value.typ))
            .map_err(|error| (error, value.clone()))?;
        let direct_record = matches!(
            value.data,
            crate::value::Data::Function(_)
                | crate::value::Data::DynamicFunction(_)
                | crate::value::Data::Method { .. }
                | crate::value::Data::Resource(_)
        );
        let stride = if direct_record {
            u32::try_from(payload.bytes.len()).map_err(|_| {
                (
                    RuntimeError::new("allocation_limit", "heap", "function record is too large"),
                    value.clone(),
                )
            })?
        } else {
            lane.stride.max(8)
        };
        if payload.bytes.len() > stride as usize {
            return Err((
                RuntimeError::new(
                    "typed_payload_invalid",
                    "heap",
                    "prepared lane exceeds layout",
                ),
                value,
            ));
        }
        for (_, child) in payload.references.iter().copied() {
            if child.valid() && !self.is_live(child) {
                return Err((
                    RuntimeError::new("stale_reference", "heap", "payload edge is not live"),
                    value,
                ));
            }
        }
        let layout = self
            .storage
            .register_layout(StorageLayout {
                stride,
                references: if direct_record {
                    payload
                        .references
                        .iter()
                        .map(|(offset, _)| *offset)
                        .collect()
                } else {
                    lane.references.clone()
                },
                dynamics: if direct_record {
                    Vec::new()
                } else {
                    lane.dynamics.clone()
                },
            })
            .map_err(|error| (error, value.clone()))?;
        let mut bytes = vec![0; stride as usize];
        bytes[..payload.bytes.len()].copy_from_slice(&payload.bytes);
        let mut meta = self.meta(handle).map_err(|error| (error, value.clone()))?;
        if expected_version.is_some_and(|expected| meta.version != expected) {
            return Ok(false);
        }
        meta.logical_bytes = charge;
        meta.typ = value.payload_type();
        meta.initialized = value.payload_initialized();
        meta.payload_len = payload.bytes.len() as u32;
        meta.kind = if direct_record {
            crate::managed_storage::StoragePayloadKind::Record
        } else {
            crate::managed_storage::StoragePayloadKind::Typed
        };
        meta.version = meta.version.wrapping_add(1).max(1);
        let committed = match expected_version {
            Some(expected) => self.storage.rewrite_with_metadata_if_version(
                handle, layout, 1, charge, &bytes, expected, meta,
            ),
            None => self
                .storage
                .rewrite_with_metadata(handle, layout, 1, charge, &bytes, meta)
                .map(|()| true),
        }
        .map_err(|error| (error, value.clone()))?;
        Ok(committed)
    }

    #[expect(
        clippy::result_large_err,
        reason = "allocation failure must return the unpublished value without allocating"
    )]
    pub fn allocate(&self, value: Value, bytes: u64) -> Result<Handle, (RuntimeError, Value)> {
        self.write_new(value, bytes)
    }
    #[cfg(test)]
    pub fn get(&self, handle: Handle) -> Result<Value, RuntimeError> {
        self.read_typed_value(handle)
    }
    pub(crate) fn allocation_bytes(&self, handle: Handle) -> Result<u64, RuntimeError> {
        Ok(self.meta(handle)?.logical_bytes)
    }
    pub(crate) fn payload_type(
        &self,
        handle: Handle,
    ) -> Result<Option<crate::types::TypeIdentity>, RuntimeError> {
        Ok(self.meta(handle)?.typ)
    }
    pub(crate) fn payload_initialized(&self, handle: Handle) -> Result<bool, RuntimeError> {
        Ok(self.meta(handle)?.initialized)
    }

    /// Prepared uninitialized cells already contain the canonical all-zero
    /// lane. Publishing that state needs no recursive aggregate construction.
    pub(crate) fn initialize_zero(&self, handle: Handle) -> Result<(), RuntimeError> {
        loop {
            let metadata = self.meta(handle)?;
            if metadata.initialized
                || self.write_storage_range_if_version(handle, 0, &[], metadata.version)?
            {
                return Ok(());
            }
        }
    }
    pub(crate) fn read_byte_storage_range(
        &self,
        handle: Handle,
        start: usize,
        length: usize,
    ) -> Result<Option<Vec<u8>>, RuntimeError> {
        let offset = u32::try_from(start)
            .map_err(|_| RuntimeError::new("invalid_slice", "bytes", "byte offset overflow"))?;
        let length = u32::try_from(length)
            .map_err(|_| RuntimeError::new("invalid_slice", "bytes", "byte length overflow"))?;
        loop {
            let meta = self.meta(handle)?;
            if meta.kind == crate::managed_storage::StoragePayloadKind::RawBytes {
                let size = meta.payload_len as usize;
                if start > size || length as usize > size - start {
                    return Err(RuntimeError::new(
                        "invalid_slice",
                        "bytes",
                        "invalid byte view",
                    ));
                }
                let snapshot = self.read_storage_snapshot_range(handle, offset, length)?;
                if snapshot.metadata.version != meta.version
                    || snapshot.metadata.kind
                        != crate::managed_storage::StoragePayloadKind::RawBytes
                {
                    continue;
                }
                return Ok(Some(snapshot.bytes));
            }
            if meta.typ != Some(crate::types::TypeIdentity::Any) || meta.payload_len != 24 {
                return Ok(None);
            }
            let snapshot = self.read_storage_snapshot_range(handle, 0, 24)?;
            if snapshot.metadata.version != meta.version
                || snapshot.metadata.typ != Some(crate::types::TypeIdentity::Any)
                || snapshot.metadata.payload_len != 24
            {
                continue;
            }
            let descriptor = &snapshot.bytes;
            if descriptor[4..8] != crate::managed_storage::VALUE_REF.to_le_bytes()
                || descriptor[20..24] != crate::managed_storage::VALUE_REF_BYTES.to_le_bytes()
            {
                return Ok(None);
            }
            let size = u32::from_le_bytes(descriptor[16..20].try_into().unwrap()) as usize;
            if start > size || length as usize > size - start {
                return Err(RuntimeError::new(
                    "invalid_slice",
                    "bytes",
                    "invalid byte view",
                ));
            }
            let backing = StorageRef::from_payload_identity(u64::from_le_bytes(
                descriptor[8..16].try_into().unwrap(),
            ));
            return self
                .read_storage_snapshot_range(backing, offset, length)
                .map(|snapshot| Some(snapshot.bytes));
        }
    }

    /// Updates a raw byte backing directly. Rebuilding the value would
    /// allocate a second full box and make a small byte write consume the
    /// backing budget. The backing version is the mutation guard.
    pub(crate) fn write_byte_storage_range_if_version(
        &self,
        handle: Handle,
        start: usize,
        bytes: &[u8],
        expected_version: u64,
    ) -> Result<bool, RuntimeError> {
        let metadata = self.meta(handle)?;
        if metadata.kind == crate::managed_storage::StoragePayloadKind::RawBytes {
            if metadata.version != expected_version {
                return Ok(false);
            }
            let length = metadata.payload_len as usize;
            if start > length || bytes.len() > length - start {
                return Err(RuntimeError::new(
                    "invalid_slice",
                    "bytes",
                    "invalid byte view",
                ));
            }
            return self.write_storage_range_if_version(
                handle,
                u32::try_from(start).map_err(|_| {
                    RuntimeError::new("invalid_slice", "bytes", "byte offset overflow")
                })?,
                bytes,
                expected_version,
            );
        }
        if metadata.version != expected_version
            || metadata.typ != Some(crate::types::TypeIdentity::Any)
            || metadata.payload_len != 24
        {
            return Ok(false);
        }
        let mut descriptor = [0_u8; 24];
        self.read_storage_into(handle, 0, &mut descriptor)?;
        if descriptor[4..8] != crate::managed_storage::VALUE_REF.to_le_bytes()
            || descriptor[20..24] != crate::managed_storage::VALUE_REF_BYTES.to_le_bytes()
        {
            return Ok(false);
        }
        let length = u32::from_le_bytes(descriptor[16..20].try_into().unwrap()) as usize;
        if start > length || bytes.len() > length - start {
            return Err(RuntimeError::new(
                "invalid_slice",
                "bytes",
                "invalid byte view",
            ));
        }
        let backing = StorageRef::from_payload_identity(u64::from_le_bytes(
            descriptor[8..16].try_into().unwrap(),
        ));
        let backing_version = self.meta(backing)?.version;
        self.write_storage_range_if_version(
            backing,
            u32::try_from(start)
                .map_err(|_| RuntimeError::new("invalid_slice", "bytes", "byte offset overflow"))?,
            bytes,
            backing_version,
        )
    }
    #[expect(
        clippy::result_large_err,
        reason = "failed writes return their value for a rooted retry without allocating"
    )]
    pub fn replace(
        &self,
        handle: Handle,
        value: Value,
        bytes: u64,
    ) -> Result<(), (RuntimeError, Value)> {
        self.replace_with_expected(handle, value, bytes, None)
    }

    #[expect(
        clippy::result_large_err,
        reason = "failed writes return their value for a rooted retry without allocating"
    )]
    fn replace_with_expected(
        &self,
        handle: Handle,
        value: Value,
        bytes: u64,
        expected_version: Option<u64>,
    ) -> Result<(), (RuntimeError, Value)> {
        if expected_version.is_some_and(|expected| {
            self.meta(handle)
                .map(|meta| meta.version != expected)
                .unwrap_or(true)
        }) {
            return Err((
                RuntimeError::new("concurrent_update", "heap", "object changed during update"),
                value,
            ));
        }
        let scope = self.begin_allocation_scope();
        let payload = self
            .prepared_storage_payload(&value)
            .map_err(|error| (error, value.clone()))?
            .ok_or_else(|| {
                (
                    RuntimeError::new(
                        "typed_payload_unsupported",
                        "heap",
                        "value has no prepared storage layout",
                    ),
                    value.clone(),
                )
            })?;
        self.publish_payload(&payload)
            .map_err(|error| (error, value.clone()))?;
        // `replace_prepared` writes through the shared directory.  The
        // target itself may still be a nursery handle (for example a newly
        // created resource whose owner record is installed immediately), so
        // publish that handle even when the payload has no private child
        // references.
        if self.local_lease_contains(handle) {
            self.flush_worker_lease()
                .map_err(|error| (error, value.clone()))?;
        }
        let replacement = value.clone();
        match self.replace_prepared(handle, value, bytes, payload, expected_version) {
            Ok(true) => {
                scope.commit();
                Ok(())
            }
            Ok(false) => Err((
                RuntimeError::new("concurrent_update", "heap", "object changed during update"),
                replacement,
            )),
            Err(error) => Err(error),
        }
    }
    pub fn collect(&self, roots: impl IntoIterator<Item = Handle>) -> Result<usize, RuntimeError> {
        self.collect_with_persistent_roots(&[], roots)
    }
    pub(crate) fn collect_minor(
        &self,
        roots: impl IntoIterator<Item = Handle>,
    ) -> Result<usize, RuntimeError> {
        self.storage.collect_minor(roots)
    }
    pub(crate) fn collect_with_persistent_roots(
        &self,
        persistent: &[Handle],
        roots: impl IntoIterator<Item = Handle>,
    ) -> Result<usize, RuntimeError> {
        self.storage
            .collect_major(persistent.iter().copied().chain(roots))
    }
    pub fn stats(&self) -> HeapStats {
        let Ok(s) = self.storage.stats() else {
            return HeapStats::default();
        };
        // StorageHeap owns the authoritative object directory and generation
        // pressure counters. Keep the VM-facing values derived from that
        // ledger so nursery and old-generation scheduling remain enabled.
        let active_objects = LOCAL_LEASES.with(|leases| {
            leases
                .borrow()
                .get(&self.worker_id)
                .map(crate::managed_storage::StorageWorkerLease::committed_objects)
                .unwrap_or_default()
        });
        let active_bytes = LOCAL_LEASES.with(|leases| {
            leases
                .borrow()
                .get(&self.worker_id)
                .map(crate::managed_storage::StorageWorkerLease::committed_bytes)
                .unwrap_or_default()
        });
        let live_objects = self
            .storage
            .live_objects()
            .unwrap_or_default()
            .saturating_add(active_objects);
        let collections = s.minor_collections.saturating_add(s.major_collections);
        let gc_pressure_bytes = s.young_bytes;
        HeapStats {
            collections,
            scanned_objects: s.scanned_objects,
            traced_objects: s.traced_objects,
            collection_nanos: s.collection_nanos,
            live_objects,
            live_bytes: s.occupied_bytes.saturating_add(active_bytes),
            young_bytes: s.young_bytes,
            old_bytes: s.old_bytes,
            guest_bytes: s.occupied_bytes.saturating_add(active_bytes),
            // Private committed objects are already included in guest/live
            // bytes, so remove just that portion from the shared reservation.
            // Unused TLAB credit remains visible until the lease is flushed.
            reserved_bytes: s.reserved_bytes.saturating_sub(active_bytes),
            total_allocated_bytes: s.total_allocated_bytes.saturating_add(active_bytes),
            gc_pressure_bytes,
            allocations: s.allocations,
            peak_bytes: s.peak_bytes,
            minor_collections: s.minor_collections,
            major_collections: s.major_collections,
            copied_bytes: s.copied_bytes,
            promoted_bytes: s.promoted_bytes,
            promoted_objects: s.promoted_objects,
            reclaimed_bytes: s.reclaimed_bytes,
            scanned_roots: s.scanned_roots,
            scanned_references: s.scanned_references,
            dirty_cards: s.scanned_cards,
            last_major_reachable_bytes: s.last_major_reachable_bytes,
            resident_page_bytes: s.resident_page_bytes,
        }
    }
    pub(crate) fn replacements_fit(
        &self,
        replacements: &[(Handle, u64)],
    ) -> Result<bool, RuntimeError> {
        let current = self.stats().live_bytes;
        let old = replacements.iter().try_fold(0u64, |n, (h, _)| {
            Ok::<_, RuntimeError>(n.saturating_add(self.allocation_bytes(*h)?))
        })?;
        let new = replacements.iter().map(|(_, b)| *b).sum::<u64>();
        Ok(current.saturating_sub(old).saturating_add(new) <= self.max_bytes)
    }

    /// Prepare existing fixed frame destinations without changing their
    /// payloads. All nested allocation and encoding precedes the storage
    /// batch commit, so a later invalid result cannot publish an earlier one.
    pub(crate) fn prepare_replacements(
        &self,
        values: Vec<(Handle, Value, u64)>,
    ) -> Result<PreparedReplacements<'_>, RuntimeError> {
        let scope = self.begin_allocation_scope();
        let mut writes = Vec::with_capacity(values.len());
        for (handle, value, charge) in values {
            let payload = self.prepared_storage_payload(&value)?.ok_or_else(|| {
                RuntimeError::new(
                    "typed_payload_unsupported",
                    "frame",
                    "result has no prepared storage layout",
                )
            })?;
            let lane = self.layout_for_type(&value.typ)?;
            let stride = lane.stride.max(8);
            let mut metadata = self.meta(handle)?;
            if metadata.logical_bytes != charge
                || payload.bytes.len() > stride as usize
                || stride > self.storage_size(handle)?
            {
                return Err(RuntimeError::new(
                    "invalid_result",
                    "frame",
                    "result does not fit its fixed storage destination",
                ));
            }
            for (_, child) in &payload.references {
                if child.valid() && !self.is_live(*child) {
                    return Err(RuntimeError::new(
                        "stale_reference",
                        "frame",
                        "result payload edge is not live",
                    ));
                }
            }
            let layout = self.storage.register_layout(StorageLayout {
                stride,
                references: lane.references.clone(),
                dynamics: lane.dynamics.clone(),
            })?;
            let mut bytes = payload.bytes;
            metadata.payload_len = bytes.len() as u32;
            bytes.resize(stride as usize, 0);
            metadata.typ = value.payload_type();
            metadata.initialized = value.payload_initialized();
            metadata.kind = crate::managed_storage::StoragePayloadKind::Typed;
            metadata.version = metadata.version.wrapping_add(1).max(1);
            writes.push(StorageRewrite {
                handle,
                layout,
                bytes,
                metadata,
            });
        }
        Ok(PreparedReplacements {
            heap: self,
            writes,
            scope,
        })
    }

    pub(crate) fn prepare_allocations(
        &self,
        values: Vec<(Value, u64)>,
    ) -> Result<AllocationBatch<'_>, RuntimeError> {
        let scope = self.begin_allocation_scope();
        let mut handles = Vec::with_capacity(values.len());
        for (value, bytes) in values {
            handles.push(self.allocate(value, bytes).map_err(|(error, _)| error)?);
        }
        Ok(AllocationBatch {
            heap: self,
            handles,
            scope,
        })
    }
}

pub(crate) struct PreparedReplacements<'a> {
    heap: &'a Heap,
    writes: Vec<StorageRewrite>,
    scope: AllocationScope<'a>,
}

impl PreparedReplacements<'_> {
    pub(crate) fn commit(self) -> Result<(), RuntimeError> {
        if !self.writes.is_empty() {
            // The batch updates the shared directory. Publish both targets
            // and prepared children before taking its exclusive commit lock.
            self.heap.flush_worker_lease()?;
            self.heap.storage.rewrite_fixed_batch(&self.writes)?;
        }
        self.scope.commit();
        Ok(())
    }
}

pub(crate) struct AllocationBatch<'a> {
    heap: &'a Heap,
    handles: Vec<Handle>,
    scope: AllocationScope<'a>,
}
impl AllocationBatch<'_> {
    pub(crate) fn handles(&self) -> &[Handle] {
        &self.handles
    }
    pub(crate) fn prepare_revision_anchor(
        &mut self,
        globals: impl ExactSizeIterator<Item = Handle>,
    ) -> Result<Handle, RuntimeError> {
        let anchor = self.heap.allocate_revision_anchor(globals)?;
        self.handles.push(anchor);
        Ok(anchor)
    }
    pub(crate) fn commit(self) {
        self.scope.commit();
    }
}

impl Heap {
    /// Encodes a value once at the managed boundary for typed containers.
    /// Fixed values are copied as numeric lanes. Dynamic values are represented
    /// by a 24-byte descriptor whose ticket points at an immutable typed box;
    /// the destination only copies the descriptor and registers that ticket as
    /// a reference in its own layout.
    pub(crate) fn prepared_payload(
        &self,
        value: &crate::value::Value,
    ) -> Result<PreparedPayload, RuntimeError> {
        let scope = self.begin_allocation_scope();
        let payload = self.prepared_payload_inner(value, false)?;
        scope.commit();
        Ok(payload)
    }

    pub(crate) fn prepared_payload_for_type(
        &self,
        value: &crate::value::Value,
        typ: &crate::types::TypeIdentity,
    ) -> Result<PreparedPayload, RuntimeError> {
        if self.layout_for_type(typ)?.kind == crate::contract_generated::Interface {
            self.prepared_dynamic_payload(value)
        } else {
            self.prepared_payload(&crate::value::Value {
                typ: typ.clone(),
                data: value.data.clone(),
            })
        }
    }

    /// Prepares a dynamic descriptor even when the runtime value is a fixed
    /// scalar. This is required by `Any` channels/maps so the typed box keeps
    /// the concrete runtime type while the container lane remains uniform.
    pub(crate) fn prepared_dynamic_payload(
        &self,
        value: &crate::value::Value,
    ) -> Result<PreparedPayload, RuntimeError> {
        let scope = self.begin_allocation_scope();
        let payload = self.prepared_payload_inner(value, true)?;
        scope.commit();
        Ok(payload)
    }

    fn dynamic_value_type(value: &crate::value::Value) -> Option<crate::types::TypeIdentity> {
        if value.typ != crate::types::TypeIdentity::Any {
            return Some(value.typ.clone());
        }
        match &value.data {
            crate::value::Data::Aggregate(value) => Some(value.typ().clone()),
            crate::value::Data::String(_) => Some(crate::types::TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveString,
            )),
            crate::value::Data::Integer(_) => Some(crate::types::TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveInt,
            )),
            crate::value::Data::Unsigned(_) => Some(crate::types::TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveUint,
            )),
            crate::value::Data::Bool(_) => Some(crate::types::TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveBool,
            )),
            crate::value::Data::Float(_) => Some(crate::types::TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveFloat64,
            )),
            crate::value::Data::Complex { .. } => Some(crate::types::TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveComplex128,
            )),
            crate::value::Data::Function(_) | crate::value::Data::Method { .. } => Some(
                crate::types::TypeIdentity::Primitive(crate::contract_generated::PrimitiveFunction),
            ),
            crate::value::Data::Slice(_) => Some(crate::types::TypeIdentity::Slice(Arc::new(
                crate::types::TypeIdentity::Primitive(crate::contract_generated::PrimitiveUint8),
            ))),
            crate::value::Data::Pointer(_) => Some(crate::types::TypeIdentity::Pointer(Arc::new(
                crate::types::TypeIdentity::Any,
            ))),
            _ => None,
        }
    }

    fn prepared_payload_inner(
        &self,
        value: &crate::value::Value,
        force_dynamic: bool,
    ) -> Result<PreparedPayload, RuntimeError> {
        // The destination decides the lane width: concrete function storage
        // holds an 8-byte record reference, while Any/interface storage holds
        // the full dynamic descriptor, including a MakeFunc value's type ID.
        if value.typ == crate::types::TypeIdentity::Any
            && matches!(
                value.data,
                crate::value::Data::Array(_)
                    | crate::value::Data::Struct(_)
                    | crate::value::Data::Aggregate(_)
            )
            && !force_dynamic
        {
            return self.prepared_any_aggregate_payload(value, true);
        }
        let concrete_type = Self::dynamic_value_type(value);
        let concrete_value =
            concrete_type
                .filter(|typ| *typ != value.typ)
                .map(|typ| crate::value::Value {
                    typ,
                    data: value.data.clone(),
                });
        let value = concrete_value.as_ref().unwrap_or(value);
        if !force_dynamic && let Some(payload) = self.prepared_inline_payload(value)? {
            return Ok(payload);
        }
        let types = self.type_registry.read().unwrap().clone();
        if force_dynamic {
            if value.typ == crate::types::TypeIdentity::Any
                && matches!(
                    value.data,
                    crate::value::Data::Array(_)
                        | crate::value::Data::Struct(_)
                        | crate::value::Data::Aggregate(_)
                )
            {
                return self.prepared_any_aggregate_payload(value, false);
            }
            if matches!(value.data, crate::value::Data::DynamicFunction(_)) {
                let registry = types.as_deref().ok_or_else(|| {
                    RuntimeError::new("invalid_type", "heap", "missing type registry")
                })?;
                let inner = self.prepared_dynamic_function_payload(value, registry)?;
                let type_id = registry.storage_type_id(&value.typ)?;
                let box_ref = self.allocate_storage_box(
                    &inner.bytes,
                    &inner.references,
                    crate::managed_storage::StoragePayloadKind::Record,
                    Some(value.typ.clone()),
                )?;
                let mut descriptor = vec![0_u8; 24];
                descriptor[0..4].copy_from_slice(&type_id.to_le_bytes());
                descriptor[4..8].copy_from_slice(&crate::managed_storage::VALUE_BOX.to_le_bytes());
                descriptor[8..12].copy_from_slice(&box_ref.slot.to_le_bytes());
                descriptor[12..16].copy_from_slice(&box_ref.generation.to_le_bytes());
                descriptor[16..20].copy_from_slice(&(inner.bytes.len() as u32).to_le_bytes());
                descriptor[20..24].copy_from_slice(&0_u32.to_le_bytes());
                return Ok(PreparedPayload {
                    bytes: descriptor,
                    references: vec![(8, box_ref)],
                });
            }
            let mut concrete = value;
            while let crate::value::Data::Interface(inner) = &concrete.data {
                concrete = inner.as_ref();
            }
            let concrete_type = Self::dynamic_value_type(concrete);
            let concrete_value =
                concrete_type
                    .filter(|typ| *typ != concrete.typ)
                    .map(|typ| crate::value::Value {
                        typ,
                        data: concrete.data.clone(),
                    });
            let concrete = concrete_value.as_ref().unwrap_or(concrete);
            let registry = types.as_deref().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
            if matches!(concrete.data, crate::value::Data::Nil) {
                let mut bytes = vec![0; 24];
                if !registry.is_interface(&concrete.typ)? {
                    let type_id = registry.storage_type_id(&concrete.typ)?;
                    bytes[0..4].copy_from_slice(&type_id.to_le_bytes());
                    bytes[4..8]
                        .copy_from_slice(&crate::managed_storage::VALUE_TYPED_NIL.to_le_bytes());
                }
                return Ok(PreparedPayload {
                    bytes,
                    references: Vec::new(),
                });
            }
            let inner = if matches!(
                &concrete.data,
                crate::value::Data::Function(_)
                    | crate::value::Data::DynamicFunction(_)
                    | crate::value::Data::Method { .. }
                    | crate::value::Data::Resource(_)
            ) {
                Some(
                    if matches!(&concrete.data, crate::value::Data::DynamicFunction(_)) {
                        self.prepared_dynamic_function_payload(concrete, registry)?
                    } else {
                        self.prepared_record_payload(concrete, registry)?
                    },
                )
            } else {
                self.prepared_inline_payload(concrete)?
            };
            if let Some(inner) = inner {
                let mut descriptor = vec![0_u8; 24];
                let type_id = registry.storage_type_id(&concrete.typ)?;
                descriptor[0..4].copy_from_slice(&type_id.to_le_bytes());
                // Primitive/complex lanes fit directly in the dynamic
                // payload. Strings, slices, pointers and aggregates
                // retain a typed immutable box because their descriptor
                // carries managed edges or backing storage.
                if inner.references.is_empty()
                    && inner.bytes.len() <= 16
                    && types
                        .as_deref()
                        .and_then(|registry| {
                            registry.storage_fixed_words(&concrete.typ).ok().flatten()
                        })
                        .is_some()
                {
                    descriptor[4..8]
                        .copy_from_slice(&crate::managed_storage::VALUE_BITS.to_le_bytes());
                    descriptor[8..8 + inner.bytes.len()].copy_from_slice(&inner.bytes);
                    return Ok(PreparedPayload {
                        bytes: descriptor,
                        references: Vec::new(),
                    });
                }
                let kind = if matches!(
                    concrete.data,
                    crate::value::Data::Function(_)
                        | crate::value::Data::DynamicFunction(_)
                        | crate::value::Data::Method { .. }
                        | crate::value::Data::Resource(_)
                ) {
                    crate::managed_storage::StoragePayloadKind::Record
                } else {
                    crate::managed_storage::StoragePayloadKind::Typed
                };
                let box_ref = self.allocate_storage_box(
                    &inner.bytes,
                    &inner.references,
                    kind,
                    Some(concrete.typ.clone()),
                )?;
                descriptor[4..8].copy_from_slice(&crate::managed_storage::VALUE_BOX.to_le_bytes());
                descriptor[8..12].copy_from_slice(&box_ref.slot.to_le_bytes());
                descriptor[12..16].copy_from_slice(&box_ref.generation.to_le_bytes());
                descriptor[16..20].copy_from_slice(&(inner.bytes.len() as u32).to_le_bytes());
                descriptor[20..24].copy_from_slice(&0_u32.to_le_bytes());
                return Ok(PreparedPayload {
                    bytes: descriptor,
                    references: vec![(8, box_ref)],
                });
            }
        }
        Err(RuntimeError::new(
            "typed_payload_unsupported",
            "heap",
            "value has no prepared storage layout",
        ))
    }

    /// Materializes an untyped aggregate into an instance-owned structural
    /// type, then stores its exact inline layout in one immutable typed box.
    /// This keeps Any arrays/structs on the same prepared representation as
    /// concrete aggregates instead of a recursive semantic record or a
    /// pseudo Any backing tag.
    fn prepared_any_aggregate_payload(
        &self,
        value: &crate::value::Value,
        backing: bool,
    ) -> Result<PreparedPayload, RuntimeError> {
        let concrete_type = if let crate::value::Data::Aggregate(aggregate) = &value.data {
            aggregate.typ().clone()
        } else {
            let mut registry = self.type_registry.write().unwrap();
            let registry = registry.as_mut().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
            let registry = Arc::make_mut(registry);
            let before = registry.dynamic_stats();
            let shape = match &value.data {
                crate::value::Data::Array(values) => {
                    let element = values
                        .first()
                        .map(|value| value.typ.clone())
                        .unwrap_or(crate::types::TypeIdentity::Any);
                    crate::types::ConstructedType::Array {
                        length: values.len(),
                        element,
                    }
                }
                crate::value::Data::Struct(fields) => {
                    let fields = fields
                        .iter()
                        .map(|(name, value)| crate::types::ConstructedField {
                            name: name.clone(),
                            typ: value.typ.clone(),
                            tag: String::new(),
                            embedded: false,
                        })
                        .collect();
                    crate::types::ConstructedType::Struct(fields)
                }
                _ => {
                    return Err(RuntimeError::new(
                        "type_error",
                        "heap",
                        "dynamic backing requires an aggregate",
                    ));
                }
            };
            let (max_nodes, max_bytes) = self.dynamic_type_limits;
            let depth = registry.traversal_limit(max_nodes);
            let typ = registry.construct(shape, max_nodes, depth)?;
            registry.register_dynamic(&typ, max_nodes, max_bytes, depth)?;
            if registry.dynamic_stats() != before {
                self.mark_type_registry_changed();
            }
            typ
        };
        let concrete = crate::value::Value {
            typ: concrete_type.clone(),
            data: value.data.clone(),
        };
        let inner = self.prepared_inline_payload(&concrete)?.ok_or_else(|| {
            RuntimeError::new(
                "typed_payload_unsupported",
                "heap",
                "aggregate backing has no prepared layout",
            )
        })?;
        let box_ref = self.allocate_prepared_box(&inner.bytes, &inner.references)?;
        let registry = self.type_registry.read().unwrap();
        let type_id = registry
            .as_deref()
            .ok_or_else(|| RuntimeError::new("invalid_type", "heap", "missing type registry"))?
            .storage_type_id(&concrete_type)?;
        let mut descriptor = vec![0_u8; 24];
        descriptor[0..4].copy_from_slice(&type_id.to_le_bytes());
        descriptor[4..8].copy_from_slice(&crate::managed_storage::VALUE_BOX.to_le_bytes());
        descriptor[8..12].copy_from_slice(&box_ref.slot.to_le_bytes());
        descriptor[12..16].copy_from_slice(&box_ref.generation.to_le_bytes());
        descriptor[16..20].copy_from_slice(&(inner.bytes.len() as u32).to_le_bytes());
        descriptor[20..24].copy_from_slice(
            &if backing {
                crate::managed_storage::VALUE_BOX_BACKING
            } else {
                0
            }
            .to_le_bytes(),
        );
        Ok(PreparedPayload {
            bytes: descriptor,
            references: vec![(8, box_ref)],
        })
    }

    /// Returns the shared guest accounting width for one managed value. This
    /// is deliberately independent of the physical prepared bytes: variable
    /// payload data lives in typed boxes or container storage and is charged
    /// by the owner that allocates that storage.
    pub(crate) fn logical_allocation_bytes(
        &self,
        value: &crate::value::Value,
    ) -> Result<u64, RuntimeError> {
        let width = match &value.data {
            // A closure owns one code anchor plus one reference lane per
            // capture. Its declared type is Any at the bytecode boundary, so
            // charging the declared dynamic descriptor would overcharge the
            // managed function record (the capture record is 24B empty).
            crate::value::Data::Function(function) => {
                let captures = (function.captures.len() as u64)
                    .checked_mul(crate::contract_generated::STORAGE_REFERENCE)
                    .ok_or_else(|| {
                        RuntimeError::new(
                            "allocation_limit",
                            "heap",
                            "function capture width overflows",
                        )
                    })?;
                crate::contract_generated::STORAGE_REFERENCE
                    .checked_add(captures)
                    .ok_or_else(|| {
                        RuntimeError::new("allocation_limit", "heap", "function width overflows")
                    })?
            }
            crate::value::Data::Method {
                function, receiver, ..
            } => {
                let base = if receiver.is_some() {
                    crate::contract_generated::STORAGE_REFERENCE
                        .checked_add(crate::contract_generated::STORAGE_DYNAMIC)
                } else {
                    Some(crate::contract_generated::STORAGE_REFERENCE)
                };
                base.and_then(|width| {
                    width.checked_add(
                        (function.captures.len() as u64)
                            .checked_mul(crate::contract_generated::STORAGE_REFERENCE)?,
                    )
                })
                .ok_or_else(|| {
                    RuntimeError::new("allocation_limit", "heap", "method width overflows")
                })?
            }
            _ => self
                .type_registry
                .read()
                .unwrap()
                .as_deref()
                .ok_or_else(|| RuntimeError::new("invalid_type", "heap", "missing type registry"))?
                .storage_logical_bytes(&value.typ)?,
        };
        crate::contract_generated::STORAGE_RECORD_HEADER
            .checked_add(width)
            .ok_or_else(|| RuntimeError::new("allocation_limit", "heap", "logical size overflow"))
    }

    /// Encodes one map lane using the shared typed-container contract. Fixed
    /// values are contiguous numeric bytes; dynamic values use the 24-byte
    /// descriptor and carry their child reference at offset eight.
    pub(crate) fn encode_map_lane(
        &self,
        value: &crate::value::Value,
        typ: &crate::types::TypeIdentity,
    ) -> Result<PreparedPayload, RuntimeError> {
        let registry = self.type_registry.read().unwrap();
        let dynamic = registry
            .as_deref()
            .ok_or_else(|| RuntimeError::new("invalid_type", "map", "missing type registry"))?
            .is_interface(typ)?;
        drop(registry);
        let payload = if dynamic {
            self.prepared_dynamic_payload(value)?
        } else {
            self.prepared_payload(value)?
        };
        let registry = self.type_registry.read().unwrap();
        let width = registry
            .as_deref()
            .and_then(|types| types.storage_fixed_words(typ).ok().flatten());
        drop(registry);
        if width.is_none() {
            return Ok(payload);
        }
        let width = width.unwrap();
        let expected = width.checked_mul(8).ok_or_else(|| {
            RuntimeError::new("allocation_limit", "map", "map lane width overflows")
        })?;
        if payload.bytes.len() != expected || !payload.references.is_empty() {
            return Err(RuntimeError::new(
                "type_error",
                "map",
                "map lane contains unsupported dynamic references",
            ));
        }
        Ok(payload)
    }

    /// Decodes a map lane at the semantic boundary. The map page remains
    /// numeric; this temporary `Value` is created only for equality, indexing,
    /// iteration, reflection, or snapshot materialization.
    pub(crate) fn decode_map_lane(
        &self,
        typ: &crate::types::TypeIdentity,
        bytes: &[u8],
        references: &[(u32, StorageRef)],
    ) -> Result<crate::value::Value, RuntimeError> {
        let registry =
            self.type_registry.read().unwrap().clone().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
        let layout = registry.storage_lane_layout(typ)?;
        if let Some(value) = self.decode_inline_payload(&registry, &layout, typ, bytes)? {
            return Ok(value);
        }
        if layout.kind == crate::contract_generated::Interface
            && bytes.len() == 24
            && (bytes[4..8] == crate::managed_storage::VALUE_REF.to_le_bytes()
                || bytes[4..8] == crate::managed_storage::VALUE_BOX.to_le_bytes())
        {
            let has_child = references
                .iter()
                .find(|(offset, _)| *offset == 8)
                .map(|(_, child)| *child)
                .filter(|child| *child != StorageRef::NULL)
                .is_some();
            let descriptor_is_nil = bytes[8..16].iter().all(|byte| *byte == 0);
            if !has_child && !descriptor_is_nil {
                return Err(RuntimeError::new(
                    "typed_payload_invalid",
                    "map",
                    "missing dynamic lane reference",
                ));
            }
            return self.decode_prepared_value(typ, bytes);
        }
        Err(RuntimeError::new(
            "typed_payload_invalid",
            "map",
            "map lane does not match its prepared type",
        ))
    }

    /// Decodes one fixed ring element produced by `prepared_payload`. The
    /// ring stores the same numeric header and prepared lanes as an ordinary
    /// managed value, so receiving does not allocate a temporary heap object.
    fn decode_inline_payload(
        &self,
        registry: &TypeRegistry,
        layout: &crate::types::StorageLaneLayout,
        typ: &crate::types::TypeIdentity,
        payload: &[u8],
    ) -> Result<Option<crate::value::Value>, RuntimeError> {
        let is_interface = layout.kind == crate::contract_generated::Interface;
        if layout.kind == crate::contract_generated::Primitive && layout.fixed {
            if payload.len() != layout.stride as usize || !matches!(payload.len(), 8 | 16) {
                return Ok(None);
            }
            let bits = [
                u64::from_le_bytes(payload[..8].try_into().unwrap()),
                if payload.len() == 16 {
                    u64::from_le_bytes(payload[8..16].try_into().unwrap())
                } else {
                    0
                },
            ];
            return Ok(Self::decode_prepared_scalar(
                &crate::types::TypeIdentity::Primitive(layout.primitive),
                bits,
            )
            .map(|mut value| {
                value.typ = typ.clone();
                value
            }));
        }
        let record_lane = is_interface
            || layout.kind == crate::contract_generated::Function
            || (layout.kind == crate::contract_generated::Primitive
                && layout.primitive == crate::contract_generated::PrimitiveFunction);
        if record_lane {
            // A nil function or interface is represented by an all-zero lane.
            // Do this before attempting to resolve a record handle: NULL is
            // the canonical zero value and must never go through the stale
            // object path.
            if payload.iter().all(|byte| *byte == 0) {
                return Ok(Some(crate::value::Value {
                    typ: typ.clone(),
                    data: crate::value::Data::Nil,
                }));
            }
            if let Some(value) = self.decode_record_lane(typ.clone(), payload)? {
                return Ok(Some(value));
            }
        }
        // Interface lanes carry the concrete stable type ID followed by an
        // explicit representation.  `Bits` keeps primitive payload bytes
        // inline and therefore does not allocate a box or a second edge.
        if is_interface
            && payload.len() == 24
            && payload[4..8] == crate::managed_storage::VALUE_TYPED_NIL.to_le_bytes()
        {
            let type_id = u32::from_le_bytes(payload[0..4].try_into().unwrap());
            let concrete = registry
                .identity_for_storage_type_id(type_id)
                .ok_or_else(|| {
                    RuntimeError::new("unknown_type", "heap", "typed nil has no concrete type")
                })?;
            return Ok(Some(crate::value::Value {
                typ: typ.clone(),
                data: crate::value::Data::Interface(Arc::new(crate::value::Value {
                    typ: concrete,
                    data: crate::value::Data::Nil,
                })),
            }));
        }
        if is_interface
            && payload.len() == 24
            && payload[4..8] == crate::managed_storage::VALUE_BITS.to_le_bytes()
        {
            let type_id = u32::from_le_bytes(payload[0..4].try_into().unwrap());
            let concrete = registry
                .identity_for_storage_type_id(type_id)
                .ok_or_else(|| {
                    RuntimeError::new(
                        "unknown_type",
                        "heap",
                        format!("dynamic type ID {type_id} is missing"),
                    )
                })?;
            let width = registry.storage_lane_layout(&concrete)?.stride as usize;
            if width > 16 || 8 + width > payload.len() {
                return Err(RuntimeError::new(
                    "typed_payload_invalid",
                    "heap",
                    "dynamic scalar lane width is invalid",
                ));
            }
            let inner = self.decode_prepared_value(&concrete, &payload[8..8 + width])?;
            return Ok(Some(if is_interface {
                crate::value::Value {
                    typ: typ.clone(),
                    data: crate::value::Data::Interface(Arc::new(inner)),
                }
            } else {
                inner
            }));
        }
        if is_interface
            && payload.len() == 24
            && payload[4..8] == crate::managed_storage::VALUE_REF.to_le_bytes()
        {
            let root = StorageRef::from_payload_identity(u64::from_le_bytes(
                payload[8..16].try_into().unwrap(),
            ));
            let flags = u32::from_le_bytes(payload[20..24].try_into().unwrap());
            if !root.valid() {
                return Ok(Some(crate::value::Value {
                    typ: typ.clone(),
                    data: crate::value::Data::Nil,
                }));
            }
            let data = match flags {
                crate::managed_storage::VALUE_REF_MAP => crate::value::Data::Map(root),
                crate::managed_storage::VALUE_REF_RESOURCE => crate::value::Data::ResourceRef(root),
                crate::managed_storage::VALUE_REF_OPAQUE => {
                    let content = if root.valid() {
                        self.read_storage(
                            root,
                            0,
                            u32::from_le_bytes(payload[16..20].try_into().unwrap()),
                        )?
                    } else {
                        Vec::new()
                    };
                    crate::value::Data::OpaqueJSON(content.into())
                }
                crate::managed_storage::VALUE_REF_BYTES => {
                    let content = if root.valid() {
                        self.read_storage(
                            root,
                            0,
                            u32::from_le_bytes(payload[16..20].try_into().unwrap()),
                        )?
                    } else {
                        Vec::new()
                    };
                    crate::value::Data::Bytes(content)
                }
                _ => return Ok(None),
            };
            let inner = crate::value::Value {
                typ: typ.clone(),
                data,
            };
            if matches!(
                flags,
                crate::managed_storage::VALUE_REF_BYTES | crate::managed_storage::VALUE_REF_OPAQUE
            ) {
                return Ok(Some(inner));
            }
            return Ok(Some(if is_interface {
                crate::value::Value {
                    typ: typ.clone(),
                    data: crate::value::Data::Interface(Arc::new(inner)),
                }
            } else {
                inner
            }));
        }
        if payload.len() != layout.stride as usize {
            return Ok(None);
        }
        let data = match layout.kind {
            _ if is_interface && payload.iter().all(|byte| *byte == 0) => crate::value::Data::Nil,
            crate::contract_generated::Primitive
                if layout.primitive == crate::contract_generated::PrimitiveString =>
            {
                let root = StorageRef {
                    slot: u32::from_le_bytes(payload[0..4].try_into().unwrap()),
                    generation: u32::from_le_bytes(payload[4..8].try_into().unwrap()),
                };
                let offset = u32::try_from(u64::from_le_bytes(payload[8..16].try_into().unwrap()))
                    .map_err(|_| {
                        RuntimeError::new(
                            "invalid_value",
                            "string",
                            "string offset exceeds storage range",
                        )
                    })?;
                let length = u32::try_from(u64::from_le_bytes(payload[16..24].try_into().unwrap()))
                    .map_err(|_| {
                        RuntimeError::new(
                            "invalid_value",
                            "string",
                            "string length exceeds storage range",
                        )
                    })?;
                if !root.valid() {
                    return Ok(Some(crate::value::Value {
                        typ: typ.clone(),
                        data: crate::value::Data::String(Vec::new().into()),
                    }));
                }
                crate::value::Data::String(crate::value::ByteString::from_managed(
                    self.read_storage(root, offset, length)?,
                    self.owner_id(),
                    root,
                    u64::from(offset),
                ))
            }
            crate::contract_generated::Pointer => {
                let root = StorageRef::from_payload_identity(u64::from_le_bytes(
                    payload[0..8].try_into().unwrap(),
                ));
                if !root.valid() {
                    return Ok(Some(crate::value::Value {
                        typ: typ.clone(),
                        data: crate::value::Data::Nil,
                    }));
                }
                let offset = u64::from_le_bytes(payload[8..16].try_into().unwrap());
                let type_id = u32::from_le_bytes(payload[16..20].try_into().unwrap());
                let flags = u32::from_le_bytes(payload[20..24].try_into().unwrap());
                crate::value::Data::Pointer(Arc::new(crate::value::Address {
                    root,
                    path: Vec::new(),
                    identity: Arc::new(crate::value::PointerIdentity {
                        numeric: Some((offset, type_id, flags)),
                        ..Default::default()
                    }),
                }))
            }
            crate::contract_generated::Slice => {
                let root = StorageRef {
                    slot: u32::from_le_bytes(payload[0..4].try_into().unwrap()),
                    generation: u32::from_le_bytes(payload[4..8].try_into().unwrap()),
                };
                if !root.valid() {
                    return Ok(Some(crate::value::Value {
                        typ: typ.clone(),
                        data: crate::value::Data::Nil,
                    }));
                }
                let offset = u64::from_le_bytes(payload[8..16].try_into().unwrap());
                let type_id = u32::from_le_bytes(payload[16..20].try_into().unwrap());
                let flags = u32::from_le_bytes(payload[20..24].try_into().unwrap());
                crate::value::Data::Slice(Arc::new(crate::value::SliceValue {
                    storage: crate::value::Address {
                        root,
                        path: Vec::new(),
                        identity: Arc::new(crate::value::PointerIdentity {
                            numeric: (type_id != 0).then_some((offset, type_id, flags)),
                            ..Default::default()
                        }),
                    },
                    start: u64::from_le_bytes(payload[24..32].try_into().unwrap()) as usize,
                    length: u64::from_le_bytes(payload[32..40].try_into().unwrap()) as usize,
                    capacity: u64::from_le_bytes(payload[40..48].try_into().unwrap()) as usize,
                    identity: Arc::default(),
                }))
            }
            crate::contract_generated::Map | crate::contract_generated::Waitable
                if payload.len() == 8 && layout.references == [0] =>
            {
                let handle = StorageRef {
                    slot: u32::from_le_bytes(payload[0..4].try_into().unwrap()),
                    generation: u32::from_le_bytes(payload[4..8].try_into().unwrap()),
                };
                if !handle.valid() {
                    return Ok(Some(crate::value::Value {
                        typ: typ.clone(),
                        data: crate::value::Data::Nil,
                    }));
                }
                if layout.kind == crate::contract_generated::Waitable {
                    crate::value::Data::ResourceRef(handle)
                } else {
                    crate::value::Data::Map(handle)
                }
            }
            crate::contract_generated::Struct | crate::contract_generated::Array => {
                crate::value::Data::Aggregate(crate::value::PreparedAggregate::new(
                    self.clone(),
                    typ.clone(),
                    registry.storage_lane_layout(typ)?,
                    payload.into(),
                    0,
                )?)
            }
            _ => return Ok(None),
        };
        Ok(Some(crate::value::Value {
            typ: typ.clone(),
            data,
        }))
    }

    fn decode_record_payload(
        &self,
        typ: &crate::types::TypeIdentity,
        payload: &[u8],
    ) -> Result<crate::value::Value, RuntimeError> {
        let Some(tag) = payload.get(..4) else {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "heap",
                "managed record is truncated",
            ));
        };
        if tag == b"DF02" {
            return self.decode_dynamic_function_bytes(payload, typ.clone());
        }
        if tag == b"MD02" {
            return self.decode_method_payload(payload, typ.clone());
        }
        if tag == b"RS01" {
            return self.decode_resource_payload(payload, typ.clone());
        }
        if tag == b"FN02" {
            return self.decode_function_bytes(payload, typ.clone());
        }
        Err(RuntimeError::new(
            "typed_payload_invalid",
            "heap",
            "managed record has an unknown payload tag",
        ))
    }

    pub(crate) fn decode_prepared_value(
        &self,
        typ: &crate::types::TypeIdentity,
        payload: &[u8],
    ) -> Result<crate::value::Value, RuntimeError> {
        let registry =
            self.type_registry.read().unwrap().clone().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
        let layout = registry.storage_lane_layout(typ)?;
        self.decode_prepared_lane(typ, &layout, payload)
    }

    pub(crate) fn decode_prepared_lane(
        &self,
        typ: &crate::types::TypeIdentity,
        layout: &crate::types::StorageLaneLayout,
        payload: &[u8],
    ) -> Result<crate::value::Value, RuntimeError> {
        // Void has no storage lane.  It is materialized as a zero value by
        // the frame/slot path, so an empty payload is a valid value rather
        // than a truncated channel descriptor.
        if payload.is_empty() && *typ == crate::types::TypeIdentity::Void {
            return Ok(crate::value::Value {
                typ: typ.clone(),
                data: crate::value::Data::Nil,
            });
        }
        let registry =
            self.type_registry.read().unwrap().clone().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
        if let Some(value) = self.decode_inline_payload(&registry, layout, typ, payload)? {
            return Ok(value);
        }
        if payload.len() == 24
            && payload[4..8] == crate::managed_storage::VALUE_BOX.to_le_bytes()
            && layout.kind == crate::contract_generated::Interface
        {
            let child = StorageRef {
                slot: u32::from_le_bytes(payload[8..12].try_into().unwrap()),
                generation: u32::from_le_bytes(payload[12..16].try_into().unwrap()),
            };
            if !child.valid() {
                return Ok(crate::value::Value {
                    typ: typ.clone(),
                    data: crate::value::Data::Nil,
                });
            }
            let size = self.storage_size(child)?;
            let declared_len = u32::from_le_bytes(payload[16..20].try_into().unwrap()) as usize;
            if declared_len > size as usize {
                return Err(RuntimeError::new(
                    "typed_payload_invalid",
                    "channel",
                    "dynamic box width does not match descriptor",
                ));
            }
            // Storage slots are aligned to an 8-byte physical width, while
            // the descriptor records the exact prepared payload width. Read
            // only the logical bytes; padding is not part of the codec.
            let box_snapshot = self.read_storage_snapshot_range(child, 0, declared_len as u32)?;
            let encoded = box_snapshot.bytes.as_slice();
            // The box owner records the payload origin.  Only a child with
            // Record kind may consume a record tag; typed lanes are decoded
            // from their declared type even when their scalar bytes happen to
            // start with DF02/MD02/RS01/FN02.
            if box_snapshot.metadata.kind == crate::managed_storage::StoragePayloadKind::Record {
                return self.decode_record_payload(typ, encoded);
            }
            // A descriptor always carries the concrete runtime identity.  An
            // `Any` lane and a named interface lane both erase that identity
            // at their outer boundary, so decode the boxed bytes with the
            // descriptor type and wrap the result back in the declared
            // interface below.  Previously only `Any` took this path; a
            // named interface then attempted to decode its concrete box as
            // an interface lane and either lost methods or reported a typed
            // payload error.
            let concrete_type =
                if *typ == crate::types::TypeIdentity::Any || registry.is_interface(typ)? {
                    let type_id = u32::from_le_bytes(payload[0..4].try_into().unwrap());
                    registry
                        .identity_for_storage_type_id(type_id)
                        .ok_or_else(|| {
                            RuntimeError::new(
                                "unknown_type",
                                "heap",
                                format!("dynamic type ID {type_id} is missing"),
                            )
                        })?
                } else {
                    typ.clone()
                };
            if concrete_type != *typ {
                let inner = self.decode_prepared_value(&concrete_type, encoded)?;
                let box_flags = u32::from_le_bytes(payload[20..24].try_into().unwrap());
                if box_flags == crate::managed_storage::VALUE_BOX_BACKING
                    && *typ == crate::types::TypeIdentity::Any
                    && matches!(&inner.data, crate::value::Data::Aggregate(_))
                {
                    return Ok(crate::value::Value {
                        typ: typ.clone(),
                        data: inner.data,
                    });
                }
                return if *typ == crate::types::TypeIdentity::Any || registry.is_interface(typ)? {
                    Ok(crate::value::Value {
                        typ: typ.clone(),
                        data: crate::value::Data::Interface(Arc::new(inner)),
                    })
                } else {
                    Ok(inner)
                };
            }
            if let Some(value) = self.decode_inline_payload(&registry, layout, typ, encoded)? {
                return Ok(value);
            }
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "heap",
                "dynamic descriptor contains an invalid prepared lane",
            ));
        }
        if payload.len() < 8 {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "channel",
                "prepared channel payload is truncated",
            ));
        }
        Err(RuntimeError::new(
            "typed_payload_invalid",
            "channel",
            "prepared payload has no typed lane",
        ))
    }

    fn decode_prepared_scalar(
        typ: &crate::types::TypeIdentity,
        bits: [u64; 2],
    ) -> Option<crate::value::Value> {
        let data = match typ {
            crate::types::TypeIdentity::Primitive(crate::contract_generated::PrimitiveBool) => {
                crate::value::Data::Bool(bits[0] != 0)
            }
            crate::types::TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveInt
                | crate::contract_generated::PrimitiveInt8
                | crate::contract_generated::PrimitiveInt16
                | crate::contract_generated::PrimitiveInt32
                | crate::contract_generated::PrimitiveInt64,
            ) => crate::value::Data::Integer(bits[0] as i64),
            crate::types::TypeIdentity::Primitive(crate::contract_generated::PrimitiveUint8) => {
                crate::value::Data::Unsigned(bits[0])
            }
            crate::types::TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveUint
                | crate::contract_generated::PrimitiveUint16
                | crate::contract_generated::PrimitiveUint32
                | crate::contract_generated::PrimitiveUint64
                | crate::contract_generated::PrimitiveUintptr,
            ) => crate::value::Data::Unsigned(bits[0]),
            crate::types::TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveFloat32
                | crate::contract_generated::PrimitiveFloat64,
            ) => crate::value::Data::Float(f64::from_bits(bits[0])),
            crate::types::TypeIdentity::Primitive(
                crate::contract_generated::PrimitiveComplex64
                | crate::contract_generated::PrimitiveComplex128,
            ) => crate::value::Data::Complex {
                real: f64::from_bits(bits[0]),
                imag: f64::from_bits(bits[1]),
            },
            _ => return None,
        };
        Some(crate::value::Value {
            typ: typ.clone(),
            data,
        })
    }

    fn prepared_path_location(
        root: &crate::types::TypeIdentity,
        path: &[usize],
        registry: &TypeRegistry,
    ) -> Result<Option<(u32, crate::types::TypeIdentity)>, RuntimeError> {
        let mut typ = root.clone();
        let mut offset = 0_u32;
        let overflow =
            || RuntimeError::new("allocation_limit", "address", "prepared offset overflows");
        for &index in path {
            match registry.storage_layout(&typ)? {
                Some(crate::types::StorageTypeLayout::Struct { fields }) => {
                    let Some((_, child)) = fields.get(index) else {
                        return Ok(None);
                    };
                    for (_, preceding) in &fields[..index] {
                        offset = offset
                            .checked_add(registry.storage_lane_layout(preceding)?.stride)
                            .ok_or_else(overflow)?;
                    }
                    typ = child.clone();
                }
                Some(crate::types::StorageTypeLayout::Array { element, length })
                    if index < length =>
                {
                    let stride = registry.storage_lane_layout(&element)?.stride;
                    let delta = u32::try_from(index)
                        .map_err(|_| overflow())?
                        .checked_mul(stride)
                        .ok_or_else(overflow)?;
                    offset = offset.checked_add(delta).ok_or_else(overflow)?;
                    typ = element;
                }
                _ => return Ok(None),
            }
        }
        Ok(Some((offset, typ)))
    }

    /// Reads a numeric pointer location directly from the prepared bank.  A
    /// pointer descriptor stores this offset and stable view type ID, so a
    /// decoded pointer never needs to reconstruct a host path.
    pub(crate) fn read_prepared_location(
        &self,
        handle: Handle,
        offset: u64,
        type_id: u32,
        flags: u32,
    ) -> Result<Option<Value>, RuntimeError> {
        let registry =
            self.type_registry.read().unwrap().clone().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
        let (typ, lane) = registry.prepared_storage_type(type_id).ok_or_else(|| {
            RuntimeError::new("unknown_type", "heap", "unknown pointer view type")
        })?;
        let offset = u32::try_from(offset)
            .map_err(|_| RuntimeError::new("invalid_address", "pointer", "offset overflows"))?;
        let metadata = self.meta(handle)?;
        // Function, method, and resource values are managed records rather
        // than ordinary prepared lanes.  Their first eight bytes contain the
        // record tag plus version/header data, so treating that prefix as a
        // StorageRef would manufacture a stale reference (and can hide the
        // record decoder behind a numeric pointer fast path).  Let the normal
        // typed-value path decode the record instead.
        if metadata.kind == crate::managed_storage::StoragePayloadKind::Record {
            return Ok(None);
        }
        let storage_handle = if flags == POINTER_COMPACT_BYTES
            && metadata.kind == crate::managed_storage::StoragePayloadKind::RawBytes
        {
            handle
        } else if flags == POINTER_COMPACT_BYTES
            && metadata.typ == Some(crate::types::TypeIdentity::Any)
            && metadata.payload_len == 24
        {
            let mut descriptor = [0_u8; 24];
            self.read_storage_into(handle, 0, &mut descriptor)?;
            if descriptor[4..8] == crate::managed_storage::VALUE_REF.to_le_bytes()
                && descriptor[20..24] == crate::managed_storage::VALUE_REF_BYTES.to_le_bytes()
            {
                StorageRef::from_payload_identity(u64::from_le_bytes(
                    descriptor[8..16].try_into().unwrap(),
                ))
            } else {
                return Ok(None);
            }
        } else {
            handle
        };
        if !metadata.initialized
            || u64::from(offset)
                + u64::from(if flags == POINTER_COMPACT_BYTES {
                    Self::compact_byte_width(&typ, registry.as_ref())? as u32
                } else {
                    lane.stride
                })
                > u64::from(self.storage_size(storage_handle)?)
        {
            return Ok(None);
        }
        if flags == POINTER_COMPACT_BYTES {
            let width = Self::compact_byte_width(&typ, registry.as_ref())?;
            let bytes = self.read_storage(
                storage_handle,
                offset,
                u32::try_from(width).map_err(|_| {
                    RuntimeError::new("allocation_limit", "pointer", "byte view overflows")
                })?,
            )?;
            return self
                .decode_compact_byte_value(&typ, &bytes, registry.as_ref())
                .map(Some);
        }
        let bytes = self.read_storage(storage_handle, offset, lane.stride)?;
        self.decode_prepared_lane(&typ, &lane, &bytes).map(Some)
    }

    /// Updates one numeric pointer location with the same optimistic version
    /// and write-barrier semantics as a normal prepared field update.
    pub(crate) fn write_prepared_location(
        &self,
        handle: Handle,
        offset: u64,
        type_id: u32,
        flags: u32,
        replacement: &Value,
    ) -> Result<bool, RuntimeError> {
        let allocations = self.begin_allocation_scope();
        let registry =
            self.type_registry.read().unwrap().clone().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
        let (typ, lane) = registry.prepared_storage_type(type_id).ok_or_else(|| {
            RuntimeError::new("unknown_type", "heap", "unknown pointer view type")
        })?;
        if typ != replacement.typ && !registry.identical(&typ, &replacement.typ)? {
            return Ok(false);
        }
        let offset = u32::try_from(offset)
            .map_err(|_| RuntimeError::new("invalid_address", "pointer", "offset overflows"))?;
        let metadata = self.meta(handle)?;
        if flags == POINTER_COMPACT_BYTES {
            let bytes = Self::encode_compact_byte_value(&typ, replacement, registry.as_ref())?;
            let width = u32::try_from(bytes.len()).map_err(|_| {
                RuntimeError::new("allocation_limit", "pointer", "byte view overflows")
            })?;
            let storage_handle = if metadata.kind
                == crate::managed_storage::StoragePayloadKind::RawBytes
            {
                handle
            } else if metadata.typ == Some(crate::types::TypeIdentity::Any)
                && metadata.payload_len == 24
            {
                let mut descriptor = [0_u8; 24];
                self.read_storage_into(handle, 0, &mut descriptor)?;
                if descriptor[4..8] == crate::managed_storage::VALUE_REF.to_le_bytes()
                    && descriptor[20..24] == crate::managed_storage::VALUE_REF_BYTES.to_le_bytes()
                {
                    StorageRef::from_payload_identity(u64::from_le_bytes(
                        descriptor[8..16].try_into().unwrap(),
                    ))
                } else {
                    return Ok(false);
                }
            } else {
                handle
            };
            if u64::from(offset) + u64::from(width) > u64::from(self.storage_size(storage_handle)?)
            {
                return Ok(false);
            }
            let version = self.meta(storage_handle)?.version;
            let written =
                self.write_storage_range_if_version(storage_handle, offset, &bytes, version)?;
            if written {
                allocations.commit();
            }
            return Ok(written);
        }
        let payload = if registry.is_interface(&typ)? {
            self.prepared_dynamic_payload(replacement)?
        } else {
            self.prepared_payload(replacement)?
        };
        if payload.bytes.len() != lane.stride as usize {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "heap",
                "pointer view width mismatch",
            ));
        }
        self.publish_payload(&payload)?;
        let written =
            self.write_storage_range_if_version(handle, offset, &payload.bytes, metadata.version)?;
        if written {
            allocations.commit();
        }
        Ok(written)
    }

    /// Copy an already prepared span. Once worker-local allocations are
    /// published, storage performs the range copy under ordered page locks;
    /// same-page overlap uses memmove semantics without allocating a source
    /// Vec. The destination version and remembered-card barrier are updated by
    /// the same storage transaction.
    pub(crate) fn copy_prepared_range(
        &self,
        source: Handle,
        source_offset: u64,
        destination: Handle,
        destination_offset: u64,
        length: u64,
    ) -> Result<(), RuntimeError> {
        let source_offset = u32::try_from(source_offset)
            .map_err(|_| RuntimeError::new("invalid_slice", "copy", "source offset overflows"))?;
        let destination_offset = u32::try_from(destination_offset).map_err(|_| {
            RuntimeError::new("invalid_slice", "copy", "destination offset overflows")
        })?;
        let length = u32::try_from(length)
            .map_err(|_| RuntimeError::new("invalid_slice", "copy", "copy length overflows"))?;
        self.flush_worker_lease()?;
        loop {
            let version = self.meta(destination)?.version;
            let copied = self.storage.copy_range_if_version_shared(
                source,
                source_offset,
                destination,
                destination_offset,
                length,
                version,
            )?;
            if copied {
                return Ok(());
            }
        }
    }

    /// Prepared offsets locate nested fields without decoding the parent value.
    pub(crate) fn read_prepared_path(
        &self,
        handle: Handle,
        parent: &crate::types::TypeIdentity,
        path: &[usize],
    ) -> Result<Option<Value>, RuntimeError> {
        let metadata = self.meta(handle)?;
        if !metadata.initialized || metadata.typ.as_ref() != Some(parent) {
            return Ok(None);
        }
        let registry =
            self.type_registry.read().unwrap().clone().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
        let Some((offset, typ)) = Self::prepared_path_location(parent, path, &registry)? else {
            return Ok(None);
        };
        let layout = registry.storage_lane_layout(&typ)?;
        let bytes = self.read_storage(handle, offset, layout.stride)?;
        self.decode_prepared_lane(&typ, &layout, &bytes).map(Some)
    }

    /// Replaces only the selected typed lane. The shared storage owner checks
    /// the object version and publishes the write barrier with the byte copy.
    pub(crate) fn update_prepared_scalar_path_typed(
        &self,
        handle: Handle,
        root_type: &crate::types::TypeIdentity,
        path: &[usize],
        replacement: &crate::value::Value,
    ) -> Result<bool, RuntimeError> {
        let allocations = self.begin_allocation_scope();
        let meta = self.meta(handle)?;
        if !meta.initialized || meta.typ.as_ref() != Some(root_type) {
            return Ok(false);
        }
        let registry =
            self.type_registry.read().unwrap().clone().ok_or_else(|| {
                RuntimeError::new("invalid_type", "heap", "missing type registry")
            })?;
        let Some((offset, expected)) = Self::prepared_path_location(root_type, path, &registry)?
        else {
            return Ok(false);
        };
        if expected != *replacement.typ() {
            return Ok(false);
        }
        let payload = if registry.is_interface(&expected)? {
            self.prepared_dynamic_payload(replacement)?
        } else {
            self.prepared_payload(replacement)?
        };
        let layout = registry.storage_lane_layout(&expected)?;
        if payload.bytes.len() != layout.stride as usize {
            return Err(RuntimeError::new(
                "typed_payload_invalid",
                "heap",
                "replacement differs from declared lane width",
            ));
        }
        for (_, reference) in &payload.references {
            if reference.valid() && !self.is_live(*reference) {
                return Err(RuntimeError::new(
                    "stale_reference",
                    "heap",
                    "payload edge is not live",
                ));
            }
        }
        self.publish_payload(&payload)?;
        let written =
            self.write_storage_range_if_version(handle, offset, &payload.bytes, meta.version)?;
        if written {
            allocations.commit();
        }
        Ok(written)
    }

    pub(crate) fn read_typed_mutation_base(
        &self,
        handle: Handle,
    ) -> Result<(crate::value::Value, u64, u64), RuntimeError> {
        let snapshot = self.read_storage_snapshot(handle)?;
        let logical_bytes = snapshot.metadata.logical_bytes;
        let version = snapshot.metadata.version;
        let value = self.decode_typed_snapshot(snapshot)?;
        Ok((value, logical_bytes, version))
    }

    pub(crate) fn update_typed(
        &self,
        handle: Handle,
        expected_version: u64,
        bytes: u64,
        _edges_unchanged: bool,
        update: impl FnOnce(&mut crate::value::Value),
    ) -> Result<bool, RuntimeError> {
        let meta = self.meta(handle)?;
        if meta.version != expected_version {
            return Ok(false);
        };
        let mut next = self.read_typed_value(handle)?;
        update(&mut next);
        match self.replace_with_expected(handle, next, bytes, Some(expected_version)) {
            Ok(()) => Ok(true),
            Err((error, _)) if error.code == "concurrent_update" => Ok(false),
            Err((error, _)) => Err(error),
        }
    }

    fn decode_typed_snapshot(
        &self,
        snapshot: StorageSnapshot,
    ) -> Result<crate::value::Value, RuntimeError> {
        let StorageSnapshot {
            metadata: meta,
            bytes,
        } = snapshot;
        if !meta.initialized {
            let typ = meta.typ.clone().ok_or_else(|| {
                RuntimeError::new(
                    "typed_payload_invalid",
                    "heap",
                    "uninitialized value has no type",
                )
            })?;
            return Ok(crate::value::Value {
                typ,
                data: crate::value::Data::Uninitialized,
            });
        }
        if meta.kind == crate::managed_storage::StoragePayloadKind::RawBytes {
            return Ok(crate::value::Value {
                typ: meta.typ.clone().unwrap_or(crate::types::TypeIdentity::Any),
                data: crate::value::Data::Bytes(bytes),
            });
        }
        if matches!(
            meta.kind,
            crate::managed_storage::StoragePayloadKind::Typed
                | crate::managed_storage::StoragePayloadKind::Record
        ) {
            let typ = meta.typ.clone().ok_or_else(|| {
                RuntimeError::new(
                    "typed_payload_invalid",
                    "heap",
                    "prepared value has no type",
                )
            })?;
            let value = if meta.kind == crate::managed_storage::StoragePayloadKind::Record {
                self.decode_record_payload(&typ, &bytes)?
            } else {
                self.decode_prepared_value(&typ, &bytes)?
            };
            return Ok(value);
        }
        Err(RuntimeError::new(
            "typed_payload_invalid",
            "heap",
            "managed value has no prepared storage metadata",
        ))
    }

    /// Rehydrates a value directly from its typed page. The metadata and
    /// payload come from one storage snapshot, so a concurrent rewrite cannot
    /// change the decoder shape between the two reads.
    pub(crate) fn read_typed_value(
        &self,
        handle: Handle,
    ) -> Result<crate::value::Value, RuntimeError> {
        let snapshot = self.read_storage_snapshot(handle)?;
        self.decode_typed_snapshot(snapshot)
    }
}
