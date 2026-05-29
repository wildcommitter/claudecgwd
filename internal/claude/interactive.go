package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ErrStalled is returned from Send when the session transcript shows no
// progress for the stall timeout — typically an upstream API hang. The
// driver also cancels the in-flight TUI request before returning, so the
// session is usable again immediately after.
var ErrStalled = errors.New("upstream stalled: no transcript progress")

// This file adds handling for Claude Code's interactive AskUserQuestion tool.
// When the model asks the user a multiple-choice question, the TUI renders a
// select menu and blocks for a keyboard selection — and the turn parks at
// stop_reason "tool_use", which is not terminal, so the driver would otherwise
// hang until the turn budget expires. We detect the parked question from the
// transcript (the tool_use input carries the full structured question), relay
// it to the chat user via a ChoiceAsker, then drive the selection back into the
// TUI with arrow-key + Enter keystrokes.

// Choice is one selectable option in an interactive question.
type Choice struct {
	Label       string
	Description string
}

// Question mirrors one entry of AskUserQuestion's `questions` array.
type Question struct {
	Header      string
	Question    string
	MultiSelect bool
	Options     []Choice
}

// Answer is the user's response to a single Question. Indices are 0-based into
// Question.Options. A non-empty FreeText with no Indices means the user typed a
// custom ("Other") answer.
type Answer struct {
	Indices  []int
	FreeText string
}

// ChoiceAsker presents the detected questions to the user and returns one
// Answer per question, in order. It is supplied by the caller of Send (the
// router, backed by the chat Origin). Blocking is expected; it should honor
// ctx cancellation.
type ChoiceAsker func(ctx context.Context, qs []Question) ([]Answer, error)

// askName is the Claude Code tool whose select menu we drive.
const askName = "AskUserQuestion"

// Menu-drive timing. Vars (not consts) so tests can shrink them; production
// keeps the TUI-friendly delays that let its input handler register each key.
var (
	keyDelay       = 45 * time.Millisecond  // between individual keystrokes
	navStepDelay   = 55 * time.Millisecond  // after a nav step, before re-reading the screen
	questionSettle = 300 * time.Millisecond // after submitting one question, before the next
	freeTextSettle = 150 * time.Millisecond // after opening the free-text entry, before typing

	// After driving a selection, confirm the menu actually closed/advanced; if a
	// submit (Enter) didn't register, re-send it up to submitConfirmRetries times
	// rather than letting the parked turn stall for minutes.
	submitConfirmWindow  = 600 * time.Millisecond
	submitConfirmRetries = 3
)

// Keystrokes understood by the Claude Code select menu.
const (
	keyUp    = "\x1b[A"
	keyDown  = "\x1b[B"
	keyEnter = "\r"
	keySpace = " "
	keyEsc   = "\x1b"
)

// rawBlock is a content block as it appears in either an assistant message
// (text / tool_use) or a user message (tool_result).
type rawBlock struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`        // tool_use
	ID        string          `json:"id"`          // tool_use
	Input     json.RawMessage `json:"input"`       // tool_use
	ToolUseID string          `json:"tool_use_id"` // tool_result
}

// rawLine is a JSONL record with enough fields to find a parked question.
type rawLine struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// askInput is the AskUserQuestion tool input schema.
type askInput struct {
	Questions []struct {
		Header      string `json:"header"`
		Question    string `json:"question"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	} `json:"questions"`
}

// pendingQuestion scans the transcript past `since` and, if the most recent
// AskUserQuestion tool_use has no matching tool_result yet, returns its
// tool_use id and parsed questions. ok is false when nothing is parked.
func (t *transcriptReader) pendingQuestion(since int64) (id string, qs []Question, ok bool) {
	p := t.path()
	if p == "" {
		return "", nil, false
	}
	f, err := os.Open(p)
	if err != nil {
		return "", nil, false
	}
	defer f.Close()
	if since > 0 {
		if _, err := f.Seek(since, 0); err != nil {
			return "", nil, false
		}
	}

	type parked struct {
		id string
		qs []Question
	}
	var cur *parked // most recent unanswered AskUserQuestion
	answered := map[string]bool{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec rawLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		var blocks []rawBlock
		if err := json.Unmarshal(rec.Message.Content, &blocks); err != nil {
			continue // string content (plain user prompt) — no blocks
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				if rec.Type == "assistant" && b.Name == askName {
					if parsed, perr := parseAskInput(b.Input); perr == nil && len(parsed) > 0 {
						cur = &parked{id: b.ID, qs: parsed}
					}
				}
			case "tool_result":
				answered[b.ToolUseID] = true
			}
		}
	}
	if cur == nil || answered[cur.id] {
		return "", nil, false
	}
	return cur.id, cur.qs, true
}

// parseAskInput converts the raw AskUserQuestion input JSON into []Question.
func parseAskInput(raw json.RawMessage) ([]Question, error) {
	var in askInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	qs := make([]Question, 0, len(in.Questions))
	for _, q := range in.Questions {
		opts := make([]Choice, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, Choice{Label: o.Label, Description: o.Description})
		}
		qs = append(qs, Question{
			Header:      q.Header,
			Question:    q.Question,
			MultiSelect: q.MultiSelect,
			Options:     opts,
		})
	}
	return qs, nil
}

// selectionKeys returns the keystroke sequence that selects `ans` for a menu of
// `nOptions` listed options. The cursor is first driven to the top with Up
// presses (a no-op at the top of the list) so we don't depend on its starting
// position, then:
//   - single-select: Down to the target index, then Enter.
//   - multi-select: walk each option top-to-bottom, Space to toggle the
//     selected ones, then Enter to submit.
//
// Free-text ("Other") answers are not driven here — the caller handles them.
func selectionKeys(ans Answer, nOptions int, multi bool) []string {
	keys := make([]string, 0, nOptions+4)
	// Normalize to the top of the list.
	for i := 0; i < nOptions; i++ {
		keys = append(keys, keyUp)
	}
	if !multi {
		idx := 0
		if len(ans.Indices) > 0 {
			idx = ans.Indices[0]
		}
		if idx < 0 {
			idx = 0
		}
		for i := 0; i < idx; i++ {
			keys = append(keys, keyDown)
		}
		keys = append(keys, keyEnter)
		return keys
	}
	sel := map[int]bool{}
	for _, i := range ans.Indices {
		sel[i] = true
	}
	for i := 0; i < nOptions; i++ {
		if sel[i] {
			keys = append(keys, keySpace)
		}
		if i < nOptions-1 {
			keys = append(keys, keyDown)
		}
	}
	keys = append(keys, keyEnter)
	return keys
}

// applyAnswers drives the TUI selection for each question in order. The TUI
// presents questions sequentially, advancing to the next once the current is
// submitted, so we inject answers in the same order with a short settle pause
// between them.
func (d *Driver) applyAnswers(qs []Question, answers []Answer) error {
	for i, q := range qs {
		var ans Answer
		if i < len(answers) {
			ans = answers[i]
		}
		if err := d.answerOne(q, ans); err != nil {
			return err
		}
		time.Sleep(questionSettle) // let the TUI advance to the next question
	}
	return nil
}

// answerOne selects the answer for a single question. It prefers a closed-loop
// drive — navigate the cursor while reading the highlighted row back off the
// screen and matching it to the known option labels, so a dropped keystroke or
// a wrapping list self-corrects rather than submitting a blind off-by-one. If
// the menu can't be read (an unexpected render), it falls back to the original
// open-loop keystroke sequence so behaviour is never worse than before.
func (d *Driver) answerOne(q Question, ans Answer) error {
	// Capture the live menu render and what we read from it — this drive is
	// otherwise silent, which made selection failures undiagnosable.
	d.logMenuRender(q, ans)
	// Free-text "Other" has no on-screen label to match against, so drive blind
	// to the trailing entry, then type the answer.
	if len(ans.Indices) == 0 && ans.FreeText != "" {
		return d.answerFreeText(q, ans.FreeText)
	}
	labels := optionLabels(q)
	if d.driveByScreen(q, ans) {
		d.log.Info("menu: selection driven by on-screen confirmation", "header", q.Header)
	} else {
		d.log.Warn("menu: could not confirm the selection on screen; using blind keystrokes",
			"header", q.Header, "multiselect", q.MultiSelect)
		if err := d.sendKeys(selectionKeys(ans, len(q.Options), q.MultiSelect)...); err != nil {
			return err
		}
	}
	// Confirm the selection submitted — the menu should advance to the next
	// question or close. A live failure mode is the Enter not registering, which
	// otherwise leaves the turn to stall for minutes; re-send just Enter a few
	// times. Re-sending Enter alone is safe: the navigation and any multi-select
	// toggles already landed, so the chosen set is never disturbed.
	for attempt := 1; ; attempt++ {
		if d.menuGone(labels) {
			if attempt > 1 {
				d.log.Info("menu: submitted after re-sending Enter", "header", q.Header, "attempts", attempt)
			}
			return nil
		}
		if attempt > submitConfirmRetries {
			d.log.Warn("menu: selection did not register after retries", "header", q.Header, "retries", submitConfirmRetries)
			return nil
		}
		d.log.Warn("menu: selection not registered; re-sending Enter", "header", q.Header, "attempt", attempt)
		if err := d.sendKeys(keyEnter); err != nil {
			return err
		}
	}
}

// menuGone reports whether this question's select menu is no longer the active
// highlighted menu — i.e. the selection submitted and the TUI advanced or
// closed. Polls briefly so a slightly-late TUI repaint isn't read as failure.
func (d *Driver) menuGone(labels []string) bool {
	deadline := time.Now().Add(submitConfirmWindow)
	for {
		if d.currentMenuIndex(labels) < 0 {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(navStepDelay)
	}
}

// logMenuRender dumps the menu-relevant screen state so a failed drive can be
// diagnosed from the journal without a live terminal: the rows the cursor
// marker is on, the index we resolve from them, and (when nothing matches) the
// raw non-empty rows so the actual TUI render is visible.
func (d *Driver) logMenuRender(q Question, ans Answer) {
	labels := optionLabels(q)
	d.term.Lock()
	cols, rows := d.term.Size()
	var marked, screen []string
	for y := 0; y < rows; y++ {
		row := readRow(d.term, y, cols)
		if row == "" {
			continue
		}
		if len(screen) < 18 {
			screen = append(screen, row)
		}
		if opt, ok := strippedMenuRow(row); ok {
			marked = append(marked, opt)
		}
	}
	d.term.Unlock()
	d.log.Info("menu render",
		"header", q.Header, "multiselect", q.MultiSelect,
		"labels", labels, "want", ans.Indices,
		"marked_rows", marked, "resolved_index", d.currentMenuIndex(labels),
		"screen", screen)
}

// answerFreeText drives the cursor past the listed options onto the trailing
// free-text ("Other") entry and types the answer. This one stays open-loop:
// the custom entry has no transcript-known label to confirm against.
func (d *Driver) answerFreeText(q Question, text string) error {
	keys := make([]string, 0, 2*len(q.Options)+1)
	for range q.Options {
		keys = append(keys, keyUp) // normalize to the top
	}
	for range q.Options { // step down onto the trailing "Other" entry
		keys = append(keys, keyDown)
	}
	keys = append(keys, keyEnter)
	if err := d.sendKeys(keys...); err != nil {
		return err
	}
	time.Sleep(freeTextSettle)
	return d.sendKeys(text, keyEnter)
}

// driveByScreen selects ans by navigating with on-screen confirmation. It
// returns false (without committing a partial selection for single-select) when
// the menu can't be read, so the caller can fall back to blind keystrokes.
func (d *Driver) driveByScreen(q Question, ans Answer) bool {
	labels := optionLabels(q)
	// Feasibility gate: bail to the fallback only before we touch anything, so
	// we never half-drive a selection and then redo it blind.
	if d.currentMenuIndex(labels) < 0 {
		return false
	}
	if q.MultiSelect {
		for _, target := range ans.Indices {
			if target < 0 || target >= len(labels) {
				continue
			}
			if !d.navigateTo(labels, target) {
				// Cursor is somewhere known but we couldn't converge; submit
				// what's toggled rather than blind-redoing (which would re-toggle).
				break
			}
			if err := d.sendKeys(keySpace); err != nil {
				return false
			}
			time.Sleep(navStepDelay)
		}
		return d.sendKeys(keyEnter) == nil
	}
	target := 0
	if len(ans.Indices) > 0 && ans.Indices[0] >= 0 {
		target = ans.Indices[0]
	}
	if target >= len(labels) {
		target = len(labels) - 1
	}
	if !d.navigateTo(labels, target) {
		return false
	}
	return d.sendKeys(keyEnter) == nil
}

// navigateTo drives the select cursor onto option `target`, re-reading the
// highlighted row after every step so a dropped keystroke or a wrapping list
// self-corrects. Returns false if it can't converge within a bounded number of
// steps (the caller falls back to blind keystrokes).
func (d *Driver) navigateTo(labels []string, target int) bool {
	maxSteps := 3*len(labels) + 4
	for step := 0; step < maxSteps; step++ {
		key, done := nextNavKey(d.currentMenuIndex(labels), target)
		if done {
			return true
		}
		if err := d.sendKeys(key); err != nil {
			return false
		}
		time.Sleep(navStepDelay)
	}
	final := d.currentMenuIndex(labels)
	if final != target {
		d.log.Warn("menu: navigation did not converge", "target", target, "final", final, "max_steps", maxSteps)
	}
	return final == target
}

// nextNavKey returns the key to step the cursor from cur toward target, or
// done=true when already there. cur < 0 means "position unknown" — step down
// and re-read on the next pass.
func nextNavKey(cur, target int) (key string, done bool) {
	switch {
	case cur == target:
		return "", true
	case cur >= 0 && cur > target:
		return keyUp, false
	default:
		return keyDown, false
	}
}

// currentMenuIndex reads the screen and returns the option index the select
// cursor is currently on, or -1 if no highlighted row matches a known label.
func (d *Driver) currentMenuIndex(labels []string) int {
	d.term.Lock()
	defer d.term.Unlock()
	cols, rows := d.term.Size()
	for y := 0; y < rows; y++ {
		opt, ok := strippedMenuRow(readRow(d.term, y, cols))
		if !ok {
			continue
		}
		if idx := matchOptionIndex(opt, labels); idx >= 0 {
			return idx
		}
	}
	return -1
}

// strippedMenuRow reports whether row is a select-menu row (its first
// non-padding rune is the cursor marker ❯ or >) and returns the text after the
// marker. Any checkbox glyph is left in place — matchOptionIndex tolerates it.
func strippedMenuRow(row string) (string, bool) {
	trimmed := strings.TrimLeft(row, " \t│")
	var marker string
	switch {
	case strings.HasPrefix(trimmed, "❯"):
		marker = "❯"
	case strings.HasPrefix(trimmed, ">"):
		marker = ">"
	default:
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, marker)), true
}

// matchOptionIndex maps an on-screen (possibly decorated/truncated) row to the
// index of the option it represents, or -1 if there's no unambiguous match.
// Order of preference: exact, then a unique label-substring (handles checkbox
// glyphs and "(recommended)" suffixes), then a unique truncation prefix.
func matchOptionIndex(screen string, labels []string) int {
	s := normalizeLabel(screen)
	if s == "" {
		return -1
	}
	for i, l := range labels {
		if normalizeLabel(l) == s {
			return i
		}
	}
	cand := -1
	for i, l := range labels {
		if nl := normalizeLabel(l); nl != "" && strings.Contains(s, nl) {
			if cand >= 0 {
				return -1 // ambiguous (one label is a substring of the row of another)
			}
			cand = i
		}
	}
	if cand >= 0 {
		return cand
	}
	if len([]rune(s)) >= 4 { // long-enough unique prefix → truncated render
		c2 := -1
		for i, l := range labels {
			if strings.HasPrefix(normalizeLabel(l), s) {
				if c2 >= 0 {
					return -1
				}
				c2 = i
			}
		}
		return c2
	}
	return -1
}

// normalizeLabel lower-cases, trims, and collapses internal whitespace so
// on-screen text and transcript labels compare cleanly.
func normalizeLabel(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// optionLabels extracts the option labels of a question in order.
func optionLabels(q Question) []string {
	labels := make([]string, len(q.Options))
	for i, o := range q.Options {
		labels[i] = o.Label
	}
	return labels
}

// sendKeys writes raw key sequences to the PTY with a small inter-key delay so
// the TUI's input handler registers each one distinctly.
func (d *Driver) sendKeys(seqs ...string) error {
	for _, s := range seqs {
		if _, err := io.WriteString(d.ptyFile, s); err != nil {
			return fmt.Errorf("send key: %w", err)
		}
		time.Sleep(keyDelay)
	}
	return nil
}

// cancelMenu dismisses an open select menu (Esc) so a parked turn can unwind
// when we can't or won't answer it.
func (d *Driver) cancelMenu() {
	_ = d.sendKeys(keyEsc, keyEsc)
}

// dumpAwaitScreen logs the current non-empty screen rows. Used by the opt-in
// menu diagnostic to capture the real render of a parked interactive menu (the
// transcript doesn't record the parked tool_use, so the screen is ground truth).
func (d *Driver) dumpAwaitScreen() {
	d.term.Lock()
	cols, rows := d.term.Size()
	var scr []string
	for y := 0; y < rows; y++ {
		if r := readRow(d.term, y, cols); r != "" {
			scr = append(scr, fmt.Sprintf("%2d|%s", y, r))
		}
	}
	d.term.Unlock()
	d.log.Info("await diagnostic screen (parked turn)", "rows", len(scr), "screen", scr)
}

// awaitTurn waits for the current turn to reach a terminal stop_reason,
// handling any AskUserQuestion menus that park the turn along the way.
// Returns:
//   - (reply, nil)          on normal completion
//   - (partial, ErrStalled) when the transcript made no progress for stallAfter
//     (the in-flight TUI request is also cancelled via Esc)
//   - (partial, ctx.Err())  on ctx cancellation
func (d *Driver) awaitTurn(ctx context.Context, since int64, ask ChoiceAsker) (string, error) {
	const poll = 150 * time.Millisecond
	stallAfter := d.stallTimeout()

	handled := map[string]bool{} // tool_use ids we've already answered
	lastProgressAt := time.Now()
	lastTextLen := 0
	lastHasAny := false
	turnStart := time.Now()
	diagDumped := false

	for {
		text, reason, hasAny := d.transcript.replyAndState(since)
		if terminalStopReason(reason) {
			return text, nil
		}
		// Diagnostic (opt-in via CLAUDECGWD_MENU_DIAG): a parked AskUserQuestion
		// tool_use is not reliably written to the transcript, so the screen is the
		// only ground truth for an interactive menu. Dump it once, early, so the
		// real render can be captured without waiting out the full stall timeout.
		if !diagDumped && os.Getenv("CLAUDECGWD_MENU_DIAG") != "" && time.Since(turnStart) > 8*time.Second {
			diagDumped = true
			d.dumpAwaitScreen()
		}
		// Track transcript progress — any new content resets the stall clock.
		if hasAny != lastHasAny || len(text) != lastTextLen {
			lastProgressAt = time.Now()
			lastTextLen = len(text)
			lastHasAny = hasAny
		}
		if stallAfter > 0 && time.Since(lastProgressAt) >= stallAfter {
			d.log.Warn("turn stalled; cancelling in-flight request",
				"stall_for", time.Since(lastProgressAt).Round(time.Second),
				"text_len", len(text))
			d.cancelInFlight()
			return text, ErrStalled
		}

		if id, qs, ok := d.transcript.pendingQuestion(since); ok && !handled[id] {
			handled[id] = true
			d.log.Info("interactive question detected", "tool_use_id", id, "questions", len(qs))
			switch {
			case ask == nil:
				d.log.Warn("no choice asker available; cancelling menu")
				d.cancelMenu()
			default:
				answers, err := ask(ctx, qs)
				if err != nil {
					d.log.Warn("choice asker failed; cancelling menu", "err", err)
					d.cancelMenu()
				} else if err := d.applyAnswers(qs, answers); err != nil {
					d.log.Warn("failed to inject answers; cancelling menu", "err", err)
					d.cancelMenu()
				}
			}
			// Time spent waiting for the human / typing keystrokes must not
			// count toward the stall timer.
			lastProgressAt = time.Now()
			select {
			case <-ctx.Done():
				return text, ctx.Err()
			case <-time.After(poll):
			}
			continue
		}

		select {
		case <-ctx.Done():
			return text, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// stallTimeout returns the configured stall threshold or a 3-minute default.
func (d *Driver) stallTimeout() time.Duration {
	if d.cfg.StallTimeoutS > 0 {
		return time.Duration(d.cfg.StallTimeoutS) * time.Second
	}
	return 3 * time.Minute
}

// cancelInFlight asks the TUI to abort whatever request it has open with the
// model. The TUI's status hint says "esc to interrupt", so we send Esc.
func (d *Driver) cancelInFlight() {
	if d.ptyFile == nil {
		return
	}
	_, _ = d.ptyFile.Write([]byte{0x1b})
}
