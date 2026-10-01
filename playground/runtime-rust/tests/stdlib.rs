#![cfg(any(feature = "host-conformance", feature = "stdlib-host"))]

#[path = "support/broker.rs"]
#[cfg(feature = "host-conformance")]
mod broker;

use mini_go::{
    RuntimeError,
    environment::{Clock, Entropy},
    instance::{ExecutionLimits, Instance, PollStatus},
    loader::LoadLimits,
    program::Program,
    snapshot::HostData,
};
use serde::{
    Deserialize,
    de::{DeserializeSeed, SeqAccess, Visitor},
};
use std::{
    fmt,
    io::Read,
    sync::{
        Arc,
        atomic::{AtomicU64, AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};

#[derive(Deserialize)]
struct Vector {
    name: String,
    image: Box<serde_json::value::RawValue>,
    tests: Vec<String>,
}

#[derive(Default)]
struct Environment {
    elapsed: AtomicU64,
    entropy: AtomicUsize,
}
impl Clock for Environment {
    fn unix_time(&self) -> (i64, u32) {
        let nanos = 123_456_789 + self.elapsed.load(Ordering::Relaxed);
        (
            1_700_000_000 + (nanos / 1_000_000_000) as i64,
            (nanos % 1_000_000_000) as u32,
        )
    }
    fn monotonic_ns(&self) -> u64 {
        self.elapsed.load(Ordering::Relaxed)
    }
}
impl Entropy for Environment {
    fn read(&self, bytes: &mut [u8]) -> (usize, Option<RuntimeError>) {
        let start = self
            .entropy
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |start| {
                Some(start.saturating_add(bytes.len()).min(4096))
            })
            .unwrap();
        let count = bytes.len().min(4096 - start);
        for (offset, byte) in bytes[..count].iter_mut().enumerate() {
            *byte = ((start + offset) % 4 + 1) as u8;
        }
        (
            count,
            (count == 0 && !bytes.is_empty()).then(|| RuntimeError::new("entropy", "host", "EOF")),
        )
    }
}

fn run_package(vector: &Vector, bridge: &dyn mini_go::ffi::Bridge) -> Result<(), RuntimeError> {
    let program = Arc::new(Program::load(
        vector.image.get().as_bytes(),
        LoadLimits::default(),
    )?);
    let mut instance = Instance::with_bridge(
        program,
        ExecutionLimits {
            max_steps: 100_000_000,
            ..ExecutionLimits::default()
        },
        bridge,
    )?;
    let environment = Arc::new(Environment::default());
    instance.set_environment(environment.clone(), environment.clone())?;
    instance.start("default", vec![])?;
    let wake = instance.wake();
    let mut last_progress = Instant::now();
    loop {
        let observed = wake.epoch();
        match instance.poll_steps(4096)? {
            PollStatus::Running => {
                last_progress = Instant::now();
            }
            PollStatus::Ready => break,
            PollStatus::Paused => {
                return Err(RuntimeError::new("paused", "stdlib", "unexpected pause"));
            }
            PollStatus::Pending => {
                if let Some(delay) = instance.next_timer_delay() {
                    environment
                        .elapsed
                        .fetch_add(delay.as_nanos() as u64, Ordering::Relaxed);
                    last_progress = Instant::now();
                } else {
                    if last_progress.elapsed() > Duration::from_secs(30) {
                        return Err(RuntimeError::new(
                            "pending",
                            "stdlib",
                            "test made no progress for 30 seconds",
                        ));
                    }
                    wake.wait(observed, Duration::from_millis(10));
                }
            }
        }
    }
    let report = instance.snapshot_results(Default::default())?;
    instance.close()?;
    if instance.heap_stats().live_objects != 0 {
        return Err(RuntimeError::new(
            "resource_leak",
            "stdlib",
            "closed instance retained guest objects",
        ));
    }
    let invalid = || RuntimeError::new("test_report", "stdlib", "invalid testing.Report");
    let root = report.roots.first().ok_or_else(invalid)?;
    let HostData::Struct(fields) = &root.data else {
        return Err(invalid());
    };
    let passed = matches!(
        fields.get("Passed").map(|value| &value.data),
        Some(HostData::Bool(true))
    );
    let Some(results) = fields.get("Results") else {
        return Err(invalid());
    };
    let HostData::Slice {
        storage,
        start,
        length,
        ..
    } = &results.data
    else {
        return Err(invalid());
    };
    let HostData::Array(values) = &report.resolve_address(storage)?.data else {
        return Err(invalid());
    };
    let mut names = Vec::new();
    let mut failures = Vec::new();
    for result in values.get(*start..start + length).ok_or_else(invalid)? {
        let HostData::Struct(fields) = &result.data else {
            return Err(invalid());
        };
        let name = String::from_utf8(report.bytes(fields.get("Name").ok_or_else(invalid)?)?)
            .map_err(|_| invalid())?;
        let status = report.bytes(fields.get("Status").ok_or_else(invalid)?)?;
        if status == b"fail" {
            let message = report.bytes(fields.get("Message").ok_or_else(invalid)?)?;
            failures.push(format!("{name}: {}", String::from_utf8_lossy(&message)));
        }
        names.push(name);
    }
    if names != vector.tests {
        return Err(RuntimeError::new(
            "test_manifest",
            "stdlib",
            "executed test names differ from compiler manifest",
        ));
    }
    if !passed || !failures.is_empty() {
        return Err(RuntimeError::new(
            "test_failure",
            "stdlib",
            failures.join("; "),
        ));
    }
    Ok(())
}

#[test]
#[cfg(feature = "host-conformance")]
fn current_standard_library_tests_execute_with_actual_go_providers() {
    let broker_path = std::env::var("MINIGO_HOST_BROKER")
        .expect("run make runtime-rust-conformance to build the Go provider broker");
    run_standard_library(|vector| {
        let bridge = broker::BrokerBridge::new(&broker_path)?;
        run_package(vector, &bridge)
    });
}

#[test]
#[cfg(feature = "stdlib-host")]
fn current_standard_library_tests_execute_with_native_rust_providers() {
    use mini_go::stdlib_host::{HostBuilder, MemoryHostOptions};
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    run_standard_library(|vector| {
        let host = runtime
            .block_on(
                HostBuilder::memory(runtime.handle().clone(), MemoryHostOptions::default())
                    .unwrap()
                    .build(),
            )
            .unwrap();
        let result = run_package(vector, host.as_ref());
        runtime.block_on(host.shutdown()).unwrap();
        result
    });
}

struct VectorSequence<'a, F> {
    run: &'a mut F,
    filter: Option<&'a str>,
    matched: bool,
    failures: Vec<String>,
}

impl<'de, F: FnMut(&Vector) -> Result<(), RuntimeError>> DeserializeSeed<'de>
    for &mut VectorSequence<'_, F>
{
    type Value = ();

    fn deserialize<D: serde::Deserializer<'de>>(self, deserializer: D) -> Result<(), D::Error> {
        deserializer.deserialize_seq(self)
    }
}

impl<'de, F: FnMut(&Vector) -> Result<(), RuntimeError>> Visitor<'de>
    for &mut VectorSequence<'_, F>
{
    type Value = ();

    fn expecting(&self, formatter: &mut fmt::Formatter) -> fmt::Result {
        formatter.write_str("an array of standard library vectors")
    }

    fn visit_seq<A: SeqAccess<'de>>(self, mut sequence: A) -> Result<(), A::Error> {
        while let Some(vector) = sequence.next_element::<Vector>()? {
            if self.filter.is_some_and(|filter| vector.name != filter) {
                continue;
            }
            self.matched = true;
            eprintln!("stdlib {}", vector.name);
            if let Err(error) = (self.run)(&vector) {
                eprintln!("stdlib {}: {error}", vector.name);
                self.failures.push(format!("{}: {error}", vector.name));
            }
        }
        Ok(())
    }
}

fn run_standard_library(mut run: impl FnMut(&Vector) -> Result<(), RuntimeError>) {
    let file = std::fs::File::open(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../testdata/runtime/stdlib.json.gz"
    ))
    .expect("run make generate for current stdlib images");
    let filter = std::env::var("RUST_STDLIB_PACKAGE").ok();
    let mut sequence = VectorSequence {
        run: &mut run,
        filter: filter.as_deref(),
        matched: false,
        failures: Vec::new(),
    };
    let reader = flate2::read::GzDecoder::new(file).take(1 << 30);
    let mut deserializer = serde_json::Deserializer::from_reader(reader);
    (&mut sequence).deserialize(&mut deserializer).unwrap();
    deserializer.end().unwrap();
    if let Some(filter) = filter.as_deref() {
        assert!(sequence.matched, "unknown stdlib package {filter}");
    }
    assert!(
        sequence.failures.is_empty(),
        "{}",
        sequence.failures.join("\n")
    );
}
