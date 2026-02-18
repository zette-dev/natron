package executor

import "context"

// EventType classifies a streamed output event from an executor.
type EventType int

const (
	EventText  EventType = iota // Partial text content
	EventDone                   // Response complete
	EventError                  // Error occurred
)

// Event is a unit of streamed output from an executor.
type Event struct {
	Type  EventType
	Text  string // Partial text (EventText) or final text (EventDone)
	Error error  // Set for EventError
}

// SessionContext is executor-agnostic context the session manager builds
// from memory and history. Each executor materializes this into whatever
// its underlying agent expects (CLAUDE.md, AGENTS.md, etc.)
type SessionContext struct {
	GlobalBriefing string
	ChatMemory     string
	RecentHistory  string
	WorkspaceInfo  string
	IdentityDoc    string
}

// Executor is the interface any CLI-based agent must implement.
// The session manager only ever talks to this interface.
type Executor interface {
	// Start spawns the underlying process in the given working directory.
	Start(ctx context.Context, workDir string, sessionCtx SessionContext) error

	// Send writes a message and returns a channel of streaming events.
	// The channel closes when the response is complete.
	Send(ctx context.Context, message string) (<-chan Event, error)

	// Stop gracefully shuts down the process.
	Stop() error

	// Alive reports whether the process is still running.
	Alive() bool

	// Name returns a human-readable identifier ("claude", "codex", etc.)
	Name() string
}
