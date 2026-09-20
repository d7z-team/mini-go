//! Owner snapshots of instance resources and invocation work.

use super::*;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum InstanceState {
    Open,
    Faulted,
    Closing,
    Closed,
}

#[derive(Clone, Debug)]
pub struct Stats {
    pub revision: patch::RevisionInfo,
    pub state: InstanceState,
    pub active_scopes: usize,
    pub runnable_tasks: usize,
    pub blocked_tasks: usize,
    pub paused_tasks: usize,
    pub pending_ffi_calls: usize,
    pub pending_boundary_bytes: usize,
    pub timers: usize,
    pub retained_revisions: usize,
    pub dynamic_types: usize,
    pub dynamic_type_bytes: u64,
    pub executed_steps: u64,
    pub heap: HeapStats,
    pub memory: memory::MemoryStats,
}

#[derive(Clone, Copy, Debug, Default)]
pub(crate) struct ScopeWork {
    pub tasks: usize,
    pub timers: usize,
    pub ffi_calls: usize,
    pub steps: u64,
}

impl Instance {
    pub fn state(&self) -> InstanceState {
        if self.closed {
            if self.cleanup_result.is_some() {
                InstanceState::Closed
            } else {
                InstanceState::Closing
            }
        } else if self.faulted {
            InstanceState::Faulted
        } else {
            InstanceState::Open
        }
    }

    pub fn stats(&self) -> Stats {
        Stats {
            revision: self.revision(),
            state: self.state(),
            active_scopes: self
                .scope_work
                .values()
                .filter(|work| work.tasks != 0 || work.timers != 0)
                .count(),
            runnable_tasks: self.runnable.len()
                + usize::from(!self.running.frames.is_empty() && !self.debug.paused),
            blocked_tasks: self.blocked.len(),
            paused_tasks: usize::from(self.debug.paused && !self.running.frames.is_empty()),
            pending_ffi_calls: self.ffi_calls.pending_count(),
            pending_boundary_bytes: self.ffi_calls.reserved_bytes(),
            timers: self.timers.len(),
            retained_revisions: self
                .retired_revisions
                .values()
                .filter(|revision| revision.strong_count() != 0)
                .count()
                + usize::from(!self.closed),
            executed_steps: self.steps,
            dynamic_types: self.types.dynamic_stats().0,
            dynamic_type_bytes: self.types.dynamic_stats().1,
            heap: self.heap.stats(),
            memory: self.memory.stats(),
        }
    }

    pub(crate) fn scope_work(&self, scope: u64) -> ScopeWork {
        let mut work = self.scope_work.get(&scope).copied().unwrap_or_default();
        work.steps = self
            .scope_steps
            .get(&scope)
            .map_or(0, |budget| budget.executed());
        work
    }
}
