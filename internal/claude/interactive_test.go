package claude

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hinshun/vt10x"
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

func TestStrippedMenuRow(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"❯ Postgres", "Postgres", true},
		{"   ❯ Postgres", "Postgres", true},
		{"│ > SQLite", "SQLite", true}, // box border + ASCII caret
		{"❯ ◉ MySQL  (recommended)", "◉ MySQL  (recommended)", true},
		{"  SQLite", "", false}, // unmarked row
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := strippedMenuRow(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("strippedMenuRow(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestMatchOptionIndex(t *testing.T) {
	labels := []string{"Postgres", "SQLite", "MySQL"}
	cases := []struct {
		screen string
		want   int
	}{
		{"Postgres", 0},               // exact
		{"sqlite", 1},                 // case-insensitive exact
		{"◉ MySQL  (recommended)", 2}, // checkbox glyph + suffix → unique substring
		{"Postg", 0},                  // truncated, unique prefix
		{"", -1},
		{"Mongo", -1}, // no match
	}
	for _, c := range cases {
		if got := matchOptionIndex(c.screen, labels); got != c.want {
			t.Errorf("matchOptionIndex(%q) = %d, want %d", c.screen, got, c.want)
		}
	}
	// Ambiguous substring → no false positive.
	if got := matchOptionIndex("cat and category", []string{"cat", "category"}); got != -1 {
		t.Errorf("ambiguous substring should not match, got %d", got)
	}
}

func TestNextNavKey(t *testing.T) {
	if k, done := nextNavKey(2, 2); !done || k != "" {
		t.Errorf("at target: got (%q,%v), want (\"\",true)", k, done)
	}
	if k, done := nextNavKey(0, 2); done || k != keyDown {
		t.Errorf("below target: got (%q,%v), want (down,false)", k, done)
	}
	if k, done := nextNavKey(3, 1); done || k != keyUp {
		t.Errorf("above target: got (%q,%v), want (up,false)", k, done)
	}
	if k, done := nextNavKey(-1, 2); done || k != keyDown {
		t.Errorf("unknown position: got (%q,%v), want (down,false)", k, done)
	}
}

// renderMenu feeds plain lines into a fresh vt10x terminal so tests can read
// them back the same way the driver reads the live TUI screen.
func renderMenu(t *testing.T, lines ...string) vt10x.Terminal {
	t.Helper()
	term := vt10x.New(vt10x.WithSize(80, 24))
	if _, err := term.Write([]byte(strings.Join(lines, "\r\n") + "\r\n")); err != nil {
		t.Fatalf("term write: %v", err)
	}
	return term
}

// The closed-loop fix hinges on reading which option the cursor is on straight
// off the rendered screen — exercise that against realistic menu renders.
func TestCurrentMenuIndex_ReadsHighlightedOption(t *testing.T) {
	labels := []string{"Postgres", "SQLite", "MySQL"}
	cases := []struct {
		name  string
		lines []string
		want  int
	}{
		{"cursor on first", []string{"Which database?", "", "❯ Postgres", "  SQLite", "  MySQL"}, 0},
		{"cursor on middle", []string{"Which database?", "", "  Postgres", "❯ SQLite", "  MySQL"}, 1},
		{"box border + ascii caret", []string{"│ Which database?", "│ > Postgres", "│   SQLite"}, 0},
		{"checkbox glyph + suffix", []string{"  Postgres", "❯ ◉ SQLite  (recommended)", "  MySQL"}, 1},
		{"no marker visible", []string{"Postgres", "SQLite", "MySQL"}, -1},
		{"marked input prompt, no label match", []string{"❯ tell me more about it"}, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Driver{term: renderMenu(t, c.lines...)}
			if got := d.currentMenuIndex(labels); got != c.want {
				t.Fatalf("currentMenuIndex = %d, want %d", got, c.want)
			}
		})
	}
}
