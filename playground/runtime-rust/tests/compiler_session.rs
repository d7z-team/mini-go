use mini_go::compiler::CompilerSession;
use mini_go::ffi::Cancellation;
use serde_json::json;

#[tokio::test]
async fn delivery_confirmation_preserves_only_committed_inputs() {
    use mini_go::compiler::{CompilerPoll, RestoreState, SessionState};
    use std::{
        sync::{
            Arc,
            atomic::{AtomicU64, Ordering},
        },
        time::Duration,
    };
    struct Clock(AtomicU64);
    impl mini_go::environment::Clock for Clock {
        fn unix_time(&self) -> (i64, u32) {
            (2_000_000_000, 0)
        }
        fn monotonic_ns(&self) -> u64 {
            self.0.load(Ordering::Relaxed)
        }
    }
    let image = include_bytes!("../assets/compiler.json.gz");
    for invalid in [
        br#"{"version":2,"input":null}"#.as_slice(),
        br#"{"version":1,"input":{"Operation":"build/prepare"}}"#,
        br#"{"version":1,"input":null,"extra":0}"#,
    ] {
        assert!(RestoreState::decode(invalid).is_err());
    }
    let clock = Arc::new(Clock(AtomicU64::new(0)));
    let mut session =
        CompilerSession::with_clock(image, 1, RestoreState::default(), clock.clone()).unwrap();
    assert_eq!(
        session
            .start(
                json!({"Operation":"hello", "Deadline":"1999999999999999999"}),
                Duration::from_secs(30)
            )
            .unwrap_err()
            .code,
        "deadline"
    );
    assert!(session.stats().is_none());
    let mut workspace: serde_json::Value =
        serde_json::from_str(include_str!("../../../testdata/language/workspace.json")).unwrap();
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
        .call(workspace, &Cancellation::default())
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
        .call(json!({"Operation":"hello"}), &Cancellation::default())
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
            .call(json!({"Operation":"hello"}), &Cancellation::default())
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
}

#[tokio::test]
async fn language_session_reuses_analysis_and_queries_shared_core() {
    let image =
        std::fs::read(std::env::var_os("MINIGO_TOOLS_IMAGE").unwrap_or_else(|| {
            concat!(env!("CARGO_MANIFEST_DIR"), "/assets/compiler.json.gz").into()
        }))
        .unwrap();
    let mut session = CompilerSession::new(&image).await.unwrap();
    let cancel = Cancellation::default();
    let mut workspace: serde_json::Value =
        serde_json::from_str(include_str!("../../../testdata/language/workspace.json")).unwrap();
    workspace["Operation"] = "workspace/open".into();
    {
        use std::future::Future;
        let mut opening = Box::pin(session.call(workspace.clone(), &cancel));
        std::future::poll_fn(|context| {
            assert!(opening.as_mut().poll(context).is_pending());
            std::task::Poll::Ready(())
        })
        .await;
    }
    assert!(session.confirmed_input().is_none());
    session.call(workspace, &cancel).await.unwrap();
    let canceled = Cancellation::default();
    canceled.cancel();
    assert_eq!(
        session
            .call(json!({"Operation":"workspace/analyze"}), &canceled)
            .await
            .unwrap_err()
            .code,
        "canceled"
    );
    let first = session
        .call(json!({"Operation":"workspace/analyze"}), &cancel)
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
            .call(json!({"Operation":operation,"Query":query}), &cancel)
            .await
            .unwrap_or_else(|error| panic!("{operation}: {error}"));
    }
    let hover=session.call(json!({"Operation":"language/hover","Query":{"Snapshot":session.snapshot(),"URI":"mini-go://sample/main.mgo","Position":{"line":2,"character":6}}}),&cancel).await.unwrap();
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
            .call(
                json!({"Operation":"workspace/sources","Trees":failure["Trees"]}),
                &cancel,
            )
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
    let build=session.call(json!({"Operation":"build/prepare","Build":{"Revision":session.revision(),"Symbols":true,"EntryPoints":debug_fixture["EntryPoints"]}}),&cancel).await.unwrap();
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
    let mut stopped = None;
    for _ in 0..1000 {
        debugger.poll().unwrap();
        if let Some(event) = debugger
            .events()
            .unwrap()
            .into_iter()
            .find(|event| event["event"] == "stopped")
        {
            stopped = Some(event);
            break;
        }
    }
    let thread = stopped.expect("breakpoint stop")["body"]["threadId"].clone();
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
    let mut terminated = false;
    for _ in 0..1000 {
        debugger.poll().unwrap();
        if debugger
            .events()
            .unwrap()
            .iter()
            .any(|event| event["event"] == "terminated")
        {
            terminated = true;
            break;
        }
    }
    assert!(terminated, "target termination");
    debugger.close();
    let previous_snapshot = session.snapshot().to_owned();
    assert!(session.upgrade(b"invalid image", &cancel).await.is_err());
    assert_eq!(session.snapshot(), previous_snapshot);
    session.upgrade(&image, &cancel).await.unwrap();
    assert_ne!(session.snapshot(), previous_snapshot);
    let previous_snapshot = session.snapshot().to_owned();
    {
        use std::future::Future;
        let mut interrupted =
            Box::pin(session.call(json!({"Operation":"workspace/analyze"}), &cancel));
        std::future::poll_fn(|context| {
            assert!(interrupted.as_mut().poll(context).is_pending());
            std::task::Poll::Ready(())
        })
        .await;
    }
    session
        .call(json!({"Operation":"workspace/analyze"}), &cancel)
        .await
        .unwrap();
    assert_ne!(session.snapshot(), previous_snapshot);
    let stale=session.call(json!({"Operation":"language/hover","Query":{"Snapshot":previous_snapshot,"URI":"mini-go://sample/main.mgo","Position":{"line":2,"character":6}}}),&cancel).await.unwrap_err();
    assert_eq!(stale.code, "stale");
    for _ in 0..20 {
        session
            .call(json!({"Operation":"workspace/analyze"}), &cancel)
            .await
            .unwrap();
    }
    let stats = session.stats().unwrap();
    assert_eq!(stats.active_scopes, 0);
    assert_eq!(stats.blocked_tasks, 0);
    assert_eq!(stats.runnable_tasks, 0);
    assert_eq!(stats.pending_ffi_calls, 0);
    session.close().await.unwrap();
}
