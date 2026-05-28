package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Sender is what the router needs from the Claude driver. Defined as an
// interface so the router can be tested with a stub.
type Sender interface {
	Send(ctx context.Context, prompt string) (string, error)
}

type Router struct {
	driver     Sender
	inbound    <-chan Inbound
	log        *slog.Logger
	turnBudget time.Duration
}

func NewRouter(driver Sender, inbound <-chan Inbound, log *slog.Logger, turnBudget time.Duration) *Router {
	if turnBudget <= 0 {
		turnBudget = 5 * time.Minute
	}
	return &Router{driver: driver, inbound: inbound, log: log, turnBudget: turnBudget}
}

func (r *Router) Run(ctx context.Context) error {
	r.log.Info("router started", "turn_budget", r.turnBudget)
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-r.inbound:
			if !ok {
				return nil
			}
			r.handle(ctx, msg)
		}
	}
}

func (r *Router) handle(ctx context.Context, msg Inbound) {
	tag := msg.Origin.Describe()
	r.log.Info("handling message", "origin", tag, "len", len(msg.Text))

	// Tag the prompt so Claude knows where it came from. This is plain text
	// (not JSON) because Claude reads it as natural language.
	tagged := fmt.Sprintf("[from %s]\n%s", tag, msg.Text)

	sendCtx, cancel := context.WithTimeout(ctx, r.turnBudget)
	defer cancel()

	reply, err := r.driver.Send(sendCtx, tagged)
	if err != nil {
		r.log.Error("driver send failed", "err", err, "origin", tag)
		_ = msg.Origin.Reply(ctx, "⚠️  assistant error: "+err.Error())
		return
	}
	if err := msg.Origin.Reply(ctx, reply); err != nil {
		r.log.Error("reply failed", "err", err, "origin", tag)
	}
}
