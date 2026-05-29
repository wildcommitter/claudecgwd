package bridge

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryStore(t *testing.T) {
	m := NewMemoryStore(filepath.Join(t.TempDir(), "memory.md"))

	if m.Preamble() != "" {
		t.Fatal("empty store should yield an empty preamble")
	}

	for _, f := range []string{"Prefers metric units", "Wife is named Ana", "Works 9–5 CET"} {
		if err := m.Add(f); err != nil {
			t.Fatalf("add %q: %v", f, err)
		}
	}
	if err := m.Add("prefers metric units"); err != nil { // dup (case-insensitive)
		t.Fatal(err)
	}
	if got := m.List(); len(got) != 3 {
		t.Fatalf("expected 3 unique facts, got %v", got)
	}

	pre := m.Preamble()
	if !strings.Contains(pre, "persistent memory") || !strings.Contains(pre, "Wife is named Ana") {
		t.Fatalf("preamble missing header or facts: %q", pre)
	}

	t.Run("forget by substring", func(t *testing.T) {
		n, err := m.Forget("wife")
		if err != nil || n != 1 {
			t.Fatalf("forget wife: n=%d err=%v", n, err)
		}
		for _, f := range m.List() {
			if strings.Contains(strings.ToLower(f), "wife") {
				t.Fatalf("'wife' fact survived: %v", m.List())
			}
		}
	})

	t.Run("persists across instances", func(t *testing.T) {
		m2 := NewMemoryStore(m.path)
		if len(m2.List()) != 2 {
			t.Fatalf("reload expected 2 facts, got %v", m2.List())
		}
	})

	t.Run("forget all clears", func(t *testing.T) {
		if _, err := m.Forget("all"); err != nil {
			t.Fatal(err)
		}
		if len(m.List()) != 0 {
			t.Fatalf("expected empty after forget all, got %v", m.List())
		}
	})
}
