package bridge

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWithRetry(t *testing.T) {
	// Shrink the backoff + per-attempt timeout so the tests don't wait seconds.
	origBase, origTO := retryBase, retryAttemptTimeout
	retryBase = time.Millisecond
	retryAttemptTimeout = 20 * time.Millisecond
	defer func() { retryBase, retryAttemptTimeout = origBase, origTO }()

	t.Run("succeeds on first try, no retry", func(t *testing.T) {
		calls := 0
		err := withRetry(context.Background(), discardLogger(), "op", func(context.Context) error {
			calls++
			return nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("calls=%d err=%v, want 1 call, nil", calls, err)
		}
	})

	t.Run("recovers after a few transient failures", func(t *testing.T) {
		calls := 0
		err := withRetry(context.Background(), discardLogger(), "op", func(context.Context) error {
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
		err := withRetry(context.Background(), discardLogger(), "op", func(context.Context) error {
			calls++
			return errors.New("still down")
		})
		if err == nil || calls != retryAttempts {
			t.Fatalf("calls=%d err=%v, want %d calls and an error", calls, err, retryAttempts)
		}
	})

	t.Run("a hung attempt is cancelled by its deadline, then retried", func(t *testing.T) {
		calls := 0
		err := withRetry(context.Background(), discardLogger(), "op", func(actx context.Context) error {
			calls++
			if calls == 1 {
				<-actx.Done() // simulate a connection that hangs until cancelled
				return actx.Err()
			}
			return nil // network "recovered" on the next attempt
		})
		if err != nil || calls != 2 {
			t.Fatalf("calls=%d err=%v, want the hung attempt timed out then a 2nd succeeded", calls, err)
		}
	})

	t.Run("aborts immediately when parent ctx is already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := withRetry(ctx, discardLogger(), "op", func(context.Context) error {
			calls++
			return errors.New("down")
		})
		if err == nil || calls != 1 {
			t.Fatalf("calls=%d err=%v, want exactly 1 call and an error", calls, err)
		}
	})
}
