package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Telegram   TelegramConfig   `yaml:"telegram"`
	Session    SessionConfig    `yaml:"session"`
	Claude     ClaudeConfig     `yaml:"claude"`
	Workspaces WorkspacesConfig `yaml:"workspaces"`
	Memory     MemoryConfig     `yaml:"memory"`
}

type TelegramConfig struct {
	BotToken       string  `yaml:"bot_token"`
	AllowedUserIDs []int64 `yaml:"allowed_user_ids"`
}

type SessionConfig struct {
	MaxResponseLength int           `yaml:"max_response_length"`
	EditInterval      time.Duration `yaml:"edit_interval"`
}

type ClaudeConfig struct {
	Model        string  `yaml:"model"`
	MaxBudgetUSD float64 `yaml:"max_budget_usd"`
}

type WorkspacesConfig struct {
	BasePath string            `yaml:"base_path"`
	ChatMap  map[string]string `yaml:"chat_map"`
	Default  string            `yaml:"default"`
}

type MemoryConfig struct {
	DBPath          string        `yaml:"db_path"`
	BriefingInterval time.Duration `yaml:"briefing_interval"`
	HistoryMessages int           `yaml:"history_messages"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// Expand environment variables in the YAML
	expanded := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Telegram.BotToken == "" {
		return fmt.Errorf("telegram.bot_token is required")
	}
	if len(c.Telegram.AllowedUserIDs) == 0 {
		return fmt.Errorf("telegram.allowed_user_ids must have at least one entry")
	}
	if c.Workspaces.BasePath == "" {
		return fmt.Errorf("workspaces.base_path is required")
	}

	// Apply defaults
	if c.Session.MaxResponseLength == 0 {
		c.Session.MaxResponseLength = 4096
	}
	if c.Session.EditInterval == 0 {
		c.Session.EditInterval = 2 * time.Second
	}
	if c.Claude.Model == "" {
		c.Claude.Model = "sonnet"
	}
	if c.Workspaces.Default == "" {
		c.Workspaces.Default = "home"
	}
	if c.Memory.HistoryMessages == 0 {
		c.Memory.HistoryMessages = 20
	}

	return nil
}
