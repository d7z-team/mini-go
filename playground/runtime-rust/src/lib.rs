//! Mini-Go runtime contracts and execution primitives.
#![forbid(unsafe_code)]

pub mod contract;
#[rustfmt::skip]
pub mod contract_generated;
pub mod environment;
pub mod error;
pub mod execution;
#[cfg(not(target_arch = "wasm32"))]
mod executor_pool;
pub mod ffi;
pub mod heap;
pub mod instance;
pub mod loader;
mod operators;
pub mod program;
#[cfg(feature = "rpc")]
pub mod rpc;
pub mod snapshot;
#[cfg(all(feature = "stdlib-host", not(target_arch = "wasm32")))]
pub mod stdlib_host;
pub mod symbols;
pub mod types;
pub mod value;

pub use error::RuntimeError;
#[cfg(not(target_arch = "wasm32"))]
pub use execution::Executor;
pub use execution::{
    Debugger, Execution, ExecutionState, InstanceOptions, ScopeStats, SharedInstance as Instance,
};
pub use instance::patch::{PatchPlan, PatchResult, RevisionInfo};
pub use instance::stats::{InstanceState, Stats};
pub use instance::{ExecutionLimits as Limits, UNLIMITED_STEPS};
pub use loader::LoadLimits as LoadOptions;
pub use program::Program;
pub use snapshot::{HostSnapshot, HostValue, SnapshotLimits};
