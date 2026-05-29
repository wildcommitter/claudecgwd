package claude

import (
	"io"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hinshun/vt10x"
)

// This file simulates the AskUserQuestion select menu deterministically so the
// closed-loop selection drive can be verified without a live terminal (and
// without a 3-minute stall on failure). A fakeMenu owns a vt10x screen, moves a
// cursor in response to the arrow/space/enter keystrokes the driver writes to a
// pipe "PTY", and re-renders — exactly the read/keystroke loop the real driver
// runs against Claude Code's TUI.

type fakeMenu struct {
	mu           sync.Mutex
	term         vt10x.Terminal
	labels       []string
	multi        bool
	wrap         bool // does the list wrap at the ends (the case blind Up*N gets wrong)
	cursor       int
	selected     map[int]bool
	submitted    bool
	submittedSel []int
	dropEnters   int // ignore this many Enter presses (simulate a submit not registering)
	done         chan struct{}
}

func newFakeMenu(labels []string, multi, wrap bool, start int) *fakeMenu {
	m := &fakeMenu{
		term:   vt10x.New(vt10x.WithSize(80, 24)),
		labels: labels, multi: multi, wrap: wrap, cursor: start,
		selected: map[int]bool{}, done: make(chan struct{}),
	}
	m.mu.Lock()
	m.renderLocked()
	m.mu.Unlock()
	return m
}

// renderLocked paints the current menu state. Each render fully repaints from
// the home position so only the current cursor row carries the ❯ marker.
func (m *fakeMenu) renderLocked() {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H") // clear + home
	if m.submitted {
		// Menu closed: nothing for the driver's screen reader to match, so
		// menuGone() resolves true (mirrors the TUI advancing past the menu).
		b.WriteString("(submitted)\r\n")
		_, _ = m.term.Write([]byte(b.String()))
		return
	}
	b.WriteString("Pick something\r\n")
	for i, l := range m.labels {
		if i == m.cursor {
			b.WriteString("❯ ") // ❯
		} else {
			b.WriteString("  ")
		}
		if m.multi {
			if m.selected[i] {
				b.WriteString("[x] ")
			} else {
				b.WriteString("[ ] ")
			}
		}
		b.WriteString(l + "\r\n")
	}
	_, _ = m.term.Write([]byte(b.String()))
}

func (m *fakeMenu) moveUp() {
	if m.cursor > 0 {
		m.cursor--
	} else if m.wrap {
		m.cursor = len(m.labels) - 1
	}
}

func (m *fakeMenu) moveDown() {
	if m.cursor < len(m.labels)-1 {
		m.cursor++
	} else if m.wrap {
		m.cursor = 0
	}
}

// pump drains keystrokes from the driver's "PTY" and applies them, buffering a
// trailing partial escape sequence across reads.
func (m *fakeMenu) pump(pr *os.File) {
	buf := make([]byte, 16)
	var rem []byte
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			rem = m.feed(append(rem, buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

func (m *fakeMenu) feed(b []byte) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := 0
	for i < len(b) {
		switch {
		case b[i] == 0x1b:
			if i+2 >= len(b) {
				return append([]byte(nil), b[i:]...) // incomplete escape; carry over
			}
			if b[i+1] == '[' && b[i+2] == 'A' {
				m.moveUp()
			} else if b[i+1] == '[' && b[i+2] == 'B' {
				m.moveDown()
			}
			i += 3
		case b[i] == ' ':
			if m.multi {
				m.selected[m.cursor] = !m.selected[m.cursor]
			}
			i++
		case b[i] == '\r':
			if m.dropEnters > 0 {
				m.dropEnters-- // simulate the submit not registering
			} else if !m.submitted {
				m.submitted = true
				if m.multi {
					m.submittedSel = sortedKeys(m.selected)
				} else {
					m.submittedSel = []int{m.cursor}
				}
				close(m.done)
			}
			i++
		default:
			i++
		}
		m.renderLocked()
	}
	return nil
}

func (m *fakeMenu) result() []int  { m.mu.Lock(); defer m.mu.Unlock(); return m.submittedSel }
func (m *fakeMenu) cursorNow() int { m.mu.Lock(); defer m.mu.Unlock(); return m.cursor }

func sortedKeys(mp map[int]bool) []int {
	var ks []int
	for k, v := range mp {
		if v {
			ks = append(ks, k)
		}
	}
	sort.Ints(ks)
	return ks
}

func choicesFrom(labels []string) []Choice {
	cs := make([]Choice, len(labels))
	for i, l := range labels {
		cs[i] = Choice{Label: l}
	}
	return cs
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fastMenuTiming shrinks the keystroke delays so the simulation runs quickly.
func fastMenuTiming(t *testing.T) {
	t.Helper()
	k, n, q, f, w := keyDelay, navStepDelay, questionSettle, freeTextSettle, submitConfirmWindow
	keyDelay, navStepDelay, questionSettle, freeTextSettle = time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond
	submitConfirmWindow = 20 * time.Millisecond
	t.Cleanup(func() {
		keyDelay, navStepDelay, questionSettle, freeTextSettle, submitConfirmWindow = k, n, q, f, w
	})
}

// driveAnswer runs the real answerOne against a fake menu and waits for submit.
func driveAnswer(t *testing.T, m *fakeMenu, q Question, ans Answer) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	go m.pump(pr)
	d := &Driver{ptyFile: pw, term: m.term, log: discardLog()}
	if err := d.answerOne(q, ans); err != nil {
		t.Fatalf("answerOne: %v", err)
	}
	select {
	case <-m.done:
	case <-time.After(3 * time.Second):
		t.Fatalf("menu never submitted (cursor now %d)", m.cursorNow())
	}
}

func TestDriveByScreen_SingleSelectLandsOnTarget(t *testing.T) {
	fastMenuTiming(t)
	labels := []string{"Coffee", "Tea", "Mate", "Water"}
	q := Question{Options: choicesFrom(labels)}
	for target := range labels {
		for _, start := range []int{0, 1, 3} {
			m := newFakeMenu(labels, false, false, start)
			driveAnswer(t, m, q, Answer{Indices: []int{target}})
			if got := m.result(); len(got) != 1 || got[0] != target {
				t.Errorf("target=%d start=%d: landed on %v, want [%d]", target, start, got, target)
			}
		}
	}
}

func TestDriveByScreen_SingleSelectWraps(t *testing.T) {
	fastMenuTiming(t)
	labels := []string{"Coffee", "Tea", "Mate", "Water"}
	q := Question{Options: choicesFrom(labels)}
	// Cursor starts mid-list AND the list wraps — the exact case the old blind
	// Up*N "normalize to top" gets wrong. Closed-loop must still land exactly.
	for target := range labels {
		m := newFakeMenu(labels, false, true, 2)
		driveAnswer(t, m, q, Answer{Indices: []int{target}})
		if got := m.result(); len(got) != 1 || got[0] != target {
			t.Errorf("wrap target=%d: landed on %v, want [%d]", target, got, target)
		}
	}
}

func TestBlindSelectMisfiresUnderWrap(t *testing.T) {
	fastMenuTiming(t)
	labels := []string{"Coffee", "Tea", "Mate", "Water"}
	// The old open-loop sequence (selectionKeys) against a wrapping list from a
	// non-top start: Up*4 from cursor 2 wraps back to 2, then Enter — landing on
	// the wrong option. This is the bug the closed-loop drive removes (the test
	// above shows the same scenario now lands correctly).
	m := newFakeMenu(labels, false, true, 2)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	go m.pump(pr)
	d := &Driver{ptyFile: pw, term: m.term, log: discardLog()}
	if err := d.sendKeys(selectionKeys(Answer{Indices: []int{0}}, len(labels), false)...); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.done:
	case <-time.After(3 * time.Second):
		t.Fatal("blind drive never submitted")
	}
	if got := m.result(); len(got) == 1 && got[0] == 0 {
		t.Errorf("blind drive unexpectedly landed correctly under wrap (%v); the regression scenario no longer holds", got)
	}
}

func TestDriveByScreen_RecoversWhenEnterDropped(t *testing.T) {
	fastMenuTiming(t)
	labels := []string{"Coffee", "Tea", "Mate", "Water"}
	q := Question{Options: choicesFrom(labels)}
	// The menu ignores the first Enter (the live "submit didn't register" mode).
	// The submit-confirmation must notice the menu didn't close and re-send
	// Enter, still landing on the chosen option rather than stalling.
	m := newFakeMenu(labels, false, false, 0)
	m.dropEnters = 1
	driveAnswer(t, m, q, Answer{Indices: []int{2}})
	if got := m.result(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("landed on %v, want [2] after a dropped Enter", got)
	}
}

func TestDriveByScreen_MultiSelectTogglesExactSet(t *testing.T) {
	fastMenuTiming(t)
	labels := []string{"Mountains", "Beach", "City", "Forest"}
	q := Question{Options: choicesFrom(labels), MultiSelect: true}
	m := newFakeMenu(labels, true, false, 0)
	driveAnswer(t, m, q, Answer{Indices: []int{0, 2, 3}})
	if got, want := m.result(), []int{0, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-select toggled %v, want %v", got, want)
	}
}
