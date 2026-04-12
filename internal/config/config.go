package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Redis  RedisConfig  `yaml:"redis"`
	Server ServerConfig `yaml:"server"`
	Rules  []RuleConfig `yaml:"rules"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type ServerConfig struct {
	Addr string `yaml:"addr"`
}

type RuleConfig struct {
	Name      string      `yaml:"name"`
	Limit     int64       `yaml:"limit"`
	Window    Duration    `yaml:"window"`
	KeySource string      `yaml:"key_source"`
	Match     MatchConfig `yaml:"match"`
}

type MatchConfig struct {
	PathPrefix string `yaml:"path_prefix"`
}

// Duration wraps time.Duration to support YAML unmarshalling from strings like "60s".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	dur, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = dur
	return nil
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening config file: %w", err)
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Redis.Addr == "" {
		return fmt.Errorf("redis.addr is required")
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	for i, r := range c.Rules {
		if r.Name == "" {
			return fmt.Errorf("rule[%d]: name is required", i)
		}
		if r.Limit <= 0 {
			return fmt.Errorf("rule %q: limit must be > 0", r.Name)
		}
		if r.Window.Duration <= 0 {
			return fmt.Errorf("rule %q: window must be > 0", r.Name)
		}
		if err := validateKeySource(r.KeySource); err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
	}
	return nil
}

func validateKeySource(ks string) error {
	if ks == "" || ks == "ip" {
		return nil
	}
	if strings.HasPrefix(ks, "header:") {
		if strings.TrimPrefix(ks, "header:") == "" {
			return fmt.Errorf("key_source \"header:\" requires a header name")
		}
		return nil
	}
	return fmt.Errorf("unknown key_source %q: must be \"ip\" or \"header:<name>\"", ks)
}
