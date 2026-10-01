use mini_go::{
    contract::canonical_hash,
    contract_generated as wire,
    execution::{ExecutionState, SharedInstance},
    ffi::Cancellation,
    instance::{
        ExecutionLimits, Instance, PollStatus,
        debug::{EventKind, StepMode},
    },
    loader::LoadLimits,
    program::Program,
    snapshot::{HostData, SnapshotLimits},
};
use serde_json::json;
use std::sync::Arc;

mod support;

#[test]
fn selected_task_steps_after_its_own_instruction() {
    let main = support::slot_code(
        json!([{"kind":wire::Function,"node":"worker.signature"}]),
        &[
            (
                "make_closure",
                json!({"function":"worker"}),
                json!({"outputs":[0]}),
            ),
            (
                "spawn",
                json!({"arg_count":0}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
            ("label", json!({"label":"loop"}), json!({})),
            ("jump", json!({"label":"loop"}), json!({})),
        ],
    );
    let worker = support::slot_code(
        json!([{"kind":3,"primitive":3}]),
        &[
            (
                "const",
                json!({"constant":"answer"}),
                json!({"outputs":[0]}),
            ),
            ("pop", json!({}), json!({"inputs":[[0,0]],"release":[0]})),
            ("label", json!({"label":"loop"}), json!({})),
            ("jump", json!({"label":"loop"}), json!({})),
        ],
    );
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"worker.signature","kind":wire::Function,"signature":{}}]},
        "constants":[{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "functions":[
            {"id":"fn.Main","code":main},
            {"id":"worker","code":worker}
        ]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.start("default", vec![]).unwrap();
    assert_eq!(instance.poll_steps(2).unwrap(), PollStatus::Running);
    instance.request_pause();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Paused);
    let stack = instance.debug_stack().unwrap();
    let worker = stack
        .iter()
        .find(|frame| frame.function == "worker")
        .unwrap();
    let task = worker.reference.task;
    assert_eq!(instance.debug_threads().len(), 2);
    assert_eq!(
        instance
            .debug_resume_task(StepMode::Instruction, u64::MAX)
            .unwrap_err()
            .code,
        "invalid_task"
    );
    instance
        .debug_resume_task(StepMode::Instruction, task)
        .unwrap();
    assert_eq!(instance.poll_steps(4096).unwrap(), PollStatus::Paused);
    let event = instance.debug_events().pop().unwrap();
    assert_eq!(event.frame.reference.task, task);
    assert_eq!(event.frame.pc, 1);
    instance.close().unwrap();
}

#[test]
fn panic_stop_preserves_the_frame_before_resuming_unwinding() {
    let code = support::slot_code(
        json!([{"kind":3,"primitive":2}]),
        &[
            (
                "const",
                json!({"constant":"message"}),
                json!({"outputs":[0]}),
            ),
            ("panic", json!({}), json!({"inputs":[[0,0]],"release":[0]})),
        ],
    );
    let image = support::image(serde_json::json!({
        "constants":[{"id":"message","type":{"kind":3,"primitive":2},"value":"boom"}],
        "functions":[{"id":"fn.Main","code":code}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.set_break_on_panic(true).unwrap();
    instance.start("default", vec![]).unwrap();
    assert_eq!(instance.poll_steps(2).unwrap(), PollStatus::Paused);
    assert_eq!(instance.steps(), 2);
    let event = instance.debug_events().pop().unwrap();
    assert_eq!(event.kind, EventKind::Panic);
    assert_eq!(event.frame.pc, 1);
    assert_eq!(instance.debug_stack().unwrap()[0].function, "fn.Main");
    instance.debug_resume(StepMode::Continue).unwrap();
    assert_eq!(instance.poll_steps(1).unwrap_err().code, "panic");
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
}

#[test]
fn paged_variables_borrow_only_requested_children_and_expire_on_resume() {
    let image = support::image(serde_json::json!({
        "type_table":{"nodes":[{"id":"array","kind":6,"length":1024,"elem":{"kind":3,"primitive":3}}]},
        "functions":[{"id":"fn.Main","locals":[{"id":"items","type":{"kind":6,"node":"array"}}],"code":support::slot_code(json!([]), &[("return",json!({}),json!({}))])}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.start("default", vec![]).unwrap();
    instance.request_pause();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Paused);
    let frame = instance.debug_stack().unwrap().remove(0).reference;
    let before = instance.heap_stats();
    assert!(
        instance
            .debug_bindings(
                &frame,
                SnapshotLimits {
                    max_bytes: 64,
                    ..Default::default()
                }
            )
            .is_err()
    );
    let variables = instance.debug_variables(&frame, 0, 1).unwrap();
    assert_eq!(variables[0].name, "items");
    assert_eq!(variables[0].children, 1024);
    let reference = variables[0].reference.clone().unwrap();
    let children = instance.debug_children(&reference, 1022, 20).unwrap();
    assert_eq!(
        children
            .iter()
            .map(|child| child.name.as_str())
            .collect::<Vec<_>>(),
        ["[1022]", "[1023]"]
    );
    assert!(
        children
            .iter()
            .all(|child| child.summary == "0" && child.reference.is_none())
    );
    assert!(
        instance
            .debug_children(&reference, 1024, 10)
            .unwrap()
            .is_empty()
    );
    assert_eq!(instance.heap_stats(), before);
    instance
        .debug_resume(mini_go::instance::debug::StepMode::Continue)
        .unwrap();
    assert_eq!(
        instance.debug_children(&reference, 0, 1).unwrap_err().code,
        "stale_reference"
    );
    instance.close().unwrap();
    assert_eq!(
        instance.debug_children(&reference, 0, 1).unwrap_err().code,
        "stale_reference"
    );
}

fn program(base: i64) -> Arc<Program> {
    let integer = json!({"kind":3,"primitive":3});
    let main = support::slot_code(
        json!([integer, integer]),
        &[
            ("const", json!({"constant":"base"}), json!({"outputs":[0]})),
            (
                "store_local",
                json!({"local":"n"}),
                json!({"inputs":[[0,0]],"release":[0]}),
            ),
            ("load_local", json!({"local":"n"}), json!({"outputs":[0]})),
            (
                "call_direct",
                json!({"function":"add","arg_count":1,"result_count":1}),
                json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,1]],"release":[1]}),
            ),
        ],
    );
    let add = support::slot_code(
        json!([integer, integer, integer]),
        &[
            ("load_local", json!({"local":"x"}), json!({"outputs":[0]})),
            ("const", json!({"constant":"delta"}), json!({"outputs":[1]})),
            (
                "binary",
                json!({"operator":"+"}),
                json!({"inputs":[[0,0],[0,1]],"outputs":[2],"release":[0,1]}),
            ),
            (
                "return",
                json!({"result_count":1}),
                json!({"inputs":[[0,2]],"release":[2]}),
            ),
        ],
    );
    let image = support::image(json!({
        "constants":[{"id":"base","type":{"kind":3,"primitive":3},"value":base},{"id":"delta","type":{"kind":3,"primitive":3},"value":22}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer]},"locals":[{"id":"n","type":integer}],"code":main},
            {"id":"add","signature":{"params":[{"type":integer}],"results":[integer]},"locals":[{"id":"x","type":integer}],"code":add}
        ]
    }));
    let program = Program::load(&image, LoadLimits::default()).unwrap();
    let contract: serde_json::Value = serde_json::from_str(wire::CONTRACT_JSON).unwrap();
    let mut symbols: wire::ProgramSymbols = serde_json::from_value(json!({
        "format":contract["spec"]["symbols_format"],"version":contract["spec"]["symbols_version"],"contract_id":contract["spec"]["symbols_contract"],
        "compiler_id":program.image().compiler_id,"program_hash":program.image().hash,
        "packages":{"test":{"module_path":"test","code_hash":program.image().packages.as_ref().unwrap()["test"].artifact_hash,
            "files":[{"id":"source","path":"main.mgo"}],"functions":[
                {"id":"fn.Main","name":"Main","locals":[{"id":"n","name":"number","scope":1}],"scopes":[{"id":1,"ranges":[{"start":0,"end":5}]}],
                 "locations":(0..5).map(|pc| json!({"pc":pc,"points":[{"file":"source","line":pc+1,"column":1}]})).collect::<Vec<_>>()},
                {"id":"add","name":"add","locals":[{"id":"x","name":"input"}],
                 "locations":(0..4).map(|pc| json!({"pc":pc,"points":[{"file":"source","line":pc+10,"column":1}]})).collect::<Vec<_>>()}
            ]}}
    })).unwrap();
    symbols.hash = canonical_hash(&symbols).unwrap();
    Arc::new(program.with_symbols(symbols).unwrap())
}

#[test]
fn source_stops_and_steps_preserve_budget_and_expire_frame_references() {
    let mut instance = Instance::new(program(20), ExecutionLimits::default()).unwrap();
    assert_eq!(
        instance.set_breakpoints("test", "main.mgo", &[3]).unwrap(),
        vec![3]
    );
    instance.start_profile(1, 2).unwrap();
    instance.start("default", Vec::new()).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Paused);
    assert_eq!(instance.steps(), 2);
    let frame = instance.debug_stack().unwrap().remove(0);
    assert_eq!(frame.pc, 2);
    assert_eq!(instance.debug_events()[0].kind, EventKind::Breakpoint);
    let values = instance
        .debug_bindings(&frame.reference, SnapshotLimits::default())
        .unwrap();
    assert_eq!(values.names, vec!["number"]);
    assert!(matches!(values.values.roots[0].data, HostData::Integer(20)));
    instance.debug_resume(StepMode::Into).unwrap();
    assert_eq!(
        instance
            .debug_bindings(&frame.reference, SnapshotLimits::default())
            .unwrap_err()
            .code,
        "stale_reference"
    );
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Paused);
    assert_eq!(instance.debug_stack().unwrap()[0].pc, 3);
    instance.debug_resume(StepMode::Over).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Paused);
    let frame = &instance.debug_stack().unwrap()[0];
    assert_eq!((&*frame.function, frame.pc), ("fn.Main", 4));
    instance.debug_resume(StepMode::Continue).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(instance.results()[0].integer().unwrap(), 42);
    let profile = instance.profile();
    assert_eq!(
        profile
            .samples
            .iter()
            .map(|sample| sample.count)
            .sum::<u64>()
            + profile.dropped,
        instance.steps()
    );
    assert!(profile.dropped > 0);
    instance.close().unwrap();
    assert!(matches!(values.values.roots[0].data, HostData::Integer(20)));
}

#[test]
fn waiting_task_pauses_without_consuming_a_step_and_cancels_cleanly() {
    let integer = json!({"kind":wire::Primitive,"primitive":wire::PrimitiveInt});
    let channel = json!({"kind":wire::Waitable,"node":"channel"});
    let image = support::image(json!({
        "type_table":{"nodes":[{"id":"channel","kind":wire::Waitable,"direction":1,"elem":integer}]},
        "functions":[{"id":"fn.Main","signature":{"results":[integer]},
            "code":support::slot_code(json!([channel,integer]), &[
                ("zero",json!({"type":channel}),json!({"outputs":[0]})),
                ("waitable_recv",json!({}),json!({"inputs":[[0,0]],"outputs":[1],"release":[0]})),
                ("return",json!({"result_count":1}),json!({"inputs":[[0,1]],"release":[1]}))
            ])}]
    }));
    let program = Arc::new(Program::load(&image, LoadLimits::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    instance.start("default", Vec::new()).unwrap();
    instance.poll_steps(2).unwrap();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Pending);
    let steps = instance.steps();
    instance.request_pause();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Paused);
    assert_eq!(instance.steps(), steps);
    let frames = instance.debug_stack().unwrap();
    assert_eq!(frames[0].function, "fn.Main");
    assert_eq!(
        instance.debug_events().last().unwrap().kind,
        EventKind::Pause
    );
    instance.debug_resume(StepMode::Continue).unwrap();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Pending);
    instance.request_pause();
    assert_eq!(instance.poll_steps(1).unwrap(), PollStatus::Paused);
    instance.cancel().unwrap();
    assert_eq!(instance.stats().paused_tasks, 0);
    instance.close().unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
}

#[test]
fn shared_pause_is_observed_before_an_instruction_and_cancellation_releases_it() {
    let instance = SharedInstance::new(program(20), ExecutionLimits::default()).unwrap();
    let debugger = instance.debugger();
    debugger.pause();
    let execution = loop {
        match instance.start("default", Vec::new()) {
            Ok(execution) => break execution,
            Err(error) if error.code == "busy" => std::thread::yield_now(),
            Err(error) => panic!("{error}"),
        }
    };
    loop {
        match execution.poll_steps(1) {
            Ok(result) => {
                assert_eq!(result, (ExecutionState::Paused, 0));
                break;
            }
            Err(error) if error.code == "busy" => std::thread::yield_now(),
            Err(error) => panic!("{error}"),
        }
    }
    execution.cancel();
    assert!(execution.wait_scope(&Cancellation::default()).is_err());
    assert_eq!(execution.state(), ExecutionState::Canceled);
    instance.shutdown(&Cancellation::default()).unwrap();
    assert_eq!(instance.heap_stats().live_objects, 0);
}

#[test]
fn patch_rebinds_breakpoints_and_expires_inspection_without_rewriting_old_frames() {
    let mut instance = Instance::new(program(20), ExecutionLimits::default()).unwrap();
    instance.start_profile(1, 128).unwrap();
    instance.set_breakpoints("test", "main.mgo", &[3]).unwrap();
    instance.start("default", vec![]).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Paused);
    let frame = instance.debug_stack().unwrap().remove(0);
    let plan = instance.prepare_patch(program(40)).unwrap();
    instance.apply_patch(plan).unwrap();
    assert_eq!(
        instance
            .debug_bindings(&frame.reference, Default::default())
            .unwrap_err()
            .code,
        "stale_reference"
    );
    let retained = instance.debug_stack().unwrap().remove(0);
    assert_eq!(retained.generation, 1);
    assert_eq!(retained.program_hash, frame.program_hash);
    instance.debug_resume(StepMode::Continue).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(instance.results()[0].integer().unwrap(), 42);
    instance.start("default", vec![]).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Paused);
    let current = instance.debug_stack().unwrap().remove(0);
    assert_eq!(current.generation, 2);
    assert_ne!(current.program_hash, retained.program_hash);
    let values = instance
        .debug_bindings(&current.reference, Default::default())
        .unwrap();
    assert!(matches!(values.values.roots[0].data, HostData::Integer(40)));
    instance.debug_resume(StepMode::Continue).unwrap();
    assert_eq!(instance.poll_steps(100).unwrap(), PollStatus::Ready);
    assert_eq!(instance.results()[0].integer().unwrap(), 62);
    let profile = instance.profile();
    assert_eq!(profile.dropped, 0);
    assert_eq!(
        profile
            .samples
            .iter()
            .map(|sample| sample.count)
            .sum::<u64>(),
        instance.steps()
    );
    for generation in [1, 2] {
        assert!(
            profile
                .samples
                .iter()
                .any(|sample| sample.generation == generation)
        );
    }
    for sample in profile.samples {
        let expected = match sample.function.as_str() {
            "fn.Main" => [
                "const",
                "store_local",
                "load_local",
                "call_direct",
                "return",
            ][sample.pc],
            "add" => ["load_local", "const", "binary", "return"][sample.pc],
            name => panic!("unexpected profiled function {name}"),
        };
        assert_eq!(sample.opcode, expected);
        assert!(!sample.locations.is_empty());
        assert_eq!(
            sample.program_hash,
            if sample.generation == 1 {
                &retained.program_hash
            } else {
                &current.program_hash
            }
            .as_str()
        );
    }
    instance.close().unwrap();
}
