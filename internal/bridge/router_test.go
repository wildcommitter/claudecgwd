package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/wildcommitter/claudecgwd/internal/claude"
)

func TestParseControl(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantArg  string
		wantOK   bool
	}{
		{"/new", "new", "", true},
		{"  /new  ", "new", "", true},
		{"/Help", "help", "", true},
		{"/status", "status", "", true},
		{"/project ~/code/foo", "project", "~/code/foo", true},
		{"/project   spaced name ", "project", "spaced name", true},
		{"hello there", "", "", false},
		{"/effort high", "", "", false}, // unknown slash -> passes through to Claude
		{"", "", "", false},
	}
	for _, c := range cases {
		name, arg, ok := parseControl(c.in)
		if ok != c.wantOK || name != c.wantName || arg != c.wantArg {
			t.Errorf("parseControl(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, name, arg, ok, c.wantName, c.wantArg, c.wantOK)
		}
	}
}

// --- stubs ---

type stubSender struct {
	mu      sync.Mutex
	prompts []string
}

func (s *stubSender) Send(_ context.Context, prompt string, _ claude.ChoiceAsker) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, prompt)
	return "ok", nil
}

type stubController struct {
	newCalls   int
	switchArg  string // raw dir passed to SwitchProject
	switchedTo string
	switchErr  error
}

func (c *stubController) NewSession(context.Context) (string, error) {
	c.newCalls++
	return "new-session-id", nil
}
func (c *stubController) SwitchProject(_ context.Context, dir string) (string, error) {
	c.switchArg = dir
	if c.switchErr != nil {
		return "", c.switchErr
	}
	// A registry hit passes an absolute path through unchanged; a bare name
	// (no registry match) is joined under /home/user like resolveWorkdir.
	if strings.HasPrefix(dir, "/") {
		c.switchedTo = dir
	} else {
		c.switchedTo = "/home/user/" + dir
	}
	return c.switchedTo, nil
}
func (c *stubController) Info() (string, string) { return "/home/user/proj", "sess-1" }
func (c *stubController) Generation() int        { return 1 }

type captureOrigin struct {
	mu      sync.Mutex
	replies []string
}

func (o *captureOrigin) Describe() string              { return "test" }
func (o *captureOrigin) NotifyPending(context.Context) {}
func (o *captureOrigin) Reply(_ context.Context, text string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.replies = append(o.replies, text)
	return nil
}
func (o *captureOrigin) AskChoices(context.Context, []claude.Question) ([]claude.Answer, error) {
	return nil, nil
}

func TestRouterControlCommands(t *testing.T) {
	sender := &stubSender{}
	ctl := &stubController{}
	r := NewRouter(sender, ctl, nil, nil, nil, "", nil, discardLogger(), 0)
	ctx := context.Background()

	t.Run("/new restarts the session and does not reach Claude", func(t *testing.T) {
		o := &captureOrigin{}
		r.handle(ctx, Inbound{Text: "/new", Origin: o})
		if ctl.newCalls != 1 {
			t.Fatalf("NewSession called %d times, want 1", ctl.newCalls)
		}
		if len(sender.prompts) != 0 {
			t.Fatalf("control command leaked to Claude: %v", sender.prompts)
		}
		if len(o.replies) != 1 {
			t.Fatalf("expected one reply, got %v", o.replies)
		}
	})

	t.Run("/project with no arg shows usage", func(t *testing.T) {
		o := &captureOrigin{}
		r.handle(ctx, Inbound{Text: "/project", Origin: o})
		if ctl.switchedTo != "" {
			t.Fatalf("SwitchProject should not have been called")
		}
		if len(o.replies) != 1 || o.replies[0] == "" {
			t.Fatalf("expected a usage reply, got %v", o.replies)
		}
	})

	t.Run("unknown slash passes through to Claude", func(t *testing.T) {
		o := &captureOrigin{}
		r.handle(ctx, Inbound{Text: "/effort high", Origin: o})
		if len(sender.prompts) != 1 {
			t.Fatalf("expected the prompt to reach Claude, got %v", sender.prompts)
		}
	})

	t.Run("/health reports a snapshot", func(t *testing.T) {
		o := &captureOrigin{}
		r.handle(ctx, Inbound{Text: "/health", Origin: o})
		if len(o.replies) != 1 ||
			!strings.Contains(o.replies[0], "Health") ||
			!strings.Contains(o.replies[0], "Uptime") ||
			!strings.Contains(o.replies[0], "/home/user/proj") {
			t.Fatalf("expected a health snapshot, got %v", o.replies)
		}
	})

	t.Run("/search with no rag configured is reported, not run", func(t *testing.T) {
		o := &captureOrigin{}
		r.handle(ctx, Inbound{Text: "/search anything", Origin: o})
		if len(o.replies) != 1 || !strings.Contains(o.replies[0], "isn't configured") {
			t.Fatalf("expected a not-configured reply, got %v", o.replies)
		}
	})
}

func TestRouterProjectWildcard(t *testing.T) {
	reg := NewProjectRegistry(t.TempDir() + "/projects.tsv")
	if err := reg.Record("/home/user/claudecgwd"); err != nil {
		t.Fatal(err)
	}
	ctl := &stubController{}
	r := NewRouter(&stubSender{}, ctl, reg, nil, nil, "", nil, discardLogger(), 0)
	ctx := context.Background()

	t.Run("bare name resolves via registry to the tracked path", func(t *testing.T) {
		o := &captureOrigin{}
		r.handle(ctx, Inbound{Text: "/project claude", Origin: o})
		if ctl.switchArg != "/home/user/claudecgwd" {
			t.Fatalf("SwitchProject got %q, want the resolved path", ctl.switchArg)
		}
	})

	t.Run("explicit path is passed through literally", func(t *testing.T) {
		o := &captureOrigin{}
		r.handle(ctx, Inbound{Text: "/project ~/other/repo", Origin: o})
		if ctl.switchArg != "~/other/repo" {
			t.Fatalf("path arg should pass through literally, got %q", ctl.switchArg)
		}
	})

	t.Run("/projects lists tracked dirs", func(t *testing.T) {
		o := &captureOrigin{}
		r.handle(ctx, Inbound{Text: "/projects", Origin: o})
		if len(o.replies) != 1 || !strings.Contains(o.replies[0], "/home/user/claudecgwd") {
			t.Fatalf("expected the tracked dir in the listing, got %v", o.replies)
		}
	})
}
