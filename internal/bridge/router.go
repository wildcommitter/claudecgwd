package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
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
	projects   *ProjectRegistry
	ragCmd     string // path to scripts/rag for the /search command ("" disables it)
	inbound    <-chan Inbound
	log        *slog.Logger
	turnBudget time.Duration
}

func NewRouter(driver Sender, ctl SessionController, projects *ProjectRegistry, ragCmd string, inbound <-chan Inbound, log *slog.Logger, turnBudget time.Duration) *Router {
	if turnBudget <= 0 {
		turnBudget = 5 * time.Minute
	}
	return &Router{driver: driver, ctl: ctl, projects: projects, ragCmd: ragCmd, inbound: inbound, log: log, turnBudget: turnBudget}
}

func (r *Router) Run(ctx context.Context) error {
	r.log.Info("router started", "turn_budget", r.turnBudget)
	// Seed the registry with the starting project so it's tracked even before
	// the first /project switch.
	if r.projects != nil && r.ctl != nil {
		wd, _ := r.ctl.Info()
		if err := r.projects.Record(wd); err != nil {
			r.log.Warn("project record failed", "dir", wd, "err", err)
		}
	}
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
	"/project <name|dir> — switch project (name is wildcard-matched against tracked projects)\n" +
	"/projects — list tracked project directories\n" +
	"/search <query> — semantic search over attachments + past conversations\n" +
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
	case "new", "project", "projects", "search", "status", "help":
		return name, arg, true
	}
	return "", "", false
}

// looksLikePath reports whether arg should be treated as a literal directory
// path rather than a project-name pattern to wildcard-match.
func looksLikePath(arg string) bool {
	return strings.ContainsAny(arg, "/~") || arg == "." || arg == ".."
}

// handleSearch runs a semantic search via the rag script and replies with the
// ranked snippets. It's a raw retrieval view — distinct from auto-retrieval,
// where the assistant queries the index itself mid-turn and synthesizes.
func (r *Router) handleSearch(ctx context.Context, reply func(string), query string) {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, r.ragCmd, "query", query, "-k", "5").CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		r.log.Warn("search failed", "err", err)
		msg := "⚠️  search failed: " + err.Error()
		if text != "" {
			msg += "\n" + text
		}
		reply(msg)
		return
	}
	if text == "" {
		reply("No matches.")
		return
	}
	reply("🔎 Results for \"" + query + "\":\n\n" + text)
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
	case "search":
		if r.ragCmd == "" {
			reply("Search isn't configured.")
			return
		}
		if arg == "" {
			reply("Usage: /search <query>")
			return
		}
		r.handleSearch(ctx, reply, arg)
	case "projects":
		if r.projects == nil {
			reply("Project tracking isn't enabled.")
			return
		}
		list := r.projects.List()
		if len(list) == 0 {
			reply("No tracked projects yet.")
			return
		}
		reply("📚 Tracked projects (most recent first):\n" + strings.Join(list, "\n"))
	case "project":
		if arg == "" {
			reply("Usage: /project <name|dir>  (e.g. /project bridge, /project myrepo, /project ~/code/foo)")
			return
		}
		target := arg
		// Wildcard-resolve a bare name against tracked projects by default; an
		// explicit path (contains / or ~) is taken literally.
		if r.projects != nil && !looksLikePath(arg) {
			matches := r.projects.Resolve(arg)
			switch {
			case len(matches) == 1:
				target = matches[0]
			case len(matches) > 1:
				reply("🔎 Several tracked projects match \"" + arg + "\":\n" +
					strings.Join(matches, "\n") + "\n\nRe-run /project with a more specific name or a full path.")
				return
				// len 0: fall through with target=arg so a real dir name under
				// $HOME still resolves via SwitchProject.
			}
		}
		wd, err := r.ctl.SwitchProject(ctx, target)
		if err != nil {
			r.log.Warn("switch project failed", "err", err, "arg", arg)
			hint := ""
			if r.projects != nil && !looksLikePath(arg) {
				if list := r.projects.List(); len(list) > 0 {
					hint = "\n\nTracked projects:\n" + strings.Join(list, "\n")
				}
			}
			reply("⚠️  couldn't switch project: " + err.Error() + hint)
			return
		}
		if r.projects != nil {
			if err := r.projects.Record(wd); err != nil {
				r.log.Warn("project record failed", "dir", wd, "err", err)
			}
		}
		r.log.Info("switched project", "workdir", wd)
		reply("📂 Switched to " + wd + " — fresh conversation here.")
	}
}
