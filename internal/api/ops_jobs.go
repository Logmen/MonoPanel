package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"

	"monopanel/internal/jobs"
	"monopanel/internal/store"
)

type jobsListInput struct {
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"500"`
	Status string `query:"status" enum:",queued,running,done,failed,cancelled"`
}

type jobsOutput struct {
	Body []*store.Job
}

type jobIDInput struct {
	ID int64 `path:"id" minimum:"1"`
}

type jobOutput struct {
	Body *store.Job
}

// SSE event payloads: one Go type per event name.
type (
	JobSnapshot struct {
		Job *store.Job `json:"job"`
	}
	QueuedEvent   jobs.Event
	StartedEvent  jobs.Event
	ProgressEvent jobs.Event
	LogEvent      jobs.Event
	DoneEvent     jobs.Event
	FailedEvent   jobs.Event
	PingEvent     struct {
		Time time.Time `json:"time"`
	}
)

// redactJob hides the secrets some jobs carry in their payload (a new
// account's password, a CMS admin password, the ssh password and the token of
// a migration source): the job runner needs them, a reader of the job list
// does not. Keys ending in "_enc" and the keys token, password and secret are
// replaced at any depth.
func redactJob(j *store.Job) *store.Job {
	if j == nil || len(j.Payload) == 0 {
		return j
	}
	if !bytes.Contains(j.Payload, []byte(`_enc"`)) && !bytes.Contains(j.Payload, []byte(`"token"`)) &&
		!bytes.Contains(j.Payload, []byte(`"password"`)) && !bytes.Contains(j.Payload, []byte(`"secret"`)) {
		return j
	}
	var v any
	if err := json.Unmarshal(j.Payload, &v); err != nil {
		return j
	}
	redactEnc(v)
	b, err := json.Marshal(v)
	if err != nil {
		return j
	}
	c := *j
	c.Payload = b
	return &c
}

func redactEnc(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if strings.HasSuffix(k, "_enc") || k == "token" || k == "password" || k == "secret" {
				if sv, ok := val.(string); ok && sv == "" {
					continue
				}
				t[k] = "[redacted]"
				continue
			}
			redactEnc(val)
		}
	case []any:
		for _, val := range t {
			redactEnc(val)
		}
	}
}

func (s *Server) canSeeJob(ctx context.Context, j *store.Job) bool {
	p := principalFrom(ctx)
	return p.Role == store.RoleAdmin || j.RequestedBy == p.Login
}

func (s *Server) registerJobs() {
	huma.Register(s.api, huma.Operation{
		OperationID: "jobs-list", Method: http.MethodGet, Path: "/jobs", Summary: "List recent jobs", Tags: []string{"jobs"}, Security: secured,
		Description: "Newest first. Administrators see every job, other accounts the jobs they asked for; limit counts the jobs the caller sees.",
	}, func(ctx context.Context, in *jobsListInput) (*jobsOutput, error) {
		// An ordinary account sees the jobs it asked for (canSeeJob): that
		// filter belongs in the query, or limit would count everybody's jobs
		// and leave the account with fewer of its own than there are.
		filter := store.JobFilter{Limit: in.Limit, Status: in.Status}
		if p := principalFrom(ctx); p.Role != store.RoleAdmin {
			filter.RequestedBy = p.Login
		}
		list, err := s.db.ListJobsFiltered(ctx, filter)
		if err != nil {
			return nil, err
		}
		out := make([]*store.Job, 0, len(list))
		for _, j := range list {
			if s.canSeeJob(ctx, j) {
				out = append(out, redactJob(j))
			}
		}
		return &jobsOutput{Body: out}, nil
	})

	huma.Register(s.api, huma.Operation{
		OperationID: "jobs-get", Method: http.MethodGet, Path: "/jobs/{id}", Summary: "Get a job with its log", Tags: []string{"jobs"}, Security: secured,
	}, func(ctx context.Context, in *jobIDInput) (*jobOutput, error) {
		j, err := s.db.GetJob(ctx, in.ID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !s.canSeeJob(ctx, j)) {
			return nil, huma.Error404NotFound("job not found")
		}
		if err != nil {
			return nil, err
		}
		return &jobOutput{Body: redactJob(j)}, nil
	})

	sse.Register(s.api, huma.Operation{
		OperationID: "jobs-events", Method: http.MethodGet, Path: "/jobs/{id}/events", Summary: "Stream job progress and log lines (SSE)", Tags: []string{"jobs"}, Security: secured,
	}, map[string]any{
		"snapshot": JobSnapshot{}, "queued": QueuedEvent{}, "started": StartedEvent{}, "progress": ProgressEvent{},
		"log": LogEvent{}, "done": DoneEvent{}, "failed": FailedEvent{}, "ping": PingEvent{},
	}, func(ctx context.Context, in *jobIDInput, send sse.Sender) {
		ch, stop := s.jobs.Broker().Subscribe(in.ID, 512)
		defer stop()
		j, err := s.db.GetJob(ctx, in.ID)
		if err != nil || !s.canSeeJob(ctx, j) {
			send.Data(FailedEvent{JobID: in.ID, Type: "failed", Error: "job not found", Time: time.Now()}) //nolint:errcheck // best effort; the caller reports the real failure
			return
		}
		if err := send.Data(JobSnapshot{Job: redactJob(j)}); err != nil {
			return
		}
		if j.Status == store.JobDone || j.Status == store.JobFailed || j.Status == store.JobCancelled {
			return
		}
		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ping.C:
				if err := send.Data(PingEvent{Time: time.Now()}); err != nil {
					return
				}
			case e := <-ch:
				var v any
				switch e.Type {
				case "queued":
					v = QueuedEvent(e)
				case "started":
					v = StartedEvent(e)
				case "progress":
					v = ProgressEvent(e)
				case "log":
					v = LogEvent(e)
				case "done":
					v = DoneEvent(e)
				case "failed":
					v = FailedEvent(e)
				default:
					continue
				}
				if err := send.Data(v); err != nil {
					return
				}
				if e.Type == "done" || e.Type == "failed" {
					return
				}
			}
		}
	})
}
