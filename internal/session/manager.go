package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zette-dev/natron/internal/config"
	"github.com/zette-dev/natron/internal/executor"
)

// ExecutorFactory creates a new executor instance for a session.
type ExecutorFactory func() executor.Executor

// Manager maps Telegram chat IDs to active executor sessions and manages
// their lifecycle (creation, inactivity timeout, cleanup).
type Manager struct {
	cfg     config.Config
	factory ExecutorFactory

	mu       sync.Mutex
	sessions map[int64]*Session
}

// NewManager creates a session manager.
func NewManager(cfg config.Config, factory ExecutorFactory) *Manager {
	return &Manager{
		cfg:      cfg,
		factory:  factory,
		sessions: make(map[int64]*Session),
	}
}

// Send routes a message to the session for the given chat ID, creating
// one if needed. Returns a channel of streaming events.
func (m *Manager) Send(ctx context.Context, chatID int64, message string) (<-chan executor.Event, error) {
	sess, err := m.acquire(ctx, chatID)
	if err != nil {
		return nil, err
	}
	defer sess.mu.Unlock()

	sess.touch()

	events, err := sess.exec.Send(ctx, message)
	if err != nil {
		return nil, fmt.Errorf("send to executor: %w", err)
	}

	return events, nil
}

// acquire returns a locked, alive session for the chat. If the existing
// session's executor has died, it is replaced with a fresh one.
func (m *Manager) acquire(ctx context.Context, chatID int64) (*Session, error) {
	sess, err := m.getOrCreate(ctx, chatID)
	if err != nil {
		return nil, err
	}

	sess.mu.Lock()

	if sess.exec.Alive() {
		return sess, nil
	}

	// Executor died — unlock, replace, and lock the new session.
	sess.mu.Unlock()
	m.remove(chatID)

	sess, err = m.getOrCreate(ctx, chatID)
	if err != nil {
		return nil, err
	}

	sess.mu.Lock()
	return sess, nil
}

// Shutdown stops all active sessions.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for chatID, sess := range m.sessions {
		slog.Info("stopping session", "chat_id", chatID)
		sess.timer.Stop()
		sess.exec.Stop()
	}
	m.sessions = make(map[int64]*Session)
}

func (m *Manager) getOrCreate(ctx context.Context, chatID int64) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sess, ok := m.sessions[chatID]; ok {
		return sess, nil
	}

	workDir := m.resolveWorkDir(chatID)
	exec := m.factory()

	if err := exec.Start(ctx, workDir, executor.SessionContext{}); err != nil {
		return nil, fmt.Errorf("start executor for chat %d: %w", chatID, err)
	}

	sess := &Session{
		chatID:    chatID,
		workspace: workDir,
		exec:      exec,
		lastAct:   time.Now(),
		timeout:   m.cfg.Session.InactivityTimeout,
	}

	sess.timer = time.AfterFunc(m.cfg.Session.InactivityTimeout, func() {
		m.expire(chatID)
	})

	m.sessions[chatID] = sess
	slog.Info("session created", "chat_id", chatID, "workspace", workDir, "executor", exec.Name())
	return sess, nil
}

func (m *Manager) remove(chatID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sess, ok := m.sessions[chatID]; ok {
		sess.timer.Stop()
		sess.exec.Stop()
		delete(m.sessions, chatID)
		slog.Info("session removed", "chat_id", chatID)
	}
}

func (m *Manager) expire(chatID int64) {
	slog.Info("session expired", "chat_id", chatID)
	m.remove(chatID)
}

func (m *Manager) resolveWorkDir(chatID int64) string {
	chatKey := fmt.Sprintf("%d", chatID)
	if name, ok := m.cfg.Workspaces.ChatMap[chatKey]; ok {
		return m.cfg.Workspaces.BasePath + "/" + name
	}
	return m.cfg.Workspaces.BasePath + "/" + m.cfg.Workspaces.Default
}
