// Package claude drives an interactive `claude` (Claude Code) session under
// a pseudo-tty, parses the TUI with a virtual terminal emulator, and exposes
// a simple Send(prompt) -> response API.
package claude

import (
	"bytes"
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

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	"github.com/hinshun/vt10x"

	"github.com/wildcommitter/claudecgwd/internal/config"
)

const (
	startupTimeout  = 60 * time.Second
	pollInterval    = 50 * time.Millisecond
	defaultIdleMs   = 150
	maxResponseSize = 1 << 20 // 1 MiB raw bytes per turn
)

// Driver owns a single long-lived `claude` PTY session.
type Driver struct {
	cfg config.ClaudeConfig
	log *slog.Logger

	cmd     *exec.Cmd
	ptyFile *os.File
	term    vt10x.Terminal

	// sendMu serializes Send calls so only one prompt is in flight.
	sendMu sync.Mutex

	// stateMu guards lastWriteAt, rawBuf, capturing.
	stateMu     sync.Mutex
	lastWriteAt time.Time
	rawBuf      bytes.Buffer
	capturing   bool

	done chan struct{}
}

// New constructs a Driver. Call Start to spawn the child.
func New(cfg config.ClaudeConfig, log *slog.Logger) *Driver {
	if log == nil {
		log = slog.Default()
	}
	return &Driver{cfg: cfg, log: log, done: make(chan struct{})}
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
func (d *Driver) Send(ctx context.Context, prompt string) (string, error) {
	d.sendMu.Lock()
	defer d.sendMu.Unlock()

	// Snapshot pre-submission visible screen so we can fall back to a screen
	// diff when raw capture is incomplete or noisy.
	beforeScreen := d.snapshotScreen()

	d.stateMu.Lock()
	d.rawBuf.Reset()
	d.capturing = true
	d.stateMu.Unlock()

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
	if err := d.waitReady(ctx); err != nil {
		return "", fmt.Errorf("waiting for response: %w", err)
	}

	d.stateMu.Lock()
	raw := append([]byte(nil), d.rawBuf.Bytes()...)
	d.capturing = false
	d.rawBuf.Reset()
	d.stateMu.Unlock()

	afterScreen := d.snapshotScreen()
	return extractResponse(raw, beforeScreen, afterScreen, prompt), nil
}

// writePrompt sends the user prompt followed by Enter. We use bracketed-paste
// markers so the TUI treats the entire payload as a single paste event,
// avoiding slash-command parsing for messages that start with `/` and keeping
// embedded newlines as literal input.
func (d *Driver) writePrompt(prompt string) error {
	const (
		bpStart = "\x1b[200~"
		bpEnd   = "\x1b[201~"
	)
	payload := bpStart + prompt + bpEnd + "\r"
	if _, err := io.WriteString(d.ptyFile, payload); err != nil {
		return fmt.Errorf("write pty: %w", err)
	}
	// Give the TUI a moment to register the paste before we begin polling for
	// busy->ready transition.
	time.Sleep(80 * time.Millisecond)
	return nil
}

// waitReady blocks until the screen shows an idle ready-prompt or ctx fires.
// "Ready" = the cursor sits on a line whose first non-space character is `>`
// AND the PTY has been idle for at least idleMs.
func (d *Driver) waitReady(ctx context.Context) error {
	idleMs := defaultIdleMs
	idle := time.Duration(idleMs) * time.Millisecond

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
		if !quiet {
			continue
		}
		if d.cursorOnPrompt() {
			return nil
		}
	}
}

// cursorOnPrompt returns true when the cursor row's first non-space character
// is `>` (the claude TUI input prompt indicator).
func (d *Driver) cursorOnPrompt() bool {
	d.term.Lock()
	defer d.term.Unlock()
	cur := d.term.Cursor()
	cols, _ := d.term.Size()
	var line strings.Builder
	for x := 0; x < cols; x++ {
		g := d.term.Cell(x, cur.Y)
		line.WriteRune(g.Char)
	}
	trimmed := strings.TrimLeft(line.String(), " \t")
	return strings.HasPrefix(trimmed, ">")
}

// snapshotScreen returns the full visible screen text with trailing whitespace
// trimmed per line and trailing empty lines removed.
func (d *Driver) snapshotScreen() string {
	d.term.Lock()
	defer d.term.Unlock()
	cols, rows := d.term.Size()
	var sb strings.Builder
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			sb.WriteRune(d.term.Cell(x, y).Char)
		}
		sb.WriteByte('\n')
	}
	// Trim trailing blank lines.
	s := strings.TrimRight(sb.String(), "\n \t\x00")
	return s
}

func (d *Driver) buildArgs() []string {
	args := []string{}
	sessFile := filepath.Join(os.Getenv("HOME"), ".claude", "sessions", d.cfg.SessionID+".json")
	if _, err := os.Stat(sessFile); err == nil {
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

func (d *Driver) readLoop() {
	buf := make([]byte, 8192)
	for {
		n, err := d.ptyFile.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			d.stateMu.Lock()
			d.lastWriteAt = time.Now()
			d.term.Write(chunk)
			if d.capturing && d.rawBuf.Len()+n <= maxResponseSize {
				d.rawBuf.Write(chunk)
			}
			d.stateMu.Unlock()
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
	// matches a line whose first non-space character is `>` (the input prompt)
	promptLineRe = regexp.MustCompile(`^\s*>(\s|$)`)
	// matches a line starting with a spinner/marker character
	spinnerLineRe = regexp.MustCompile(`^\s*[⠁⠂⠄⡀⢀⠠⠐⠈⠉⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✻✶✢*]`)
	// matches lines starting with known TUI status keywords
	statusLineRe = regexp.MustCompile(`(?i)^\s*(esc to interrupt|ctrl\+[a-z] to|approximate|tokens|context left|pondering|thinking|working|crunching|cooking|brewing|mulling|simmering)`)
	// matches purely decorative separators
	separatorRe = regexp.MustCompile(`^\s*[─━═╌╍-]{3,}\s*$`)
)

// extractResponse takes the raw byte stream captured during a turn and
// produces a best-effort plain-text response.
//
// The TUI redraws aggressively so the raw stream contains overlapping
// versions of the screen. We strip ANSI, scan the resulting lines, drop
// status/spinner/prompt/separator lines, dedupe consecutive duplicates, and
// trim any echo of the user's prompt and any leading/trailing prompt artifacts.
//
// If the raw extraction comes back empty, fall back to a screen diff.
func extractResponse(raw []byte, beforeScreen, afterScreen, userPrompt string) string {
	stripped := ansi.Strip(string(raw))
	stripped = strings.ReplaceAll(stripped, "\r", "")

	cleaned := filterLines(stripped)
	cleaned = dropEchoedPrompt(cleaned, userPrompt)
	cleaned = strings.TrimSpace(cleaned)

	if cleaned == "" {
		// Fall back to comparing visible screens.
		cleaned = screenDiff(beforeScreen, afterScreen)
		cleaned = filterLines(cleaned)
		cleaned = dropEchoedPrompt(cleaned, userPrompt)
		cleaned = strings.TrimSpace(cleaned)
	}
	return cleaned
}

func filterLines(s string) string {
	out := make([]string, 0, 64)
	var last string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			if last == "" {
				continue
			}
			out = append(out, "")
			last = ""
			continue
		}
		if promptLineRe.MatchString(line) || spinnerLineRe.MatchString(line) || statusLineRe.MatchString(line) || separatorRe.MatchString(line) {
			continue
		}
		// Drop lines that look like the empty input prompt frame: "│ > " etc.
		if isBoxedPromptLine(line) {
			continue
		}
		if line == last {
			continue
		}
		out = append(out, line)
		last = line
	}
	return strings.Join(out, "\n")
}

func isBoxedPromptLine(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	// Common box-drawing characters used in the claude TUI input frame.
	const boxChars = "│┌┐└┘─━╭╮╯╰"
	first, _ := firstRune(t)
	return strings.ContainsRune(boxChars, first)
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, 1
	}
	return 0, 0
}

func dropEchoedPrompt(s, userPrompt string) string {
	if userPrompt == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	want := strings.TrimSpace(userPrompt)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		// Skip exact echoes of the user prompt and skip the conventional
		// "> <prompt>" rendering.
		if t == want || t == "> "+want || strings.TrimPrefix(t, "> ") == want {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// screenDiff returns the lines in after that aren't present (in order) at the
// same trailing region of before. Crude but works as a fallback.
func screenDiff(before, after string) string {
	beforeSet := make(map[string]struct{})
	for _, l := range strings.Split(before, "\n") {
		beforeSet[strings.TrimSpace(l)] = struct{}{}
	}
	var out []string
	for _, l := range strings.Split(after, "\n") {
		if _, ok := beforeSet[strings.TrimSpace(l)]; ok {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

