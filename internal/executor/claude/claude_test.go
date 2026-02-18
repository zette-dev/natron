package claude

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zette-dev/natron/internal/executor"
)

// --- parseLine unit tests ---

func TestParseLine_SystemInit(t *testing.T) {
	e := New("sonnet")
	line := `{"type":"system","subtype":"init","session_id":"sess-123"}`

	evt, done := e.parseLine([]byte(line))

	if evt != nil {
		t.Errorf("expected no event for system init, got %+v", evt)
	}
	if done {
		t.Error("system init should not signal done")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sessionID != "sess-123" {
		t.Errorf("expected session ID sess-123, got %q", e.sessionID)
	}
}

func TestParseLine_AssistantText(t *testing.T) {
	e := New("sonnet")
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}`

	evt, done := e.parseLine([]byte(line))

	if evt == nil {
		t.Fatal("expected event for assistant message")
	}
	if evt.Type != executor.EventText {
		t.Errorf("expected EventText, got %d", evt.Type)
	}
	if evt.Text != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", evt.Text)
	}
	if done {
		t.Error("assistant message should not signal done")
	}
}

func TestParseLine_AssistantMultipleBlocks(t *testing.T) {
	e := New("sonnet")
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello "},{"type":"tool_use","id":"t1"},{"type":"text","text":"world"}]}}`

	evt, done := e.parseLine([]byte(line))

	if evt == nil {
		t.Fatal("expected event")
	}
	if evt.Text != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", evt.Text)
	}
	if done {
		t.Error("should not be done")
	}
}

func TestParseLine_Result(t *testing.T) {
	e := New("sonnet")
	line := `{"type":"result","result":{"content":[{"type":"text","text":"Final answer"}]}}`

	evt, done := e.parseLine([]byte(line))

	if evt == nil {
		t.Fatal("expected event for result")
	}
	if evt.Type != executor.EventDone {
		t.Errorf("expected EventDone, got %d", evt.Type)
	}
	if evt.Text != "Final answer" {
		t.Errorf("expected 'Final answer', got %q", evt.Text)
	}
	if !done {
		t.Error("result should signal done")
	}
}

func TestParseLine_UnknownType(t *testing.T) {
	e := New("sonnet")
	line := `{"type":"stream_event","event":{"type":"content_block_delta"}}`

	evt, done := e.parseLine([]byte(line))

	if evt != nil {
		t.Errorf("expected no event for unknown type, got %+v", evt)
	}
	if done {
		t.Error("unknown type should not signal done")
	}
}

func TestParseLine_InvalidJSON(t *testing.T) {
	e := New("sonnet")

	evt, done := e.parseLine([]byte("not json"))

	if evt != nil {
		t.Errorf("expected no event for invalid JSON, got %+v", evt)
	}
	if done {
		t.Error("invalid JSON should not signal done")
	}
}

func TestParseLine_AssistantEmptyContent(t *testing.T) {
	e := New("sonnet")
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1"}]}}`

	evt, done := e.parseLine([]byte(line))

	if evt != nil {
		t.Errorf("expected no event for tool_use-only content, got %+v", evt)
	}
	if done {
		t.Error("should not be done")
	}
}

// --- extractText unit tests ---

func TestExtractText_Nil(t *testing.T) {
	if got := extractText(nil); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractText_BadJSON(t *testing.T) {
	if got := extractText(json.RawMessage(`not json`)); got != "" {
		t.Errorf("expected empty for bad JSON, got %q", got)
	}
}

func TestExtractText_NoContent(t *testing.T) {
	if got := extractText(json.RawMessage(`{}`)); got != "" {
		t.Errorf("expected empty for no content, got %q", got)
	}
}

// --- readLoop integration test using pipes ---

// TestReadLoop_FullConversation simulates a Claude subprocess by feeding
// NDJSON lines through a pipe and verifying the executor dispatches the
// correct events.
func TestReadLoop_FullConversation(t *testing.T) {
	e := New("sonnet")

	// Wire up: pr is what readLoop reads from, pw is where we write NDJSON.
	pr, pw := io.Pipe()

	e.mu.Lock()
	e.alive = true
	e.mu.Unlock()

	// Start readLoop in background
	go e.readLoop(pr)

	// Register a response channel (mimicking what Send does internally)
	ch := make(chan executor.Event, 64)
	e.respMu.Lock()
	e.respCh = ch
	e.respMu.Unlock()

	// Feed a system init message
	writeLine(t, pw, `{"type":"system","subtype":"init","session_id":"test-sess-1"}`)

	// Feed an assistant message
	writeLine(t, pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello from Claude"}]}}`)

	// Feed a result message (ends the response)
	writeLine(t, pw, `{"type":"result","result":{"content":[{"type":"text","text":"Hello from Claude"}]}}`)

	// Collect events with timeout
	events := collectEvents(t, ch, 3*time.Second)

	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %+v", len(events), events)
	}

	// First event: assistant text
	if events[0].Type != executor.EventText || events[0].Text != "Hello from Claude" {
		t.Errorf("event[0]: expected EventText 'Hello from Claude', got %+v", events[0])
	}

	// Second event: result/done
	if events[1].Type != executor.EventDone || events[1].Text != "Hello from Claude" {
		t.Errorf("event[1]: expected EventDone 'Hello from Claude', got %+v", events[1])
	}

	// Verify session ID was captured
	e.mu.Lock()
	sid := e.sessionID
	e.mu.Unlock()
	if sid != "test-sess-1" {
		t.Errorf("expected session ID test-sess-1, got %q", sid)
	}

	pw.Close()
}

// TestReadLoop_MultiTurn simulates two sequential request/response cycles,
// mimicking how Send() swaps the response channel between turns.
func TestReadLoop_MultiTurn(t *testing.T) {
	e := New("sonnet")

	pr, pw := io.Pipe()

	e.mu.Lock()
	e.alive = true
	e.mu.Unlock()

	go e.readLoop(pr)

	// --- Turn 1 ---
	ch1 := make(chan executor.Event, 64)
	e.respMu.Lock()
	e.respCh = ch1
	e.respMu.Unlock()

	writeLine(t, pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"Turn 1 response"}]}}`)
	writeLine(t, pw, `{"type":"result","result":{"content":[{"type":"text","text":"Turn 1 response"}]}}`)

	events1 := collectEvents(t, ch1, 3*time.Second)
	if len(events1) != 2 {
		t.Fatalf("turn 1: expected 2 events, got %d", len(events1))
	}
	if events1[0].Text != "Turn 1 response" {
		t.Errorf("turn 1 text: %q", events1[0].Text)
	}

	// --- Turn 2 ---
	ch2 := make(chan executor.Event, 64)
	e.respMu.Lock()
	e.respCh = ch2
	e.respMu.Unlock()

	writeLine(t, pw, `{"type":"assistant","message":{"content":[{"type":"text","text":"Turn 2 response"}]}}`)
	writeLine(t, pw, `{"type":"result","result":{"content":[{"type":"text","text":"Turn 2 response"}]}}`)

	events2 := collectEvents(t, ch2, 3*time.Second)
	if len(events2) != 2 {
		t.Fatalf("turn 2: expected 2 events, got %d", len(events2))
	}
	if events2[0].Text != "Turn 2 response" {
		t.Errorf("turn 2 text: %q", events2[0].Text)
	}

	pw.Close()
}

// TestReadLoop_ProcessExit verifies that when the pipe closes (simulating
// process exit), the response channel is closed and alive becomes false.
func TestReadLoop_ProcessExit(t *testing.T) {
	e := New("sonnet")

	pr, pw := io.Pipe()

	e.mu.Lock()
	e.alive = true
	e.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.readLoop(pr)
	}()

	ch := make(chan executor.Event, 64)
	e.respMu.Lock()
	e.respCh = ch
	e.respMu.Unlock()

	// Close the pipe — simulates process exit
	pw.Close()

	// Wait for readLoop to finish
	wg.Wait()

	// Response channel should be closed
	_, ok := <-ch
	if ok {
		t.Error("expected response channel to be closed after process exit")
	}

	if e.Alive() {
		t.Error("expected executor to be not alive after process exit")
	}
}

// TestSendWritesCorrectJSON verifies the JSON format written to stdin.
func TestSendWritesCorrectJSON(t *testing.T) {
	e := New("sonnet")

	// Use a pipe as fake stdin so we can read what Send writes.
	// io.Pipe is synchronous, so we must read concurrently.
	pr, pw := io.Pipe()

	e.mu.Lock()
	e.stdin = pw
	e.alive = true
	e.mu.Unlock()

	stdoutR, stdoutW := io.Pipe()
	go e.readLoop(stdoutR)

	// Read stdin in a goroutine since Send blocks until the write completes
	stdinResult := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := pr.Read(buf)
		if err != nil {
			stdinResult <- ""
			return
		}
		stdinResult <- strings.TrimSpace(string(buf[:n]))
	}()

	ctx := context.Background()
	ch, err := e.Send(ctx, "What is 2+2?")
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}

	// Wait for the stdin read
	var line string
	select {
	case line = <-stdinResult:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out reading from stdin pipe")
	}

	if line == "" {
		t.Fatal("got empty stdin output")
	}

	var msg streamInput
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("unmarshal stdin JSON: %v (raw: %q)", err, line)
	}

	if msg.Type != "user" {
		t.Errorf("expected type 'user', got %q", msg.Type)
	}
	if msg.Message.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Message.Role)
	}
	if msg.Message.Content != "What is 2+2?" {
		t.Errorf("expected content 'What is 2+2?', got %q", msg.Message.Content)
	}

	// Feed a result to close the response channel
	writeLine(t, stdoutW, `{"type":"result","result":{"content":[{"type":"text","text":"4"}]}}`)

	events := collectEvents(t, ch, 3*time.Second)
	if len(events) == 0 {
		t.Fatal("expected at least one event")
	}
	last := events[len(events)-1]
	if last.Type != executor.EventDone {
		t.Errorf("expected EventDone, got %d", last.Type)
	}

	stdoutW.Close()
	pr.Close()
}

// --- test helpers ---

func writeLine(t *testing.T, w io.Writer, line string) {
	t.Helper()
	if _, err := io.WriteString(w, line+"\n"); err != nil {
		t.Fatalf("write line: %v", err)
	}
}

func collectEvents(t *testing.T, ch <-chan executor.Event, timeout time.Duration) []executor.Event {
	t.Helper()
	var events []executor.Event
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, evt)
		case <-timer.C:
			t.Fatalf("timed out waiting for events (collected %d so far)", len(events))
			return events
		}
	}
}
