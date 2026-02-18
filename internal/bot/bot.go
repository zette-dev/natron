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
	"github.com/zette-dev/natron/internal/session"
)

const maxMessageLen = 4096

// SessionProvider is the interface the bot uses to interact with sessions.
type SessionProvider interface {
	// Send routes a message to the appropriate session and returns streamed events.
	// username is the Telegram @username without the @ prefix (empty for DMs or
	// private groups without a username). title is the group/channel display name.
	Send(ctx context.Context, chatID int64, username, title, message string) (<-chan executor.Event, error)

	// Reset stops the active session for chatID so the next message starts fresh.
	Reset(chatID int64)

	// Status returns the current session state for chatID.
	Status(chatID int64) session.StatusInfo
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
		bot.WithMessageTextHandler("/new", bot.MatchTypePrefix, b.handleNew),
		bot.WithMessageTextHandler("/status", bot.MatchTypePrefix, b.handleStatus),
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

	chat := update.Message.Chat
	chatID := chat.ID
	text := update.Message.Text

	// Send typing indicator
	tg.SendChatAction(ctx, &bot.SendChatActionParams{
		ChatID: chatID,
		Action: models.ChatActionTyping,
	})

	events, err := b.sessions.Send(ctx, chatID, chat.Username, chat.Title, text)
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

// handleNew clears the active session so the next message starts a fresh conversation.
func (b *Bot) handleNew(ctx context.Context, tg *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	chatID := update.Message.Chat.ID
	b.sessions.Reset(chatID)
	tg.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   "Session cleared. Starting fresh.",
	})
}

// handleStatus reports the current session state for the chat.
func (b *Bot) handleStatus(ctx context.Context, tg *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	chatID := update.Message.Chat.ID
	info := b.sessions.Status(chatID)

	var text string
	if !info.Exists {
		text = "No active session. Send a message to start one."
	} else {
		age := time.Since(info.CreatedAt).Round(time.Second)
		text = fmt.Sprintf("Active since %s (%s ago)\nWorkspace: %s",
			info.CreatedAt.Format("15:04"),
			formatDuration(age),
			info.Workspace,
		)
	}

	tg.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	})
}

// formatDuration returns a human-readable duration string (e.g. "2h 5m", "45s").
func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// streamResponse sends an initial message and edits it in place as events
// arrive. Splits into new messages if the response exceeds 4096 chars.
// Intermediate edits are plain text; the final edit uses MarkdownV2.
func (b *Bot) streamResponse(ctx context.Context, tg *bot.Bot, chatID int64, events <-chan executor.Event) {
	var (
		msgID    int
		buf      strings.Builder
		lastEdit string
		ticker   = time.NewTicker(b.editIvl)
	)
	defer ticker.Stop()

	flush := func(final bool) {
		raw := buf.String()
		if raw == "" {
			return
		}

		var sendText string
		var parseMode models.ParseMode
		if final {
			sendText = formatV2(raw)
			parseMode = models.ParseModeMarkdown // maps to "MarkdownV2" in this library
		} else {
			sendText = raw
		}

		if sendText == lastEdit {
			return
		}

		// Truncate to max length for current message
		if utf8.RuneCountInString(sendText) > maxMessageLen {
			sendText = truncateRunes(sendText, maxMessageLen-3) + "..."
		}

		if msgID == 0 {
			sent, err := tg.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:    chatID,
				Text:      sendText,
				ParseMode: parseMode,
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
				Text:      sendText,
				ParseMode: parseMode,
			})
			if err != nil {
				slog.Debug("edit message failed", "error", err)
			}
		}
		lastEdit = sendText
	}

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				// Channel closed — final flush
				flush(true)
				return
			}

			switch evt.Type {
			case executor.EventText:
				// If adding this text would exceed the limit, flush current
				// message and start a new one.
				if utf8.RuneCountInString(buf.String())+utf8.RuneCountInString(evt.Text) > maxMessageLen {
					flush(true)
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
				flush(true)
				return

			case executor.EventError:
				slog.Error("executor error", "error", evt.Error)
				if buf.Len() == 0 {
					buf.WriteString("An error occurred while processing your message.")
				}
				flush(false)
				return
			}

		case <-ticker.C:
			flush(false)

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

// formatV2 converts Claude markdown output to Telegram MarkdownV2.
//
// Code fences (``` ... ```) are preserved with their language hint; content
// inside is escaped (only \ and ` need escaping in a code block). Inline code
// spans (` ... `) are preserved similarly. All other MarkdownV2 special
// characters are escaped in plain-text segments so the message is never
// rejected by Telegram. Bold/italic/headers are not converted — they render
// as their literal characters, which is readable.
func formatV2(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence := false

	for _, line := range lines {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			out = append(out, line) // fence delimiters pass through unchanged
			continue
		}
		if inFence {
			// Escape only backslash and backtick inside code blocks.
			line = strings.ReplaceAll(line, `\`, `\\`)
			line = strings.ReplaceAll(line, "`", "\\`")
			out = append(out, line)
		} else {
			out = append(out, escapeV2Line(line))
		}
	}

	// If input had an unclosed fence, close it so Telegram doesn't reject it.
	if inFence {
		out = append(out, "```")
	}

	return strings.Join(out, "\n")
}

// escapeV2Line escapes a single plain-text line for Telegram MarkdownV2.
// Inline code spans (` ... `) and bold spans (**...**) are preserved and
// converted to their MarkdownV2 equivalents. Everything else has special
// characters escaped with a backslash.
func escapeV2Line(line string) string {
	var out strings.Builder
	i := 0
	for i < len(line) {
		// Inline code span: `...`
		if line[i] == '`' {
			j := strings.IndexByte(line[i+1:], '`')
			if j >= 0 {
				j += i + 1 // absolute index of closing backtick
				out.WriteByte('`')
				// Inside inline code: escape only backslash.
				content := strings.ReplaceAll(line[i+1:j], `\`, `\\`)
				out.WriteString(content)
				out.WriteByte('`')
				i = j + 1
				continue
			}
			// No closing backtick — escape it as a literal character.
			out.WriteString("\\`")
			i++
			continue
		}

		// Bold span: **...** → *...*  (MarkdownV2 bold uses single *)
		if i+1 < len(line) && line[i] == '*' && line[i+1] == '*' {
			j := strings.Index(line[i+2:], "**")
			if j >= 0 {
				j += i + 2 // absolute index of closing **
				out.WriteByte('*')
				for _, r := range line[i+2 : j] {
					if isV2Special(r) {
						out.WriteByte('\\')
					}
					out.WriteRune(r)
				}
				out.WriteByte('*')
				i = j + 2
				continue
			}
			// No closing ** — escape both asterisks as literals.
			out.WriteString("\\*\\*")
			i += 2
			continue
		}

		r, size := utf8.DecodeRuneInString(line[i:])
		if isV2Special(r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
		i += size
	}
	return out.String()
}

// isV2Special reports whether r must be escaped in Telegram MarkdownV2.
func isV2Special(r rune) bool {
	const special = `\_*[]()~` + "`" + `>#+-=|{}.!`
	return strings.ContainsRune(special, r)
}
