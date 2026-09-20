//! Shared instruction allowance. A grant owns capacity, not executed steps.

use crate::RuntimeError;
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicU64, Ordering},
};

#[derive(Default)]
pub(super) struct StepBudget {
    executed: AtomicU64,
    state: Mutex<BudgetState>,
    wake: std::sync::Weak<crate::ffi::Wake>,
}

#[derive(Default)]
struct BudgetState {
    committed: u64,
    reserved: u64,
    waiting: bool,
}

pub(super) struct StepGrant {
    budget: Arc<StepBudget>,
    capacity: u64,
    pub remaining: u64,
}

impl StepBudget {
    pub fn new(wake: &Arc<crate::ffi::Wake>) -> Self {
        Self {
            wake: Arc::downgrade(wake),
            ..Self::default()
        }
    }

    pub fn executed(&self) -> u64 {
        self.executed.load(Ordering::Relaxed)
    }

    pub fn reserve(
        self: &Arc<Self>,
        limit: i64,
        quantum: u64,
    ) -> Result<Option<StepGrant>, RuntimeError> {
        let mut state = self.state.lock().unwrap();
        let capacity = if limit > 0 {
            let limit = limit as u64;
            if state.committed >= limit {
                return Err(RuntimeError::new(
                    "step_limit",
                    "instance",
                    "instruction budget exhausted",
                ));
            }
            quantum.min(limit - state.committed - state.reserved)
        } else {
            quantum
        };
        if capacity == 0 {
            state.waiting = true;
            return Ok(None);
        }
        state.reserved += capacity;
        Ok(Some(StepGrant {
            budget: self.clone(),
            capacity,
            remaining: capacity,
        }))
    }

    #[cfg(test)]
    pub fn with_executed(executed: u64) -> Self {
        Self {
            executed: AtomicU64::new(executed),
            state: Mutex::new(BudgetState {
                committed: executed,
                reserved: 0,
                waiting: false,
            }),
            wake: Default::default(),
        }
    }
}

impl StepGrant {
    pub fn consume(&mut self) {
        assert!(self.remaining != 0);
        self.remaining -= 1;
        let _ = self
            .budget
            .executed
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |steps| {
                Some(steps.saturating_add(1))
            });
    }
}

impl Drop for StepGrant {
    fn drop(&mut self) {
        let mut state = self.budget.state.lock().unwrap();
        state.reserved -= self.capacity;
        state.committed = state
            .committed
            .saturating_add(self.capacity - self.remaining);
        let wake = std::mem::take(&mut state.waiting);
        drop(state);
        if wake && let Some(wake) = self.budget.wake.upgrade() {
            wake.signal();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unused_grant_is_returned_and_reserved_allowance_is_not_exhaustion() {
        let budget = Arc::new(StepBudget::default());
        let mut grant = budget.reserve(4, 64).unwrap().unwrap();
        grant.consume();
        assert!(budget.reserve(4, 64).unwrap().is_none());
        assert_eq!(budget.executed(), 1);
        drop(grant);
        let mut grant = budget.reserve(4, 64).unwrap().unwrap();
        assert_eq!(grant.remaining, 3);
        for _ in 0..3 {
            grant.consume();
        }
        drop(grant);
        assert_eq!(budget.reserve(4, 64).err().unwrap().code, "step_limit");
        assert_eq!(budget.executed(), 4);
    }

    #[test]
    fn returning_a_grant_wakes_waiters_for_capacity_or_final_exhaustion() {
        for consumed in [1, 4] {
            let wake = Arc::new(crate::ffi::Wake::default());
            let budget = Arc::new(StepBudget::new(&wake));
            let mut grant = budget.reserve(4, 64).unwrap().unwrap();
            for _ in 0..consumed {
                grant.consume();
            }
            let observed = wake.epoch();
            assert!(budget.reserve(4, 64).unwrap().is_none());
            drop(grant);
            assert_ne!(wake.epoch(), observed);
            if consumed == 4 {
                assert_eq!(budget.reserve(4, 64).err().unwrap().code, "step_limit");
            } else {
                assert_eq!(budget.reserve(4, 64).unwrap().unwrap().remaining, 3);
            }
        }
    }

    #[test]
    fn concurrent_quanta_share_one_exact_step_limit() {
        for workers in [1, 2, 4] {
            let budget = Arc::new(StepBudget::default());
            std::thread::scope(|scope| {
                for _ in 0..workers {
                    let budget = budget.clone();
                    scope.spawn(move || {
                        loop {
                            match budget.reserve(10003, 64) {
                                Ok(Some(mut grant)) => {
                                    while grant.remaining != 0 {
                                        grant.consume();
                                    }
                                }
                                Ok(None) => std::thread::yield_now(),
                                Err(error) => {
                                    assert_eq!(error.code, "step_limit");
                                    break;
                                }
                            }
                        }
                    });
                }
            });
            assert_eq!(budget.executed(), 10003);
            let state = budget.state.lock().unwrap();
            assert_eq!(state.committed, 10003);
            assert_eq!(state.reserved, 0);
        }
    }
}
