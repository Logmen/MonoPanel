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
	wg       sync.WaitGroup
	mu       sync.RWMutex
	timeout  time.Duration
}

// NewRunner creates a runner with n workers.
func NewRunner(db *store.DB, n int, log *slog.Logger) *Runner {
	if n <= 0 {
		n = 1
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{db: db, broker: NewBroker(), log: log, workers: n, handlers: map[string]Handler{}, wake: make(chan struct{}, 1), timeout: time.Hour}
}

// Register binds a job type to a handler.
func (r *Runner) Register(typ string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[typ] = h
}

// Broker exposes the event broker (for SSE).
func (r *Runner) Broker() *Broker { return r.broker }

// Option customises an enqueued job.
type Option func(*store.Job)

// WithLockKey serialises jobs sharing a key (e.g. "site:12").
func WithLockKey(k string) Option { return func(j *store.Job) { j.LockKey = k } }

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
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return j, nil
}

// Start launches workers until ctx is cancelled.
func (r *Runner) Start(ctx context.Context) {
	if n, err := r.db.RequeueStaleJobs(ctx); err == nil && n > 0 {
		r.log.Warn("requeued jobs left running by a previous process", "count", n)
	}
	for i := 0; i < r.workers; i++ {
		r.wg.Add(1)
		go r.worker(ctx, fmt.Sprintf("w%d", i+1))
	}
}

// Wait blocks until all workers exit.
func (r *Runner) Wait() { r.wg.Wait() }

func (r *Runner) worker(ctx context.Context, name string) {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		job, err := r.db.ClaimJob(ctx, name)
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
		case <-r.wake:
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
		jctx, cancel := context.WithTimeout(ctx, r.timeout)
		err = safeCall(jctx, h, jc)
		cancel()
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
