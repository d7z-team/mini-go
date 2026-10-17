mod support;

use mini_go::{
    contract::{canonical_hash, canonical_json},
    contract_generated as wire,
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
};
use serde_json::json;
use std::sync::Arc;

#[test]
fn stored_closure_keeps_its_revision_through_collection_and_repeated_patches() {
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let function = json!({"kind":wire::Function,"node":"callback"});
    let build = |answer: i64, operation: &str| {
        let mut operations = match operation {
            "save" => vec![
                (
                    "make_closure",
                    json!({"function":"value"}),
                    json!({"outputs":[0]}),
                ),
                (
                    "store_global",
                    json!({"global":"held"}),
                    json!({"inputs":[[0,0]],"release":[0]}),
                ),
                (
                    "const",
                    json!({"constant":"answer"}),
                    json!({"outputs":[1]}),
                ),
            ],
            "call" => vec![
                (
                    "load_global",
                    json!({"global":"held"}),
                    json!({"outputs":[0]}),
                ),
                (
                    "call_value",
                    json!({"result_count":1}),
                    json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
                ),
            ],
            "clear" => vec![
                ("zero", json!({"type":function}), json!({"outputs":[0]})),
                (
                    "store_global",
                    json!({"global":"held"}),
                    json!({"inputs":[[0,0]],"release":[0]}),
                ),
                (
                    "const",
                    json!({"constant":"answer"}),
                    json!({"outputs":[1]}),
                ),
            ],
            _ => unreachable!(),
        };
        operations.push((
            "return",
            json!({"result_count":1}),
            json!({"inputs":[[0,1]],"release":[1]}),
        ));
        Arc::new(Program::load(&support::image(json!({
            "type_table":{"nodes":[{"id":"callback","kind":wire::Function,"signature":{"results":[integer]}}]},
            "constants":[{"id":"answer","type":integer,"value":answer}],
            "globals":[{"id":"held","type":function}],
            "functions":[
                {"id":"fn.Main","signature":{"results":[integer]},"code":support::slot_code(json!([function,integer]), &operations)},
                {"id":"value","revision_local":true,"signature":{"results":[integer]},"code":support::slot_code(json!([integer]), &[
                    ("const",json!({"constant":"answer"}),json!({"outputs":[0]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))
                ])}
            ]
        })), LoadLimits::default()).unwrap())
    };
    let mut instance = Instance::new(build(20, "save"), ExecutionLimits::default()).unwrap();
    let original = instance.revision();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(16).unwrap(), PollStatus::Ready);
    instance.collect_garbage().unwrap();
    for answer in [30, 40, 50] {
        let patch = instance.prepare_patch(build(answer, "call")).unwrap();
        instance.apply_patch(patch).unwrap();
        instance.collect_garbage().unwrap();
        instance.start("default", &[]).unwrap();
        assert_eq!(instance.poll_steps(16).unwrap(), PollStatus::Ready);
        assert_eq!(
            instance.snapshot_results(Default::default()).unwrap().roots[0]
                .integer()
                .unwrap(),
            20
        );
        assert!(instance.retained_revisions().contains(&original));
    }
    let patch = instance.prepare_patch(build(42, "clear")).unwrap();
    instance.apply_patch(patch).unwrap();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(16).unwrap(), PollStatus::Ready);
    instance.collect_garbage().unwrap();
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        42
    );
    assert_eq!(instance.retained_revisions(), [instance.revision()]);
    instance.close().unwrap();
}

#[test]
fn patch_static_types_and_dynamic_metadata_have_separate_budgets() {
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let build = |answer: i64| {
        Arc::new(Program::load(&support::image(json!({
            "type_table":{"nodes":[
                {"id":"pair","kind":wire::Array,"length":2,"elem":integer},
                {"id":"triple","kind":wire::Array,"length":3,"elem":integer}
            ]},
            "constants":[{"id":"answer","type":integer,"value":answer}],
            "functions":[{"id":"fn.Main","signature":{"results":[integer]},
                "code":support::slot_code(json!([integer]), &[
                    ("const",json!({"constant":"answer"}),json!({"outputs":[0]})),
                    ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))
                ])}]
        })), LoadLimits::default()).unwrap())
    };
    let mut instance = Instance::new(
        build(1),
        ExecutionLimits {
            max_dynamic_types: 1,
            max_boundary_elements: 1,
            ..Default::default()
        },
    )
    .unwrap();
    let plan = instance.prepare_patch(build(42)).unwrap();
    instance.apply_patch(plan).unwrap();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(8).unwrap(), PollStatus::Ready);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        42
    );
    assert_eq!(instance.stats().dynamic_types, 0);
    instance.close().unwrap();
}

#[test]
fn method_rebinding_rejects_patch_and_preserves_existing_receiver_metadata() {
    let body = support::slot_code(
        json!([{"kind":3,"primitive":3}]),
        &[
            (
                "const",
                json!({"constant":"answer"}),
                json!({"outputs":[0]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
        ],
    );
    let build = |function: &str| {
        Arc::new(Program::load(&support::image(json!({
            "type_table":{"nodes":[{"id":"number","kind":4,
                "identity":{"module_path":"test","decl_id":"Number"},
                "underlying":{"kind":3,"primitive":3},
                "methods":[{"name":"Value","receiver":{"kind":4,"named":{"module_path":"test","decl_id":"Number"}},
                    "signature":{"results":[{"kind":3,"primitive":3}]},"function_id":function,"module_path":"test"}]}]},
            "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
            "functions":[
                {"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"code":body},
                {"id":"first","signature":{"params":[{"type":{"kind":4,"named":{"module_path":"test","decl_id":"Number"}}}],"results":[{"kind":3,"primitive":3}]},
                    "locals":[{"id":"self","type":{"kind":4,"named":{"module_path":"test","decl_id":"Number"}}}],
                    "code":body},
                {"id":"second","signature":{"params":[{"type":{"kind":4,"named":{"module_path":"test","decl_id":"Number"}}}],"results":[{"kind":3,"primitive":3}]},
                    "locals":[{"id":"self","type":{"kind":4,"named":{"module_path":"test","decl_id":"Number"}}}],
                    "code":body}
            ]
        })), LoadLimits::default()).unwrap())
    };
    let mut instance = Instance::new(build("first"), ExecutionLimits::default()).unwrap();
    let revision = instance.revision();
    let error = instance.prepare_patch(build("second")).err().unwrap();
    assert_eq!(error.code, "type_shape_changed");
    assert_eq!(instance.revision(), revision);
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(2).unwrap(), PollStatus::Ready);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        42
    );
    instance.close().unwrap();
}

#[test]
fn collection_reclaims_canceled_revision_without_changing_the_current_program() {
    let mut instance = Instance::new(
        program(10, false),
        ExecutionLimits {
            max_retained_revisions: 2,
            ..Default::default()
        },
    )
    .unwrap();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Running);
    let plan = instance.prepare_patch(program(20, false)).unwrap();
    instance.apply_patch(plan).unwrap();
    let revision = instance.revision();
    let heap = instance.heap_stats();
    let error = instance.prepare_patch(program(42, false)).err().unwrap();
    assert_eq!(error.code, "resource_limit");
    assert_eq!(instance.revision(), revision);
    assert_eq!(instance.heap_stats(), heap);
    instance.cancel().unwrap();
    instance.collect_garbage().unwrap();
    assert_eq!(instance.retained_revisions(), [instance.revision()]);
    let plan = instance.prepare_patch(program(42, false)).unwrap();
    instance.apply_patch(plan).unwrap();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(20).unwrap(), PollStatus::Ready);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        42
    );
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
}

#[test]
fn patch_global_allocation_failure_preserves_heap_results_and_generation() {
    let original = program(20, false);
    let mut target = program(42, false).image().clone();
    let extra: wire::ExecutionImage = serde_json::from_slice(&support::image(json!({
        "globals":(0..20).map(|index| json!({"id":format!("global{index}"),"type":{"kind":3,"primitive":3}})).collect::<Vec<_>>(),
        "functions":[{"id":"fn.Main","code":support::slot_code(json!([]), &[("return", json!({}), json!({}))])}]
    }))).unwrap();
    let mut archive = extra.packages.unwrap().remove("test").unwrap();
    let mut artifact: wire::Artifact =
        serde_json::from_str(archive.artifact.as_ref().unwrap().get()).unwrap();
    artifact.module.path = "extra".into();
    archive.artifact_hash = canonical_hash(&artifact).unwrap();
    archive.artifact = Some(
        serde_json::value::RawValue::from_string(
            String::from_utf8(canonical_json(&artifact).unwrap()).unwrap(),
        )
        .unwrap(),
    );
    target
        .packages
        .as_mut()
        .unwrap()
        .insert("extra".into(), archive);
    target.hash.clear();
    target.hash = canonical_hash(&target).unwrap();
    let target =
        Arc::new(Program::load(&canonical_json(&target).unwrap(), LoadLimits::default()).unwrap());
    let mut instance = Instance::new(
        original,
        ExecutionLimits {
            max_guest_bytes: 512,
            ..Default::default()
        },
    )
    .unwrap();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(20).unwrap(), PollStatus::Ready);
    let revision = instance.revision();
    let heap = instance.heap_stats();
    let plan = instance.prepare_patch(target).unwrap();
    assert_eq!(
        instance.apply_patch(plan).unwrap_err().code,
        "allocation_limit"
    );
    assert_eq!(instance.revision(), revision);
    let after = instance.heap_stats();
    // Staging may allocate and roll back records. Historical allocation and
    // peak counters retain that work; the live guest state must be unchanged.
    assert_eq!(after.live_objects, heap.live_objects);
    assert_eq!(after.guest_bytes, heap.guest_bytes);
    assert_eq!(after.reserved_bytes, heap.reserved_bytes);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        20
    );
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(20).unwrap(), PollStatus::Ready);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        20
    );
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_bytes, 0);
}

fn program(number: i64, closure: bool) -> Arc<Program> {
    program_with_options(number, closure, false)
}

fn program_with_revision_local(number: i64, revision_local: bool) -> Arc<Program> {
    program_with_options(number, false, revision_local)
}

fn program_with_options(number: i64, closure: bool, revision_local: bool) -> Arc<Program> {
    let integer = json!({"kind":3,"primitive":3});
    let mut operations = vec![];
    if closure {
        operations.push((
            "make_closure",
            json!({"function":"value"}),
            json!({"outputs":[0]}),
        ));
        operations.push((
            "call_value",
            json!({"result_count":1}),
            json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
        ));
    } else {
        operations.push(("const", json!({"constant":"n"}), json!({"outputs":[0]})));
        operations.push(("pop", json!({}), json!({"inputs":[[0,0]],"release":[0]})));
        operations.push((
            "call_direct",
            json!({"function":"value","result_count":1}),
            json!({"outputs":[1]}),
        ));
    }
    operations.push((
        "return",
        json!({"result_count":1}),
        json!({"inputs":[[0,1]],"release":[1]}),
    ));
    let first_type = if closure {
        json!({"kind":wire::Function,"node":"value.signature"})
    } else {
        integer.clone()
    };
    let code = support::slot_code(json!([first_type, integer]), &operations);
    let value = support::slot_code(
        json!([integer]),
        &[
            ("const", json!({"constant":"n"}), json!({"outputs":[0]})),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"value.signature","kind":wire::Function,"signature":{"results":[integer]}}]},
        "constants":[{"id":"n","type":{"kind":3,"primitive":3},"value":number}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer]},"code":code},
            {"id":"value","revision_local":revision_local,"signature":{"results":[integer]},"code":value}
        ]
    }));
    Arc::new(Program::load(&image, LoadLimits::default()).unwrap())
}

#[test]
fn anonymous_types_are_resolved_in_their_frame_revision() {
    let array = json!({"kind":6,"node":"source.array"});
    let body = support::slot_code(
        json!([array,{"kind":3,"primitive":3}]),
        &[
            ("zero", json!({"type":array}), json!({"outputs":[0]})),
            ("pop", json!({}), json!({"inputs":[[0,0]],"release":[0]})),
            ("zero", json!({"type":array}), json!({"outputs":[0]})),
            (
                "len",
                json!({}),
                json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,1]],"release":[1]}),
            ),
        ],
    );
    let build = |length| {
        Arc::new(Program::load(&support::image(json!({
            "type_table":{"nodes":[{"id":"source.array","kind":6,"length":length,"elem":{"kind":3,"primitive":3}}]},
            "functions":[{"id":"fn.Main","signature":{"results":[{"kind":3,"primitive":3}]},"code":body}]
        })), LoadLimits::default()).unwrap())
    };
    let mut instance = Instance::new(build(2), ExecutionLimits::default()).unwrap();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Running);
    let patch = instance.prepare_patch(build(3)).unwrap();
    instance.apply_patch(patch).unwrap();
    assert_eq!(instance.poll_steps(10).unwrap(), PollStatus::Ready);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        2
    );
    instance.collect_garbage().unwrap();
    assert_eq!(instance.retained_revisions(), [instance.revision()]);
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(10).unwrap(), PollStatus::Ready);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        3
    );
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
}

#[test]
fn active_frames_keep_code_and_named_calls_use_the_published_revision() {
    for closure in [false, true] {
        let mut instance = Instance::new(program(20, closure), ExecutionLimits::default()).unwrap();
        instance.start_profile(1, 100).unwrap();
        instance.start("default", &[]).unwrap();
        assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Running);
        let plan = instance.prepare_patch(program(42, closure)).unwrap();
        assert_eq!(plan.base().generation, 1);
        let applied = instance.apply_patch(plan).unwrap();
        assert_eq!(applied.current.generation, 2);
        assert_eq!(instance.retained_revisions()[0].generation, 1);
        assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
        assert_eq!(
            instance.snapshot_results(Default::default()).unwrap().roots[0]
                .integer()
                .unwrap(),
            if closure { 20 } else { 42 }
        );
        instance.collect_garbage().unwrap();
        assert_eq!(instance.retained_revisions(), [instance.revision()]);
        let profile = instance.profile();
        assert!(profile.samples.iter().any(|sample| sample.generation == 1));
        if !closure {
            assert!(profile.samples.iter().any(|sample| sample.generation == 2));
        }
        instance.start("default", &[]).unwrap();
        assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
        assert_eq!(
            instance.snapshot_results(Default::default()).unwrap().roots[0]
                .integer()
                .unwrap(),
            42
        );
        instance.close().unwrap();
        assert_eq!(instance.heap_stats().live_objects, 0);
    }
}

#[test]
fn revision_local_direct_calls_keep_the_callers_revision() {
    let mut instance = Instance::new(
        program_with_revision_local(20, true),
        ExecutionLimits::default(),
    )
    .unwrap();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Running);
    let plan = instance
        .prepare_patch(program_with_revision_local(42, true))
        .unwrap();
    instance.apply_patch(plan).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        20
    );
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        42
    );
    instance.close().unwrap();
}

#[test]
fn logical_calls_rebind_after_function_reordering_without_affecting_other_instances() {
    let old = program(20, false);
    let target = program(42, false);
    let package = target
        .image()
        .packages
        .as_ref()
        .unwrap()
        .values()
        .next()
        .unwrap();
    let mut artifact: serde_json::Value =
        serde_json::from_str(package.artifact.as_ref().unwrap().get()).unwrap();
    artifact["functions"].as_array_mut().unwrap().reverse();
    let reordered =
        Arc::new(Program::load(&support::image(artifact), LoadLimits::default()).unwrap());
    let mut first = Instance::new(old.clone(), ExecutionLimits::default()).unwrap();
    let mut second = Instance::new(old, ExecutionLimits::default()).unwrap();
    for vm in [&mut first, &mut second] {
        vm.start("default", &[]).unwrap();
        assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Running);
    }
    let plan = first.prepare_patch(reordered).unwrap();
    first.apply_patch(plan).unwrap();
    for (vm, expected) in [(&mut first, 42), (&mut second, 20)] {
        while vm.poll_steps(1).unwrap() != PollStatus::Ready {}
        assert_eq!(
            vm.snapshot_results(Default::default()).unwrap().roots[0]
                .integer()
                .unwrap(),
            expected
        );
        vm.close().unwrap();
    }
}

#[test]
fn deferred_closure_runs_old_code_then_releases_the_revision() {
    let integer = json!({"kind":3,"primitive":3});
    let main = support::slot_code(
        json!([{"kind":wire::Function,"node":"save.signature"},integer]),
        &[
            (
                "make_closure",
                json!({"function":"save"}),
                json!({"outputs":[0]}),
            ),
            (
                "defer_push",
                json!({"owner_depth":0}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
            (
                "load_global",
                json!({"global":"answer"}),
                json!({"outputs":[1]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,1]],"release":[1]}),
            ),
        ],
    );
    let save = support::slot_code(
        json!([integer]),
        &[
            ("const", json!({"constant":"n"}), json!({"outputs":[0]})),
            (
                "store_global",
                json!({"global":"answer"}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
            ("return", json!({}), json!({})),
        ],
    );
    let build = |number| {
        Arc::new(Program::load(&support::image(json!({
        "type_table":{"nodes":[{"id":"save.signature","kind":wire::Function,"signature":{}}]},
        "constants":[{"id":"n","type":{"kind":3,"primitive":3},"value":number}],
        "globals":[{"id":"answer","type":{"kind":3,"primitive":3}}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer]},"code":main},
            {"id":"save","code":save}
        ]
    })), LoadLimits::default()).unwrap())
    };
    let mut instance = Instance::new(build(20), ExecutionLimits::default()).unwrap();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(2).unwrap(), PollStatus::Running);
    let plan = instance.prepare_patch(build(42)).unwrap();
    instance.apply_patch(plan).unwrap();
    instance.collect_garbage().unwrap();
    assert_eq!(instance.retained_revisions().len(), 2);
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    instance.collect_garbage().unwrap();
    assert_eq!(instance.retained_revisions(), [instance.revision()]);
    for expected in [20, 42] {
        instance.start("default", &[]).unwrap();
        assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
        assert_eq!(
            instance.snapshot_results(Default::default()).unwrap().roots[0]
                .integer()
                .unwrap(),
            expected
        );
    }
    instance.close().unwrap();
}

#[test]
fn plans_are_bound_to_owner_and_base_and_failure_preserves_execution() {
    let original = program(20, false);
    let target = program(42, false);
    let mut first = Instance::new(original.clone(), ExecutionLimits::default()).unwrap();
    let mut second = Instance::new(original, ExecutionLimits::default()).unwrap();
    let foreign = first.prepare_patch(target.clone()).unwrap();
    assert_eq!(
        second.apply_patch(foreign).unwrap_err().code,
        "wrong_instance"
    );
    assert_eq!(second.revision().generation, 1);
    let stale = first.prepare_patch(target.clone()).unwrap();
    let valid = first.prepare_patch(target).unwrap();
    first.apply_patch(valid).unwrap();
    assert_eq!(first.apply_patch(stale).unwrap_err().code, "stale_plan");
    assert_eq!(first.revision().generation, 2);
    second.start("default", &[]).unwrap();
    assert_eq!(second.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(
        second.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        20
    );
    first.close().unwrap();
    second.close().unwrap();
}

#[test]
fn patch_preserves_globals_and_shape_failure_preserves_revision() {
    let build = |delta: i64, primitive: u8| {
        let typ = json!({"kind":3,"primitive":primitive});
        let body = support::slot_code(
            json!([typ, typ, typ]),
            &[
                (
                    "load_global",
                    json!({"global":"counter"}),
                    json!({"outputs":[0]}),
                ),
                ("const", json!({"constant":"delta"}), json!({"outputs":[1]})),
                (
                    "binary",
                    json!({"operator":"+"}),
                    json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]}),
                ),
                (
                    "store_global",
                    json!({"global":"counter"}),
                    json!({"inputs":[[0,2]],"release":[2]}),
                ),
                (
                    "load_global",
                    json!({"global":"counter"}),
                    json!({"outputs":[0]}),
                ),
                (
                    "return",
                    json!({"result_count":1}),
                    json!({"inputs":[[0,0]],"release":[0]}),
                ),
            ],
        );
        let image = support::image(json!({
            "constants":[{"id":"delta","type":typ,"value":delta}],
            "globals":[{"id":"counter","type":typ}],
            "functions":[{"id":"fn.Main","signature":{"results":[typ]},"code":body}]
        }));
        Arc::new(Program::load(&image, LoadLimits::default()).unwrap())
    };
    let mut instance = Instance::new(build(20, 3), ExecutionLimits::default()).unwrap();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        20
    );
    let before = instance.heap_stats();
    let revision = instance.revision();
    assert_eq!(
        instance.prepare_patch(build(2, 4)).err().unwrap().code,
        "global_shape_changed"
    );
    assert_eq!(instance.revision(), revision);
    assert_eq!(instance.heap_stats(), before);
    let patch = instance.prepare_patch(build(22, 3)).unwrap();
    instance.apply_patch(patch).unwrap();
    instance.start("default", &[]).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(
        instance.snapshot_results(Default::default()).unwrap().roots[0]
            .integer()
            .unwrap(),
        42
    );
    instance.close().unwrap();
}
