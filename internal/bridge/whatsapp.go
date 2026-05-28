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
	cfg     config.WhatsAppConfig
	log     *slog.Logger
	inbound chan<- Inbound
	qrSink  QRSink

	allowed map[string]struct{} // bare sender phone numbers permitted to drive

	client *whatsmeow.Client

	// pendingAns holds, per chat JID, a channel awaiting the next inbound
	// message as the answer to an interactive question.
	amu        sync.Mutex
	pendingAns map[string]chan string
}

func NewWhatsApp(cfg config.WhatsAppConfig, log *slog.Logger, inbound chan<- Inbound, qrSink QRSink) *WhatsApp {
	allow := make(map[string]struct{}, len(cfg.AllowedJIDs))
	for _, j := range cfg.AllowedJIDs {
		allow[strings.TrimSpace(j)] = struct{}{}
	}
	return &WhatsApp{cfg: cfg, log: log, inbound: inbound, qrSink: qrSink, allowed: allow, pendingAns: map[string]chan string{}}
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
	if m.Info.IsFromMe {
		return
	}
	text := m.Message.GetConversation()
	if text == "" {
		text = m.Message.GetExtendedTextMessage().GetText()
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	sender := m.Info.Sender.ToNonAD().User
	if _, ok := w.allowed[sender]; !ok {
		w.log.Warn("whatsapp: rejecting unauthorized sender", "sender", sender)
		return
	}
	chat := m.Info.Chat

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

	origin := &waOrigin{bridge: w, chat: chat, sender: sender}
	select {
	case w.inbound <- Inbound{Text: text, Origin: origin}:
	default:
		w.log.Warn("whatsapp: inbound buffer full, dropping message", "sender", sender)
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
		_, err := o.bridge.client.SendMessage(ctx, o.chat, &waE2E.Message{Conversation: proto.String(chunk)})
		if err != nil {
			return fmt.Errorf("whatsapp send: %w", err)
		}
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
