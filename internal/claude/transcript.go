package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The TUI screen is a lossy view of a turn: vt10x keeps no scrollback, so any
// assistant text that scrolls off the top during a turn is gone before we can
// snapshot it. Claude Code, however, appends every message to an authoritative
// session transcript as JSONL. We read the assistant's reply from there and
// fall back to screen-scraping only if that fails.
//
// transcriptReader locates and reads a single session's JSONL file.
type transcriptReader struct {
	sessionID string
	workdir   string
}

func newTranscriptReader(sessionID, workdir string) *transcriptReader {
	return &transcriptReader{sessionID: sessionID, workdir: workdir}
}

// path returns the JSONL transcript path for this session, or "" if it can't
// be located. Claude Code stores transcripts under
// ~/.claude/projects/<cwd-slug>/<session-id>.jsonl, where the slug encoding of
// the cwd is an internal detail. Rather than reproduce that encoding, we glob
// for the (unique) session-id filename across all project dirs, and fall back
// to the documented "/"->"-" slug if the glob finds nothing.
func (t *transcriptReader) path() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projects := filepath.Join(home, ".claude", "projects")
	if matches, _ := filepath.Glob(filepath.Join(projects, "*", t.sessionID+".jsonl")); len(matches) > 0 {
		return matches[0]
	}
	abs, err := filepath.Abs(t.workdir)
	if err != nil {
		abs = t.workdir
	}
	slug := strings.ReplaceAll(abs, "/", "-")
	return filepath.Join(projects, slug, t.sessionID+".jsonl")
}

// offset returns the current byte size of the transcript file, to be used as a
// watermark before a prompt is sent. Returns 0 if the file does not yet exist.
func (t *transcriptReader) offset() int64 {
	p := t.path()
	if p == "" {
		return 0
	}
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// transcriptLine is the subset of a JSONL record we care about.
type transcriptLine struct {
	Type    string `json:"type"`
	Message struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
	} `json:"message"`
}

// terminalStopReason reports whether a stop_reason value means the turn is
// fully done (no more streaming, no more tool calls). "tool_use" or empty
// means more is coming; everything else is a final state for that turn.
func terminalStopReason(r string) bool {
	switch r {
	case "end_turn", "max_tokens", "stop_sequence", "refusal":
		return true
	}
	return false
}

// contentBlock is one element of an assistant message's content array.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// replySince reads the transcript starting at byte offset `since` and returns
// the concatenation of all assistant text blocks appended after it — i.e. the
// assistant's reply (including any narration emitted around tool calls) for the
// turn that began at `since`. Returns ("", false) if no assistant text is
// present yet.
func (t *transcriptReader) replySince(since int64) (string, bool) {
	p := t.path()
	if p == "" {
		return "", false
	}
	f, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if since > 0 {
		if _, err := f.Seek(since, 0); err != nil {
			return "", false
		}
	}

	var parts []string
	sc := bufio.NewScanner(f)
	// Transcript lines can be large (e.g. big tool results); raise the cap well
	// above bufio's default 64 KiB.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec transcriptLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type != "assistant" || rec.Message.Role != "assistant" {
			continue
		}
		var blocks []contentBlock
		if err := json.Unmarshal(rec.Message.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "text" {
				if txt := strings.TrimSpace(b.Text); txt != "" {
					parts = append(parts, txt)
				}
			}
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n\n"), true
}

// waitForReplySince polls replySince until assistant text appears after the
// watermark or the deadline passes. The TUI returning to ready usually means
// the final message is already flushed, but a short poll absorbs any lag
// between render and disk flush.
func (t *transcriptReader) waitForReplySince(since int64, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if reply, ok := t.replySince(since); ok {
			return reply, true
		}
		if !time.Now().Before(deadline) {
			return "", false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// replyAndState reads everything appended past `since` and returns the
// concatenated assistant text plus the most recent assistant entry's
// stop_reason. Intermediate text blocks (narration around tool calls) are
// included so users see Claude's mid-turn commentary too.
func (t *transcriptReader) replyAndState(since int64) (text string, lastStopReason string, hasAny bool) {
	p := t.path()
	if p == "" {
		return "", "", false
	}
	f, err := os.Open(p)
	if err != nil {
		return "", "", false
	}
	defer f.Close()
	if since > 0 {
		if _, err := f.Seek(since, 0); err != nil {
			return "", "", false
		}
	}
	var parts []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec transcriptLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type != "assistant" || rec.Message.Role != "assistant" {
			continue
		}
		hasAny = true
		var blocks []contentBlock
		if err := json.Unmarshal(rec.Message.Content, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "text" {
				if txt := strings.TrimSpace(b.Text); txt != "" {
					parts = append(parts, txt)
				}
			}
		}
		if rec.Message.StopReason != "" {
			lastStopReason = rec.Message.StopReason
		}
	}
	text = strings.Join(parts, "\n\n")
	return text, lastStopReason, hasAny
}

// waitForTurnComplete polls the transcript until the most recent assistant
// entry past `since` has a terminal stop_reason (turn fully done). Returns
// the concatenated reply text. If ctx is cancelled before a terminal reason
// is observed, returns whatever text exists with ok=false.
func (t *transcriptReader) waitForTurnComplete(ctx context.Context, since int64) (string, bool) {
	const poll = 150 * time.Millisecond
	for {
		text, reason, _ := t.replyAndState(since)
		if terminalStopReason(reason) {
			return text, true
		}
		select {
		case <-ctx.Done():
			return text, false
		case <-time.After(poll):
		}
	}
}

// describe is used in logs to show which transcript path is in effect.
func (t *transcriptReader) describe() string {
	if p := t.path(); p != "" {
		return p
	}
	return fmt.Sprintf("(unresolved session %s)", t.sessionID)
}
