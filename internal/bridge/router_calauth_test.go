package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeGcalAuth drops an executable stand-in for scripts/gcal-auth that
// mimics the real CLI's contract: `url` prints a consent URL on stdout, and
// `exchange <code>` prints a success line echoing the code. Returns its path.
func writeFakeGcalAuth(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gcal-auth")
	script := "#!/usr/bin/env bash\n" +
		"case \"$1\" in\n" +
		"  url) echo 'https://accounts.google.com/o/oauth2/auth?code_challenge=x' ;;\n" +
		"  exchange) echo \"authorized — token saved ($2)\" ;;\n" +
		"  *) echo \"unknown: $1\" >&2; exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gcal-auth: %v", err)
	}
	return path
}

// calAuthRouter builds a router wired to the given gcal-auth path, with control
// commands enabled (a non-nil SessionController).
func calAuthRouter(gcalCmd string) *Router {
	inbound := make(chan Inbound, 1)
	return NewRouter(&stubSender{}, &stubController{}, nil, nil, nil, "", gcalCmd, inbound, discardLogger(), 0)
}

func TestParseControlRecognizesCalAuth(t *testing.T) {
	name, arg, ok := parseControl("/calauth exchange 4/abc")
	if !ok || name != "calauth" || arg != "exchange 4/abc" {
		t.Fatalf("parseControl(/calauth …) = (%q,%q,%v), want (calauth, exchange 4/abc, true)", name, arg, ok)
	}
}

func TestCalAuthURLStep(t *testing.T) {
	r := calAuthRouter(writeFakeGcalAuth(t))
	o := &captureOrigin{}
	r.handle(context.Background(), Inbound{Origin: o, Text: "/calauth"})

	if len(o.replies) != 1 {
		t.Fatalf("want 1 reply, got %d: %v", len(o.replies), o.replies)
	}
	if !strings.Contains(o.replies[0], "accounts.google.com") {
		t.Errorf("url step should relay the consent URL, got: %q", o.replies[0])
	}
	if !strings.Contains(o.replies[0], "/calauth") {
		t.Errorf("url step should tell the user how to paste back, got: %q", o.replies[0])
	}
}

func TestCalAuthExchangeStep(t *testing.T) {
	r := calAuthRouter(writeFakeGcalAuth(t))
	o := &captureOrigin{}
	// A bare argument (a pasted redirect URL) is treated as the code to exchange.
	r.handle(context.Background(), Inbound{Origin: o, Text: "/calauth http://localhost/?code=4/xyz"})

	if len(o.replies) != 1 {
		t.Fatalf("want 1 reply, got %d: %v", len(o.replies), o.replies)
	}
	if !strings.Contains(o.replies[0], "connected") {
		t.Errorf("exchange step should confirm connection, got: %q", o.replies[0])
	}
	if !strings.Contains(o.replies[0], "4/xyz") {
		t.Errorf("exchange step should pass the pasted code to the script, got: %q", o.replies[0])
	}
}

func TestCalAuthDisabledWhenUnconfigured(t *testing.T) {
	r := calAuthRouter("") // empty gcalCmd disables the command
	o := &captureOrigin{}
	r.handle(context.Background(), Inbound{Origin: o, Text: "/calauth"})

	if len(o.replies) != 1 || !strings.Contains(o.replies[0], "isn't configured") {
		t.Fatalf("want a not-configured reply, got: %v", o.replies)
	}
}
