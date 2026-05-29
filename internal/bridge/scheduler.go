package bridge

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Scheduler fires one-shot reminders as proactive pings. It complements the
// Notifier: where the FIFO delivers an immediate push, the Scheduler delivers a
// future one. The assistant creates reminders with scripts/remind, which
// appends a line to the store; the Scheduler polls the store and, when a
// reminder comes due, pushes it to every configured surface via the same
// Pusher path the Notifier uses.
//
// The store is an append-only TSV file ("<RFC3339>\t<id>\t<message>"), chosen
// over JSON so the shell script needs no escaping. Fired ids are recorded in a
// sidecar so a restart doesn't re-fire them; a reminder that came due while the
// service was down fires on the next tick after startup.
type Scheduler struct {
	storePath string
	firedPath string
	interval  time.Duration
	log       *slog.Logger
	pushers   []Pusher

	fired map[string]bool
}

// Reminder is one scheduled ping.
type Reminder struct {
	FireAt  time.Time
	ID      string
	Message string
}

func NewScheduler(storePath string, log *slog.Logger, pushers ...Pusher) *Scheduler {
	storePath = RemindersPath(storePath)
	s := &Scheduler{
		storePath: storePath,
		firedPath: storePath + ".fired",
		interval:  20 * time.Second,
		log:       log,
		pushers:   pushers,
		fired:     map[string]bool{},
	}
	s.loadFired() // honour reminders already delivered in a previous run
	return s
}

// RemindersPath resolves the store path: the explicit arg, else
// $CLAUDECGWD_REMINDERS, else ~/.local/share/assistant/reminders.jsonl.
func RemindersPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("CLAUDECGWD_REMINDERS"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "assistant", "reminders.jsonl")
}

func (s *Scheduler) Run(ctx context.Context) error {
	if len(s.pushers) == 0 {
		return nil // nothing to deliver to
	}
	s.log.Info("reminder scheduler active", "store", s.storePath, "surfaces", len(s.pushers))

	t := time.NewTicker(s.interval)
	defer t.Stop()
	s.tick(ctx) // catch anything already due (e.g. fired while the service was down)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick fires every reminder that is now due and not yet fired.
func (s *Scheduler) tick(ctx context.Context) {
	rs, err := s.load()
	if err != nil {
		s.log.Warn("reminder store read failed", "err", err)
		return
	}
	now := time.Now()
	for _, r := range rs {
		if s.fired[r.ID] || r.FireAt.After(now) {
			continue
		}
		text := "⏰ Reminder: " + r.Message
		anyOK := false
		for _, p := range s.pushers {
			if err := p(ctx, text); err != nil {
				s.log.Warn("reminder push failed", "id", r.ID, "err", err)
				continue
			}
			anyOK = true
		}
		// Mark fired once it reached at least one surface; if every surface
		// failed (e.g. transiently down), leave it for the next tick to retry.
		if anyOK {
			s.markFired(r.ID)
			s.log.Info("reminder fired", "id", r.ID, "due", r.FireAt.Format(time.RFC3339))
		}
	}
}

// load reads the reminder store. Missing file = no reminders. Malformed lines
// are skipped so a partial write can't wedge the scheduler.
func (s *Scheduler) load() ([]Reminder, error) {
	f, err := os.Open(s.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Reminder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		fireAt, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		out = append(out, Reminder{FireAt: fireAt, ID: parts[1], Message: parts[2]})
	}
	return out, nil
}

func (s *Scheduler) loadFired() {
	f, err := os.Open(s.firedPath)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if id := strings.TrimSpace(sc.Text()); id != "" {
			s.fired[id] = true
		}
	}
}

// markFired records an id both in memory and in the sidecar so it survives a
// restart.
func (s *Scheduler) markFired(id string) {
	s.fired[id] = true
	if err := os.MkdirAll(filepath.Dir(s.firedPath), 0o700); err != nil {
		s.log.Warn("reminder fired-sidecar mkdir failed", "err", err)
		return
	}
	f, err := os.OpenFile(s.firedPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		s.log.Warn("reminder fired-sidecar open failed", "err", err)
		return
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, id); err != nil {
		s.log.Warn("reminder fired-sidecar write failed", "err", err)
	}
}
