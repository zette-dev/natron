package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/zette-dev/natron/internal/config"
	"github.com/zette-dev/natron/internal/executor"
)

const maxMessageLen = 4096

// SessionProvider is the interface the bot uses to get an executor for a chat.
type SessionProvider interface {
	// Send routes a message to the appropriate session and returns streamed events.
	Send(ctx context.Context, chatID int64, message string) (<-chan executor.Event, error)
}

// Bot wraps the Telegram bot and routes messages to sessions.
type Bot struct {
	bot      *bot.Bot
	sessions SessionProvider
	cfg      config.TelegramConfig
	editIvl  time.Duration
	allowed  map[int64]bool
}

// New creates a Telegram bot wired to the given session provider.
func New(cfg config.TelegramConfig, editInterval time.Duration, sessions SessionProvider) (*Bot, error) {
	allowed := make(map[int64]bool, len(cfg.AllowedUserIDs))
	for _, id := range cfg.AllowedUserIDs {
		allowed[id] = true
	}

	b := &Bot{
		sessions: sessions,
		cfg:      cfg,
		editIvl:  editInterval,
		allowed:  allowed,
	}

	opts := []bot.Option{
		bot.WithMiddlewares(b.authMiddleware),
		bot.WithDefaultHandler(b.handleMessage),
	}

	tgBot, err := bot.New(cfg.BotToken, opts...)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	b.bot = tgBot
	return b, nil
}

// Start begins long polling. Blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) {
	slog.Info("telegram bot starting long poll")
	b.bot.Start(ctx)
}

// authMiddleware silently drops messages from unauthorized users.
func (b *Bot) authMiddleware(next bot.HandlerFunc) bot.HandlerFunc {
	return func(ctx context.Context, tg *bot.Bot, update *models.Update) {
		if update.Message == nil || update.Message.From == nil {
			return
		}
		if !b.allowed[update.Message.From.ID] {
			slog.Warn("unauthorized message", "user_id", update.Message.From.ID)
			return
		}
		next(ctx, tg, update)
	}
}

// handleMessage processes an incoming text message.
func (b *Bot) handleMessage(ctx context.Context, tg *bot.Bot, update *models.Update) {
	if update.Message == nil || update.Message.Text == "" {
		return
	}

	chatID := update.Message.Chat.ID
	text := update.Message.Text

	// Send typing indicator
	tg.SendChatAction(ctx, &bot.SendChatActionParams{
		ChatID: chatID,
		Action: models.ChatActionTyping,
	})

	events, err := b.sessions.Send(ctx, chatID, text)
	if err != nil {
		slog.Error("session send failed", "chat_id", chatID, "error", err)
		tg.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   "Something went wrong. Please try again.",
		})
		return
	}

	b.streamResponse(ctx, tg, chatID, events)
}

// streamResponse sends an initial message and edits it in place as events
// arrive. Splits into new messages if the response exceeds 4096 chars.
func (b *Bot) streamResponse(ctx context.Context, tg *bot.Bot, chatID int64, events <-chan executor.Event) {
	var (
		msgID     int
		buf       strings.Builder
		lastEdit  string
		ticker    = time.NewTicker(b.editIvl)
	)
	defer ticker.Stop()

	flush := func() {
		text := buf.String()
		if text == "" || text == lastEdit {
			return
		}

		// Truncate to max length for current message
		if utf8.RuneCountInString(text) > maxMessageLen {
			text = truncateRunes(text, maxMessageLen-3) + "..."
		}

		if msgID == 0 {
			sent, err := tg.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID,
				Text:   text,
			})
			if err != nil {
				slog.Error("send message failed", "error", err)
				return
			}
			msgID = sent.ID
		} else {
			_, err := tg.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    chatID,
				MessageID: msgID,
				Text:      text,
			})
			if err != nil {
				slog.Debug("edit message failed", "error", err)
			}
		}
		lastEdit = text
	}

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				// Channel closed — final flush
				flush()
				return
			}

			switch evt.Type {
			case executor.EventText:
				// If adding this text would exceed the limit, flush current
				// message and start a new one.
				if utf8.RuneCountInString(buf.String())+utf8.RuneCountInString(evt.Text) > maxMessageLen {
					flush()
					buf.Reset()
					lastEdit = ""
					msgID = 0
				}
				buf.WriteString(evt.Text)

			case executor.EventDone:
				// Final text — replace buffer if non-empty
				if evt.Text != "" {
					buf.Reset()
					buf.WriteString(evt.Text)
				}
				flush()
				return

			case executor.EventError:
				slog.Error("executor error", "error", evt.Error)
				if buf.Len() == 0 {
					buf.WriteString("An error occurred while processing your message.")
				}
				flush()
				return
			}

		case <-ticker.C:
			flush()

		case <-ctx.Done():
			return
		}
	}
}

// truncateRunes returns the first n runes of s.
func truncateRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i >= n {
			return s[:j]
		}
		i++
	}
	return s
}
