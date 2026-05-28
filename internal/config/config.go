package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Claude   ClaudeConfig   `yaml:"claude"`
	Telegram TelegramConfig `yaml:"telegram"`
	IRC      IRCConfig      `yaml:"irc"`
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
}

type TelegramConfig struct {
	TokenEnv       string  `yaml:"token_env"`
	AllowedUserIDs []int64 `yaml:"allowed_user_ids"`
	Token          string  `yaml:"-"`
}

type IRCConfig struct {
	Server          string   `yaml:"server"`
	TLS             bool     `yaml:"tls"`
	Nick            string   `yaml:"nick"`
	User            string   `yaml:"user,omitempty"`
	RealName        string   `yaml:"real_name,omitempty"`
	SaslUser        string   `yaml:"sasl_user"`
	SaslPassEnv     string   `yaml:"sasl_pass_env"`
	Channels        []string `yaml:"channels"`
	AllowedAccounts []string `yaml:"allowed_accounts"`
	AllowedNicks    []string `yaml:"allowed_nicks,omitempty"`
	SaslPass        string   `yaml:"-"`
}

type RouterConfig struct {
	InboundBuffer    int `yaml:"inbound_buffer"`
	ReadyIdleMs      int `yaml:"ready_idle_ms"`
	WatchdogTimeoutS int `yaml:"watchdog_timeout_s"`
}

func (c *TelegramConfig) Enabled() bool { return c.TokenEnv != "" }
func (c *IRCConfig) Enabled() bool      { return c.Server != "" && c.Nick != "" }

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
		c.Claude.PtyRows = 60
	}
	if c.Router.InboundBuffer == 0 {
		c.Router.InboundBuffer = 32
	}
	if c.Router.ReadyIdleMs == 0 {
		c.Router.ReadyIdleMs = 150
	}
	if c.Router.WatchdogTimeoutS == 0 {
		c.Router.WatchdogTimeoutS = 300
	}
}

func (c *Config) resolveSecrets() error {
	if c.Telegram.Enabled() {
		c.Telegram.Token = os.Getenv(c.Telegram.TokenEnv)
		if c.Telegram.Token == "" {
			return fmt.Errorf("telegram token env %q is unset", c.Telegram.TokenEnv)
		}
	}
	if c.IRC.Enabled() && c.IRC.SaslPassEnv != "" {
		c.IRC.SaslPass = os.Getenv(c.IRC.SaslPassEnv)
		if c.IRC.SaslPass == "" {
			return fmt.Errorf("irc sasl password env %q is unset", c.IRC.SaslPassEnv)
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
	if !c.Telegram.Enabled() && !c.IRC.Enabled() {
		return fmt.Errorf("at least one of telegram or irc must be configured")
	}
	if c.Telegram.Enabled() && len(c.Telegram.AllowedUserIDs) == 0 {
		return fmt.Errorf("telegram.allowed_user_ids must list at least one user id")
	}
	if c.IRC.Enabled() && len(c.IRC.AllowedAccounts) == 0 && len(c.IRC.AllowedNicks) == 0 {
		return fmt.Errorf("irc.allowed_accounts or irc.allowed_nicks must list at least one entry")
	}
	return nil
}
