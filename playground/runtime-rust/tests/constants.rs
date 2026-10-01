mod support;

use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
};
use serde_json::json;
use std::sync::Arc;

#[test]
fn any_constants_follow_go_scalar_decoding_and_host_boundary_failures() {
    use mini_go::{SnapshotLimits, snapshot::HostData};
    let returning = support::slot_code(
        json!([{"kind":2}]),
        &[
            ("const", json!({"constant":"raw"}), json!({"outputs":[0]})),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
        ],
    );
    let discarded = support::slot_code(
        json!([{"kind":2}]),
        &[
            ("const", json!({"constant":"raw"}), json!({"outputs":[0]})),
            ("pop", json!({}), json!({"inputs":[[0,0]],"release":[0]})),
        ],
    );
    for (raw, expected) in [
        (json!(42), Some("42")),
        (json!("42"), Some("42")),
        (json!("+1"), Some("+1")),
        (json!("text"), Some("text")),
        (json!(true), Some("true")),
        (json!(null), Some("nil")),
        (json!([]), None),
        (json!({"x": 1}), None),
    ] {
        let image = support::image(json!({
            "constants":[{"id":"raw","type":{"kind":2},"value":raw}],
            "functions":[{"id":"fn.Main","signature":{"results":[{"kind":2}]},"code":returning}]
        }));
        let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
        let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
        instance.start("default", vec![]).unwrap();
        assert_eq!(instance.poll_steps(2).unwrap(), PollStatus::Ready);
        if let Some(expected) = expected {
            let snapshot = instance
                .snapshot_results(SnapshotLimits::default())
                .unwrap();
            let actual = match &snapshot.roots[0].data {
                HostData::Integer(value) => value.to_string(),
                HostData::Bool(value) => value.to_string(),
                HostData::String(bytes) => String::from_utf8(bytes.clone()).unwrap(),
                HostData::Nil => "nil".into(),
                data => panic!("unexpected scalar {data:?}"),
            };
            assert_eq!(actual, expected);
        } else {
            assert_eq!(
                instance
                    .snapshot_results(SnapshotLimits::default())
                    .unwrap_err()
                    .code,
                "snapshot_type"
            );
            assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Ready);
        }
        instance.close().unwrap();
        assert_eq!(instance.heap_stats().live_objects, 0);
    }
    for value in [json!(1.5), json!("1.5"), json!("9223372036854775808")] {
        let image = support::image(json!({
            "constants":[{"id":"raw","type":{"kind":2},"value":value}],
            "functions":[{"id":"fn.Main","code":discarded}]
        }));
        let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
        let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
        instance.start("default", vec![]).unwrap();
        assert_eq!(instance.poll_steps(1).unwrap_err().code, "invalid_constant");
        assert_eq!(
            instance.start("default", vec![]).unwrap_err().code,
            "faulted"
        );
        instance.close().unwrap();
        assert_eq!(instance.heap_stats().live_objects, 0);
    }
}

#[test]
fn preparation_validates_constant_ranges_and_exact_metadata() {
    for constant in [
        json!({"id":"bad", "type":{"kind":3,"primitive":4}, "value":128}),
        json!({"id":"bad", "type":{"kind":3,"primitive":9}, "value":-1}),
        json!({"id":"bad", "type":{"kind":3,"primitive":15}, "value":"1/0", "untyped":true}),
        json!({"id":"bad", "type":{"kind":3,"primitive":17}, "value":{"real":"0/2","imag":"0/1"}, "untyped":true}),
        json!({"id":"bad", "type":{"kind":3,"primitive":3}, "value":null}),
    ] {
        let image = support::image(
            json!({"constants":[constant], "functions":[{"id":"fn.Main","code":{"descriptors":{}}}]}),
        );
        assert_eq!(
            Program::load(&image, LoadLimits::default())
                .err()
                .unwrap()
                .code,
            "invalid_constant"
        );
    }
    let metadata =
        json!({"id":"third", "type":{"kind":3,"primitive":15}, "value":"1/3", "untyped":true});
    let image = support::image(
        json!({"constants":[metadata], "functions":[{"id":"fn.Main","code":{"descriptors":{}}}]}),
    );
    Program::load(&image, LoadLimits::default()).unwrap();
    let code = support::slot_code(
        json!([{"kind":3,"primitive":15}]),
        &[
            ("const", json!({"constant":"third"}), json!({"outputs":[0]})),
            ("pop", json!({}), json!({"inputs":[[0,0]],"release":[0]})),
        ],
    );
    let image =
        support::image(json!({"constants":[metadata],"functions":[{"id":"fn.Main","code":code}]}));
    assert_eq!(
        Program::load(&image, LoadLimits::default())
            .err()
            .unwrap()
            .code,
        "invalid_operand"
    );
}

#[test]
fn byte_constant_backing_is_shared_within_an_instance_and_isolated_between_instances() {
    let integer = json!({"kind":3,"primitive":3});
    let byte = json!({"kind":3,"primitive":9});
    let slice = json!({"kind":5,"node":"bytes"});
    let code = support::slot_code(
        json!([slice, integer, byte, integer]),
        &[
            ("const", json!({"constant":"data"}), json!({"outputs":[0]})),
            ("const", json!({"constant":"index"}), json!({"outputs":[1]})),
            (
                "load_index",
                json!({}),
                json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]}),
            ),
            (
                "convert",
                json!({"type":integer}),
                json!({"inputs":[[0,2]],"outputs":[3],"release":[2]}),
            ),
            (
                "store_local",
                json!({"local":"previous"}),
                json!({"inputs":[[0,3]],"release":[3]}),
            ),
            ("const", json!({"constant":"data"}), json!({"outputs":[0]})),
            ("const", json!({"constant":"index"}), json!({"outputs":[1]})),
            ("const", json!({"constant":"value"}), json!({"outputs":[2]})),
            (
                "store_index",
                json!({}),
                json!({"inputs":[[0,0],[0,1],[0,2]],"release":[0,1,2]}),
            ),
            (
                "load_local",
                json!({"local":"previous"}),
                json!({"outputs":[3]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,3]],"release":[3]}),
            ),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"bytes","kind":5,"elem":{"kind":3,"primitive":9}}]},
        "constants":[
            {"id":"data","type":{"kind":5,"node":"bytes"},"value":"/wA="},
            {"id":"index","type":{"kind":3,"primitive":3},"value":0},
            {"id":"value","type":{"kind":3,"primitive":9},"value":42}
        ],
        "functions":[{"id":"fn.Main","signature":{"results":[integer]},"locals":[{"id":"previous","type":integer}],"code":code}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let entry = program.image().entries[0].name.clone();
    for _ in 0..2 {
        let mut instance = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
        for expected in [255, 42] {
            instance.start(&entry, Vec::new()).unwrap();
            while instance.poll_steps(32).unwrap() != PollStatus::Ready {}
            assert_eq!(instance.results()[0].integer().unwrap(), expected);
        }
        instance.close().unwrap();
        assert_eq!(instance.heap_stats().live_bytes, 0);
    }
}
