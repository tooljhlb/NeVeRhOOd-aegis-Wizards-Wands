package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type ModelProfile struct {
	Name         string `json:"name"`
	Provider     string `json:"provider"` // demo | openai-compatible
	BaseURL      string `json:"base_url,omitempty"`
	Model        string `json:"model"`
	APIKeyEnv    string `json:"api_key_env,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
}
type MCPProfile struct {
	Name            string   `json:"name"`
	URL             string   `json:"url"`
	TokenEnv        string   `json:"token_env,omitempty"`
	ProtocolVersion string   `json:"protocol_version,omitempty"`
	SafeTools       []string `json:"safe_tools,omitempty"`
}
type DataProfile struct {
	BaseURL   string `json:"base_url,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
}
type Config struct {
	Listen         string         `json:"listen"`
	DataDir        string         `json:"data_dir"`
	DefaultModel   string         `json:"default_model"`
	MaxSteps       int            `json:"max_steps"`
	Models         []ModelProfile `json:"models"`
	MCPServers     []MCPProfile   `json:"mcp_servers"`
	DataFrameworks DataProfile    `json:"data_frameworks"`
}

func demoConfig() Config {
	return Config{Listen: "127.0.0.1:8787", DataDir: "./.ww-data", DefaultModel: "demo", MaxSteps: 6, Models: []ModelProfile{{Name: "demo", Provider: "demo", Model: "demo"}}}
}
func loadConfig(path string) (Config, error) {
	c := demoConfig()
	if path != "" {
		b, e := os.ReadFile(path)
		if e != nil {
			return c, e
		}
		if e = json.Unmarshal(b, &c); e != nil {
			return c, e
		}
	}
	if v := os.Getenv("WW_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WW_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if c.Listen == "" {
		return c, errors.New("listen address is required")
	}
	host, _, e := net.SplitHostPort(c.Listen)
	if e != nil {
		return c, e
	}
	if !isLoopbackHost(host) && os.Getenv("WW_ALLOW_REMOTE_BIND") != "1" {
		return c, errors.New("non-loopback bind requires WW_ALLOW_REMOTE_BIND=1 and a reverse proxy with authentication")
	}
	if c.DataDir == "" {
		c.DataDir = "./.ww-data"
	}
	if c.MaxSteps <= 0 || c.MaxSteps > 16 {
		return c, errors.New("max_steps must be 1..16")
	}
	if len(c.Models) == 0 {
		return c, errors.New("at least one model is required")
	}
	seen := map[string]bool{}
	for _, m := range c.Models {
		if m.Name == "" || seen[m.Name] {
			return c, fmt.Errorf("invalid or duplicate model: %q", m.Name)
		}
		seen[m.Name] = true
		if m.Provider != "demo" && m.Provider != "openai-compatible" {
			return c, fmt.Errorf("unsupported provider for %q", m.Name)
		}
		if m.Provider != "demo" {
			if m.Model == "" {
				return c, fmt.Errorf("model ID required: %s", m.Name)
			}
			if e = validateConfiguredEndpoint(m.BaseURL); e != nil {
				return c, fmt.Errorf("model %s: %w", m.Name, e)
			}
		}
	}
	if !seen[c.DefaultModel] {
		return c, fmt.Errorf("default model %s not configured", c.DefaultModel)
	}
	seen = map[string]bool{}
	for _, m := range c.MCPServers {
		if m.Name == "" || seen[m.Name] || !isToolIdentifier(m.Name) {
			return c, fmt.Errorf("invalid or duplicate MCP server %q", m.Name)
		}
		seen[m.Name] = true
		if e = validateConfiguredEndpoint(m.URL); e != nil {
			return c, fmt.Errorf("MCP %s: %w", m.Name, e)
		}
	}
	if c.DataFrameworks.BaseURL != "" {
		if e = validateConfiguredEndpoint(c.DataFrameworks.BaseURL); e != nil {
			return c, e
		}
	}
	if c.DataDir, e = filepath.Abs(c.DataDir); e != nil {
		return c, e
	}
	return c, nil
}
func isLoopbackHost(host string) bool {
	return host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}
func validateConfiguredEndpoint(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid configured endpoint URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
		return nil
	}
	return errors.New("only HTTPS or loopback HTTP endpoints are accepted")
}
func isToolIdentifier(v string) bool {
	if v == "" || len(v) > 80 {
		return false
	}
	for _, ch := range v {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-", ch) {
			return false
		}
	}
	return true
}
func (c Config) model(name string) (ModelProfile, bool) {
	for _, m := range c.Models {
		if m.Name == name {
			return m, true
		}
	}
	return ModelProfile{}, false
}
