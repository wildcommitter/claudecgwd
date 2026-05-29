package bridge

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MemoryStore is a small, chat-managed set of durable facts about the user
// (preferences, people, projects, context). It is distinct from RAG search over
// transcripts: this is curated "what you know about the user," injected into the
// first prompt of each session so it carries across /new, /project, and
// restarts. The assistant appends facts with scripts/remember; the user manages
// them with /memory and /forget.
//
// The store is a plain markdown bullet list, one fact per line, so it's
// human-readable and the shell helper can append to it without escaping.
type MemoryStore struct {
	path string
	mu   sync.Mutex
}

func NewMemoryStore(path string) *MemoryStore {
	return &MemoryStore{path: MemoryPath(path)}
}

// MemoryPath resolves the store path: explicit, else $CLAUDECGWD_MEMORY, else
// ~/.local/share/assistant/memory.md.
func MemoryPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("CLAUDECGWD_MEMORY"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "assistant", "memory.md")
}

// List returns the stored facts (bullet markers stripped), in file order.
func (m *MemoryStore) List() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load()
}

func (m *MemoryStore) load() []string {
	f, err := os.Open(m.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "- ")
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Add appends a fact. Trivial de-duplication: an identical fact isn't added
// twice.
func (m *MemoryStore) Add(fact string) error {
	fact = strings.TrimSpace(fact)
	if fact == "" {
		return fmt.Errorf("empty fact")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.load() {
		if strings.EqualFold(f, fact) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(m.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "- %s\n", strings.ReplaceAll(fact, "\n", " "))
	return err
}

// Forget removes facts containing substr (case-insensitive); substr "all" or "*"
// clears everything. Returns how many were removed.
func (m *MemoryStore) Forget(substr string) (int, error) {
	substr = strings.TrimSpace(substr)
	if substr == "" {
		return 0, fmt.Errorf("nothing specified")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	facts := m.load()
	clearAll := substr == "all" || substr == "*"
	needle := strings.ToLower(substr)
	kept := make([]string, 0, len(facts))
	removed := 0
	for _, f := range facts {
		if clearAll || strings.Contains(strings.ToLower(f), needle) {
			removed++
			continue
		}
		kept = append(kept, f)
	}
	if removed == 0 {
		return 0, nil
	}
	return removed, m.write(kept)
}

func (m *MemoryStore) write(facts []string) error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o700); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	var b strings.Builder
	for _, f := range facts {
		fmt.Fprintf(&b, "- %s\n", f)
	}
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// Preamble formats the stored facts as a context block to prepend to the first
// prompt of a session, or "" when there's nothing remembered.
func (m *MemoryStore) Preamble() string {
	facts := m.List()
	if len(facts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[persistent memory — durable facts you know about the user; treat as background context, don't recite unprompted]\n")
	for _, f := range facts {
		b.WriteString("- ")
		b.WriteString(f)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
