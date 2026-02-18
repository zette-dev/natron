package session

import (
	"sync"
	"time"

	"github.com/zette-dev/natron/internal/executor"
)

// Session is an active executor process bound to a Telegram chat.
type Session struct {
	chatID    int64
	workspace string
	exec      executor.Executor
	lastAct   time.Time
	timer     *time.Timer
	timeout   time.Duration
	mu        sync.Mutex
}

// touch resets the inactivity timer.
func (s *Session) touch() {
	s.lastAct = time.Now()
	s.timer.Reset(s.timeout)
}
