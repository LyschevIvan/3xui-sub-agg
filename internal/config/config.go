package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Name               string `yaml:"name"`
	APIURL             string `yaml:"api_url"`
	Path               string `yaml:"path"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	HostOverride       string `yaml:"host_override"`
}

type Config struct {
	Listen          string        `yaml:"listen"`
	RefreshInterval time.Duration `yaml:"refresh_interval"`
	RequestTimeout  time.Duration `yaml:"request_timeout"`
	Servers         []Server      `yaml:"servers"`
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
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("no servers in config")
	}
	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		if s.Name == "" || s.APIURL == "" || s.Username == "" {
			return nil, fmt.Errorf("server[%d]: name, api_url, username are required", i)
		}
		if s.Path == "" {
			s.Path = "/"
		}
		if s.Path[0] != '/' {
			s.Path = "/" + s.Path
		}
		if s.Path[len(s.Path)-1] != '/' {
			s.Path += "/"
		}
	}
	return &cfg, nil
}
