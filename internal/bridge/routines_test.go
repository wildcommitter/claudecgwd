package bridge

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestParseScheduleAndDue(t *testing.T) {
	// every <dur>
	s, err := parseSchedule("every 6h")
	if err != nil || s.every != 6*time.Hour {
		t.Fatalf("every 6h: %+v err=%v", s, err)
	}
	now := time.Now()
	if s.due(now.Add(-5*time.Hour), now) {
		t.Error("every 6h should not be due after 5h")
	}
	if !s.due(now.Add(-7*time.Hour), now) {
		t.Error("every 6h should be due after 7h")
	}

	// hourly
	if s, _ := parseSchedule("hourly"); s.every != time.Hour {
		t.Error("hourly should be every 1h")
	}

	// daily HH:MM
	s, err = parseSchedule("daily 08:00")
	if err != nil || !s.daily || s.hh != 8 || s.mm != 0 {
		t.Fatalf("daily 08:00: %+v err=%v", s, err)
	}
	at := func(h, m int) time.Time {
		return time.Date(2026, 5, 29, h, m, 0, 0, time.Local)
	}
	// last ran yesterday; now is 08:30 today → boundary 08:00 today is after last → due.
	if !s.due(at(8, 0).AddDate(0, 0, -1), at(8, 30)) {
		t.Error("daily should be due after the boundary passes")
	}
	// already ran at 08:05 today; now 09:00 → boundary 08:00 today is before last → not due.
	if s.due(at(8, 5), at(9, 0)) {
		t.Error("daily should not re-fire same day")
	}

	for _, bad := range []string{"", "daily", "daily 99:99", "every", "every nonsense", "weekly mon"} {
		if _, err := parseSchedule(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestRoutinesTickFiresDue(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "routines.jsonl")
	// every 1ns → always due once anchored.
	os.WriteFile(store, []byte(`{"id":"r1","spec":"every 1ns","prompt":"digest"}`+"\n"), 0o600)

	var mu sync.Mutex
	var pushed []string
	pusher := func(_ context.Context, text string) error {
		mu.Lock()
		defer mu.Unlock()
		pushed = append(pushed, text)
		return nil
	}
	r := NewRoutines(store, "claude", dir, discardLogger(), pusher)
	r.runFn = func(_ context.Context, prompt string) (string, error) { return "result for " + prompt, nil }

	ctx := context.Background()
	r.tick(ctx) // first sight → anchors, does not fire
	mu.Lock()
	n0 := len(pushed)
	mu.Unlock()
	if n0 != 0 {
		t.Fatalf("first tick must anchor, not fire; got %d", n0)
	}
	r.tick(ctx) // now due (every 1ns)
	mu.Lock()
	defer mu.Unlock()
	if len(pushed) != 1 {
		t.Fatalf("expected one push, got %d: %v", len(pushed), pushed)
	}
	if !contains(pushed[0], "digest") || !contains(pushed[0], "result for") {
		t.Fatalf("push should carry the routine prompt + result: %q", pushed[0])
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
