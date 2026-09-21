#![cfg(feature = "rpc")]
use mini_go::{ffi::Cancellation, rpc::*};
use std::sync::Arc;
use tokio::sync::{Mutex, mpsc};

#[path = "support/connection.rs"]
mod connection;
use connection::Pipe;

struct ExpiringResource(Arc<tokio::sync::Semaphore>);

struct SlowCloseResource(Arc<std::sync::atomic::AtomicUsize>);

struct AdmissionFilter {
    pipe: Pipe,
    assembly: std::sync::Mutex<protocol::Assembly>,
    suppress: Arc<std::sync::atomic::AtomicBool>,
    written: Option<Arc<std::sync::atomic::AtomicBool>>,
}
impl MessageConn for AdmissionFilter {
    fn read(&self) -> BoxFuture<'_, Result<Vec<u8>>> {
        self.pipe.read()
    }
    fn write(&self, fragment: Vec<u8>) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            let message = self
                .assembly
                .lock()
                .unwrap()
                .push(&fragment, &Limits::default())?;
            if let Some(message) = message
                && protocol::Frame::decode(&message, &Limits::default())?.kind == protocol::ACCEPTED
            {
                if self.suppress.load(std::sync::atomic::Ordering::SeqCst) {
                    return Ok(());
                }
                if let Some(written) = &self.written {
                    // Expose the interval between queue admission and write completion.
                    tokio::task::yield_now().await;
                    self.pipe.write(fragment).await?;
                    written.store(true, std::sync::atomic::Ordering::SeqCst);
                    return Ok(());
                }
            }
            self.pipe.write(fragment).await
        })
    }
    fn close(&self) {
        self.pipe.close();
    }
}
#[tokio::test(flavor = "current_thread")]
async fn handler_waits_for_admission_write_completion() {
    use std::sync::atomic::{AtomicBool, Ordering};
    let runtime = tokio::runtime::Handle::current();
    let written = Arc::new(AtomicBool::new(false));
    let observed = written.clone();
    let method = Method {
        id: "fixture.Service.value".into(),
        service: "fixture.Service".into(),
        name: "value".into(),
        contract_hash: "a".repeat(64),
        resource_type_hash: String::new(),
    };
    let provider = Arc::new(
        StaticProvider::new(vec![MethodBinding {
            method: method.clone(),
            invoke: Some(Arc::new(move |_, _| {
                let admitted = observed.load(Ordering::SeqCst);
                Box::pin(async move {
                    if !admitted {
                        return Err(Status::new("internal", "handler overtook admission write"));
                    }
                    Ok(vec![])
                })
            })),
        }])
        .unwrap(),
    );
    let binder = Arc::new(
        LocalBinder::new(runtime.clone(), Limits::default(), vec![provider.clone()]).unwrap(),
    );
    let (a, ar) = mpsc::channel(4);
    let (b, br) = mpsc::channel(4);
    let closed = Cancellation::default();
    let left = Endpoint::open(
        runtime.clone(),
        Arc::new(Pipe {
            send: a,
            receive: Mutex::new(br),
            closed: closed.clone(),
        }),
        None,
        EndpointOptions::default(),
    )
    .unwrap();
    let right = Endpoint::open(
        runtime,
        Arc::new(AdmissionFilter {
            pipe: Pipe {
                send: b,
                receive: Mutex::new(ar),
                closed,
            },
            assembly: Default::default(),
            suppress: Arc::new(AtomicBool::new(false)),
            written: Some(written.clone()),
        }),
        Some(binder),
        EndpointOptions::default(),
    )
    .unwrap();
    let routes = left
        .bind(
            CallContext::default(),
            BindRequest::new(provider.contract()),
        )
        .await
        .unwrap();
    written.store(false, Ordering::SeqCst);
    let result = routes
        .invoke(
            CallContext::default(),
            Call {
                method,
                receiver: None,
                arguments: vec![],
            },
        )
        .await;
    // Always release the connection owners, including a regression failure.
    let outcome = match result {
        Ok(result) => result.discard().await,
        Err(error) => Err(error),
    };
    routes.shutdown().await.unwrap();
    left.shutdown().await.unwrap();
    right.shutdown().await.unwrap();
    outcome.unwrap();
}

#[tokio::test(flavor = "current_thread")]
async fn admitted_calls_preserve_caller_deadline_and_cancellation_status() {
    let runtime = tokio::runtime::Handle::current();
    let method = Method {
        id: "fixture.Wait.Call".into(),
        service: "fixture.Wait".into(),
        name: "Call".into(),
        contract_hash: "a".repeat(64),
        resource_type_hash: String::new(),
    };
    let (entered, mut entries) = mpsc::unbounded_channel();
    let provider = Arc::new(
        StaticProvider::new(vec![MethodBinding {
            method: method.clone(),
            invoke: Some(Arc::new(move |context, _| {
                let entered = entered.clone();
                Box::pin(async move {
                    let _ = entered.send(());
                    context.cancellation.cancelled().await;
                    Err(Status::new("canceled", "stopped"))
                })
            })),
        }])
        .unwrap(),
    );
    let binder = Arc::new(
        LocalBinder::new(runtime.clone(), Limits::default(), vec![provider.clone()]).unwrap(),
    );
    let (a, ar) = mpsc::channel(4);
    let (b, br) = mpsc::channel(4);
    let closed = Cancellation::default();
    let options = EndpointOptions {
        lease_ttl: std::time::Duration::from_millis(200),
        admission_timeout: std::time::Duration::from_secs(1),
        ..Default::default()
    };
    let left = Endpoint::open(
        runtime.clone(),
        Arc::new(Pipe {
            send: a,
            receive: Mutex::new(br),
            closed: closed.clone(),
        }),
        None,
        options.clone(),
    )
    .unwrap();
    let right = Endpoint::open(
        runtime,
        Arc::new(Pipe {
            send: b,
            receive: Mutex::new(ar),
            closed,
        }),
        Some(binder),
        options,
    )
    .unwrap();
    let routes = left
        .bind(
            CallContext::default(),
            BindRequest::new(provider.contract()),
        )
        .await
        .unwrap();

    let owner = routes.clone();
    let deadline_method = method.clone();
    let deadline = tokio::spawn(async move {
        owner
            .invoke(
                CallContext::with_deadline(
                    web_time::Instant::now() + std::time::Duration::from_millis(700),
                ),
                Call {
                    method: deadline_method,
                    receiver: None,
                    arguments: vec![],
                },
            )
            .await
    });
    tokio::time::timeout(std::time::Duration::from_secs(1), entries.recv())
        .await
        .unwrap()
        .unwrap();
    let result = tokio::time::timeout(std::time::Duration::from_secs(3), deadline)
        .await
        .unwrap()
        .unwrap();
    let error = match result {
        Ok(_) => panic!("deadline call completed"),
        Err(error) => error,
    };
    assert_eq!(error.code, "deadline_exceeded");

    let cancellation = Cancellation::default();
    let owner = routes.clone();
    let canceled = cancellation.clone();
    let canceled_call = tokio::spawn(async move {
        owner
            .invoke(
                CallContext::with_cancellation(canceled),
                Call {
                    method,
                    receiver: None,
                    arguments: vec![],
                },
            )
            .await
    });
    tokio::time::timeout(std::time::Duration::from_secs(1), entries.recv())
        .await
        .unwrap()
        .unwrap();
    cancellation.cancel();
    let result = tokio::time::timeout(std::time::Duration::from_secs(2), canceled_call)
        .await
        .unwrap()
        .unwrap();
    let error = match result {
        Ok(_) => panic!("canceled call completed"),
        Err(error) => error,
    };
    assert_eq!(error.code, "canceled");

    left.ping(CallContext::default()).await.unwrap();
    routes.shutdown().await.unwrap();
    left.shutdown().await.unwrap();
    right.shutdown().await.unwrap();
}

impl Resource for SlowCloseResource {
    fn invoke(
        &self,
        _: CallContext,
        _: String,
        _: Vec<Value>,
    ) -> BoxFuture<'_, Result<Vec<Value>>> {
        Box::pin(async { Ok(vec![]) })
    }
    fn close(&self) -> BoxFuture<'_, Result<()>> {
        Box::pin(async {
            tokio::time::sleep(std::time::Duration::from_millis(900)).await;
            self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            Ok(())
        })
    }
}

#[tokio::test(flavor = "current_thread")]
async fn admission_bounds_waiting_and_renewal_keeps_adopted_operations_alive() {
    let runtime = tokio::runtime::Handle::current();
    let closed_resources = Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let owner = closed_resources.clone();
    let method = Method {
        id: "fixture.Slow.open".into(),
        service: "fixture.Slow".into(),
        name: "open".into(),
        contract_hash: "a".repeat(64),
        resource_type_hash: String::new(),
    };
    let provider = Arc::new(
        StaticProvider::new(vec![MethodBinding {
            method: method.clone(),
            invoke: Some(Arc::new(move |context, _| {
                let owner = owner.clone();
                Box::pin(async move {
                    tokio::time::sleep(std::time::Duration::from_millis(900)).await;
                    Ok(vec![context.export(
                        Arc::new(SlowCloseResource(owner)),
                        "b".repeat(64),
                    )?])
                })
            })),
        }])
        .unwrap(),
    );
    let binder = Arc::new(
        LocalBinder::new(runtime.clone(), Limits::default(), vec![provider.clone()]).unwrap(),
    );
    let (a, ar) = mpsc::channel(4);
    let (b, br) = mpsc::channel(4);
    let closed = Cancellation::default();
    let suppress_admission = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let left = Endpoint::open(
        runtime.clone(),
        Arc::new(Pipe {
            send: a,
            receive: Mutex::new(br),
            closed: closed.clone(),
        }),
        None,
        EndpointOptions {
            lease_ttl: std::time::Duration::from_millis(800),
            admission_timeout: std::time::Duration::from_millis(150),
            ..Default::default()
        },
    )
    .unwrap();
    let right = Endpoint::open(
        runtime,
        Arc::new(AdmissionFilter {
            pipe: Pipe {
                send: b,
                receive: Mutex::new(ar),
                closed,
            },
            assembly: Default::default(),
            suppress: suppress_admission.clone(),
            written: None,
        }),
        Some(binder),
        EndpointOptions {
            lease_ttl: std::time::Duration::from_millis(300),
            ..Default::default()
        },
    )
    .unwrap();
    let routes = left
        .bind(
            CallContext::default(),
            BindRequest::new(provider.contract()),
        )
        .await
        .unwrap();
    let result = routes
        .invoke(
            CallContext::default(),
            Call {
                method: method.clone(),
                receiver: None,
                arguments: vec![],
            },
        )
        .await
        .unwrap();
    tokio::time::sleep(std::time::Duration::from_millis(900)).await;
    let values = result.accept().await.unwrap().consume();
    let Data::Resource(reference) = &values[0].data else {
        panic!("expected resource")
    };
    routes
        .drop_resource(CallContext::default(), reference.clone())
        .await
        .unwrap();
    assert_eq!(
        closed_resources.load(std::sync::atomic::Ordering::SeqCst),
        1
    );
    suppress_admission.store(true, std::sync::atomic::Ordering::SeqCst);
    let error = match routes
        .invoke(
            CallContext::default(),
            Call {
                method,
                receiver: None,
                arguments: vec![],
            },
        )
        .await
    {
        Ok(_) => panic!("call completed without admission"),
        Err(error) => error,
    };
    assert_eq!(error.code, "unavailable");
    left.ping(CallContext::default()).await.unwrap();
    suppress_admission.store(false, std::sync::atomic::Ordering::SeqCst);
    routes.shutdown().await.unwrap();
    left.shutdown().await.unwrap();
    right.shutdown().await.unwrap();
    assert_eq!(right.stats(), EndpointStats::default());
}
impl Resource for ExpiringResource {
    fn invoke(
        &self,
        _: CallContext,
        _: String,
        _: Vec<Value>,
    ) -> BoxFuture<'_, Result<Vec<Value>>> {
        Box::pin(async { Ok(vec![]) })
    }
    fn close(&self) -> BoxFuture<'_, Result<()>> {
        Box::pin(async {
            self.0.add_permits(1);
            Ok(())
        })
    }
}

#[tokio::test(flavor = "current_thread")]
async fn dropped_result_releases_multiple_resources() {
    let runtime = tokio::runtime::Handle::current();
    let released = Arc::new(tokio::sync::Semaphore::new(0));
    let owner = released.clone();
    let method = Method {
        id: "fixture.Open.Call".into(),
        service: "fixture.Open".into(),
        name: "Call".into(),
        contract_hash: "a".repeat(64),
        resource_type_hash: String::new(),
    };
    let provider = Arc::new(
        StaticProvider::new(vec![MethodBinding {
            method: method.clone(),
            invoke: Some(Arc::new(move |context, _| {
                let owner = owner.clone();
                Box::pin(async move {
                    let first = context
                        .export(Arc::new(ExpiringResource(owner.clone())), "b".repeat(64))?;
                    let second =
                        context.export(Arc::new(ExpiringResource(owner)), "b".repeat(64))?;
                    Ok(vec![first.clone(), first, second])
                })
            })),
        }])
        .unwrap(),
    );
    let binder = Arc::new(
        LocalBinder::new(
            runtime.clone(),
            Limits {
                max_resources: 2,
                ..Default::default()
            },
            vec![provider.clone()],
        )
        .unwrap(),
    );
    let (a, ar) = mpsc::channel(4);
    let (b, br) = mpsc::channel(4);
    let closed = Cancellation::default();
    let left = Endpoint::open(
        runtime.clone(),
        Arc::new(Pipe {
            send: a,
            receive: Mutex::new(br),
            closed: closed.clone(),
        }),
        None,
        EndpointOptions::default(),
    )
    .unwrap();
    let right = Endpoint::open(
        runtime,
        Arc::new(Pipe {
            send: b,
            receive: Mutex::new(ar),
            closed,
        }),
        Some(binder),
        EndpointOptions {
            ..Default::default()
        },
    )
    .unwrap();
    let routes = left
        .bind(
            CallContext::default(),
            BindRequest::new(provider.contract()),
        )
        .await
        .unwrap();
    let call = Call {
        method,
        receiver: None,
        arguments: vec![],
    };
    let pending = routes
        .invoke(CallContext::default(), call.clone())
        .await
        .unwrap();
    pending.discard().await.unwrap();
    tokio::time::timeout(std::time::Duration::from_secs(2), released.acquire_many(2))
        .await
        .unwrap()
        .unwrap()
        .forget();
    routes
        .invoke(CallContext::default(), call)
        .await
        .unwrap()
        .discard()
        .await
        .unwrap();
    assert_eq!(released.available_permits(), 2);
    routes.close();
    left.begin_shutdown();
    left.shutdown().await.unwrap();
    right.shutdown().await.unwrap();
    assert_eq!(right.stats().pending_results, 0);
}

#[tokio::test(flavor = "current_thread")]
async fn renewal_timeout_closes_a_blocked_transport_and_wakes_owners() {
    let runtime = tokio::runtime::Handle::current();
    let (a, ar) = mpsc::channel(4);
    let (b, br) = mpsc::channel(4);
    let closed = Cancellation::default();
    let entered = Cancellation::default();
    let left = Endpoint::open(
        runtime.clone(),
        Arc::new(GatedWrite {
            kind: protocol::RENEW,
            pipe: Pipe {
                send: a,
                receive: Mutex::new(br),
                closed: closed.clone(),
            },
            assembly: Default::default(),
            entered: entered.clone(),
            release: Cancellation::default(),
            events: Default::default(),
        }),
        None,
        EndpointOptions {
            limits: Limits {
                max_pending_controls: 1,
                ..Default::default()
            },
            lease_ttl: std::time::Duration::from_millis(4),
            admission_timeout: std::time::Duration::from_millis(10),
            ..Default::default()
        },
    )
    .unwrap();
    let right = Endpoint::open(
        runtime,
        Arc::new(Pipe {
            send: b,
            receive: Mutex::new(ar),
            closed: closed.clone(),
        }),
        None,
        EndpointOptions::default(),
    )
    .unwrap();
    entered.cancelled().await;
    assert_eq!(
        left.ping(CallContext::default()).await.unwrap_err().code,
        "resource_exhausted"
    );
    tokio::time::timeout(std::time::Duration::from_secs(2), closed.cancelled())
        .await
        .unwrap();
    left.shutdown().await.unwrap();
    right.shutdown().await.unwrap();
    assert_eq!(left.stats().pending_calls, 0);
}

struct GatedWrite {
    kind: u64,
    pipe: Pipe,
    assembly: std::sync::Mutex<protocol::Assembly>,
    entered: Cancellation,
    release: Cancellation,
    events: Arc<std::sync::Mutex<Vec<u64>>>,
}
impl MessageConn for GatedWrite {
    fn read(&self) -> BoxFuture<'_, Result<Vec<u8>>> {
        self.pipe.read()
    }
    fn close(&self) {
        self.pipe.close();
        self.release.cancel();
    }
    fn write(&self, data: Vec<u8>) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            let frame = self
                .assembly
                .lock()
                .unwrap()
                .push(&data, &Limits::default())?
                .map(|bytes| protocol::Frame::decode(&bytes, &Limits::default()))
                .transpose()?;
            if frame
                .as_ref()
                .is_some_and(|frame| frame.kind == self.kind && !frame.reply)
            {
                self.entered.cancel();
                self.release.cancelled().await;
            }
            self.pipe.write(data).await?;
            if let Some(frame) = frame {
                self.events.lock().unwrap().push(frame.kind);
            }
            Ok(())
        })
    }
}

#[tokio::test(flavor = "current_thread")]
async fn cancel_during_physical_write_closes_transport_and_reclaims_capacity() {
    for saturated in [false, true] {
        let runtime = tokio::runtime::Handle::current();
        let method = Method {
            id: "fixture.Wait.Call".into(),
            service: "fixture.Wait".into(),
            name: "Call".into(),
            contract_hash: "a".repeat(64),
            resource_type_hash: String::new(),
        };
        let provider = Arc::new(
            StaticProvider::new(vec![MethodBinding {
                method: method.clone(),
                invoke: Some(Arc::new(|context, _| {
                    Box::pin(async move {
                        context.cancellation.cancelled().await;
                        Err(Status::new("canceled", "stopped"))
                    })
                })),
            }])
            .unwrap(),
        );
        let binder = Arc::new(
            LocalBinder::new(runtime.clone(), Limits::default(), vec![provider.clone()]).unwrap(),
        );
        let (a, ar) = mpsc::channel(4);
        let (b, br) = mpsc::channel(4);
        let closed = Cancellation::default();
        let entered = Cancellation::default();
        let release = Cancellation::default();
        let events = Arc::new(std::sync::Mutex::new(Vec::new()));
        let left = Endpoint::open(
            runtime.clone(),
            Arc::new(GatedWrite {
                kind: protocol::CALL,
                pipe: Pipe {
                    send: a,
                    receive: Mutex::new(br),
                    closed: closed.clone(),
                },
                assembly: Default::default(),
                entered: entered.clone(),
                release: release.clone(),
                events: events.clone(),
            }),
            None,
            EndpointOptions {
                limits: Limits {
                    max_pending_controls: if saturated { 128 } else { 1 },
                    ..Default::default()
                },
                admission_timeout: if saturated {
                    std::time::Duration::from_millis(50)
                } else {
                    std::time::Duration::from_secs(5)
                },
                ..Default::default()
            },
        )
        .unwrap();
        let right = Endpoint::open(
            runtime,
            Arc::new(Pipe {
                send: b,
                receive: Mutex::new(ar),
                closed: closed.clone(),
            }),
            Some(binder),
            EndpointOptions::default(),
        )
        .unwrap();
        let routes = left
            .bind(
                CallContext::default(),
                BindRequest::new(provider.contract()),
            )
            .await
            .unwrap();
        let context = CallContext::default();
        let cancellation = context.cancellation.clone();
        let owner = routes.clone();
        let task = tokio::spawn(async move {
            owner
                .invoke(
                    context,
                    Call {
                        method,
                        receiver: None,
                        arguments: vec![],
                    },
                )
                .await
        });
        entered.cancelled().await;
        let mut queued = Vec::new();
        if saturated {
            // Fill the reserved control queue while a physical data write is blocked.
            for _ in 0..33 {
                let endpoint = left.clone();
                queued.push(tokio::spawn(async move {
                    endpoint.ping(CallContext::default()).await
                }));
            }
            tokio::time::timeout(std::time::Duration::from_secs(2), async {
                while left.stats().pending_calls < 34 {
                    tokio::task::yield_now().await;
                }
            })
            .await
            .unwrap();
        }
        cancellation.cancel();
        assert!(task.await.unwrap().is_err());
        assert!(!events.lock().unwrap().contains(&protocol::CANCEL));
        tokio::time::timeout(std::time::Duration::from_secs(2), closed.cancelled())
            .await
            .unwrap();
        for task in queued {
            assert!(task.await.unwrap().is_err());
        }
        routes.shutdown().await.unwrap();
        left.shutdown().await.unwrap();
        right.shutdown().await.unwrap();
        assert_eq!(left.stats().pending_calls, 0);
    }
}

#[tokio::test(flavor = "current_thread")]
async fn symmetric_calls_fragmentation_and_shutdown() {
    let method = Method {
        id: "sample.Echo.echo".into(),
        service: "sample.Echo".into(),
        name: "echo".into(),
        contract_hash: "a".repeat(64),
        resource_type_hash: String::new(),
    };
    let provider = Arc::new(
        StaticProvider::new(vec![MethodBinding {
            method: method.clone(),
            invoke: Some(Arc::new(|_, values| Box::pin(async { Ok(values) }))),
        }])
        .unwrap(),
    );
    let contract = provider.contract();
    let limits = Limits {
        max_frame_bytes: 256,
        max_pending_calls: 1,
        max_pending_results: 1,
        ..Limits::default()
    };
    let binder = Arc::new(
        LocalBinder::new(
            tokio::runtime::Handle::current(),
            limits.clone(),
            vec![provider],
        )
        .unwrap(),
    );
    let (a, ar) = mpsc::channel(2);
    let (b, br) = mpsc::channel(2);
    let closed = Cancellation::default();
    let left = Endpoint::open(
        tokio::runtime::Handle::current(),
        Arc::new(Pipe {
            send: a,
            receive: Mutex::new(br),
            closed: closed.clone(),
        }),
        Some(binder.clone()),
        EndpointOptions {
            limits: Limits {
                max_pending_calls: 2,
                max_pending_results: 2,
                ..limits.clone()
            },
            ..EndpointOptions::default()
        },
    )
    .unwrap();
    let right = Endpoint::open(
        tokio::runtime::Handle::current(),
        Arc::new(Pipe {
            send: b,
            receive: Mutex::new(ar),
            closed,
        }),
        Some(binder),
        EndpointOptions {
            limits,
            ..EndpointOptions::default()
        },
    )
    .unwrap();
    left.ping(CallContext::default()).await.unwrap();
    let mut abandoned =
        Box::pin(left.bind(CallContext::default(), BindRequest::new(contract.clone())));
    assert!(
        std::future::poll_fn(|cx| std::task::Poll::Ready(abandoned.as_mut().poll(cx).is_pending()))
            .await
    );
    tokio::time::timeout(std::time::Duration::from_secs(2), async {
        while right.stats().bindings != 1 || left.stats().pending_calls != 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    // The remote reply is already queued, but the caller never polls it.
    drop(abandoned);
    tokio::time::timeout(std::time::Duration::from_secs(2), async {
        while right.stats().bindings != 0 {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    let (a, b) = tokio::join!(
        left.bind(CallContext::default(), BindRequest::new(contract.clone())),
        right.bind(CallContext::default(), BindRequest::new(contract))
    );
    let a = a.unwrap();
    let b = b.unwrap();
    for routes in [&a, &b] {
        let values = vec![Value::new("string", Data::String("message".repeat(100)))];
        let reply = routes
            .invoke(
                CallContext::default(),
                Call {
                    method: method.clone(),
                    receiver: None,
                    arguments: values.clone(),
                },
            )
            .await
            .unwrap();
        left.ping(CallContext::default()).await.unwrap();
        right.ping(CallContext::default()).await.unwrap();
        let rejected = routes
            .invoke(
                CallContext::default(),
                Call {
                    method: method.clone(),
                    receiver: None,
                    arguments: values.clone(),
                },
            )
            .await;
        assert_eq!(rejected.err().unwrap().code, "resource_exhausted");
        assert_eq!(reply.accept().await.unwrap().consume(), values);
    }
    let (a, b) = tokio::join!(a.shutdown(), b.shutdown());
    a.unwrap();
    b.unwrap();
    left.ping(CallContext::default()).await.unwrap();
    let (a, b) = tokio::join!(left.shutdown(), right.shutdown());
    a.unwrap();
    b.unwrap();
    assert_eq!(left.stats(), EndpointStats::default());
    assert_eq!(right.stats(), EndpointStats::default());
}
