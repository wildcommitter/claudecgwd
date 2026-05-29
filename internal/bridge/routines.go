package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Routines runs scheduled, proactive tasks: at the appointed time it feeds a
// prompt to a fresh headless `claude -p` run (a separate session, so it never
// pollutes the live conversation) and pushes the result to every surface via
// the same Pusher fan-out the notifier/reminders use.
//
// Routines are created with scripts/routine (the assistant calls it when the
// user asks for a recurring task). The store is JSONL ({id,spec,prompt}); a
// sidecar tracks each routine's last run so recurrence survives restarts.
type Routines struct {
	storePath string
	statePath string
	claudeBin string
	workdir   string
	interval  time.Duration
	log       *slog.Logger
	pushers   []Pusher

	mu      sync.Mutex
	lastRun map[string]time.Time

	// runFn executes one routine prompt; overridable in tests.
	runFn func(ctx context.Context, prompt string) (string, error)
}

type routine struct {
	ID     string `json:"id"`
	Spec   string `json:"spec"`
	Prompt string `json:"prompt"`
}

func NewRoutines(storePath, claudeBin, workdir string, log *slog.Logger, pushers ...Pusher) *Routines {
	if storePath == "" {
		home, _ := os.UserHomeDir()
		storePath = filepath.Join(home, ".local", "share", "assistant", "routines.jsonl")
	}
	r := &Routines{
		storePath: storePath,
		statePath: storePath + ".state",
		claudeBin: claudeBin,
		workdir:   workdir,
		interval:  30 * time.Second,
		log:       log,
		pushers:   pushers,
		lastRun:   map[string]time.Time{},
	}
	r.runFn = r.defaultRun
	r.loadState()
	return r
}

func (r *Routines) Run(ctx context.Context) error {
	if len(r.pushers) == 0 {
		return nil
	}
	r.log.Info("routines scheduler active", "store", r.storePath, "surfaces", len(r.pushers))
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// tick fires every routine that is now due.
func (r *Routines) tick(ctx context.Context) {
	routines, err := r.load()
	if err != nil {
		r.log.Warn("routines store read failed", "err", err)
		return
	}
	now := time.Now()
	for _, rt := range routines {
		sched, err := parseSchedule(rt.Spec)
		if err != nil {
			r.log.Warn("bad routine spec", "id", rt.ID, "spec", rt.Spec, "err", err)
			continue
		}
		lr, seen := r.getLast(rt.ID)
		if !seen {
			// First time we see this routine: anchor its clock to now so it
			// doesn't fire retroactively / immediately.
			r.setLast(rt.ID, now)
			continue
		}
		if !sched.due(lr, now) {
			continue
		}
		r.log.Info("routine firing", "id", rt.ID, "spec", rt.Spec)
		r.setLast(rt.ID, now) // mark before running so a long run doesn't double-fire
		r.fire(ctx, rt)
	}
}

func (r *Routines) fire(ctx context.Context, rt routine) {
	out, err := r.runFn(ctx, rt.Prompt)
	if err != nil {
		r.log.Warn("routine run failed", "id", rt.ID, "err", err)
		out = "⚠️ routine failed: " + err.Error()
	}
	header := rt.Prompt
	if len(header) > 80 {
		header = header[:80] + "…"
	}
	msg := "🔁 Routine — " + header + "\n\n" + out
	for _, p := range r.pushers {
		if err := p(ctx, msg); err != nil {
			r.log.Warn("routine push failed", "id", rt.ID, "err", err)
		}
	}
}

func (r *Routines) defaultRun(ctx context.Context, prompt string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.claudeBin, "-p", prompt, "--dangerously-skip-permissions")
	cmd.Dir = r.workdir
	cmd.Env = append(os.Environ(), "TERM=dumb")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// --- schedule parsing ---

type schedule struct {
	daily  bool
	hh, mm int
	every  time.Duration
}

// parseSchedule understands "daily HH:MM", "hourly", and "every <duration>"
// (e.g. every 30m, every 6h).
func parseSchedule(spec string) (schedule, error) {
	f := strings.Fields(strings.ToLower(strings.TrimSpace(spec)))
	if len(f) == 0 {
		return schedule{}, fmt.Errorf("empty spec")
	}
	switch f[0] {
	case "daily":
		if len(f) != 2 {
			return schedule{}, fmt.Errorf("daily needs HH:MM")
		}
		var hh, mm int
		if _, err := fmt.Sscanf(f[1], "%d:%d", &hh, &mm); err != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
			return schedule{}, fmt.Errorf("bad time %q", f[1])
		}
		return schedule{daily: true, hh: hh, mm: mm}, nil
	case "hourly":
		return schedule{every: time.Hour}, nil
	case "every":
		if len(f) != 2 {
			return schedule{}, fmt.Errorf("every needs a duration")
		}
		d, err := time.ParseDuration(f[1])
		if err != nil || d <= 0 {
			return schedule{}, fmt.Errorf("bad duration %q", f[1])
		}
		return schedule{every: d}, nil
	}
	return schedule{}, fmt.Errorf("unknown spec %q", spec)
}

// due reports whether the routine should fire now given when it last ran.
func (s schedule) due(lastRun, now time.Time) bool {
	if s.every > 0 {
		return now.Sub(lastRun) >= s.every
	}
	if s.daily {
		boundary := time.Date(now.Year(), now.Month(), now.Day(), s.hh, s.mm, 0, 0, now.Location())
		if boundary.After(now) {
			boundary = boundary.AddDate(0, 0, -1)
		}
		return boundary.After(lastRun)
	}
	return false
}

// --- store / state ---

func (r *Routines) load() ([]routine, error) {
	f, err := os.Open(r.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []routine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rt routine
		if err := json.Unmarshal([]byte(line), &rt); err != nil || rt.ID == "" {
			continue
		}
		out = append(out, rt)
	}
	return out, nil
}

func (r *Routines) getLast(id string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.lastRun[id]
	return t, ok
}

func (r *Routines) setLast(id string, t time.Time) {
	r.mu.Lock()
	r.lastRun[id] = t
	r.mu.Unlock()
	r.saveState()
}

func (r *Routines) loadState() {
	f, err := os.Open(r.statePath)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.SplitN(strings.TrimSpace(sc.Text()), "\t", 2)
		if len(parts) != 2 {
			continue
		}
		if t, err := time.Parse(time.RFC3339, parts[1]); err == nil {
			r.lastRun[parts[0]] = t
		}
	}
}

func (r *Routines) saveState() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(r.statePath), 0o700); err != nil {
		return
	}
	var b strings.Builder
	for id, t := range r.lastRun {
		fmt.Fprintf(&b, "%s\t%s\n", id, t.UTC().Format(time.RFC3339))
	}
	tmp := r.statePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, r.statePath)
}
