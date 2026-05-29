package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
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

	bot          *bot.Bot
	allowed      map[int64]struct{}
	inboxDir     string        // where sent files are downloaded
	stt          *Transcriber  // optional voice/audio transcription
	voice        *VoiceOut     // optional spoken (voice-note) replies
	indexTrigger func()        // optional: poke the RAG auto-indexer after a file save
	ready        chan struct{} // closed once bot is set, so QRSink can wait for startup
	readyOnce    sync.Once     // guards the ready close across bridge restarts

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

func NewTelegram(cfg config.TelegramConfig, log *slog.Logger, inbound chan<- Inbound, inboxDir string, stt *Transcriber, voice *VoiceOut, indexTrigger func()) *Telegram {
	allow := make(map[int64]struct{}, len(cfg.AllowedUserIDs))
	for _, id := range cfg.AllowedUserIDs {
		allow[id] = struct{}{}
	}
	return &Telegram{cfg: cfg, log: log, inbound: inbound, allowed: allow, inboxDir: inboxDir, stt: stt, voice: voice, indexTrigger: indexTrigger, ready: make(chan struct{}), waiters: map[string]*tgWaiter{}}
}

// fireIndex pokes the auto-indexer (if wired) after a file is saved.
func (t *Telegram) fireIndex() {
	if t.indexTrigger != nil {
		t.indexTrigger()
	}
}

func (t *Telegram) Run(ctx context.Context) error {
	b, err := bot.New(t.cfg.Token, bot.WithDefaultHandler(t.handle))
	if err != nil {
		return fmt.Errorf("telegram bot: %w", err)
	}
	t.bot = b
	t.readyOnce.Do(func() { close(t.ready) })
	t.log.Info("telegram bridge starting")
	b.Start(ctx)
	return nil
}

func (t *Telegram) handle(ctx context.Context, b *bot.Bot, update *models.Update) {
	if update.CallbackQuery != nil {
		t.handleCallback(ctx, update.CallbackQuery)
		return
	}
	msg := update.Message
	if msg == nil {
		return
	}
	from := msg.From
	if from == nil {
		return
	}
	if _, ok := t.allowed[from.ID]; !ok {
		t.log.Warn("telegram: rejecting unauthorized user", "user_id", from.ID, "username", from.Username)
		return
	}
	origin := &tgOrigin{
		bridge:    t,
		chatID:    msg.Chat.ID,
		userID:    from.ID,
		username:  from.Username,
		replyToID: msg.ID,
	}

	// Voice/audio? Transcribe it and feed the text in as the prompt. Mark the
	// origin so the reply mirrors back as a voice note (in auto mode).
	if t.stt.Enabled() && (msg.Voice != nil || msg.Audio != nil) {
		fileID, name := "", "voice.ogg"
		if msg.Voice != nil {
			fileID = msg.Voice.FileID
		} else {
			fileID, name = msg.Audio.FileID, "audio.mp3"
		}
		origin.voiceIn = true
		go t.transcribeAttachment(ctx, fileID, name, origin)
		return
	}

	// Other attachment? Download it (off the update loop) and notify the
	// session. Images get fed in as a vision turn (see receivedNotice).
	if fileID, name, ok := tgAttachment(msg); ok {
		go t.saveAttachment(ctx, fileID, name, msg.Caption, tgIsImage(msg), origin)
		return
	}

	if msg.Text == "" {
		return
	}
	select {
	case t.inbound <- Inbound{Text: msg.Text, Origin: origin}:
	default:
		t.log.Warn("telegram: inbound buffer full, dropping message", "user_id", from.ID)
	}
}

// tgAttachment returns the file_id and a suggested filename for the first
// attachment on a message, if any.
func tgAttachment(m *models.Message) (fileID, name string, ok bool) {
	switch {
	case m.Document != nil:
		return m.Document.FileID, m.Document.FileName, true
	case len(m.Photo) > 0:
		return m.Photo[len(m.Photo)-1].FileID, "photo.jpg", true // largest size
	case m.Voice != nil:
		return m.Voice.FileID, "voice.ogg", true
	case m.Audio != nil:
		return m.Audio.FileID, "audio.mp3", true
	case m.Video != nil:
		return m.Video.FileID, "video.mp4", true
	case m.VideoNote != nil:
		return m.VideoNote.FileID, "videonote.mp4", true
	case m.Sticker != nil:
		return m.Sticker.FileID, "sticker.webp", true
	}
	return "", "", false
}

// tgIsImage reports whether the message's attachment is a still image, so it
// can be fed to Claude as a vision turn rather than catalogued as a plain file.
func tgIsImage(m *models.Message) bool {
	if len(m.Photo) > 0 {
		return true
	}
	if m.Document != nil && strings.HasPrefix(m.Document.MimeType, "image/") {
		return true
	}
	return false
}

// saveAttachment downloads a Telegram file to the inbox and enqueues a notice.
func (t *Telegram) saveAttachment(ctx context.Context, fileID, name, caption string, isImage bool, origin *tgOrigin) {
	data, err := t.downloadFile(ctx, fileID)
	if err != nil {
		t.log.Warn("telegram: file download failed", "err", err)
		_ = origin.Reply(ctx, "⚠️  couldn't download that file: "+err.Error())
		return
	}
	path, err := saveInbox(t.inboxDir, "telegram", name, data)
	if err != nil {
		t.log.Warn("telegram: saving file failed", "err", err)
		_ = origin.Reply(ctx, "⚠️  couldn't save that file: "+err.Error())
		return
	}
	t.log.Info("telegram: saved incoming file", "path", path, "bytes", len(data), "image", isImage)
	t.fireIndex() // make the new attachment searchable right away
	text := receivedNotice("telegram", path, caption, isImage)
	select {
	case t.inbound <- Inbound{Text: text, Origin: origin}:
	default:
		t.log.Warn("telegram: inbound buffer full, dropping file notice")
	}
}

// transcribeAttachment downloads a voice/audio file, transcribes it, and feeds
// the transcript in as the prompt (archiving the audio in the inbox).
func (t *Telegram) transcribeAttachment(ctx context.Context, fileID, name string, origin *tgOrigin) {
	data, err := t.downloadFile(ctx, fileID)
	if err != nil {
		t.log.Warn("telegram: audio download failed", "err", err)
		_ = origin.Reply(ctx, "⚠️  couldn't download that audio: "+err.Error())
		return
	}
	path, err := saveInbox(t.inboxDir, "telegram", name, data)
	if err != nil {
		t.log.Warn("telegram: saving audio failed", "err", err)
	}
	transcript, err := t.stt.Transcribe(ctx, path)
	if err != nil {
		t.log.Warn("telegram: transcription failed", "err", err)
		_ = origin.Reply(ctx, "⚠️  couldn't transcribe that audio.")
		return
	}
	if transcript == "" {
		_ = origin.Reply(ctx, "🔇 I couldn't make out any speech in that audio.")
		return
	}
	t.log.Info("telegram: transcribed audio", "chars", len(transcript))
	select {
	case t.inbound <- Inbound{Text: transcript, Origin: origin}:
	default:
		t.log.Warn("telegram: inbound buffer full, dropping transcript")
	}
}

// downloadFile fetches a Telegram file's bytes by file_id.
func (t *Telegram) downloadFile(ctx context.Context, fileID string) ([]byte, error) {
	f, err := t.bot.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.bot.FileDownloadLink(f), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

type tgOrigin struct {
	bridge    *Telegram
	chatID    int64
	userID    int64
	username  string
	replyToID int
	voiceIn   bool       // the triggering message was a voice note (→ mirror with a spoken reply)
	mu        sync.Mutex // serializes Replies on the same origin
}

func (o *tgOrigin) Describe() string {
	if o.username != "" {
		return fmt.Sprintf("telegram(@%s)", o.username)
	}
	return fmt.Sprintf("telegram(id=%d)", o.userID)
}

// NotifyPending keeps the "typing" chat action alive (Telegram fades it after
// ~5s, so the 4s ticker re-arms it) and, if the turn outlasts heartbeatDelay,
// posts a one-line textual nudge so the user knows Claude isn't stuck on a long
// turn. That nudge is deleted once ctx is cancelled (reply ready / turn over),
// so quick turns stay clean and long ones leave nothing behind.
func (o *tgOrigin) NotifyPending(ctx context.Context) {
	typing := time.NewTicker(4 * time.Second)
	defer typing.Stop()
	heartbeat := time.NewTimer(heartbeatDelay)
	defer heartbeat.Stop()

	arm := func() {
		_, _ = o.bridge.bot.SendChatAction(ctx, &bot.SendChatActionParams{
			ChatID: o.chatID,
			Action: models.ChatActionTyping,
		})
	}

	var heartbeatID int
	arm() // first typing ping immediately
	for {
		select {
		case <-ctx.Done():
			if heartbeatID != 0 {
				// ctx is already cancelled, so clean up on a fresh,
				// short-lived context of our own.
				delCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, _ = o.bridge.bot.DeleteMessage(delCtx, &bot.DeleteMessageParams{
					ChatID:    o.chatID,
					MessageID: heartbeatID,
				})
				cancel()
			}
			return
		case <-typing.C:
			arm()
		case <-heartbeat.C:
			m, err := o.bridge.bot.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: o.chatID,
				Text:   heartbeatText,
			})
			if err == nil && m != nil {
				heartbeatID = m.ID
			}
		}
	}
}

func (o *tgOrigin) Reply(ctx context.Context, text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if strings.TrimSpace(text) == "" {
		text = "(empty response)"
	}
	// Spoken reply when the policy says so; fall back to text on any failure.
	if o.bridge.voice.Enabled() && o.bridge.voice.Policy.ShouldSpeak(o.voiceIn, text) {
		if err := o.replyVoice(ctx, text); err == nil {
			return nil
		} else {
			o.bridge.log.Warn("telegram: voice reply failed; sending text", "err", err)
		}
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
		err := withRetry(ctx, o.bridge.log, "telegram reply", func(actx context.Context) error {
			_, e := o.bridge.bot.SendMessage(actx, params)
			return e
		})
		if err != nil {
			return fmt.Errorf("telegram send: %w", err)
		}
		if i+1 < len(chunks) {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// replyVoice synthesizes text to an OGG/Opus note and sends it as a Telegram
// voice message. The caller holds o.mu.
func (o *tgOrigin) replyVoice(ctx context.Context, text string) error {
	path, err := o.bridge.voice.Synth.Synthesize(ctx, text)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	params := &bot.SendVoiceParams{
		ChatID: o.chatID,
		Voice:  &models.InputFileUpload{Filename: "reply.ogg", Data: bytes.NewReader(data)},
	}
	if o.replyToID != 0 {
		params.ReplyParameters = &models.ReplyParameters{MessageID: o.replyToID}
	}
	return withRetry(ctx, o.bridge.log, "telegram voice", func(actx context.Context) error {
		_, e := o.bridge.bot.SendVoice(actx, params)
		return e
	})
}

// SendTextToOwner pushes a proactive text message to the first allowed user.
// Serves as a Pusher for the Notifier. Waits briefly for startup.
func (t *Telegram) SendTextToOwner(ctx context.Context, text string) error {
	select {
	case <-t.ready:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("telegram bot not ready")
	}
	if len(t.cfg.AllowedUserIDs) == 0 {
		return fmt.Errorf("no allowed telegram users")
	}
	for _, chunk := range chunkText(text, tgMaxChars) {
		err := withRetry(ctx, t.log, "telegram notify", func(actx context.Context) error {
			_, e := t.bot.SendMessage(actx, &bot.SendMessageParams{
				ChatID: t.cfg.AllowedUserIDs[0],
				Text:   chunk,
			})
			return e
		})
		if err != nil {
			return fmt.Errorf("telegram notify: %w", err)
		}
	}
	return nil
}

// SendQRToOwner sends a QR image (e.g. a WhatsApp pairing code) to the first
// allowed user as an uncompressed DOCUMENT. Telegram recompresses photos, which
// blurs a dense QR enough that camera scanners fail — a document is delivered
// byte-for-byte. Serves as the WhatsApp QRSink. Waits briefly for the bot to
// finish starting if called early.
func (t *Telegram) SendQRToOwner(ctx context.Context, png []byte, caption string) error {
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
	return withRetry(ctx, t.log, "telegram qr", func(actx context.Context) error {
		_, e := t.bot.SendDocument(actx, &bot.SendDocumentParams{
			ChatID:   t.cfg.AllowedUserIDs[0],
			Document: &models.InputFileUpload{Filename: "whatsapp-qr.png", Data: bytes.NewReader(png)},
			Caption:  caption,
		})
		return e
	})
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
