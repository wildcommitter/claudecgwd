package bridge

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestIndexerTriggerCoalesces(t *testing.T) {
	ix := NewIndexer("rag", time.Minute, discardLogger())
	// Many triggers with no consumer collapse into a single pending signal —
	// non-blocking, so a burst of file arrivals can't wedge the caller.
	for i := 0; i < 100; i++ {
		ix.Trigger()
	}
	if got := len(ix.trigger); got != 1 {
		t.Fatalf("pending triggers = %d, want 1 (coalesced)", got)
	}
}

func TestIndexerRunsOnStartupTickAndTrigger(t *testing.T) {
	var mu sync.Mutex
	reasons := []string{}
	ix := NewIndexer("rag", 20*time.Millisecond, discardLogger())
	ix.debounce = time.Millisecond
	ix.runFn = func(context.Context) (string, error) {
		// Reason isn't passed to runFn; count invocations instead.
		mu.Lock()
		reasons = append(reasons, "run")
		mu.Unlock()
		return "indexed 0 new chunk(s)", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = ix.Run(ctx); close(done) }()

	// Startup run + a couple of ticks.
	time.Sleep(70 * time.Millisecond)
	ix.Trigger()
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	n := len(reasons)
	mu.Unlock()
	if n < 2 {
		t.Fatalf("expected several index passes (startup + ticks/trigger), got %d", n)
	}
}
