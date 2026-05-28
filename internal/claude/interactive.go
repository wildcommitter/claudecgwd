package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
		// Free-text "Other": select the option past the listed ones, then type.
		if len(ans.Indices) == 0 && ans.FreeText != "" {
			keys := make([]string, 0, len(q.Options)+2)
			for range q.Options {
				keys = append(keys, keyUp)
			}
			for range q.Options { // step down onto the trailing "Other" entry
				keys = append(keys, keyDown)
			}
			keys = append(keys, keyEnter)
			if err := d.sendKeys(keys...); err != nil {
				return err
			}
			time.Sleep(150 * time.Millisecond)
			if err := d.sendKeys(ans.FreeText, keyEnter); err != nil {
				return err
			}
		} else {
			if err := d.sendKeys(selectionKeys(ans, len(q.Options), q.MultiSelect)...); err != nil {
				return err
			}
		}
		time.Sleep(300 * time.Millisecond) // let the TUI advance to the next question
	}
	return nil
}

// sendKeys writes raw key sequences to the PTY with a small inter-key delay so
// the TUI's input handler registers each one distinctly.
func (d *Driver) sendKeys(seqs ...string) error {
	for _, s := range seqs {
		if _, err := io.WriteString(d.ptyFile, s); err != nil {
			return fmt.Errorf("send key: %w", err)
		}
		time.Sleep(45 * time.Millisecond)
	}
	return nil
}

// cancelMenu dismisses an open select menu (Esc) so a parked turn can unwind
// when we can't or won't answer it.
func (d *Driver) cancelMenu() {
	_ = d.sendKeys(keyEsc, keyEsc)
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

	for {
		text, reason, hasAny := d.transcript.replyAndState(since)
		if terminalStopReason(reason) {
			return text, nil
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
