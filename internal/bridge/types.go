package bridge

import (
	"context"
	"time"

	"github.com/wildcommitter/claudecgwd/internal/claude"
)

// heartbeatDelay is how long a turn must run before NotifyPending posts a
// textual "working…" nudge on top of the platform typing indicator. Quick
// turns finish well under this and stay silent; the typing dots are enough for
// them and a posted-then-deleted line would only flicker. Once a turn outlasts
// this, the nudge reassures the user that Claude isn't stuck, and it is removed
// when the reply lands.
const heartbeatDelay = 20 * time.Second

// heartbeatText is the one-line nudge posted on long turns.
const heartbeatText = "⏳ working on it…"

// Origin knows how to deliver a reply back to whoever sent an Inbound message.
// Each bridge defines its own concrete Origin implementation.
type Origin interface {
	// Describe returns a short human-readable tag, e.g. "telegram(@alice)".
	// Used both for logging and for the routing prefix passed to Claude so the
	// model knows where a message came from.
	Describe() string

	// NotifyPending is called between message receipt and reply dispatch and
	// should signal "I'm working" on the origin platform (e.g. Telegram's
	// typing indicator). Implementations must loop until ctx is cancelled
	// since most platforms time the hint out after a few seconds. For
	// platforms without such an affordance, this can be a no-op.
	NotifyPending(ctx context.Context)

	// Reply sends text to the origin platform. Implementations are responsible
	// for chunking long messages to fit platform limits.
	Reply(ctx context.Context, text string) error

	// AskChoices presents one or more interactive multiple-choice questions
	// (from Claude's AskUserQuestion tool) to the user and blocks until they
	// respond, returning one Answer per question in order. Implementations use
	// whatever native affordance fits the platform (e.g. Telegram inline
	// buttons). It must honor ctx cancellation (turn timeout) so a parked
	// question can't pin the session forever.
	AskChoices(ctx context.Context, qs []claude.Question) ([]claude.Answer, error)
}

// Inbound is the unit of work flowing from bridges to the router.
type Inbound struct {
	Text   string
	Origin Origin
}

// Bridge is the lifecycle interface every chat bridge implements.
type Bridge interface {
	// Run blocks until ctx is cancelled or a fatal error occurs.
	Run(ctx context.Context) error
}
