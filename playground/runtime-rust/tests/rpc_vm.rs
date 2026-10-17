#![cfg(feature = "rpc")]
#[path = "support/laboratory.rs"]
mod laboratory;
#[path = "../../../testdata/rpc/generated/rust/service.rs"]
pub mod rpcservice;
#[path = "../../../testdata/rpc/generated/rust/types.rs"]
pub mod rpctypes;
use mini_go::{
    ffi::Cancellation,
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    rpc::*,
};
use std::{
    io::Read,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};

fn run_image(image: &[u8], host: &Host, stop: Cancellation) -> Option<i64> {
    let mut bytes = Vec::new();
    flate2::read::GzDecoder::new(image)
        .read_to_end(&mut bytes)
        .unwrap();
    let program = Arc::new(Program::load(&bytes, LoadLimits::default()).unwrap());
    let mut instance = Instance::with_bridge(
        program,
        ExecutionLimits {
            max_steps: 5_000_000,
            ..ExecutionLimits::default()
        },
        host,
    )
    .unwrap();
    instance.start("default", &[]).unwrap();
    let deadline = Instant::now() + Duration::from_secs(15);
    let wake = instance.wake();
    let value = loop {
        assert!(Instant::now() < deadline, "RPC image did not settle");
        if stop.is_cancelled() {
            break None;
        }
        let epoch = wake.epoch();
        match instance.poll_steps(4096).unwrap() {
            PollStatus::Ready => {
                break Some(
                    instance.snapshot_results(Default::default()).unwrap().roots[0]
                        .integer()
                        .unwrap(),
                );
            }
            PollStatus::Pending => wake.wait(epoch, Duration::from_millis(10)),
            PollStatus::Running => {}
            PollStatus::Paused => panic!("unexpected debugger pause"),
        }
    };
    instance.close().unwrap();
    value
}

#[tokio::test(flavor = "current_thread")]
async fn precompiled_client_calls_rust_host_and_releases_resource() {
    let released = Arc::new(AtomicUsize::new(0));
    let provider =
        rpcservice::laboratory_provider(Arc::new(laboratory::Laboratory(released.clone())))
            .unwrap();
    let mut options = HostOptions::new(tokio::runtime::Handle::current());
    options.providers.push(provider);
    let host = Arc::new(Host::new(options).unwrap());
    let worker = host.clone();
    let value = VmExecutor::new(tokio::runtime::Handle::current(), 1)
        .unwrap()
        .run(move || {
            run_image(
                include_bytes!("../../../testdata/rpc/images/call.json.gz"),
                &worker,
                Cancellation::default(),
            )
        })
        .await
        .unwrap();
    assert_eq!(value, Some(42));
    assert_eq!(released.load(Ordering::SeqCst), 1);
    host.shutdown().await.unwrap();
}

#[tokio::test(flavor = "current_thread")]
async fn precompiled_provider_serves_rust_client_and_cancels_wait() {
    let router = Arc::new(router::Router::new(
        tokio::runtime::Handle::current(),
        Limits::default(),
        None,
    ));
    let mut options = HostOptions::new(tokio::runtime::Handle::current());
    options.publish_provider = Some(router.clone());
    let host = Arc::new(Host::new(options).unwrap());
    let worker = host.clone();
    let stop = Cancellation::default();
    let stopping = stop.clone();
    let task = tokio::spawn(async move {
        VmExecutor::new(tokio::runtime::Handle::current(), 1)
            .unwrap()
            .run(move || {
                run_image(
                    include_bytes!("../../../testdata/rpc/images/provider.json.gz"),
                    &worker,
                    stopping,
                )
            })
            .await
            .unwrap()
    });
    let context = CallContext::with_deadline(Instant::now() + Duration::from_secs(10));
    router
        .watch(context.clone(), &router.revision().router_id, 0)
        .await
        .unwrap();
    let client = rpcservice::LaboratoryClient::bind(
        context.clone(),
        router.as_ref(),
        BindOptions::default(),
    )
    .await
    .unwrap();
    let (counter, details) = client.open(context.clone(), 40).await.unwrap();
    let counter = counter.unwrap();
    assert_eq!(details.label, "counter");
    assert_eq!(counter.add(context.clone(), 2).await.unwrap().0, 42);
    assert_eq!(
        client
            .read(context.clone(), Some(counter.clone()))
            .await
            .unwrap()
            .0,
        42
    );
    counter.close(context).await.unwrap();
    let error = client
        .wait(CallContext::with_deadline(
            Instant::now() + Duration::from_millis(30),
        ))
        .await
        .unwrap_err();
    assert_eq!(error.code, "deadline_exceeded");
    client.close().await.unwrap();
    stop.cancel();
    assert_eq!(task.await.unwrap(), None);
    host.shutdown().await.unwrap();
    router.shutdown().await.unwrap();
}
