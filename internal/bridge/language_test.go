package bridge

import "testing"

func TestLookupLanguage(t *testing.T) {
	cases := []struct {
		query       string
		wantWhisper string
		wantHasVoice bool
		wantOK      bool
	}{
		{"spanish", "es", true, true},
		{"Spain", "es", true, true},
		{"es", "es", true, true},
		{"mexico", "es", true, true}, // country alias, different voice, same whisper code
		{"de", "de", true, true},
		{"germany", "de", true, true},
		{"japanese", "ja", false, true}, // STT-only: no piper voice
		{"auto", "", true, true},        // auto-detect whisper, English voice
		{"klingon", "", false, false},
	}
	for _, c := range cases {
		l, ok := lookupLanguage(c.query)
		if ok != c.wantOK {
			t.Errorf("lookupLanguage(%q) ok=%v, want %v", c.query, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if l.Whisper != c.wantWhisper {
			t.Errorf("lookupLanguage(%q) whisper=%q, want %q", c.query, l.Whisper, c.wantWhisper)
		}
		if (l.Piper != "") != c.wantHasVoice {
			t.Errorf("lookupLanguage(%q) hasVoice=%v, want %v", c.query, l.Piper != "", c.wantHasVoice)
		}
	}
}

func TestLanguagePolicy(t *testing.T) {
	p := NewLanguagePolicy("auto")
	if p.WhisperCode() != "" || p.PiperVoice() == "" {
		t.Fatalf("auto: want empty whisper + an English voice, got whisper=%q voice=%q", p.WhisperCode(), p.PiperVoice())
	}
	es, _ := lookupLanguage("spanish")
	p.Set(es)
	if p.WhisperCode() != "es" || p.PiperVoice() != "es_ES-davefx-medium" {
		t.Fatalf("after /speech spanish: got whisper=%q voice=%q", p.WhisperCode(), p.PiperVoice())
	}
	// An unknown default falls back to auto.
	if NewLanguagePolicy("nonsense").Current().Name != "Auto-detect" {
		t.Fatal("unknown default should fall back to auto-detect")
	}

	// CanSpeak: STT-only language (Japanese) has no voice → can't speak.
	ja, _ := lookupLanguage("japanese")
	v := &VoiceOut{Synth: nil, Policy: NewVoicePolicy("auto"), Lang: p}
	v.Lang.Set(ja)
	if v.CanSpeak() {
		t.Fatal("Japanese has no voice and TTS is nil; CanSpeak must be false")
	}
}
