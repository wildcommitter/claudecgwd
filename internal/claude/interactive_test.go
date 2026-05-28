package claude

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func newTestTranscriptInteractive(t *testing.T) (*transcriptReader, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	dir := filepath.Join(home, ".claude", "projects", "-home-user-claudecgwd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sid+".jsonl")
	return newTranscriptReader(sid, "/home/user/claudecgwd"), path
}

const (
	askToolUse    = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"AskUserQuestion","input":{"questions":[{"header":"DB","question":"Which database?","multiSelect":false,"options":[{"label":"Postgres","description":"relational"},{"label":"SQLite","description":"embedded"}]}]}}]}}`
	askToolResult = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"Postgres"}]}}`
	bashToolUse   = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_x","name":"Bash","input":{"command":"ls"}}]}}`
)

func TestPendingQuestion_DetectsUnansweredAsk(t *testing.T) {
	tr, path := newTestTranscriptInteractive(t)
	off := tr.offset()
	appendLines(t, path, askToolUse)

	id, qs, ok := tr.pendingQuestion(off)
	if !ok {
		t.Fatal("expected a pending question")
	}
	if id != "tu_1" {
		t.Fatalf("id = %q want tu_1", id)
	}
	if len(qs) != 1 || qs[0].Question != "Which database?" || len(qs[0].Options) != 2 {
		t.Fatalf("unexpected parse: %+v", qs)
	}
	if qs[0].Options[0].Label != "Postgres" || qs[0].Options[1].Label != "SQLite" {
		t.Fatalf("option labels wrong: %+v", qs[0].Options)
	}
}

func TestPendingQuestion_ClearedOnceAnswered(t *testing.T) {
	tr, path := newTestTranscriptInteractive(t)
	off := tr.offset()
	appendLines(t, path, askToolUse, askToolResult)

	if _, _, ok := tr.pendingQuestion(off); ok {
		t.Fatal("answered question must not be reported as pending")
	}
}

func TestPendingQuestion_IgnoresNonAskTools(t *testing.T) {
	tr, path := newTestTranscriptInteractive(t)
	off := tr.offset()
	appendLines(t, path, bashToolUse)

	if _, _, ok := tr.pendingQuestion(off); ok {
		t.Fatal("a Bash tool_use is not an interactive question")
	}
}

func TestSelectionKeys_SingleSelect(t *testing.T) {
	// Choose index 2 of 4 options: Up*4 (to top), Down*2, Enter.
	got := selectionKeys(Answer{Indices: []int{2}}, 4, false)
	want := []string{keyUp, keyUp, keyUp, keyUp, keyDown, keyDown, keyEnter}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("single-select keys:\n got %v\nwant %v", got, want)
	}
}

func TestSelectionKeys_SingleSelectFirst(t *testing.T) {
	got := selectionKeys(Answer{Indices: []int{0}}, 3, false)
	want := []string{keyUp, keyUp, keyUp, keyEnter}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("first-option keys:\n got %v\nwant %v", got, want)
	}
}

func TestSelectionKeys_MultiSelect(t *testing.T) {
	// Options: 0,1,2. Select 0 and 2.
	// Up*3, [opt0] Space, Down, [opt1] Down, [opt2] Space, Enter.
	got := selectionKeys(Answer{Indices: []int{0, 2}}, 3, true)
	want := []string{keyUp, keyUp, keyUp, keySpace, keyDown, keyDown, keySpace, keyEnter}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-select keys:\n got %v\nwant %v", got, want)
	}
}
