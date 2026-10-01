//! Fixed workloads; counts are observations, not machine-dependent pass limits.
use mini_go::{
    Executor, InstanceOptions, LoadOptions, Program,
    ffi::Cancellation,
    instance::{ExecutionLimits, Instance, PollStatus},
    snapshot::HostData,
    value::Value,
};
use serde_json::json;
#[cfg(feature = "compiler")]
use std::time::Duration;
use std::{
    alloc::{GlobalAlloc, Layout, System},
    collections::BTreeMap,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::Instant,
};

#[path = "../tests/support/mod.rs"]
mod support;

struct CountingAllocator;
static ALLOCATIONS: AtomicU64 = AtomicU64::new(0);
static BYTES: AtomicU64 = AtomicU64::new(0);
static LIVE_BYTES: AtomicU64 = AtomicU64::new(0);

// This standalone benchmark is single-threaded. The allocator delegates storage
// and layout ownership unchanged to System; only successful requests are counted.
unsafe impl GlobalAlloc for CountingAllocator {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        let pointer = unsafe { System.alloc(layout) };
        if !pointer.is_null() {
            ALLOCATIONS.fetch_add(1, Ordering::Relaxed);
            BYTES.fetch_add(layout.size() as u64, Ordering::Relaxed);
            LIVE_BYTES.fetch_add(layout.size() as u64, Ordering::Relaxed);
        }
        pointer
    }
    unsafe fn dealloc(&self, pointer: *mut u8, layout: Layout) {
        LIVE_BYTES.fetch_sub(layout.size() as u64, Ordering::Relaxed);
        unsafe { System.dealloc(pointer, layout) }
    }
    unsafe fn alloc_zeroed(&self, layout: Layout) -> *mut u8 {
        let pointer = unsafe { System.alloc_zeroed(layout) };
        if !pointer.is_null() {
            ALLOCATIONS.fetch_add(1, Ordering::Relaxed);
            BYTES.fetch_add(layout.size() as u64, Ordering::Relaxed);
            LIVE_BYTES.fetch_add(layout.size() as u64, Ordering::Relaxed);
        }
        pointer
    }
    unsafe fn realloc(&self, pointer: *mut u8, layout: Layout, size: usize) -> *mut u8 {
        let next = unsafe { System.realloc(pointer, layout, size) };
        if !next.is_null() {
            ALLOCATIONS.fetch_add(1, Ordering::Relaxed);
            BYTES.fetch_add(size as u64, Ordering::Relaxed);
            if size >= layout.size() {
                LIVE_BYTES.fetch_add((size - layout.size()) as u64, Ordering::Relaxed);
            } else {
                LIVE_BYTES.fetch_sub((layout.size() - size) as u64, Ordering::Relaxed);
            }
        }
        next
    }
}

#[global_allocator]
static ALLOCATOR: CountingAllocator = CountingAllocator;

fn run(name: &str, image: &[u8], expected: i64, profile: bool) {
    let program = Arc::new(Program::load(image, LoadOptions::default()).unwrap());
    let mut instance = Instance::new(program, ExecutionLimits::default()).unwrap();
    if profile {
        instance.start_profile(4096, 128).unwrap();
    }
    for iteration in 0..2 {
        if iteration == 1 {
            ALLOCATIONS.store(0, Ordering::Relaxed);
            BYTES.store(0, Ordering::Relaxed);
        }
        let before = Instant::now();
        let start_steps = instance.steps();
        let heap = instance.heap_stats();
        // A separate warmed batch excludes loading and initial frame allocation.
        let repetitions = if iteration == 1 { 100 } else { 1 };
        for _ in 0..repetitions {
            instance.start("default", vec![Value::int(1000)]).unwrap();
            let mut budget = 100_000;
            loop {
                let quantum = budget.min(1024);
                assert_ne!(quantum, 0, "bounded benchmark work");
                budget -= quantum;
                match instance.poll_steps(quantum).unwrap() {
                    PollStatus::Running => {}
                    PollStatus::Ready => break,
                    status => panic!("unexpected benchmark state: {status:?}"),
                }
            }
            assert_eq!(instance.results()[0].integer().unwrap(), expected);
        }
        if iteration == 1 {
            let allocations = ALLOCATIONS.load(Ordering::Relaxed);
            let bytes = BYTES.load(Ordering::Relaxed);
            let after = instance.heap_stats();
            let elapsed = before.elapsed().as_secs_f64();
            let steps = instance.steps() - start_steps;
            println!(
                "{}",
                json!({"workload":name,"mode":if profile {"sampled"} else {"normal"},"calls":100,"elapsed_s":elapsed,"steps":steps,"steps_s":steps as f64/elapsed,"allocations":allocations,"allocated_bytes":bytes,"guest_allocated_bytes":after.total_allocated_bytes-heap.total_allocated_bytes,"collections":after.collections-heap.collections})
            );
        }
    }
    if !profile {
        instance.start_profile(1, 100_000).unwrap();
        let before = instance.steps();
        instance.start("default", vec![Value::int(1000)]).unwrap();
        while instance.poll_steps(1024).unwrap() != PollStatus::Ready {}
        assert_eq!(instance.results()[0].integer().unwrap(), expected);
        print_opcodes(name, &instance, before);
    }
    instance.close().unwrap();
}

fn print_opcodes(name: &str, vm: &Instance, before: u64) {
    let profile = vm.profile();
    assert_eq!(profile.dropped, 0);
    let mut counts = BTreeMap::<String, u64>::new();
    let mut functions = BTreeMap::<String, u64>::new();
    for sample in profile.samples {
        *counts.entry(sample.opcode).or_default() += sample.count;
        *functions
            .entry(format!("{}::{}", sample.module, sample.function))
            .or_default() += sample.count;
    }
    assert_eq!(counts.values().sum::<u64>(), vm.steps() - before);
    println!(
        "{}",
        json!({"workload":name,"mode":"exact","steps":vm.steps()-before,"opcodes":counts,"function_steps":functions})
    );
}

#[cfg(feature = "compiler")]
fn compiler_request(
    vm: &mut Instance,
    entry: &str,
    request: &serde_json::Value,
    timeout: Duration,
) -> serde_json::Value {
    vm.start_bytes(entry, &serde_json::to_vec(request).unwrap())
        .unwrap();
    let deadline = Instant::now() + timeout;
    while vm.poll_steps(4096).unwrap() != PollStatus::Ready {
        assert!(Instant::now() < deadline, "compiler request deadline");
    }
    let result = vm.snapshot_results(Default::default()).unwrap();
    let bytes = result.bytes(&result.roots[0]).unwrap();
    let response: serde_json::Value = if entry == "tools" {
        mini_go::compiler::decode_tools_response(&bytes).unwrap()
    } else {
        serde_json::from_slice(&bytes).unwrap()
    };
    assert!(
        response["Error"].is_null() || response["Error"] == "",
        "{response}"
    );
    response
}

#[cfg(feature = "compiler")]
fn compiler_session_workload() {
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    runtime.block_on(async {
        let cancel = Cancellation::default();
        let image = std::env::var_os("MINIGO_BENCH_IMAGE")
            .map(|path| std::fs::read(path).expect("read compiler benchmark image"));
        let mut session = mini_go::compiler::CompilerSession::new(
            image.as_deref().unwrap_or(include_bytes!("../assets/compiler.json.gz"))
        ).await.unwrap();
        let response = session.call(json!({"Operation":"workspace/open","Root":"probe",
            "Packages":[{"Namespace":"module:probe","ModulePath":"probe","Files":[{"Path":"main.mgo","Text":"package main\nfunc Main() int { return 42 }\n"}]}]}), &cancel).await.unwrap();
        assert!(response["Error"].is_null(), "{response}");
        for build in [false, true] {
            let request = json!({"Operation":if build {"build/prepare"} else {"workspace/analyze"},
                "Build":{"EntryPoints":[{"Name":"default","ModulePath":"probe","Function":"Main"}]}});
            for _ in 0..2 { session.call(request.clone(), &cancel).await.unwrap(); }
            let steps = session.stats().unwrap().executed_steps;
            let allocations = ALLOCATIONS.load(Ordering::Relaxed);
            let bytes = BYTES.load(Ordering::Relaxed);
            let started = Instant::now();
            for _ in 0..5 {
                let response = session.call(request.clone(), &cancel).await.unwrap();
                assert!(response["Error"].is_null(), "{response}");
                if build { assert!(!response["ImageJSON"].as_str().unwrap().is_empty()); }
            }
            let elapsed = started.elapsed().as_secs_f64();
            let allocations = ALLOCATIONS.load(Ordering::Relaxed) - allocations;
            let bytes = BYTES.load(Ordering::Relaxed) - bytes;
            let steps = session.stats().unwrap().executed_steps - steps;
            println!("{}", json!({"workload":if build {"compiler-session-build"} else {"compiler-session-analyze"},"mode":"normal","calls":5,"elapsed_s":elapsed,"steps":steps,"steps_s":steps as f64/elapsed,"allocations":allocations,"allocated_bytes":bytes}));
        }
        session.close().await.unwrap();
    });
}

#[cfg(feature = "compiler")]
fn compiler_workloads() {
    use mini_go::loader::{DecodedImage, LoadLimits};
    let external_image = std::env::var_os("MINIGO_BENCH_IMAGE")
        .map(|path| std::fs::read(path).expect("read compiler benchmark image"));
    let live_bytes = LIVE_BYTES.load(Ordering::Relaxed);
    let allocations = ALLOCATIONS.load(Ordering::Relaxed);
    let bytes = BYTES.load(Ordering::Relaxed);
    let started = Instant::now();
    let program = Arc::new(
        Program::prepare(
            DecodedImage::decode_gzip(
                external_image
                    .as_deref()
                    .unwrap_or(include_bytes!("../assets/compiler.json.gz")),
                LoadLimits::compiler(),
            )
            .unwrap(),
        )
        .unwrap(),
    );
    let elapsed = started.elapsed().as_secs_f64();
    let allocations = ALLOCATIONS.load(Ordering::Relaxed) - allocations;
    let bytes = BYTES.load(Ordering::Relaxed) - bytes;
    let retained_bytes = LIVE_BYTES.load(Ordering::Relaxed) - live_bytes;
    println!(
        "{}",
        json!({"workload":"compiler-load", "mode":"load", "elapsed_s":elapsed,
        "allocations":allocations,"allocated_bytes":bytes,"retained_bytes":retained_bytes,"steps":null,"steps_s":null,"opcodes":null})
    );
    println!(
        "{}",
        json!({"mode":"configuration","compiler_id":program.image().compiler_id,"image_hash":program.image().hash})
    );
    if std::env::var_os("MINIGO_BENCH_IDENTITY").is_some() {
        return;
    }
    if std::env::var_os("MINIGO_BENCH_COLD").is_some() {
        let workload =
            std::env::var("MINIGO_BENCH_COLD_WORKLOAD").unwrap_or_else(|_| "declarations".into());
        let prepare = workload == "rpc-prepare";
        let source = match workload.as_str() {
            "declarations" => {
                let mut source = String::from("package main\nfunc Main() int { return F99(1) }\n");
                for index in 0..100 {
                    source.push_str(&format!(
                        "func F{index}(x int) int {{ y := x + {index}; return y }}\n"
                    ));
                }
                source
            }
            "unicode" => "package sample\nimport \"unicode\"\nfunc Main() bool { return unicode.IsLetter('界') }\n".into(),
            name => {
                let workloads: serde_json::Value = serde_json::from_str(include_str!(
                    "../../../testdata/language/workloads.json"
                )).unwrap();
                workloads.as_array().unwrap().iter()
                    .find(|item| item["Name"] == if name == "rpc-prepare" { "rpc" } else { name })
                    .unwrap_or_else(|| panic!("unknown cold benchmark workload {name}"))["Source"]
                    .as_str().unwrap().to_owned()
            }
        };
        let packages = json!([{"Namespace":"module:probe","ModulePath":"probe","Files":[{"Path":"main.mgo","Text":source}]}]);
        let request = if prepare {
            json!({"Format":"mini-go-compiler-service","Version":11,"Operation":"prepare","Root":"probe",
                "Packages":packages,"EntryPoints":[{"Name":"default","ModulePath":"probe","Function":"Main"}]})
        } else {
            json!({"Format":"mini-go-tools","Version":3,"Operation":"workspace/open","Root":"probe","Packages":packages})
        };
        let name = if workload == "rpc-prepare" {
            "compiler-default-cold-rpc-prepare".to_owned()
        } else {
            format!("compiler-tools-cold-{workload}")
        };
        let timing_only = std::env::var_os("MINIGO_BENCH_TIMING_ONLY").is_some();
        for profile in [false, true] {
            if profile && timing_only {
                break;
            }
            let mut vm = Instance::new(program.clone(), ExecutionLimits::compiler()).unwrap();
            if profile {
                vm.start_profile(1, 100_000).unwrap();
            }
            let before = vm.steps();
            let allocations = ALLOCATIONS.load(Ordering::Relaxed);
            let bytes = BYTES.load(Ordering::Relaxed);
            let started = Instant::now();
            // A watchdog bounds a stalled benchmark independently of its
            // performance result. Deterministic guest limits remain unchanged.
            let seconds = if workload == "declarations" { 20 } else { 120 };
            let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                compiler_request(
                    &mut vm,
                    if prepare { "default" } else { "tools" },
                    &request,
                    Duration::from_secs(seconds),
                )
            }));
            let elapsed = started.elapsed().as_secs_f64();
            let steps = vm.steps() - before;
            if profile {
                print_opcodes(&name, &vm, before);
            } else {
                let heap = vm.heap_stats();
                println!(
                    "{}",
                    json!({"workload":name,"mode":if outcome.is_ok() { "normal" } else { "failure" },"calls":1,
                    "elapsed_s":elapsed,"steps":steps,"steps_s":steps as f64/elapsed,
                    "allocations":ALLOCATIONS.load(Ordering::Relaxed)-allocations,
                    "allocated_bytes":BYTES.load(Ordering::Relaxed)-bytes,
                    "heap_live_bytes":heap.live_bytes,"heap_peak_bytes":heap.peak_bytes})
                );
            }
            vm.close().unwrap();
            if let Err(error) = outcome {
                std::panic::resume_unwind(error);
            }
        }
        return;
    }
    let packages = json!([{"Namespace":"module:probe","ModulePath":"probe","Files":[{"Path":"main.mgo","Text":"package main\nfunc Main() int { return 42 }\n"}]}]);
    for entry in ["default", "tools"] {
        let mut vm = Instance::new(program.clone(), ExecutionLimits::compiler()).unwrap();
        let (session, revision) = if entry == "tools" {
            let response = compiler_request(
                &mut vm,
                entry,
                &json!({"Format":"mini-go-tools","Version":3,"Operation":"workspace/open","Root":"probe","Packages":packages}),
                Duration::from_secs(20),
            );
            (response["Session"].clone(), response["Revision"].clone())
        } else {
            (json!(""), json!(""))
        };
        for (build, symbols) in [(false, false), (true, false), (true, true)] {
            if symbols && entry != "tools" {
                continue;
            }
            let request = if entry == "tools" {
                json!({"Format":"mini-go-tools","Version":3,"Operation":if build {"build/prepare"} else {"workspace/analyze"},"Session":session,"Revision":revision,"Build":{"Revision":revision,"Symbols":symbols,"EntryPoints":[{"Name":"default","ModulePath":"probe","Function":"Main"}]}})
            } else {
                json!({"Format":"mini-go-compiler-service","Version":11,"Operation":if build {"prepare"} else {"check"},"Root":"probe","Packages":packages,"EntryPoints":[{"Name":"default","ModulePath":"probe","Function":"Main"}]})
            };
            let name = format!(
                "compiler-{entry}-{}",
                if symbols {
                    "build-symbols"
                } else if build {
                    "build"
                } else {
                    "analyze"
                }
            );
            for _ in 0..2 {
                compiler_request(&mut vm, entry, &request, Duration::from_secs(20));
            }
            let start_steps = vm.steps();
            let allocations = ALLOCATIONS.load(Ordering::Relaxed);
            let bytes = BYTES.load(Ordering::Relaxed);
            let started = Instant::now();
            for _ in 0..5 {
                let response = compiler_request(&mut vm, entry, &request, Duration::from_secs(20));
                if symbols {
                    assert!(!response["SymbolsJSON"].as_str().unwrap().is_empty());
                }
                if build {
                    assert!(if entry == "tools" {
                        !response["ImageJSON"].as_str().unwrap().is_empty()
                    } else {
                        !response["Image"].is_null()
                    });
                }
            }
            let elapsed = started.elapsed().as_secs_f64();
            let steps = vm.steps() - start_steps;
            println!(
                "{}",
                json!({"workload":name,"mode":"normal","calls":5,"elapsed_s":elapsed,"steps":steps,"steps_s":steps as f64/elapsed,"allocations":ALLOCATIONS.load(Ordering::Relaxed)-allocations,"allocated_bytes":BYTES.load(Ordering::Relaxed)-bytes})
            );
            vm.start_profile(1, 100_000).unwrap();
            let before = vm.steps();
            compiler_request(&mut vm, entry, &request, Duration::from_secs(20));
            print_opcodes(&name, &vm, before);
            vm.start_profile(0, 0).unwrap();
        }
        vm.close().unwrap();
    }
}

fn main() {
    if std::env::var_os("MINIGO_BENCH_COMPILER").is_some()
        || std::env::var_os("MINIGO_BENCH_COLD").is_some()
    {
        #[cfg(feature = "compiler")]
        {
            compiler_workloads();
            if std::env::var_os("MINIGO_BENCH_COLD").is_none()
                && std::env::var_os("MINIGO_BENCH_IDENTITY").is_none()
            {
                compiler_session_workload();
            }
            return;
        }
        #[cfg(not(feature = "compiler"))]
        panic!("compiler measurements require --features compiler");
    }
    let contract: serde_json::Value =
        serde_json::from_str(mini_go::contract_generated::CONTRACT_JSON).unwrap();
    let operation = |name: &str, descriptor, operands| {
        let opcode = contract["spec"]["opcodes"]
            .as_array()
            .unwrap()
            .iter()
            .position(|entry| entry["op"] == name)
            .unwrap()
            + 1;
        json!([opcode, descriptor, operands])
    };
    let integer = json!({"kind":3,"primitive":3});
    let mut instructions = Vec::new();
    for _ in 0..1000 {
        instructions.extend([
            operation("load_local", 0, 0),
            operation("store_local", 0, 1),
        ]);
    }
    instructions.extend([operation("load_local", 0, 0), operation("return", 0, 1)]);
    let scalar = support::image(json!({"functions":[{
        "id":"fn.Main",
        "signature":{"params":[{"type":{"kind":3,"primitive":3}}],"results":[{"kind":3,"primitive":3}]},
        "locals":[{"id":"n","type":{"kind":3,"primitive":3}}],
        "code":{"types":[integer], "instructions":instructions,
            "descriptors":{"local":[{"local":"n"}],"return":[{"result_count":1}]},
            "operands":[{"outputs":[0]},{"inputs":[[0,0]],"release":[0]}]}
    }]}));
    run("scalar-slots", &scalar, 1000, false);
    let program = Arc::new(Program::load(&scalar, LoadOptions::default()).unwrap());
    let executor = Executor::new(1).unwrap();
    let shared_instance = program
        .instantiate(InstanceOptions {
            parallelism: 1,
            executor: Some(executor.clone()),
            ..Default::default()
        })
        .unwrap();
    let cancel = Cancellation::default();
    for _ in 0..10 {
        let execution = shared_instance
            .start("default", vec![Value::int(1000)])
            .unwrap();
        execution.wait(&cancel).unwrap();
    }
    let before_steps = shared_instance.stats().executed_steps;
    ALLOCATIONS.store(0, Ordering::Relaxed);
    BYTES.store(0, Ordering::Relaxed);
    let started = Instant::now();
    for _ in 0..1000 {
        let execution = shared_instance
            .start("default", vec![Value::int(1000)])
            .unwrap();
        let result = execution.wait(&cancel).unwrap();
        assert!(matches!(result.roots[0].data, HostData::Integer(1000)));
    }
    let elapsed = started.elapsed().as_secs_f64();
    let allocations = ALLOCATIONS.load(Ordering::Relaxed);
    let bytes = BYTES.load(Ordering::Relaxed);
    let steps = shared_instance.stats().executed_steps - before_steps;
    println!(
        "{}",
        json!({"workload":"shared-scalar-slots","mode":"normal","calls":1000,"elapsed_s":elapsed,"steps":steps,"steps_s":steps as f64/elapsed,"allocations":allocations,"allocated_bytes":bytes})
    );
    shared_instance.shutdown(&cancel).unwrap();
    executor.shutdown(&cancel).unwrap();
    run("sparse-profile", &scalar, 1000, true);
    let mut calls = Vec::new();
    for _ in 0..1000 {
        calls.extend([
            operation("call_direct", 0, 0),
            operation("store_local", 0, 1),
        ]);
    }
    calls.push(operation("return", 0, 2));
    let tiny_artifact = json!({"functions":[
        {"id":"fn.Main","signature":{"params":[{"type":integer}],"results":[integer]},"locals":[{"id":"n","type":integer}],
            "code":{"types":[integer],"instructions":calls,
                "descriptors":{"call":[{"function":"Id","arg_count":1,"result_count":1}],"local":[{"local":"n"}],"return":[{"result_count":1}]},
                "operands":[{"inputs":[[2,0]],"outputs":[0]},{"inputs":[[0,0]],"release":[0]},{"inputs":[[2,0]]}]}},
        {"id":"Id","signature":{"params":[{"type":integer}],"results":[integer]},"locals":[{"id":"n","type":integer}],
            "code":{"instructions":[operation("return",0,0)],"descriptors":{"return":[{"result_count":1}]},"operands":[{"inputs":[[2,0]]}]}}
    ]});
    run(
        "tiny-call",
        &support::image(tiny_artifact.clone()),
        1000,
        false,
    );
    for indirect in [false, true] {
        let mut artifact = tiny_artifact.clone();
        if indirect {
            artifact["type_table"] = json!({"nodes":[{"id":"callable","kind":mini_go::contract_generated::Function,"signature":{"params":[{"type":integer}],"results":[integer]}}]});
            let code = &mut artifact["functions"][0]["code"];
            code["types"]
                .as_array_mut()
                .unwrap()
                .push(json!({"kind":mini_go::contract_generated::Function,"node":"callable"}));
            code["descriptors"]["closure"] = json!([{"function":"Id"}]);
            code["operands"][0]["inputs"] = json!([[0, 1], [2, 0]]);
            code["operands"][2]["release"] = json!([1]);
            code["operands"]
                .as_array_mut()
                .unwrap()
                .push(json!({"outputs":[1]}));
            let body = code["instructions"].as_array_mut().unwrap();
            for instruction in body.iter_mut().step_by(2).take(1000) {
                *instruction = operation("call_value", 0, 0);
            }
            body.insert(0, operation("make_closure", 0, 3));
        } else {
            let mut target = artifact["functions"][1].clone();
            target["id"] = json!("TailTarget");
            artifact["functions"].as_array_mut().unwrap().push(target);
            artifact["functions"][1]["code"] = json!({
                "types":[integer],
                "instructions":[operation("load_local",0,0),operation("tail_call_direct",0,1)],
                "descriptors":{"local":[{"local":"n"}],"call":[{"function":"TailTarget","arg_count":1,"result_count":1}]},
                "operands":[{"outputs":[0]},{"inputs":[[0,0]],"release":[0]}]
            });
        }
        run(
            if indirect {
                "tiny-indirect-call"
            } else {
                "tiny-tail-call"
            },
            &support::image(artifact),
            1000,
            false,
        );
    }
    for fused in [false, true] {
        let mut body = vec![operation("label", 0, 0), operation("binary", 0, 1)];
        if !fused {
            body.push(operation("store_local", 0, 2));
        }
        body.extend([operation("compare_branch", 0, 3), operation("return", 0, 4)]);
        let image = support::image(
            json!({"constants":[{"id":"one","type":integer,"value":1},{"id":"zero","type":integer,"value":0}],"functions":[{
                "id":"fn.Main","signature":{"params":[{"type":integer}],"results":[integer]},"locals":[{"id":"n","type":integer}],
                "code":{"types":if fused {vec![]} else {vec![integer.clone()]},"instructions":body,
                    "descriptors":{"label":[{"label":"loop"}],"operator":[{"operator":"-"}],"local":[{"local":"n"}],"comparebranch":[{"operator":">","type":integer,"label":"loop","when":true}],"return":[{"result_count":1}]},
                    "operands":[{}, {"inputs":[[2,0],[1,0]],"outputs":[if fused {1u32 << 31} else {0}]},
                        if fused {json!({})} else {json!({"inputs":[[0,0]],"release":[0]})},
                        {"inputs":[[2,0],[1,1]]},{"inputs":[[2,0]]}]}
            }]}),
        );
        run(
            if fused {
                "local-output-fused"
            } else {
                "local-output-store"
            },
            &image,
            0,
            false,
        );
    }
    run(
        "arithmetic",
        include_bytes!("../examples/blocks/arithmetic.json"),
        500500,
        false,
    );
    run(
        "closure",
        include_bytes!("../examples/blocks/closure.json"),
        2003,
        false,
    );
}
