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
	// Generation increments on each session (re)start so the router can re-inject
	// persistent memory into the first prompt of a new session.
	Generation() int
}

type Router struct {
	driver     Sender
	ctl        SessionController
	projects   *ProjectRegistry
	voice      *VoiceOut    // for the /voice command ("" disables it)
	memory     *MemoryStore // persistent user facts, injected per session (nil disables)
	ragCmd     string       // path to scripts/rag for the /search command ("" disables it)
	gcalCmd    string       // path to scripts/gcal-auth for /calauth ("" disables it)
	inbound    <-chan Inbound
	log        *slog.Logger
	turnBudget time.Duration
	started    time.Time

	lastMemGen int // session generation memory was last injected for
}

func NewRouter(driver Sender, ctl SessionController, projects *ProjectRegistry, voice *VoiceOut, memory *MemoryStore, ragCmd, gcalCmd string, inbound <-chan Inbound, log *slog.Logger, turnBudget time.Duration) *Router {
	if turnBudget <= 0 {
		turnBudget = 5 * time.Minute
	}
	return &Router{driver: driver, ctl: ctl, projects: projects, voice: voice, memory: memory, ragCmd: ragCmd, gcalCmd: gcalCmd, inbound: inbound, log: log, turnBudget: turnBudget, started: time.Now(), lastMemGen: -1}
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

	// On the first real prompt of a new session, prepend persistent memory so
	// the assistant knows the user across /new, /project, and restarts.
	tagged = r.withMemory(tagged)

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

// withMemory prepends the persistent-memory preamble to a prompt once per
// session (detected via the driver's generation counter), so the facts ride the
// first real turn after a start / /new / /project without re-injecting on every
// message.
func (r *Router) withMemory(tagged string) string {
	if r.memory == nil || r.ctl == nil {
		return tagged
	}
	gen := r.ctl.Generation()
	if gen == r.lastMemGen {
		return tagged
	}
	r.lastMemGen = gen
	if pre := r.memory.Preamble(); pre != "" {
		r.log.Info("injecting persistent memory", "session_gen", gen)
		return pre + "\n\n" + tagged
	}
	return tagged
}

// controlHelp lists the bridge commands and is shown by /help.
const controlHelp = "Session commands:\n" +
	"/new — start a fresh conversation (clears context)\n" +
	"/project <name|dir> — switch project (name is wildcard-matched against tracked projects)\n" +
	"/projects — list tracked project directories\n" +
	"/search <query> — semantic search over attachments + past conversations\n" +
	"/calauth — connect Google Calendar (sends a consent link to paste back)\n" +
	"/voice <on|off|auto> — spoken replies: always / never / mirror voice notes\n" +
	"/speech <language|country> — set the audio language (transcription + voice)\n" +
	"/memory — list what I remember about you\n" +
	"/forget <text|all> — drop remembered facts matching text (or everything)\n" +
	"/status — show the current project and session\n" +
	"/health — uptime + a snapshot of the bridge's state\n" +
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
	case "new", "project", "projects", "search", "calauth", "voice", "speech", "memory", "forget", "status", "health", "help":
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

// handleCalAuth drives the chat-based Google Calendar OAuth handshake by
// shelling out to scripts/gcal-auth. It's a two-step copy-paste flow so it works
// on a headless server where the user is on a messenger:
//
//	/calauth                  → print a consent URL to open and authorize
//	/calauth <code-or-url>    → exchange the pasted code / redirect URL for a token
//
// `/calauth url` and `/calauth exchange <x>` map to the same two steps for users
// who type the subcommand explicitly. Both messengers reach this identically.
func (r *Router) handleCalAuth(ctx context.Context, reply func(string), arg string) {
	if r.gcalCmd == "" {
		reply("Calendar auth isn't configured.")
		return
	}

	// Resolve the subcommand. Default (no arg) starts the flow with `url`; a bare
	// argument that isn't the literal "url" is taken as the pasted code/redirect
	// URL to exchange.
	var scriptArgs []string
	exchanging := false
	switch {
	case arg == "" || strings.EqualFold(arg, "url"):
		scriptArgs = []string{"url"}
	case strings.HasPrefix(strings.ToLower(arg), "exchange"):
		code := strings.TrimSpace(arg[len("exchange"):])
		if code == "" {
			reply("Paste the code: /calauth exchange <code-or-redirect-url>")
			return
		}
		scriptArgs = []string{"exchange", code}
		exchanging = true
	default:
		scriptArgs = []string{"exchange", arg}
		exchanging = true
	}

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, r.gcalCmd, scriptArgs...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		r.log.Warn("calauth failed", "err", err, "step", scriptArgs[0])
		msg := "⚠️  calendar auth failed"
		if text != "" {
			msg += ":\n" + text
		}
		reply(msg)
		return
	}

	if exchanging {
		reply("✅ Google Calendar connected. " + text)
		return
	}
	// `url` step: either already authorized, or a consent URL to relay with
	// copy-paste instructions for the redirect.
	if strings.HasPrefix(text, "http") {
		reply("🔗 Open this link, grant access, then paste back the URL you land on " +
			"(it'll look like a broken http://localhost page — that's fine, the address bar is what matters):\n\n" +
			text + "\n\nThen send:  /calauth <paste-the-url-or-code-here>")
		return
	}
	reply("📅 " + text)
}

// healthReport assembles a one-shot snapshot of the bridge's state.
func (r *Router) healthReport(ctx context.Context) string {
	var b strings.Builder
	b.WriteString("🩺 Health\n")
	fmt.Fprintf(&b, "Uptime: %s\n", time.Since(r.started).Round(time.Second))
	if r.ctl != nil {
		wd, sid := r.ctl.Info()
		fmt.Fprintf(&b, "Project: %s\nSession: %s\n", wd, sid)
	}
	if r.voice != nil && r.voice.Lang != nil {
		fmt.Fprintf(&b, "Audio language: %s\n", r.voice.Lang.Current().Name)
	}
	if r.voice != nil && r.voice.Policy != nil {
		voice := "off (no TTS engine)"
		if r.voice.Enabled() {
			voice = r.voice.Policy.Mode().String()
		}
		fmt.Fprintf(&b, "Voice replies: %s\n", voice)
	}
	if r.ragCmd != "" {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err := exec.CommandContext(cctx, r.ragCmd, "stats").CombinedOutput()
		cancel()
		if err == nil {
			b.WriteString("RAG index — " + strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", "; ") + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
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
	case "health":
		reply(r.healthReport(ctx))
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
	case "calauth":
		r.handleCalAuth(ctx, reply, arg)
	case "voice":
		if !r.voice.Enabled() {
			reply("Voice replies aren't enabled.")
			return
		}
		if arg == "" {
			reply("🔊 Voice replies are " + r.voice.Policy.Mode().String() +
				". Use /voice on (always), off (never), or auto (mirror voice notes).")
			return
		}
		m, ok := parseVoiceMode(arg)
		if !ok {
			reply("Usage: /voice <on|off|auto>")
			return
		}
		r.voice.Policy.Set(m)
		reply("🔊 Voice replies set to " + m.String() + ".")
	case "speech":
		if r.voice == nil || r.voice.Lang == nil {
			reply("Language switching isn't available.")
			return
		}
		if arg == "" {
			reply("🗣️ Audio language: " + r.voice.Lang.Describe() +
				"\n\nSet it with /speech <language or country>, e.g. /speech spanish, /speech mexico, /speech de.\n\nSupported: " + languageList())
			return
		}
		l, ok := lookupLanguage(arg)
		if !ok {
			reply("Unknown language \"" + arg + "\". Try a language or country name/code (e.g. spanish, fr, mexico). Supported: " + languageList())
			return
		}
		r.voice.Lang.Set(l)
		msg := "🗣️ Audio language set to " + l.Name + " (transcribe: " + whisperLabel(l.Whisper) + ")."
		if l.Piper == "" {
			msg += " No voice for this language — spoken replies will fall back to text."
		} else {
			msg += " The voice model downloads on first use."
		}
		reply(msg)
	case "memory":
		if r.memory == nil {
			reply("Persistent memory isn't enabled.")
			return
		}
		facts := r.memory.List()
		if len(facts) == 0 {
			reply("🧠 I don't have anything remembered yet. Tell me to remember something, and it'll stick across sessions.")
			return
		}
		reply("🧠 What I remember about you:\n- " + strings.Join(facts, "\n- "))
	case "forget":
		if r.memory == nil {
			reply("Persistent memory isn't enabled.")
			return
		}
		if arg == "" {
			reply("Usage: /forget <text>  (or /forget all to clear everything)")
			return
		}
		n, err := r.memory.Forget(arg)
		if err != nil {
			reply("⚠️  couldn't update memory: " + err.Error())
			return
		}
		if n == 0 {
			reply("Nothing matched \"" + arg + "\".")
			return
		}
		reply(fmt.Sprintf("🧠 Forgot %d memory(ies).", n))
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
