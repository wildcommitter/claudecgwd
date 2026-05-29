package bridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWithRetry(t *testing.T) {
	// Shrink the backoff so the retrying tests don't actually wait seconds.
	orig := retryBase
	retryBase = time.Millisecond
	defer func() { retryBase = orig }()

	t.Run("succeeds on first try, no retry", func(t *testing.T) {
		calls := 0
		err := withRetry(context.Background(), discardLogger(), "op", func() error {
			calls++
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("calls=%d err=%v, want 1 call, nil", calls, err)
		}
	})

	t.Run("recovers after a few transient failures", func(t *testing.T) {
		calls := 0
		err := withRetry(context.Background(), discardLogger(), "op", func() error {
			calls++
			if calls < 3 {
				return errors.New("temporary network glitch")
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("calls=%d err=%v, want 3 calls, nil", calls, err)
		}
	})

	t.Run("gives up after the attempt cap", func(t *testing.T) {
		calls := 0
		err := withRetry(context.Background(), discardLogger(), "op", func() error {
			calls++
			return errors.New("still down")
		})
		if err == nil || calls != retryAttempts {
			t.Fatalf("calls=%d err=%v, want %d calls and an error", calls, err, retryAttempts)
		}
	})

	t.Run("aborts immediately when ctx is already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := withRetry(ctx, discardLogger(), "op", func() error {
			calls++
			return errors.New("down")
		})
		// One attempt happens, then the cancelled ctx stops further retries.
		if err == nil || calls != 1 {
			t.Fatalf("calls=%d err=%v, want exactly 1 call and an error", calls, err)
		}
	})
}
