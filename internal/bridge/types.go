package bridge

import "context"

// Origin knows how to deliver a reply back to whoever sent an Inbound message.
// Each bridge defines its own concrete Origin implementation.
type Origin interface {
	// Describe returns a short human-readable tag, e.g. "telegram(@alice)" or
	// "irc(libera/#chan:alice)". Used both for logging and for the routing
	// prefix passed to Claude so the model knows where a message came from.
	Describe() string

	// NotifyPending is called between message receipt and reply dispatch and
	// should signal "I'm working" on the origin platform (e.g. Telegram's
	// typing indicator). Implementations must loop until ctx is cancelled
	// since most platforms time the hint out after a few seconds. For
	// platforms without such an affordance (IRC), this can be a no-op.
	NotifyPending(ctx context.Context)

	// Reply sends text to the origin platform. Implementations are responsible
	// for chunking long messages to fit platform limits.
	Reply(ctx context.Context, text string) error
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
