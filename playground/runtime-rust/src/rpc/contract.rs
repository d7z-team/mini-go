use crate::ffi::Cancellation;
use std::collections::BTreeMap;
use std::fmt;
use std::future::Future;
use std::pin::Pin;
use std::time::Instant;

pub const CONTRACT_PROTOCOL: &str = "minigo.rpc.contract.v2";
pub const FFI_PROTOCOL: &str = "minigo.mrpc.ffi/v3";
pub const FFI_ROUTE: &str = "minigo.mrpc/v2";
pub const ENDPOINT_PROTOCOL: &str = "minigo.rpc.endpoint.v12";

/// Stable wire codes. Status retains a String so unknown peer codes can be forwarded.
pub mod codes {
    pub const UNKNOWN: &str = "unknown";
    pub const ABORTED: &str = "aborted";
    pub const OUT_OF_RANGE: &str = "out_of_range";
    pub const DATA_LOSS: &str = "data_loss";
    pub const UNAUTHENTICATED: &str = "unauthenticated";
    pub const CANCELED: &str = "canceled";
    pub const INVALID_ARGUMENT: &str = "invalid_argument";
    pub const DEADLINE_EXCEEDED: &str = "deadline_exceeded";
    pub const NOT_FOUND: &str = "not_found";
    pub const ALREADY_EXISTS: &str = "already_exists";
    pub const PERMISSION_DENIED: &str = "permission_denied";
    pub const RESOURCE_EXHAUSTED: &str = "resource_exhausted";
    pub const FAILED_PRECONDITION: &str = "failed_precondition";
    pub const UNIMPLEMENTED: &str = "unimplemented";
    pub const INTERNAL: &str = "internal";
    pub const UNAVAILABLE: &str = "unavailable";
    pub const PROTOCOL: &str = "protocol";
}

pub type BoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;
pub type Result<T> = std::result::Result<T, Status>;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Status {
    pub code: String,
    pub message: String,
}

impl Status {
    pub fn new(code: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
        }
    }
    pub(crate) fn protocol(message: impl Into<String>) -> Self {
        Self::new("protocol", message)
    }
    pub(crate) fn exhausted(message: impl Into<String>) -> Self {
        Self::new("resource_exhausted", message)
    }
}
impl fmt::Display for Status {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}
impl std::error::Error for Status {}

#[derive(Clone, Debug, Default, PartialEq, Eq, PartialOrd, Ord)]
pub struct Method {
    pub id: String,
    pub service: String,
    pub name: String,
    pub contract_hash: String,
    pub resource_type_hash: String,
}
impl Method {
    pub fn validate(&self) -> Result<()> {
        if self.service.is_empty()
            || self.name.is_empty()
            || self.id != format!("{}.{}", self.service, self.name)
            || self.contract_hash.len() != 64
            || ![0, 64].contains(&self.resource_type_hash.len())
        {
            return Err(Status::new("invalid_argument", "invalid RPC method"));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Contract {
    pub protocol: String,
    pub methods: Vec<Method>,
}
impl Contract {
    pub fn new(methods: Vec<Method>) -> Self {
        Self {
            protocol: CONTRACT_PROTOCOL.into(),
            methods,
        }
    }
    pub fn normalized(&self, limits: &Limits) -> Result<Self> {
        if self.protocol != CONTRACT_PROTOCOL {
            return Err(Status::protocol("unsupported RPC contract protocol"));
        }
        if self.methods.is_empty() {
            return Err(Status::new("invalid_argument", "RPC contract is empty"));
        }
        if self.methods.len() > limits.max_methods {
            return Err(Status::exhausted("RPC contract limit exceeded"));
        }
        let mut methods = self.methods.clone();
        for method in &methods {
            method.validate()?;
        }
        methods.sort();
        if methods.windows(2).any(|pair| pair[0] == pair[1]) {
            return Err(Status::new("invalid_argument", "duplicate RPC method"));
        }
        Ok(Self::new(methods))
    }
    pub fn check_support(&self, required: &Contract) -> Result<()> {
        let mut missing = None;
        for method in &required.methods {
            if self.methods.contains(method) {
                continue;
            }
            if self
                .methods
                .iter()
                .any(|available| available.id == method.id)
            {
                return Err(Status::new(
                    "failed_precondition",
                    format!("RPC method contract does not match: {}", method.id),
                ));
            }
            missing = Some(Status::new(
                "unimplemented",
                format!("RPC method is not implemented: {}", method.id),
            ));
        }
        missing.map_or(Ok(()), Err)
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct BindOptions {
    pub affinity_key: String,
    pub labels: BTreeMap<String, String>,
}
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct PeerInfo {
    pub identity: String,
    pub attributes: BTreeMap<String, String>,
}
#[derive(Clone, Debug)]
pub struct BindRequest {
    pub contract: Contract,
    pub options: BindOptions,
    pub peer: PeerInfo,
    pub provider: String,
    pub hops: i64,
}
impl BindRequest {
    pub fn new(contract: Contract) -> Self {
        Self {
            contract,
            options: BindOptions::default(),
            peer: PeerInfo::default(),
            provider: String::new(),
            hops: 0,
        }
    }
}

#[derive(Clone, Default)]
pub struct CallContext {
    pub cancellation: Cancellation,
    pub deadline: Option<Instant>,
    pub peer: PeerInfo,
    pub provider: String,
    pub(crate) resources: Option<std::sync::Arc<super::binding::CallResources>>,
}
impl CallContext {
    pub fn with_cancellation(cancellation: Cancellation) -> Self {
        Self {
            cancellation,
            ..Self::default()
        }
    }
    pub fn with_deadline(deadline: Instant) -> Self {
        Self {
            deadline: Some(deadline),
            ..Self::default()
        }
    }
    pub fn check(&self) -> Result<()> {
        if self.cancellation.is_cancelled() {
            return Err(Status::new("canceled", "RPC call canceled"));
        }
        if self
            .deadline
            .is_some_and(|deadline| deadline <= Instant::now())
        {
            return Err(Status::new("deadline_exceeded", "RPC deadline exceeded"));
        }
        Ok(())
    }
    pub async fn run<T>(&self, future: impl Future<Output = Result<T>>) -> Result<T> {
        self.check()?;
        let timeout = async {
            match self.deadline {
                Some(deadline) => super::platform::sleep_until(deadline).await,
                None => std::future::pending().await,
            }
        };
        tokio::select! {
            biased;
            _ = self.cancellation.cancelled() => Err(Status::new("canceled", "RPC call canceled")),
            _ = timeout => Err(Status::new("deadline_exceeded", "RPC deadline exceeded")),
            result = future => result,
        }
    }
}

#[derive(Clone, Debug)]
pub struct Limits {
    pub max_frame_bytes: usize,
    pub max_message_bytes: usize,
    pub max_in_flight_bytes: usize,
    pub max_bindings: usize,
    pub max_pending_calls: usize,
    pub max_pending_results: usize,
    pub max_pending_controls: usize,
    pub max_resources: usize,
    pub max_methods: usize,
    pub max_value_depth: usize,
    pub max_value_elements: usize,
}
impl Default for Limits {
    fn default() -> Self {
        Self {
            max_frame_bytes: 1 << 20,
            max_message_bytes: 64 << 20,
            max_in_flight_bytes: 128 << 20,
            max_bindings: 4096,
            max_pending_calls: 65536,
            max_pending_results: 65536,
            max_pending_controls: 4096,
            max_resources: 65536,
            max_methods: 4096,
            max_value_depth: 128,
            max_value_elements: 1_000_000,
        }
    }
}
