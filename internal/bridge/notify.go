package bridge

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Pusher delivers a proactive (unsolicited) message to one chat surface.
type Pusher func(ctx context.Context, text string) error

// Notifier watches a FIFO for notification lines and broadcasts each to every
// configured surface. This is the proactive-push path: replies are tied to an
// inbound turn, but a watcher finishing later has no turn to ride on, so the
// assistant writes to the FIFO (via scripts/notify.sh) and the bridge fans it
// out. Lines are base64-encoded so multi-line notifications survive.
type Notifier struct {
	path    string
	log     *slog.Logger
	pushers []Pusher
}

func NewNotifier(path string, log *slog.Logger, pushers ...Pusher) *Notifier {
	return &Notifier{path: NotifyPath(path), log: log, pushers: pushers}
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
		n.log.Info("broadcasting notification", "len", len(text))
		for _, p := range n.pushers {
			if err := p(ctx, text); err != nil {
				n.log.Warn("notify push failed", "err", err)
			}
		}
	}
	return nil
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
