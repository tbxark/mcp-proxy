package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validTestConfig() *Config {
	return &Config{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test-proxy",
			Version: "test",
			Type:    MCPServerTypeStreamable,
			Options: &OptionsV2{},
		},
		McpServers: map[string]*MCPClientConfigV2{
			"stdio": {
				Command: "test-server",
				Options: &OptionsV2{},
			},
		},
	}
}

func TestParseMCPClientConfigV2(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  *MCPClientConfigV2
		wantErr string
		want    any
	}{
		{name: "infer stdio", config: &MCPClientConfigV2{Command: "server"}, want: &StdioMCPClientConfig{}},
		{name: "infer sse", config: &MCPClientConfigV2{URL: "https://example.com/sse"}, want: &SSEMCPClientConfig{}},
		{name: "explicit streamable", config: &MCPClientConfigV2{TransportType: MCPClientTypeStreamable, URL: "https://example.com/mcp"}, want: &StreamableMCPClientConfig{}},
		{name: "null", config: nil, wantErr: "server config is null"},
		{name: "ambiguous", config: &MCPClientConfigV2{Command: "server", URL: "https://example.com"}, wantErr: "mutually exclusive"},
		{name: "missing endpoint", config: &MCPClientConfigV2{}, wantErr: "command or url is required"},
		{name: "unknown transport", config: &MCPClientConfigV2{TransportType: "websocket", URL: "https://example.com"}, wantErr: "unsupported transportType"},
		{name: "stdio missing command", config: &MCPClientConfigV2{TransportType: MCPClientTypeStdio}, wantErr: "command is required"},
		{name: "oauth on stdio", config: &MCPClientConfigV2{Command: "server", OAuth: &OAuthClientConfig{}}, wantErr: "oauth is not supported"},
		{name: "negative timeout", config: &MCPClientConfigV2{TransportType: MCPClientTypeStreamable, URL: "https://example.com", Timeout: -1}, wantErr: "timeout cannot be negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMCPClientConfigV2(tt.config)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			switch tt.want.(type) {
			case *StdioMCPClientConfig:
				if _, ok := got.(*StdioMCPClientConfig); !ok {
					t.Fatalf("type = %T, want *StdioMCPClientConfig", got)
				}
			case *SSEMCPClientConfig:
				if _, ok := got.(*SSEMCPClientConfig); !ok {
					t.Fatalf("type = %T, want *SSEMCPClientConfig", got)
				}
			case *StreamableMCPClientConfig:
				if _, ok := got.(*StreamableMCPClientConfig); !ok {
					t.Fatalf("type = %T, want *StreamableMCPClientConfig", got)
				}
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		if err := validateConfig(validTestConfig()); err != nil {
			t.Fatalf("validate config: %v", err)
		}
	})

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "invalid base URL", mutate: func(c *Config) { c.McpProxy.BaseURL = "localhost:9090" }, wantErr: "absolute http(s) URL"},
		{name: "unknown proxy type", mutate: func(c *Config) { c.McpProxy.Type = "websocket" }, wantErr: "mcpProxy.type"},
		{name: "empty token", mutate: func(c *Config) { c.McpServers["stdio"].Options.AuthTokens = []string{""} }, wantErr: "authTokens[0]"},
		{name: "unknown filter mode", mutate: func(c *Config) {
			c.McpServers["stdio"].Options.ToolFilter = &ToolFilterConfig{Mode: "permit", List: []string{"tool"}}
		}, wantErr: "toolFilter.mode"},
		{name: "invalid remote URL", mutate: func(c *Config) {
			c.McpServers["stdio"] = &MCPClientConfigV2{URL: "://bad", Options: &OptionsV2{}}
		}, wantErr: "is invalid"},
		{name: "non-loopback OAuth redirect", mutate: func(c *Config) {
			c.McpServers["stdio"] = &MCPClientConfigV2{
				URL:     "https://example.com/mcp",
				OAuth:   &OAuthClientConfig{RedirectURI: "http://0.0.0.0:8090/oauth/callback"},
				Options: &OptionsV2{},
			}
		}, wantErr: "loopback"},
		{name: "secret without client ID", mutate: func(c *Config) {
			c.McpServers["stdio"] = &MCPClientConfigV2{
				URL:     "https://example.com/mcp",
				OAuth:   &OAuthClientConfig{ClientSecret: "secret"},
				Options: &OptionsV2{},
			}
		}, wantErr: "clientSecret requires clientId"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validTestConfig()
			tt.mutate(config)
			err := validateConfig(config)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsInvalidServerConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
		"mcpProxy": {
			"baseURL": "http://localhost:9090",
			"addr": ":9090",
			"name": "test",
			"version": "test",
			"type": "streamable-http"
		},
		"mcpServers": {
			"broken": {"transportType": "streamable-http"}
		}
	}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := load(path, false, false, "", 10)
	if err == nil || !strings.Contains(err.Error(), `mcpServers["broken"]`) {
		t.Fatalf("load error = %v, want broken server context", err)
	}
}
