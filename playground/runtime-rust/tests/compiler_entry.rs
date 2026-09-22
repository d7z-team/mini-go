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

#[test]
fn compiler_workload_initializes_and_checks_source() {
    let image = DecodedImage::decode_gzip(
        include_bytes!("../assets/compiler.json.gz"),
        LoadLimits::compiler(),
    )
    .unwrap();
    let program = Arc::new(Program::prepare(image).unwrap());
    let mut vm = Instance::new(
        program,
        ExecutionLimits {
            max_steps: 5_000_000,
            ..ExecutionLimits::compiler()
        },
    )
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(20);
    while vm.poll_initialize(&Cancellation::default(), 4096).unwrap() != PollStatus::Ready {
        assert!(
            Instant::now() < deadline,
            "compiler initialization deadline"
        );
    }
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
        let deadline = Instant::now() + Duration::from_secs(20);
        while vm.poll_steps(4096).unwrap() != PollStatus::Ready {
            assert!(Instant::now() < deadline, "compiler request deadline");
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
