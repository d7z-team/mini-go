mod support;

use mini_go::{
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    value::Data,
};
use serde_json::{Value, json};
use std::sync::Arc;

fn operation(name: &str, descriptor: u32, operands: u32) -> Value {
    let contract: Value = serde_json::from_str(mini_go::contract_generated::CONTRACT_JSON).unwrap();
    let opcode = contract["spec"]["opcodes"]
        .as_array()
        .unwrap()
        .iter()
        .position(|entry| entry["op"] == name)
        .unwrap();
    json!([opcode + 1, descriptor, operands])
}

#[test]
fn labels_resolve_to_executable_pcs_and_profile_names() {
    let code = support::slot_code(
        json!([]),
        &[
            ("jump", json!({"label":"second"}), json!({})),
            ("label", json!({"label":"first"}), json!({})),
            ("label", json!({"label":"second"}), json!({})),
            ("return", json!({"result_count":0}), json!({})),
            ("label", json!({"label":"end"}), json!({})),
        ],
    );
    let bytes = support::image(json!({"functions":[{"id":"fn.Main","signature":{},"code":code}]}));
    let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
    for profile in [false, true] {
        let mut vm = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
        if profile {
            vm.start_profile(1, 16).unwrap();
        }
        vm.start("default", vec![]).unwrap();
        while vm.poll_steps(1).unwrap() != PollStatus::Ready {}
        assert!(vm.results().is_empty());
        assert_eq!(vm.steps(), 2);
        if profile {
            let samples = vm.profile();
            assert_eq!(samples.dropped, 0);
            let names: std::collections::BTreeMap<_, _> = samples
                .samples
                .iter()
                .map(|s| ((s.pc, s.opcode.as_str()), s.count))
                .collect();
            assert_eq!(names, [((0, "jump"), 1), ((1, "return"), 1)].into());
        }
        vm.close().unwrap();
    }
}

#[test]
fn invalid_labels_are_checked_in_unreachable_code() {
    for (labels, target, expected) in [
        (vec!["again", "again"], "again", "invalid_label"),
        (vec![""], "", "invalid_label"),
        (vec!["valid"], "missing", "invalid_jump"),
    ] {
        let mut operations = vec![("return", json!({"result_count":0}), json!({}))];
        operations.extend(
            labels
                .into_iter()
                .map(|label| ("label", json!({"label":label}), json!({}))),
        );
        operations.push(("jump", json!({"label":target}), json!({})));
        let code = support::slot_code(json!([]), &operations);
        let bytes =
            support::image(json!({"functions":[{"id":"fn.Main","signature":{},"code":code}]}));
        assert_eq!(
            Program::load(&bytes, LoadLimits::default())
                .unwrap_err()
                .code,
            expected
        );
    }
}

#[test]
fn slot_calls_transfer_arguments_through_tail_replacement_and_bound_functions() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    for indirect in [false, true] {
        let mut instructions = vec![operation("binary", 0, 0)];
        let mut operands = vec![json!({"inputs":[[1,0],[1,0]],"outputs":[0]})];
        if indirect {
            instructions.push(operation("make_closure", 0, 1));
            operands.push(json!({"outputs":[1]}));
        }
        instructions.push(operation(
            if indirect {
                "call_value"
            } else {
                "tail_call_direct"
            },
            0,
            operands.len() as u32,
        ));
        operands.push(if indirect {
            json!({"inputs":[[0,1],[0,0]],"outputs":[2],"release":[0,1]})
        } else {
            json!({"inputs":[[0,0]],"release":[0]})
        });
        if indirect {
            instructions.push(operation("return", 0, operands.len() as u32));
            operands.push(json!({"inputs":[[0,2]],"release":[2]}));
        }
        let bytes = support::image(json!({
            "type_table":{"nodes":[{"id":"callable","kind":wire::Function,"signature":{"params":[{"type":integer}],"results":[integer]}}]},
            "constants":[{"id":"half","type":integer,"value":21}],
            "functions":[
                {"id":"fn.Main","signature":{"results":[integer]},"code":{
                    "types":[integer,{"kind":wire::Function,"node":"callable"},integer],
                    "descriptors":{"operator":[{"operator":"+"}],"closure":[{"function":"callee"}],"call":[{"function":"callee","arg_count":1,"result_count":1}],"return":[{"result_count":1}]},
                    "instructions":instructions,"operands":operands}},
                {"id":"callee","signature":{"params":[{"type":integer}],"results":[integer]},"locals":[{"id":"value","type":integer}],"code":{
                    "descriptors":{"return":[{"result_count":1}]},"instructions":[operation("return",0,0)],"operands":[{"inputs":[[2,0]]}]}}
            ]
        }));
        let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
        let mut vm = Instance::new(
            program,
            ExecutionLimits {
                max_frames: if indirect { 2 } else { 1 },
                ..Default::default()
            },
        )
        .unwrap();
        vm.start("default", vec![]).unwrap();
        while vm.poll_steps(1).unwrap() != PollStatus::Ready {}
        assert_eq!(vm.results()[0].integer().unwrap(), 42);
        assert_eq!(vm.steps(), if indirect { 5 } else { 3 });
        vm.close().unwrap();
        assert_eq!(vm.heap_stats().live_objects, 0);
    }
}

#[test]
fn direct_local_results_preserve_budget_and_reject_invalid_destinations() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let boolean = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveBool});
    for (target, local_type, valid) in [
        (1u32 << 31, integer.clone(), true),
        ((1u32 << 31) | 1, integer.clone(), false),
        (1u32 << 31, boolean, false),
    ] {
        let bytes = support::image(json!({
            "constants":[{"id":"one","type":integer,"value":1},{"id":"end","type":integer,"value":9}],
            "functions":[{"id":"fn.Main","signature":{"results":[integer]},"locals":[{"id":"counter","type":local_type}],"code":{
                "descriptors":{"operator":[{"operator":"+"}],"comparebranch":[{"operator":"<","type":integer,"label":"loop","when":true}],"label":[{"label":"loop"}],"return":[{"result_count":1}]},
                "instructions":[operation("label",0,0),operation("binary",0,1),operation("compare_branch",0,2),operation("return",0,3)],
                "operands":[{}, {"inputs":[[2,0],[1,0]],"outputs":[target]}, {"inputs":[[2,0],[1,1]]}, {"inputs":[[2,0]]}]
            }}]
        }));
        let loaded = Program::load(&bytes, LoadLimits::default());
        if !valid {
            assert!(loaded.is_err());
            continue;
        }
        let program = Arc::new(loaded.unwrap());
        for quantum in [1, 4096] {
            let mut vm = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
            vm.start("default", vec![]).unwrap();
            loop {
                let before = vm.steps();
                let status = vm.poll_steps(quantum).unwrap();
                assert!(vm.steps() - before <= quantum as u64);
                if status == PollStatus::Ready {
                    break;
                }
                assert!(vm.steps() <= 19);
            }
            assert_eq!(vm.steps(), 19);
            assert_eq!(vm.results()[0].integer().unwrap(), 9);
            vm.close().unwrap();
        }
    }
}

#[test]
fn initialization_rechecks_backedges_and_entry_releases() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let boolean = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveBool});
    for (release, redefine, on_entry, valid) in [
        (false, false, false, true),
        (true, false, false, false),
        (true, true, false, true),
        (false, false, true, false),
    ] {
        let mut instructions = vec![operation("zero", 0, 0), operation("label", 0, 1)];
        if redefine {
            instructions.push(operation("zero", 0, 0));
        }
        instructions.extend([
            operation("store_local", 0, 2),
            operation("jump_if", 0, 3),
            operation("zero", 0, 0),
            operation("return", 0, 4),
            operation("return", 0, 4),
        ]);
        let bytes = support::image(json!({
            "constants":[{"id":"branch","type":boolean,"value":true}],
            "functions":[{"id":"fn.Main","signature":{"results":[integer]},"locals":[{"id":"value","type":integer}],"code":{
                "types":[integer],
                "descriptors":{"type":[{"type":integer}],"label":[{"label":"again"}],"jump":[{"label":"again"}],"local":[{"local":"value"}],"return":[{"result_count":1}]},
                "instructions":instructions,
                "operands":[{"outputs":[0]},{},{"inputs":[[0,0]],"release":if release {vec![0]} else {vec![]},"release_before":if on_entry {vec![0]} else {vec![]}}, {"inputs":[[1,0]]},{"inputs":[[0,0]],"release":[0]}]
            }}]
        }));
        let loaded = Program::load(&bytes, LoadLimits::default());
        if valid {
            assert!(loaded.is_ok(), "{:?}", loaded.err());
        } else {
            let error = match loaded {
                Ok(_) => panic!("must reject uninitialized reads"),
                Err(error) => error,
            };
            assert!(error.to_string().contains("uninitialized"), "{error}");
        }
    }
}

#[test]
fn scalar_destinations_and_operands_match_the_operator_contract() {
    use mini_go::contract_generated as wire;
    for (operator, left, right, result, valid) in [
        (
            "+",
            wire::PrimitiveInt,
            wire::PrimitiveInt,
            wire::PrimitiveInt,
            true,
        ),
        (
            "+",
            wire::PrimitiveInt,
            wire::PrimitiveInt,
            wire::PrimitiveBool,
            false,
        ),
        (
            "+",
            wire::PrimitiveInt,
            wire::PrimitiveUint,
            wire::PrimitiveInt,
            false,
        ),
        (
            "<<",
            wire::PrimitiveInt,
            wire::PrimitiveUint,
            wire::PrimitiveInt,
            true,
        ),
        (
            "<",
            wire::PrimitiveInt,
            wire::PrimitiveInt,
            wire::PrimitiveBool,
            true,
        ),
        (
            "<",
            wire::PrimitiveInt,
            wire::PrimitiveInt,
            wire::PrimitiveInt,
            false,
        ),
        (
            "&",
            wire::PrimitiveFloat64,
            wire::PrimitiveFloat64,
            wire::PrimitiveFloat64,
            false,
        ),
        (
            "complex",
            wire::PrimitiveFloat32,
            wire::PrimitiveFloat32,
            wire::PrimitiveComplex64,
            true,
        ),
    ] {
        let typ = |primitive| json!({"kind":wire::Primitive,"primitive":primitive});
        let bytes = support::image(json!({
            "constants":[{"id":"left","type":typ(left),"value":1},{"id":"right","type":typ(right),"value":2}],
            "functions":[{"id":"fn.Main","signature":{"results":[typ(result)]},"code":{
                "types":[typ(result)],"descriptors":{"operator":[{"operator":operator}],"return":[{"result_count":1}]},
                "instructions":[operation("binary",0,0),operation("return",0,1)],
                "operands":[{"inputs":[[1,0],[1,1]],"outputs":[0]},{"inputs":[[0,0]],"release":[0]}]
            }}]
        }));
        let loaded = Program::load(&bytes, LoadLimits::default());
        assert_eq!(
            loaded.is_ok(),
            valid,
            "{operator}/{left}/{right}/{result}: {:?}",
            loaded.err()
        );
    }
}

#[test]
fn aggregate_inputs_match_the_declared_field_types() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let boolean = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveBool});
    let record = json!({"kind":wire::Struct,"node":"record"});
    for valid in [true, false] {
        let bytes = support::image(json!({
            "type_table":{"nodes":[{"id":"record","kind":wire::Struct,"fields":[{"name":"value","type":integer}]}]},
            "constants":[{"id":"input","type":if valid {integer.clone()} else {boolean.clone()},"value":if valid {json!(42)} else {json!(true)}}],
            "functions":[{"id":"fn.Main","signature":{"results":[record]},"code":{
                "types":[record],"descriptors":{"makestruct":[{"type":record,"fields":["value"]}],"return":[{"result_count":1}]},
                "instructions":[operation("make_struct",0,0),operation("return",0,1)],
                "operands":[{"inputs":[[1,0]],"outputs":[0]},{"inputs":[[0,0]],"release":[0]}]
            }}]
        }));
        let loaded = Program::load(&bytes, LoadLimits::default());
        if !valid {
            let Err(error) = loaded else {
                panic!("constructor must reject incompatible field input");
            };
            assert!(error.to_string().contains("type mismatch"), "{error}");
            continue;
        }
        let mut vm = Instance::new(Arc::new(loaded.unwrap()), ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        while vm.poll_steps(1).unwrap() != PollStatus::Ready {}
        let Data::Struct(fields) = vm.results()[0].data() else {
            panic!("expected struct result");
        };
        assert_eq!(fields.get("value").unwrap().integer().unwrap(), 42);
        vm.close().unwrap();
    }
}

#[test]
fn slot_call_results_survive_poll_boundaries_and_release_inputs() {
    let integer = json!({"kind":3,"primitive":3});
    let bytes = support::image(json!({
        "constants":[{"id":"forty","type":integer,"value":40},{"id":"two","type":integer,"value":2}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer]},"code":{
                "types":[integer,integer],
                "descriptors":{"call":[{"function":"fn.Child","arg_count":1,"result_count":1}],"operator":[{"operator":"+"}],"return":[{"result_count":1}]},
                "instructions":[operation("call_direct",0,0),operation("binary",0,1),operation("return",0,2)],
                "operands":[
                    {"inputs":[[1,1]],"outputs":[0]},
                    {"inputs":[[1,0],[0,0]],"outputs":[1],"release":[0]},
                    {"inputs":[[0,1]],"release":[1]}
                ]}},
            {"id":"fn.Child","signature":{"params":[{"type":integer}],"results":[integer]},"locals":[{"id":"argument","type":integer}],"code":{
                "types":[integer],
                "descriptors":{"local":[{"local":"argument"}],"return":[{"result_count":1}]},
                "instructions":[operation("load_local",0,0),operation("return",0,1)],
                "operands":[{"outputs":[0]},{"inputs":[[0,0]],"release":[0]}]
            }}
        ]
    }));
    let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
    for quantum in [1, 2, 64] {
        let mut vm = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        for _ in 0..16 {
            let before = vm.steps();
            let status = vm.poll_steps(quantum).unwrap();
            assert!(vm.steps() - before <= quantum as u64);
            if status == PollStatus::Ready {
                break;
            }
        }
        assert!(matches!(vm.results()[0].data(), Data::Integer(42)));
        assert_eq!(vm.steps(), 5);
        vm.close().unwrap();
    }
}

#[test]
fn collection_access_checks_keys_values_and_sizes() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let boolean = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveBool});
    let text = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveString});
    let sequence = json!({"kind":wire::Slice,"node":"sequence"});
    let mapping = json!({"kind":wire::Map,"node":"mapping"});
    let record = json!({"kind":wire::Struct,"node":"record"});
    let channel = json!({"kind":wire::Waitable,"node":"channel"});
    for (op, descriptor, inputs, outputs, invalid_input) in [
        (
            "load_index",
            None,
            vec![sequence.clone(), integer.clone()],
            vec![integer.clone()],
            1,
        ),
        (
            "store_index",
            None,
            vec![sequence.clone(), integer.clone(), integer.clone()],
            vec![],
            2,
        ),
        (
            "load_index_ok",
            None,
            vec![mapping.clone(), text.clone()],
            vec![integer.clone(), boolean.clone()],
            1,
        ),
        (
            "store_index",
            None,
            vec![mapping.clone(), text.clone(), integer.clone()],
            vec![],
            2,
        ),
        (
            "make_map",
            Some(("makemap", json!({"type":mapping,"entry_count":1}))),
            vec![text.clone(), integer.clone()],
            vec![mapping.clone()],
            0,
        ),
        (
            "make_map",
            Some(("makemap", json!({"type":mapping,"entry_count":1}))),
            vec![text.clone(), integer.clone()],
            vec![mapping.clone()],
            1,
        ),
        (
            "make_map",
            Some((
                "makemap",
                json!({"type":mapping,"entry_count":0,"has_capacity":true}),
            )),
            vec![integer.clone()],
            vec![mapping.clone()],
            0,
        ),
        (
            "make_slice",
            Some(("makeslice", json!({"type":sequence}))),
            vec![integer.clone()],
            vec![sequence.clone()],
            0,
        ),
        (
            "store_field",
            Some(("field", json!({"field":"value"}))),
            vec![record.clone(), integer.clone()],
            vec![],
            1,
        ),
        (
            "waitable_send",
            None,
            vec![channel.clone(), integer.clone()],
            vec![],
            1,
        ),
        (
            "waitable_try_send",
            None,
            vec![channel.clone(), integer.clone()],
            vec![boolean.clone()],
            1,
        ),
        (
            "waitable_recv_ok",
            None,
            vec![channel.clone()],
            vec![integer.clone(), boolean.clone()],
            0,
        ),
        (
            "waitable_try_recv",
            None,
            vec![channel.clone()],
            vec![integer.clone(), boolean.clone()],
            0,
        ),
        (
            "waitable_can_send",
            None,
            vec![channel.clone()],
            vec![boolean.clone()],
            0,
        ),
        ("waitable_close", None, vec![channel.clone()], vec![], 0),
        (
            "string_rune_at",
            None,
            vec![text.clone(), integer.clone()],
            vec![json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt32})],
            0,
        ),
        (
            "string_next_rune_index",
            None,
            vec![text.clone(), integer.clone()],
            vec![integer.clone()],
            1,
        ),
        (
            "delete",
            None,
            vec![mapping.clone(), text.clone()],
            vec![],
            1,
        ),
        ("clear", None, vec![sequence.clone()], vec![], 0),
    ] {
        for valid in [true, false] {
            let mut types = inputs.clone();
            if !valid {
                types[invalid_input] = boolean.clone();
            }
            let mut descriptors = json!({"type":[], "return":[{"result_count":0}]});
            let mut instructions = Vec::new();
            let mut operands = Vec::new();
            for (index, typ) in types.iter().enumerate() {
                descriptors["type"]
                    .as_array_mut()
                    .unwrap()
                    .push(json!({"type":typ}));
                instructions.push(operation("zero", index as u32, index as u32));
                operands.push(json!({"outputs":[index]}));
            }
            if let Some((kind, descriptor)) = descriptor.as_ref() {
                descriptors[*kind] = json!([descriptor]);
            }
            instructions.push(operation(op, 0, operands.len() as u32));
            operands.push(json!({
                "inputs":(0..inputs.len()).map(|index| json!([0,index])).collect::<Vec<_>>(),
                "outputs":(inputs.len()..inputs.len()+outputs.len()).collect::<Vec<_>>()
            }));
            types.extend(outputs.iter().cloned());
            instructions.push(operation("return", 0, operands.len() as u32));
            operands.push(json!({}));
            let bytes = support::image(json!({
                "type_table":{"nodes":[
                    {"id":"sequence","kind":wire::Slice,"elem":integer},
                    {"id":"mapping","kind":wire::Map,"key":text,"elem":integer},
                    {"id":"record","kind":wire::Struct,"fields":[{"name":"value","type":integer}]},
                    {"id":"channel","kind":wire::Waitable,"direction":wire::ChannelBoth,"elem":integer}
                ]},
                "functions":[{"id":"fn.Main","signature":{},"code":{
                    "types":types,"descriptors":descriptors,"instructions":instructions,"operands":operands
                }}]
            }));
            let loaded = Program::load(&bytes, LoadLimits::default());
            assert_eq!(
                loaded.is_ok(),
                valid,
                "{op}/{invalid_input}, valid={valid}: {:?}",
                loaded.err()
            );
        }
    }
}

#[test]
fn channel_access_honors_direction_and_result_types() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let boolean = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveBool});
    let channel = json!({"kind":wire::Waitable,"node":"channel"});
    for direction in [wire::ChannelBoth, wire::ChannelSend, wire::ChannelReceive] {
        for op in [
            "waitable_send",
            "waitable_try_send",
            "waitable_recv",
            "waitable_recv_ok",
            "waitable_try_recv",
            "waitable_can_send",
            "waitable_can_recv",
            "waitable_close",
        ] {
            for forged_result in [false, true] {
                let mut types = vec![channel.clone(), integer.clone()];
                let mut inputs = vec![json!([0, 0])];
                let mut outputs = Vec::new();
                let sending = matches!(
                    op,
                    "waitable_send" | "waitable_try_send" | "waitable_can_send" | "waitable_close"
                );
                if matches!(op, "waitable_send" | "waitable_try_send") {
                    inputs.push(json!([0, 1]));
                }
                if matches!(
                    op,
                    "waitable_recv" | "waitable_recv_ok" | "waitable_try_recv"
                ) {
                    outputs.push(types.len());
                    types.push(integer.clone());
                }
                if matches!(
                    op,
                    "waitable_recv_ok"
                        | "waitable_try_recv"
                        | "waitable_try_send"
                        | "waitable_can_send"
                        | "waitable_can_recv"
                ) {
                    outputs.push(types.len());
                    types.push(boolean.clone());
                }
                if forged_result {
                    let Some(&first) = outputs.first() else {
                        continue;
                    };
                    types[first] =
                        json!({"kind":wire::Primitive,"primitive":wire::PrimitiveString});
                }
                let bytes = support::image(json!({
                    "type_table":{"nodes":[{"id":"channel","kind":wire::Waitable,"direction":direction,"elem":integer}]},
                    "functions":[{"id":"fn.Main","signature":{},"code":{
                        "types":types,
                        "descriptors":{"type":[{"type":channel},{"type":integer}],"return":[{"result_count":0}]},
                        "instructions":[operation("zero",0,0),operation("zero",1,1),operation(op,0,2),operation("return",0,3)],
                        "operands":[{"outputs":[0]},{"outputs":[1]},{"inputs":inputs,"outputs":outputs},{}]
                    }}]
                }));
                let valid = !forged_result
                    && (direction == wire::ChannelBoth
                        || sending == (direction == wire::ChannelSend));
                let loaded = Program::load(&bytes, LoadLimits::default());
                assert_eq!(
                    loaded.is_ok(),
                    valid,
                    "{op} direction={direction} forged={forged_result}: {:?}",
                    loaded.err()
                );
            }
        }
    }
}

#[test]
fn intrinsic_results_are_written_to_explicit_destinations() {
    use mini_go::contract_generated as wire;
    let unsigned = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveUint64});
    let float = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveFloat64});
    let bytes = support::image(json!({
        "constants":[{"id":"bits","type":unsigned,"value":42f64.to_bits()}],
        "functions":[{"id":"fn.Main","signature":{"results":[float]},"code":{
            "types":[float],
            "descriptors":{"callintrinsic":[{"id":"math.float64_from_bits","arg_count":1,"result_count":1}],"return":[{"result_count":1}]},
            "instructions":[operation("call_intrinsic",0,0),operation("return",0,1)],
            "operands":[{"inputs":[[1,0]],"outputs":[0]},{"inputs":[[0,0]],"release":[0]}]
        }}]
    }));
    let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    vm.start("default", Vec::new()).unwrap();
    while vm.poll_steps(1).unwrap() != PollStatus::Ready {
        assert!(vm.steps() <= 2);
    }
    assert!(matches!(vm.results()[0].data(), Data::Float(42.0)));
    assert_eq!(vm.steps(), 2);
    vm.close().unwrap();
}

#[test]
fn direct_pointer_inputs_preserve_shared_pointees_and_lazy_nil() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let pointer = json!({"kind":wire::Pointer,"node":"pointer"});
    for nil in [false, true] {
        let (instructions, operands, results) = if nil {
            (
                vec![operation("return", 0, 0)],
                vec![json!({"inputs":[[2,1]]})],
                vec![pointer.clone()],
            )
        } else {
            (
                vec![
                    operation("store_local", 0, 0),
                    operation("address_of", 0, 1),
                    operation("store_local", 1, 2),
                    operation("store_indirect", 0, 3),
                    operation("load_indirect", 0, 4),
                    operation("return", 0, 5),
                ],
                vec![
                    json!({"inputs":[[1,0]]}),
                    json!({"outputs":[0]}),
                    json!({"inputs":[[0,0]],"release":[0]}),
                    json!({"inputs":[[2,1],[1,1]]}),
                    json!({"inputs":[[2,1]],"outputs":[1]}),
                    json!({"inputs":[[0,1]],"release":[1]}),
                ],
                vec![integer.clone()],
            )
        };
        let bytes = support::image(json!({
            "type_table":{"nodes":[{"id":"pointer","kind":wire::Pointer,"elem":integer}]},
            "constants":[{"id":"initial","type":integer,"value":41},{"id":"answer","type":integer,"value":42}],
            "functions":[{"id":"fn.Main","signature":{"results":results},
                "locals":[{"id":"value","type":integer},{"id":"pointer","type":pointer}],"code":{
                    "types":[pointer,integer],"instructions":instructions,"operands":operands,
                    "descriptors":{"local":[{"local":"value"},{"local":"pointer"}],
                        "address":[{"kind":"local","local":"value"}],"return":[{"result_count":1}]}
                }}]
        }));
        let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
        for quantum in [1, 2, 8] {
            let mut vm = Instance::new(program.clone(), ExecutionLimits::default()).unwrap();
            vm.start("default", Vec::new()).unwrap();
            while vm.poll_steps(quantum).unwrap() != PollStatus::Ready {}
            if nil {
                assert!(matches!(vm.results()[0].data(), Data::Nil));
                assert_eq!(vm.steps(), 1);
            } else {
                assert_eq!(vm.results()[0].integer().unwrap(), 42);
                assert_eq!(vm.steps(), 6);
            }
            vm.close().unwrap();
            assert_eq!(vm.heap_stats().live_bytes, 0);
        }
    }
}

#[test]
fn direct_local_inputs_initialize_zero_and_reject_addressable_storage() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let pointer = json!({"kind":wire::Pointer,"node":"pointer"});
    for addressable in [false, true] {
        let mut instructions = vec![operation("return", 0, 0)];
        let mut operands = vec![json!({"inputs":[[2,0]]})];
        if addressable {
            instructions.push(operation("address_of", 0, 1));
            operands.push(json!({"outputs":[0]}));
        }
        let bytes = support::image(json!({
            "type_table":{"nodes":[{"id":"pointer","kind":wire::Pointer,"elem":integer}]},
            "functions":[{"id":"fn.Main","signature":{"results":[integer]},
                "locals":[{"id":"value","type":integer}],"code":{
                    "types":[pointer],"instructions":instructions,"operands":operands,
                    "descriptors":{"return":[{"result_count":1}],"address":[{"kind":"local","local":"value"}]}
                }}]
        }));
        let loaded = Program::load(&bytes, LoadLimits::default());
        if addressable {
            let Err(error) = loaded else {
                panic!("addressable local cannot be a direct input");
            };
            assert!(error.to_string().contains("private scalar"), "{error}");
            continue;
        }
        let mut vm = Instance::new(Arc::new(loaded.unwrap()), ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
        assert!(matches!(vm.results()[0].data(), Data::Integer(0)));
        vm.close().unwrap();
    }
}

#[test]
fn repeated_calls_reset_private_locals_and_keep_unused_cells_lazy() {
    let integer = json!({"kind":3,"primitive":3});
    let boolean = json!({"kind":3,"primitive":1});
    let bytes = support::image(json!({
        "constants":[{"id":"skip","type":boolean,"value":true},{"id":"write","type":boolean,"value":false},{"id":"answer","type":integer,"value":42}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer,integer,integer]},"code":{
                "types":[integer,integer,integer],
                "descriptors":{"call":[{"function":"callee","arg_count":1,"result_count":1}],"return":[{"result_count":3}]},
                "instructions":[operation("call_direct",0,0),operation("call_direct",0,1),operation("call_direct",0,2),operation("return",0,3)],
                "operands":[{"inputs":[[1,1]],"outputs":[0]},{"inputs":[[1,0]],"outputs":[1]},{"inputs":[[1,0]],"outputs":[2]},{"inputs":[[0,0],[0,1],[0,2]],"release":[0,1,2]}]
            }},
            {"id":"callee","signature":{"params":[{"type":boolean}],"results":[integer]},
                "locals":[{"id":"skip","type":boolean},{"id":"result","type":integer},{"id":"unused","type":integer}],"code":{
                "descriptors":{"jump":[{"label":"done"}],"label":[{"label":"done"}],"local":[{"local":"result"}],"return":[{"result_count":1}]},
                "instructions":[operation("jump_if",0,0),operation("store_local",0,1),operation("label",0,2),operation("return",0,3)],
                "operands":[{"inputs":[[2,0]]},{"inputs":[[1,2]]},{},{"inputs":[[2,1]]}]
            }}
        ]
    }));
    let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
    for cache_bytes in [0, 64 * 1024] {
        let mut vm = Instance::new(
            program.clone(),
            ExecutionLimits {
                max_frame_cache_bytes: cache_bytes,
                ..Default::default()
            },
        )
        .unwrap();
        vm.start("default", vec![]).unwrap();
        while vm.poll_steps(1).unwrap() != PollStatus::Ready {}
        assert_eq!(
            vm.results()
                .iter()
                .map(|value| value.integer().unwrap())
                .collect::<Vec<_>>(),
            [42, 0, 0]
        );
        vm.close().unwrap();
        assert_eq!(vm.heap_stats().live_bytes, 0);
    }
}

#[test]
fn scalar_and_lazy_inputs_survive_one_step_polls() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let byte_slice = json!({"kind":wire::Slice,"node":"bytes"});
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"bytes","kind":wire::Slice,"elem":{"kind":wire::Primitive,"primitive":wire::PrimitiveUint8}}]},
        "constants":[{"id":"forty","type":integer,"value":40},{"id":"bytes","type":byte_slice,"value":"AQI="}],
        "functions":[{"id":"fn.Main","signature":{"results":[integer]},"locals":[{"id":"zero","type":integer}],"code":{
            "types":[integer,integer,integer],
            "descriptors":{"operator":[{"operator":"+"}],"return":[{"result_count":1}]},
            "instructions":[operation("binary",0,0),operation("len",0,1),operation("binary",0,2),operation("return",0,3)],
            "operands":[{"inputs":[[1,0],[2,0]],"outputs":[0]}, {"inputs":[[1,1]],"outputs":[1]}, {"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]}, {"inputs":[[0,2]],"release":[2]}]
        }}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    vm.start("default", vec![]).unwrap();
    for _ in 0..3 {
        assert_ne!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
    }
    assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
    assert_eq!(vm.steps(), 4);
    assert_eq!(vm.results()[0].integer().unwrap(), 42);
    vm.close().unwrap();
    assert_eq!(vm.heap_stats().live_objects, 0);
}

#[test]
fn private_field_inputs_initialize_zero_and_reject_addressable_storage() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let record = json!({"kind":wire::Struct,"node":"record"});
    let pointer = json!({"kind":wire::Pointer,"node":"pointer"});
    for (addressable, direct) in [(false, false), (false, true), (true, false), (true, true)] {
        let mut instructions = vec![operation("get_path", 0, 0), operation("return", 0, 1)];
        let mut operands = vec![
            json!({"inputs":[[2,0]],"outputs":[if direct { (1u32 << 31) | 1 } else { 0 }]}),
            if direct {
                json!({"inputs":[[2,1]]})
            } else {
                json!({"inputs":[[0,0]],"release":[0]})
            },
        ];
        if addressable {
            instructions.push(operation("address_of", 0, 2));
            operands.push(json!({"outputs":[1]}));
        }
        let bytes = support::image(json!({
            "type_table":{"nodes":[{"id":"record","kind":wire::Struct,"fields":[{"name":"Value","type":integer}]}, {"id":"pointer","kind":wire::Pointer,"elem":record}]},
            "functions":[{"id":"fn.Main","signature":{"results":[integer]},
                "locals":[{"id":"value","type":record},{"id":"result","type":integer}],"code":{
                    "types":[integer,pointer],"instructions":instructions,"operands":operands,
                    "descriptors":{"fieldpath":[{"type":record,"fields":[0]}],"return":[{"result_count":1}],"address":[{"kind":"local","local":"value"}]}
                }}]
        }));
        let loaded = Program::load(&bytes, LoadLimits::default());
        if addressable {
            assert!(
                loaded.is_err(),
                "addressable aggregate accepted as a borrowed field input"
            );
            continue;
        }
        let mut vm = Instance::new(Arc::new(loaded.unwrap()), ExecutionLimits::default()).unwrap();
        vm.start("default", vec![]).unwrap();
        assert_ne!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
        assert!(matches!(vm.results()[0].data(), Data::Integer(0)));
        vm.close().unwrap();
    }
}

#[test]
fn slot_loop_preserves_exact_work_across_poll_and_instance_limits() {
    use mini_go::contract_generated as wire;
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let bytes = support::image(json!({
        "constants":[{"id":"one","type":integer,"value":1},{"id":"end","type":integer,"value":9}],
        "functions":[{"id":"fn.Main","signature":{"results":[integer]},
            "locals":[{"id":"counter","type":integer}],"code":{
                "types":[integer],
                "descriptors":{"label":[{"label":"loop"}],"operator":[{"operator":"+"}],
                    "local":[{"local":"counter"}],"comparebranch":[{"operator":"<","type":integer,"label":"loop","when":true}],
                    "return":[{"result_count":1}]},
                "instructions":[operation("label",0,0),operation("binary",0,1),operation("store_local",0,2),operation("compare_branch",0,3),operation("return",0,4)],
                "operands":[{}, {"inputs":[[2,0],[1,0]],"outputs":[0]}, {"inputs":[[0,0]],"release":[0]},
                    {"inputs":[[2,0],[1,1]]}, {"inputs":[[2,0]]}]
            }}]
    }));
    let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
    for limit in [-1, 1, 3, 8, 15, 27, 28, 29] {
        for quantum in 1..=12 {
            let mut vm = Instance::new(
                program.clone(),
                ExecutionLimits {
                    max_steps: limit,
                    ..Default::default()
                },
            )
            .unwrap();
            vm.start("default", Vec::new()).unwrap();
            loop {
                let before = vm.steps();
                let result = vm.poll_steps(quantum);
                assert!(vm.steps() - before <= quantum as u64);
                match result {
                    Err(error) => {
                        assert!((1..28).contains(&limit), "{error}");
                        assert_eq!(error.code, "step_limit");
                        assert_eq!(vm.steps(), limit as u64);
                        break;
                    }
                    Ok(PollStatus::Ready) => {
                        assert_eq!(vm.steps(), 28);
                        assert!(matches!(vm.results()[0].data(), Data::Integer(9)));
                        break;
                    }
                    _ => assert!(vm.steps() < 28),
                }
            }
            vm.close().unwrap();
        }
    }
}
