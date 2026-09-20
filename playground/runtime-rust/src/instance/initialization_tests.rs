use super::*;

#[test]
fn entry_initialization_belongs_to_the_admitted_task() {
    let program = test_helpers::program_with_artifact(|artifact| {
        artifact["functions"] = serde_json::json!([
            {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]},
            {"id":"fn.init", "instructions":[{"op":"return","payload":{}}]}
        ]);
    });
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    vm.start("default", Vec::new()).unwrap();
    let owner = vm.running.id;
    assert_eq!(
        vm.initializing.values().copied().collect::<Vec<_>>(),
        vec![owner]
    );
    assert_eq!(vm.poll_steps(8).unwrap(), PollStatus::Ready);
    assert!(vm.initializing.is_empty());
    vm.close().unwrap();
}

#[test]
fn module_wait_releases_task_and_observes_completion_or_failure() {
    for failed in [false, true] {
        let program = test_helpers::program_with_artifact(|artifact| {
            artifact["functions"] = serde_json::json!([
                {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
            ]);
        });
        let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
        vm.start("default", Vec::new()).unwrap();
        let task = vm.running.id;
        vm.initializing.insert("dependency".into(), task + 1);
        assert!(vm.initialize_module("dependency").unwrap());
        assert!(vm.running.frames.is_empty());
        vm.resume_blocked().unwrap();
        assert!(vm.runnable.is_empty());
        assert_eq!(vm.blocked.iter().next().unwrap().id, task);
        vm.initializing.remove("dependency");
        if failed {
            vm.failed_initializations.insert(
                "dependency".into(),
                RuntimeError::new("internal", "module_init", "initializer failed"),
            );
        } else {
            vm.initialized.insert("dependency".into());
        }
        vm.blocked.notify_module("dependency");
        vm.blocked.notify_module("dependency");
        if failed {
            assert_eq!(
                vm.resume_blocked().unwrap_err().message,
                "initializer failed"
            );
            assert!(vm.runnable.is_empty());
        } else {
            vm.resume_blocked().unwrap();
            assert!(vm.blocked.is_empty());
            assert_eq!(vm.runnable.len(), 1);
            assert_eq!(vm.poll_steps(1).unwrap(), PollStatus::Ready);
            assert_eq!(vm.steps(), 1);
        }
        vm.close().unwrap();
    }
}

#[test]
fn module_wait_detects_cross_task_cycle_before_parking() {
    let program = test_helpers::program_with_artifact(|artifact| {
        artifact["functions"] = serde_json::json!([
            {"id":"fn.Main", "instructions":[{"op":"return","payload":{}}]}
        ]);
    });
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    vm.start("default", Vec::new()).unwrap();
    let task = vm.running.id;
    vm.initializing.insert("first".into(), task);
    vm.initializing.insert("second".into(), task + 1);
    vm.blocked.push(
        scheduler::Task {
            id: task + 1,
            scope: vm.running.scope,
            frames: Vec::new(),
            blocked: Some(scheduler::Blocked::Module("first".into())),
            ..scheduler::Task::default()
        },
        Vec::new(),
    );
    assert_eq!(
        vm.initialize_module("second").unwrap_err().message,
        "module initialization cycle"
    );
    assert!(!vm.running.frames.is_empty());
    vm.blocked.clear();
    vm.initializing.clear();
    vm.close().unwrap();
}
