package bridge

import (
	"context"
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
	switchedTo string
	switchErr  error
}

func (c *stubController) NewSession(context.Context) (string, error) {
	c.newCalls++
	return "new-session-id", nil
}
func (c *stubController) SwitchProject(_ context.Context, dir string) (string, error) {
	if c.switchErr != nil {
		return "", c.switchErr
	}
	c.switchedTo = "/home/user/" + dir
	return c.switchedTo, nil
}
func (c *stubController) Info() (string, string) { return "/home/user/proj", "sess-1" }

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
	r := NewRouter(sender, ctl, nil, discardLogger(), 0)
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
}
