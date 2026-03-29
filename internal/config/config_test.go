package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"status-check/internal/config"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	f.Close()
	return f.Name()
}

func TestLoad_ValidMinimal(t *testing.T) {
	path := writeTemp(t, `
urls:
  - https://example.com
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.URLs) != 1 || cfg.URLs[0] != "https://example.com" {
		t.Errorf("unexpected URLs: %v", cfg.URLs)
	}
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTemp(t, `
urls:
  - https://example.com
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Checker.TimeoutSeconds != 10 {
		t.Errorf("expected default timeout 10, got %d", cfg.Checker.TimeoutSeconds)
	}
	if cfg.Checker.IntervalSeconds != 60 {
		t.Errorf("expected default interval 60, got %d", cfg.Checker.IntervalSeconds)
	}
	// Zero recheck_interval_seconds means repeat alerts are not throttled.
	if cfg.Checker.RecheckIntervalSeconds != 0 {
		t.Errorf("expected default recheck interval 0 (disabled), got %d", cfg.Checker.RecheckIntervalSeconds)
	}
}

func TestLoad_ExplicitRecheckInterval(t *testing.T) {
	path := writeTemp(t, `
urls:
  - https://example.com
checker:
  recheck_interval_seconds: 300
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Checker.RecheckIntervalSeconds != 300 {
		t.Errorf("expected recheck interval 300, got %d", cfg.Checker.RecheckIntervalSeconds)
	}
}

func TestLoad_ExplicitValues(t *testing.T) {
	path := writeTemp(t, `
urls:
  - https://example.com
  - https://other.com
checker:
  timeout_seconds: 30
  interval_seconds: 120
  recheck_interval_seconds: 600
mail:
  ses:
    enabled: true
    from: alerts@example.com
    to:
      - admin@example.com
    region: eu-west-2
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.URLs) != 2 {
		t.Errorf("expected 2 URLs, got %d", len(cfg.URLs))
	}
	if cfg.Checker.TimeoutSeconds != 30 {
		t.Errorf("expected timeout 30, got %d", cfg.Checker.TimeoutSeconds)
	}
	if cfg.Checker.IntervalSeconds != 120 {
		t.Errorf("expected interval 120, got %d", cfg.Checker.IntervalSeconds)
	}
	if cfg.Checker.RecheckIntervalSeconds != 600 {
		t.Errorf("expected recheck interval 600, got %d", cfg.Checker.RecheckIntervalSeconds)
	}
	if !cfg.Mail.SES.Enabled {
		t.Error("expected SES to be enabled")
	}
	if cfg.Mail.SES.From != "alerts@example.com" {
		t.Errorf("unexpected SES from: %s", cfg.Mail.SES.From)
	}
	if cfg.Mail.SES.Region != "eu-west-2" {
		t.Errorf("unexpected SES region: %s", cfg.Mail.SES.Region)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTemp(t, `this: is: not: valid: yaml: [[[`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestLoad_NoURLs(t *testing.T) {
	path := writeTemp(t, `
mail:
  ses:
    enabled: false
`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for missing URLs, got nil")
	}
}

func TestLoad_SESEnabledMissingFrom(t *testing.T) {
	path := writeTemp(t, `
urls:
  - https://example.com
mail:
  ses:
    enabled: true
    to:
      - admin@example.com
    region: us-east-1
`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for missing SES from, got nil")
	}
}

func TestLoad_SESEnabledMissingTo(t *testing.T) {
	path := writeTemp(t, `
urls:
  - https://example.com
mail:
  ses:
    enabled: true
    from: alerts@example.com
    region: us-east-1
`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for missing SES to, got nil")
	}
}

func TestLoad_SESEnabledMissingRegion(t *testing.T) {
	path := writeTemp(t, `
urls:
  - https://example.com
mail:
  ses:
    enabled: true
    from: alerts@example.com
    to:
      - admin@example.com
`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for missing SES region, got nil")
	}
}
