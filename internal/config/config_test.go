package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/nitish/ratelimiter/internal/config"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "rules-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	f.WriteString(content)
	f.Close()
	return f.Name()
}

func TestLoad_Valid(t *testing.T) {
	path := writeTemp(t, `
redis:
  addr: "localhost:6379"
server:
  addr: ":8080"
rules:
  - name: default
    limit: 100
    window: 60s
    key_source: ip
  - name: auth
    match:
      path_prefix: /auth/login
    limit: 5
    window: 300s
    key_source: "header:X-API-Key"
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(cfg.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(cfg.Rules))
	}
	if cfg.Rules[0].Window.Duration != 60*time.Second {
		t.Errorf("expected 60s window, got %v", cfg.Rules[0].Window.Duration)
	}
	if cfg.Rules[1].Match.PathPrefix != "/auth/login" {
		t.Errorf("expected path prefix /auth/login, got %q", cfg.Rules[1].Match.PathPrefix)
	}
}

func TestLoad_MissingRedisAddr(t *testing.T) {
	path := writeTemp(t, `
rules:
  - name: default
    limit: 100
    window: 60s
    key_source: ip
`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for missing redis.addr")
	}
}

func TestLoad_InvalidKeySource(t *testing.T) {
	path := writeTemp(t, `
redis:
  addr: "localhost:6379"
rules:
  - name: default
    limit: 100
    window: 60s
    key_source: "unknown"
`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for invalid key_source")
	}
}

func TestLoad_InvalidDuration(t *testing.T) {
	path := writeTemp(t, `
redis:
  addr: "localhost:6379"
rules:
  - name: default
    limit: 100
    window: "not-a-duration"
    key_source: ip
`)
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}
