//! Load a Go-precompiled block once and invoke it repeatedly with host inputs.
use mini_go::{HostValue, InstanceOptions, LoadOptions, Program, ffi::Cancellation};
use std::sync::Arc;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mut arguments = std::env::args().skip(1);
    let path = arguments
        .next()
        .ok_or("expected image path followed by integers")?;
    let program = Arc::new(Program::load(
        &std::fs::read(path)?,
        LoadOptions::default(),
    )?);
    let instance = program.instantiate(InstanceOptions::default())?;
    let cancellation = Cancellation::default();
    let outcome = (|| -> Result<(), Box<dyn std::error::Error>> {
        for input in arguments {
            let execution = instance.start("default", &[HostValue::int(input.parse()?)])?;
            let result = execution.wait(&cancellation)?;
            println!("{:?}", result.roots);
        }
        Ok(())
    })();
    let closed = instance.shutdown(&cancellation);
    outcome?;
    closed?;
    Ok(())
}
