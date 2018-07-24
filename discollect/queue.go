package discollect

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// A Queue is used to submit and retrieve individual tasks
type Queue interface {
	Pop(ctx context.Context) (*QueuedTask, error)
	Push(ctx context.Context, tasks []*QueuedTask) error

	Finish(ctx context.Context, taskID uuid.UUID) error
	Status(ctx context.Context, scrapeID uuid.UUID) *ScrapeStatus
}

var ErrCompletedScrape = errors.New("completed scrape")

// A QueuedTask is the struct for a task that goes on the Queue
type QueuedTask struct {
	// set by the TaskQueue
	TaskID   uuid.UUID `json:"task_id"`
	ScrapeID uuid.UUID `json:"scrape_id"`

	QueuedAt time.Time `json:"queued_at"`
	Config   *Config   `json:"config"`
	Plugin   string    `json:"plugin"`
	Retries  int       `json:"retries"`

	Task *Task `json:"task"`
}

// A Task generally maps to a single HTTP request, but sometimes more than one
// may be made
type Task struct {
	URL string `json:"url"`
	// Extra can be used to send information from a parent task to its children
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
	// Timeout is the timeout a single task should have attached to it
	// defaults to 15s
	Timeout time.Duration
}

// ScrapeStatus is returned from a Queue with information about a specific scrape
type ScrapeStatus struct {
	TotalTasks     int `json:"total_tasks,omitempty"`
	InFlightTasks  int `json:"in_flight_tasks,omitempty"`
	CompletedTasks int `json:"completed_tasks,omitempty"`
	RetriedTasks   int `json:"retried_tasks,omitempty"`
}

// NewMemQueue makes a new purely in-memory queue
func NewMemQueue() *MemQueue {
	return &MemQueue{}
}

// A MemQueue is a super simple Queue backed by an array and a mutex
type MemQueue struct {
	mu sync.Mutex

	inflight       int
	totalTasks     int
	completedTasks int
	retriedTasks   int

	q []*QueuedTask
}

// Pop pops a single task off the left side of the array
func (mq *MemQueue) Pop(ctx context.Context) (*QueuedTask, error) {
	mq.mu.Lock()
	defer mq.mu.Unlock()

	if len(mq.q) == 0 {
		return nil, nil
	}

	mq.inflight += 1

	qt := mq.q[0]
	mq.q = mq.q[1:]

	return qt, nil
}

// Push appends tasks to the right side of the array
func (mq *MemQueue) Push(ctx context.Context, tasks []*QueuedTask) error {
	mq.mu.Lock()
	defer mq.mu.Unlock()

	for _, t := range tasks {
		if t == nil {
			continue
		}

		if t.Retries == 0 {
			mq.totalTasks += 1
		} else {
			mq.retriedTasks += 1
		}
	}

	mq.q = append(mq.q, tasks...)

	return nil
}

// Finish is a no-op for the MemQueue
func (mq *MemQueue) Finish(ctx context.Context, taskID uuid.UUID) error {
	mq.mu.Lock()
	defer mq.mu.Unlock()

	mq.inflight -= 1
	mq.completedTasks += 1

	return nil
}

// Status returns the status for a given scrape
func (mq *MemQueue) Status(ctx context.Context, scrapeID uuid.UUID) *ScrapeStatus {
	mq.mu.Lock()
	defer mq.mu.Unlock()

	return &ScrapeStatus{
		CompletedTasks: mq.completedTasks,
		TotalTasks:     mq.totalTasks,
		RetriedTasks:   mq.retriedTasks,
		InFlightTasks:  mq.inflight,
	}
}
