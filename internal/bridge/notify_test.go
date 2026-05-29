package bridge

import (
	"context"
	"encoding/base64"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseMediaDirective(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantFile string
		wantCap  string
	}{
		{`{"file":"/tmp/chart.png","caption":"here"}`, true, "/tmp/chart.png", "here"},
		{`{"file":"/tmp/a.pdf"}`, true, "/tmp/a.pdf", ""},
		{`{"caption":"no file"}`, false, "", ""},     // missing file → not media
		{"just a plain notification", false, "", ""}, // plain text
		{`{"not":"json-we-care-about"}`, false, "", ""},
		{"", false, "", ""},
	}
	for _, c := range cases {
		d, ok := parseMediaDirective(c.in)
		if ok != c.wantOK {
			t.Errorf("parseMediaDirective(%q) ok=%v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && (d.File != c.wantFile || d.Caption != c.wantCap) {
			t.Errorf("parseMediaDirective(%q) = {%q,%q}, want {%q,%q}", c.in, d.File, d.Caption, c.wantFile, c.wantCap)
		}
	}
}

// A proactive push that fails during the post-restart reconnect window must be
// retried until the surface comes up, not dropped — and a permanently-failing
// one must give up within the window rather than spin forever.
func TestNotifierDeliverRidesOutReconnect(t *testing.T) {
	owindow, obackoff := notifyDeliverWindow, notifyDeliverBackoff
	notifyDeliverWindow, notifyDeliverBackoff = 500*time.Millisecond, 5*time.Millisecond
	defer func() { notifyDeliverWindow, notifyDeliverBackoff = owindow, obackoff }()

	n := &Notifier{log: discardLogger()}

	// Fails twice ("not ready"), then connects — deliver should keep trying.
	var calls atomic.Int32
	n.deliver(context.Background(), "test", func(context.Context) error {
		if calls.Add(1) < 3 {
			return errors.New("telegram bot not ready")
		}
		return nil
	})
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected 3 attempts (2 fail + recover), got %d", got)
	}

	// Permanently down: must give up within the window, not hang.
	var down atomic.Int32
	done := make(chan struct{})
	go func() {
		n.deliver(context.Background(), "down", func(context.Context) error {
			down.Add(1)
			return errors.New("websocket not connected")
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deliver did not give up within the window")
	}
	if down.Load() < 2 {
		t.Fatalf("expected multiple attempts before giving up, got %d", down.Load())
	}

	// A cancelled parent ctx stops retries promptly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	n.deliver(ctx, "cancelled", func(context.Context) error { return errors.New("x") })
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("deliver ignored ctx cancellation")
	}
}

// The send-file script base64-encodes the JSON onto a FIFO line; ensure the
// notifier's decode→parse path recovers it (mirrors notify.sh's wire format).
func TestMediaDirectiveBase64RoundTrip(t *testing.T) {
	json := `{"file": "/inbox/report.pdf", "caption": "Q2 report"}`
	line := base64.StdEncoding.EncodeToString([]byte(json))
	d, ok := parseMediaDirective(decodeNotif(line))
	if !ok || d.File != "/inbox/report.pdf" || d.Caption != "Q2 report" {
		t.Fatalf("round trip failed: ok=%v d=%+v", ok, d)
	}
}
