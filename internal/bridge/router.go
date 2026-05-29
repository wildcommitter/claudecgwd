package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

// SessionController lets chat control commands (/new, /project, /status)
// reconfigure the live Claude session. The concrete claude.Driver implements
// it; it is optional (nil disables the commands), which keeps the router
// testable with a bare Sender stub.
type SessionController interface {
	NewSession(ctx context.Context) (string, error)
	SwitchProject(ctx context.Context, dir string) (string, error)
	Info() (workdir, sessionID string)
}

type Router struct {
	driver     Sender
	ctl        SessionController
	inbound    <-chan Inbound
	log        *slog.Logger
	turnBudget time.Duration
}

func NewRouter(driver Sender, ctl SessionController, inbound <-chan Inbound, log *slog.Logger, turnBudget time.Duration) *Router {
	if turnBudget <= 0 {
		turnBudget = 5 * time.Minute
	}
	return &Router{driver: driver, ctl: ctl, inbound: inbound, log: log, turnBudget: turnBudget}
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

	// Bridge-level control commands (/new, /project, …) are handled here and
	// never reach Claude. Unknown slash text falls through as a normal prompt.
	if r.ctl != nil {
		if name, arg, ok := parseControl(msg.Text); ok {
			r.log.Info("control command", "origin", tag, "cmd", name)
			r.handleControl(ctx, msg, name, arg)
			return
		}
	}

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

	// On an upstream stall, transparently retry once. Crucially, do NOT
	// cancel pendingCtx between attempts — that keeps the platform "typing"
	// indicator alive across the gap so the user sees uninterrupted activity
	// (Telegram chat-action, WhatsApp composing presence).
	if errors.Is(err, claude.ErrStalled) {
		r.log.Warn("upstream stalled; retrying once", "origin", tag, "partial_len", len(reply))
		notice := "⚠️  Claude stalled — retrying once…"
		if reply != "" {
			notice = reply + "\n\n— — —\n" + notice
		}
		_ = msg.Origin.Reply(ctx, notice)
		reply, err = r.driver.Send(sendCtx, tagged, ask)
	}

	pendingCancel()

	if err != nil {
		switch {
		case errors.Is(err, claude.ErrStalled):
			r.log.Warn("upstream stalled on retry; giving up", "origin", tag, "partial_len", len(reply))
			out := "⚠️  Claude stalled again after the retry. The session is usable; please send the message again."
			if reply != "" {
				out = reply + "\n\n— — —\n" + out
			}
			_ = msg.Origin.Reply(ctx, out)
		default:
			r.log.Error("driver send failed", "err", err, "origin", tag)
			_ = msg.Origin.Reply(ctx, "⚠️  assistant error: "+err.Error())
		}
		return
	}
	if err := msg.Origin.Reply(ctx, reply); err != nil {
		r.log.Error("reply failed", "err", err, "origin", tag)
	}
}

// controlHelp lists the bridge commands and is shown by /help.
const controlHelp = "Session commands:\n" +
	"/new — start a fresh conversation (clears context)\n" +
	"/project <dir> — switch to another project dir (fresh conversation there)\n" +
	"/status — show the current project and session\n" +
	"/help — this message"

// parseControl recognizes a bridge-level control command. It returns the
// command name (without slash, lower-cased) and any trailing argument. ok is
// false for non-commands and for unknown slash text (which is passed to Claude
// so its own slash commands still work).
func parseControl(text string) (name, arg string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return "", "", false
	}
	fields := strings.SplitN(t, " ", 2)
	name = strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	if len(fields) == 2 {
		arg = strings.TrimSpace(fields[1])
	}
	switch name {
	case "new", "project", "status", "help":
		return name, arg, true
	}
	return "", "", false
}

// handleControl executes a control command and replies on the origin.
func (r *Router) handleControl(ctx context.Context, msg Inbound, name, arg string) {
	reply := func(text string) {
		if err := msg.Origin.Reply(ctx, text); err != nil {
			r.log.Error("control reply failed", "err", err, "cmd", name)
		}
	}
	switch name {
	case "help":
		reply(controlHelp)
	case "status":
		wd, sid := r.ctl.Info()
		reply(fmt.Sprintf("📋 Project: %s\nSession: %s", wd, sid))
	case "new":
		sid, err := r.ctl.NewSession(ctx)
		if err != nil {
			r.log.Error("new session failed", "err", err)
			reply("⚠️  couldn't start a new session: " + err.Error())
			return
		}
		r.log.Info("started new session", "session", sid)
		reply("🆕 Fresh conversation started — the previous context is cleared.")
	case "project":
		if arg == "" {
			reply("Usage: /project <dir>  (e.g. /project myrepo or /project ~/code/foo)")
			return
		}
		wd, err := r.ctl.SwitchProject(ctx, arg)
		if err != nil {
			r.log.Warn("switch project failed", "err", err, "arg", arg)
			reply("⚠️  couldn't switch project: " + err.Error())
			return
		}
		r.log.Info("switched project", "workdir", wd)
		reply("📂 Switched to " + wd + " — fresh conversation here.")
	}
}
