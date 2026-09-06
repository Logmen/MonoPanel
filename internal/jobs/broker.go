package jobs

import (
	"sync"
	"time"
)

// Event is a job lifecycle notification delivered over SSE.
type Event struct {
	JobID    int64     `json:"job_id"`
	Type     string    `json:"type"` // queued started progress log done failed
	Progress int       `json:"progress,omitempty"`
	Message  string    `json:"message,omitempty"`
	Line     string    `json:"line,omitempty"`
	Error    string    `json:"error,omitempty"`
	Time     time.Time `json:"time"`
}

// Broker fans job events out to subscribers (per job or all jobs).
type Broker struct {
	mu   sync.Mutex
	subs map[int64]map[chan Event]struct{}
	all  map[chan Event]struct{}
}

// NewBroker creates an empty broker.
func NewBroker() *Broker {
	return &Broker{subs: map[int64]map[chan Event]struct{}{}, all: map[chan Event]struct{}{}}
}

// Subscribe returns a channel for events of jobID (0 = every job) and a
// cancel func. Slow subscribers drop events instead of blocking workers.
func (b *Broker) Subscribe(jobID int64, buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	b.mu.Lock()
	if jobID == 0 {
		b.all[ch] = struct{}{}
	} else {
		if b.subs[jobID] == nil {
			b.subs[jobID] = map[chan Event]struct{}{}
		}
		b.subs[jobID][ch] = struct{}{}
	}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if jobID == 0 {
			delete(b.all, ch)
		} else if m := b.subs[jobID]; m != nil {
			delete(m, ch)
			if len(m) == 0 {
				delete(b.subs, jobID)
			}
		}
		b.mu.Unlock()
	}
}

// Publish delivers an event without blocking.
func (b *Broker) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[e.JobID] {
		select {
		case ch <- e:
		default:
		}
	}
	for ch := range b.all {
		select {
		case ch <- e:
		default:
		}
	}
}
