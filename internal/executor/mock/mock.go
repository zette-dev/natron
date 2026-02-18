package mock

import (
	"context"
	"sync"

	"github.com/zette-dev/natron/internal/executor"
)

// Executor is a test double that returns canned responses.
type Executor struct {
	mu      sync.Mutex
	alive   bool
	Handler func(ctx context.Context, message string) (<-chan executor.Event, error)
}

func New() *Executor {
	return &Executor{}
}

func (e *Executor) Name() string { return "mock" }

func (e *Executor) Alive() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.alive
}

func (e *Executor) Start(_ context.Context, _ string, _ executor.SessionContext) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.alive = true
	return nil
}

func (e *Executor) Send(ctx context.Context, message string) (<-chan executor.Event, error) {
	if e.Handler != nil {
		return e.Handler(ctx, message)
	}

	ch := make(chan executor.Event, 2)
	ch <- executor.Event{Type: executor.EventText, Text: "mock response to: " + message}
	ch <- executor.Event{Type: executor.EventDone, Text: "mock response to: " + message}
	close(ch)
	return ch, nil
}

func (e *Executor) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.alive = false
	return nil
}
