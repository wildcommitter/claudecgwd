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

// retryBase is the first backoff step (doubled each attempt). retryAttemptTimeout
// bounds a single attempt. Both are vars (not consts) only so tests can shrink
// them; production never changes them.
var (
	retryBase           = 500 * time.Millisecond
	retryAttemptTimeout = 12 * time.Second
)

// withRetry runs fn, retrying on error with exponential backoff up to
// retryAttempts. label names the operation for logs. Returns nil on the first
// success, or the last error wrapped once the attempts are exhausted.
//
// Each attempt gets its OWN bounded context (retryAttemptTimeout). This is the
// crucial bit for a network drop: a send whose connection hangs would otherwise
// block on the OS TCP timeout (minutes) and never give the retry loop a turn —
// the per-attempt deadline cancels the stuck call so the next attempt can run.
// If the *parent* ctx is cancelled (shutdown / turn budget) we stop entirely,
// since that error is terminal rather than transient.
//
// Sends are at-least-once: if a request reaches the server but the response is
// lost (or an attempt is cancelled after the server got it), a retry can deliver
// a duplicate. That's an accepted trade for not dropping messages, and rare.
func withRetry(ctx context.Context, log *slog.Logger, label string, fn func(context.Context) error) error {
	var err error
	for attempt := 1; attempt <= retryAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, retryAttemptTimeout)
		err = fn(attemptCtx)
		cancel()
		if err == nil {
			if attempt > 1 && log != nil {
				log.Info("send recovered after retry", "op", label, "attempt", attempt)
			}
			return nil
		}
		// Stop only if the *parent* context is done — a per-attempt timeout
		// (attemptCtx) is exactly the transient case we want to retry.
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
