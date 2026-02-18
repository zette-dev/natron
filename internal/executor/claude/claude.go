package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/zette-dev/natron/internal/executor"
)

const (
	shutdownTimeout = 5 * time.Second
	scanBufSize     = 1024 * 1024 // 1MB max line length for NDJSON
)

// Executor spawns and manages a persistent Claude Code CLI subprocess
// using the stream-json protocol for bidirectional communication.
type Executor struct {
	model string

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	cancel    context.CancelFunc
	alive     bool
	sessionID string

	// respCh is set by Send() and consumed by the reader goroutine.
	// Only one response can be in flight at a time (enforced by
	// the session manager's per-chat lock).
	respMu sync.Mutex
	respCh chan<- executor.Event
}

// New creates a Claude Code executor with the given model.
func New(model string) *Executor {
	return &Executor{model: model}
}

func (e *Executor) Name() string { return "claude" }

func (e *Executor) Alive() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.alive
}

// Start spawns the Claude Code subprocess in the given working directory.
func (e *Executor) Start(ctx context.Context, workDir string, _ executor.SessionContext) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.alive {
		return fmt.Errorf("executor already running")
	}

	procCtx, cancel := context.WithCancel(ctx)
	e.cancel = cancel

	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", e.model,
	}

	e.cmd = exec.CommandContext(procCtx, "claude", args...)
	e.cmd.Dir = workDir
	e.cmd.Env = append(os.Environ(), "TERM=dumb")

	var err error
	e.stdin, err = e.cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := e.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := e.cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := e.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start claude: %w", err)
	}

	e.alive = true

	go e.drainStderr(stderr)
	go e.readLoop(stdout)

	return nil
}

// Send writes a user message to the Claude subprocess and returns a channel
// of streaming events. The channel closes when the response is complete.
//
// Only one Send may be in flight at a time. The session manager's per-chat
// lock enforces this.
func (e *Executor) Send(ctx context.Context, message string) (<-chan executor.Event, error) {
	e.mu.Lock()
	if !e.alive {
		e.mu.Unlock()
		return nil, fmt.Errorf("executor not running")
	}
	stdin := e.stdin
	e.mu.Unlock()

	msg := streamInput{
		Type: "user",
		Message: streamInputMessage{
			Role:    "user",
			Content: message,
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	data = append(data, '\n')

	// Set up the response channel before writing to stdin so the
	// reader goroutine can dispatch events immediately.
	ch := make(chan executor.Event, 64)
	e.respMu.Lock()
	e.respCh = ch
	e.respMu.Unlock()

	if _, err := stdin.Write(data); err != nil {
		e.respMu.Lock()
		e.respCh = nil
		e.respMu.Unlock()
		close(ch)
		return nil, fmt.Errorf("write to stdin: %w", err)
	}

	// Wrap in a context-aware channel
	out := make(chan executor.Event, 64)
	go func() {
		defer close(out)
		for evt := range ch {
			select {
			case out <- evt:
			case <-ctx.Done():
				out <- executor.Event{Type: executor.EventError, Error: ctx.Err()}
				return
			}
		}
	}()

	return out, nil
}

// Stop gracefully shuts down the Claude subprocess.
func (e *Executor) Stop() error {
	e.mu.Lock()
	if !e.alive {
		e.mu.Unlock()
		return nil
	}
	cmd := e.cmd
	stdin := e.stdin
	cancel := e.cancel
	e.mu.Unlock()

	// Close stdin to signal EOF
	if stdin != nil {
		stdin.Close()
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		<-done
	}

	e.mu.Lock()
	e.alive = false
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return nil
}

var _ executor.Executor = (*Executor)(nil)

// readLoop is the single goroutine that reads all NDJSON from stdout
// and dispatches events to the current response channel.
func (e *Executor) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), scanBufSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		evt, done := e.parseLine(line)
		if evt != nil {
			e.dispatch(*evt)
		}
		if done {
			e.closeResp()
		}
	}

	if err := scanner.Err(); err != nil {
		e.dispatch(executor.Event{Type: executor.EventError, Error: fmt.Errorf("read stdout: %w", err)})
	}

	// Process exited — close any pending response channel
	e.closeResp()

	e.mu.Lock()
	e.alive = false
	e.mu.Unlock()

	slog.Info("claude process exited")
}

func (e *Executor) dispatch(evt executor.Event) {
	e.respMu.Lock()
	ch := e.respCh
	e.respMu.Unlock()

	if ch != nil {
		ch <- evt
	}
}

func (e *Executor) closeResp() {
	e.respMu.Lock()
	if e.respCh != nil {
		close(e.respCh)
		e.respCh = nil
	}
	e.respMu.Unlock()
}

// parseLine parses a single NDJSON line from Claude's stdout.
// Returns an event (or nil) and whether this line signals end of response.
func (e *Executor) parseLine(line []byte) (*executor.Event, bool) {
	var msg streamMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		slog.Warn("unparseable NDJSON line", "error", err, "line", string(line))
		return nil, false
	}

	switch msg.Type {
	case "system":
		e.handleSystem(msg)
		return nil, false

	case "assistant":
		text := extractText(msg.Message)
		if text != "" {
			return &executor.Event{Type: executor.EventText, Text: text}, false
		}
		return nil, false

	case "result":
		text := extractText(msg.Result)
		return &executor.Event{Type: executor.EventDone, Text: text}, true

	default:
		return nil, false
	}
}

func (e *Executor) handleSystem(msg streamMessage) {
	if msg.Subtype == "init" && msg.SessionID != "" {
		e.mu.Lock()
		e.sessionID = msg.SessionID
		e.mu.Unlock()
		slog.Info("claude session initialized", "session_id", msg.SessionID)
	}
}

func (e *Executor) drainStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		slog.Debug("claude stderr", "line", scanner.Text())
	}
}

// --- stream-json protocol types ---

type streamInput struct {
	Type    string             `json:"type"`
	Message streamInputMessage `json:"message"`
}

type streamInputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamMessage struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

type contentMessage struct {
	Content []contentBlock `json:"content,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func extractText(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}

	var msg contentMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}

	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}
