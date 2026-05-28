package claude

import (
	"strings"
	"testing"
)

func TestFilterLines_DropsPromptStatusSeparator(t *testing.T) {
	in := strings.Join([]string{
		"> hello",                 // prompt echo
		"hello there",             // real content
		"───────────────",         // separator
		"⠋ Pondering...",          // spinner status
		"hello there",             // duplicate of prior real line
		"second real line",
		"esc to interrupt",
	}, "\n")
	got := filterLines(in)
	want := "hello there\nsecond real line"
	if got != want {
		t.Fatalf("filterLines\nwant: %q\ngot:  %q", want, got)
	}
}

func TestDropEchoedPrompt(t *testing.T) {
	in := "what's 2+2?\nIt's 4."
	got := dropEchoedPrompt(in, "what's 2+2?")
	want := "It's 4."
	if got != want {
		t.Fatalf("dropEchoedPrompt\nwant: %q\ngot:  %q", want, got)
	}
}

func TestExtractResponse_StripsAnsi(t *testing.T) {
	// A simplified raw byte stream that includes color escapes, the echoed
	// prompt as "> hi", a status line, and the actual response.
	raw := []byte("\x1b[1m> hi\x1b[0m\n\x1b[32m⠋ Pondering...\x1b[0m\nHello, friend!\n")
	got := extractResponse(raw, "", "", "hi")
	if !strings.Contains(got, "Hello, friend!") {
		t.Fatalf("expected response to contain greeting, got: %q", got)
	}
	if strings.Contains(got, "Pondering") {
		t.Fatalf("status line leaked into output: %q", got)
	}
	if strings.Contains(got, "> hi") {
		t.Fatalf("prompt echo leaked into output: %q", got)
	}
}

func TestExtractResponse_FallsBackToScreenDiff(t *testing.T) {
	// raw is empty/whitespace only — fall back to diff of before/after.
	before := "header\nready prompt\n"
	after := "header\nshiny new response\nready prompt\n"
	got := extractResponse([]byte("   \n  "), before, after, "anything")
	if !strings.Contains(got, "shiny new response") {
		t.Fatalf("expected fallback diff, got: %q", got)
	}
}
