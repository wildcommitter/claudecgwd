// Package claude drives an interactive `claude` (Claude Code) session under
// a pseudo-tty, parses the TUI with a virtual terminal emulator, and exposes
// a simple Send(prompt) -> response API.
package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"

	"github.com/wildcommitter/claudecgwd/internal/config"
)

const (
	startupTimeout = 60 * time.Second
	pollInterval   = 50 * time.Millisecond
	// How long the PTY must be quiet (no bytes written) before we consider a
	// ready/busy classification reliable. Claude streams in bursts with gaps;
	// too-short a window triggers in the middle of a stream.
	defaultIdleMs = 800
	// After detecting "first ready" on startup, give the TUI this much
	// additional time to render one-time hints / announcements before we
	// snapshot the screen for the first user turn.
	startupSettle = 2 * time.Second
	// Phase 1 of waitForResponse waits up to this long to observe the busy
	// state appearing. If Claude responds instantly we accept that.
	phase1Timeout = 5 * time.Second
	// After the TUI returns to ready, how long to poll the session transcript
	// for the assistant's reply to be flushed to disk before giving up and
	// falling back to the screen scrape.
	transcriptFlushWait = 3 * time.Second
)

// Driver owns a single long-lived `claude` PTY session.
type Driver struct {
	cfg config.ClaudeConfig
	log *slog.Logger

	cmd     *exec.Cmd
	ptyFile *os.File
	term    vt10x.Terminal

	// transcript reads the authoritative reply text from Claude Code's session
	// JSONL, sidestepping the lossy TUI screen-scrape.
	transcript *transcriptReader

	// sendMu serializes Send calls so only one prompt is in flight.
	sendMu sync.Mutex

	// stateMu guards lastWriteAt.
	stateMu     sync.Mutex
	lastWriteAt time.Time

	done chan struct{}
}

// New constructs a Driver. Call Start to spawn the child.
func New(cfg config.ClaudeConfig, log *slog.Logger) *Driver {
	if log == nil {
		log = slog.Default()
	}
	return &Driver{
		cfg:        cfg,
		log:        log,
		done:       make(chan struct{}),
		transcript: newTranscriptReader(cfg.SessionID, cfg.Workdir),
	}
}

// Start spawns the claude binary under a PTY and waits for the first
// ready-prompt before returning.
func (d *Driver) Start(ctx context.Context) error {
	args := d.buildArgs()
	d.log.Info("spawning claude", "binary", d.cfg.Binary, "args", args, "workdir", d.cfg.Workdir)

	d.cmd = exec.CommandContext(ctx, d.cfg.Binary, args...)
	d.cmd.Dir = d.cfg.Workdir
	// Pass through env but force a sane TERM. xterm-256color is what vt10x parses best.
	d.cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	sz := &pty.Winsize{Cols: d.cfg.PtyCols, Rows: d.cfg.PtyRows}
	f, err := pty.StartWithSize(d.cmd, sz)
	if err != nil {
		return fmt.Errorf("pty start: %w", err)
	}
	d.ptyFile = f
	d.term = vt10x.New(vt10x.WithSize(int(d.cfg.PtyCols), int(d.cfg.PtyRows)))

	go d.readLoop()
	go d.waitLoop()

	startCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err := d.waitReady(startCtx); err != nil {
		return fmt.Errorf("waiting for first ready prompt: %w", err)
	}
	// Let the TUI render any one-time announcements (e.g. "Opus 4.X is here",
	// "Tip: ...") before the first user turn snapshots the screen.
	select {
	case <-time.After(startupSettle):
	case <-ctx.Done():
		return ctx.Err()
	}
	d.log.Info("claude ready")
	return nil
}

// Done is closed when the underlying claude process exits.
func (d *Driver) Done() <-chan struct{} { return d.done }

// Close terminates the claude child.
func (d *Driver) Close() error {
	if d.cmd == nil || d.cmd.Process == nil {
		return nil
	}
	// Try graceful first.
	_, _ = d.ptyFile.Write([]byte{0x03}) // Ctrl-C
	time.Sleep(200 * time.Millisecond)
	_ = d.cmd.Process.Kill()
	if d.ptyFile != nil {
		_ = d.ptyFile.Close()
	}
	return nil
}

// Send writes prompt into the TUI, waits for the next ready-prompt, and
// returns the captured response text.
func (d *Driver) Send(ctx context.Context, prompt string, ask ChoiceAsker) (string, error) {
	d.sendMu.Lock()
	defer d.sendMu.Unlock()

	beforeScreen := d.snapshotScreen()
	d.dumpScreen("before", beforeScreen)
	// Watermark the transcript before sending so we can read back exactly the
	// lines this turn appends.
	transcriptOffset := d.transcript.offset()

	if err := d.writePrompt(prompt); err != nil {
		return "", err
	}

	// If caller didn't set a deadline, fall back to a sensible upper bound so
	// a stuck TUI can't pin the driver forever.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	// Primary path: wait for the transcript to record a terminal stop_reason
	// (end_turn / max_tokens / stop_sequence / refusal). The TUI returns to
	// "ready" between tool calls too, so a TUI-only signal would slice the
	// turn into fragments — only the transcript knows whether more is coming.
	if d.transcript.path() != "" {
		reply, err := d.awaitTurn(ctx, transcriptOffset, ask)
		switch {
		case err == nil:
			return reply, nil
		case errors.Is(err, ErrStalled):
			// Propagate verbatim so the router can render a friendly timeout
			// message instead of treating this as a generic failure.
			return reply, err
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// Fall through to the TUI scrape fallback.
			d.log.Warn("transcript wait cancelled; falling back to TUI scrape",
				"err", err, "transcript", d.transcript.describe())
		default:
			d.log.Warn("transcript wait failed; falling back to TUI scrape",
				"err", err, "transcript", d.transcript.describe())
		}
	}

	// Fallback: TUI-based readiness + screen scrape. Only reached if the
	// transcript can't be located or ctx expired before end_turn.
	if err := d.waitForResponse(ctx); err != nil {
		return "", fmt.Errorf("waiting for response: %w", err)
	}
	if reply, ok := d.transcript.waitForReplySince(transcriptOffset, transcriptFlushWait); ok {
		return reply, nil
	}
	afterScreen := d.snapshotScreen()
	d.dumpScreen("after", afterScreen)
	return extractResponse(beforeScreen, afterScreen, prompt), nil
}

// dumpScreen writes a screen snapshot to $CLAUDECGWD_DUMP_DIR if set.
// No-op otherwise.
func (d *Driver) dumpScreen(label, screen string) {
	dir := os.Getenv("CLAUDECGWD_DUMP_DIR")
	if dir == "" {
		return
	}
	ts := time.Now().UnixNano()
	path := filepath.Join(dir, fmt.Sprintf("screen-%d-%s.txt", ts, label))
	_ = os.WriteFile(path, []byte(screen), 0o644)
	d.log.Info("screen dumped", "label", label, "path", path)
}

// writePrompt sends the user prompt followed by Enter. We use bracketed-paste
// markers so the TUI treats the message body as a single paste event, then
// send Enter as a separate keystroke after a brief pause so the TUI has a
// chance to commit the paste before submitting.
func (d *Driver) writePrompt(prompt string) error {
	const (
		bpStart = "\x1b[200~"
		bpEnd   = "\x1b[201~"
	)
	if _, err := io.WriteString(d.ptyFile, bpStart+prompt+bpEnd); err != nil {
		return fmt.Errorf("write paste: %w", err)
	}
	time.Sleep(120 * time.Millisecond)
	if _, err := io.WriteString(d.ptyFile, "\r"); err != nil {
		return fmt.Errorf("write enter: %w", err)
	}
	time.Sleep(80 * time.Millisecond)
	return nil
}

// waitReady blocks until the screen shows the input-prompt anchor AND no
// busy marker AND the PTY has been idle for at least defaultIdleMs.
// Used by Start() for the very first ready detection.
func (d *Driver) waitReady(ctx context.Context) error {
	idle := time.Duration(defaultIdleMs) * time.Millisecond
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.done:
			return errors.New("claude process exited")
		case <-t.C:
		}
		d.stateMu.Lock()
		quiet := time.Since(d.lastWriteAt) >= idle
		d.stateMu.Unlock()
		if quiet && d.screenIsReady() {
			return nil
		}
	}
}

// waitForResponse handles the post-submission wait. It uses a two-phase
// strategy so we don't declare ready before Claude even starts working:
//
//  1. Wait up to phase1Timeout for the screen to enter a "busy" state (a
//     spinner / status keyword appears). If busy never appears (the response
//     was instant), we accept whatever ready state we observe at the end of
//     phase 1.
//  2. Once busy was observed, wait for the busy markers to clear AND the
//     screen to be idle AND the prompt anchor to be visible.
func (d *Driver) waitForResponse(ctx context.Context) error {
	idle := time.Duration(defaultIdleMs) * time.Millisecond

	phase1Deadline := time.Now().Add(phase1Timeout)
	sawBusy := false
	t := time.NewTicker(pollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.done:
			return errors.New("claude process exited")
		case <-t.C:
		}

		d.stateMu.Lock()
		quiet := time.Since(d.lastWriteAt) >= idle
		d.stateMu.Unlock()

		busy, hasPrompt := d.screenState()
		if busy {
			sawBusy = true
			continue
		}
		// Not busy.
		if !hasPrompt {
			continue
		}
		if !quiet {
			continue
		}
		if sawBusy {
			return nil
		}
		// Haven't seen busy yet — give phase 1 a chance.
		if time.Now().After(phase1Deadline) {
			return nil
		}
	}
}

// screenIsReady is equivalent to !busy && hasPrompt.
func (d *Driver) screenIsReady() bool {
	busy, hasPrompt := d.screenState()
	return !busy && hasPrompt
}

// screenState reports whether the screen currently shows a "busy" marker
// (spinner, status keyword) and whether the input-prompt anchor is visible.
func (d *Driver) screenState() (busy, hasPrompt bool) {
	d.term.Lock()
	defer d.term.Unlock()
	cols, rows := d.term.Size()
	for y := 0; y < rows; y++ {
		line := readRow(d.term, y, cols)
		if line == "" {
			continue
		}
		// The bottom hint contains words like "/effort" and "shift+tab" that we
		// otherwise treat as status; recognize and skip it.
		if bottomHintRe.MatchString(line) {
			trimmed := strings.TrimLeft(line, " \t│")
			if strings.HasPrefix(trimmed, "❯") || strings.HasPrefix(trimmed, ">") {
				hasPrompt = true
			}
			continue
		}
		if busyMarkerRe.MatchString(line) {
			busy = true
		}
		trimmed := strings.TrimLeft(line, " \t│")
		if strings.HasPrefix(trimmed, "❯") || strings.HasPrefix(trimmed, ">") {
			hasPrompt = true
		}
	}
	return busy, hasPrompt
}

// readRow returns the text content of row y. Unicode whitespace is normalized
// to ASCII space so downstream regex / trim logic doesn't have to know about
// NBSP, ideographic spaces, etc. Assumes caller holds the terminal lock.
func readRow(term vt10x.Terminal, y, cols int) string {
	var sb strings.Builder
	for x := 0; x < cols; x++ {
		g := term.Cell(x, y)
		switch {
		case g.Char == 0:
			sb.WriteByte(' ')
		case unicode.IsSpace(g.Char):
			sb.WriteByte(' ')
		default:
			sb.WriteRune(g.Char)
		}
	}
	return strings.TrimRight(sb.String(), " \t")
}

// snapshotScreen returns the full visible screen text, one row per line,
// trailing whitespace trimmed per row and trailing blank rows removed.
func (d *Driver) snapshotScreen() string {
	d.term.Lock()
	defer d.term.Unlock()
	cols, rows := d.term.Size()
	lines := make([]string, 0, rows)
	for y := 0; y < rows; y++ {
		lines = append(lines, readRow(d.term, y, cols))
	}
	// Trim trailing blank rows.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func (d *Driver) buildArgs() []string {
	args := []string{}
	if d.sessionExists() {
		args = append(args, "--resume", d.cfg.SessionID)
	} else {
		args = append(args, "--session-id", d.cfg.SessionID)
	}
	switch d.cfg.PermissionMode {
	case "bypassPermissions":
		// CLI's own flag for the full bypass.
		args = append(args, "--dangerously-skip-permissions")
	case "":
		// no-op, use the binary's default
	default:
		args = append(args, "--permission-mode", d.cfg.PermissionMode)
	}
	args = append(args, d.cfg.ExtraArgs...)
	return args
}

// sessionExists checks whether the claude transcript file for the configured
// session UUID and workdir already exists. claude stores transcripts at
// ~/.claude/projects/<slug>/<uuid>.jsonl where <slug> is the absolute workdir
// with `/` replaced by `-`.
func (d *Driver) sessionExists() bool {
	absWorkdir, err := filepath.Abs(d.cfg.Workdir)
	if err != nil {
		return false
	}
	slug := strings.ReplaceAll(absWorkdir, "/", "-")
	path := filepath.Join(os.Getenv("HOME"), ".claude", "projects", slug, d.cfg.SessionID+".jsonl")
	_, err = os.Stat(path)
	return err == nil
}

func (d *Driver) readLoop() {
	buf := make([]byte, 8192)
	for {
		n, err := d.ptyFile.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			d.stateMu.Lock()
			d.lastWriteAt = time.Now()
			d.stateMu.Unlock()
			d.term.Write(chunk)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				d.log.Warn("pty read", "err", err)
			}
			return
		}
	}
}

func (d *Driver) waitLoop() {
	_ = d.cmd.Wait()
	close(d.done)
}

// --- response extraction --------------------------------------------------

var (
	// matches a line whose first non-space character is the input prompt anchor
	promptLineRe = regexp.MustCompile(`^\s*[│]*\s*[❯>](\s|$)`)
	// matches a line that is a TUI status/spinner/completion marker we want
	// to drop from the captured response.
	noiseLineRe = regexp.MustCompile(`(?i)^\s*(?:[·•⠁⠂⠄⡀⢀⠠⠐⠈⠉⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✻✶✢*]\s+\S|esc to interrupt|ctrl\+[a-z] to|cogitated for|pondered for|thought for|cooked for)`)
	// matches purely decorative separators
	separatorRe = regexp.MustCompile(`^\s*[─━═╌╍-]{3,}\s*$`)
	// matches the persistent bottom hint line (different from transient status)
	bottomHintRe = regexp.MustCompile(`(?i)(shift\+tab to cycle|bypass permissions on)`)
	// busyMarkerRe matches a *line* that strongly indicates the TUI is
	// currently busy. Two acceptable patterns:
	//   1. "esc to interrupt"      — the cancel hint claude shows while busy
	//   2. <spinner> <verb-ing>... — e.g. "· Propagating…", "⠋ Pondering..."
	// The verbs are intentionally NOT matched alone (Claude's response text
	// often contains words like "working", "reading", "planning") and the
	// spinner charset intentionally excludes ✻/✶/✢ which are post-hoc
	// completion markers like "✻ Cogitated for 2s".
	busyMarkerRe = regexp.MustCompile(`(?i)(esc to interrupt|^\s*[·•⠁⠂⠄⡀⢀⠠⠐⠈⠉⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s+(?:pondering|thinking|working|crunching|cooking|brewing|mulling|simmering|propagating|synthesizing|brainstorming|deliberating|musing|ruminating|considering|reasoning|reflecting|computing|analyzing|generating|processing|cogitating|loading|reading|searching|exploring|planning|writing|preparing|fetching|gathering|reviewing|inspecting|investigating)\b)`)
	// assistantPrefixRe strips the leading "● " marker the TUI uses to label
	// assistant message lines.
	assistantPrefixRe = regexp.MustCompile(`^\s*●\s+`)
)

// extractResponse compares the visible-screen snapshots taken right before
// submission and right after the TUI returned to its ready prompt, and
// returns the new content that appeared (the assistant's reply).
//
// Strategy: compute longest common prefix and suffix of the two screens at
// line granularity. The "middle" of `after` that isn't part of either is the
// newly-added content. Then filter status/prompt/separator boilerplate and
// drop any echo of the user's prompt.
func extractResponse(beforeScreen, afterScreen, userPrompt string) string {
	beforeLines := strings.Split(beforeScreen, "\n")
	afterLines := strings.Split(afterScreen, "\n")

	// Common prefix.
	prefix := 0
	for prefix < len(beforeLines) && prefix < len(afterLines) &&
		strings.TrimRight(beforeLines[prefix], " \t") == strings.TrimRight(afterLines[prefix], " \t") {
		prefix++
	}
	// Common suffix (constrained to not overlap the prefix on either side).
	suffix := 0
	for suffix < len(beforeLines)-prefix && suffix < len(afterLines)-prefix &&
		strings.TrimRight(beforeLines[len(beforeLines)-1-suffix], " \t") ==
			strings.TrimRight(afterLines[len(afterLines)-1-suffix], " \t") {
		suffix++
	}

	mid := afterLines[prefix : len(afterLines)-suffix]
	cleaned := filterLines(strings.Join(mid, "\n"))
	cleaned = dropEchoedPrompt(cleaned, userPrompt)
	cleaned = strings.TrimSpace(cleaned)
	return cleaned
}

func filterLines(s string) string {
	out := make([]string, 0, 64)
	var lastNonBlank string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		if promptLineRe.MatchString(line) ||
			noiseLineRe.MatchString(line) ||
			separatorRe.MatchString(line) ||
			bottomHintRe.MatchString(line) ||
			isBoxedFrameLine(line) {
			continue
		}
		// Strip a leading box-drawing column ("│ " etc.) that the TUI uses to
		// indent assistant content inside a frame.
		line = stripFramePrefix(line)
		// Strip the "● " assistant-message prefix.
		line = assistantPrefixRe.ReplaceAllString(line, "")
		if line == "" {
			continue
		}
		if line == lastNonBlank {
			continue
		}
		out = append(out, line)
		lastNonBlank = line
	}
	// Trim trailing blanks.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func isBoxedFrameLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	// A line consisting solely of frame characters (corners, horizontals).
	const frameChars = "┌┐└┘─━╭╮╯╰═┄┅"
	for _, r := range t {
		if !strings.ContainsRune(frameChars, r) && r != ' ' {
			return false
		}
	}
	return true
}

// stripFramePrefix removes a leading "│ " column from the line, which the
// TUI uses to render the input frame's left edge.
func stripFramePrefix(line string) string {
	t := line
	for strings.HasPrefix(t, "│") || strings.HasPrefix(t, " ") || strings.HasPrefix(t, "\t") {
		t = strings.TrimPrefix(t, "│")
		t = strings.TrimLeft(t, " \t")
		if len(t) == len(line) {
			break
		}
		line = t
	}
	return t
}

func dropEchoedPrompt(s, userPrompt string) string {
	if userPrompt == "" {
		return s
	}
	want := strings.TrimSpace(userPrompt)
	if want == "" {
		return s
	}
	wantLines := strings.Split(want, "\n")
	for i := range wantLines {
		wantLines[i] = strings.TrimSpace(wantLines[i])
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		match := false
		// Single-line exact match
		if t == want || t == "> "+want || t == "❯ "+want {
			match = true
		}
		// Match any of the prompt's individual lines (handles multi-line prompts
		// where each line appears separately in the rendering).
		if !match {
			for _, wl := range wantLines {
				if wl != "" && t == wl {
					match = true
					break
				}
			}
		}
		if match {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
