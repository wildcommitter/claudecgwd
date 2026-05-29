package bridge

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSchedulerFiresOnlyDueReminders(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "reminders.jsonl")
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	content := past + "\tid-past\tcall the dentist\n" +
		future + "\tid-future\tnot yet\n" +
		"garbage line without tabs\n"
	if err := os.WriteFile(store, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []string
	pusher := func(_ context.Context, text string) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, text)
		return nil
	}

	s := NewScheduler(store, discardLogger(), pusher)
	s.tick(context.Background())

	if len(got) != 1 {
		t.Fatalf("expected 1 reminder fired, got %d: %v", len(got), got)
	}
	if got[0] != "⏰ Reminder: call the dentist" {
		t.Fatalf("unexpected reminder text: %q", got[0])
	}

	// Firing again must not re-deliver the already-fired reminder.
	s.tick(context.Background())
	if len(got) != 1 {
		t.Fatalf("reminder re-fired: %v", got)
	}

	// A fresh scheduler over the same store must honour the fired sidecar.
	s2 := NewScheduler(store, discardLogger(), pusher)
	s2.tick(context.Background())
	if len(got) != 1 {
		t.Fatalf("fired sidecar not honoured across restart: %v", got)
	}
}

func TestSchedulerRetriesWhenAllPushersFail(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "reminders.jsonl")
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := os.WriteFile(store, []byte(past+"\tid1\thello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	failing := func(_ context.Context, _ string) error { return io.ErrClosedPipe }
	s := NewScheduler(store, discardLogger(), failing)
	s.tick(context.Background())
	if s.fired["id1"] {
		t.Fatal("reminder marked fired despite every push failing")
	}
}
