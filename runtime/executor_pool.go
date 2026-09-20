package runtime

import (
	"context"
	"errors"
	goruntime "runtime"
	"sync"
)

var sharedExecutionPool = sync.OnceValue(func() *executionPool {
	pool, _ := newExecutionPool(goruntime.GOMAXPROCS(0))
	return pool
})

var defaultExecutor = sync.OnceValue(func() *Executor {
	return &Executor{pool: sharedExecutionPool(), workers: goruntime.GOMAXPROCS(0), shared: true}
})

// Executor is a bounded worker pool that may be shared by multiple instances.
// Instances never own one goroutine per guest task. A host-created executor
// remains owned by the host and must be shut down after all attached instances.
type Executor struct {
	pool      *executionPool
	workers   int
	shared    bool
	mu        sync.Mutex
	closing   bool
	instances map[*Instance]struct{}
}

func NewExecutor(workers int) (*Executor, error) {
	pool, err := newExecutionPool(workers)
	if err != nil {
		return nil, err
	}
	return &Executor{pool: pool, workers: workers, instances: make(map[*Instance]struct{})}, nil
}

func (executor *Executor) Workers() int {
	if executor == nil {
		return 0
	}
	return executor.workers
}

// Shutdown requests shutdown for every attached instance, waits for their
// cleanup, and then stops a host-created executor. The process-wide default
// executor cannot be shut down.
func (executor *Executor) Shutdown(ctx context.Context) error {
	if executor == nil || executor.pool == nil {
		return nil
	}
	if executor.shared {
		return errors.New("runtime default executor cannot be shut down")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	executor.mu.Lock()
	executor.closing = true
	instances := make([]*Instance, 0, len(executor.instances))
	for instance := range executor.instances {
		instances = append(instances, instance)
	}
	executor.mu.Unlock()
	for _, instance := range instances {
		instance.beginShutdown()
	}
	for _, instance := range instances {
		select {
		case <-instance.shutdownDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return executor.pool.shutdown(ctx)
}

func (executor *Executor) register(instance *Instance) error {
	if executor == nil || executor.pool == nil {
		return errors.New("runtime executor is unavailable")
	}
	if executor.shared {
		return nil
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.closing {
		return errors.New("runtime executor is closing")
	}
	executor.instances[instance] = struct{}{}
	return nil
}

func (executor *Executor) unregister(instance *Instance) {
	if executor == nil || executor.shared {
		return
	}
	executor.mu.Lock()
	delete(executor.instances, instance)
	executor.mu.Unlock()
}

// executionPool runs resumable work, not blocked guest calls. A job relinquishes
// its worker after each slice; notifications during a slice are retained until
// that slice has returned ownership.
type executionPool struct {
	mu      sync.Mutex
	ready   []*executionJob
	head    int
	changed *sync.Cond
	closed  bool
	workers sync.WaitGroup
	done    chan struct{}
}

type executionJob struct {
	pool    *executionPool
	run     func() bool
	queued  bool
	running bool
	woken   bool
	stopped bool
}

func newExecutionPool(workers int) (*executionPool, error) {
	if workers <= 0 {
		return nil, errors.New("execution worker count must be positive")
	}
	pool := &executionPool{done: make(chan struct{})}
	pool.changed = sync.NewCond(&pool.mu)
	pool.workers.Add(workers)
	for range workers {
		go pool.work()
	}
	return pool, nil
}

func (pool *executionPool) job(run func() bool) *executionJob {
	return &executionJob{pool: pool, run: run}
}

// wake never waits for worker capacity and never executes the job inline.
func (job *executionJob) wake() {
	pool := job.pool
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.closed || job.stopped {
		return
	}
	if job.running {
		job.woken = true
		return
	}
	if !job.queued {
		job.queued = true
		pool.ready = append(pool.ready, job)
		pool.changed.Signal()
	}
}

// stop prevents further slices; a running slice must observe cancellation in
// its own state and return before its storage can be released.
func (job *executionJob) stop() {
	job.pool.mu.Lock()
	job.stopped = true
	job.woken = false
	job.pool.mu.Unlock()
}

func (pool *executionPool) work() {
	defer pool.workers.Done()
	for {
		pool.mu.Lock()
		for pool.head == len(pool.ready) && !pool.closed {
			pool.changed.Wait()
		}
		if pool.closed {
			pool.mu.Unlock()
			return
		}
		job := pool.ready[pool.head]
		pool.ready[pool.head] = nil
		pool.head++
		if pool.head == len(pool.ready) {
			pool.ready = pool.ready[:0]
			pool.head = 0
		} else if pool.head >= len(pool.ready)/2 {
			remaining := copy(pool.ready, pool.ready[pool.head:])
			clear(pool.ready[remaining:])
			pool.ready = pool.ready[:remaining]
			pool.head = 0
		}
		job.queued = false
		if job.stopped {
			pool.mu.Unlock()
			continue
		}
		job.running = true
		pool.mu.Unlock()

		again := job.run()

		pool.mu.Lock()
		job.running = false
		if !pool.closed && !job.stopped && (again || job.woken) {
			job.queued = true
			pool.ready = append(pool.ready, job)
			pool.changed.Signal()
		}
		job.woken = false
		pool.mu.Unlock()
	}
}

// shutdown belongs to the external driver, never to a worker in this pool.
// Callers first cancel the jobs' work so active slices can return.
func (pool *executionPool) shutdown(ctx context.Context) error {
	pool.mu.Lock()
	if !pool.closed {
		pool.closed = true
		for _, job := range pool.ready[pool.head:] {
			job.queued = false
		}
		pool.ready = nil
		pool.head = 0
		pool.changed.Broadcast()
		go func() {
			pool.workers.Wait()
			close(pool.done)
		}()
	}
	pool.mu.Unlock()
	select {
	case <-pool.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
