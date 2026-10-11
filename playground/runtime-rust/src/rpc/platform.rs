//! Native task and timer boundary for the Rust RPC runtime.

pub use tokio::{
    runtime::Handle,
    time::{sleep, timeout},
};

pub fn sleep_until(deadline: std::time::Instant) -> tokio::time::Sleep {
    tokio::time::sleep_until(deadline.into())
}
