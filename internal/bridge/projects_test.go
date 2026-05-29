package bridge

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestProjectRegistryRecordAndResolve(t *testing.T) {
	store := filepath.Join(t.TempDir(), "projects.tsv")
	r := NewProjectRegistry(store)

	// Record in this order; most-recent-first ordering should put bridge last-recorded first.
	for _, p := range []string{"/home/user/website", "/home/user/claudecgwd", "/home/user/notes-app"} {
		if err := r.Record(p); err != nil {
			t.Fatalf("record %s: %v", p, err)
		}
	}

	t.Run("substring wildcard by default", func(t *testing.T) {
		got := r.Resolve("claude")
		if !reflect.DeepEqual(got, []string{"/home/user/claudecgwd"}) {
			t.Fatalf("resolve claude = %v", got)
		}
	})

	t.Run("matches on basename across entries", func(t *testing.T) {
		got := r.Resolve("app")
		if !reflect.DeepEqual(got, []string{"/home/user/notes-app"}) {
			t.Fatalf("resolve app = %v", got)
		}
	})

	t.Run("glob pattern", func(t *testing.T) {
		got := r.Resolve("*site")
		if !reflect.DeepEqual(got, []string{"/home/user/website"}) {
			t.Fatalf("resolve *site = %v", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		if got := r.Resolve("nonexistent"); got != nil {
			t.Fatalf("expected no match, got %v", got)
		}
	})

	t.Run("re-record updates recency, not duplicates", func(t *testing.T) {
		if err := r.Record("/home/user/website"); err != nil {
			t.Fatal(err)
		}
		list := r.List()
		if len(list) != 3 {
			t.Fatalf("expected 3 unique entries, got %v", list)
		}
		if list[0] != "/home/user/website" {
			t.Fatalf("most-recent should be website, got %v", list)
		}
	})

	t.Run("persists across instances", func(t *testing.T) {
		r2 := NewProjectRegistry(store)
		if got := r2.Resolve("notes"); !reflect.DeepEqual(got, []string{"/home/user/notes-app"}) {
			t.Fatalf("reload resolve notes = %v", got)
		}
	})
}
