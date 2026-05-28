package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Claude   ClaudeConfig   `yaml:"claude"`
	Telegram TelegramConfig `yaml:"telegram"`
	Router   RouterConfig   `yaml:"router"`
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
	return nil
}
