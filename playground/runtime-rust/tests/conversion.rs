mod support;

use mini_go::{
    HostValue, LoadOptions, Program, SnapshotLimits, contract_generated as wire,
    instance::{ExecutionLimits, Instance, PollStatus},
    snapshot::HostData,
};
use serde_json::json;
use std::sync::Arc;

#[test]
fn byte_append_growth_keeps_the_old_backing_and_copies_only_visible_bytes() {
    let slice = json!({"kind":wire::Slice,"node":"bytes"});
    let integer = json!({"kind":3,"primitive":3});
    let byte = json!({"kind":3,"primitive":9});
    let code = support::slot_code(
        json!([slice, byte, slice, integer]),
        &[
            (
                "load_local",
                json!({"local":"input"}),
                json!({"outputs":[0]}),
            ),
            ("const", json!({"constant":"byte"}), json!({"outputs":[1]})),
            (
                "append",
                json!({"count":1}),
                json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]}),
            ),
            (
                "store_local",
                json!({"local":"grown"}),
                json!({"inputs":[[0,2]],"release":[2]}),
            ),
            (
                "load_local",
                json!({"local":"grown"}),
                json!({"outputs":[2]}),
            ),
            ("zero", json!({"type":integer}), json!({"outputs":[3]})),
            ("const", json!({"constant":"byte"}), json!({"outputs":[1]})),
            (
                "store_index",
                json!({}),
                json!({"inputs":[[0,2],[0,3],[0,1]],"release":[2,3,1]}),
            ),
            (
                "load_local",
                json!({"local":"input"}),
                json!({"outputs":[0]}),
            ),
            (
                "load_local",
                json!({"local":"grown"}),
                json!({"outputs":[2]}),
            ),
            (
                "return",
                json!({"result_count":2}),
                json!({"inputs":[[0,0],[0,2]],"release":[0,2]}),
            ),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"bytes","kind":wire::Slice,"elem":{"kind":3,"primitive":9}}]},
        "constants":[{"id":"byte","type":{"kind":3,"primitive":9},"value":255}],
        "functions":[{"id":"fn.Main","signature":{"params":[{"type":slice}],"results":[slice,slice]},
            "locals":[{"id":"input","type":slice},{"id":"grown","type":slice}],"code":code}]
    }));
    let program = Arc::new(Program::load(&image, LoadOptions::default()).unwrap());
    let mut instance = Instance::new(
        program,
        ExecutionLimits {
            max_heap_bytes: 20_000,
            ..ExecutionLimits::default()
        },
    )
    .unwrap();
    let input = vec![128; 4096];
    instance.start_bytes("default", &input).unwrap();
    assert_eq!(instance.poll_steps(11).unwrap(), PollStatus::Ready);
    let snapshot = instance
        .snapshot_results(SnapshotLimits::default())
        .unwrap();
    instance.close().unwrap();
    assert_eq!(snapshot.bytes(&snapshot.roots[0]).unwrap(), input);
    let grown = snapshot.bytes(&snapshot.roots[1]).unwrap();
    assert_eq!(grown.len(), 4097);
    assert_eq!(grown[0], 255);
    assert_eq!(grown[4096], 255);
    assert_eq!(&grown[1..4096], &input[1..]);
    assert_eq!(instance.heap_stats().live_bytes, 0);
}

#[test]
fn byte_backing_mutation_preserves_pointer_aliases_with_a_byte_sized_budget() {
    let slice = json!({"kind":wire::Slice,"node":"bytes"});
    let array = json!({"kind":wire::Array,"node":"pair"});
    let pointer = json!({"kind":wire::Pointer,"node":"pointer"});
    let code = support::slot_code(
        json!([slice,pointer,array,{"kind":3,"primitive":3},{"kind":3,"primitive":9}]),
        &[
            (
                "load_local",
                json!({"local":"input"}),
                json!({"outputs":[0]}),
            ),
            (
                "convert",
                json!({"type":pointer}),
                json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
            ),
            (
                "store_local",
                json!({"local":"pointer"}),
                json!({"inputs":[[0,1]],"release":[1]}),
            ),
            (
                "load_local",
                json!({"local":"pointer"}),
                json!({"outputs":[1]}),
            ),
            ("zero", json!({"type":array}), json!({"outputs":[2]})),
            (
                "store_indirect",
                json!({}),
                json!({"inputs":[[0,1],[0,2]],"release":[1,2]}),
            ),
            (
                "load_local",
                json!({"local":"input"}),
                json!({"outputs":[0]}),
            ),
            ("const", json!({"constant":"one"}), json!({"outputs":[3]})),
            ("const", json!({"constant":"byte"}), json!({"outputs":[4]})),
            (
                "store_index",
                json!({}),
                json!({"inputs":[[0,0],[0,3],[0,4]],"release":[0,3,4]}),
            ),
            (
                "load_local",
                json!({"local":"input"}),
                json!({"outputs":[0]}),
            ),
            (
                "load_local",
                json!({"local":"pointer"}),
                json!({"outputs":[1]}),
            ),
            (
                "return",
                json!({"result_count":2}),
                json!({"inputs":[[0,0],[0,1]],"release":[0,1]}),
            ),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[
            {"id":"bytes","kind":wire::Slice,"elem":{"kind":3,"primitive":9}},
            {"id":"pair","kind":wire::Array,"length":2,"elem":{"kind":3,"primitive":9}},
            {"id":"pointer","kind":wire::Pointer,"elem":array}
        ]},
        "constants":[{"id":"one","type":{"kind":3,"primitive":3},"value":1},
            {"id":"byte","type":{"kind":3,"primitive":9},"value":255}],
        "functions":[{"id":"fn.Main","signature":{"params":[{"type":slice}],"results":[slice,pointer]},
            "locals":[{"id":"input","type":slice},{"id":"pointer","type":pointer}],"code":code}]
    }));
    let program = Arc::new(Program::load(&image, LoadOptions::default()).unwrap());
    let mut instance = Instance::new(
        program,
        ExecutionLimits {
            max_heap_bytes: 5120,
            ..ExecutionLimits::default()
        },
    )
    .unwrap();
    let input = vec![128; 4096];
    instance.start_bytes("default", &input).unwrap();
    assert_eq!(instance.poll_steps(13).unwrap(), PollStatus::Ready);
    let snapshot = instance
        .snapshot_results(SnapshotLimits::default())
        .unwrap();
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_bytes, 0);
    let bytes = snapshot.bytes(&snapshot.roots[0]).unwrap();
    assert_eq!(&bytes[..3], &[0, 255, 128]);
    assert_eq!(bytes.len(), input.len());
    assert_eq!(&input[..3], &[128; 3]);
    let HostData::Pointer(address) = &snapshot.roots[1].data else {
        panic!("expected pointer")
    };
    let array = snapshot.resolve_address(address).unwrap();
    let HostData::Array(values) = &array.data else {
        panic!("expected array view")
    };
    assert!(matches!(values[0].data, HostData::Unsigned(0)));
    assert!(matches!(values[1].data, HostData::Unsigned(255)));
}

#[test]
fn repeated_pointer_type_views_write_original_storage_and_survive_in_snapshots() {
    let a = json!({"kind":4,"node":"A","named":{"module_path":"test","decl_id":"A"}});
    let b = json!({"kind":4,"node":"B","named":{"module_path":"test","decl_id":"B"}});
    let pa = json!({"kind":8,"node":"pointer.A"});
    let pb = json!({"kind":8,"node":"pointer.B"});
    let mut operations = vec![(
        "address_of",
        json!({"kind":"local","local":"value"}),
        json!({"outputs":[0]}),
    )];
    for index in 0..3001 {
        let source = index % 2;
        let target = 1 - source;
        operations.push((
            "convert",
            json!({"type":if target == 1 { &pb } else { &pa }}),
            json!({"inputs":[[0,source]],"outputs":[target],"release":[source]}),
        ));
    }
    operations.extend([
        (
            "store_local",
            json!({"local":"pointer"}),
            json!({"inputs":[[0,1]],"release":[1]}),
        ),
        (
            "load_local",
            json!({"local":"pointer"}),
            json!({"outputs":[1]}),
        ),
        (
            "const",
            json!({"constant":"answer"}),
            json!({"outputs":[2]}),
        ),
        (
            "store_indirect",
            json!({}),
            json!({"inputs":[[0,1],[0,2]],"release":[1,2]}),
        ),
        (
            "load_local",
            json!({"local":"pointer"}),
            json!({"outputs":[1]}),
        ),
        (
            "load_local",
            json!({"local":"value"}),
            json!({"outputs":[3]}),
        ),
        (
            "return",
            json!({"result_count":2}),
            json!({"inputs":[[0,1],[0,3]],"release":[1,3]}),
        ),
    ]);
    let code = support::slot_code(json!([pa, pb, b, a]), &operations);
    let image = support::image(json!({
        "type_table":{"nodes":[
            {"id":"A","kind":4,"identity":{"module_path":"test","decl_id":"A"},"underlying":{"kind":3,"primitive":3}},
            {"id":"B","kind":4,"identity":{"module_path":"test","decl_id":"B"},"underlying":{"kind":3,"primitive":3}},
            {"id":"pointer.A","kind":8,"elem":a}, {"id":"pointer.B","kind":8,"elem":b}
        ]},
        "constants":[{"id":"answer","type":b,"value":42}],
        "functions":[{"id":"fn.Main","signature":{"results":[pb,a]},
            "locals":[{"id":"value","type":a},{"id":"pointer","type":pb}],"code":code}]
    }));
    let program = Arc::new(Program::load(&image, LoadOptions::default()).unwrap());
    let expected = program
        .types()
        .resolve("test", &serde_json::from_value(b).unwrap())
        .unwrap();
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.start("default", vec![]).unwrap();
    assert_eq!(instance.poll_steps(4000).unwrap(), PollStatus::Ready);
    let snapshot = instance
        .snapshot_results(SnapshotLimits::default())
        .unwrap();
    let HostData::Pointer(address) = &snapshot.roots[0].data else {
        panic!("expected pointer")
    };
    assert_eq!(
        address.path.len(),
        1,
        "views retain the original storage without intermediate views"
    );
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
    let value = snapshot.resolve_address(address).unwrap();
    assert_eq!(value.typ, expected);
    assert!(matches!(value.data, HostData::Integer(42)));
    assert!(matches!(snapshot.roots[1].data, HostData::Integer(42)));
}

#[test]
fn array_pointer_conversion_preserves_snapshot_view_and_faults_on_short_input() {
    for target_kind in [wire::Array, wire::Pointer] {
        let slice = json!({"kind":wire::Slice,"node":"slice"});
        let array = json!({"kind":wire::Array,"node":"array"});
        let pointer = json!({"kind":wire::Pointer,"node":"pointer"});
        let target = if target_kind == wire::Array {
            &array
        } else {
            &pointer
        };
        let code = support::slot_code(
            json!([slice, target]),
            &[
                (
                    "load_local",
                    json!({"local":"input"}),
                    json!({"outputs":[0]}),
                ),
                (
                    "convert",
                    json!({"type":target}),
                    json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
                ),
                (
                    "return",
                    json!({"result_count":1}),
                    json!({"inputs":[[0,1]],"release":[1]}),
                ),
            ],
        );
        let image = support::image(json!({
            "type_table":{"nodes":[
                {"id":"slice","kind":wire::Slice,"elem":{"kind":3,"primitive":3}},
                {"id":"array","kind":wire::Array,"length":2,"elem":{"kind":3,"primitive":3}},
                {"id":"pointer","kind":wire::Pointer,"elem":array}
            ]},
            "functions":[{"id":"fn.Main","signature":{"params":[{"type":slice}],"results":[target]},
                "locals":[{"id":"input","type":slice}],"code":code}]
        }));
        let program = Arc::new(Program::load(&image, LoadOptions::default()).unwrap());
        let typ = program
            .types()
            .resolve("test", &serde_json::from_value(slice).unwrap())
            .unwrap();
        let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
        let mut input = HostValue {
            typ,
            data: HostData::Array(vec![
                HostValue::int(20),
                HostValue::int(22),
                HostValue::int(99),
            ]),
        };
        instance
            .start_host("default", std::slice::from_ref(&input))
            .unwrap();
        assert_eq!(instance.poll_steps(3).unwrap(), PollStatus::Ready);
        let snapshot = instance
            .snapshot_results(SnapshotLimits::default())
            .unwrap();
        let root = &snapshot.roots[0];
        let value = match &root.data {
            HostData::Pointer(address) => snapshot.resolve_address(address).unwrap(),
            _ => std::borrow::Cow::Borrowed(root),
        };
        let HostData::Array(values) = &value.data else {
            panic!("expected array view")
        };
        assert_eq!(values.len(), 2);
        assert!(matches!(values[0].data, HostData::Integer(20)));
        assert!(matches!(values[1].data, HostData::Integer(22)));
        input.data = HostData::Array(vec![HostValue::int(1)]);
        instance.start_host("default", &[input]).unwrap();
        assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Running);
        assert_eq!(instance.poll_steps(1).unwrap_err().code, "type_error");
        assert_eq!(
            instance.start("default", vec![]).unwrap_err().code,
            "faulted"
        );
        instance.close().unwrap();
        assert_eq!(instance.heap_stats().live_objects, 0);
        assert!(matches!(values[1].data, HostData::Integer(22)));
    }
}
