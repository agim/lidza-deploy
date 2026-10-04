package agent

import (
	"errors"
	"github.com/agim/lidza-deploy/internal/platform/state"
	"os"
	"path/filepath"
)

// Adapted from monolithcms-app/agent/config.go. Configuration remains private
// and atomically replaced; registration is explicit rather than trust-on-first-use.
type Config struct {
	TLSListen   string `json:"tls_listen"`
	Listen      string `json:"listen"`
	ProxyListen string `json:"proxy_listen"`
	DataDir     string `json:"data_dir"`
	APIKey      string `json:"api_key"`
}

func LoadConfig(path string) (Config, error) {
	c := Config{TLSListen: "127.0.0.1:443", Listen: "127.0.0.1:9090", ProxyListen: "127.0.0.1:8081", DataDir: "/var/lib/lidza-agent"}
	if err := state.Load(path, &c); err != nil {
		return c, err
	}
	if v := os.Getenv("LIDZA_AGENT_TOKEN"); v != "" {
		c.APIKey = v
	}
	if len(c.APIKey) < 32 {
		return c, errors.New("agent API key must contain at least 32 characters")
	}
	if !filepath.IsAbs(c.DataDir) {
		return c, errors.New("data_dir must be absolute")
	}
	return c, nil
}
