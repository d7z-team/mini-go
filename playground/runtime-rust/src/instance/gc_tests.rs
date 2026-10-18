use super::*;
use serde::Deserialize;

#[path = "../../tests/support/execution_vectors.rs"]
mod execution_vectors;

#[derive(Deserialize)]
struct Vector {
    name: String,
    optimization: u8,
    image: Box<serde_json::value::RawValue>,
    result_integer: String,
}

#[test]
fn reflected_type_construction_roots_prepared_payload_until_owner_release() {
    let vector = execution_vectors::load::<Vector>()
        .into_iter()
        .find(|vector| vector.name == "stdlib_reflect_value" && vector.optimization == 2)
        .unwrap();
    let program =
        Arc::new(Program::load(vector.image.get().as_bytes(), Default::default()).unwrap());
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    let expected = TypeIdentity::Primitive(wire::PrimitiveInt);
    let reflected = vm.reflect_type(&expected).unwrap();
    let mut references = Vec::new();
    reflected.trace(&mut |handle| references.push(handle));
    assert!(!references.is_empty());
    vm.collect_rooted().unwrap();
    assert_eq!(vm.reflected_type(&reflected).unwrap(), expected);
    assert!(references.iter().all(|handle| vm.heap.is_live(*handle)));
    vm.running.transient_roots.clear();
    vm.collect_rooted().unwrap();
    assert!(references.iter().all(|handle| !vm.heap.is_live(*handle)));
    vm.close().unwrap();
}

#[test]
fn panic_and_recovery_owners_keep_the_managed_payload_graph() {
    for recovered in [false, true] {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "code":test_helpers::micro_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", &[]).unwrap();
        if vm.running.frames.is_empty() {
            assert!(vm.schedule_next());
        }
        let pointee = vm.allocate(Value::int(42)).unwrap();
        let panic = vm
            .allocate(Value {
                typ: TypeIdentity::Any,
                data: Data::Array(
                    vec![Value {
                        typ: TypeIdentity::Pointer(Arc::new(TypeIdentity::Primitive(
                            wire::PrimitiveInt,
                        ))),
                        data: Data::Pointer(Arc::new(Address {
                            root: pointee,
                            path: Vec::new(),
                            identity: Arc::default(),
                        })),
                    }]
                    .into(),
                ),
            })
            .unwrap();
        let frame = vm.running.frames.last_mut().unwrap();
        if recovered {
            frame.recovered = Some(panic);
        } else {
            frame.panic = Some(panic);
        }
        vm.running.transient_roots.clear();
        vm.collect_minor_rooted().unwrap();
        vm.collect_rooted().unwrap();
        assert!(vm.heap.is_live(panic));
        assert_eq!(vm.heap.get(pointee).unwrap().integer().unwrap(), 42);
        let frame = vm.running.frames.last_mut().unwrap();
        frame.panic = None;
        frame.recovered = None;
        vm.collect_rooted().unwrap();
        assert!(!vm.heap.is_live(panic));
        assert!(!vm.heap.is_live(pointee));
        vm.close().unwrap();
    }
}

#[test]
fn collection_during_allocation_keeps_the_unpublished_payload_type() {
    let program = test_helpers::program_with_artifact(|_| {});
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    let typ = vm
        .types
        .construct(
            crate::types::ConstructedType::Array {
                length: 3,
                element: TypeIdentity::Primitive(wire::PrimitiveInt),
            },
            32,
            16,
        )
        .unwrap();
    vm.types.register_dynamic(&typ, 32, 1 << 20, 16).unwrap();
    vm.heap.set_type_registry(vm.types.clone());
    vm.collect_major_after_bytes = 0;
    let handle = vm
        .allocate(Value {
            typ: typ.clone(),
            data: Data::Array(vec![Value::int(7), Value::int(20), Value::int(22)].into()),
        })
        .unwrap();
    assert!(vm.heap_stats().major_collections > 0);
    assert!(vm.running.allocation_roots.is_empty());
    let stored = vm.heap.read_typed_value(handle).unwrap();
    assert_eq!(
        (0..3)
            .map(|index| stored.element(index).unwrap().integer().unwrap())
            .collect::<Vec<_>>(),
        [7, 20, 22]
    );
    vm.collect_at_boundary().unwrap();
    assert!(!vm.heap.is_live(handle));
    assert!(vm.types.node(&typ).is_err());
    vm.close().unwrap();
}

#[test]
fn nursery_pressure_prefers_minor_recovery_for_young_churn() {
    let program = test_helpers::program_with_artifact(|artifact| {
        artifact["functions"] = serde_json::json!([
            {"id":"fn.Main", "code":test_helpers::micro_code(serde_json::json!([]), &[("return",serde_json::json!({}),serde_json::json!({}))])}
        ]);
    });
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    vm.start("default", &[]).unwrap();

    // Keep a stable old graph large enough that the young watermark would
    // otherwise exceed the physical nursery. The large string backings are
    // old from first publication; their descriptors remain rooted by globals.
    for index in 0..512 {
        let handle = vm.allocate(Value::string(vec![b'x'; 20_000])).unwrap();
        vm.globals
            .insert(("gc-test".into(), index.to_string()), handle);
        vm.running.transient_roots.clear();
    }
    vm.collect_rooted().unwrap();
    let nursery = vm.heap.nursery_pressure().unwrap();
    assert!(vm.collect_after_bytes <= nursery.limit);

    let before = vm.heap_stats();
    for index in 0..200_000 {
        let _ = vm.allocate(Value::int(index)).unwrap();
        vm.running.transient_roots.clear();
    }
    vm.heap.flush_worker_lease().unwrap();
    let after = vm.heap_stats();
    assert!(after.minor_collections > before.minor_collections);
    assert_eq!(after.major_collections, before.major_collections);
    vm.close().unwrap();
}

#[test]
fn instruction_boundary_collection_preserves_managed_execution_roots() {
    let scenarios = [
        "nested_value_storage",
        "managed_graph_churn",
        "frame_cell_rebind",
        "closure",
        "loop_capture",
        "channel_buffer",
        "defer_result",
        "recover_panic",
        "map_range",
        "select_transaction",
        "select_wait",
        "stdlib_reflect_slice",
        "slice_array_pointer",
    ];
    let mut covered = HashSet::new();
    for vector in execution_vectors::load::<Vector>() {
        if !scenarios.contains(&vector.name.as_str()) {
            continue;
        }
        let label = format!("{}/O{}", vector.name, vector.optimization);
        let program = Arc::new(
            Program::load(vector.image.get().as_bytes(), Default::default())
                .unwrap_or_else(|error| panic!("{label}: {error}")),
        );
        let entry = program.image().entries[0].name.clone();
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start(&entry, &[]).unwrap();
        let mut boundaries = 0;
        loop {
            // Poll has returned ownership of all worker storage. Both paths
            // use the real instance roots; nursery evacuation poisons the
            // former page so stale payload views fail on the next instruction.
            vm.running.transient_roots.clear();
            if boundaries % 7 == 0 {
                vm.collect_rooted()
                    .unwrap_or_else(|error| panic!("{label} major/{boundaries}: {error}"));
            } else {
                vm.collect_minor_rooted()
                    .unwrap_or_else(|error| panic!("{label} minor/{boundaries}: {error}"));
            }
            match vm.poll_steps(1) {
                Ok(PollStatus::Running) => boundaries += 1,
                Ok(PollStatus::Ready) => break,
                state => panic!("{label} boundary/{boundaries}: {state:?}"),
            }
        }
        assert_eq!(
            vm.guest_results()[0].integer().unwrap().to_string(),
            vector.result_integer,
            "{label}"
        );
        let stats = vm.heap_stats();
        assert!(stats.minor_collections > 0, "{label}: minor GC");
        assert!(stats.major_collections > 0, "{label}: major GC");
        assert!(stats.copied_bytes > 0, "{label}: payload evacuation");
        vm.close().unwrap();
        assert_eq!(vm.heap_stats().live_bytes, 0, "{label}: close");
        covered.insert(vector.name);
    }
    for scenario in scenarios {
        assert!(covered.contains(scenario), "missing fixture {scenario}");
    }
}

#[test]
fn returning_frame_gc_reclaims_dead_local_and_keeps_named_result() {
    let program = test_helpers::program_with_artifact(|artifact| {
        let string = serde_json::json!({
            "kind": wire::Primitive,
            "primitive": wire::PrimitiveString
        });
        artifact["functions"] = serde_json::json!([{
            "id": "fn.Main",
            "signature": {"results": [string]},
            "locals": [
                {"id": "dead", "type": string},
                {"id": "result", "type": string}
            ],
            "result_locals": ["result"],
            "code": test_helpers::micro_code(
                serde_json::json!([string]),
                &[
                    (
                        "load_local",
                        serde_json::json!({"local": "dead"}),
                        serde_json::json!({"outputs": [0]})
                    ),
                    (
                        "return",
                        serde_json::json!({"result_count": 1}),
                        serde_json::json!({"inputs": [[0, 0]], "release": [0]})
                    )
                ]
            )
        }]);
    });
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    vm.start("default", &[]).unwrap();
    if vm.running.frames.is_empty() {
        assert!(vm.schedule_next());
    }

    let dead = vm.allocate(Value::string("dead")).unwrap();
    let result = vm.allocate(Value::string("result")).unwrap();
    let frame = vm.running.frames.last_mut().unwrap();
    frame.locals[0] = frame::Local::Private(dead);
    frame.locals[1] = frame::Local::Private(result);
    frame.returning = Some(1);
    // This is the PC whose pre-return map still sees `dead` as an operand.
    frame.pc = 0;
    vm.running.transient_roots.clear();

    vm.collect_rooted().unwrap();
    assert!(
        !vm.heap.is_live(dead),
        "dead local was revived by return tracing"
    );
    assert!(
        vm.heap.is_live(result),
        "named result was lost during return tracing"
    );

    vm.finish_frame().unwrap();
    assert!(matches!(
        &vm.guest_results()[0].data,
        Data::String(bytes) if &**bytes == b"result"
    ));
    vm.close().unwrap();
}
