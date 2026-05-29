package bridge

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProjectRegistry is the persistent set of project directories the assistant
// has worked in. It is the "serialize down / serialize up" store behind project
// tracking: every /project switch (and the startup workdir) is recorded here,
// so the list survives the session restart a switch causes, and a later mention
// of a project by (partial) name can be resolved by wildcard against it.
//
// The store is a tiny append-friendly TSV ("<abspath>\t<RFC3339-last-used>"),
// the same escaping-free shape the reminder store uses, so the bundled
// project-tracker skill can read and write it from shell too.
type ProjectRegistry struct {
	path string
	mu   sync.Mutex
}

func NewProjectRegistry(path string) *ProjectRegistry {
	return &ProjectRegistry{path: ProjectsPath(path)}
}

// ProjectsPath resolves the store path: the explicit arg, else
// $CLAUDECGWD_PROJECTS, else ~/.local/share/assistant/projects.tsv.
func ProjectsPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("CLAUDECGWD_PROJECTS"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "assistant", "projects.tsv")
}

// projectEntry is one tracked directory.
type projectEntry struct {
	path     string
	lastUsed time.Time
}

// Record upserts dir as used now. A no-op error (bad path) is returned so the
// caller can log it, but recording is best-effort — it never blocks a switch.
func (r *ProjectRegistry) Record(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.load()
	now := time.Now().UTC()
	found := false
	for i := range entries {
		if entries[i].path == abs {
			entries[i].lastUsed = now
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, projectEntry{path: abs, lastUsed: now})
	}
	return r.save(entries)
}

// List returns the known project paths, most-recently-used first.
func (r *ProjectRegistry) List() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.load()
	sortByRecency(entries)
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.path
	}
	return out
}

// Resolve returns the tracked directories matching pattern, most-recent first.
// Matching is wildcard by default: a pattern with no glob metacharacter is
// treated as a case-insensitive substring of the basename or the full path; a
// pattern containing * or ? is matched as a glob against the basename.
func (r *ProjectRegistry) Resolve(pattern string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.load()
	sortByRecency(entries)
	pat := strings.ToLower(strings.TrimSpace(pattern))
	glob := strings.ContainsAny(pat, "*?[")
	var out []string
	for _, e := range entries {
		base := strings.ToLower(filepath.Base(e.path))
		full := strings.ToLower(e.path)
		var hit bool
		if glob {
			hit, _ = filepath.Match(pat, base)
		} else {
			hit = strings.Contains(base, pat) || strings.Contains(full, pat)
		}
		if hit {
			out = append(out, e.path)
		}
	}
	return out
}

func sortByRecency(entries []projectEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].lastUsed.After(entries[j].lastUsed)
	})
}

// load reads the store. Missing file = empty. Malformed lines are skipped.
// Callers hold r.mu.
func (r *ProjectRegistry) load() []projectEntry {
	f, err := os.Open(r.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []projectEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if parts[0] == "" {
			continue
		}
		e := projectEntry{path: parts[0]}
		if len(parts) == 2 {
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[1])); err == nil {
				e.lastUsed = t
			}
		}
		out = append(out, e)
	}
	return out
}

// save rewrites the store atomically. Callers hold r.mu.
func (r *ProjectRegistry) save(entries []projectEntry) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range entries {
		ts := e.lastUsed
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\n", e.path, ts.Format(time.RFC3339)); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
