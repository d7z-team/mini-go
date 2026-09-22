use crate::compiler::CompilerSession;
#[cfg(not(target_arch = "wasm32"))]
use crate::{error::RuntimeError, ffi::Cancellation};
#[cfg(not(target_arch = "wasm32"))]
use serde_json::{Value, json};

mod sources;
pub use sources::*;

pub struct LanguageService {
    pub session: CompilerSession,
}
#[cfg(not(target_arch = "wasm32"))]
impl LanguageService {
    pub async fn new(image: &[u8]) -> Result<Self, RuntimeError> {
        Ok(Self {
            session: CompilerSession::new(image).await?,
        })
    }
    pub async fn sources(
        &mut self,
        trees: &[SourceTree],
        cancel: &Cancellation,
    ) -> Result<SourcePackages, RuntimeError> {
        let mut reply = self
            .session
            .call(
                json!({"Operation": "workspace/sources", "Trees": trees}),
                cancel,
            )
            .await?;
        serde_json::from_value(reply["Value"].take())
            .map_err(|e| RuntimeError::new("invalid_argument", "sources", e.to_string()))
    }
    pub async fn open(
        &mut self,
        mut workspace: Value,
        cancel: &Cancellation,
    ) -> Result<Value, RuntimeError> {
        workspace["Operation"] = json!("workspace/open");
        self.session.call(workspace, cancel).await
    }
    pub async fn update(
        &mut self,
        changes: Value,
        cancel: &Cancellation,
    ) -> Result<Value, RuntimeError> {
        self.session
            .call(
                json!({"Operation":"document/update","Changes":changes}),
                cancel,
            )
            .await
    }
    pub async fn analyze(&mut self, cancel: &Cancellation) -> Result<Value, RuntimeError> {
        self.session
            .call(json!({"Operation":"workspace/analyze"}), cancel)
            .await
    }
    pub async fn replace_workspace(
        &mut self,
        mut workspace: Value,
        cancel: &Cancellation,
    ) -> Result<Value, RuntimeError> {
        workspace["Operation"] = json!("workspace/update");
        self.session.call(workspace, cancel).await
    }
    pub async fn prepare(
        &mut self,
        options: Value,
        cancel: &Cancellation,
    ) -> Result<Value, RuntimeError> {
        self.session
            .call(
                json!({"Operation":"build/prepare", "Build":options}),
                cancel,
            )
            .await
    }
    pub async fn upgrade(
        &mut self,
        image: &[u8],
        cancel: &Cancellation,
    ) -> Result<crate::compiler::UpgradeResult, RuntimeError> {
        self.session.upgrade(image, cancel).await
    }
    pub async fn query(
        &mut self,
        operation: &str,
        parameters: Value,
        cancel: &Cancellation,
    ) -> Result<Value, RuntimeError> {
        Ok(self
            .session
            .call(
                json!({"Operation":format!("language/{operation}"),"Query":parameters}),
                cancel,
            )
            .await?["Value"]
            .take())
    }
    pub async fn close(&mut self) -> Result<(), RuntimeError> {
        self.session.close().await
    }
}
