package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lrstanley/girc"

	"github.com/wildcommitter/claudecgwd/internal/config"
)

const (
	ircMaxLineBytes = 400 // safe under the 510-byte IRC line limit including overhead
	ircSendInterval = 700 * time.Millisecond
)

type IRC struct {
	cfg     config.IRCConfig
	log     *slog.Logger
	inbound chan<- Inbound

	client   *girc.Client
	allowedAccounts map[string]struct{}
	allowedNicks    map[string]struct{}

	sendMu sync.Mutex // serializes outbound sends across all origins for rate limiting
}

func NewIRC(cfg config.IRCConfig, log *slog.Logger, inbound chan<- Inbound) *IRC {
	accts := make(map[string]struct{}, len(cfg.AllowedAccounts))
	for _, a := range cfg.AllowedAccounts {
		accts[strings.ToLower(a)] = struct{}{}
	}
	nicks := make(map[string]struct{}, len(cfg.AllowedNicks))
	for _, n := range cfg.AllowedNicks {
		nicks[strings.ToLower(n)] = struct{}{}
	}
	return &IRC{cfg: cfg, log: log, inbound: inbound, allowedAccounts: accts, allowedNicks: nicks}
}

func (b *IRC) Run(ctx context.Context) error {
	host, port := splitHostPort(b.cfg.Server)
	gcfg := girc.Config{
		Server: host,
		Port:   port,
		Nick:   b.cfg.Nick,
		User:   firstNonEmpty(b.cfg.User, b.cfg.Nick),
		Name:   firstNonEmpty(b.cfg.RealName, b.cfg.Nick),
		SSL:    b.cfg.TLS,
		SupportedCaps: map[string][]string{
			"account-tag":      nil,
			"server-time":      nil,
			"message-tags":     nil,
			"extended-join":    nil,
			"account-notify":   nil,
		},
	}
	if b.cfg.SaslUser != "" && b.cfg.SaslPass != "" {
		gcfg.SASL = &girc.SASLPlain{User: b.cfg.SaslUser, Pass: b.cfg.SaslPass}
	}

	b.client = girc.New(gcfg)
	b.client.Handlers.Add(girc.RPL_WELCOME, b.onWelcome)
	b.client.Handlers.Add(girc.PRIVMSG, b.onMessage)

	// Reconnect with backoff until ctx is cancelled.
	go func() {
		<-ctx.Done()
		b.client.Close()
	}()

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		b.log.Info("irc connecting", "server", b.cfg.Server)
		err := b.client.Connect()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			b.log.Warn("irc connect failed", "err", err, "retry_in", backoff)
		} else {
			b.log.Info("irc disconnected, will reconnect", "retry_in", backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

func (b *IRC) onWelcome(c *girc.Client, e girc.Event) {
	if len(b.cfg.Channels) > 0 {
		c.Cmd.Join(b.cfg.Channels...)
	}
	b.log.Info("irc connected", "nick", c.GetNick())
}

func (b *IRC) onMessage(c *girc.Client, e girc.Event) {
	if e.Source == nil {
		return
	}
	text := e.Last()
	target := ""
	if len(e.Params) > 0 {
		target = e.Params[0]
	}
	isChan := e.IsFromChannel()

	// Authorize: prefer SASL account tag, fall back to nick allowlist.
	if !b.isAuthorized(e) {
		return
	}

	// In channels, require the message be addressed to our nick.
	if isChan {
		prefix := strings.ToLower(c.GetNick())
		lower := strings.ToLower(text)
		stripped := ""
		for _, sep := range []string{": ", ", ", " "} {
			if strings.HasPrefix(lower, prefix+sep) {
				stripped = strings.TrimSpace(text[len(prefix)+len(sep):])
				break
			}
		}
		if stripped == "" {
			return
		}
		text = stripped
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	replyTarget := target
	addressNick := ""
	if isChan {
		addressNick = e.Source.Name // prefix the reply with "nick: " in channels
	} else {
		replyTarget = e.Source.Name // DM: reply to nick
	}

	origin := &ircOrigin{
		bridge:      b,
		target:      replyTarget,
		addressNick: addressNick,
	}

	select {
	case b.inbound <- Inbound{Text: text, Origin: origin}:
	default:
		b.log.Warn("irc: inbound buffer full, dropping", "from", e.Source.Name)
	}
}

func (b *IRC) isAuthorized(e girc.Event) bool {
	if len(b.allowedAccounts) > 0 {
		if acct, ok := e.Tags.Get("account"); ok && acct != "" {
			if _, allow := b.allowedAccounts[strings.ToLower(acct)]; allow {
				return true
			}
		}
	}
	if len(b.allowedNicks) > 0 && e.Source != nil {
		if _, allow := b.allowedNicks[strings.ToLower(e.Source.Name)]; allow {
			return true
		}
	}
	return false
}

type ircOrigin struct {
	bridge      *IRC
	target      string // channel or nick
	addressNick string // for channel replies, prefix with "nick: " on the first line
}

func (o *ircOrigin) Describe() string {
	if o.addressNick != "" {
		return fmt.Sprintf("irc(%s:%s)", o.target, o.addressNick)
	}
	return fmt.Sprintf("irc(/msg %s)", o.target)
}

// NotifyPending is a no-op on IRC — there's no per-target "typing" affordance.
func (o *ircOrigin) NotifyPending(ctx context.Context) {}

func (o *ircOrigin) Reply(ctx context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		text = "(empty response)"
	}
	lines := splitIRCLines(text, ircMaxLineBytes)
	o.bridge.sendMu.Lock()
	defer o.bridge.sendMu.Unlock()
	for i, line := range lines {
		if i == 0 && o.addressNick != "" {
			line = o.addressNick + ": " + line
		} else if i > 0 {
			line = "  " + line // indent continuation lines
		}
		o.bridge.client.Cmd.Message(o.target, line)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ircSendInterval):
		}
	}
	return nil
}

// splitIRCLines splits text into IRC-safe lines: first by newline, then
// re-wraps any line longer than maxBytes at word boundaries (UTF-8 safe via
// byte-anchored search for ASCII space).
func splitIRCLines(text string, maxBytes int) []string {
	var out []string
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimRight(raw, "\r ")
		if raw == "" {
			continue
		}
		for len(raw) > maxBytes {
			cut := maxBytes
			if i := strings.LastIndex(raw[:maxBytes], " "); i > maxBytes/2 {
				cut = i
			}
			out = append(out, strings.TrimSpace(raw[:cut]))
			raw = strings.TrimSpace(raw[cut:])
		}
		if raw != "" {
			out = append(out, raw)
		}
	}
	return out
}

func splitHostPort(s string) (string, int) {
	host := s
	port := 6667
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host = s[:i]
		var p int
		_, err := fmt.Sscanf(s[i+1:], "%d", &p)
		if err == nil && p > 0 {
			port = p
		}
	}
	return host, port
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
