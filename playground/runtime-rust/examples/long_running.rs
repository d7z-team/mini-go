//! Bounded host loop: await scopes, cancel work, replace code and observe resources.
use mini_go::{
    HostValue, InstanceOptions, Limits, LoadOptions, Program, RuntimeError, UNLIMITED_STEPS,
    ffi::Cancellation,
};
use std::{
    sync::Arc,
    time::{Duration, Instant},
};

fn retry_busy<T>(
    mut operation: impl FnMut() -> Result<T, RuntimeError>,
) -> Result<T, RuntimeError> {
    let deadline = Instant::now() + Duration::from_secs(1);
    loop {
        match operation() {
            Err(error) if error.code == "busy" && Instant::now() < deadline => {
                std::thread::yield_now()
            }
            result => return result,
        }
    }
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args: Vec<_> = std::env::args().skip(1).collect();
    if !(2..=3).contains(&args.len()) {
        return Err("expected two compatible arithmetic images and optional round count".into());
    }
    let rounds: usize = args.get(2).map_or(Ok(10), |value| value.parse())?;
    if rounds == 0 || rounds > 1_000_000 {
        return Err("round count must be between 1 and 1000000".into());
    }
    let programs = args[..2]
        .iter()
        .map(|path| {
            Ok(Arc::new(Program::load(
                &std::fs::read(path)?,
                LoadOptions::default(),
            )?))
        })
        .collect::<Result<Vec<_>, Box<dyn std::error::Error>>>()?;
    let instance = programs[0].instantiate(InstanceOptions {
        limits: Limits {
            max_steps: UNLIMITED_STEPS,
            ..Default::default()
        },
        ..Default::default()
    })?;
    let wait = Cancellation::default();
    let outcome = (|| -> Result<(), Box<dyn std::error::Error>> {
        for round in 0..rounds {
            let call = retry_busy(|| instance.start("default", &[HostValue::int(10)]))?;
            call.wait(&wait)?;
            call.wait_scope(&wait)?;
            retry_busy(|| {
                let plan = instance.prepare_patch(programs[(round + 1) % 2].clone())?;
                instance.apply_patch(plan)
            })?;
            if round % 1000 == 0 || round + 1 == rounds {
                let stats = instance.stats();
                println!(
                    "round={} revisions={} scopes={} ffi={} timers={} arena_bytes={} allocated_bytes={}",
                    round + 1,
                    stats.retained_revisions,
                    stats.active_scopes,
                    stats.pending_ffi_calls,
                    stats.timers,
                    stats.heap.live_bytes,
                    stats.heap.total_allocated_bytes
                );
                if stats.retained_revisions != 1 || stats.active_scopes != 0 {
                    return Err("scope or revision remains active after replacement".into());
                }
            }
        }
        let call = retry_busy(|| instance.start("default", &[HostValue::int(1_000_000)]))?;
        call.cancel();
        match call.wait(&wait) {
            Err(error) if error.code == "canceled" => {}
            Err(error) => return Err(error.into()),
            Ok(_) => return Err("expected canceled call".into()),
        }
        if let Err(error) = call.wait_scope(&wait)
            && error.code != "canceled"
        {
            return Err(error.into());
        }
        Ok(())
    })();
    let closed = instance.shutdown(&wait);
    outcome?;
    closed?;
    Ok(())
}
