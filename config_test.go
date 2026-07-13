package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tbxark/optional-go"
)

func TestLoad_LocalFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	configContent := FullConfig{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test-proxy",
			Version: "1.0.0",
			Type:    MCPServerTypeSSE,
			Options: &OptionsV2{
				AuthTokens: []string{"test-token"},
			},
		},
		McpServers: map[string]*MCPClientConfigV2{
			"test-server": {
				Command: "echo",
				Args:    []string{"hello"},
			},
		},
	}

	configBytes, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	err = os.WriteFile(configPath, configBytes, 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	conf, err := load(configPath, false, false, "", 10)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if conf.McpProxy == nil {
		t.Fatal("McpProxy should not be nil")
	}
	if conf.McpProxy.BaseURL != "http://localhost:9090" {
		t.Errorf("BaseURL = %v, want http://localhost:9090", conf.McpProxy.BaseURL)
	}
	if conf.McpProxy.Addr != ":9090" {
		t.Errorf("Addr = %v, want :9090", conf.McpProxy.Addr)
	}
	if conf.McpProxy.Type != MCPServerTypeSSE {
		t.Errorf("Type = %v, want %v", conf.McpProxy.Type, MCPServerTypeSSE)
	}
}

func TestLoad_LocalFileWithEnvExpansion(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	os.Setenv("TEST_BASE_URL", "http://test:8080")
	defer os.Unsetenv("TEST_BASE_URL")

	configContent := `{
		"mcpProxy": {
			"baseURL": "${TEST_BASE_URL}",
			"addr": ":9090",
			"name": "test-proxy",
			"version": "1.0.0"
		}
	}`

	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	conf, err := load(configPath, false, true, "", 10)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if conf.McpProxy.BaseURL != "http://test:8080" {
		t.Errorf("BaseURL = %v, want http://test:8080", conf.McpProxy.BaseURL)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := load("/nonexistent/path/config.json", false, false, "", 10)
	if err == nil {
		t.Error("load() expected error for missing file, got nil")
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	err := os.WriteFile(configPath, []byte(`{invalid json}`), 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	_, err = load(configPath, false, false, "", 10)
	if err == nil {
		t.Error("load() expected error for invalid JSON, got nil")
	}
}

func TestLoad_MissingMcpProxy(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	configContent := FullConfig{
		McpServers: map[string]*MCPClientConfigV2{
			"test": {Command: "echo"},
		},
	}

	configBytes, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	err = os.WriteFile(configPath, configBytes, 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	_, err = load(configPath, false, false, "", 10)
	if err == nil {
		t.Error("load() expected error for missing mcpProxy, got nil")
	}
}

func TestLoad_DefaultOptions(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	configContent := FullConfig{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test-proxy",
			Version: "1.0.0",
			Options: &OptionsV2{
				AuthTokens:     []string{"proxy-token"},
				PanicIfInvalid: optional.NewField(true),
				LogEnabled:     optional.NewField(true),
			},
		},
		McpServers: map[string]*MCPClientConfigV2{
			"server1": {
				Command: "echo",
			},
			"server2": {
				URL: "http://localhost:8080/sse",
				Options: &OptionsV2{
					AuthTokens: []string{"server-token"},
				},
			},
		},
	}

	configBytes, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	err = os.WriteFile(configPath, configBytes, 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	conf, err := load(configPath, false, false, "", 10)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	server1 := conf.McpServers["server1"]
	if server1.Options == nil {
		t.Fatal("server1 Options should not be nil")
	}
	if len(server1.Options.AuthTokens) != 1 || server1.Options.AuthTokens[0] != "proxy-token" {
		t.Errorf("server1 AuthTokens = %v, want [proxy-token]", server1.Options.AuthTokens)
	}
	if !server1.Options.PanicIfInvalid.OrElse(false) {
		t.Error("server1 PanicIfInvalid should be true (inherited from proxy)")
	}

	server2 := conf.McpServers["server2"]
	if len(server2.Options.AuthTokens) != 1 || server2.Options.AuthTokens[0] != "server-token" {
		t.Errorf("server2 AuthTokens = %v, want [server-token]", server2.Options.AuthTokens)
	}
}

func TestLoad_DefaultServerType(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	configContent := FullConfig{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test-proxy",
			Version: "1.0.0",
		},
	}

	configBytes, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	err = os.WriteFile(configPath, configBytes, 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	conf, err := load(configPath, false, false, "", 10)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if conf.McpProxy.Type != MCPServerTypeSSE {
		t.Errorf("Type = %v, want %v (default)", conf.McpProxy.Type, MCPServerTypeSSE)
	}
}

func TestLoad_V1Migration(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	stdioConfig, _ := json.Marshal(StdioMCPClientConfig{
		Command: "npx",
		Args:    []string{"test"},
	})

	configContent := FullConfig{
		DeprecatedServerV1: &MCPProxyConfigV1{
			BaseURL:          "http://localhost:9090",
			Addr:             ":9090",
			Name:             "test-proxy",
			Version:          "1.0.0",
			GlobalAuthTokens: []string{"global-token"},
		},
		DeprecatedClientsV1: map[string]*MCPClientConfigV1{
			"stdio-client": {
				Type:       MCPClientTypeStdio,
				Config:     stdioConfig,
				AuthTokens: []string{"client-token"},
			},
		},
	}

	configBytes, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	err = os.WriteFile(configPath, configBytes, 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	conf, err := load(configPath, false, false, "", 10)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if conf.McpProxy == nil {
		t.Fatal("McpProxy should not be nil after V1 migration")
	}
	if conf.McpProxy.BaseURL != "http://localhost:9090" {
		t.Errorf("BaseURL = %v, want http://localhost:9090", conf.McpProxy.BaseURL)
	}
	if _, ok := conf.McpServers["stdio-client"]; !ok {
		t.Error("stdio-client should exist in McpServers after migration")
	}
}

func TestNewConfProvider_UnsupportedPath(t *testing.T) {
	_, err := newConfProvider("invalid://path", false, false, "", 10)
	if err == nil {
		t.Error("newConfProvider() expected error for unsupported path, got nil")
	}
}

func TestNewConfProvider_LocalFileNoExpandEnv(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	os.Setenv("TEST_VAR", "expanded")
	defer os.Unsetenv("TEST_VAR")

	configContent := `{"mcpProxy": {"baseURL": "${TEST_VAR}", "addr": ":9090", "name": "test", "version": "1.0"}}`
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	pro, err := newConfProvider(configPath, false, false, "", 10)
	if err != nil {
		t.Fatalf("newConfProvider() error = %v", err)
	}
	if pro == nil {
		t.Fatal("provider should not be nil")
	}
}

func TestNewConfProvider_LocalFileWithExpandEnv(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	os.Setenv("TEST_VAR", "expanded-value")
	defer os.Unsetenv("TEST_VAR")

	configContent := `{"mcpProxy": {"baseURL": "${TEST_VAR}", "addr": ":9090", "name": "test", "version": "1.0"}}`
	err := os.WriteFile(configPath, []byte(configContent), 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	pro, err := newConfProvider(configPath, false, true, "", 10)
	if err != nil {
		t.Fatalf("newConfProvider() error = %v", err)
	}
	if pro == nil {
		t.Fatal("provider should not be nil")
	}
}

func TestLoad_HTTPTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	configContent := FullConfig{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test-proxy",
			Version: "1.0.0",
		},
	}

	configBytes, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	err = os.WriteFile(configPath, configBytes, 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	conf, err := load(configPath, false, false, "", 30)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if conf.McpProxy == nil {
		t.Fatal("McpProxy should not be nil")
	}
}

func TestLoad_ZeroTimeout(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	configContent := FullConfig{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test-proxy",
			Version: "1.0.0",
		},
	}

	configBytes, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	err = os.WriteFile(configPath, configBytes, 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	conf, err := load(configPath, false, false, "", 0)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if conf.McpProxy == nil {
		t.Fatal("McpProxy should not be nil")
	}
}

func TestNewConfProvider_HTTPURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		config := FullConfig{
			McpProxy: &MCPProxyConfigV2{
				BaseURL: "http://localhost:9090",
				Addr:    ":9090",
				Name:    "test-proxy",
				Version: "1.0.0",
			},
		}
		configBytes, _ := json.Marshal(config)
		w.Header().Set("Content-Type", "application/json")
		w.Write(configBytes)
	}))
	defer server.Close()

	pro, err := newConfProvider(server.URL, false, false, "", 10)
	if err != nil {
		t.Fatalf("newConfProvider() error = %v", err)
	}
	if pro == nil {
		t.Fatal("provider should not be nil")
	}
}

func TestNewConfProvider_HTTPURLWithInsecure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		config := FullConfig{
			McpProxy: &MCPProxyConfigV2{
				BaseURL: "http://localhost:9090",
				Addr:    ":9090",
				Name:    "test-proxy",
				Version: "1.0.0",
			},
		}
		configBytes, _ := json.Marshal(config)
		w.Header().Set("Content-Type", "application/json")
		w.Write(configBytes)
	}))
	defer server.Close()

	pro, err := newConfProvider(server.URL, true, false, "", 10)
	if err != nil {
		t.Fatalf("newConfProvider() error = %v", err)
	}
	if pro == nil {
		t.Fatal("provider should not be nil")
	}
}

func TestNewConfProvider_HTTPURLWithHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		config := FullConfig{
			McpProxy: &MCPProxyConfigV2{
				BaseURL: "http://localhost:9090",
				Addr:    ":9090",
				Name:    "test-proxy",
				Version: "1.0.0",
			},
		}
		configBytes, _ := json.Marshal(config)
		w.Header().Set("Content-Type", "application/json")
		w.Write(configBytes)
	}))
	defer server.Close()

	pro, err := newConfProvider(server.URL, false, false, "X-Custom:value;Authorization:Bearer token", 10)
	if err != nil {
		t.Fatalf("newConfProvider() error = %v", err)
	}
	if pro == nil {
		t.Fatal("provider should not be nil")
	}
}

func TestNewConfProvider_HTTPURLWithExpandEnv(t *testing.T) {
	os.Setenv("TEST_EXPAND_URL", "expanded")
	defer os.Unsetenv("TEST_EXPAND_URL")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		config := FullConfig{
			McpProxy: &MCPProxyConfigV2{
				BaseURL: "${TEST_EXPAND_URL}",
				Addr:    ":9090",
				Name:    "test-proxy",
				Version: "1.0.0",
			},
		}
		configBytes, _ := json.Marshal(config)
		w.Header().Set("Content-Type", "application/json")
		w.Write(configBytes)
	}))
	defer server.Close()

	pro, err := newConfProvider(server.URL, false, true, "", 10)
	if err != nil {
		t.Fatalf("newConfProvider() error = %v", err)
	}
	if pro == nil {
		t.Fatal("provider should not be nil")
	}
}

func TestLoad_HTTPURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		config := FullConfig{
			McpProxy: &MCPProxyConfigV2{
				BaseURL: "http://remote:9090",
				Addr:    ":9090",
				Name:    "remote-proxy",
				Version: "1.0.0",
			},
			McpServers: map[string]*MCPClientConfigV2{
				"remote-server": {
					URL: "http://remote:8080/sse",
				},
			},
		}
		configBytes, _ := json.Marshal(config)
		w.Header().Set("Content-Type", "application/json")
		w.Write(configBytes)
	}))
	defer server.Close()

	conf, err := load(server.URL, false, false, "", 10)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if conf.McpProxy.BaseURL != "http://remote:9090" {
		t.Errorf("BaseURL = %v, want http://remote:9090", conf.McpProxy.BaseURL)
	}
	if len(conf.McpServers) != 1 {
		t.Errorf("McpServers length = %d, want 1", len(conf.McpServers))
	}
}

func TestLoad_StreamableHTTPServer(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	configContent := FullConfig{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test-proxy",
			Version: "1.0.0",
			Type:    MCPServerTypeStreamable,
		},
	}

	configBytes, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	err = os.WriteFile(configPath, configBytes, 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	conf, err := load(configPath, false, false, "", 10)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if conf.McpProxy.Type != MCPServerTypeStreamable {
		t.Errorf("Type = %v, want %v", conf.McpProxy.Type, MCPServerTypeStreamable)
	}
}

func TestLoad_DisabledClient(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")

	configContent := FullConfig{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test-proxy",
			Version: "1.0.0",
		},
		McpServers: map[string]*MCPClientConfigV2{
			"disabled-server": {
				Command: "echo",
				Options: &OptionsV2{
					Disabled: true,
				},
			},
		},
	}

	configBytes, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal config: %v", err)
	}

	err = os.WriteFile(configPath, configBytes, 0644)
	if err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	conf, err := load(configPath, false, false, "", 10)
	if err != nil {
		t.Fatalf("load() error = %v", err)
	}

	if _, ok := conf.McpServers["disabled-server"]; !ok {
		t.Error("disabled-server should still exist in config")
	}
}

func TestParseMCPClientConfigV2_Stdio(t *testing.T) {
	tests := []struct {
		name    string
		conf    *MCPClientConfigV2
		want    *StdioMCPClientConfig
		wantErr bool
	}{
		{
			name: "stdio with command",
			conf: &MCPClientConfigV2{
				TransportType: MCPClientTypeStdio,
				Command:       "npx",
				Args:          []string{"-y", "@anthropic/mcp-server"},
				Env:           map[string]string{"NODE_ENV": "test"},
			},
			want: &StdioMCPClientConfig{
				Command: "npx",
				Args:    []string{"-y", "@anthropic/mcp-server"},
				Env:     map[string]string{"NODE_ENV": "test"},
			},
			wantErr: false,
		},
		{
			name: "stdio inferred from command",
			conf: &MCPClientConfigV2{
				Command: "uvx",
				Args:    []string{"mcp-server-git"},
			},
			want: &StdioMCPClientConfig{
				Command: "uvx",
				Args:    []string{"mcp-server-git"},
			},
			wantErr: false,
		},
		{
			name: "stdio missing command",
			conf: &MCPClientConfigV2{
				TransportType: MCPClientTypeStdio,
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "stdio with empty command",
			conf: &MCPClientConfigV2{
				TransportType: MCPClientTypeStdio,
				Command:       "",
			},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMCPClientConfigV2(tt.conf)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMCPClientConfigV2() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			stdioGot, ok := got.(*StdioMCPClientConfig)
			if !ok {
				t.Errorf("parseMCPClientConfigV2() got = %T, want *StdioMCPClientConfig", got)
				return
			}
			if stdioGot.Command != tt.want.Command {
				t.Errorf("Command = %v, want %v", stdioGot.Command, tt.want.Command)
			}
			if len(stdioGot.Args) != len(tt.want.Args) {
				t.Errorf("Args length = %v, want %v", len(stdioGot.Args), len(tt.want.Args))
			}
		})
	}
}

func TestParseMCPClientConfigV2_SSE(t *testing.T) {
	tests := []struct {
		name    string
		conf    *MCPClientConfigV2
		want    *SSEMCPClientConfig
		wantErr bool
	}{
		{
			name: "sse with url",
			conf: &MCPClientConfigV2{
				TransportType: MCPClientTypeSSE,
				URL:           "http://localhost:8080/sse",
				Headers:       map[string]string{"Authorization": "Bearer token"},
			},
			want: &SSEMCPClientConfig{
				URL:     "http://localhost:8080/sse",
				Headers: map[string]string{"Authorization": "Bearer token"},
			},
			wantErr: false,
		},
		{
			name: "sse default when no transport type",
			conf: &MCPClientConfigV2{
				URL: "http://localhost:8080/sse",
			},
			want: &SSEMCPClientConfig{
				URL:     "http://localhost:8080/sse",
				Headers: nil,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMCPClientConfigV2(tt.conf)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMCPClientConfigV2() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			sseGot, ok := got.(*SSEMCPClientConfig)
			if !ok {
				t.Errorf("parseMCPClientConfigV2() got = %T, want *SSEMCPClientConfig", got)
				return
			}
			if sseGot.URL != tt.want.URL {
				t.Errorf("URL = %v, want %v", sseGot.URL, tt.want.URL)
			}
		})
	}
}

func TestParseMCPClientConfigV2_StreamableHTTP(t *testing.T) {
	tests := []struct {
		name    string
		conf    *MCPClientConfigV2
		want    *StreamableMCPClientConfig
		wantErr bool
	}{
		{
			name: "streamable-http with url and timeout",
			conf: &MCPClientConfigV2{
				TransportType: MCPClientTypeStreamable,
				URL:           "http://localhost:8080/mcp",
				Headers:       map[string]string{"X-Custom": "value"},
				Timeout:       30 * time.Second,
			},
			want: &StreamableMCPClientConfig{
				URL:     "http://localhost:8080/mcp",
				Headers: map[string]string{"X-Custom": "value"},
				Timeout: 30 * time.Second,
			},
			wantErr: false,
		},
		{
			name: "streamable-http without timeout",
			conf: &MCPClientConfigV2{
				TransportType: MCPClientTypeStreamable,
				URL:           "http://localhost:8080/mcp",
			},
			want: &StreamableMCPClientConfig{
				URL:     "http://localhost:8080/mcp",
				Headers: nil,
				Timeout: 0,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMCPClientConfigV2(tt.conf)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMCPClientConfigV2() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			streamGot, ok := got.(*StreamableMCPClientConfig)
			if !ok {
				t.Errorf("parseMCPClientConfigV2() got = %T, want *StreamableMCPClientConfig", got)
				return
			}
			if streamGot.URL != tt.want.URL {
				t.Errorf("URL = %v, want %v", streamGot.URL, tt.want.URL)
			}
			if streamGot.Timeout != tt.want.Timeout {
				t.Errorf("Timeout = %v, want %v", streamGot.Timeout, tt.want.Timeout)
			}
		})
	}
}

func TestParseMCPClientConfigV2_Invalid(t *testing.T) {
	tests := []struct {
		name string
		conf *MCPClientConfigV2
	}{
		{
			name: "empty config",
			conf: &MCPClientConfigV2{},
		},
		{
			name: "only transport type without required fields",
			conf: &MCPClientConfigV2{
				TransportType: MCPClientTypeSSE,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseMCPClientConfigV2(tt.conf)
			if err == nil {
				t.Error("parseMCPClientConfigV2() expected error, got nil")
			}
		})
	}
}

func TestParseMCPClientConfigV1_Stdio(t *testing.T) {
	configBytes, _ := json.Marshal(StdioMCPClientConfig{
		Command: "npx",
		Args:    []string{"-y", "test"},
		Env:     map[string]string{"KEY": "VALUE"},
	})

	conf := &MCPClientConfigV1{
		Type:   MCPClientTypeStdio,
		Config: configBytes,
	}

	got, err := parseMCPClientConfigV1(conf)
	if err != nil {
		t.Fatalf("parseMCPClientConfigV1() error = %v", err)
	}

	stdioGot, ok := got.(*StdioMCPClientConfig)
	if !ok {
		t.Fatalf("got = %T, want *StdioMCPClientConfig", got)
	}

	if stdioGot.Command != "npx" {
		t.Errorf("Command = %v, want npx", stdioGot.Command)
	}
}

func TestParseMCPClientConfigV1_SSE(t *testing.T) {
	configBytes, _ := json.Marshal(SSEMCPClientConfig{
		URL:     "http://localhost:8080/sse",
		Headers: map[string]string{"Auth": "token"},
	})

	conf := &MCPClientConfigV1{
		Type:   MCPClientTypeSSE,
		Config: configBytes,
	}

	got, err := parseMCPClientConfigV1(conf)
	if err != nil {
		t.Fatalf("parseMCPClientConfigV1() error = %v", err)
	}

	sseGot, ok := got.(*SSEMCPClientConfig)
	if !ok {
		t.Fatalf("got = %T, want *SSEMCPClientConfig", got)
	}

	if sseGot.URL != "http://localhost:8080/sse" {
		t.Errorf("URL = %v, want http://localhost:8080/sse", sseGot.URL)
	}
}

func TestParseMCPClientConfigV1_StreamableHTTP(t *testing.T) {
	configBytes, _ := json.Marshal(StreamableMCPClientConfig{
		URL:     "http://localhost:8080/mcp",
		Headers: map[string]string{"Auth": "token"},
		Timeout: 30 * time.Second,
	})

	conf := &MCPClientConfigV1{
		Type:   MCPClientTypeStreamable,
		Config: configBytes,
	}

	got, err := parseMCPClientConfigV1(conf)
	if err != nil {
		t.Fatalf("parseMCPClientConfigV1() error = %v", err)
	}

	streamGot, ok := got.(*StreamableMCPClientConfig)
	if !ok {
		t.Fatalf("got = %T, want *StreamableMCPClientConfig", got)
	}

	if streamGot.URL != "http://localhost:8080/mcp" {
		t.Errorf("URL = %v, want http://localhost:8080/mcp", streamGot.URL)
	}
}

func TestParseMCPClientConfigV1_Invalid(t *testing.T) {
	tests := []struct {
		name string
		conf *MCPClientConfigV1
	}{
		{
			name: "invalid type",
			conf: &MCPClientConfigV1{
				Type:   "invalid",
				Config: []byte(`{}`),
			},
		},
		{
			name: "invalid json",
			conf: &MCPClientConfigV1{
				Type:   MCPClientTypeStdio,
				Config: []byte(`{invalid json`),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseMCPClientConfigV1(tt.conf)
			if err == nil {
				t.Error("parseMCPClientConfigV1() expected error, got nil")
			}
		})
	}
}

func TestAdaptMCPClientConfigV1ToV2(t *testing.T) {
	t.Run("migrate server config", func(t *testing.T) {
		conf := &FullConfig{
			DeprecatedServerV1: &MCPProxyConfigV1{
				BaseURL:          "http://localhost",
				Addr:             ":9090",
				Name:             "test-server",
				Version:          "1.0.0",
				GlobalAuthTokens: []string{"token1", "token2"},
			},
		}

		adaptMCPClientConfigV1ToV2(conf)

		if conf.McpProxy == nil {
			t.Fatal("McpProxy should not be nil")
		}
		if conf.McpProxy.BaseURL != "http://localhost" {
			t.Errorf("BaseURL = %v, want http://localhost", conf.McpProxy.BaseURL)
		}
		if conf.McpProxy.Addr != ":9090" {
			t.Errorf("Addr = %v, want :9090", conf.McpProxy.Addr)
		}
		if conf.DeprecatedServerV1 != nil {
			t.Error("DeprecatedServerV1 should be nil after migration")
		}
	})

	t.Run("migrate client configs", func(t *testing.T) {
		stdioConfig, _ := json.Marshal(StdioMCPClientConfig{
			Command: "npx",
			Args:    []string{"test"},
		})

		conf := &FullConfig{
			DeprecatedServerV1: &MCPProxyConfigV1{
				BaseURL:          "http://localhost",
				Addr:             ":9090",
				Name:             "test-server",
				Version:          "1.0.0",
				GlobalAuthTokens: []string{"global-token"},
			},
			DeprecatedClientsV1: map[string]*MCPClientConfigV1{
				"stdio-client": {
					Type:       MCPClientTypeStdio,
					Config:     stdioConfig,
					AuthTokens: []string{"client-token"},
				},
			},
		}

		adaptMCPClientConfigV1ToV2(conf)

		if len(conf.McpServers) == 0 {
			t.Fatal("McpServers should not be empty")
		}
		if _, ok := conf.McpServers["stdio-client"]; !ok {
			t.Error("stdio-client should exist in McpServers")
		}
		if conf.DeprecatedClientsV1 != nil {
			t.Error("DeprecatedClientsV1 should be nil after migration")
		}
	})

	t.Run("skip if v2 already present", func(t *testing.T) {
		conf := &FullConfig{
			DeprecatedServerV1: &MCPProxyConfigV1{
				BaseURL: "http://old",
				Addr:    ":8080",
			},
			McpProxy: &MCPProxyConfigV2{
				BaseURL: "http://new",
				Addr:    ":9090",
			},
		}

		adaptMCPClientConfigV1ToV2(conf)

		if conf.McpProxy.BaseURL != "http://new" {
			t.Errorf("BaseURL should remain http://new, got %v", conf.McpProxy.BaseURL)
		}
	})

	t.Run("migrate sse client", func(t *testing.T) {
		sseConfig, _ := json.Marshal(SSEMCPClientConfig{
			URL:     "http://localhost:8080/sse",
			Headers: map[string]string{"Auth": "token"},
		})

		conf := &FullConfig{
			DeprecatedServerV1: &MCPProxyConfigV1{
				BaseURL: "http://localhost",
				Addr:    ":9090",
				Name:    "test-server",
				Version: "1.0.0",
			},
			DeprecatedClientsV1: map[string]*MCPClientConfigV1{
				"sse-client": {
					Type:   MCPClientTypeSSE,
					Config: sseConfig,
				},
			},
		}

		adaptMCPClientConfigV1ToV2(conf)

		if len(conf.McpServers) == 0 {
			t.Fatal("McpServers should not be empty")
		}
		if _, ok := conf.McpServers["sse-client"]; !ok {
			t.Error("sse-client should exist in McpServers")
		}
	})

	t.Run("migrate streamable http client", func(t *testing.T) {
		streamableConfig, _ := json.Marshal(StreamableMCPClientConfig{
			URL:     "http://localhost:8080/mcp",
			Headers: map[string]string{"Auth": "token"},
			Timeout: 30 * time.Second,
		})

		conf := &FullConfig{
			DeprecatedServerV1: &MCPProxyConfigV1{
				BaseURL: "http://localhost",
				Addr:    ":9090",
				Name:    "test-server",
				Version: "1.0.0",
			},
			DeprecatedClientsV1: map[string]*MCPClientConfigV1{
				"streamable-client": {
					Type:   MCPClientTypeStreamable,
					Config: streamableConfig,
				},
			},
		}

		adaptMCPClientConfigV1ToV2(conf)

		if len(conf.McpServers) == 0 {
			t.Fatal("McpServers should not be empty")
		}
		if _, ok := conf.McpServers["streamable-client"]; !ok {
			t.Error("streamable-client should exist in McpServers")
		}
	})

	t.Run("skip invalid client type", func(t *testing.T) {
		conf := &FullConfig{
			DeprecatedServerV1: &MCPProxyConfigV1{
				BaseURL: "http://localhost",
				Addr:    ":9090",
				Name:    "test-server",
				Version: "1.0.0",
			},
			DeprecatedClientsV1: map[string]*MCPClientConfigV1{
				"invalid-client": {
					Type:   "invalid-type",
					Config: []byte(`{}`),
				},
			},
		}

		adaptMCPClientConfigV1ToV2(conf)

		if len(conf.McpServers) > 0 {
			t.Error("McpServers should be empty for invalid client type")
		}
	})

	t.Run("merge global auth tokens with client tokens", func(t *testing.T) {
		stdioConfig, _ := json.Marshal(StdioMCPClientConfig{
			Command: "npx",
		})

		conf := &FullConfig{
			DeprecatedServerV1: &MCPProxyConfigV1{
				BaseURL:          "http://localhost",
				Addr:             ":9090",
				Name:             "test-server",
				Version:          "1.0.0",
				GlobalAuthTokens: []string{"global1", "global2"},
			},
			DeprecatedClientsV1: map[string]*MCPClientConfigV1{
				"client-with-tokens": {
					Type:       MCPClientTypeStdio,
					Config:     stdioConfig,
					AuthTokens: []string{"client1"},
				},
			},
		}

		adaptMCPClientConfigV1ToV2(conf)

		client := conf.McpServers["client-with-tokens"]
		if client == nil {
			t.Fatal("client should not be nil")
		}
		if len(client.Options.AuthTokens) != 3 {
			t.Errorf("AuthTokens length = %d, want 3", len(client.Options.AuthTokens))
		}
	})
}

func TestToolFilterConfig(t *testing.T) {
	t.Run("allow mode", func(t *testing.T) {
		filter := &ToolFilterConfig{
			Mode: ToolFilterModeAllow,
			List: []string{"tool1", "tool2"},
		}

		if filter.Mode != ToolFilterModeAllow {
			t.Errorf("Mode = %v, want %v", filter.Mode, ToolFilterModeAllow)
		}
	})

	t.Run("block mode", func(t *testing.T) {
		filter := &ToolFilterConfig{
			Mode: ToolFilterModeBlock,
			List: []string{"tool3"},
		}

		if filter.Mode != ToolFilterModeBlock {
			t.Errorf("Mode = %v, want %v", filter.Mode, ToolFilterModeBlock)
		}
	})
}

func TestOptionsV2(t *testing.T) {
	t.Run("with all fields", func(t *testing.T) {
		opts := &OptionsV2{
			PanicIfInvalid: optional.NewField(true),
			LogEnabled:     optional.NewField(true),
			AuthTokens:     []string{"token1"},
			ToolFilter: &ToolFilterConfig{
				Mode: ToolFilterModeAllow,
				List: []string{"tool1"},
			},
			Disabled: true,
		}

		if !opts.PanicIfInvalid.OrElse(false) {
			t.Error("PanicIfInvalid should be true")
		}
		if !opts.LogEnabled.OrElse(false) {
			t.Error("LogEnabled should be true")
		}
		if len(opts.AuthTokens) != 1 {
			t.Errorf("AuthTokens length = %v, want 1", len(opts.AuthTokens))
		}
		if !opts.Disabled {
			t.Error("Disabled should be true")
		}
	})

	t.Run("empty options", func(t *testing.T) {
		opts := &OptionsV2{}

		if opts.PanicIfInvalid.Present() {
			t.Error("PanicIfInvalid should not be present")
		}
		if opts.LogEnabled.Present() {
			t.Error("LogEnabled should not be present")
		}
	})
}

func TestMCPProxyConfigV2(t *testing.T) {
	conf := &MCPProxyConfigV2{
		BaseURL: "http://localhost:9090",
		Addr:    ":9090",
		Name:    "test-proxy",
		Version: "1.0.0",
		Type:    MCPServerTypeSSE,
		Options: &OptionsV2{
			AuthTokens: []string{"token"},
		},
	}

	if conf.BaseURL != "http://localhost:9090" {
		t.Errorf("BaseURL = %v", conf.BaseURL)
	}
	if conf.Type != MCPServerTypeSSE {
		t.Errorf("Type = %v, want %v", conf.Type, MCPServerTypeSSE)
	}
}

func TestMCPServerType_Constants(t *testing.T) {
	if MCPServerTypeSSE != "sse" {
		t.Errorf("MCPServerTypeSSE = %v, want sse", MCPServerTypeSSE)
	}
	if MCPServerTypeStreamable != "streamable-http" {
		t.Errorf("MCPServerTypeStreamable = %v, want streamable-http", MCPServerTypeStreamable)
	}
}

func TestMCPClientType_Constants(t *testing.T) {
	if MCPClientTypeStdio != "stdio" {
		t.Errorf("MCPClientTypeStdio = %v, want stdio", MCPClientTypeStdio)
	}
	if MCPClientTypeSSE != "sse" {
		t.Errorf("MCPClientTypeSSE = %v, want sse", MCPClientTypeSSE)
	}
	if MCPClientTypeStreamable != "streamable-http" {
		t.Errorf("MCPClientTypeStreamable = %v, want streamable-http", MCPClientTypeStreamable)
	}
}
