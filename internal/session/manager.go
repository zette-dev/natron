package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zette-dev/natron/internal/config"
	"github.com/zette-dev/natron/internal/executor"
)

// ExecutorFactory creates a new executor instance for a session.
type ExecutorFactory func() executor.Executor

// StatusInfo describes the current state of a chat's session.
type StatusInfo struct {
	Exists    bool
	Workspace string
	CreatedAt time.Time
}

// Manager maps Telegram chat IDs to active executor sessions and manages
// their lifecycle (creation and cleanup).
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
// one if needed. username and title are used for workspace resolution and
// may be empty for DMs or when not provided by Telegram.
func (m *Manager) Send(ctx context.Context, chatID int64, username, title, message string) (<-chan executor.Event, error) {
	sess, err := m.acquire(ctx, chatID, username, title)
	if err != nil {
		return nil, err
	}
	defer sess.mu.Unlock()

	events, err := sess.exec.Send(ctx, message)
	if err != nil {
		return nil, fmt.Errorf("send to executor: %w", err)
	}

	return events, nil
}

// Reset stops and removes any active session for chatID.
// The next message will create a fresh session.
func (m *Manager) Reset(chatID int64) {
	m.remove(chatID)
}

// Status returns the current session state for a chat.
func (m *Manager) Status(chatID int64) StatusInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	sess, ok := m.sessions[chatID]
	if !ok {
		return StatusInfo{}
	}
	return StatusInfo{
		Exists:    true,
		Workspace: sess.workspace,
		CreatedAt: sess.createdAt,
	}
}

// Shutdown stops all active sessions.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for chatID, sess := range m.sessions {
		slog.Info("stopping session", "chat_id", chatID)
		sess.exec.Stop()
	}
	m.sessions = make(map[int64]*Session)
}

// acquire returns a locked, alive session for the chat. If the existing
// session's executor has died, it is replaced with a fresh one.
func (m *Manager) acquire(ctx context.Context, chatID int64, username, title string) (*Session, error) {
	sess, err := m.getOrCreate(ctx, chatID, username, title)
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

	sess, err = m.getOrCreate(ctx, chatID, username, title)
	if err != nil {
		return nil, err
	}

	sess.mu.Lock()
	return sess, nil
}

func (m *Manager) getOrCreate(ctx context.Context, chatID int64, username, title string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sess, ok := m.sessions[chatID]; ok {
		return sess, nil
	}

	workDir := m.resolveWorkDir(chatID, username, title)
	exec := m.factory()

	if err := exec.Start(ctx, workDir, executor.SessionContext{IdentityDoc: m.loadIdentity()}); err != nil {
		return nil, fmt.Errorf("start executor for chat %d: %w", chatID, err)
	}

	sess := &Session{
		chatID:    chatID,
		workspace: workDir,
		exec:      exec,
		createdAt: time.Now(),
	}

	m.sessions[chatID] = sess
	slog.Info("session created", "chat_id", chatID, "workspace", workDir, "executor", exec.Name())
	return sess, nil
}

func (m *Manager) remove(chatID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sess, ok := m.sessions[chatID]; ok {
		sess.exec.Stop()
		delete(m.sessions, chatID)
		slog.Info("session removed", "chat_id", chatID)
	}
}

// loadIdentity reads the soul and memory files and combines them into a
// single string for use as a system prompt addition. Missing files are
// silently skipped — neither is required for the bot to function.
func (m *Manager) loadIdentity() string {
	var parts []string

	if soul, err := os.ReadFile(m.cfg.Claude.SoulPath); err == nil && len(soul) > 0 {
		parts = append(parts, strings.TrimSpace(string(soul)))
	}
	if memory, err := os.ReadFile(m.cfg.Claude.MemoryPath); err == nil && len(memory) > 0 {
		parts = append(parts, "---\n\n## Shared Memory\n\n"+strings.TrimSpace(string(memory)))
	}

	return strings.Join(parts, "\n\n")
}

// resolveWorkDir maps a chat to its workspace directory. Resolution order:
//  1. @username (config key "@mygroup" or "mygroup")
//  2. Chat title (e.g. "My Team")
//  3. Numeric chat ID string (e.g. "-1001234567890")
//  4. Default workspace
func (m *Manager) resolveWorkDir(chatID int64, username, title string) string {
	// Username lookup — accept keys with or without leading @
	if username != "" {
		uname := strings.TrimPrefix(username, "@")
		if name, ok := m.cfg.Workspaces.ChatMap["@"+uname]; ok {
			return filepath.Join(m.cfg.Workspaces.BasePath, name)
		}
		if name, ok := m.cfg.Workspaces.ChatMap[uname]; ok {
			return filepath.Join(m.cfg.Workspaces.BasePath, name)
		}
	}
	// Title lookup
	if title != "" {
		if name, ok := m.cfg.Workspaces.ChatMap[title]; ok {
			return filepath.Join(m.cfg.Workspaces.BasePath, name)
		}
	}
	// Numeric chat ID lookup
	if name, ok := m.cfg.Workspaces.ChatMap[fmt.Sprintf("%d", chatID)]; ok {
		return filepath.Join(m.cfg.Workspaces.BasePath, name)
	}
	return filepath.Join(m.cfg.Workspaces.BasePath, m.cfg.Workspaces.Default)
}
