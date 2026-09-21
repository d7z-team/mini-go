//! Shared RPC endpoint and transport ownership for VM and host-side bindings.

use crate::{RpcOptions, error};
use mini_go::rpc::{self, MessageConn};
use std::sync::Arc;
use wasm_bindgen::JsValue;

pub(crate) struct RpcNetwork {
    transport: Arc<crate::transport::Transport>,
    pub(crate) endpoint: Arc<rpc::Endpoint>,
    pub(crate) router: Arc<rpc::router::Router>,
}

impl RpcNetwork {
    pub(crate) fn open(options: &RpcOptions) -> Result<Arc<Self>, JsValue> {
        options
            .validate()
            .map_err(|failure| error(rpc::Status::new("invalid_argument", failure)))?;
        let transport = crate::transport::Transport::new();
        let mut endpoint_options = rpc::EndpointOptions::default();
        endpoint_options.limits.max_frame_bytes = 1 << 20;
        if let Some(milliseconds) = options.lease_ttl_ms {
            endpoint_options.lease_ttl = std::time::Duration::from_millis(milliseconds);
        }
        if let Some(milliseconds) = options.admission_timeout_ms {
            endpoint_options.admission_timeout = std::time::Duration::from_millis(milliseconds);
        }
        endpoint_options.max_call_duration = options
            .max_call_duration_ms
            .map(std::time::Duration::from_millis);
        let router = Arc::new(rpc::router::Router::new(
            rpc::platform::Handle,
            Default::default(),
            None,
        ));
        let endpoint = rpc::Endpoint::open(
            rpc::platform::Handle,
            transport.clone(),
            Some(router.clone()),
            endpoint_options,
        )
        .map_err(error)?;
        Ok(Arc::new(Self {
            transport,
            endpoint,
            router,
        }))
    }

    pub(crate) fn receive(&self, frame: &[u8]) -> Result<(), JsValue> {
        self.transport.receive(frame.to_vec()).map_err(error)
    }

    pub(crate) fn outgoing(&self) -> Vec<crate::transport::Outbound> {
        self.transport.drain()
    }

    pub(crate) fn sent(&self, id: u32) {
        self.transport.sent(id);
    }

    pub(crate) fn disconnect(&self) {
        self.transport.close();
    }

    pub(crate) async fn shutdown(&self) -> rpc::Result<()> {
        let endpoint = self.endpoint.shutdown().await;
        let router = self.router.force_shutdown().await;
        endpoint.and(router)
    }
}
