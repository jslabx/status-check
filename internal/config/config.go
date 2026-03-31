package config

import (
	"fmt"
	"os"

	"github.com/goccy/go-yaml"
)

type Config struct {
	URLs    []string      `yaml:"urls"`
	Mail    MailConfig    `yaml:"mail"`
	Checker CheckerConfig `yaml:"checker"`
}

type MailConfig struct {
	SES SESConfig `yaml:"ses"`
}

type SESConfig struct {
	Enabled bool     `yaml:"enabled"`
	From    string   `yaml:"from"`
	To      []string `yaml:"to"`
	Region  string   `yaml:"region"`
}

type CheckerConfig struct {
	TimeoutSeconds        int `yaml:"timeout_seconds"`
	IntervalSeconds       int `yaml:"interval_seconds"`
	RecheckIntervalSeconds int `yaml:"recheck_interval_seconds"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	applyDefaults(&cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Checker.TimeoutSeconds == 0 {
		cfg.Checker.TimeoutSeconds = 10
	}
	if cfg.Checker.IntervalSeconds == 0 {
		cfg.Checker.IntervalSeconds = 60
	}
}

func (c *Config) Validate() error {
	if len(c.URLs) == 0 {
		return fmt.Errorf("at least one URL must be configured")
	}
	if c.Mail.SES.Enabled {
		if c.Mail.SES.From == "" {
			return fmt.Errorf("mail.ses.from is required when SES is enabled")
		}
		if len(c.Mail.SES.To) == 0 {
			return fmt.Errorf("mail.ses.to must have at least one address when SES is enabled")
		}
		if c.Mail.SES.Region == "" {
			return fmt.Errorf("mail.ses.region is required when SES is enabled")
		}
	}
	return nil
}
