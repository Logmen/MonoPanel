// Package jobs runs asynchronous work: a persistent queue in SQLite, N
// workers, per-entity locks and a broker for live progress.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"monopanel/internal/store"
)

// Handler executes one job type.
type Handler func(ctx context.Context, job *Context) error

// Context is what a handler sees.
type Context struct {
	ID          int64
	Type        string
	Payload     json.RawMessage
	RequestedBy string
	runner      *Runner
}

// Logf appends a line to the job log and streams it.
func (c *Context) Logf(format string, args ...any) {
	c.runner.logLine(c.ID, fmt.Sprintf(format, args...))
}

// Progress updates progress (0-100) and the status message.
func (c *Context) Progress(pct int, message string) {
	c.runner.progress(c.ID, pct, message)
}

// Unmarshal decodes the payload.
func (c *Context) Unmarshal(v any) error { return json.Unmarshal(c.Payload, v) }

// Runner owns the workers.
type Runner struct {
	db       *store.DB
	broker   *Broker
	log      *slog.Logger
	workers  int
	handlers map[string]Handler
	wake     chan struct{}
	// spareWake wakes the spare worker (Start). It has a channel of its
	// own: one that took a worker's wake with nothing it may claim would
	// leave the job to the next tick of the worker that may.
	spareWake chan struct{}
	wg        sync.WaitGroup
	mu        sync.RWMutex
	// timeout is how long a job may run unless its type has a limit of its
	// own in limits (SetLimit).
	timeout time.Duration
	limits  map[string]time.Duration
	// ready holds the workers back at Start until it returns (Hold).
	ready func(ctx context.Context)
	// exclusive are the groups of job types of which no two run at the
	// same time (Exclusive).
	exclusive [][]string
}

// DefaultLimit is how long a job may run, from its start, unless its type
// has a limit of its own (SetLimit): its context ends then.
const DefaultLimit = time.Hour

// NewRunner creates a runner with n workers.
func NewRunner(db *store.DB, n int, log *slog.Logger) *Runner {
	if n <= 0 {
		n = 1
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{db: db, broker: NewBroker(), log: log, workers: n, handlers: map[string]Handler{}, wake: make(chan struct{}, 1), spareWake: make(chan struct{}, 1), timeout: DefaultLimit, limits: map[string]time.Duration{}}
}

// Register binds a job type to a handler.
func (r *Runner) Register(typ string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[typ] = h
}

// SetLimit gives the jobs of a type a time limit of their own instead of
// DefaultLimit: a backup or a restore runs restic for hours, a migration
// copies a whole account. A limit that is not positive leaves the default.
// A type whose limit is above the default is a long one: Start keeps a
// spare worker for the others while long jobs hold every worker. Called
// before Start, as Register.
func (r *Runner) SetLimit(typ string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d > 0 {
		r.limits[typ] = d
	} else {
		delete(r.limits, typ)
	}
}

// Limit is how long a job of the type may run.
func (r *Runner) Limit(typ string) time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if d, ok := r.limits[typ]; ok {
		return d
	}
	return r.timeout
}

// longTypes are the job types whose limit is above the default (SetLimit),
// sorted; under r.mu.
func (r *Runner) longTypes() []string {
	var long []string
	for typ, d := range r.limits {
		if d > r.timeout {
			long = append(long, typ)
		}
	}
	slices.Sort(long)
	return long
}

// Broker exposes the event broker (for SSE).
func (r *Runner) Broker() *Broker { return r.broker }

// Option customises an enqueued job.
type Option func(*store.Job)

// WithLockKey serialises jobs sharing a key: "site:<domain>" for the jobs
// of a site, "user:<login>" for those of an account, "stack" for the
// stack's (the doctor's jobs line counts failures by it as well).
func WithLockKey(k string) Option { return func(j *store.Job) { j.LockKey = k } }

// Exclusive makes the jobs of the types never run at the same time, in
// whichever order they come, whatever their lock keys: one of them stays
// queued while another of them runs, and so waits for it rather than meeting
// it. Their lock keys go on serialising each with the jobs of its own key,
// and jobs of other types run beside them as before. Called before Start.
func (r *Runner) Exclusive(types ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exclusive = append(r.exclusive, slices.Clone(types))
}

// WithRequestedBy records the actor.
func WithRequestedBy(actor string) Option { return func(j *store.Job) { j.RequestedBy = actor } }

// WithIdempotencyKey makes repeated enqueues return the same job.
func WithIdempotencyKey(k string) Option { return func(j *store.Job) { j.IdempotencyKey = k } }

// Enqueue stores a job and wakes a worker.
func (r *Runner) Enqueue(ctx context.Context, typ string, payload any, opts ...Option) (*store.Job, error) {
	r.mu.RLock()
	_, known := r.handlers[typ]
	r.mu.RUnlock()
	if !known {
		return nil, fmt.Errorf("unknown job type %q", typ)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	j := &store.Job{Type: typ, Payload: b}
	for _, o := range opts {
		o(j)
	}
	if err := r.db.EnqueueJob(ctx, j); err != nil {
		return nil, err
	}
	r.broker.Publish(Event{JobID: j.ID, Type: "queued"})
	for _, wake := range []chan struct{}{r.wake, r.spareWake} {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	return j, nil
}

// Hold makes the workers wait, from Start on, until ready returns (it gets
// the context Start was given) before they claim a job; Enqueue goes on
// queueing meanwhile. The API holds them until its agent answers: every job
// acts through the agent, which may listen a minute or two after the API
// started, and a job claimed before would fail on "agent unreachable" for
// that alone. ready decides itself how long it waits. Hold is called before
// Start.
func (r *Runner) Hold(ready func(ctx context.Context)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = ready
}

// Start launches workers until ctx is cancelled: the n of NewRunner and,
// when some job type has a limit above the default (SetLimit), one more, a
// spare. A long job — a backup, a restore, a migration — may hold its
// worker for hours, and once long jobs hold every worker every other job
// would wait for them: a site's apply, a certificate's renewal, the applies
// a migration queues for the sites it moved, behind the next migration in
// the queue. The spare takes none of the long jobs, and none at all until
// long jobs hold every other worker (store.ClaimSpareJob): the other jobs
// then run on it one at a time, each within the default limit.
func (r *Runner) Start(ctx context.Context) {
	if n, err := r.db.RequeueStaleJobs(ctx); err == nil && n > 0 {
		r.log.Warn("requeued jobs left running by a previous process", "count", n)
	}
	r.mu.RLock()
	ready := r.ready
	long := r.longTypes()
	r.mu.RUnlock()
	released := make(chan struct{})
	if ready == nil {
		close(released)
	} else {
		go func() {
			defer close(released)
			ready(ctx)
		}()
	}
	for i := 0; i < r.workers; i++ {
		r.wg.Add(1)
		go r.worker(ctx, fmt.Sprintf("w%d", i+1), released, r.wake, func(ctx context.Context, name string, exclusive ...[]string) (*store.Job, error) {
			return r.db.ClaimJob(ctx, name, exclusive...)
		})
	}
	if len(long) > 0 {
		r.wg.Add(1)
		go r.worker(ctx, fmt.Sprintf("w%d", r.workers+1), released, r.spareWake, func(ctx context.Context, name string, exclusive ...[]string) (*store.Job, error) {
			return r.db.ClaimSpareJob(ctx, name, long, r.workers, exclusive...)
		})
	}
}

// Wait blocks until all workers exit.
func (r *Runner) Wait() { r.wg.Wait() }

// worker claims jobs with claim and runs them, one at a time, until ctx
// ends; it looks for one when wake says a job was queued, and every second.
func (r *Runner) worker(ctx context.Context, name string, start, wake <-chan struct{}, claim func(ctx context.Context, name string, exclusive ...[]string) (*store.Job, error)) {
	defer r.wg.Done()
	select {
	case <-start:
	case <-ctx.Done():
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	r.mu.RLock()
	exclusive := r.exclusive
	r.mu.RUnlock()
	for {
		job, err := claim(ctx, name, exclusive...)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.log.Error("claim job", "err", err)
		}
		if job != nil {
			r.run(ctx, job)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-ticker.C:
		}
	}
}

func (r *Runner) run(ctx context.Context, job *store.Job) {
	r.mu.RLock()
	h := r.handlers[job.Type]
	r.mu.RUnlock()
	r.broker.Publish(Event{JobID: job.ID, Type: "started"})
	jc := &Context{ID: job.ID, Type: job.Type, Payload: job.Payload, RequestedBy: job.RequestedBy, runner: r}
	var err error
	if h == nil {
		err = fmt.Errorf("no handler for job type %q", job.Type)
	} else {
		jctx, cancel := context.WithTimeout(ctx, r.Limit(job.Type))
		err = safeCall(jctx, h, jc)
		cancel()
	}
	if err != nil && ctx.Err() != nil {
		// The panel is stopping (systemctl restart in the middle of a job):
		// the job stays "running", and the next start puts it back in the
		// queue instead of leaving a half-done failure behind.
		r.logLine(job.ID, "[runner] interrupted by shutdown ("+err.Error()+"); runs again after the restart")
		r.log.Warn("job interrupted by shutdown", "id", job.ID, "type", job.Type)
		return
	}
	if err != nil {
		r.logLine(job.ID, "error: "+err.Error())
		_ = r.db.FinishJob(context.Background(), job.ID, store.JobFailed, err.Error())
		r.broker.Publish(Event{JobID: job.ID, Type: "failed", Error: err.Error()})
		r.log.Warn("job failed", "id", job.ID, "type", job.Type, "err", err)
		return
	}
	_ = r.db.FinishJob(context.Background(), job.ID, store.JobDone, "")
	r.broker.Publish(Event{JobID: job.ID, Type: "done", Progress: 100})
	r.log.Info("job done", "id", job.ID, "type", job.Type)
}

func safeCall(ctx context.Context, h Handler, jc *Context) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v\n%s", rec, debug.Stack())
		}
	}()
	return h(ctx, jc)
}

func (r *Runner) logLine(id int64, line string) {
	_ = r.db.AppendJobLog(context.Background(), id, line+"\n")
	r.broker.Publish(Event{JobID: id, Type: "log", Line: line})
}

func (r *Runner) progress(id int64, pct int, message string) {
	_ = r.db.UpdateJobProgress(context.Background(), id, pct, message)
	r.broker.Publish(Event{JobID: id, Type: "progress", Progress: pct, Message: message})
}

// ErrCancelled is returned by handlers that observed cancellation.
var ErrCancelled = errors.New("cancelled")
