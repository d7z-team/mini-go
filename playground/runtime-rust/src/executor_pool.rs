//! Bounded workers execute finite slices. Waiting jobs retain continuations,
//! never a worker thread; notifications during execution survive handoff.

use crate::RuntimeError;
use std::{
    collections::{HashMap, VecDeque},
    sync::{Arc, Condvar, Mutex, OnceLock, Weak},
    thread::JoinHandle,
    time::{Duration, Instant},
};

pub(crate) enum Next {
    Ready,
    Idle,
    After(Duration),
    Done,
}

#[derive(Default)]
struct JobState {
    queued: bool,
    running: bool,
    woken: bool,
    stopped: bool,
}

pub(crate) struct Job {
    id: u64,
    pool: Arc<Pool>,
    state: Mutex<JobState>,
    run: Box<dyn Fn() -> Next + Send + Sync>,
}

#[derive(Default)]
struct State {
    ready: VecDeque<Arc<Job>>,
    timers: HashMap<u64, (Instant, Weak<Job>)>,
    next: u64,
    closed: bool,
}

pub(crate) struct Pool {
    state: Mutex<State>,
    changed: Condvar,
    workers: Mutex<Vec<JoinHandle<()>>>,
    worker_count: usize,
}

static SHARED: OnceLock<Result<Arc<Pool>, RuntimeError>> = OnceLock::new();

impl Pool {
    pub fn shared() -> Result<Arc<Self>, RuntimeError> {
        SHARED
            .get_or_init(|| Self::new(std::thread::available_parallelism().map_or(1, usize::from)))
            .clone()
    }

    pub(crate) fn new(workers: usize) -> Result<Arc<Self>, RuntimeError> {
        if workers == 0 {
            return Err(RuntimeError::new(
                "executor",
                "workers",
                "expected positive worker count",
            ));
        }
        let pool = Arc::new(Self {
            state: Mutex::new(State::default()),
            changed: Condvar::new(),
            workers: Mutex::new(Vec::new()),
            worker_count: workers,
        });
        for _ in 0..workers {
            let owner = pool.clone();
            match std::thread::Builder::new()
                .name("mini-go-worker".into())
                .spawn(move || owner.work())
            {
                Ok(worker) => pool.workers.lock().unwrap().push(worker),
                Err(error) => {
                    pool.shutdown();
                    return Err(RuntimeError::new("executor", "workers", error.to_string()));
                }
            }
        }
        Ok(pool)
    }

    pub(crate) fn worker_count(&self) -> usize {
        self.worker_count
    }

    pub fn job(
        self: &Arc<Self>,
        run: impl Fn() -> Next + Send + Sync + 'static,
    ) -> Result<Arc<Job>, RuntimeError> {
        let mut state = self.state.lock().unwrap();
        if state.closed {
            return Err(RuntimeError::new(
                "closed",
                "executor",
                "executor is closed",
            ));
        }
        let id = state
            .next
            .checked_add(1)
            .ok_or_else(|| RuntimeError::new("executor", "job", "job identity exhausted"))?;
        state.next = id;
        Ok(Arc::new(Job {
            id,
            pool: self.clone(),
            state: Mutex::default(),
            run: Box::new(run),
        }))
    }

    fn work(&self) {
        loop {
            let mut state = self.state.lock().unwrap();
            let job = loop {
                if state.closed {
                    return;
                }
                let now = Instant::now();
                let mut due = Vec::new();
                state.timers.retain(|_, (deadline, job)| {
                    if *deadline <= now {
                        if let Some(job) = job.upgrade() {
                            due.push(job);
                        }
                        false
                    } else {
                        job.strong_count() != 0
                    }
                });
                for job in due {
                    let mut status = job.state.lock().unwrap();
                    if !status.stopped && !status.queued {
                        status.queued = true;
                        state.ready.push_back(job.clone());
                    }
                }
                if let Some(job) = state.ready.pop_front() {
                    let mut status = job.state.lock().unwrap();
                    status.queued = false;
                    if status.stopped {
                        continue;
                    }
                    status.running = true;
                    drop(status);
                    break job;
                }
                state = if let Some(deadline) =
                    state.timers.values().map(|(deadline, _)| *deadline).min()
                {
                    self.changed
                        .wait_timeout(state, deadline.saturating_duration_since(now))
                        .unwrap()
                        .0
                } else {
                    self.changed.wait(state).unwrap()
                };
            };
            drop(state);
            let next = (job.run)();
            let mut state = self.state.lock().unwrap();
            let mut status = job.state.lock().unwrap();
            status.running = false;
            status.stopped |= matches!(next, Next::Done);
            if !state.closed && !status.stopped {
                if status.woken || matches!(next, Next::Ready) {
                    status.queued = true;
                    state.ready.push_back(job.clone());
                    self.changed.notify_one();
                } else if let Next::After(delay) = next {
                    // Long timers are rechecked periodically without changing
                    // their VM deadline. This also bounds platform wait ranges.
                    let deadline = Instant::now() + delay.min(Duration::from_secs(86_400));
                    state
                        .timers
                        .insert(job.id, (deadline, Arc::downgrade(&job)));
                    self.changed.notify_all();
                }
            }
            status.woken = false;
        }
    }

    // External owners first request cancellation, then join finite slices.
    pub(crate) fn shutdown(&self) {
        {
            let mut state = self.state.lock().unwrap();
            state.closed = true;
            state.ready.clear();
            state.timers.clear();
            self.changed.notify_all();
        }
        for worker in self.workers.lock().unwrap().drain(..) {
            let _ = worker.join();
        }
    }
}

impl std::task::Wake for Job {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }
    fn wake_by_ref(self: &Arc<Self>) {
        let mut pool = self.pool.state.lock().unwrap();
        let mut state = self.state.lock().unwrap();
        if pool.closed || state.stopped {
            return;
        }
        pool.timers.remove(&self.id);
        if state.running {
            state.woken = true;
        } else if !state.queued {
            state.queued = true;
            pool.ready.push_back(self.clone());
            self.pool.changed.notify_one();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        sync::{
            atomic::{AtomicBool, AtomicUsize, Ordering},
            mpsc,
        },
        task::Wake,
    };

    #[test]
    fn one_worker_resumes_a_peer_after_returning_its_slice() {
        let pool = Pool::new(1).unwrap();
        let ready = Arc::new(AtomicUsize::new(0));
        let (done, result) = mpsc::channel();
        let state = ready.clone();
        let waiting = pool
            .job(move || {
                if state.load(Ordering::Acquire) == 1 {
                    done.send(()).unwrap();
                    Next::Done
                } else {
                    Next::Idle
                }
            })
            .unwrap();
        waiting.wake_by_ref();
        let producer = pool
            .job(move || {
                ready.store(1, Ordering::Release);
                waiting.wake_by_ref();
                Next::Done
            })
            .unwrap();
        producer.wake_by_ref();
        let result = result.recv_timeout(Duration::from_secs(5));
        pool.shutdown();
        result.unwrap();
    }

    #[test]
    fn notifications_before_handoff_are_deduplicated_and_not_lost() {
        let pool = Pool::new(2).unwrap();
        let (entered, entry) = mpsc::channel();
        let (release, gate) = mpsc::channel();
        let gate = Mutex::new(gate);
        let (done, result) = mpsc::channel();
        let calls = Arc::new(AtomicUsize::new(0));
        let count = calls.clone();
        let active = Arc::new(AtomicUsize::new(0));
        let running = active.clone();
        let overlap = Arc::new(AtomicBool::new(false));
        let overlapping = overlap.clone();
        let job = pool
            .job(move || {
                if running.fetch_add(1, Ordering::SeqCst) != 0 {
                    overlapping.store(true, Ordering::SeqCst);
                }
                let next = if count.fetch_add(1, Ordering::SeqCst) == 0 {
                    entered.send(()).unwrap();
                    gate.lock().unwrap().recv().unwrap();
                    Next::Idle
                } else {
                    done.send(()).unwrap();
                    Next::Done
                };
                running.fetch_sub(1, Ordering::SeqCst);
                next
            })
            .unwrap();
        job.wake_by_ref();
        let entered = entry.recv_timeout(Duration::from_secs(5));
        for _ in 0..100 {
            job.wake_by_ref();
        }
        release.send(()).unwrap();
        let result = result.recv_timeout(Duration::from_secs(5));
        pool.shutdown();
        entered.unwrap();
        result.unwrap();
        assert_eq!(calls.load(Ordering::SeqCst), 2);
        assert_eq!(active.load(Ordering::SeqCst), 0);
        assert!(!overlap.load(Ordering::SeqCst));
    }

    #[test]
    fn long_timer_relinquishes_worker_and_notification_cancels_the_wait() {
        let pool = Pool::new(1).unwrap();
        let (done, result) = mpsc::channel();
        let calls = AtomicUsize::new(0);
        let job = pool
            .job(move || {
                if calls.fetch_add(1, Ordering::SeqCst) == 0 {
                    Next::After(Duration::MAX)
                } else {
                    done.send(()).unwrap();
                    Next::Done
                }
            })
            .unwrap();
        job.wake_by_ref();
        let state = pool.state.lock().unwrap();
        let (state, timeout) = pool
            .changed
            .wait_timeout_while(state, Duration::from_secs(5), |state| {
                !state.timers.contains_key(&job.id)
            })
            .unwrap();
        let armed = !timeout.timed_out()
            && state
                .timers
                .get(&job.id)
                .is_some_and(|(deadline, _)| *deadline > Instant::now());
        drop(state);
        job.wake_by_ref();
        let delivered = result.recv_timeout(Duration::from_secs(5));
        pool.shutdown();
        assert!(armed);
        delivered.unwrap();
        assert!(pool.state.lock().unwrap().timers.is_empty());
    }
}
