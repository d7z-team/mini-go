use mini_go::{
    ffi::Cancellation,
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::{DecodedImage, LoadLimits},
    program::Program,
};
use std::{
    sync::Arc,
    time::{Duration, Instant},
};

fn compiler_instance(max_steps: i64, deadline: Instant) -> Instance {
    let image = DecodedImage::decode_gzip(
        include_bytes!("../assets/compiler.json.gz"),
        mini_go::compiler::CompilerOptions::default().load,
    )
    .unwrap();
    let program = Arc::new(Program::prepare(image).unwrap());
    let mut vm = Instance::new(
        program,
        ExecutionLimits {
            max_steps,
            ..mini_go::compiler::CompilerOptions::default().limits
        },
    )
    .unwrap();
    while vm.poll_initialize(&Cancellation::default(), 4096).unwrap() != PollStatus::Ready {
        assert!(
            Instant::now() < deadline,
            "compiler initialization watchdog: {:?}",
            vm.stats()
        );
    }
    vm
}

#[test]
fn compiler_workload_initializes_and_checks_source() {
    let deadline = Instant::now() + Duration::from_secs(600);
    let mut vm = compiler_instance(5_000_000, deadline);
    let mut envelope = serde_json::json!({});
    for index in 0..3 {
        let request = if index == 0 {
            serde_json::json!({})
        } else {
            serde_json::json!({
                "Format": envelope["Format"], "Version": envelope["Version"], "Operation":"check", "Root":"probe",
                "Packages":[{"Namespace":"module:probe", "PackagePath":"", "ModulePath":"probe", "Files":[{"Path":"main.mgo","Text":"package main\nfunc main() {}\n"}]}]
            })
        };
        vm.start_bytes("default", &serde_json::to_vec(&request).unwrap())
            .unwrap();
        while vm.poll_steps(4096).unwrap() != PollStatus::Ready {
            if Instant::now() >= deadline {
                let stats = vm.stats();
                vm.close().unwrap();
                panic!("compiler request {index} watchdog: {stats:?}");
            }
        }
        let snapshot = vm.snapshot_results(Default::default()).unwrap();
        envelope = serde_json::from_slice(&snapshot.bytes(&snapshot.roots[0]).unwrap()).unwrap();
        if index == 0 {
            assert!(!envelope["Error"].as_str().unwrap().is_empty());
        } else {
            assert_eq!(envelope["Error"], "");
            assert!(envelope["Diagnostics"].is_null());
        }
        assert_eq!(vm.stats().active_scopes, 0);
        assert_eq!(vm.stats().pending_ffi_calls, 0);
    }
    vm.close().unwrap();
}

#[test]
fn compiler_prepares_unicode_with_default_limits() {
    let deadline = Instant::now() + Duration::from_secs(600);
    let mut compiler = compiler_instance(
        mini_go::compiler::CompilerOptions::default()
            .limits
            .max_steps,
        deadline,
    );
    compiler.start_bytes("default", b"{}").unwrap();
    while compiler.poll_steps(4096).unwrap() != PollStatus::Ready {
        assert!(Instant::now() < deadline, "compiler handshake watchdog");
    }
    let snapshot = compiler.snapshot_results(Default::default()).unwrap();
    let envelope: serde_json::Value =
        serde_json::from_slice(&snapshot.bytes(&snapshot.roots[0]).unwrap()).unwrap();
    let request = serde_json::json!({
        "Format": envelope["Format"], "Version": envelope["Version"],
        "Operation": "prepare", "Root": "probe",
        "Packages": [{"Namespace": "module:probe", "ModulePath": "probe",
            "Files": [{"Path": "main.mgo", "Text":
                "package main\nimport \"unicode\"\nfunc Main() bool { return unicode.IsLetter('中') && unicode.ToUpper('a') == 'A' }\n"}]}],
        "EntryPoints": [{"Name": "default", "ModulePath": "probe", "Function": "Main"}]
    });
    compiler
        .start_bytes("default", &serde_json::to_vec(&request).unwrap())
        .unwrap();
    while compiler.poll_steps(4096).unwrap() != PollStatus::Ready {
        assert!(
            Instant::now() < deadline,
            "Unicode prepare watchdog: {:?}",
            compiler.stats()
        );
    }
    let snapshot = compiler.snapshot_results(Default::default()).unwrap();
    let bytes = snapshot.bytes(&snapshot.roots[0]).unwrap();
    let response: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(response["Error"], "");
    assert!(response["Diagnostics"].as_array().is_none_or(Vec::is_empty));
    let fields: std::collections::BTreeMap<String, Box<serde_json::value::RawValue>> =
        serde_json::from_slice(&bytes).unwrap();
    assert_eq!(compiler.stats().active_scopes, 0);
    compiler.close().unwrap();
    drop(compiler);

    let program =
        Arc::new(Program::load(fields["Image"].get().as_bytes(), LoadLimits::default()).unwrap());
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    vm.start("default", Vec::new()).unwrap();
    while vm.poll_steps(4096).unwrap() != PollStatus::Ready {
        assert!(
            Instant::now() < deadline,
            "compiled Unicode execution watchdog"
        );
    }
    let output = vm.snapshot_results(Default::default()).unwrap();
    assert!(matches!(
        output.roots[0].data,
        mini_go::snapshot::HostData::Bool(true)
    ));
    vm.close().unwrap();
}
