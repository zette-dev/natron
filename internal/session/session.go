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
	createdAt time.Time
	mu        sync.Mutex
}
