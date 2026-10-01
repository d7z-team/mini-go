use mini_go::compiler::CompilerSession;
use mini_go::ffi::Cancellation;
use serde_json::json;
use std::time::Duration;

const REQUEST_TIMEOUT: Duration = Duration::from_secs(300);
const TEST_TIMEOUT: Duration = Duration::from_secs(600);

#[tokio::test]
async fn host_load_budget_is_preserved_by_upgrade() {
    use mini_go::compiler::CompilerOptions;
    tokio::time::timeout(TEST_TIMEOUT, async {
        let image = include_bytes!("../assets/compiler.json.gz");
        let mut options = CompilerOptions::default();
        // Check loading separately from execution, then preserve a custom
        // image envelope and step budget across a successful upgrade.
        options.load.max_packages = 1;
        assert!(CompilerSession::new_with_options(image, options, REQUEST_TIMEOUT).await.is_err());
        options = CompilerOptions::default();
        options.load.max_image_bytes = 60 << 20;
        options.limits.max_steps = 5_000_000;
        let mut session = CompilerSession::new_with_options(image, options, REQUEST_TIMEOUT).await.unwrap();
        let cancel = Cancellation::default();
        let generation = session.generation();
        let oversized = vec![0; options.load.max_image_bytes + 1];
        assert_eq!(session.upgrade_with_timeout(&oversized, &cancel, REQUEST_TIMEOUT).await.unwrap_err().code, "load_limit");
        assert_eq!(session.generation(), generation);
        drop(oversized);
        session.upgrade_with_timeout(image, &cancel, REQUEST_TIMEOUT).await.unwrap();
        let outcome = async {
            session.call_with_timeout(json!({
            "Operation":"workspace/open", "Root":"sample", "Packages":[{
                "Namespace":"module:sample", "ModulePath":"sample", "Files":[{
                    "Path":"main.mgo", "Text":"package sample\nimport \"unicode\"\nfunc Main() bool { return unicode.IsLetter('中') }\n"
                }]
            }]
            }), &cancel, REQUEST_TIMEOUT).await?;
            session.call_with_timeout(json!({"Operation":"workspace/analyze"}), &cancel, REQUEST_TIMEOUT).await
        }.await;
        assert_eq!(outcome.unwrap_err().code, "step_limit");
        session.close().await.unwrap();
    }).await.expect("compiler budget test watchdog");
}

#[derive(Default)]
struct TestClock(std::sync::atomic::AtomicU64);

impl mini_go::environment::Clock for TestClock {
    fn unix_time(&self) -> (i64, u32) {
        let elapsed = self.monotonic_ns();
        (
            1_700_000_000 + (elapsed / 1_000_000_000) as i64,
            (elapsed % 1_000_000_000) as u32,
        )
    }
    fn monotonic_ns(&self) -> u64 {
        self.0.load(std::sync::atomic::Ordering::Relaxed)
    }
}

macro_rules! language_workload_test {
    ($test:ident, $workload:literal) => {
        #[tokio::test]
        async fn $test() {
            tokio::time::timeout(TEST_TIMEOUT, async {
                assert_warm_analysis($workload).await;
            })
            .await
            .expect("compiler integration test watchdog");
        }
    };
}

#[tokio::test]
async fn compiler_deadlines_share_the_guest_clock_and_preserve_explicit_budgets() {
    use mini_go::compiler::{CompilerPoll, RestoreState};
    use std::sync::{Arc, atomic::Ordering};
    tokio::time::timeout(TEST_TIMEOUT, async {
        let image = include_bytes!("../assets/compiler.json.gz");
        let clock = Arc::new(TestClock::default());
        let mut session = CompilerSession::with_clock(
            image,
            1,
            RestoreState::default(),
            Default::default(),
            clock.clone(),
        )
        .unwrap();
        for (timeout, request, code) in [
            (
                Duration::MAX,
                json!({"Operation":"hello"}),
                "invalid_argument",
            ),
            (Duration::ZERO, json!({"Operation":"hello"}), "deadline"),
            (
                REQUEST_TIMEOUT,
                json!({"Deadline":"9223372036854775808"}),
                "invalid_argument",
            ),
            (
                Duration::from_secs(10_000_000_000),
                json!({}),
                "invalid_argument",
            ),
        ] {
            assert_eq!(session.start(request, timeout).unwrap_err().code, code);
            assert!(session.stats().is_none());
        }
        for generation in 1..=2 {
            if generation > 1 {
                session.set_generation(generation).unwrap();
            }
            session
                .start(json!({"Operation":"hello"}), REQUEST_TIMEOUT)
                .unwrap();
            // Advance during initialization. Both the owner and the guest must honor
            // the longer budget, even though this clock predates the real wall clock.
            clock.0.fetch_add(60_000_000_000, Ordering::Relaxed);
            loop {
                if let CompilerPoll::Ready(_) = session.poll(4096).unwrap() {
                    break;
                }
                tokio::task::yield_now().await;
            }
            session.acknowledge().unwrap();
            session.abandon();
        }
        session.set_generation(3).unwrap();
        let deadline =
            (1_700_000_000_000_000_000u64 + clock.0.load(Ordering::Relaxed) + 1_000_000_000)
                .to_string();
        session
            .start(
                json!({"Operation":"hello", "Deadline":deadline}),
                REQUEST_TIMEOUT,
            )
            .unwrap();
        clock.0.fetch_add(1_000_000_000, Ordering::Relaxed);
        assert_eq!(session.poll(1).unwrap_err().code, "deadline");
        assert!(session.stats().is_none());
        session.set_generation(4).unwrap();
        session
            .start(json!({"Operation":"hello"}), REQUEST_TIMEOUT)
            .unwrap();
        clock.0.fetch_add(300_000_000_000, Ordering::Relaxed);
        assert_eq!(session.poll(1).unwrap_err().code, "deadline");
        // Exercise the default async entry point without waiting for real time.
        session.set_generation(5).unwrap();
        let cancel = Cancellation::default();
        let mut default_call = Box::pin(session.call(json!({"Operation":"hello"}), &cancel));
        std::future::poll_fn(|cx| {
            use std::future::Future;
            assert!(default_call.as_mut().poll(cx).is_pending());
            std::task::Poll::Ready(())
        })
        .await;
        clock.0.fetch_add(30_000_000_000, Ordering::Relaxed);
        assert_eq!(default_call.await.unwrap_err().code, "deadline");
        let generation = session.generation();
        for (timeout, code) in [
            (Duration::ZERO, "deadline"),
            (Duration::MAX, "invalid_argument"),
        ] {
            assert_eq!(
                session
                    .upgrade_with_timeout(image, &cancel, timeout)
                    .await
                    .err()
                    .unwrap()
                    .code,
                code
            );
            assert_eq!(session.generation(), generation);
        }
        session
            .upgrade_with_timeout(image, &cancel, REQUEST_TIMEOUT)
            .await
            .unwrap();
        session
            .start(json!({"Operation":"hello"}), Duration::from_secs(1))
            .unwrap();
        clock.0.fetch_add(1_000_000_000, Ordering::Relaxed);
        assert_eq!(session.poll(1).unwrap_err().code, "deadline");
        session.close().await.unwrap();
        assert_eq!(
            CompilerSession::new_with_timeout(image, Duration::ZERO)
                .await
                .err()
                .unwrap()
                .code,
            "deadline"
        );
        assert_eq!(
            CompilerSession::new_with_timeout(image, Duration::MAX)
                .await
                .err()
                .unwrap()
                .code,
            "invalid_argument"
        );
    })
    .await
    .expect("compiler deadline test watchdog");
}

language_workload_test!(pure_preserves_warm_analysis, "pure");
language_workload_test!(
    generic_methods_preserve_analysis_through_edit_and_build,
    "generic-methods"
);
language_workload_test!(typed_view_preserves_warm_analysis, "typed-view");
language_workload_test!(errors_preserves_warm_analysis, "errors");
language_workload_test!(ffi_preserves_warm_analysis, "ffi");
language_workload_test!(rpc_preserves_analysis_through_edit_and_build, "rpc");
language_workload_test!(ffi_view_preserves_warm_analysis, "ffi-view");

#[tokio::test]
async fn compiler_preserves_binary_literals_through_image_generation() {
    tokio::time::timeout(TEST_TIMEOUT, async {
    use mini_go::{
        instance::{ExecutionLimits, Instance, PollStatus},
        loader::LoadLimits,
        program::Program,
    };
    use std::sync::Arc;

    let mut session = CompilerSession::new_with_timeout(include_bytes!("../assets/compiler.json.gz"), REQUEST_TIMEOUT)
        .await
        .unwrap();
    let cancel = Cancellation::default();
    let source = r#"package sample
type Label string
const raw = "\000\200\xff"
func Main() int {
    text := Label(raw + "\u0080")
    if len(text) != 5 || text[0] != 0 || text[1] != 128 || text[2] != 255 || text[3] != 194 || text[4] != 128 { return -1 }
    return 42
}"#;
    let opened = session
        .call_with_timeout(
            json!({"Operation":"workspace/open", "Root":"sample",
        "Packages":[{"Namespace":"module:sample", "ModulePath":"sample",
            "Files":[{"Path":"main.mgo", "Text":source}]}]}),
            &cancel, REQUEST_TIMEOUT)
        .await
        .unwrap();
    assert!(opened["Error"].is_null(), "{opened}");
    let built = session.call_with_timeout(json!({"Operation":"build/prepare", "Build":{
        "Revision":session.revision(), "EntryPoints":[{"Name":"default", "ModulePath":"sample", "Function":"Main"}]}}), &cancel, REQUEST_TIMEOUT).await.unwrap();
    assert!(built["Error"].is_null(), "{built}");
    let program = Arc::new(
        Program::load(
            built["ImageJSON"].as_str().unwrap().as_bytes(),
            LoadLimits::default(),
        )
        .unwrap(),
    );
    session.close().await.unwrap();
    let mut vm = Instance::new(program, ExecutionLimits::default()).unwrap();
    vm.start("default", Vec::new()).unwrap();
    while vm.poll_steps(4096).unwrap() != PollStatus::Ready {}
    assert_eq!(vm.results()[0].integer().unwrap(), 42);
    vm.close().unwrap();
    }).await.expect("compiler integration test watchdog");
}

async fn assert_warm_analysis(name: &str) {
    let workloads: serde_json::Value =
        serde_json::from_str(include_str!("../../../testdata/language/workloads.json")).unwrap();
    let workload = workloads
        .as_array()
        .unwrap()
        .iter()
        .find(|workload| workload["Name"] == name)
        .unwrap_or_else(|| panic!("missing shared workload {name}"));
    eprintln!("compiler workload {}", workload["Name"]);
    let mut session = CompilerSession::new_with_timeout(
        include_bytes!("../assets/compiler.json.gz"),
        REQUEST_TIMEOUT,
    )
    .await
    .unwrap();
    let result = session
        .call_with_timeout(
            json!({"Operation":"workspace/open", "Root":"sample",
            "Packages":[{"Namespace":"module:sample", "ModulePath":"sample", "Files":[
                {"Path":"main.mgo", "Text":workload["Source"]}]}]}),
            &Cancellation::default(),
            REQUEST_TIMEOUT,
        )
        .await
        .unwrap_or_else(|error| panic!("{}: {error}", workload["Name"]));
    assert!(result["Error"].is_null(), "{}: {result}", workload["Name"]);
    eprintln!("{} open: {:?}", workload["Name"], session.stats());
    let first = session
        .call_with_timeout(
            json!({"Operation":"workspace/analyze"}),
            &Cancellation::default(),
            REQUEST_TIMEOUT,
        )
        .await
        .unwrap();
    assert!(first["Error"].is_null(), "{first}");
    for report in first["Analysis"]["Diagnostics"]
        .as_object()
        .unwrap()
        .values()
    {
        assert!(
            report["items"].as_array().is_none_or(Vec::is_empty),
            "{report}"
        );
    }
    let second = session
        .call_with_timeout(
            json!({"Operation":"workspace/analyze"}),
            &Cancellation::default(),
            REQUEST_TIMEOUT,
        )
        .await
        .unwrap();
    assert_eq!(
        first["Analysis"]["Snapshot"],
        second["Analysis"]["Snapshot"]
    );
    if name == "generic-methods" {
        let (line, text) = workload["Source"]
            .as_str()
            .unwrap()
            .lines()
            .enumerate()
            .find(|(_, text)| text.contains("X: 42"))
            .unwrap();
        let character = text[..text.find("X: 42").unwrap()].encode_utf16().count();
        let query = json!({"Snapshot":session.snapshot(), "URI":"mini-go://sample/main.mgo", "Position":{"line":line,"character":character}});
        let definitions = session
            .call_with_timeout(
                json!({"Operation":"language/definition","Query":query}),
                &Cancellation::default(),
                REQUEST_TIMEOUT,
            )
            .await
            .unwrap();
        assert_eq!(definitions["Value"][0]["range"]["start"]["line"], 1);
        let hover = session
            .call_with_timeout(
                json!({"Operation":"language/hover","Query":query}),
                &Cancellation::default(),
                REQUEST_TIMEOUT,
            )
            .await
            .unwrap();
        assert!(
            hover["Value"]["contents"]["value"]
                .as_str()
                .unwrap()
                .contains("X Int")
        );
    }
    if matches!(name, "rpc" | "generic-methods") {
        let cancel = Cancellation::default();
        let uri = "mini-go://sample/main.mgo";
        let (before, after) = if name == "rpc" {
            ("Signed:41", "Signed:40")
        } else {
            ("X: 42", "X: 41")
        };
        for (version, operation, text) in [
            (1, "open", workload["Source"].as_str().unwrap().to_owned()),
            (
                2,
                "change",
                workload["Source"].as_str().unwrap().replace(before, after),
            ),
        ] {
            let change = if operation == "open" {
                json!({"Operation":operation,"Identity":{"URI":uri,"ModulePath":"sample","Path":"main.mgo"},"Version":version,"Text":text})
            } else {
                json!({"Operation":operation,"Identity":{"URI":uri},"Version":version,"Changes":[{"text":text}]})
            };
            session
                .call_with_timeout(
                    json!({"Operation":"document/update","Changes":[change]}),
                    &cancel,
                    REQUEST_TIMEOUT,
                )
                .await
                .unwrap();
        }
        let edited = session
            .call_with_timeout(
                json!({"Operation":"workspace/analyze"}),
                &cancel,
                REQUEST_TIMEOUT,
            )
            .await
            .unwrap();
        assert_ne!(
            first["Analysis"]["Snapshot"],
            edited["Analysis"]["Snapshot"]
        );
        let started = std::time::Instant::now();
        let built = session
            .call_with_timeout(
                json!({"Operation":"build/prepare","Build":{
                    "Revision":session.revision(),"Symbols":true,
                    "EntryPoints":[{"Name":"default","ModulePath":"sample","Function":"Main"}]
                }}),
                &cancel,
                REQUEST_TIMEOUT,
            )
            .await
            .unwrap_or_else(|error| {
                panic!(
                    "{name} edited build after {:?}: {error}; {:?}",
                    started.elapsed(),
                    session.stats()
                )
            });
        assert!(built["Error"].is_null(), "{built}");
        let image = built["ImageJSON"].as_str().unwrap();
        let symbols = built["SymbolsJSON"].as_str().unwrap();
        assert!(!image.is_empty() && !symbols.is_empty());
        let program =
            mini_go::program::Program::load(image.as_bytes(), Default::default()).unwrap();
        let program = program
            .with_symbols(serde_json::from_str(symbols).unwrap())
            .unwrap();
        assert!(!program.image().hash.is_empty());
        if name == "generic-methods" {
            use mini_go::instance::{ExecutionLimits, Instance, PollStatus};
            let mut instance =
                Instance::new(std::sync::Arc::new(program), ExecutionLimits::default()).unwrap();
            while instance.poll_initialize(&cancel, 1024).unwrap() != PollStatus::Ready {}
            instance.start("default", vec![]).unwrap();
            while instance.poll_steps(1024).unwrap() != PollStatus::Ready {}
            assert_eq!(instance.results()[0].integer().unwrap(), 41);
            instance.close().unwrap();
        }
        let after = session
            .call_with_timeout(
                json!({"Operation":"workspace/analyze"}),
                &cancel,
                REQUEST_TIMEOUT,
            )
            .await
            .unwrap();
        assert_eq!(
            after["Analysis"]["Snapshot"],
            edited["Analysis"]["Snapshot"]
        );
        eprintln!(
            "{name} edited build: {:?}; {:?}",
            started.elapsed(),
            session.stats()
        );
    }
    session.close().await.unwrap();
}

#[tokio::test]
async fn delivery_confirmation_preserves_only_committed_inputs() {
    tokio::time::timeout(TEST_TIMEOUT, async {
        use mini_go::compiler::{CompilerPoll, RestoreState, SessionState};
        use std::{
            sync::{Arc, atomic::Ordering},
            time::Duration,
        };
        let image = include_bytes!("../assets/compiler.json.gz");
        for invalid in [
            br#"{"version":2,"input":null}"#.as_slice(),
            br#"{"version":1,"input":{"Operation":"build/prepare"}}"#,
            br#"{"version":1,"input":null,"extra":0}"#,
        ] {
            assert!(RestoreState::decode(invalid).is_err());
        }
        let clock = Arc::new(TestClock::default());
        let mut session = CompilerSession::with_clock(
            image,
            1,
            RestoreState::default(),
            Default::default(),
            clock.clone(),
        )
        .unwrap();
        assert_eq!(
            session
                .start(
                    json!({"Operation":"hello", "Deadline":"1699999999999999999"}),
                    Duration::from_secs(30)
                )
                .unwrap_err()
                .code,
            "deadline"
        );
        assert!(session.stats().is_none());
        let mut workspace: serde_json::Value =
            serde_json::from_str(include_str!("../../../testdata/language/workspace.json"))
                .unwrap();
        workspace["Operation"] = "workspace/open".into();
        session
            .start(workspace.clone(), Duration::from_secs(30))
            .unwrap();
        loop {
            if let CompilerPoll::Ready(reply) = session.poll(4096).unwrap() {
                assert!(reply.value["Recovery"].is_object());
                break;
            }
            tokio::task::yield_now().await;
        }
        assert_eq!(session.state(), SessionState::AwaitingConfirmation);
        assert_eq!(
            session
                .start(json!({"Operation":"hello"}), Duration::from_secs(1))
                .unwrap_err()
                .code,
            "busy"
        );
        assert!(session.confirmed_input().is_none());
        session.cancel();
        assert!(session.stats().is_none());
        session.set_generation(2).unwrap();
        session
            .call_with_timeout(workspace, &Cancellation::default(), REQUEST_TIMEOUT)
            .await
            .unwrap();
        let confirmed = session.restore_state().encode().unwrap();
        let saved = session.restore_state().clone();
        let mut detached = session.confirmed_input().unwrap();
        detached["Root"] = "caller-owned".into();
        assert_eq!(session.restore_state().encode().unwrap(), confirmed);
        assert!(RestoreState::decode(&confirmed).unwrap().encode().is_ok());
        session
            .start(
                json!({"Operation":"workspace/analyze"}),
                Duration::from_millis(1),
            )
            .unwrap();
        clock.0.store(1_000_000, Ordering::Relaxed);
        assert_eq!(session.poll(4096).unwrap_err().code, "deadline");
        assert_eq!(session.restore_state().encode().unwrap(), confirmed);
        session
            .call_with_timeout(
                json!({"Operation":"hello"}),
                &Cancellation::default(),
                REQUEST_TIMEOUT,
            )
            .await
            .unwrap();
        session
            .start(
                json!({"Operation":"workspace/analyze"}),
                Duration::from_secs(30),
            )
            .unwrap();
        assert!(matches!(
            session.poll(1).unwrap(),
            CompilerPoll::Running | CompilerPoll::Pending
        ));
        session.cancel();
        clock.0.fetch_add(2_000_000_000, Ordering::Relaxed);
        assert_eq!(session.poll(1).unwrap_err().code, "canceled");
        assert!(session.stats().is_none());
        assert_eq!(session.restore_state().encode().unwrap(), confirmed);
        session.set_generation(u64::MAX).unwrap();
        session.abandon();
        assert_eq!(
            session
                .call_with_timeout(
                    json!({"Operation":"hello"}),
                    &Cancellation::default(),
                    REQUEST_TIMEOUT
                )
                .await
                .unwrap_err()
                .code,
            "budget"
        );
        session.close().await.unwrap();
        assert!(session.confirmed_input().is_none());
        assert_eq!(saved.encode().unwrap(), confirmed);
        assert_eq!(
            session
                .start(json!({"Operation":"hello"}), Duration::from_secs(1))
                .unwrap_err()
                .code,
            "closed"
        );
    })
    .await
    .expect("compiler integration test watchdog");
}

#[tokio::test]
async fn language_session_reuses_analysis_and_queries_shared_core() {
    tokio::time::timeout(TEST_TIMEOUT, async {
    let image =
        std::fs::read(std::env::var_os("MINIGO_TOOLS_IMAGE").unwrap_or_else(|| {
            concat!(env!("CARGO_MANIFEST_DIR"), "/assets/compiler.json.gz").into()
        }))
        .unwrap();
    let mut session = CompilerSession::new_with_timeout(&image, REQUEST_TIMEOUT).await.unwrap();
    let cancel = Cancellation::default();
    let mut workspace: serde_json::Value =
        serde_json::from_str(include_str!("../../../testdata/language/workspace.json")).unwrap();
    workspace["Operation"] = "workspace/open".into();
    {
        use std::future::Future;
        let mut opening = Box::pin(session.call_with_timeout(workspace.clone(), &cancel, REQUEST_TIMEOUT));
        std::future::poll_fn(|context| {
            assert!(opening.as_mut().poll(context).is_pending());
            std::task::Poll::Ready(())
        })
        .await;
    }
    assert!(session.confirmed_input().is_none());
    session.call_with_timeout(workspace, &cancel, REQUEST_TIMEOUT).await.unwrap();
    let canceled = Cancellation::default();
    canceled.cancel();
    assert_eq!(
        session
            .call_with_timeout(json!({"Operation":"workspace/analyze"}), &canceled, REQUEST_TIMEOUT)
            .await
            .unwrap_err()
            .code,
        "canceled"
    );
    let first = session
        .call_with_timeout(json!({"Operation":"workspace/analyze"}), &cancel, REQUEST_TIMEOUT)
        .await
        .unwrap();
    assert_eq!(first["Analysis"]["Revision"], "1");
    let queries: Vec<serde_json::Value> =
        serde_json::from_str(include_str!("../../../testdata/language/queries.json")).unwrap();
    for mut query in queries {
        let operation = format!("language/{}", query["Operation"].as_str().unwrap());
        query["URI"] = "mini-go://sample/main.mgo".into();
        query["Snapshot"] = session.snapshot().into();
        session
            .call_with_timeout(json!({"Operation":operation,"Query":query}), &cancel, REQUEST_TIMEOUT)
            .await
            .unwrap_or_else(|error| panic!("{operation}: {error}"));
    }
    let hover=session.call_with_timeout(json!({"Operation":"language/hover","Query":{"Snapshot":session.snapshot(),"URI":"mini-go://sample/main.mgo","Position":{"line":2,"character":6}}}),&cancel, REQUEST_TIMEOUT).await.unwrap();
    assert!(
        hover["Value"]["contents"]["value"]
            .as_str()
            .unwrap()
            .contains("Answer")
    );
    let fixture: serde_json::Value =
        serde_json::from_str(include_str!("../../../testdata/workspace/sources.json")).unwrap();
    let trees =
        serde_json::from_value::<Vec<mini_go::language::SourceTree>>(fixture["Trees"].clone())
            .unwrap();
    let mut language = mini_go::language::LanguageService { session };
    let assembled = language.sources(&trees, &cancel).await.unwrap();
    session = language.session;
    let paths: Vec<_> = assembled
        .packages
        .iter()
        .map(|p| p.module_path.as_str())
        .collect();
    assert_eq!(serde_json::json!(paths), fixture["Paths"]);
    assert_eq!(
        assembled.packages[1]
            .resources
            .as_ref()
            .unwrap()
            .iter()
            .find(|r| r.path == "assets/data.bin")
            .unwrap()
            .data
            .as_deref(),
        Some("AP8B")
    );
    for failure in fixture["Failures"].as_array().unwrap() {
        let error = session
            .call_with_timeout(
                json!({"Operation":"workspace/sources","Trees":failure["Trees"]}),
                &cancel, REQUEST_TIMEOUT)
            .await
            .unwrap_err();
        assert!(
            error
                .to_string()
                .contains(failure["Error"].as_str().unwrap()),
            "{error}"
        );
    }
    let debug_fixture: serde_json::Value =
        serde_json::from_str(include_str!("../../../testdata/debug/breakpoint.json")).unwrap();
    let build=session.call_with_timeout(json!({"Operation":"build/prepare","Build":{"Revision":session.revision(),"Symbols":true,"EntryPoints":debug_fixture["EntryPoints"]}}),&cancel, REQUEST_TIMEOUT).await.unwrap();
    assert!(
        build["Diagnostics"]
            .as_array()
            .is_none_or(|items| items.is_empty()),
        "{build}"
    );
    let mut debugger = mini_go::dap::DebugSession::default();
    debugger
        .launch(
            build["ImageJSON"].as_str().unwrap().as_bytes(),
            build["SymbolsJSON"].as_str(),
            serde_json::from_value(build["Sources"].clone()).unwrap(),
            "default",
        )
        .unwrap();
    let breakpoints = debugger
        .request(
            "setBreakpoints",
            &json!({"source":{"path":debug_fixture["Source"]},"breakpoints":[{"line":debug_fixture["Line"]}]}),
        )
        .unwrap();
    assert_eq!(breakpoints["breakpoints"][0]["verified"], true);
    debugger.request("configurationDone", &json!({})).unwrap();
    let deadline = std::time::Instant::now() + TEST_TIMEOUT;
    let stopped = loop {
        assert!(std::time::Instant::now() < deadline, "debug event wait expired");
        tokio::task::yield_now().await;
        debugger.poll().unwrap();
        if let Some(event) = debugger
            .events()
            .unwrap()
            .into_iter()
            .find(|event| event["event"] == "stopped")
        {
            break event;
        }
    };
    let thread = stopped["body"]["threadId"].clone();
    let stack = debugger
        .request("stackTrace", &json!({"threadId":thread}))
        .unwrap();
    assert!(!stack["stackFrames"].as_array().unwrap().is_empty());
    let source = debugger
        .request(
            "source",
            &json!({"sourceReference":stack["stackFrames"][0]["source"]["sourceReference"]}),
        )
        .unwrap();
    assert!(source["content"].as_str().unwrap().contains("Answer"));
    debugger
        .request("continue", &json!({"threadId":thread}))
        .unwrap();
    let deadline = std::time::Instant::now() + TEST_TIMEOUT;
    loop {
        assert!(std::time::Instant::now() < deadline, "debug event wait expired");
        tokio::task::yield_now().await;
        debugger.poll().unwrap();
        if debugger
            .events()
            .unwrap()
            .iter()
            .any(|event| event["event"] == "terminated")
        {
            break;
        }
    }
    debugger.close();
    let previous_snapshot = session.snapshot().to_owned();
    assert!(session.upgrade_with_timeout(b"invalid image", &cancel, REQUEST_TIMEOUT).await.is_err());
    assert_eq!(session.snapshot(), previous_snapshot);
    session.upgrade_with_timeout(&image, &cancel, REQUEST_TIMEOUT).await.unwrap();
    assert_ne!(session.snapshot(), previous_snapshot);
    let previous_snapshot = session.snapshot().to_owned();
    {
        use std::future::Future;
        let mut interrupted =
            Box::pin(session.call_with_timeout(json!({"Operation":"workspace/analyze"}), &cancel, REQUEST_TIMEOUT));
        std::future::poll_fn(|context| {
            assert!(interrupted.as_mut().poll(context).is_pending());
            std::task::Poll::Ready(())
        })
        .await;
    }
    session
        .call_with_timeout(json!({"Operation":"workspace/analyze"}), &cancel, REQUEST_TIMEOUT)
        .await
        .unwrap();
    assert_ne!(session.snapshot(), previous_snapshot);
    let stale=session.call_with_timeout(json!({"Operation":"language/hover","Query":{"Snapshot":previous_snapshot,"URI":"mini-go://sample/main.mgo","Position":{"line":2,"character":6}}}),&cancel, REQUEST_TIMEOUT).await.unwrap_err();
    assert_eq!(stale.code, "stale");
    for _ in 0..20 {
        session
            .call_with_timeout(json!({"Operation":"workspace/analyze"}), &cancel, REQUEST_TIMEOUT)
            .await
            .unwrap();
    }
    let stats = session.stats().unwrap();
    assert_eq!(stats.active_scopes, 0);
    assert_eq!(stats.blocked_tasks, 0);
    assert_eq!(stats.runnable_tasks, 0);
    assert_eq!(stats.pending_ffi_calls, 0);
    session.close().await.unwrap();
    }).await.expect("compiler integration test watchdog");
}
