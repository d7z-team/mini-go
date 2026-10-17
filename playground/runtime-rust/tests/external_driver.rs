mod support;
use mini_go::{
    Instance, Limits, LoadOptions, Program, RuntimeError,
    ffi::{self, Bridge, Call, Cancellation, Completion, Reply, Session},
    instance::{Instance as Machine, PollStatus},
};
use serde_json::json;
use std::{
    future::Future,
    pin::Pin,
    sync::{Arc, Mutex},
    task::{Poll, Waker},
};
use support::slot_code;

#[derive(Clone, Default)]
struct Host {
    completion: Arc<Mutex<Option<Completion>>>,
    cleanup: Arc<Mutex<(bool, Option<Waker>)>>,
}
impl Call for Host {
    fn cancel(&self) {}
}
impl Bridge for Host {
    fn open(&self, _: Cancellation) -> Result<Box<dyn Session>, RuntimeError> {
        Ok(Box::new(self.clone()))
    }
}
impl Session for Host {
    fn start(
        &self,
        _: Cancellation,
        _: ffi::Request,
        completion: Completion,
    ) -> Result<Box<dyn Call>, RuntimeError> {
        *self.completion.lock().unwrap() = Some(completion);
        Ok(Box::<Host>::default())
    }
    fn shutdown(&self, _: Cancellation) -> Result<(), RuntimeError> {
        panic!("async cleanup must be polled")
    }
    fn shutdown_async(
        &self,
    ) -> Pin<Box<dyn Future<Output = Result<(), RuntimeError>> + Send + '_>> {
        Box::pin(std::future::poll_fn(|cx| {
            let mut cleanup = self.cleanup.lock().unwrap();
            if cleanup.0 {
                Poll::Ready(Ok(()))
            } else {
                cleanup.1 = Some(cx.waker().clone());
                Poll::Pending
            }
        }))
    }
}

fn initialization_fixture() -> Vec<u8> {
    let integer = json!({"kind":3,"primitive":3});
    let string = json!({"kind":3,"primitive":2});
    let bytes = json!({"kind":5,"node":"bytes"});
    support::image(json!({
        "type_table":{"nodes":[{"id":"bytes","kind":5,"elem":{"kind":3,"primitive":9}}]},
        "constants":[{"id":"route","type":{"kind":3,"primitive":2},"value":"external.test"},{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "functions":[
            {"id":"fn.Main","signature":{"results":[integer]},"code":slot_code(json!([integer]),&[
                ("const",json!({"constant":"answer"}),json!({"outputs":[0]})),
                ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]})),
            ])},
            {"id":"fn.init","code":slot_code(json!([string,bytes,bytes,string,integer]),&[
                ("const",json!({"constant":"route"}),json!({"outputs":[0]})),
                ("zero",json!({"type":bytes}),json!({"outputs":[1]})),
                ("call_ffi",json!({"arg_count":2,"result_count":3}),json!({"inputs":[[0,0],[0,1]],"outputs":[2,3,4],"release":[0,1]})),
                ("pop",json!({}),json!({"inputs":[[0,4]],"release":[4]})),
                ("pop",json!({}),json!({"inputs":[[0,3]],"release":[3]})),
                ("pop",json!({}),json!({"inputs":[[0,2]],"release":[2]})),
                ("return",json!({}),json!({})),
            ])},
        ]
    }))
}

#[test]
fn external_driver_initializes_asynchronously_and_retains_cleanup_until_complete() {
    let host = Host::default();
    let program =
        Arc::new(Program::load(&initialization_fixture(), LoadOptions::default()).unwrap());
    let mut machine = Machine::with_bridge(program, Limits::default(), &host).unwrap();
    assert_eq!(
        machine
            .poll_initialize(&Cancellation::default(), 256)
            .unwrap(),
        PollStatus::Pending
    );
    assert!(!machine.root_ready());
    host.completion
        .lock()
        .unwrap()
        .take()
        .unwrap()
        .complete(Reply::new(vec![], None, None));
    assert_eq!(
        machine
            .poll_initialize(&Cancellation::default(), 256)
            .unwrap(),
        PollStatus::Ready
    );
    let instance = Instance::externally_driven(machine).unwrap();
    let execution = instance.start("default", &[]).unwrap();
    instance.drive(256).unwrap();
    assert!(execution.scope_settled());
    assert!(execution.result().is_ok());
    instance.begin_shutdown();
    instance.drive(256).unwrap();
    assert!(instance.shutdown_result().is_none());
    let observed = instance.wake().epoch();
    for _ in 0..3 {
        instance.begin_shutdown();
        assert!(!instance.drive(256).unwrap());
    }
    assert_eq!(
        instance.wake().epoch(),
        observed,
        "pending cleanup must sleep until a real notification"
    );
    let wake = {
        let mut cleanup = host.cleanup.lock().unwrap();
        cleanup.0 = true;
        cleanup.1.take().unwrap()
    };
    wake.wake();
    instance.drive(256).unwrap();
    assert!(instance.shutdown_result().unwrap().is_ok());
}

#[test]
fn external_driver_cancels_running_work() {
    let bytes = support::image(json!({
        "functions":[{"id":"fn.Main","code":slot_code(json!([]), &[
            ("label",json!({"label":"loop"}),json!({})),
            ("jump",json!({"label":"loop"}),json!({})),
        ])}]
    }));
    let program = Arc::new(Program::load(&bytes, LoadOptions::default()).unwrap());
    let mut machine = Machine::new(program, Limits::default()).unwrap();
    machine.initialize_root(&Cancellation::default()).unwrap();
    let instance = Instance::externally_driven(machine).unwrap();
    let execution = instance.start("default", &[]).unwrap();
    assert!(instance.drive(8).unwrap());
    execution.cancel();
    instance.drive(8).unwrap();
    assert!(execution.scope_settled());
    assert_eq!(execution.result().unwrap_err().code, "canceled");
    instance.begin_shutdown();
    instance.drive(8).unwrap();
    assert!(instance.shutdown_result().unwrap().is_ok());
}
