package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Network resilience policy. Outbound chat sends are single round-trips to the
// Telegram/WhatsApp servers, so a brief connectivity blip would otherwise drop
// a reply or a notification on the floor. withRetry rides out a short outage by
// retrying with exponential backoff, bounded so a genuinely-down link can't pin
// the (single-threaded) router forever — on a long outage we give up and log,
// and the bridge supervisor + the platform's own reconnect handle recovery.
const (
	retryAttempts = 6
	retryCap      = 10 * time.Second
)

// retryBase is the first backoff step (doubled each attempt). A var, not a
// const, only so tests can shrink it; production never changes it.
var retryBase = 500 * time.Millisecond

// withRetry runs fn, retrying on error with exponential backoff up to
// retryAttempts. It aborts immediately if ctx is cancelled (shutdown / turn
// timeout) rather than burning the budget on a doomed call. label names the
// operation for logs. Returns nil on the first success, or the last error
// wrapped once the attempts are exhausted.
//
// Sends are at-least-once: if a request reaches the server but the response is
// lost, a retry can deliver a duplicate. That's an accepted trade for not
// dropping messages, and rare in practice.
func withRetry(ctx context.Context, log *slog.Logger, label string, fn func() error) error {
	var err error
	for attempt := 1; attempt <= retryAttempts; attempt++ {
		if err = fn(); err == nil {
			if attempt > 1 && log != nil {
				log.Info("send recovered after retry", "op", label, "attempt", attempt)
			}
			return nil
		}
		// Don't keep trying once the caller's context is done — that error is
		// terminal (shutting down or the turn budget elapsed), not transient.
		if ctx.Err() != nil {
			return err
		}
		if attempt == retryAttempts {
			break
		}
		delay := retryBase << (attempt - 1)
		if delay > retryCap {
			delay = retryCap
		}
		if log != nil {
			log.Warn("send failed; retrying", "op", label, "attempt", attempt, "retry_in", delay, "err", err)
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("%s: gave up after %d attempts: %w", label, retryAttempts, err)
}
