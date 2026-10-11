//! Injectable wall/monotonic time and entropy for deterministic owner polling.

use crate::error::RuntimeError;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

pub trait Clock: Send + Sync {
    fn unix_time(&self) -> (i64, u32);
    fn monotonic_ns(&self) -> u64;
}

pub struct SystemClock {
    origin: Instant,
}
impl Default for SystemClock {
    fn default() -> Self {
        Self {
            origin: Instant::now(),
        }
    }
}
impl Clock for SystemClock {
    fn unix_time(&self) -> (i64, u32) {
        match SystemTime::now().duration_since(UNIX_EPOCH) {
            Ok(duration) => (duration.as_secs() as i64, duration.subsec_nanos()),
            Err(error) => {
                let duration = error.duration();
                if duration.subsec_nanos() == 0 {
                    (-(duration.as_secs() as i64), 0)
                } else {
                    (
                        -(duration.as_secs() as i64) - 1,
                        1_000_000_000 - duration.subsec_nanos(),
                    )
                }
            }
        }
    }
    fn monotonic_ns(&self) -> u64 {
        self.origin.elapsed().as_nanos().min(u128::from(u64::MAX)) as u64
    }
}

pub trait Entropy: Send + Sync {
    /// Returns a partial count and an optional failure; successful bytes remain
    /// observable even when the provider subsequently reports an error.
    fn read(&self, bytes: &mut [u8]) -> (usize, Option<RuntimeError>);
}

pub struct SystemEntropy;
impl Entropy for SystemEntropy {
    fn read(&self, bytes: &mut [u8]) -> (usize, Option<RuntimeError>) {
        match getrandom::fill(bytes) {
            Ok(()) => (bytes.len(), None),
            Err(error) => (
                0,
                Some(RuntimeError::new("entropy", "host", error.to_string())),
            ),
        }
    }
}
