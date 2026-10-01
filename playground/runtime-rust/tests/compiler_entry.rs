use mini_go::{
    ffi::Cancellation,
    instance::debug::StepMode,
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
        LoadLimits::compiler(),
    )
    .unwrap();
    let program = Arc::new(Program::prepare(image).unwrap());
    let mut vm = Instance::new(
        program,
        ExecutionLimits {
            max_steps,
            ..ExecutionLimits::compiler()
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

/// Opt-in measurement of the complete compiler workload at an expanded, isolated
/// resource envelope. It deliberately does not change the production limits.
#[test]
#[ignore = "diagnostic measurement with expanded compiler resource envelope"]
fn measure_rpc_prepare_to_completion() {
    let workloads: serde_json::Value =
        serde_json::from_str(include_str!("../../../testdata/language/workloads.json")).unwrap();
    let source = &workloads
        .as_array()
        .unwrap()
        .iter()
        .find(|workload| workload["Name"] == "rpc")
        .unwrap()["Source"];
    let image = DecodedImage::decode_gzip(
        include_bytes!("../assets/compiler.json.gz"),
        LoadLimits::compiler(),
    )
    .unwrap();
    let program = Arc::new(Program::prepare(image).unwrap());
    let mut limits = ExecutionLimits::compiler();
    limits.max_steps = -1;
    limits.max_heap_bytes = 2 << 30;
    limits.max_objects = 8_000_000;
    limits.max_sequence_elements = 128 << 20;
    limits.max_allocated_bytes = 512 << 30;
    let mut compiler = Instance::new(program, limits).unwrap();
    let started = Instant::now();
    let timeout = Duration::from_secs(7200);
    let cancel = Cancellation::default();
    while compiler.poll_initialize(&cancel, 4096).unwrap() != PollStatus::Ready {
        assert!(
            started.elapsed() < timeout,
            "compiler initialization timeout"
        );
    }
    compiler.start_bytes("default", b"{}").unwrap();
    while compiler.poll_steps(4096).unwrap() != PollStatus::Ready {
        assert!(started.elapsed() < timeout, "compiler envelope timeout");
    }
    let envelope = compiler.snapshot_results(Default::default()).unwrap();
    let envelope: serde_json::Value =
        serde_json::from_slice(&envelope.bytes(&envelope.roots[0]).unwrap()).unwrap();
    let request = serde_json::json!({
        "Format":envelope["Format"], "Version":envelope["Version"],
        "Operation":"prepare", "Root":"sample",
        "Packages":[{"Namespace":"module:sample", "ModulePath":"sample",
            "Files":[{"Path":"main.mgo", "Text":source}]}],
        "EntryPoints":[{"Name":"default", "ModulePath":"sample", "Function":"Main"}]
    });
    let request_start = Instant::now();
    let start_steps = compiler.steps();
    compiler
        .start_bytes("default", &serde_json::to_vec(&request).unwrap())
        .unwrap();
    let mut last_report = 0;
    let sample_frames = std::env::var_os("MINIGO_RPC_SAMPLE_FRAMES").is_some();
    let samples = [
        500_000_000,
        1_000_000_000,
        2_000_000_000,
        3_000_000_000,
        4_000_000_000,
        4_500_000_000,
    ];
    let mut sample = 0;
    loop {
        let status = compiler.poll_steps(4096);
        let steps = compiler.steps() - start_steps;
        if steps / 100_000_000 > last_report {
            last_report = steps / 100_000_000;
            eprintln!(
                "RPC measure progress: {:?}, {steps} steps, heap {:?}",
                request_start.elapsed(),
                compiler.heap_stats()
            );
        }
        let status = status.unwrap_or_else(|error| {
            panic!(
                "RPC measure stopped after {:?}, {steps} steps, heap {:?}: {error}",
                request_start.elapsed(),
                compiler.heap_stats()
            )
        });
        if status == PollStatus::Ready {
            break;
        }
        if sample_frames && sample < samples.len() && steps >= samples[sample] {
            compiler.request_pause();
            assert_eq!(compiler.poll_steps(1).unwrap(), PollStatus::Paused);
            let stack = compiler.debug_stack().unwrap();
            eprintln!(
                "RPC frames at {steps} steps: {:?}",
                stack
                    .iter()
                    .take(16)
                    .map(|frame| (&frame.module, &frame.function, frame.pc))
                    .collect::<Vec<_>>()
            );
            compiler.debug_resume(StepMode::Continue).unwrap();
            sample += 1;
        }
        assert!(
            request_start.elapsed() < timeout,
            "RPC measure timed out after {steps} steps, heap {:?}",
            compiler.heap_stats()
        );
    }
    let snapshot = compiler.snapshot_results(Default::default()).unwrap();
    let response: serde_json::Value =
        serde_json::from_slice(&snapshot.bytes(&snapshot.roots[0]).unwrap()).unwrap();
    eprintln!(
        "RPC measure completed: {:?}, {} request steps, heap {:?}",
        request_start.elapsed(),
        compiler.steps() - start_steps,
        compiler.heap_stats()
    );
    assert_eq!(response["Error"], "", "{response}");
    assert!(response["Image"].is_object(), "missing compiled image");
    compiler.close().unwrap();
}

#[cfg(feature = "rpc")]
#[tokio::test(flavor = "current_thread")]
async fn compiler_prepares_rpc_source_and_executes_owned_host_call() {
    use mini_go::rpc;
    use std::sync::atomic::{AtomicUsize, Ordering};

    let calls = Arc::new(AtomicUsize::new(0));
    let observed = calls.clone();
    let provider = rpc::StaticProvider::new(vec![rpc::MethodBinding {
        method: rpc::Method {
            id: "probe.v1::Echo.Add".into(),
            service: "probe.v1::Echo".into(),
            name: "Add".into(),
            contract_hash: "1".repeat(64),
            resource_type_hash: String::new(),
        },
        invoke: Some(Arc::new(move |_, values| {
            observed.fetch_add(1, Ordering::SeqCst);
            Box::pin(async move {
                assert_eq!(values, vec![rpc::Value::new("int64", rpc::Data::Int(41))]);
                Ok(vec![rpc::Value::new("int64", rpc::Data::Int(42))])
            })
        })),
    }])
    .unwrap();
    let handle = tokio::runtime::Handle::current();
    let mut options = rpc::HostOptions::new(handle.clone());
    options.providers.push(Arc::new(provider));
    let host = Arc::new(rpc::Host::new(options).unwrap());
    let worker = host.clone();
    rpc::VmExecutor::new(handle, 1)
        .unwrap()
        .run(move || {
            let deadline = Instant::now() + Duration::from_secs(600);
            let workloads: serde_json::Value =
                serde_json::from_str(include_str!("../../../testdata/language/workloads.json"))
                    .unwrap();
            let source = &workloads
                .as_array()
                .unwrap()
                .iter()
                .find(|workload| workload["Name"] == "rpc")
                .unwrap()["Source"];
            let mut compiler = compiler_instance(ExecutionLimits::compiler().max_steps, deadline);
            compiler.start_bytes("default", b"{}").unwrap();
            while compiler.poll_steps(4096).unwrap() != PollStatus::Ready {
                assert!(
                    Instant::now() < deadline,
                    "compiler handshake watchdog: {:?}",
                    compiler.stats()
                );
            }
            let envelope = compiler.snapshot_results(Default::default()).unwrap();
            let envelope: serde_json::Value =
                serde_json::from_slice(&envelope.bytes(&envelope.roots[0]).unwrap()).unwrap();
            let request = serde_json::json!({
                "Format": envelope["Format"], "Version": envelope["Version"],
                "Operation":"prepare", "Root":"sample",
                "Packages":[{"Namespace":"module:sample", "ModulePath":"sample",
                    "Files":[{"Path":"main.mgo", "Text":source}]}],
                "EntryPoints":[{"Name":"default", "ModulePath":"sample", "Function":"Main"}]
            });
            let start = Instant::now();
            compiler
                .start_bytes("default", &serde_json::to_vec(&request).unwrap())
                .unwrap();
            // Step/heap limits bound guest work; the watchdog bounds the whole test.
            while compiler.poll_steps(4096).unwrap_or_else(|error| {
                panic!(
                    "RPC prepare failed after {:?}, {} steps: {error}",
                    start.elapsed(),
                    compiler.steps()
                )
            }) != PollStatus::Ready
            {
                assert!(
                    Instant::now() < deadline,
                    "RPC prepare watchdog: {:?}",
                    compiler.stats()
                );
            }
            eprintln!(
                "RPC prepare: {:?}, {} steps",
                start.elapsed(),
                compiler.steps()
            );
            let snapshot = compiler.snapshot_results(Default::default()).unwrap();
            let bytes = snapshot.bytes(&snapshot.roots[0]).unwrap();
            let response: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
            assert_eq!(response["Error"], "");
            assert!(response["Diagnostics"].as_array().is_none_or(Vec::is_empty));
            // Load the exact wire tokens so constant identity retains integer
            // precision and the compiler's canonical string escaping.
            let fields: std::collections::BTreeMap<String, Box<serde_json::value::RawValue>> =
                serde_json::from_slice(&bytes).unwrap();
            assert_eq!(compiler.stats().active_scopes, 0);
            compiler.close().unwrap();
            drop(compiler);
            let program = Arc::new(
                Program::load(fields["Image"].get().as_bytes(), LoadLimits::default()).unwrap(),
            );
            let mut vm =
                Instance::with_bridge(program, ExecutionLimits::default(), worker.as_ref())
                    .unwrap();
            vm.start("default", Vec::new()).unwrap();
            let wake = vm.wake();
            loop {
                assert!(Instant::now() < deadline, "compiled RPC did not settle");
                let epoch = wake.epoch();
                match vm.poll_steps(4096).unwrap() {
                    PollStatus::Ready => break,
                    PollStatus::Pending => wake.wait(epoch, Duration::from_millis(10)),
                    PollStatus::Running => {}
                    PollStatus::Paused => panic!("unexpected debugger pause"),
                }
            }
            assert_eq!(vm.results()[0].integer().unwrap(), 42);
            assert_eq!(vm.stats().active_scopes, 0);
            assert_eq!(vm.stats().pending_ffi_calls, 0);
            assert_eq!(vm.stats().pending_boundary_bytes, 0);
            vm.close().unwrap();
        })
        .await
        .unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    host.shutdown().await.unwrap();
}
