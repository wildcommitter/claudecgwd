package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Claude   ClaudeConfig   `yaml:"claude"`
	Telegram TelegramConfig `yaml:"telegram"`
	WhatsApp WhatsAppConfig `yaml:"whatsapp"`
	Files    FilesConfig    `yaml:"files"`
	STT      STTConfig      `yaml:"stt"`
	Router   RouterConfig   `yaml:"router"`
}

type FilesConfig struct {
	// InboxDir is where files sent over chat are downloaded. Defaults to
	// ~/.local/share/assistant/inbox.
	InboxDir string `yaml:"inbox_dir"`
}

type STTConfig struct {
	// Enabled turns voice/audio transcription on: audio messages are
	// transcribed and fed in as the prompt text.
	Enabled bool `yaml:"enabled"`
	// Model is the faster-whisper model name (tiny/base/small/medium/...).
	Model string `yaml:"model"`
	// Python is the venv interpreter with faster-whisper installed. Defaults to
	// ~/.local/share/assistant/stt-venv/bin/python.
	Python string `yaml:"python"`
	// Script is the transcribe.py path. Defaults to <workdir>/scripts/transcribe.py.
	Script string `yaml:"script"`
}

type ClaudeConfig struct {
	Binary         string   `yaml:"binary"`
	Workdir        string   `yaml:"workdir"`
	SessionID      string   `yaml:"session_id"`
	PermissionMode string   `yaml:"permission_mode"`
	PtyCols        uint16   `yaml:"pty_cols"`
	PtyRows        uint16   `yaml:"pty_rows"`
	ExtraArgs      []string `yaml:"extra_args,omitempty"`
	// StallTimeoutS is how long the driver waits for the next transcript
	// progress before declaring an upstream stall and cancelling the
	// in-flight request. 0 = use the built-in default (90s).
	StallTimeoutS int `yaml:"stall_timeout_s,omitempty"`
}

type TelegramConfig struct {
	TokenEnv       string  `yaml:"token_env"`
	AllowedUserIDs []int64 `yaml:"allowed_user_ids"`
	Token          string  `yaml:"-"`
}

type WhatsAppConfig struct {
	// Enabled turns the WhatsApp (whatsmeow linked-device) bridge on.
	Enabled bool `yaml:"enabled"`
	// StorePath is the SQLite file holding the paired session. Defaults to
	// ~/.local/share/assistant/whatsapp.db.
	StorePath string `yaml:"store_path"`
	// AllowedJIDs lists the bare phone numbers (sender "user" part, e.g.
	// "34123456789") permitted to drive the assistant.
	AllowedJIDs []string `yaml:"allowed_jids"`
}

type RouterConfig struct {
	InboundBuffer    int `yaml:"inbound_buffer"`
	ReadyIdleMs      int `yaml:"ready_idle_ms"`
	WatchdogTimeoutS int `yaml:"watchdog_timeout_s"`
}

func (c *TelegramConfig) Enabled() bool { return c.TokenEnv != "" }

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.resolveSecrets(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Claude.Binary == "" {
		c.Claude.Binary = "claude"
	}
	if c.Claude.PermissionMode == "" {
		c.Claude.PermissionMode = "bypassPermissions"
	}
	if c.Claude.PtyCols == 0 {
		c.Claude.PtyCols = 200
	}
	if c.Claude.PtyRows == 0 {
		c.Claude.PtyRows = 500
	}
	if c.WhatsApp.Enabled && c.WhatsApp.StorePath == "" {
		home, _ := os.UserHomeDir()
		c.WhatsApp.StorePath = filepath.Join(home, ".local", "share", "assistant", "whatsapp.db")
	}
	if c.Files.InboxDir == "" {
		home, _ := os.UserHomeDir()
		c.Files.InboxDir = filepath.Join(home, ".local", "share", "assistant", "inbox")
	}
	if c.STT.Enabled {
		home, _ := os.UserHomeDir()
		if c.STT.Model == "" {
			c.STT.Model = "small"
		}
		if c.STT.Python == "" {
			c.STT.Python = filepath.Join(home, ".local", "share", "assistant", "stt-venv", "bin", "python")
		}
		if c.STT.Script == "" {
			c.STT.Script = filepath.Join(c.Claude.Workdir, "scripts", "transcribe.py")
		}
	}
	if c.Router.InboundBuffer == 0 {
		c.Router.InboundBuffer = 32
	}
	if c.Router.ReadyIdleMs == 0 {
		c.Router.ReadyIdleMs = 150
	}
	if c.Router.WatchdogTimeoutS == 0 {
		// 1 hour: generous enough for any non-interactive turn including
		// big tool-heavy multi-step ones. AskChoices waits sit on the
		// parent ctx so this doesn't bound human reply time.
		c.Router.WatchdogTimeoutS = 3600
	}
}

func (c *Config) resolveSecrets() error {
	if c.Telegram.Enabled() {
		c.Telegram.Token = os.Getenv(c.Telegram.TokenEnv)
		if c.Telegram.Token == "" {
			return fmt.Errorf("telegram token env %q is unset", c.Telegram.TokenEnv)
		}
	}
	return nil
}

func (c *Config) validate() error {
	if c.Claude.SessionID == "" {
		return fmt.Errorf("claude.session_id is required (generate with uuidgen)")
	}
	if c.Claude.Workdir == "" {
		return fmt.Errorf("claude.workdir is required")
	}
	if !c.Telegram.Enabled() {
		return fmt.Errorf("telegram must be configured")
	}
	if len(c.Telegram.AllowedUserIDs) == 0 {
		return fmt.Errorf("telegram.allowed_user_ids must list at least one user id")
	}
	if c.WhatsApp.Enabled {
		if !c.Telegram.Enabled() {
			return fmt.Errorf("whatsapp requires telegram (QR pairing is delivered via Telegram)")
		}
		if len(c.WhatsApp.AllowedJIDs) == 0 {
			return fmt.Errorf("whatsapp.allowed_jids must list at least one phone number")
		}
	}
	return nil
}
