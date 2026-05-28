package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/wildcommitter/claudecgwd/internal/claude"
)

// Sender is what the router needs from the Claude driver. Defined as an
// interface so the router can be tested with a stub. The ask callback lets the
// driver surface an interactive AskUserQuestion menu back to the origin
// mid-turn.
type Sender interface {
	Send(ctx context.Context, prompt string, ask claude.ChoiceAsker) (string, error)
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

	pendingCtx, pendingCancel := context.WithCancel(sendCtx)
	go msg.Origin.NotifyPending(pendingCtx)
	defer pendingCancel()

	// AskChoices uses the parent ctx, not sendCtx, so a slow human reply
	// can't trip the per-turn budget. The driver guarantees AskChoices is
	// only invoked while it has a parked tool_use waiting on a tool_result —
	// once an answer comes back, the rest of the turn falls back under
	// sendCtx normally.
	ask := func(_ context.Context, qs []claude.Question) ([]claude.Answer, error) {
		return msg.Origin.AskChoices(ctx, qs)
	}
	reply, err := r.driver.Send(sendCtx, tagged, ask)
	pendingCancel()
	if err != nil {
		r.log.Error("driver send failed", "err", err, "origin", tag)
		_ = msg.Origin.Reply(ctx, "⚠️  assistant error: "+err.Error())
		return
	}
	if err := msg.Origin.Reply(ctx, reply); err != nil {
		r.log.Error("reply failed", "err", err, "origin", tag)
	}
}
