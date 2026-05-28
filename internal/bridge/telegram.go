package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/wildcommitter/claudecgwd/internal/config"
)

const tgMaxChars = 4000 // leave headroom under Telegram's 4096 limit

type Telegram struct {
	cfg     config.TelegramConfig
	log     *slog.Logger
	inbound chan<- Inbound

	bot     *bot.Bot
	allowed map[int64]struct{}
}

func NewTelegram(cfg config.TelegramConfig, log *slog.Logger, inbound chan<- Inbound) *Telegram {
	allow := make(map[int64]struct{}, len(cfg.AllowedUserIDs))
	for _, id := range cfg.AllowedUserIDs {
		allow[id] = struct{}{}
	}
	return &Telegram{cfg: cfg, log: log, inbound: inbound, allowed: allow}
}

func (t *Telegram) Run(ctx context.Context) error {
	b, err := bot.New(t.cfg.Token, bot.WithDefaultHandler(t.handle))
	if err != nil {
		return fmt.Errorf("telegram bot: %w", err)
	}
	t.bot = b
	t.log.Info("telegram bridge starting")
	b.Start(ctx)
	return nil
}

func (t *Telegram) handle(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.Message == nil || update.Message.Text == "" {
		return
	}
	from := update.Message.From
	if from == nil {
		return
	}
	if _, ok := t.allowed[from.ID]; !ok {
		t.log.Warn("telegram: rejecting unauthorized user", "user_id", from.ID, "username", from.Username)
		return
	}
	// Show a typing indicator so the user knows we received the message.
	go func() {
		_, _ = b.SendChatAction(ctx, &bot.SendChatActionParams{
			ChatID: update.Message.Chat.ID,
			Action: models.ChatActionTyping,
		})
	}()
	origin := &tgOrigin{
		bridge:    t,
		chatID:    update.Message.Chat.ID,
		userID:    from.ID,
		username:  from.Username,
		replyToID: update.Message.ID,
	}
	select {
	case t.inbound <- Inbound{Text: update.Message.Text, Origin: origin}:
	default:
		t.log.Warn("telegram: inbound buffer full, dropping message", "user_id", from.ID)
	}
}

type tgOrigin struct {
	bridge    *Telegram
	chatID    int64
	userID    int64
	username  string
	replyToID int
	mu        sync.Mutex // serializes Replies on the same origin
}

func (o *tgOrigin) Describe() string {
	if o.username != "" {
		return fmt.Sprintf("telegram(@%s)", o.username)
	}
	return fmt.Sprintf("telegram(id=%d)", o.userID)
}

func (o *tgOrigin) Reply(ctx context.Context, text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if strings.TrimSpace(text) == "" {
		text = "(empty response)"
	}
	chunks := chunkText(text, tgMaxChars)
	for i, chunk := range chunks {
		params := &bot.SendMessageParams{
			ChatID: o.chatID,
			Text:   chunk,
		}
		if i == 0 && o.replyToID != 0 {
			params.ReplyParameters = &models.ReplyParameters{MessageID: o.replyToID}
		}
		if _, err := o.bridge.bot.SendMessage(ctx, params); err != nil {
			return fmt.Errorf("telegram send: %w", err)
		}
		if i+1 < len(chunks) {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// chunkText splits s into runs of at most max bytes, preferring split points
// at paragraph then line boundaries.
func chunkText(s string, max int) []string {
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		cut := max
		// Prefer last double-newline before max.
		if i := strings.LastIndex(s[:max], "\n\n"); i > max/2 {
			cut = i + 2
		} else if i := strings.LastIndex(s[:max], "\n"); i > max/2 {
			cut = i + 1
		} else if i := strings.LastIndex(s[:max], " "); i > max/2 {
			cut = i + 1
		}
		out = append(out, strings.TrimRight(s[:cut], " \t\n"))
		s = strings.TrimLeft(s[cut:], " \t\n")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
