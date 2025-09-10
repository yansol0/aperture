package testconfig

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Auth struct {
	Type       string `yaml:"type"` // "header" or "cookie"
	Value      string `yaml:"value"`
	HeaderName string `yaml:"header_name"` // optional; defaults to Authorization
}

type User struct {
	Name   string            `yaml:"name"`
	Auth   Auth              `yaml:"auth"`
	Fields map[string]string `yaml:"fields"`
}

type Config struct {
	Users                 []User `yaml:"users"`
	DefaultAuthHeaderName string `yaml:"default_auth_header_name"`
}

func Load(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse yaml: %w", err)
	}
	if cfg.DefaultAuthHeaderName == "" {
		cfg.DefaultAuthHeaderName = "Authorization"
	}
	return cfg, nil
}
