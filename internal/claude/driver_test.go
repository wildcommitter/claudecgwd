package claude

import (
	"strings"
	"testing"
)

func TestExtractResponse_AppendsNewMiddleLines(t *testing.T) {
	before := strings.Join([]string{
		"prior conversation line 1",
		"prior conversation line 2",
		"",
		"────────────────────────────",
		"❯ Try something",
		"────────────────────────────",
		"⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n")
	after := strings.Join([]string{
		"prior conversation line 1",
		"prior conversation line 2",
		"",
		"user> hello",
		"",
		"Hello back!",
		"",
		"────────────────────────────",
		"❯ Try something",
		"────────────────────────────",
		"⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n")
	got := extractResponse(before, after, "hello")
	if !strings.Contains(got, "Hello back!") {
		t.Fatalf("expected response, got: %q", got)
	}
	if strings.Contains(got, "bypass permissions") {
		t.Fatalf("status bar leaked: %q", got)
	}
	if strings.Contains(got, "❯") {
		t.Fatalf("prompt anchor leaked: %q", got)
	}
}

func TestFilterLines_DropsStatusAndPromptAndFrames(t *testing.T) {
	in := strings.Join([]string{
		"  ❯ hello",                         // prompt echo
		"hello there",                       // real content
		"────────────────────────",          // separator
		"⠋ Pondering...",                    // spinner status
		"hello there",                       // duplicate
		"second real line",
		"esc to interrupt",
		"⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n")
	got := filterLines(in)
	want := "hello there\nsecond real line"
	if got != want {
		t.Fatalf("filterLines\nwant: %q\ngot:  %q", want, got)
	}
}

func TestDropEchoedPrompt_Multiline(t *testing.T) {
	in := strings.Join([]string{
		"[from telegram(@me)]",
		"reply with: pong",
		"",
		"pong",
	}, "\n")
	got := dropEchoedPrompt(in, "[from telegram(@me)]\nreply with: pong")
	if strings.Contains(got, "reply with: pong") {
		t.Fatalf("prompt echo not removed: %q", got)
	}
	if !strings.Contains(got, "pong") {
		t.Fatalf("response lost: %q", got)
	}
}

func TestBusyMarkerRe_DoesNotMatchContentWords(t *testing.T) {
	// Claude's response text often contains words like "working", "reading",
	// "planning". These must NOT trigger busy detection — only the TUI status
	// line format (spinner + present-participle, or "esc to interrupt") does.
	notBusy := []string{
		"there are uncommitted tweaks in the working tree",
		"Reading the file should be fine",
		"  planning to commit later",
		"✻ Cogitated for 2s",
		"✻ Brewed for 18s",
		"⏵⏵ bypass permissions on (shift+tab to cycle)",
		"● Going well 👋",
		"  ⎿  diff --git a/foo b/foo",
	}
	for _, line := range notBusy {
		if busyMarkerRe.MatchString(line) {
			t.Errorf("busyMarkerRe wrongly matched non-busy line: %q", line)
		}
	}
	busy := []string{
		"· Propagating…",
		"⠋ Pondering for 5s",
		"Press esc to interrupt",
		"⠙ Working... (3s)",
	}
	for _, line := range busy {
		if !busyMarkerRe.MatchString(line) {
			t.Errorf("busyMarkerRe missed actual busy line: %q", line)
		}
	}
}

func TestIsBoxedFrameLine(t *testing.T) {
	cases := map[string]bool{
		"╭────────╮":  true,
		"│        │":  false, // has spaces inside — but pure frame+space? need to think
		"│ content │": false,
		"hello":       false,
	}
	for in, want := range cases {
		if got := isBoxedFrameLine(in); got != want {
			t.Errorf("isBoxedFrameLine(%q): want %v, got %v", in, want, got)
		}
	}
}
