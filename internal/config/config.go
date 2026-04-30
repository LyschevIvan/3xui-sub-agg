package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

type AdminBootstrap struct {
	Login    string `yaml:"login"`
	Password string `yaml:"password"`
}

type Config struct {
	Listen          string         `yaml:"listen"`
	PublicURL       string         `yaml:"public_url"` // база для ссылок-приглашений/подписок, напр. "https://sub.example.com"
	RefreshInterval time.Duration  `yaml:"refresh_interval"`
	RequestTimeout  time.Duration  `yaml:"request_timeout"`
	DBPath          string         `yaml:"db_path"`
	CookiesSecure   bool           `yaml:"cookies_secure"`
	Admin           AdminBootstrap `yaml:"admin"`

	// MasterKey — ключ для шифрования паролей 3x-ui в БД (AES-256-GCM).
	// Если пустой — пароли остаются plaintext (поведение по умолчанию,
	// для совместимости с существующими установками). Рекомендуется задавать
	// через ${ENV_VAR} в YAML, чтобы ключ не попал в git.
	MasterKey string `yaml:"master_key"`
}

var envRe = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// Load читает YAML-конфиг и раскрывает ${ENV_VAR}.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded := envRe.ReplaceAllFunc(raw, func(m []byte) []byte {
		name := string(m[2 : len(m)-1])
		return []byte(os.Getenv(name))
	})

	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 5 * time.Minute
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "data.db"
	}
	if cfg.Admin.Login == "" || cfg.Admin.Password == "" {
		return nil, fmt.Errorf("admin.login and admin.password are required (bootstrap credentials)")
	}
	return &cfg, nil
}
