mod support;

use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    snapshot::{HostData, SnapshotLimits},
};
use serde_json::json;
use std::sync::Arc;

#[test]
fn detached_snapshot_preserves_cycles_and_survives_instance_close() {
    let map = json!({"kind":7,"node":"map"});
    let code = support::slot_code(
        json!([map,{"kind":3,"primitive":2},map]),
        &[
            ("make_map", json!({"type":map}), json!({"outputs":[0]})),
            (
                "store_local",
                json!({"local":"map"}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
            ("load_local", json!({"local":"map"}), json!({"outputs":[0]})),
            ("const", json!({"constant":"key"}), json!({"outputs":[1]})),
            ("load_local", json!({"local":"map"}), json!({"outputs":[2]})),
            (
                "store_index",
                json!({}),
                json!({"inputs":[[0,0],[0,1],[0,2]],"release":[0,1,2]}),
            ),
            ("load_local", json!({"local":"map"}), json!({"outputs":[0]})),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"map","kind":7,"key":{"kind":3,"primitive":2},"elem":{"kind":2}}]},
        "constants":[{"id":"key","type":{"kind":3,"primitive":2},"value":"self"}],
        "functions":[{"id":"fn.Main","signature":{"results":[map]},"locals":[{"id":"map","type":map}],"code":code}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.start("default", Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(32).unwrap(), PollStatus::Ready);
    let before = instance.heap_stats();
    assert_eq!(
        instance
            .snapshot_results(SnapshotLimits {
                max_objects: 0,
                ..SnapshotLimits::default()
            })
            .unwrap_err()
            .code,
        "snapshot_limit"
    );
    assert_eq!(instance.heap_stats(), before);
    let snapshot = instance
        .snapshot_results(SnapshotLimits::default())
        .unwrap();
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_bytes, 0);
    let HostData::Map(root) = snapshot.roots[0].data else {
        panic!("expected map root")
    };
    let HostData::MapEntries(entries) = &snapshot.objects[root].data else {
        panic!("expected map backing")
    };
    let HostData::Interface(value) = &entries[0].1.data else {
        panic!("expected dynamic map")
    };
    assert!(matches!(value.data, HostData::Map(index) if index == root));
}

#[test]
fn byte_snapshot_owns_binary_data_after_guest_storage_is_released() {
    let bytes = json!({"kind":5,"node":"bytes"});
    let code = support::slot_code(
        json!([bytes]),
        &[
            ("const", json!({"constant":"data"}), json!({"outputs":[0]})),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"bytes","kind":5,"elem":{"kind":3,"primitive":9}}]},
        "constants":[{"id":"data","type":bytes,"value":"AP+A"}],
        "functions":[{"id":"fn.Main","signature":{"results":[bytes]},"code":code}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.start("default", Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(8).unwrap(), PollStatus::Ready);
    let snapshot = instance
        .snapshot_results(SnapshotLimits::default())
        .unwrap();
    instance.close().unwrap();
    assert_eq!(snapshot.bytes(&snapshot.roots[0]).unwrap(), [0, 255, 128]);
}
