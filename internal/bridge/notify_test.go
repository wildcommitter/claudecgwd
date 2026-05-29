package bridge

import (
	"encoding/base64"
	"testing"
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
		{`{"caption":"no file"}`, false, "", ""},      // missing file → not media
		{"just a plain notification", false, "", ""},  // plain text
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
