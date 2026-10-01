mod support;

use mini_go::{
    HostValue, InstanceOptions, LoadOptions, Program, contract_generated as wire,
    ffi::Cancellation,
    instance::{ExecutionLimits, Instance, PollStatus},
    snapshot::HostData,
    types::TypeIdentity,
};
use serde_json::json;
use std::{collections::BTreeMap, sync::Arc};
use support::slot_code;

#[test]
fn repeated_interface_assignments_keep_receiver_identity_and_reject_missing_methods() {
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let reader = json!({"kind":wire::Interface,"node":"reader"});
    let signature = json!({"results":[integer]});
    let mut nodes = vec![json!({"id":"reader","kind":wire::Interface,
        "methods":[{"name":"Value","signature":signature}]})];
    let mut main = vec![
        (
            "load_local",
            json!({"local":"input"}),
            json!({"outputs":[0]}),
        ),
        (
            "call_interface",
            json!({"interface_type":reader,"method":"Value","arg_count":0,"result_count":1}),
            json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
        ),
        (
            "return",
            json!({"result_count":1}),
            json!({"inputs":[[0,1]],"release":[1]}),
        ),
    ];
    let mut functions = vec![json!({"id":"fn.Main",
    "signature":{"params":[{"type":reader}],"results":[integer]},
    "locals":[{"id":"input","type":reader}],"code":slot_code(json!([reader,integer]),&main)})];
    let mut identities = Vec::new();
    for index in 0..320 {
        let name = format!("Number{index}");
        let identity = json!({"module_path":"test","decl_id":name});
        let named = json!({"kind":wire::Named,"named":identity});
        let function = format!("value{index}");
        nodes.push(
            json!({"id":name,"kind":wire::Named,"identity":identity,"underlying":integer,
            "methods":[{"name":"Value","receiver":named,"signature":signature,
                "function_id":function,"module_path":"test"}]}),
        );
        functions.push(json!({"id":function,
        "signature":{"params":[{"type":named}],"results":[integer]},
        "locals":[{"id":"self","type":named}],"code":slot_code(json!([named,integer]), &[
            ("load_local",json!({"local":"self"}),json!({"outputs":[0]})),
            ("convert",json!({"type":integer}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
            ("return",json!({"result_count":1}),json!({"inputs":[[0,1]],"release":[1]}))])}));
        identities.push(named);
    }
    let mut artifact = json!({"type_table":{"nodes":nodes},"functions":functions});
    let image = support::image(artifact.clone());
    let program = Arc::new(Program::load(&image, LoadOptions::default()).unwrap());
    main.splice(
        0..0,
        [
            ("zero", json!({"type":integer}), json!({"outputs":[1]})),
            ("pop", json!({}), json!({"inputs":[[0,1]],"release":[1]})),
        ],
    );
    artifact["functions"][0]["code"] = slot_code(json!([reader, integer]), &main);
    let updated =
        Arc::new(Program::load(&support::image(artifact), LoadOptions::default()).unwrap());
    let values: Vec<_> = identities
        .into_iter()
        .enumerate()
        .map(|(index, named)| HostValue {
            typ: program
                .types()
                .resolve("test", &serde_json::from_value(named).unwrap())
                .unwrap(),
            data: HostData::Integer(index as i64),
        })
        .collect();
    let mut instance = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
    for target in [updated, program] {
        for value in &values {
            instance
                .start_host("default", std::slice::from_ref(value))
                .unwrap();
            assert_eq!(instance.poll_steps(32).unwrap(), PollStatus::Ready);
            assert!(matches!(value.data, HostData::Integer(expected)
                if instance.results()[0].integer().unwrap() == expected));
        }
        let result = instance.results()[0].integer().unwrap();
        assert_eq!(
            instance
                .start_host("default", &[HostValue::int(42)])
                .unwrap_err()
                .code,
            "type_error"
        );
        assert_eq!(instance.results()[0].integer().unwrap(), result);
        let plan = instance.prepare_patch(target).unwrap();
        instance.apply_patch(plan).unwrap();
    }
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_bytes, 0);
}

#[test]
fn slices_of_named_bytes_preserve_the_element_identity() {
    let named = json!({"kind":4,"named":{"module_path":"test","decl_id":"Byte"}});
    let slice = json!({"kind":5,"node":"bytes"});
    let image = support::image(json!({
        "type_table":{"nodes":[
            {"id":"Byte","kind":4,"identity":{"module_path":"test","decl_id":"Byte"},"underlying":{"kind":3,"primitive":9}},
            {"id":"bytes","kind":5,"elem":named}
        ]},
        "functions":[{"id":"fn.Main","signature":{"params":[{"type":slice}],"results":[named]},
            "locals":[{"id":"input","type":slice}],"code":slot_code(json!([slice,{"kind":3,"primitive":3},named]), &[
                ("load_local",json!({"local":"input"}),json!({"outputs":[0]})),
                ("zero",json!({"type":{"kind":3,"primitive":3}}),json!({"outputs":[1]})),
                ("load_index",json!({}),json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]})),
                ("return",json!({"result_count":1}),json!({"inputs":[[0,2]],"release":[2]}))])}]
    }));
    let program = Arc::new(Program::load(&image, LoadOptions::default()).unwrap());
    let element = program
        .types()
        .resolve("test", &serde_json::from_value(named).unwrap())
        .unwrap();
    let typ = program
        .types()
        .resolve("test", &serde_json::from_value(slice).unwrap())
        .unwrap();
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance
        .start_host(
            "default",
            &[HostValue {
                typ,
                data: HostData::Array(vec![HostValue {
                    typ: element.clone(),
                    data: HostData::Unsigned(42),
                }]),
            }],
        )
        .unwrap();
    assert_eq!(instance.poll_steps(4).unwrap(), PollStatus::Ready);
    assert_eq!(instance.results()[0].typ(), &element);
    let snapshot = instance.snapshot_results(Default::default()).unwrap();
    instance.close().unwrap();
    assert_eq!(snapshot.roots[0].typ, element);
    assert!(matches!(snapshot.roots[0].data, HostData::Unsigned(42)));
}

#[test]
fn typed_host_collections_are_copied_and_failure_preserves_prior_results() {
    let map = json!({"kind":7,"node":"map"});
    let slice = json!({"kind":5,"node":"slice"});
    let structure = json!({"kind":12,"node":"payload"});
    let integer = json!({"kind":3,"primitive":3});
    let image = support::image(json!({
        "type_table":{"nodes":[
            {"id":"map","kind":7,"key":{"kind":3,"primitive":2},"elem":{"kind":3,"primitive":3}},
            {"id":"slice","kind":5,"elem":{"kind":3,"primitive":3}},
            {"id":"payload","kind":12,"fields":[{"name":"Extra","type":{"kind":3,"primitive":3}}]}
        ]},
        "constants":[{"id":"key","type":{"kind":3,"primitive":2},"value":"answer"}],
        "functions":[{"id":"fn.Main","signature":{"params":[{"type":map},{"type":slice},{"type":{"kind":3,"primitive":1}},{"type":structure}],"results":[{"kind":3,"primitive":3}]},
        "locals":[{"id":"map","type":map},{"id":"slice","type":slice},{"id":"flag","type":{"kind":3,"primitive":1}},{"id":"payload","type":structure}],
        "code":slot_code(json!([{"kind":3,"primitive":1},map,{"kind":3,"primitive":2},integer,slice,integer,integer,structure]), &[
            ("load_local",json!({"local":"flag"}),json!({"outputs":[0]})),
            ("jump_if",json!({"label":"sum"}),json!({"inputs":[[0,0]],"release":[0]})),
            ("zero",json!({"type":integer}),json!({"outputs":[3]})),
            ("return",json!({"result_count":1}),json!({"inputs":[[0,3]],"release":[3]})),
            ("label",json!({"label":"sum"}),json!({})),
            ("load_local",json!({"local":"map"}),json!({"outputs":[1]})),
            ("const",json!({"constant":"key"}),json!({"outputs":[2]})),
            ("load_index",json!({}),json!({"inputs":[[0,1],[0,2]],"outputs":[3],"release":[1,2]})),
            ("load_local",json!({"local":"slice"}),json!({"outputs":[4]})),
            ("zero",json!({"type":integer}),json!({"outputs":[5]})),
            ("load_index",json!({}),json!({"inputs":[[0,4],[0,5]],"outputs":[6],"release":[4,5]})),
            ("binary",json!({"operator":"+"}),json!({"inputs":[[0,3],[0,6]],"outputs":[5],"release":[3,6]})),
            ("load_local",json!({"local":"payload"}),json!({"outputs":[7]})),
            ("load_field",json!({"field":"Extra"}),json!({"inputs":[[0,7]],"outputs":[3],"release":[7]})),
            ("binary",json!({"operator":"+"}),json!({"inputs":[[0,5],[0,3]],"outputs":[6],"release":[5,3]})),
            ("return",json!({"result_count":1}),json!({"inputs":[[0,6]],"release":[6]}))])}]
    }));
    let program = Arc::new(Program::load(&image, LoadOptions::default()).unwrap());
    let resolve = |reference| {
        program
            .types()
            .resolve(
                "test",
                &serde_json::from_value::<wire::TypeRef>(reference).unwrap(),
            )
            .unwrap()
    };
    let mut arguments = vec![
        HostValue {
            typ: resolve(map),
            data: HostData::MapEntries(vec![(HostValue::string("answer"), HostValue::int(20))]),
        },
        HostValue {
            typ: resolve(slice),
            data: HostData::Array(vec![HostValue::int(20)]),
        },
        HostValue::boolean(true),
        HostValue {
            typ: resolve(structure),
            data: HostData::Struct(BTreeMap::from([("Extra".to_owned(), HostValue::int(2))])),
        },
    ];
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.start_host("default", &arguments).unwrap();
    arguments[1].data = HostData::Array(vec![HostValue::int(99)]);
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(instance.results()[0].integer().unwrap(), 42);
    assert_eq!(instance.collect_garbage().unwrap().live_objects, 0);
    arguments[1].data = HostData::Array(vec![HostValue::string("invalid integer")]);
    assert!(instance.start_host("default", &arguments).is_err());
    assert_eq!(instance.results()[0].integer().unwrap(), 42);
    assert_eq!(instance.collect_garbage().unwrap().live_objects, 0);
    arguments[1].data = HostData::Array(vec![HostValue::int(20)]);
    instance.start_host("default", &arguments).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(instance.results()[0].integer().unwrap(), 42);
    instance.close().unwrap();
}

#[test]
fn shared_host_byte_input_preserves_binary_data_and_validates_nil() {
    let bytes = json!({"kind":5,"node":"bytes"});
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"bytes","kind":5,"elem":{"kind":3,"primitive":9}}]},
        "functions":[{"id":"fn.Main","signature":{"params":[{"type":bytes}],"results":[bytes]},"locals":[{"id":"bytes","type":bytes}],"code":slot_code(json!([bytes]), &[
            ("load_local",json!({"local":"bytes"}),json!({"outputs":[0]})),
            ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))])}]
    }));
    let program = Arc::new(Program::load(&image, LoadOptions::default()).unwrap());
    let instance = program
        .instantiate(InstanceOptions {
            limits: ExecutionLimits {
                max_string_bytes: 1,
                ..ExecutionLimits::default()
            },
            ..InstanceOptions::default()
        })
        .unwrap();
    let argument = HostValue::bytes(vec![0, 255, 128]);
    let execution = loop {
        match instance.start_host("default", std::slice::from_ref(&argument)) {
            Ok(execution) => break execution,
            Err(error) if error.code == "busy" => std::thread::yield_now(),
            Err(error) => panic!("{error}"),
        }
    };
    let result = execution.wait(&Cancellation::default()).unwrap();
    instance.shutdown(&Cancellation::default()).unwrap();
    assert_eq!(result.bytes(&result.roots[0]).unwrap(), [0, 255, 128]);
    assert!(matches!(argument.data, HostData::String(bytes) if bytes == [0, 255, 128]));

    let mut machine = Instance::new(program, ExecutionLimits::default()).unwrap();
    let invalid = HostValue {
        typ: TypeIdentity::Primitive(wire::PrimitiveInt),
        data: HostData::Nil,
    };
    assert_eq!(
        machine.start_host("default", &[invalid]).unwrap_err().code,
        "host_type"
    );
    assert_eq!(machine.heap_stats().live_objects, 0);
    machine.close().unwrap();
}
