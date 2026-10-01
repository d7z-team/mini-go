#[path = "support/execution_vectors.rs"]
mod execution_vectors;
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

fn fixture(name: &str) -> Vec<u8> {
    let patch = name.ends_with("-patch");
    let name = name.trim_end_matches("-patch");
    let integer = json!({"kind":3,"primitive":3});
    let string = json!({"kind":3,"primitive":2});
    let bytes = json!({"kind":5,"node":"bytes"});
    let ffi_types = json!([string, bytes, bytes, string, integer]);
    let ffi = [
        ("const", json!({"constant":"route"}), json!({"outputs":[0]})),
        ("zero", json!({"type":bytes}), json!({"outputs":[1]})),
        (
            "call_ffi",
            json!({"arg_count":2,"result_count":3}),
            json!({"inputs":[[0,0],[0,1]],"outputs":[2,3,4],"release":[0,1]}),
        ),
        ("pop", json!({}), json!({"inputs":[[0,4]],"release":[4]})),
        ("pop", json!({}), json!({"inputs":[[0,3]],"release":[3]})),
        ("pop", json!({}), json!({"inputs":[[0,2]],"release":[2]})),
        ("return", json!({}), json!({})),
    ];
    let answer = [
        (
            "const",
            json!({"constant":"answer"}),
            json!({"outputs":[0]}),
        ),
        (
            "return",
            json!({"result_count":1}),
            json!({"inputs":[[0,0]],"release":[0]}),
        ),
    ];
    let mut artifact = json!({
        "type_table":{"nodes":[{"id":"bytes","kind":5,"elem":{"kind":3,"primitive":9}}]},
        "constants":[{"id":"route","type":{"kind":3,"primitive":2},"value":"wasm.test"},{"id":"answer","type":{"kind":3,"primitive":3},"value":42}],
        "functions":[{"id":"fn.Main","signature":{"results":[integer]},"code":slot_code(json!([integer]),&answer)}]
    });
    if name == "init" {
        artifact["functions"]
            .as_array_mut()
            .unwrap()
            .push(json!({"id":"fn.init","code":slot_code(ffi_types.clone(),&ffi)}));
    }
    if name == "host" {
        artifact["functions"][0] = json!({"id":"fn.Main","code":slot_code(ffi_types.clone(),&ffi)});
    }
    if name == "host-result" {
        let mut instructions = ffi[..3].to_vec();
        instructions.push((
            "return",
            json!({"result_count":3}),
            json!({"inputs":[[0,2],[0,3],[0,4]],"release":[2,3,4]}),
        ));
        artifact["functions"][0] = json!({"id":"fn.Main","signature":{"results":[bytes,string,integer]},"code":slot_code(ffi_types.clone(),&instructions)});
    }
    if name == "background" {
        artifact["functions"]
            .as_array_mut()
            .unwrap()
            .push(json!({"id":"worker","code":slot_code(ffi_types.clone(),&ffi)}));
        artifact["type_table"]["nodes"]
            .as_array_mut()
            .unwrap()
            .push(json!({"id":"function","kind":10,"signature":{}}));
        let mut operations = vec![
            (
                "make_closure",
                json!({"function":"worker"}),
                json!({"outputs":[1]}),
            ),
            (
                "spawn",
                json!({"arg_count":0}),
                json!({"inputs":[[0,1]],"release":[1]}),
            ),
        ];
        operations.extend(answer.clone());
        artifact["functions"][0]["code"] =
            slot_code(json!([integer,{"kind":10,"node":"function"}]), &operations);
    }
    if name == "echo" || name == "string" {
        let typ = json!({"kind":3,"primitive":if name == "echo" {3} else {2}});
        artifact["functions"][0] = json!({"id":"fn.Main","signature":{"params":[{"type":typ}],"results":[typ]},"locals":[{"id":"input","type":typ}],"code":slot_code(json!([typ]), &[
            ("load_local",json!({"local":"input"}),json!({"outputs":[0]})),
            ("return",json!({"result_count":1}),json!({"inputs":[[0,0]],"release":[0]}))])});
    }
    if name == "timer" {
        artifact["type_table"]["nodes"]
            .as_array_mut()
            .unwrap()
            .push(json!({"id":"channel","kind":9,"direction":1,"elem":{"kind":3,"primitive":1}}));
        artifact["constants"].as_array_mut().unwrap().extend([
            json!({"id":"capacity","type":{"kind":3,"primitive":3},"value":1}),
            json!({"id":"delay","type":{"kind":3,"primitive":7},"value":20000000}),
        ]);
        artifact["functions"][0]["locals"] =
            json!([{"id":"channel","type":{"kind":9,"node":"channel"}}]);
        let mut operations = vec![
            (
                "const",
                json!({"constant":"capacity"}),
                json!({"outputs":[0]}),
            ),
            (
                "make_waitable",
                json!({"type":{"kind":9,"node":"channel"}}),
                json!({"inputs":[[0,0]],"outputs":[1],"release":[0]}),
            ),
            (
                "store_local",
                json!({"local":"channel"}),
                json!({"inputs":[[0,1]],"release":[1]}),
            ),
            (
                "load_local",
                json!({"local":"channel"}),
                json!({"outputs":[1]}),
            ),
            ("const", json!({"constant":"delay"}), json!({"outputs":[2]})),
            (
                "zero",
                json!({"type":{"kind":3,"primitive":7}}),
                json!({"outputs":[3]}),
            ),
            (
                "call_intrinsic",
                json!({"id":"time.timer_start","arg_count":3}),
                json!({"inputs":[[0,1],[0,2],[0,3]],"release":[1,2,3]}),
            ),
            (
                "load_local",
                json!({"local":"channel"}),
                json!({"outputs":[1]}),
            ),
            (
                "waitable_recv",
                json!({}),
                json!({"inputs":[[0,1]],"outputs":[4],"release":[1]}),
            ),
            ("pop", json!({}), json!({"inputs":[[0,4]],"release":[4]})),
        ];
        operations.extend(answer.clone());
        artifact["functions"][0]["code"] = slot_code(
            json!([integer,{"kind":9,"node":"channel"},{"kind":3,"primitive":7},{"kind":3,"primitive":7},{"kind":3,"primitive":1}]),
            &operations,
        );
    }
    if name == "loop" {
        artifact["functions"][0] = json!({"id":"fn.Main","code":slot_code(json!([]), &[
            ("label",json!({"label":"loop"}),json!({})),("jump",json!({"label":"loop"}),json!({}))])});
    }
    if patch {
        artifact["constants"][1]["value"] = json!(43);
    }
    support::image(artifact)
}

#[test]
fn external_driver_initializes_asynchronously_and_retains_cleanup_until_complete() {
    let host = Host::default();
    let program = Arc::new(Program::load(&fixture("init"), LoadOptions::default()).unwrap());
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
    let execution = instance.start("default", vec![]).unwrap();
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
fn external_driver_cancels_running_work_and_wasm_fixtures_validate() {
    for name in [
        "answer",
        "answer-patch",
        "init",
        "host",
        "host-result",
        "loop",
        "background",
        "echo",
        "string",
        "timer",
    ] {
        let bytes = fixture(name);
        let program = Arc::new(Program::load(&bytes, LoadOptions::default()).unwrap());
        if let Some(directory) = std::env::var_os("MINIGO_WASM_FIXTURES") {
            std::fs::create_dir_all(&directory).unwrap();
            std::fs::write(
                std::path::Path::new(&directory).join(format!("{name}.json")),
                &bytes,
            )
            .unwrap();
        }
        if name != "loop" {
            continue;
        }
        let mut machine = Machine::new(program, Limits::default()).unwrap();
        machine.initialize_root(&Cancellation::default()).unwrap();
        let instance = Instance::externally_driven(machine).unwrap();
        let execution = instance.start("default", vec![]).unwrap();
        assert!(instance.drive(8).unwrap());
        execution.cancel();
        instance.drive(8).unwrap();
        assert!(execution.scope_settled());
        assert_eq!(execution.result().unwrap_err().code, "canceled");
        instance.begin_shutdown();
        instance.drive(8).unwrap();
        assert!(instance.shutdown_result().unwrap().is_ok());
    }
    if let Some(directory) = std::env::var_os("MINIGO_WASM_FIXTURES") {
        #[derive(serde::Deserialize, serde::Serialize)]
        struct Vector {
            name: String,
            optimization: u8,
            result_integer: String,
            #[serde(skip_serializing)]
            image: Box<serde_json::value::RawValue>,
        }
        let mut vectors = execution_vectors::load::<Vector>();
        vectors.retain(|vector| {
            matches!(
                vector.name.as_str(),
                "arithmetic"
                    | "closure"
                    | "channel_buffer"
                    | "channel_rendezvous"
                    | "select_wait"
                    | "recover_panic"
                    | "semantic_boundaries"
            )
        });
        let directory = std::path::Path::new(&directory);
        for vector in &vectors {
            std::fs::write(
                directory.join(format!(
                    "vector-{}-{}.json",
                    vector.name, vector.optimization
                )),
                vector.image.get(),
            )
            .unwrap();
        }
        std::fs::write(
            directory.join("vectors.json"),
            serde_json::to_vec(&vectors).unwrap(),
        )
        .unwrap();
    }
}
