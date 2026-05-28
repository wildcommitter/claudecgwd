package claude

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTranscript creates ~/.claude/projects/<slug>/<sid>.jsonl under a temp
// HOME and returns the reader plus the file path.
func newTestTranscript(t *testing.T) (*transcriptReader, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	sid := "11111111-2222-3333-4444-555555555555"
	dir := filepath.Join(home, ".claude", "projects", "-home-user-claudecgwd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sid+".jsonl")
	return newTranscriptReader(sid, "/home/user/claudecgwd"), path
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	userLine      = `{"type":"user","message":{"role":"user","content":"do the thing"}}`
	asstHello     = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello there."}]}}`
	asstNarrate   = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Let me check."}]}}`
	asstToolOnly  = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`
	toolResult    = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"a\nb"}]}}`
	asstFinal     = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"All done."}]}}`
	asstMultiText = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Part one."},{"type":"text","text":"Part two."}]}}`
)

func TestReplySince_CollectsAssistantTextOnly(t *testing.T) {
	tr, path := newTestTranscript(t)
	// Pre-existing prior turn that must be excluded by the watermark.
	appendLines(t, path, userLine, asstHello)
	offset := tr.offset()

	// This turn: narration, a tool call (no text), tool result, final text.
	appendLines(t, path, userLine, asstNarrate, asstToolOnly, toolResult, asstFinal)

	reply, ok := tr.replySince(offset)
	if !ok {
		t.Fatal("expected a reply, got none")
	}
	want := "Let me check.\n\nAll done."
	if reply != want {
		t.Fatalf("reply mismatch:\n got: %q\nwant: %q", reply, want)
	}
}

func TestReplySince_MultipleTextBlocksInOneMessage(t *testing.T) {
	tr, path := newTestTranscript(t)
	offset := tr.offset()
	appendLines(t, path, userLine, asstMultiText)

	reply, ok := tr.replySince(offset)
	if !ok {
		t.Fatal("expected a reply")
	}
	if want := "Part one.\n\nPart two."; reply != want {
		t.Fatalf("got %q want %q", reply, want)
	}
}

func TestReplySince_NoAssistantTextYet(t *testing.T) {
	tr, path := newTestTranscript(t)
	offset := tr.offset()
	// Only a tool call has landed; the final text hasn't flushed.
	appendLines(t, path, userLine, asstToolOnly)

	if _, ok := tr.replySince(offset); ok {
		t.Fatal("expected no reply when only tool_use is present")
	}
}

func TestReplySince_MissingFile(t *testing.T) {
	tr := newTranscriptReader("does-not-exist-session", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if _, ok := tr.replySince(0); ok {
		t.Fatal("expected no reply for missing transcript")
	}
	if off := tr.offset(); off != 0 {
		t.Fatalf("expected offset 0 for missing file, got %d", off)
	}
}

func TestPath_GlobFindsSessionRegardlessOfSlug(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sid := "abc-session"
	// Use a project dir whose slug does NOT match a naive "/"->"-" of workdir,
	// to prove the glob (not the slug computation) resolves it.
	dir := filepath.Join(home, ".claude", "projects", "-some-other-encoded-path")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(want, []byte(asstFinal+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := newTranscriptReader(sid, "/home/user/claudecgwd")
	if got := tr.path(); got != want {
		t.Fatalf("path() = %q, want %q", got, want)
	}
}

func TestWaitForReplySince_PollsUntilFlushed(t *testing.T) {
	tr, path := newTestTranscript(t)
	offset := tr.offset()
	appendLines(t, path, userLine, asstToolOnly) // no text yet

	// Reply should be found within the timeout once we append text mid-wait.
	go func() {
		time.Sleep(150 * time.Millisecond)
		appendLines(t, path, asstFinal)
	}()

	reply, ok := tr.waitForReplySince(offset, 2*time.Second)
	if !ok {
		t.Fatal("expected reply after flush")
	}
	if reply != "All done." {
		t.Fatalf("got %q", reply)
	}
}
