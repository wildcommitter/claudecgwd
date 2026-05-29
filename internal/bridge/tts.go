package bridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/wildcommitter/claudecgwd/internal/config"
)

// Synthesizer turns reply text into an OGG/Opus voice note by shelling out to
// the local piper venv (scripts/tts). A nil *Synthesizer, or one whose
// Enabled() is false, means replies are text-only.
type Synthesizer struct {
	command string
	voice   string          // fallback voice when no language policy is set
	lang    *LanguagePolicy // optional: selects the voice for the current language
}

func NewSynthesizer(cfg config.TTSConfig, lang *LanguagePolicy) *Synthesizer {
	if !cfg.Enabled {
		return nil
	}
	return &Synthesizer{command: cfg.Command, voice: cfg.Voice, lang: lang}
}

func (s *Synthesizer) Enabled() bool { return s != nil && s.command != "" }

// voiceFor returns the piper voice to use for this reply. In auto mode the
// voice follows the reply's detected language (so a Spanish answer is spoken by
// a Spanish voice); with a language pinned via /speech it's that fixed voice.
// Falls back to the policy/default voice when detection is unconfident.
func (s *Synthesizer) voiceFor(text string) string {
	if s.lang != nil {
		if s.lang.AutoVoice() {
			if l := detectVoiceLanguage(text); l != nil && l.Piper != "" {
				return l.Piper
			}
		}
		if v := s.lang.PiperVoice(); v != "" {
			return v
		}
	}
	return s.voice
}

// Synthesize writes text to a temporary OGG/Opus file and returns its path. The
// caller is responsible for removing the file once sent.
func (s *Synthesizer) Synthesize(ctx context.Context, text string) (string, error) {
	f, err := os.CreateTemp("", "tts-*.ogg")
	if err != nil {
		return "", err
	}
	path := f.Name()
	_ = f.Close()

	cmd := exec.CommandContext(ctx, s.command, path)
	cmd.Stdin = strings.NewReader(text)
	if v := s.voiceFor(text); v != "" {
		cmd.Env = append(os.Environ(), "TTS_VOICE="+v)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("tts: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return path, nil
}

// VoiceMode controls when replies are spoken.
type VoiceMode int32

const (
	VoiceAuto   VoiceMode = iota // speak only when the user's message was a voice note
	VoiceAlways                  // speak every (speakable) reply
	VoiceOff                     // never speak
)

// VoicePolicy is the live, runtime-toggleable voice-reply setting (shared by the
// surfaces, which read it, and the /voice command, which sets it).
type VoicePolicy struct{ mode atomic.Int32 }

func NewVoicePolicy(mode string) *VoicePolicy {
	p := &VoicePolicy{}
	if m, ok := parseVoiceMode(mode); ok {
		p.mode.Store(int32(m))
	}
	return p
}

func parseVoiceMode(s string) (VoiceMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto", "":
		return VoiceAuto, true
	case "always", "on":
		return VoiceAlways, true
	case "off", "none":
		return VoiceOff, true
	}
	return VoiceAuto, false
}

func (p *VoicePolicy) Mode() VoiceMode { return VoiceMode(p.mode.Load()) }
func (p *VoicePolicy) Set(m VoiceMode) { p.mode.Store(int32(m)) }

func (m VoiceMode) String() string {
	switch m {
	case VoiceAlways:
		return "always"
	case VoiceOff:
		return "off"
	default:
		return "auto"
	}
}

// ShouldSpeak decides whether to deliver text as a voice note, given whether the
// triggering inbound message was itself a voice note.
func (p *VoicePolicy) ShouldSpeak(inboundWasVoice bool, text string) bool {
	switch p.Mode() {
	case VoiceOff:
		return false
	case VoiceAlways:
		return speakable(text)
	default: // auto
		return inboundWasVoice && speakable(text)
	}
}

// speakable filters out replies that make poor voice notes: empty text, code
// (fenced blocks), and anything too long to comfortably listen to.
func speakable(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || strings.Contains(t, "```") {
		return false
	}
	return utf8.RuneCountInString(t) <= 700
}

// VoiceOut bundles the synthesizer, on/off policy, and language policy so they
// can be wired through the bridges and router as one optional dependency
// (nil = voice replies disabled).
type VoiceOut struct {
	Synth  *Synthesizer
	Policy *VoicePolicy
	Lang   *LanguagePolicy
}

// Enabled reports whether the TTS engine is available at all.
func (v *VoiceOut) Enabled() bool { return v != nil && v.Synth.Enabled() }

// CanSpeak reports whether a spoken reply is currently possible — the engine is
// enabled AND the active language has a voice (some languages are STT-only).
func (v *VoiceOut) CanSpeak() bool {
	if !v.Enabled() {
		return false
	}
	return v.Lang == nil || v.Lang.PiperVoice() != ""
}
