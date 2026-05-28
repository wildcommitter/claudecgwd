package bridge

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"github.com/wildcommitter/claudecgwd/internal/claude"
	"github.com/wildcommitter/claudecgwd/internal/config"
)

func init() {
	// whatsmeow asks for the "sqlite3" SQL dialect; the CGO-free modernc driver
	// registers itself as "sqlite". Alias it under "sqlite3" (grabbing the
	// already-registered driver) so we keep a static, CGO-free binary.
	if !hasDriver("sqlite3") {
		if db, err := sql.Open("sqlite", ":memory:"); err == nil {
			sql.Register("sqlite3", db.Driver())
			_ = db.Close()
		}
	}
}

func hasDriver(name string) bool {
	for _, d := range sql.Drivers() {
		if d == name {
			return true
		}
	}
	return false
}

// QRSink delivers a pairing QR code (PNG) out-of-band so the user can scan it
// — here, by sending it through Telegram.
type QRSink func(ctx context.Context, png []byte, caption string) error

// WhatsApp bridges a personal WhatsApp account (via the whatsmeow multi-device
// / linked-device protocol) to the shared Claude session. Pairing is done once
// by scanning a QR code; subsequent runs resume from the stored session.
type WhatsApp struct {
	cfg      config.WhatsAppConfig
	log      *slog.Logger
	inbound  chan<- Inbound
	qrSink   QRSink
	inboxDir string       // where sent files are downloaded
	stt      *Transcriber // optional voice/audio transcription

	allowed map[string]struct{} // bare sender phone numbers permitted to drive

	client *whatsmeow.Client

	// pendingAns holds, per chat JID, a channel awaiting the next inbound
	// message as the answer to an interactive question.
	amu        sync.Mutex
	pendingAns map[string]chan string

	// sentIDs records IDs of messages we sent so we can skip their echoes.
	// In the linked-device model the bot runs on the operator's own account,
	// so its own replies come back as IsFromMe events and would otherwise loop.
	sentMu  sync.Mutex
	sentIDs map[string]struct{}
}

func NewWhatsApp(cfg config.WhatsAppConfig, log *slog.Logger, inbound chan<- Inbound, qrSink QRSink, inboxDir string, stt *Transcriber) *WhatsApp {
	allow := make(map[string]struct{}, len(cfg.AllowedJIDs))
	for _, j := range cfg.AllowedJIDs {
		allow[strings.TrimSpace(j)] = struct{}{}
	}
	return &WhatsApp{cfg: cfg, log: log, inbound: inbound, qrSink: qrSink, inboxDir: inboxDir, stt: stt, allowed: allow, pendingAns: map[string]chan string{}, sentIDs: map[string]struct{}{}}
}

func (w *WhatsApp) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(w.cfg.StorePath), 0o700); err != nil {
		return fmt.Errorf("whatsapp store dir: %w", err)
	}
	// modernc DSN pragma syntax (differs from mattn's _foreign_keys=on).
	dsn := "file:" + w.cfg.StorePath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)"
	container, err := sqlstore.New(ctx, "sqlite3", dsn, waLog.Noop)
	if err != nil {
		return fmt.Errorf("whatsapp sqlstore: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return fmt.Errorf("whatsapp device: %w", err)
	}
	w.client = whatsmeow.NewClient(device, waLog.Noop)
	w.client.AddEventHandler(w.handleEvent)

	if w.client.Store.ID == nil {
		// Not paired yet: surface a QR code for the user to scan.
		qrChan, _ := w.client.GetQRChannel(ctx)
		if err := w.client.Connect(); err != nil {
			return fmt.Errorf("whatsapp connect: %w", err)
		}
		go w.consumeQR(ctx, qrChan)
		w.log.Info("whatsapp: not paired, awaiting QR scan")
	} else {
		if err := w.client.Connect(); err != nil {
			return fmt.Errorf("whatsapp connect: %w", err)
		}
		w.log.Info("whatsapp: resumed existing session", "jid", w.client.Store.ID.String())
	}

	<-ctx.Done()
	w.client.Disconnect()
	return nil
}

// consumeQR renders each pairing code to a PNG and pushes it through the QR
// sink (Telegram). whatsmeow emits a fresh code periodically until paired, so a
// transient sink failure is retried on the next code.
func (w *WhatsApp) consumeQR(ctx context.Context, qrChan <-chan whatsmeow.QRChannelItem) {
	for evt := range qrChan {
		switch evt.Event {
		case "code":
			png, err := qrcode.Encode(evt.Code, qrcode.Medium, 768)
			if err != nil {
				w.log.Error("whatsapp: qr encode failed", "err", err)
				continue
			}
			caption := "📲 Link WhatsApp: open WhatsApp → Settings → Linked Devices → Link a device, then scan this. (Code refreshes periodically.)"
			if w.qrSink == nil {
				w.log.Warn("whatsapp: no QR sink configured; cannot deliver pairing code")
				continue
			}
			if err := w.qrSink(ctx, png, caption); err != nil {
				w.log.Warn("whatsapp: QR sink failed", "err", err)
			}
		case "success":
			w.log.Info("whatsapp: pairing successful")
		default:
			w.log.Info("whatsapp: qr event", "event", evt.Event)
		}
	}
}

// waMedia reports a suggested filename and caption if the message carries a
// downloadable attachment.
func waMedia(m *waE2E.Message) (name, caption string, ok bool) {
	switch {
	case m.GetImageMessage() != nil:
		return "image.jpg", m.GetImageMessage().GetCaption(), true
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		n := d.GetFileName()
		if n == "" {
			n = "document"
		}
		return n, d.GetCaption(), true
	case m.GetVideoMessage() != nil:
		return "video.mp4", m.GetVideoMessage().GetCaption(), true
	case m.GetAudioMessage() != nil:
		return "audio.ogg", "", true
	case m.GetStickerMessage() != nil:
		return "sticker.webp", "", true
	}
	return "", "", false
}

// transcribeAudio downloads a WhatsApp voice/audio message, transcribes it, and
// feeds the transcript in as the prompt (archiving the audio in the inbox).
func (w *WhatsApp) transcribeAudio(msg *waE2E.Message, origin *waOrigin) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	data, err := w.client.DownloadAny(ctx, msg)
	if err != nil {
		w.log.Warn("whatsapp: audio download failed", "err", err)
		_ = origin.Reply(ctx, "⚠️  couldn't download that audio: "+err.Error())
		return
	}
	path, err := saveInbox(w.inboxDir, "whatsapp", "voice.ogg", data)
	if err != nil {
		w.log.Warn("whatsapp: saving audio failed", "err", err)
	}
	transcript, err := w.stt.Transcribe(ctx, path)
	if err != nil {
		w.log.Warn("whatsapp: transcription failed", "err", err)
		_ = origin.Reply(ctx, "⚠️  couldn't transcribe that audio.")
		return
	}
	if transcript == "" {
		_ = origin.Reply(ctx, "🔇 I couldn't make out any speech in that audio.")
		return
	}
	w.log.Info("whatsapp: transcribed audio", "chars", len(transcript))
	select {
	case w.inbound <- Inbound{Text: transcript, Origin: origin}:
	default:
		w.log.Warn("whatsapp: inbound buffer full, dropping transcript")
	}
}

// saveMedia downloads a WhatsApp attachment to the inbox and enqueues a notice.
func (w *WhatsApp) saveMedia(msg *waE2E.Message, name, caption string, origin *waOrigin) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	data, err := w.client.DownloadAny(ctx, msg)
	if err != nil {
		w.log.Warn("whatsapp: file download failed", "err", err)
		_ = origin.Reply(ctx, "⚠️  couldn't download that file: "+err.Error())
		return
	}
	path, err := saveInbox(w.inboxDir, "whatsapp", name, data)
	if err != nil {
		w.log.Warn("whatsapp: saving file failed", "err", err)
		_ = origin.Reply(ctx, "⚠️  couldn't save that file: "+err.Error())
		return
	}
	w.log.Info("whatsapp: saved incoming file", "path", path, "bytes", len(data))
	text := "[file received via whatsapp — saved to " + path + "]"
	if caption != "" {
		text += "\n" + caption
	}
	select {
	case w.inbound <- Inbound{Text: text, Origin: origin}:
	default:
		w.log.Warn("whatsapp: inbound buffer full, dropping file notice")
	}
}

// PushToOwner sends a proactive message to the operator's own (self) chat.
// Serves as a Pusher for the Notifier.
func (w *WhatsApp) PushToOwner(ctx context.Context, text string) error {
	if w.client == nil || w.client.Store.ID == nil {
		return fmt.Errorf("whatsapp not connected")
	}
	to := w.client.Store.ID.ToNonAD()
	for _, chunk := range chunkText(text, 4000) {
		resp, err := w.client.SendMessage(ctx, to, &waE2E.Message{Conversation: proto.String(chunk)})
		if err != nil {
			return fmt.Errorf("whatsapp notify: %w", err)
		}
		w.markSent(string(resp.ID))
	}
	return nil
}

// markSent records a message ID we sent so its echo can be skipped. Bounded so
// it can't grow without limit over a long-lived session.
func (w *WhatsApp) markSent(id string) {
	if id == "" {
		return
	}
	w.sentMu.Lock()
	defer w.sentMu.Unlock()
	if len(w.sentIDs) > 2000 {
		w.sentIDs = map[string]struct{}{}
	}
	w.sentIDs[id] = struct{}{}
}

func (w *WhatsApp) wasSent(id string) bool {
	w.sentMu.Lock()
	defer w.sentMu.Unlock()
	_, ok := w.sentIDs[id]
	return ok
}

func (w *WhatsApp) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		w.onMessage(v)
	case *events.Connected:
		w.log.Info("whatsapp: connected")
	case *events.LoggedOut:
		w.log.Warn("whatsapp: logged out (re-pair needed)", "on_connect", v.OnConnect)
	}
}

func (w *WhatsApp) onMessage(m *events.Message) {
	// The bot runs as a linked device on the operator's own account, so the
	// operator talks to it via the "Message Yourself" chat — those messages are
	// IsFromMe. Accept those, but skip (a) the bot's own replies echoing back
	// and (b) the operator's messages to *other* chats (don't butt in).
	chatUser := m.Info.Chat.ToNonAD().User
	senderUser := m.Info.Sender.ToNonAD().User
	// The operator drives the bot from the "Message Yourself" chat. In a from-me
	// event the sender is always the operator; if the chat is the operator too
	// (chat == sender) it's that self-chat. Comparing chat vs sender works
	// regardless of whether WhatsApp addresses it as a phone JID or an @lid.
	selfChat := m.Info.IsFromMe && chatUser == senderUser

	rawText := m.Message.GetConversation()
	if rawText == "" {
		rawText = m.Message.GetExtendedTextMessage().GetText()
	}

	if m.Info.IsFromMe {
		if w.wasSent(string(m.Info.ID)) {
			return // our own outgoing reply echoing back
		}
		if !selfChat {
			return // operator messaging someone else — don't butt in
		}
		// from-me + self-chat = the operator's control channel; authorized.
	} else {
		// Incoming from someone else: require an allowlisted sender.
		if _, ok := w.allowed[senderUser]; !ok {
			w.log.Warn("whatsapp: rejecting unauthorized sender", "sender", senderUser)
			return
		}
	}

	// Reply target: for the self-chat, send to our own phone-number JID (the
	// visible "Message Yourself" thread) rather than the @lid the event came in
	// on — replies to the @lid don't surface in that chat on the phone.
	chat := m.Info.Chat
	if selfChat && w.client.Store.ID != nil {
		chat = w.client.Store.ID.ToNonAD()
	}
	origin := &waOrigin{bridge: w, chat: chat, sender: senderUser}

	// Voice/audio? Transcribe and feed the text in as the prompt.
	if w.stt.Enabled() && m.Message.GetAudioMessage() != nil {
		w.log.Info("whatsapp: accepted audio for transcription", "self_chat", selfChat)
		go w.transcribeAudio(m.Message, origin)
		return
	}

	// File attachment? Download it (off the event goroutine) and notify the
	// session with the saved path. Checked before the empty-text return since
	// media messages carry no conversation text.
	if name, caption, ok := waMedia(m.Message); ok {
		w.log.Info("whatsapp: accepted file", "self_chat", selfChat)
		go w.saveMedia(m.Message, name, caption, origin)
		return
	}

	text := strings.TrimSpace(rawText)
	if text == "" {
		return
	}
	w.log.Info("whatsapp: accepted message", "self_chat", selfChat, "from_me", m.Info.IsFromMe)

	// Route to a pending interactive-question waiter if one is active.
	w.amu.Lock()
	ans := w.pendingAns[chat.String()]
	w.amu.Unlock()
	if ans != nil {
		select {
		case ans <- text:
		default:
		}
		return
	}

	select {
	case w.inbound <- Inbound{Text: text, Origin: origin}:
	default:
		w.log.Warn("whatsapp: inbound buffer full, dropping message", "sender", senderUser)
	}
}

// askQuestion presents a numbered list and treats the user's next WhatsApp
// message on this chat as the selection.
func (w *WhatsApp) askQuestion(ctx context.Context, o *waOrigin, q claude.Question) (claude.Answer, error) {
	var sb strings.Builder
	if q.Header != "" {
		sb.WriteString(q.Header + ": ")
	}
	sb.WriteString(q.Question + "\n")
	for i, opt := range q.Options {
		fmt.Fprintf(&sb, "%d. %s", i+1, opt.Label)
		if opt.Description != "" {
			sb.WriteString(" — " + opt.Description)
		}
		sb.WriteString("\n")
	}
	if q.MultiSelect {
		sb.WriteString("(reply with the numbers, comma-separated)")
	} else {
		sb.WriteString("(reply with the number)")
	}
	if err := o.Reply(ctx, sb.String()); err != nil {
		return claude.Answer{}, err
	}

	ch := make(chan string, 1)
	key := o.chat.String()
	w.amu.Lock()
	w.pendingAns[key] = ch
	w.amu.Unlock()
	defer func() {
		w.amu.Lock()
		delete(w.pendingAns, key)
		w.amu.Unlock()
	}()

	select {
	case reply := <-ch:
		return parseChoiceReply(reply, len(q.Options), q.MultiSelect), nil
	case <-ctx.Done():
		return claude.Answer{}, ctx.Err()
	}
}

// waOrigin is the reply target for one WhatsApp chat.
type waOrigin struct {
	bridge *WhatsApp
	chat   types.JID
	sender string
}

func (o *waOrigin) Describe() string { return fmt.Sprintf("whatsapp(%s)", o.sender) }

// NotifyPending shows a "typing…" presence, re-armed periodically until done.
func (o *waOrigin) NotifyPending(ctx context.Context) {
	t := time.NewTicker(8 * time.Second)
	defer t.Stop()
	for {
		_ = o.bridge.client.SendChatPresence(ctx, o.chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)
		select {
		case <-ctx.Done():
			_ = o.bridge.client.SendChatPresence(context.Background(), o.chat, types.ChatPresencePaused, types.ChatPresenceMediaText)
			return
		case <-t.C:
		}
	}
}

func (o *waOrigin) Reply(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		text = "(empty response)"
	}
	// WhatsApp tolerates long messages, but keep chunks reasonable.
	chunks := chunkText(text, 4000)
	for i, chunk := range chunks {
		resp, err := o.bridge.client.SendMessage(ctx, o.chat, &waE2E.Message{Conversation: proto.String(chunk)})
		if err != nil {
			o.bridge.log.Warn("whatsapp: send failed", "to", o.chat.String(), "err", err)
			return fmt.Errorf("whatsapp send: %w", err)
		}
		o.bridge.log.Info("whatsapp: sent reply", "to", o.chat.String(), "id", string(resp.ID), "chunk", i+1)
		o.bridge.markSent(string(resp.ID)) // so the echo of our own reply is ignored
		if i+1 < len(chunks) {
			time.Sleep(150 * time.Millisecond)
		}
	}
	return nil
}

func (o *waOrigin) AskChoices(ctx context.Context, qs []claude.Question) ([]claude.Answer, error) {
	out := make([]claude.Answer, len(qs))
	for i, q := range qs {
		ans, err := o.bridge.askQuestion(ctx, o, q)
		if err != nil {
			return nil, err
		}
		out[i] = ans
	}
	return out, nil
}

// parseChoiceReply turns "2", "1,3", or free text into an Answer. Numbers are
// 1-based in the message and converted to 0-based indices, bounded to nOptions.
func parseChoiceReply(reply string, nOptions int, multi bool) claude.Answer {
	fields := strings.FieldsFunc(reply, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	var idxs []int
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 1 || n > nOptions {
			continue
		}
		idxs = append(idxs, n-1)
		if !multi {
			break
		}
	}
	if len(idxs) == 0 {
		return claude.Answer{FreeText: strings.TrimSpace(reply)}
	}
	return claude.Answer{Indices: idxs}
}
