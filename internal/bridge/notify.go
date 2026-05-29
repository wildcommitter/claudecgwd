package bridge

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Pusher delivers a proactive (unsolicited) text message to one chat surface.
type Pusher func(ctx context.Context, text string) error

// MediaPusher proactively delivers a file (image or document) to one chat
// surface, with an optional caption.
type MediaPusher func(ctx context.Context, path, caption string) error

// Notifier watches a FIFO for notification lines and broadcasts each to every
// configured surface. This is the proactive-push path: replies are tied to an
// inbound turn, but a watcher finishing later has no turn to ride on, so the
// assistant writes to the FIFO (via scripts/notify.sh) and the bridge fans it
// out. Lines are base64-encoded so multi-line notifications survive. A line that
// decodes to a {"file":...} JSON directive (from scripts/send-file) is delivered
// as media instead of text.
type Notifier struct {
	path    string
	log     *slog.Logger
	pushers []Pusher
	media   []MediaPusher
}

func NewNotifier(path string, log *slog.Logger, pushers ...Pusher) *Notifier {
	return &Notifier{path: NotifyPath(path), log: log, pushers: pushers}
}

// WithMedia registers the per-surface file senders used for {"file":...}
// directives. Returns the notifier for chaining.
func (n *Notifier) WithMedia(media ...MediaPusher) *Notifier {
	n.media = media
	return n
}

// NotifyPath resolves the FIFO path: the explicit arg, else
// $CLAUDECGWD_NOTIFY_FIFO, else ~/.local/share/assistant/notify.fifo.
func NotifyPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("CLAUDECGWD_NOTIFY_FIFO"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "assistant", "notify.fifo")
}

func (n *Notifier) Run(ctx context.Context) error {
	if len(n.pushers) == 0 {
		return nil // nothing to deliver to
	}
	if err := ensureFIFO(n.path); err != nil {
		return fmt.Errorf("notify fifo: %w", err)
	}
	// O_RDWR keeps a writer handle open on our side so reads block for data
	// instead of hitting EOF every time a writer closes.
	f, err := os.OpenFile(n.path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open notify fifo: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = f.Close()
	}()
	n.log.Info("notify path active", "fifo", n.path, "surfaces", len(n.pushers))

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		text := decodeNotif(line)
		if d, ok := parseMediaDirective(text); ok {
			if len(n.media) == 0 {
				n.log.Warn("media directive but no media surfaces configured; cannot deliver file", "file", d.File)
				continue
			}
			n.log.Info("broadcasting media", "file", d.File, "surfaces", len(n.media))
			for _, m := range n.media {
				if err := m(ctx, d.File, d.Caption); err != nil {
					n.log.Warn("media push failed", "err", err)
				}
			}
			continue
		}
		n.log.Info("broadcasting notification", "len", len(text))
		for _, p := range n.pushers {
			if err := p(ctx, text); err != nil {
				n.log.Warn("notify push failed", "err", err)
			}
		}
	}
	return nil
}

// mediaDirective is the JSON a scripts/send-file line decodes to.
type mediaDirective struct {
	File    string `json:"file"`
	Caption string `json:"caption"`
}

// parseMediaDirective recognizes a {"file":...} send-file directive. Plain text
// notifications aren't JSON objects, so they pass through untouched.
func parseMediaDirective(s string) (mediaDirective, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return mediaDirective{}, false
	}
	var d mediaDirective
	if err := json.Unmarshal([]byte(s), &d); err != nil || d.File == "" {
		return mediaDirective{}, false
	}
	return d, true
}

// decodeNotif base64-decodes a line, falling back to the raw text (so a manual
// `echo hello > fifo` still works).
func decodeNotif(line string) string {
	if b, err := base64.StdEncoding.DecodeString(line); err == nil {
		return string(b)
	}
	return line
}

func ensureFIFO(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err == nil {
		if fi.Mode()&os.ModeNamedPipe == 0 {
			return fmt.Errorf("%s exists but is not a FIFO", path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return syscall.Mkfifo(path, 0o600)
}
