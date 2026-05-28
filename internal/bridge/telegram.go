package bridge

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/wildcommitter/claudecgwd/internal/claude"
	"github.com/wildcommitter/claudecgwd/internal/config"
)

const (
	tgMaxChars  = 4000           // leave headroom under Telegram's 4096 limit
	tgAskExpiry = 24 * time.Hour // safety bound on a parked question — long enough that a real human won't trip it
)

type Telegram struct {
	cfg     config.TelegramConfig
	log     *slog.Logger
	inbound chan<- Inbound

	bot     *bot.Bot
	allowed map[int64]struct{}
	ready   chan struct{} // closed once bot is set, so QRSink can wait for startup

	// Pending AskUserQuestion waiters, keyed by a per-question token embedded in
	// inline-button callback data.
	wmu     sync.Mutex
	waiters map[string]*tgWaiter
	seq     atomic.Uint64
}

// tgWaiter is an in-flight interactive question awaiting the user's tap.
type tgWaiter struct {
	q        claude.Question
	chatID   int64
	msgID    int
	selected map[int]bool // multi-select toggle state
	result   chan []int   // selected indices, delivered once
	done     bool         // guards single delivery
}

func NewTelegram(cfg config.TelegramConfig, log *slog.Logger, inbound chan<- Inbound) *Telegram {
	allow := make(map[int64]struct{}, len(cfg.AllowedUserIDs))
	for _, id := range cfg.AllowedUserIDs {
		allow[id] = struct{}{}
	}
	return &Telegram{cfg: cfg, log: log, inbound: inbound, allowed: allow, ready: make(chan struct{}), waiters: map[string]*tgWaiter{}}
}

func (t *Telegram) Run(ctx context.Context) error {
	b, err := bot.New(t.cfg.Token, bot.WithDefaultHandler(t.handle))
	if err != nil {
		return fmt.Errorf("telegram bot: %w", err)
	}
	t.bot = b
	close(t.ready)
	t.log.Info("telegram bridge starting")
	b.Start(ctx)
	return nil
}

func (t *Telegram) handle(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery != nil {
		t.handleCallback(ctx, update.CallbackQuery)
		return
	}
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

// NotifyPending pings the "typing" chat action every 4s until ctx is
// cancelled. Telegram fades the indicator after ~5s, so the loop re-arms it.
func (o *tgOrigin) NotifyPending(ctx context.Context) {
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		_, _ = o.bridge.bot.SendChatAction(ctx, &bot.SendChatActionParams{
			ChatID: o.chatID,
			Action: models.ChatActionTyping,
		})
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
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

// SendPhotoToOwner sends a photo (e.g. a WhatsApp pairing QR) to the first
// allowed user. Serves as a QRSink for the WhatsApp bridge. Waits briefly for
// the bot to finish starting if called early.
func (t *Telegram) SendPhotoToOwner(ctx context.Context, png []byte, caption string) error {
	select {
	case <-t.ready:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("telegram bot not ready")
	}
	if len(t.cfg.AllowedUserIDs) == 0 {
		return fmt.Errorf("no allowed telegram users to send QR to")
	}
	_, err := t.bot.SendPhoto(ctx, &bot.SendPhotoParams{
		ChatID:  t.cfg.AllowedUserIDs[0],
		Photo:   &models.InputFileUpload{Filename: "whatsapp-qr.png", Data: bytes.NewReader(png)},
		Caption: caption,
	})
	return err
}

// AskChoices presents each question via a Telegram inline keyboard and blocks
// for the user's taps, returning one Answer per question in order.
func (o *tgOrigin) AskChoices(ctx context.Context, qs []claude.Question) ([]claude.Answer, error) {
	out := make([]claude.Answer, len(qs))
	for i, q := range qs {
		ans, err := o.bridge.askQuestion(ctx, o.chatID, q)
		if err != nil {
			return nil, err
		}
		out[i] = ans
	}
	return out, nil
}

// askQuestion sends one question with inline buttons and waits for resolution.
func (t *Telegram) askQuestion(ctx context.Context, chatID int64, q claude.Question) (claude.Answer, error) {
	token := strconv.FormatUint(t.seq.Add(1), 36)
	w := &tgWaiter{q: q, chatID: chatID, selected: map[int]bool{}, result: make(chan []int, 1)}

	msg, err := t.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:      chatID,
		Text:        formatQuestion(q),
		ReplyMarkup: t.buildKeyboard(token, q, w.selected),
	})
	if err != nil {
		return claude.Answer{}, fmt.Errorf("telegram ask send: %w", err)
	}
	w.msgID = msg.ID

	t.wmu.Lock()
	t.waiters[token] = w
	t.wmu.Unlock()
	defer func() {
		t.wmu.Lock()
		delete(t.waiters, token)
		t.wmu.Unlock()
	}()

	timer := time.NewTimer(tgAskExpiry)
	defer timer.Stop()
	select {
	case idx := <-w.result:
		return claude.Answer{Indices: idx}, nil
	case <-timer.C:
		return claude.Answer{}, fmt.Errorf("question timed out after %s", tgAskExpiry)
	case <-ctx.Done():
		return claude.Answer{}, ctx.Err()
	}
}

// handleCallback resolves an inline-button tap against a pending waiter.
func (t *Telegram) handleCallback(ctx context.Context, cq *models.CallbackQuery) {
	if _, ok := t.allowed[cq.From.ID]; !ok {
		return
	}
	ack := func(text string) {
		_, _ = t.bot.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cq.ID, Text: text})
	}
	parts := strings.Split(cq.Data, "|")
	if len(parts) < 2 {
		ack("")
		return
	}
	kind, token := parts[0], parts[1]
	t.wmu.Lock()
	w := t.waiters[token]
	t.wmu.Unlock()
	if w == nil {
		ack("This question has expired.")
		return
	}

	switch kind {
	case "s": // single-select: resolve immediately
		idx, err := strconv.Atoi(parts[2])
		if err != nil {
			ack("")
			return
		}
		t.deliver(ctx, w, []int{idx})
		ack("Selected.")
	case "m": // multi-select: toggle and re-render
		idx, err := strconv.Atoi(parts[2])
		if err != nil {
			ack("")
			return
		}
		t.wmu.Lock()
		w.selected[idx] = !w.selected[idx]
		sel := copySelected(w.selected)
		t.wmu.Unlock()
		_, _ = t.bot.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{
			ChatID:      w.chatID,
			MessageID:   w.msgID,
			ReplyMarkup: t.buildKeyboard(token, w.q, sel),
		})
		ack("")
	case "d": // multi-select: done
		t.wmu.Lock()
		idxs := selectedIndices(w.selected)
		t.wmu.Unlock()
		t.deliver(ctx, w, idxs)
		ack("Submitted.")
	default:
		ack("")
	}
}

// deliver hands the selection to the waiting askQuestion exactly once and
// disables the message's buttons.
func (t *Telegram) deliver(ctx context.Context, w *tgWaiter, idxs []int) {
	t.wmu.Lock()
	if w.done {
		t.wmu.Unlock()
		return
	}
	w.done = true
	t.wmu.Unlock()
	w.result <- idxs
	_, _ = t.bot.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{
		ChatID:      w.chatID,
		MessageID:   w.msgID,
		ReplyMarkup: models.InlineKeyboardMarkup{},
	})
}

// buildKeyboard renders the option buttons for a question. Multi-select options
// carry a ▢/☑ marker and a trailing "Done" row.
func (t *Telegram) buildKeyboard(token string, q claude.Question, selected map[int]bool) models.InlineKeyboardMarkup {
	rows := make([][]models.InlineKeyboardButton, 0, len(q.Options)+1)
	for i, o := range q.Options {
		label := fmt.Sprintf("%d. %s", i+1, o.Label)
		var data string
		if q.MultiSelect {
			mark := "▢"
			if selected[i] {
				mark = "☑"
			}
			label = mark + " " + label
			data = "m|" + token + "|" + strconv.Itoa(i)
		} else {
			data = "s|" + token + "|" + strconv.Itoa(i)
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: truncateButton(label), CallbackData: data}})
	}
	if q.MultiSelect {
		rows = append(rows, []models.InlineKeyboardButton{{Text: "✅ Done", CallbackData: "d|" + token}})
	}
	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func formatQuestion(q claude.Question) string {
	var b strings.Builder
	if q.Header != "" {
		b.WriteString("❓ " + q.Header + "\n")
	}
	b.WriteString(q.Question)
	for i, o := range q.Options {
		fmt.Fprintf(&b, "\n%d. %s", i+1, o.Label)
		if o.Description != "" {
			b.WriteString(" — " + o.Description)
		}
	}
	if q.MultiSelect {
		b.WriteString("\n\nSelect any number of options, then tap ✅ Done.")
	} else {
		b.WriteString("\n\nTap a choice below.")
	}
	return b.String()
}

func truncateButton(s string) string {
	const max = 60
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func copySelected(m map[int]bool) map[int]bool {
	out := make(map[int]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func selectedIndices(m map[int]bool) []int {
	var out []int
	for i, on := range m {
		if on {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
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
