use super::*;
use crate::executor_pool::{Next, Pool};
use std::task::Wake;

// A dropped future must never leave a mutation eligible for later confirmation.
struct RequestGuard<'a>(&'a mut CompilerSession);
impl Drop for RequestGuard<'_> {
    fn drop(&mut self) {
        if self.0.state != SessionState::Idle {
            self.0.abandon();
        }
    }
}

impl CompilerSession {
    pub async fn bundled() -> Result<Self, RuntimeError> {
        Self::new(include_bytes!("../../assets/compiler.json.gz")).await
    }
    pub async fn new(image: &[u8]) -> Result<Self, RuntimeError> {
        Self::new_with_timeout(image, Duration::from_secs(30)).await
    }
    /// Prepare and initialize a compiler within one shared timeout.
    pub async fn new_with_timeout(image: &[u8], timeout: Duration) -> Result<Self, RuntimeError> {
        Self::new_with_options(image, CompilerOptions::default(), timeout).await
    }
    /// Prepare with host budgets, preserved by recovery and upgrade.
    pub async fn new_with_options(
        image: &[u8],
        options: CompilerOptions,
        timeout: Duration,
    ) -> Result<Self, RuntimeError> {
        if timeout.is_zero() {
            return Err(failure("deadline", "compiler preparation deadline"));
        }
        let deadline = tokio::time::Instant::now()
            .checked_add(timeout)
            .filter(|_| u64::try_from(timeout.as_nanos()).is_ok())
            .ok_or_else(|| failure("invalid_argument", "request timeout is too large"))?;
        let mut session = tokio::time::timeout_at(
            deadline,
            Self::prepare_native(
                image,
                1,
                RestoreState::default(),
                options,
                Arc::new(SystemClock::default()),
            ),
        )
        .await
        .map_err(|_| failure("deadline", "compiler preparation deadline"))??;
        session
            .call_with_timeout(
                json!({"Operation":"hello"}),
                &Cancellation::default(),
                deadline.saturating_duration_since(tokio::time::Instant::now()),
            )
            .await?;
        Ok(session)
    }
    async fn prepare_native(
        image: &[u8],
        generation: u64,
        restore: RestoreState,
        options: CompilerOptions,
        clock: Arc<dyn Clock>,
    ) -> Result<Self, RuntimeError> {
        if image.len() > options.load.max_image_bytes {
            return Err(failure("load_limit", "compiler image too large"));
        }
        let image = image.to_vec();
        let (send, receive) = tokio::sync::oneshot::channel();
        let work = Mutex::new(Some((image, restore, send)));
        let job = Pool::shared()?.job(move || {
            if let Some((image, restore, send)) = work.lock().unwrap().take()
                && !send.is_closed()
            {
                let result = Self::with_clock(&image, generation, restore, options, clock.clone());
                let _ = send.send(result);
            }
            Next::Done
        })?;
        job.wake_by_ref();
        receive
            .await
            .map_err(|_| failure("closed", "compiler preparation interrupted"))?
    }
    pub async fn call(
        &mut self,
        request: Value,
        cancel: &Cancellation,
    ) -> Result<Value, RuntimeError> {
        self.call_with_timeout(request, cancel, Duration::from_secs(30))
            .await
    }
    pub async fn call_with_timeout(
        &mut self,
        request: Value,
        cancel: &Cancellation,
        timeout: Duration,
    ) -> Result<Value, RuntimeError> {
        if cancel.is_cancelled() {
            return Err(failure("canceled", "request canceled"));
        }
        if self.state == SessionState::Discarded {
            self.set_generation(
                self.generation_high_watermark
                    .checked_add(1)
                    .ok_or_else(|| failure("budget", "instance generations exhausted"))?,
            )?;
        }
        self.start(request, timeout)?;
        let guard = RequestGuard(self);
        loop {
            if cancel.is_cancelled() {
                guard.0.cancel();
            }
            match guard.0.poll(4096)? {
                CompilerPoll::Running => tokio::task::yield_now().await,
                CompilerPoll::Pending => tokio::time::sleep(Duration::from_millis(1)).await,
                CompilerPoll::Ready(reply) => {
                    if cancel.is_cancelled() {
                        return Err(failure("canceled", "request canceled before delivery"));
                    }
                    guard.0.acknowledge()?;
                    return Ok(reply.value);
                }
            }
        }
    }
    pub async fn upgrade(
        &mut self,
        image: &[u8],
        cancel: &Cancellation,
    ) -> Result<UpgradeResult, RuntimeError> {
        self.upgrade_with_timeout(image, cancel, Duration::from_secs(30))
            .await
    }
    /// Prepare and restore a replacement within one shared timeout.
    pub async fn upgrade_with_timeout(
        &mut self,
        image: &[u8],
        cancel: &Cancellation,
        timeout: Duration,
    ) -> Result<UpgradeResult, RuntimeError> {
        if !matches!(self.state, SessionState::Idle | SessionState::Discarded) {
            return Err(failure("busy", "session unavailable for upgrade"));
        }
        if cancel.is_cancelled() {
            return Err(failure("canceled", "upgrade canceled"));
        }
        if timeout.is_zero() {
            return Err(failure("deadline", "upgrade preparation deadline"));
        }
        let deadline = tokio::time::Instant::now()
            .checked_add(timeout)
            .filter(|_| u64::try_from(timeout.as_nanos()).is_ok())
            .ok_or_else(|| failure("invalid_argument", "request timeout is too large"))?;
        let generation = self
            .generation_high_watermark
            .checked_add(1)
            .ok_or_else(|| failure("budget", "instance generations exhausted"))?;
        self.generation_high_watermark = generation;
        let mut candidate = tokio::select! {
            _ = cancel.cancelled() => return Err(failure("canceled", "upgrade canceled")),
            result = tokio::time::timeout_at(deadline, Self::prepare_native(image, generation, self.confirmed.clone(), self.options, self.clock.clone())) =>
                result.map_err(|_| failure("deadline", "upgrade preparation deadline"))??,
        };
        // Initial polling restores and analyzes confirmed inputs before hello is delivered.
        candidate
            .call_with_timeout(
                json!({"Operation":"hello"}),
                cancel,
                deadline.saturating_duration_since(tokio::time::Instant::now()),
            )
            .await?;
        if cancel.is_cancelled() {
            return Err(failure("canceled", "upgrade canceled before delivery"));
        }
        let mut previous = std::mem::replace(self, candidate);
        // The new session is committed. Closing the old owner cannot roll it back.
        Ok(UpgradeResult {
            cleanup_error: previous.close_now().err(),
        })
    }
    pub async fn close(&mut self) -> Result<(), RuntimeError> {
        self.close_now()
    }
}
