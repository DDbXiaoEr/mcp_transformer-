package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen  string         `yaml:"listen"`
	Admin   AdminConfig    `yaml:"admin"`
	Servers []ServerConfig `yaml:"servers"`
}

type AdminConfig struct {
	Listen      string `yaml:"listen"`
	StatusPath  string `yaml:"statusPath"`
	ControlPath string `yaml:"controlPath"`
}

type ServerConfig struct {
	Name            string            `yaml:"name"`
	Endpoint        string            `yaml:"endpoint"`
	Version         string            `yaml:"version"`
	ProtocolVersion string            `yaml:"protocolVersion"`
	Command         string            `yaml:"command"`
	Args            []string          `yaml:"args"`
	Env             map[string]string `yaml:"env"`
}

const (
	defaultStatusPath  = "/__admin/status"
	defaultControlPath = "/__admin/control"
	defaultListen      = ":8080"
	defaultAdminListen = ":8081"
)

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("no servers configured")
	}
	return &cfg, nil
}

func validateServer(sc *ServerConfig) error {
	if sc.Name == "" {
		return fmt.Errorf("name is required")
	}
	if sc.Command == "" {
		return fmt.Errorf("command is required")
	}
	if sc.Endpoint == "" {
		return fmt.Errorf("endpoint is required")
	}
	if !strings.HasPrefix(sc.Endpoint, "/") {
		return fmt.Errorf("endpoint must start with '/'")
	}
	if sc.ProtocolVersion != "" && !slices.Contains(mcp.ValidProtocolVersions, sc.ProtocolVersion) {
		return fmt.Errorf("unsupported protocolVersion %q (valid: %s)", sc.ProtocolVersion, strings.Join(mcp.ValidProtocolVersions, ", "))
	}
	return nil
}

func adminPaths(a AdminConfig) (string, string) {
	statusPath := a.StatusPath
	if statusPath == "" {
		statusPath = defaultStatusPath
	}
	controlPath := a.ControlPath
	if controlPath == "" {
		controlPath = defaultControlPath
	}
	return statusPath, controlPath
}

func validateAdminPath(path, field string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%s must start with '/'", field)
	}
	return nil
}

func envList(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
