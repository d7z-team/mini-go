#![cfg(feature = "stdlib-host")]
use mini_go::{
    ffi::Bridge,
    rpc::*,
    stdlib_host::{
        self as host,
        console_binding::*,
        filesystem::{Filesystem, FilesystemProvider},
        memory::MemoryFilesystem,
        os_binding::*,
    },
};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
};

#[derive(serde::Deserialize)]
struct Scenarios {
    version: u64,
    environment: EnvironmentScenario,
    filesystem: Vec<FileAction>,
    vm: VmScenario,
}
#[derive(serde::Deserialize)]
struct EnvironmentScenario {
    entries: Vec<String>,
    lookups: Vec<Lookup>,
}
#[derive(serde::Deserialize)]
struct Lookup {
    key: String,
    value: String,
    found: bool,
}
#[derive(serde::Deserialize, Default)]
#[serde(default)]
struct FileAction {
    id: String,
    operation: String,
    data: String,
    code: String,
    size: i64,
    offset: i64,
    whence: i64,
    position: i64,
    nil: bool,
}
#[derive(serde::Deserialize)]
struct VmScenario {
    environment: Vec<String>,
    result: i64,
    stdout: String,
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn shared_host_scenarios_and_precompiled_vm() {
    let scenario: Scenarios = serde_json::from_slice(include_bytes!(
        "../../../testdata/stdlib-host/scenarios.json"
    ))
    .unwrap();
    assert_eq!(scenario.version, 1);
    let pool = host::BlockingPool::new(tokio::runtime::Handle::current(), 8).unwrap();
    let filesystem =
        MemoryFilesystem::new(BTreeMap::from([("input".into(), b"abc".to_vec())])).unwrap();
    let binder = LocalBinder::new(
        tokio::runtime::Handle::current(),
        Limits::default(),
        vec![
            FilesystemProvider {
                backend: filesystem,
                pool: pool.clone(),
            }
            .provider()
            .unwrap(),
            host::environment::Environment::snapshot(scenario.environment.entries)
                .provider()
                .unwrap(),
        ],
    )
    .unwrap();
    let client = OsFilesystemClient::bind(CallContext::default(), &binder, BindOptions::default())
        .await
        .unwrap();
    let (file, error) = client
        .open(CallContext::default(), "input".into(), 2, 0)
        .await
        .unwrap();
    assert!(error.code.is_empty());
    let file = file.unwrap();
    for action in scenario.filesystem {
        match action.operation.as_str() {
            "read" | "read_at" => {
                let (data, fault) = if action.operation == "read" {
                    file.read(CallContext::default(), action.size).await
                } else {
                    file.read_at(CallContext::default(), action.size, action.offset)
                        .await
                }
                .unwrap();
                assert_eq!(fault.code, action.code, "{}", action.id);
                assert_eq!(data.is_none(), action.nil, "{}", action.id);
                assert_eq!(
                    data.unwrap_or_default(),
                    action.data.as_bytes(),
                    "{}",
                    action.id
                );
            }
            "write_at" | "seek" => {
                let (position, fault) = if action.operation == "seek" {
                    file.seek(CallContext::default(), action.offset, action.whence)
                        .await
                } else {
                    file.write_at(
                        CallContext::default(),
                        Some(action.data.into_bytes()),
                        action.offset,
                    )
                    .await
                }
                .unwrap();
                assert_eq!(fault.code, action.code, "{}", action.id);
                assert_eq!(position, action.position, "{}", action.id);
            }
            other => panic!("unknown operation {other}"),
        }
    }
    file.close(CallContext::default()).await.unwrap();
    client.close().await.unwrap();
    let environment =
        OsEnvironmentClient::bind(CallContext::default(), &binder, BindOptions::default())
            .await
            .unwrap();
    for lookup in scenario.environment.lookups {
        assert_eq!(
            environment
                .lookup(CallContext::default(), lookup.key)
                .await
                .unwrap(),
            (lookup.value, lookup.found)
        );
    }
    environment.close().await.unwrap();
    pool.shutdown().await.unwrap();

    let output = Arc::new(Mutex::new(Vec::new()));
    let host = host::HostBuilder::memory(
        tokio::runtime::Handle::current(),
        host::MemoryHostOptions {
            environment: scenario.vm.environment,
            stdout: Some(output.clone()),
            ..Default::default()
        },
    )
    .unwrap()
    .build()
    .await
    .unwrap();
    let worker = host.clone();
    let result = VmExecutor::new(tokio::runtime::Handle::current(), 1)
        .unwrap()
        .run(move || {
            use mini_go::{
                instance::{ExecutionLimits, Instance, PollStatus},
                loader::LoadLimits,
                program::Program,
            };
            use std::{
                io::Read,
                time::{Duration, Instant},
            };
            let mut bytes = Vec::new();
            flate2::read::GzDecoder::new(
                &include_bytes!("../../../testdata/stdlib-host/images/call.json.gz")[..],
            )
            .read_to_end(&mut bytes)
            .unwrap();
            let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
            let mut instance = Instance::with_bridge(
                program,
                ExecutionLimits {
                    max_steps: 5_000_000,
                    ..Default::default()
                },
                worker.as_ref(),
            )
            .unwrap();
            instance.start("default", &[]).unwrap();
            let wake = instance.wake();
            let deadline = Instant::now() + Duration::from_secs(15);
            let value = loop {
                assert!(Instant::now() < deadline, "host image did not settle");
                let epoch = wake.epoch();
                match instance.poll_steps(4096).unwrap() {
                    PollStatus::Ready => {
                        break instance.snapshot_results(Default::default()).unwrap().roots[0]
                            .integer()
                            .unwrap();
                    }
                    PollStatus::Pending => wake.wait(epoch, Duration::from_millis(10)),
                    PollStatus::Running => {}
                    PollStatus::Paused => panic!("unexpected debugger pause"),
                }
            };
            instance.close().unwrap();
            value
        })
        .await
        .unwrap();
    host.shutdown().await.unwrap();
    assert_eq!(result, scenario.vm.result);
    assert_eq!(*output.lock().unwrap(), scenario.vm.stdout.as_bytes());
    assert_eq!(host.stats(), HostStats::default());
}

#[tokio::test(flavor = "current_thread")]
async fn blocking_owner_survives_waiter_cancellation_and_shutdown_waits_for_io() {
    let pool = host::BlockingPool::new(tokio::runtime::Handle::current(), 1).unwrap();
    let (entered, started) = tokio::sync::oneshot::channel();
    let (release, released) = std::sync::mpsc::channel();
    let owner = pool.clone();
    let task = tokio::spawn(async move {
        owner
            .run(move || {
                let _ = entered.send(());
                released.recv().unwrap();
            })
            .await
    });
    started.await.unwrap();
    task.abort();
    let _ = task.await;
    assert_eq!(
        pool.run(|| ()).await.unwrap_err().code,
        "resource_exhausted"
    );
    assert!(
        tokio::time::timeout(std::time::Duration::from_millis(10), pool.shutdown())
            .await
            .is_err()
    );
    release.send(()).unwrap();
    pool.shutdown().await.unwrap();
    assert_eq!(pool.run(|| ()).await.unwrap_err().code, "unavailable");
}

#[tokio::test(flavor = "current_thread")]
async fn typed_filesystem_preserves_faults_cursors_and_nil_resources() {
    let pool = host::BlockingPool::new(tokio::runtime::Handle::current(), 8).unwrap();
    let filesystem =
        MemoryFilesystem::new(BTreeMap::from([("input".into(), b"abc".to_vec())])).unwrap();
    let provider = FilesystemProvider {
        backend: filesystem.clone(),
        pool: pool.clone(),
    }
    .provider()
    .unwrap();
    let binder = LocalBinder::new(
        tokio::runtime::Handle::current(),
        Limits::default(),
        vec![provider],
    )
    .unwrap();
    let client = OsFilesystemClient::bind(CallContext::default(), &binder, BindOptions::default())
        .await
        .unwrap();
    let (missing, error) = client
        .open(CallContext::default(), "missing".into(), 0, 0)
        .await
        .unwrap();
    assert!(missing.is_none());
    assert_eq!(error.code, "not_exist");
    let (file, error) = client
        .open(CallContext::default(), "input".into(), 2, 0)
        .await
        .unwrap();
    assert!(error.code.is_empty());
    let file = file.unwrap();
    let (data, error) = file.read_at(CallContext::default(), 5, 1).await.unwrap();
    assert_eq!(data, Some(b"bc".to_vec()));
    assert_eq!(error.code, "eof");
    assert_eq!(file.seek(CallContext::default(), 0, 1).await.unwrap().0, 0);
    assert_eq!(
        file.read(CallContext::default(), 5).await.unwrap().0,
        Some(b"abc".to_vec())
    );
    let (data, error) = file.read(CallContext::default(), 1).await.unwrap();
    assert_eq!(data, Some(vec![]));
    assert!(error.code.is_empty());
    assert_eq!(
        file.write_at(CallContext::default(), Some(b"z".to_vec()), 1)
            .await
            .unwrap()
            .0,
        1
    );
    client.close().await.unwrap();
    assert_eq!(file.stat(CallContext::default()).await.unwrap().0.size, 3);
    file.close(CallContext::default()).await.unwrap();
    pool.shutdown().await.unwrap();
    assert_eq!(filesystem.stat("input", true).unwrap().size, 3);
}

#[tokio::test(flavor = "current_thread")]
async fn console_and_environment_use_caller_owned_inputs() {
    let pool = host::BlockingPool::new(tokio::runtime::Handle::current(), 4).unwrap();
    let output = Arc::new(Mutex::new(Vec::<u8>::new()));
    let console = Arc::new(host::console::Streams {
        input: Some(Arc::new(host::console::Reader(Mutex::new(
            std::io::Cursor::new(b"abc".to_vec()),
        )))),
        stdout: Some(output.clone()),
        stderr: None,
        pool: pool.clone(),
    })
    .provider()
    .unwrap();
    let environment = host::environment::Environment::snapshot([
        "A=old".into(),
        "A=new=tail".into(),
        "EMPTY=".into(),
        "ignored".into(),
    ])
    .provider()
    .unwrap();
    let binder = LocalBinder::new(
        tokio::runtime::Handle::current(),
        Limits::default(),
        vec![console, environment],
    )
    .unwrap();
    let console = FmtConsoleClient::bind(CallContext::default(), &binder, BindOptions::default())
        .await
        .unwrap();
    assert_eq!(
        console.read(CallContext::default(), 0).await.unwrap().0,
        Some(vec![])
    );
    assert_eq!(
        console.read(CallContext::default(), 9).await.unwrap().0,
        Some(b"abc".to_vec())
    );
    assert_eq!(
        console
            .read(CallContext::default(), 9)
            .await
            .unwrap()
            .1
            .code,
        "eof"
    );
    assert_eq!(
        console
            .write(CallContext::default(), 1, Some(b"hello".to_vec()))
            .await
            .unwrap()
            .0,
        5
    );
    console.close().await.unwrap();
    let environment =
        OsEnvironmentClient::bind(CallContext::default(), &binder, BindOptions::default())
            .await
            .unwrap();
    assert_eq!(
        environment
            .lookup(CallContext::default(), "A".into())
            .await
            .unwrap(),
        ("new=tail".into(), true)
    );
    assert_eq!(
        environment
            .lookup(CallContext::default(), "EMPTY".into())
            .await
            .unwrap(),
        ("".into(), true)
    );
    assert_eq!(
        environment
            .lookup(CallContext::default(), "missing".into())
            .await
            .unwrap(),
        ("".into(), false)
    );
    environment.close().await.unwrap();
    pool.shutdown().await.unwrap();
    assert_eq!(*output.lock().unwrap(), b"hello");
}

struct PanickingContract;
impl Provider for PanickingContract {
    fn contract(&self) -> Contract {
        panic!("injected contract panic")
    }
    fn bind(
        &self,
        _: CallContext,
        _: BindRequest,
    ) -> BoxFuture<'_, Result<Arc<dyn ProviderLease>>> {
        Box::pin(async { Err(Status::new("internal", "invalid provider")) })
    }
}

#[tokio::test(flavor = "current_thread")]
async fn host_owns_cleanup_on_success_and_constructor_failure() {
    for scenario in ["valid", "missing", "panic"] {
        let order = Arc::new(Mutex::new(Vec::new()));
        let mut builder = host::HostBuilder::new(tokio::runtime::Handle::current());
        builder.required.push(
            if scenario == "missing" {
                "missing"
            } else {
                "environment"
            }
            .into(),
        );
        let seen = order.clone();
        builder.providers.push(host::Provider {
            capability: "environment".into(),
            rpc: if scenario == "panic" {
                Arc::new(PanickingContract)
            } else {
                host::environment::Environment::snapshot([])
                    .provider()
                    .unwrap()
            },
            close: Some(Box::new(move || {
                Box::pin(async move {
                    seen.lock().unwrap().push(1);
                    Ok(())
                })
            })),
        });
        let pool = host::BlockingPool::new(tokio::runtime::Handle::current(), 1).unwrap();
        let seen = order.clone();
        builder.providers.push(host::Provider {
            capability: "filesystem".into(),
            rpc: FilesystemProvider {
                backend: MemoryFilesystem::new(BTreeMap::new()).unwrap(),
                pool: pool.clone(),
            }
            .provider()
            .unwrap(),
            close: Some(Box::new(move || {
                Box::pin(async move {
                    pool.shutdown().await?;
                    seen.lock().unwrap().push(2);
                    Ok(())
                })
            })),
        });
        let result = builder.build().await;
        if scenario != "valid" {
            assert!(result.is_err());
        } else {
            let host = result.unwrap();
            assert_eq!(host.capabilities(), ["environment", "filesystem"]);
            host.shutdown().await.unwrap();
            host.shutdown().await.unwrap();
            assert_eq!(host.stats(), HostStats::default());
        }
        assert_eq!(*order.lock().unwrap(), [2, 1]);
    }
}

#[tokio::test(flavor = "current_thread")]
async fn cleanup_failure_is_retained_and_other_owned_backends_are_closed() {
    let closed = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let owner = closed.clone();
    let mut builder = host::HostBuilder::new(tokio::runtime::Handle::current());
    builder.providers.push(host::Provider {
        capability: "environment".into(),
        rpc: host::environment::Environment::snapshot([])
            .provider()
            .unwrap(),
        close: Some(Box::new(move || {
            Box::pin(async move {
                owner.store(true, std::sync::atomic::Ordering::Release);
                Ok(())
            })
        })),
    });
    let pool = host::BlockingPool::new(tokio::runtime::Handle::current(), 1).unwrap();
    builder.providers.push(host::Provider {
        capability: "filesystem".into(),
        rpc: FilesystemProvider {
            backend: MemoryFilesystem::new(BTreeMap::new()).unwrap(),
            pool,
        }
        .provider()
        .unwrap(),
        close: Some(Box::new(|| {
            Box::pin(async { panic!("injected cleanup panic") })
        })),
    });
    let host = builder.build().await.unwrap();
    let error = host.shutdown().await.unwrap_err();
    assert_eq!(error.code, "internal");
    assert_eq!(host.shutdown().await.unwrap_err(), error);
    assert!(closed.load(std::sync::atomic::Ordering::Acquire));
}

struct PartialInput(bool);
impl host::console::Input for PartialInput {
    fn read(
        &self,
        _: &CallContext,
        buffer: &mut [u8],
    ) -> (usize, Option<host::console::InputFault>) {
        buffer[0] = b'x';
        (
            1,
            Some(if self.0 {
                host::console::InputFault::Eof
            } else {
                host::console::InputFault::Io(std::io::Error::other("partial read"))
            }),
        )
    }
}
#[tokio::test(flavor = "current_thread")]
async fn console_preserves_partial_data_with_eof_and_io_fault() {
    for eof in [false, true] {
        let pool = host::BlockingPool::new(tokio::runtime::Handle::current(), 1).unwrap();
        let provider = Arc::new(host::console::Streams {
            input: Some(Arc::new(PartialInput(eof))),
            stdout: None,
            stderr: None,
            pool: pool.clone(),
        })
        .provider()
        .unwrap();
        let binder = LocalBinder::new(
            tokio::runtime::Handle::current(),
            Limits::default(),
            vec![provider],
        )
        .unwrap();
        let client =
            FmtConsoleClient::bind(CallContext::default(), &binder, BindOptions::default())
                .await
                .unwrap();
        let (data, fault) = client.read(CallContext::default(), 2).await.unwrap();
        assert_eq!(data, Some(b"x".to_vec()));
        assert_eq!(fault.code, if eof { "" } else { "io" });
        client.close().await.unwrap();
        pool.shutdown().await.unwrap();
    }
}
