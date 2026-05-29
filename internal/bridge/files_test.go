package bridge

import (
	"strings"
	"testing"
)

func TestReceivedNotice(t *testing.T) {
	t.Run("image instructs Claude to view it", func(t *testing.T) {
		got := receivedNotice("telegram", "/inbox/a.jpg", "", true)
		if !strings.Contains(got, "image received via telegram") {
			t.Fatalf("missing header: %q", got)
		}
		if !strings.Contains(got, "Read tool") {
			t.Fatalf("image notice should tell Claude to view it: %q", got)
		}
	})
	t.Run("plain file is catalogued, not viewed", func(t *testing.T) {
		got := receivedNotice("whatsapp", "/inbox/a.pdf", "", false)
		if !strings.Contains(got, "file received via whatsapp") {
			t.Fatalf("missing header: %q", got)
		}
		if strings.Contains(got, "Read tool") {
			t.Fatalf("non-image should not ask to view: %q", got)
		}
	})
	t.Run("caption is appended", func(t *testing.T) {
		got := receivedNotice("telegram", "/inbox/a.jpg", "what is this?", true)
		if !strings.Contains(got, "what is this?") {
			t.Fatalf("caption missing: %q", got)
		}
	})
}
