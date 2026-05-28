package bridge

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/wildcommitter/claudecgwd/internal/config"
)

// Transcriber turns an audio file into text by shelling out to the
// faster-whisper venv (scripts/transcribe.py). A nil *Transcriber, or one whose
// Enabled() is false, means audio is treated as a plain file instead.
type Transcriber struct {
	command string
	model   string
}

func NewTranscriber(cfg config.STTConfig) *Transcriber {
	if !cfg.Enabled {
		return nil
	}
	return &Transcriber{command: cfg.Command, model: cfg.Model}
}

func (t *Transcriber) Enabled() bool { return t != nil && t.command != "" }

// Transcribe returns the transcript of the audio file at path.
func (t *Transcriber) Transcribe(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, t.command, path, t.model)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("transcribe: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("transcribe: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
