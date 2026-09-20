//! Deletable deadline heap; channel handles are stable timer identities.
use super::*;
pub(super) struct Timer {
    pub channel: Value,
    pub scope: u64,
    pub(super) deadline: u64,
    pub(super) order: u64,
    pub(super) period: u64,
}

#[derive(Default)]
pub(super) struct TimerQueue {
    entries: Vec<Timer>,
    positions: HashMap<Handle, usize>,
    next: u64,
}

impl Timer {
    fn id(&self) -> Handle {
        let Data::ResourceRef(handle) = self.channel.data else {
            unreachable!("timer channel")
        };
        handle
    }
}

impl TimerQueue {
    pub fn len(&self) -> usize {
        self.entries.len()
    }
    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }
    pub fn iter(&self) -> std::slice::Iter<'_, Timer> {
        self.entries.iter()
    }
    pub fn clear(&mut self) {
        self.entries.clear();
        self.positions.clear();
    }
    pub fn deadline(&self) -> Option<u64> {
        self.entries.first().map(|timer| timer.deadline)
    }

    fn swap(&mut self, left: usize, right: usize) {
        self.entries.swap(left, right);
        self.positions.insert(self.entries[left].id(), left);
        self.positions.insert(self.entries[right].id(), right);
    }

    fn repair(&mut self, mut index: usize) {
        while index > 0 {
            let parent = (index - 1) / 2;
            if (self.entries[parent].deadline, self.entries[parent].order)
                <= (self.entries[index].deadline, self.entries[index].order)
            {
                break;
            }
            self.swap(parent, index);
            index = parent;
        }
        loop {
            let left = index * 2 + 1;
            if left >= self.entries.len() {
                break;
            }
            let right = left + 1;
            let child = if right < self.entries.len()
                && (self.entries[right].deadline, self.entries[right].order)
                    < (self.entries[left].deadline, self.entries[left].order)
            {
                right
            } else {
                left
            };
            if (self.entries[index].deadline, self.entries[index].order)
                <= (self.entries[child].deadline, self.entries[child].order)
            {
                break;
            }
            self.swap(index, child);
            index = child;
        }
    }

    pub fn remove(&mut self, handle: Handle) -> Option<Timer> {
        let index = self.positions.remove(&handle)?;
        let timer = self.entries.swap_remove(index);
        if index < self.entries.len() {
            self.positions.insert(self.entries[index].id(), index);
            self.repair(index);
        }
        Some(timer)
    }

    pub fn pop(&mut self) -> Option<Timer> {
        let id = self.entries.first()?.id();
        self.remove(id)
    }

    pub fn push(&mut self, mut timer: Timer) {
        self.remove(timer.id());
        self.next += 1;
        timer.order = self.next;
        let index = self.entries.len();
        self.positions.insert(timer.id(), index);
        self.entries.push(timer);
        self.repair(index);
    }

    pub fn retain(&mut self, mut keep: impl FnMut(&Timer) -> bool) {
        let remove: Vec<_> = self
            .entries
            .iter()
            .filter(|timer| !keep(timer))
            .map(Timer::id)
            .collect();
        for id in remove {
            self.remove(id);
        }
    }
}

impl<'a> IntoIterator for &'a TimerQueue {
    type Item = &'a Timer;
    type IntoIter = std::slice::Iter<'a, Timer>;
    fn into_iter(self) -> Self::IntoIter {
        self.iter()
    }
}

impl Instance {
    pub fn next_timer_delay(&self) -> Option<std::time::Duration> {
        self.timers.deadline().map(|deadline| {
            std::time::Duration::from_nanos(deadline.saturating_sub(self.clock.monotonic_ns()))
        })
    }
    pub(super) fn deliver_timers(&mut self) -> Result<(), RuntimeError> {
        if self.timers.is_empty() {
            return Ok(());
        }
        let now = self.clock.monotonic_ns();
        let mut expired = Vec::new();
        while self
            .timers
            .deadline()
            .is_some_and(|deadline| deadline <= now)
        {
            let timer = self.timers.pop().unwrap();
            timer
                .channel
                .trace(&mut |handle| self.running.transient_roots.push(handle));
            expired.push(timer);
        }
        for mut timer in expired {
            self.try_send(&timer.channel, Value::boolean(true))?;
            if let Some(missed) = (now - timer.deadline).checked_div(timer.period) {
                let periods = missed.saturating_add(1);
                timer.deadline = timer
                    .deadline
                    .saturating_add(timer.period.saturating_mul(periods));
                self.timers.push(timer);
            } else {
                self.scope_work.entry(timer.scope).or_default().timers -= 1;
                self.changed_scopes.insert(timer.scope);
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deadlines_remain_ordered_after_restart_and_scope_removal() {
        let heap = Heap::new(16, 4096).unwrap();
        let mut timers = TimerQueue::default();
        let mut handles = Vec::new();
        for (scope, deadline) in [40, 10, 30, 10, 50, 20].into_iter().enumerate() {
            let handle = heap.allocate(Value::int(0), 16).unwrap();
            handles.push(handle);
            timers.push(Timer {
                channel: Value {
                    typ: TypeIdentity::Any,
                    data: Data::ResourceRef(handle),
                },
                scope: scope as u64,
                deadline,
                period: 0,
                order: 0,
            });
        }
        assert_eq!(timers.pop().unwrap().id(), handles[1]);
        assert_eq!(timers.pop().unwrap().id(), handles[3]);
        let mut restarted = timers.remove(handles[4]).unwrap();
        restarted.deadline = 5;
        timers.push(restarted);
        timers.retain(|timer| timer.scope != 5);
        assert_eq!(timers.deadline(), Some(5));
        for index in [4, 2, 0] {
            assert_eq!(timers.pop().unwrap().id(), handles[index]);
        }
        assert!(timers.deadline().is_none());
        assert!(timers.positions.is_empty());
    }
}
