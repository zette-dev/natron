package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zette-dev/natron/internal/config"
	"github.com/zette-dev/natron/internal/executor"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Session: config.SessionConfig{
			InactivityTimeout: 5 * time.Second,
			EditInterval:      1 * time.Second,
		},
		Workspaces: config.WorkspacesConfig{
			BasePath: t.TempDir(),
			Default:  "home",
		},
	}
}

// --- mockExec is a minimal test double local to session tests ---

type mockExec struct {
	mu      sync.Mutex
	alive   bool
	started int
	stopped int
	handler func(string) (<-chan executor.Event, error)
}

func (m *mockExec) Name() string { return "mock" }

func (m *mockExec) Alive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.alive
}

func (m *mockExec) Start(_ context.Context, _ string, _ executor.SessionContext) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alive = true
	m.started++
	return nil
}

func (m *mockExec) Send(_ context.Context, msg string) (<-chan executor.Event, error) {
	if m.handler != nil {
		return m.handler(msg)
	}
	ch := make(chan executor.Event, 2)
	ch <- executor.Event{Type: executor.EventText, Text: "echo: " + msg}
	ch <- executor.Event{Type: executor.EventDone, Text: "echo: " + msg}
	close(ch)
	return ch, nil
}

func (m *mockExec) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alive = false
	m.stopped++
	return nil
}

// --- Tests ---

func TestManager_CreateSession(t *testing.T) {
	cfg := testConfig(t)
	var created mockExec
	mgr := NewManager(cfg, func() executor.Executor { return &created })

	ctx := context.Background()
	events, err := mgr.Send(ctx, 100, "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := drain(t, events)
	if len(got) < 1 {
		t.Fatal("expected at least one event")
	}
	if got[0].Text != "echo: hello" {
		t.Errorf("expected 'echo: hello', got %q", got[0].Text)
	}
	if created.started != 1 {
		t.Errorf("expected 1 start, got %d", created.started)
	}
}

func TestManager_ReuseSession(t *testing.T) {
	cfg := testConfig(t)
	startCount := 0
	mgr := NewManager(cfg, func() executor.Executor {
		startCount++
		return &mockExec{}
	})

	ctx := context.Background()

	_, err := mgr.Send(ctx, 200, "first")
	if err != nil {
		t.Fatalf("first Send: %v", err)
	}

	_, err = mgr.Send(ctx, 200, "second")
	if err != nil {
		t.Fatalf("second Send: %v", err)
	}

	if startCount != 1 {
		t.Errorf("expected 1 factory call (session reuse), got %d", startCount)
	}
}

func TestManager_DifferentChatsGetDifferentSessions(t *testing.T) {
	cfg := testConfig(t)
	startCount := 0
	mgr := NewManager(cfg, func() executor.Executor {
		startCount++
		return &mockExec{}
	})

	ctx := context.Background()
	mgr.Send(ctx, 300, "a")
	mgr.Send(ctx, 400, "b")

	if startCount != 2 {
		t.Errorf("expected 2 factory calls for 2 chats, got %d", startCount)
	}
}

func TestManager_DeadExecutorRecovery(t *testing.T) {
	cfg := testConfig(t)
	callCount := 0

	mgr := NewManager(cfg, func() executor.Executor {
		callCount++
		return &mockExec{}
	})

	ctx := context.Background()

	// First message — creates session
	_, err := mgr.Send(ctx, 500, "first")
	if err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 factory call, got %d", callCount)
	}

	// Kill the executor behind the manager's back
	mgr.mu.Lock()
	sess := mgr.sessions[int64(500)]
	mgr.mu.Unlock()

	sess.exec.Stop() // sets alive=false

	// Second message — should detect dead executor and create a new session
	_, err = mgr.Send(ctx, 500, "second")
	if err != nil {
		t.Fatalf("second Send after death: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 factory calls (recovery), got %d", callCount)
	}
}

func TestManager_Shutdown(t *testing.T) {
	cfg := testConfig(t)
	var execs []*mockExec

	mgr := NewManager(cfg, func() executor.Executor {
		e := &mockExec{}
		execs = append(execs, e)
		return e
	})

	ctx := context.Background()
	mgr.Send(ctx, 600, "a")
	mgr.Send(ctx, 700, "b")

	mgr.Shutdown()

	for i, e := range execs {
		if e.Alive() {
			t.Errorf("executor %d still alive after shutdown", i)
		}
		if e.stopped != 1 {
			t.Errorf("executor %d: expected 1 stop, got %d", i, e.stopped)
		}
	}
}

func TestManager_InactivityTimeout(t *testing.T) {
	cfg := testConfig(t)
	cfg.Session.InactivityTimeout = 100 * time.Millisecond

	var exec mockExec
	mgr := NewManager(cfg, func() executor.Executor { return &exec })

	ctx := context.Background()
	_, err := mgr.Send(ctx, 800, "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Wait for the timeout to fire
	time.Sleep(300 * time.Millisecond)

	// Session should have been expired
	mgr.mu.Lock()
	_, exists := mgr.sessions[int64(800)]
	mgr.mu.Unlock()

	if exists {
		t.Error("session should have been expired by inactivity timeout")
	}
	if exec.stopped != 1 {
		t.Errorf("expected executor to be stopped, stopped count: %d", exec.stopped)
	}
}

func TestManager_TouchResetsTimeout(t *testing.T) {
	cfg := testConfig(t)
	cfg.Session.InactivityTimeout = 200 * time.Millisecond

	mgr := NewManager(cfg, func() executor.Executor { return &mockExec{} })

	ctx := context.Background()

	// Send first message
	mgr.Send(ctx, 900, "first")

	// Wait 150ms (within the 200ms timeout), then send another message
	time.Sleep(150 * time.Millisecond)
	mgr.Send(ctx, 900, "second") // should reset the timer

	// Wait another 150ms — total 300ms since first message, but only 150ms since last
	time.Sleep(150 * time.Millisecond)

	mgr.mu.Lock()
	_, exists := mgr.sessions[int64(900)]
	mgr.mu.Unlock()

	if !exists {
		t.Error("session should still exist — touch should have reset the timeout")
	}

	// Now wait for it to actually expire
	time.Sleep(200 * time.Millisecond)

	mgr.mu.Lock()
	_, exists = mgr.sessions[int64(900)]
	mgr.mu.Unlock()

	if exists {
		t.Error("session should have expired after full timeout without activity")
	}
}

func TestManager_WorkspaceMapping(t *testing.T) {
	cfg := testConfig(t)
	cfg.Workspaces.ChatMap = map[string]string{
		"1000": "zette",
	}

	var startedWorkDir string
	mgr := NewManager(cfg, func() executor.Executor {
		return &mockExec{}
	})

	// Patch factory to capture the workdir — we need to check resolveWorkDir
	workDir := mgr.resolveWorkDir(1000)
	if workDir != cfg.Workspaces.BasePath+"/zette" {
		t.Errorf("expected zette workspace, got %q", workDir)
	}

	workDir = mgr.resolveWorkDir(9999)
	if workDir != cfg.Workspaces.BasePath+"/home" {
		t.Errorf("expected default home workspace, got %q", workDir)
	}

	_ = startedWorkDir
}

func TestManager_ConcurrentSendsSameChat(t *testing.T) {
	cfg := testConfig(t)

	var mu sync.Mutex
	inFlight := 0
	maxInFlight := 0

	mgr := NewManager(cfg, func() executor.Executor {
		e := &mockExec{}
		e.handler = func(msg string) (<-chan executor.Event, error) {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()

			// Simulate work
			time.Sleep(50 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()

			ch := make(chan executor.Event, 2)
			ch <- executor.Event{Type: executor.EventText, Text: msg}
			ch <- executor.Event{Type: executor.EventDone}
			close(ch)
			return ch, nil
		}
		return e
	})

	ctx := context.Background()
	var wg sync.WaitGroup

	// Fire 5 concurrent sends to the same chat
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			events, err := mgr.Send(ctx, 1100, fmt.Sprintf("msg-%d", i))
			if err != nil {
				t.Errorf("send %d: %v", i, err)
				return
			}
			drain(t, events)
		}(i)
	}

	wg.Wait()

	mu.Lock()
	peak := maxInFlight
	mu.Unlock()

	if peak > 1 {
		t.Errorf("per-chat lock violated: max concurrent in-flight was %d (expected 1)", peak)
	}
}

// --- helpers ---

func drain(t *testing.T, ch <-chan executor.Event) []executor.Event {
	t.Helper()
	var events []executor.Event
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, evt)
		case <-timer.C:
			t.Fatalf("drain timed out after collecting %d events", len(events))
			return events
		}
	}
}
