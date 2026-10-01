use crate::{encode, encode_stats, error};
use mini_go::compiler::{CompilerOptions, CompilerPoll, CompilerSession, RestoreState};
use std::time::Duration;
use wasm_bindgen::prelude::*;

#[wasm_bindgen]
pub struct WasmCompiler(CompilerSession);

#[derive(Default, serde::Deserialize)]
#[serde(default, deny_unknown_fields)]
struct Options {
    limits: Option<crate::limits::ExecutionOptions>,
    load: Option<crate::limits::ImageOptions>,
}

#[wasm_bindgen]
impl WasmCompiler {
    #[wasm_bindgen(constructor)]
    pub fn new(
        image: &[u8],
        generation: u64,
        restore: &[u8],
        options: JsValue,
    ) -> Result<WasmCompiler, JsValue> {
        let options: Options = serde_wasm_bindgen::from_value(options).map_err(error)?;
        let defaults = CompilerOptions::default();
        let options = CompilerOptions {
            limits: options
                .limits
                .unwrap_or_default()
                .apply(defaults.limits)
                .map_err(error)?,
            load: options
                .load
                .unwrap_or_default()
                .apply(defaults.load)
                .map_err(error)?,
        };
        let restore = if restore.is_empty() {
            RestoreState::default()
        } else {
            RestoreState::decode(restore).map_err(error)?
        };
        Ok(Self(
            CompilerSession::from_image(image, generation, restore, options).map_err(error)?,
        ))
    }
    pub fn start(&mut self, request: &str, timeout_ms: u32) -> Result<(), JsValue> {
        let input = serde_json::from_str(request).map_err(error)?;
        self.0
            .start(input, Duration::from_millis(u64::from(timeout_ms)))
            .map_err(error)?;
        Ok(())
    }
    pub fn poll(&mut self, steps: usize) -> Result<JsValue, JsValue> {
        match self.0.poll(steps).map_err(error)? {
            CompilerPoll::Running => Ok("running".into()),
            CompilerPoll::Pending => Ok("pending".into()),
            CompilerPoll::Ready(reply) => {
                #[derive(serde::Serialize)]
                struct Delivery {
                    value: String,
                    #[serde(with = "serde_bytes")]
                    restore: Vec<u8>,
                }
                encode(&Delivery {
                    value: serde_json::to_string(&reply.value).map_err(error)?,
                    restore: reply.restore.encode().map_err(error)?,
                })
            }
        }
    }
    pub fn acknowledge(&mut self) -> Result<(), JsValue> {
        self.0.acknowledge().map_err(error)
    }
    pub fn cancel(&mut self) {
        self.0.cancel();
    }
    pub fn reusable(&self) -> bool {
        self.0.state() == mini_go::compiler::SessionState::Idle && self.0.stats().is_some()
    }
    pub fn close(&mut self) -> Result<(), JsValue> {
        self.0.close_now().map_err(error)
    }
    pub fn stats(&self) -> Result<JsValue, JsValue> {
        encode_stats(
            self.0
                .stats()
                .ok_or_else(|| error("compiler unavailable"))?,
        )
    }
}
