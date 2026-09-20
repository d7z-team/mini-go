//! Stable wait registrations and deduplicated owner-ready events.
use super::*;
use scheduler::{Blocked, Task};
use std::collections::BTreeSet;

pub(super) struct WaitingTask {
    pub task: Task,
    pub(super) dependencies: Vec<Handle>,
}

#[derive(Default)]
pub(super) struct Waiters {
    entries: BTreeMap<u64, WaitingTask>,
    resources: HashMap<Handle, BTreeSet<u64>>,
    calls: HashMap<u64, u64>,
    modules: HashMap<String, BTreeSet<u64>>,
    ready: BTreeSet<u64>,
    next: u64,
}

impl Waiters {
    pub fn select_peer(
        &self,
        resource: Handle,
        sending: bool,
    ) -> Option<(u64, usize, Option<Value>)> {
        for key in self.resources.get(&resource)? {
            let Some(Blocked::Select(selection)) = &self.entries[key].task.blocked else {
                continue;
            };
            if selection.outcome.is_some() {
                continue;
            }
            for (index, case) in selection.cases.iter().enumerate() {
                if case.send.is_some() != sending
                    && matches!(case.channel.data, Data::ResourceRef(handle) if handle == resource)
                {
                    return Some((*key, index, case.send.clone()));
                }
            }
        }
        None
    }

    pub fn complete_selection(&mut self, key: u64, outcome: select::SelectOutcome) {
        let waiting = self.entries.get_mut(&key).expect("live selection");
        let Some(Blocked::Select(selection)) = &mut waiting.task.blocked else {
            unreachable!()
        };
        assert!(selection.outcome.is_none(), "selection completed twice");
        selection.outcome = Some(outcome);
        for handle in waiting.dependencies.drain(..) {
            let keys = self.resources.get_mut(&handle).unwrap();
            keys.remove(&key);
            if keys.is_empty() {
                self.resources.remove(&handle);
            }
        }
        self.ready.insert(key);
    }
    pub fn len(&self) -> usize {
        self.entries.len()
    }
    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }
    pub fn iter(&self) -> impl Iterator<Item = &Task> {
        self.entries.values().map(|waiting| &waiting.task)
    }
    pub fn iter_mut(&mut self) -> impl Iterator<Item = &mut Task> {
        self.entries.values_mut().map(|waiting| &mut waiting.task)
    }
    pub fn clear(&mut self) {
        self.entries.clear();
        self.resources.clear();
        self.calls.clear();
        self.modules.clear();
        self.ready.clear();
    }
    pub fn push(&mut self, task: Task, dependencies: Vec<Handle>) {
        self.next += 1;
        let key = self.next;
        self.insert(key, WaitingTask { task, dependencies });
        self.ready.insert(key);
    }
    pub fn insert(&mut self, key: u64, waiting: WaitingTask) {
        for handle in &waiting.dependencies {
            self.resources.entry(*handle).or_default().insert(key);
        }
        if let Some(Blocked::Ffi(call)) = waiting.task.blocked {
            self.calls.insert(call, key);
        }
        if let Some(Blocked::Module(module)) = &waiting.task.blocked {
            self.modules.entry(module.clone()).or_default().insert(key);
        }
        self.entries.insert(key, waiting);
    }
    pub fn remove(&mut self, key: u64) -> WaitingTask {
        self.ready.remove(&key);
        let waiting = self.entries.remove(&key).expect("live wait registration");
        for handle in &waiting.dependencies {
            let keys = self.resources.get_mut(handle).unwrap();
            keys.remove(&key);
            if keys.is_empty() {
                self.resources.remove(handle);
            }
        }
        if let Some(Blocked::Ffi(call)) = waiting.task.blocked {
            self.calls.remove(&call);
        }
        if let Some(Blocked::Module(module)) = &waiting.task.blocked {
            let keys = self.modules.get_mut(module).unwrap();
            keys.remove(&key);
            if keys.is_empty() {
                self.modules.remove(module);
            }
        }
        waiting
    }
    pub fn next_ready(&mut self) -> Option<u64> {
        self.ready.pop_first()
    }
    pub fn notify_resource(&mut self, handle: Handle) {
        if let Some(keys) = self.resources.get(&handle) {
            self.ready.extend(keys);
        }
    }
    pub fn notify_call(&mut self, call: u64) {
        if let Some(key) = self.calls.get(&call) {
            self.ready.insert(*key);
        }
    }
    pub fn notify_module(&mut self, module: &str) {
        if let Some(keys) = self.modules.get(module) {
            self.ready.extend(keys);
        }
    }
    pub fn remove_scope(&mut self, scope: u64) -> Vec<Task> {
        let keys: Vec<_> = self
            .entries
            .iter()
            .filter(|(_, entry)| entry.task.scope == scope)
            .map(|(key, _)| *key)
            .collect();
        keys.into_iter().map(|key| self.remove(key).task).collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ready_events_are_deduplicated_ordered_and_detached_on_cancel() {
        let heap = Heap::new(4, 1024).unwrap();
        let resource = heap.allocate(Value::int(0), 16).unwrap();
        let unrelated = heap.allocate(Value::int(0), 16).unwrap();
        let mut waiters = Waiters::default();
        for id in 1..=128 {
            waiters.push(
                Task {
                    id,
                    scope: id,
                    frames: Vec::new(),
                    blocked: Some(Blocked::Ffi(id)),
                    ..Task::default()
                },
                vec![if id <= 2 { resource } else { unrelated }],
            );
        }
        while waiters.next_ready().is_some() {}
        // Idle owner boundaries consume no registrations, regardless of table size.
        for _ in 0..128 {
            assert!(waiters.next_ready().is_none());
        }
        waiters.notify_call(2);
        waiters.notify_resource(resource);
        waiters.notify_resource(resource);
        let first = waiters.next_ready().unwrap();
        assert_eq!(waiters.remove(first).task.id, 1);
        let second = waiters.next_ready().unwrap();
        assert_eq!(waiters.remove(second).task.id, 2);
        assert!(waiters.next_ready().is_none());
        waiters.notify_call(1);
        waiters.notify_resource(resource);
        assert!(waiters.next_ready().is_none());
        assert_eq!(waiters.remove_scope(3)[0].id, 3);
        waiters.notify_call(3);
        assert!(waiters.next_ready().is_none());
        waiters.clear();
        assert!(waiters.resources.is_empty());
        assert!(waiters.calls.is_empty());
    }
}
