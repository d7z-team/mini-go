mod support;

use mini_go::{
    contract_generated as wire,
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    value::Data,
};
use serde_json::json;
use std::sync::Arc;

#[test]
fn struct_construction_fills_defaults_and_preserves_copied_array_fields() {
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let array = json!({"kind":wire::Array,"node":"pair"});
    let record = json!({"kind":wire::Struct,"node":"record"});
    let pointer = json!({"kind":wire::Pointer,"node":"pair.pointer"});
    let code = support::slot_code(
        json!([integer, integer, array, record, pointer]),
        &[
            (
                "const",
                json!({"constant":"answer"}),
                json!({"outputs":[0]}),
            ),
            ("zero", json!({"type":integer}), json!({"outputs":[1]})),
            (
                "make_sequence",
                json!({"type":array,"element_count":2}),
                json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]}),
            ),
            (
                "store_local",
                json!({"local":"array"}),
                json!({"inputs":[[0,2]],"release":[2]}),
            ),
            (
                "load_local",
                json!({"local":"array"}),
                json!({"outputs":[2]}),
            ),
            (
                "make_struct",
                json!({"type":record,"fields":["Values"]}),
                json!({"inputs":[[0,2]],"outputs":[3],"release":[2]}),
            ),
            (
                "store_local",
                json!({"local":"record"}),
                json!({"inputs":[[0,3]],"release":[3]}),
            ),
            (
                "address_of",
                json!({"kind":"local","local":"array"}),
                json!({"outputs":[4]}),
            ),
            ("zero", json!({"type":integer}), json!({"outputs":[0]})),
            ("zero", json!({"type":integer}), json!({"outputs":[1]})),
            (
                "store_index",
                json!({}),
                json!({"inputs":[[0,4],[0,0],[0,1]],"release":[4,0,1]}),
            ),
            (
                "load_local",
                json!({"local":"record"}),
                json!({"outputs":[3]}),
            ),
            (
                "load_local",
                json!({"local":"array"}),
                json!({"outputs":[2]}),
            ),
            (
                "return",
                json!({"result_count":2}),
                json!({"inputs":[[0,3],[0,2]],"release":[3,2]}),
            ),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[
            {"id":"pair","kind":wire::Array,"length":2,"elem":integer},
            {"id":"pair.pointer","kind":wire::Pointer,"elem":array},
            {"id":"record","kind":wire::Struct,"fields":[
                {"name":"Values","type":array},{"name":"Default","type":integer}
            ]}
        ]},
        "constants":[{"id":"answer","type":integer,"value":42}],
        "functions":[{"id":"fn.Main","signature":{"results":[record,array]},
            "locals":[{"id":"array","type":array},{"id":"record","type":record}],
            "code":code}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    for quantum in [1, 64] {
        let mut vm = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        while vm.poll_steps(quantum).unwrap() != PollStatus::Ready {}
        let Data::Struct(fields) = vm.results()[0].data() else {
            panic!("expected record")
        };
        assert_eq!(fields["Default"].integer().unwrap(), 0);
        let Data::Array(copied) = fields["Values"].data() else {
            panic!("expected copied array")
        };
        assert_eq!(copied[0].integer().unwrap(), 42);
        let Data::Array(original) = vm.results()[1].data() else {
            panic!("expected original array")
        };
        assert_eq!(original[0].integer().unwrap(), 0);
        vm.close().unwrap();
        assert_eq!(vm.heap_stats().live_bytes, 0);
    }
}

#[test]
fn struct_layout_rejects_unknown_and_duplicate_initializers_during_preparation() {
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let record = json!({"kind":wire::Struct,"node":"record"});
    for names in [vec!["unknown"], vec!["Value", "Value"]] {
        let mut operations: Vec<_> = names
            .iter()
            .enumerate()
            .map(|(index, _)| ("zero", json!({"type":integer}), json!({"outputs":[index]})))
            .collect();
        operations.push(("make_struct",json!({"type":record,"fields":names}),json!({"inputs":(0..names.len()).map(|index|json!([0,index])).collect::<Vec<_>>(),"outputs":[names.len()],"release":(0..names.len()).collect::<Vec<_>>()})));
        operations.push((
            "return",
            json!({"result_count":1}),
            json!({"inputs":[[0,names.len()]],"release":[names.len()]}),
        ));
        let mut types = vec![integer.clone(); names.len()];
        types.push(record.clone());
        let code = support::slot_code(json!(types), &operations);
        let image = support::image(json!({
            "type_table":{"nodes":[{"id":"record","kind":wire::Struct,
                "fields":[{"name":"Value","type":integer}]}]},
            "functions":[{"id":"fn.Main","signature":{"results":[record]},"code":code}]
        }));
        let error = Program::load(&image, LoadLimits::default()).unwrap_err();
        assert!(
            matches!(error.code, "invalid_operand" | "missing_field"),
            "{error}"
        );
    }
}
