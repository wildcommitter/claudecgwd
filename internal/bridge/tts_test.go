package bridge

import (
	"strings"
	"testing"
)

func TestVoicePolicyShouldSpeak(t *testing.T) {
	long := strings.Repeat("word ", 200)   // > 700 runes
	code := "here you go:\n```go\nx := 1\n```"

	cases := []struct {
		mode        string
		inboundVoice bool
		text        string
		want        bool
	}{
		{"auto", true, "sure, the capital is Paris", true},
		{"auto", false, "sure, the capital is Paris", false}, // text in → text out
		{"always", false, "spoken even without a voice prompt", true},
		{"off", true, "never spoken", false},
		{"auto", true, code, false}, // code is never spoken
		{"auto", true, long, false}, // too long to listen to
		{"always", true, "", false}, // empty
	}
	for _, c := range cases {
		p := NewVoicePolicy(c.mode)
		if got := p.ShouldSpeak(c.inboundVoice, c.text); got != c.want {
			t.Errorf("mode=%s inboundVoice=%v len=%d: got %v want %v",
				c.mode, c.inboundVoice, len(c.text), got, c.want)
		}
	}
}

func TestParseVoiceMode(t *testing.T) {
	for in, want := range map[string]VoiceMode{
		"auto": VoiceAuto, "": VoiceAuto, "on": VoiceAlways, "always": VoiceAlways,
		"off": VoiceOff, "none": VoiceOff,
	} {
		if m, ok := parseVoiceMode(in); !ok || m != want {
			t.Errorf("parseVoiceMode(%q) = (%v,%v), want %v", in, m, ok, want)
		}
	}
	if _, ok := parseVoiceMode("gibberish"); ok {
		t.Error("expected gibberish to be rejected")
	}
}

func TestVoiceOutEnabledNilSafe(t *testing.T) {
	var v *VoiceOut
	if v.Enabled() {
		t.Fatal("nil VoiceOut must report disabled")
	}
	v = &VoiceOut{Synth: nil, Policy: NewVoicePolicy("auto")}
	if v.Enabled() {
		t.Fatal("VoiceOut with nil synth must report disabled")
	}
}
