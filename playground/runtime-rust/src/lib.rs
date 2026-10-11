//! Mini-Go runtime contracts and execution primitives.
#![forbid(unsafe_code)]

#[cfg(feature = "compiler")]
pub mod compiler;
#[cfg(feature = "dap")]
pub mod dap;
#[cfg(feature = "compiler")]
pub mod language;
#[cfg(feature = "language-server")]
pub mod lsp;
#[cfg(feature = "language-server")]
mod server;
#[cfg(feature = "language-server")]
mod transport;
#[cfg(feature = "compiler")]
pub use compiler::{CompilerOptions, CompilerSession};
#[cfg(feature = "dap")]
pub use dap::DebugSession;
#[cfg(feature = "compiler")]
pub use language::LanguageService;
#[cfg(feature = "language-server")]
pub use lsp::LanguageServer;

pub mod contract;
#[rustfmt::skip]
pub mod contract_generated;
pub mod environment;
pub mod error;
pub mod execution;
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
#[cfg(feature = "stdlib-host")]
pub mod stdlib_host;
pub mod symbols;
pub mod types;
pub mod value;

pub use error::RuntimeError;
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
