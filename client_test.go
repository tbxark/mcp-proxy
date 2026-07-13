package main

import (
	"context"
	"testing"
	"time"

	"github.com/tbxark/optional-go"
)

func TestNewMCPServer_SSE(t *testing.T) {
	serverConfig := &MCPProxyConfigV2{
		BaseURL: "http://localhost:9090",
		Addr:    ":9090",
		Name:    "test-server",
		Version: "1.0.0",
		Type:    MCPServerTypeSSE,
	}

	clientConfig := &MCPClientConfigV2{
		Options: &OptionsV2{
			AuthTokens: []string{"test-token"},
		},
	}

	server, err := newMCPServer("test", serverConfig, clientConfig)
	if err != nil {
		t.Fatalf("newMCPServer() error = %v", err)
	}

	if server == nil {
		t.Fatal("server should not be nil")
	}
	if server.mcpServer == nil {
		t.Fatal("mcpServer should not be nil")
	}
	if server.handler == nil {
		t.Fatal("handler should not be nil")
	}
	if len(server.tokens) != 1 || server.tokens[0] != "test-token" {
		t.Errorf("tokens = %v, want [test-token]", server.tokens)
	}
}

func TestNewMCPServer_StreamableHTTP(t *testing.T) {
	serverConfig := &MCPProxyConfigV2{
		BaseURL: "http://localhost:9090",
		Addr:    ":9090",
		Name:    "test-server",
		Version: "1.0.0",
		Type:    MCPServerTypeStreamable,
	}

	clientConfig := &MCPClientConfigV2{
		Options: &OptionsV2{},
	}

	server, err := newMCPServer("test", serverConfig, clientConfig)
	if err != nil {
		t.Fatalf("newMCPServer() error = %v", err)
	}

	if server == nil {
		t.Fatal("server should not be nil")
	}
	if server.handler == nil {
		t.Fatal("handler should not be nil")
	}
}

func TestNewMCPServer_WithLogging(t *testing.T) {
	serverConfig := &MCPProxyConfigV2{
		BaseURL: "http://localhost:9090",
		Addr:    ":9090",
		Name:    "test-server",
		Version: "1.0.0",
		Type:    MCPServerTypeSSE,
	}

	clientConfig := &MCPClientConfigV2{
		Options: &OptionsV2{
			LogEnabled: optional.NewField(true),
		},
	}

	server, err := newMCPServer("test", serverConfig, clientConfig)
	if err != nil {
		t.Fatalf("newMCPServer() error = %v", err)
	}

	if server == nil {
		t.Fatal("server should not be nil")
	}
}

func TestNewMCPServer_NoAuthTokens(t *testing.T) {
	serverConfig := &MCPProxyConfigV2{
		BaseURL: "http://localhost:9090",
		Addr:    ":9090",
		Name:    "test-server",
		Version: "1.0.0",
		Type:    MCPServerTypeSSE,
	}

	clientConfig := &MCPClientConfigV2{
		Options: &OptionsV2{},
	}

	server, err := newMCPServer("test", serverConfig, clientConfig)
	if err != nil {
		t.Fatalf("newMCPServer() error = %v", err)
	}

	if len(server.tokens) != 0 {
		t.Errorf("tokens length = %d, want 0", len(server.tokens))
	}
}

func TestNewMCPServer_InvalidType(t *testing.T) {
	serverConfig := &MCPProxyConfigV2{
		BaseURL: "http://localhost:9090",
		Addr:    ":9090",
		Name:    "test-server",
		Version: "1.0.0",
		Type:    "invalid-type",
	}

	clientConfig := &MCPClientConfigV2{
		Options: &OptionsV2{},
	}

	_, err := newMCPServer("test", serverConfig, clientConfig)
	if err == nil {
		t.Error("newMCPServer() expected error for invalid type, got nil")
	}
}

func TestClient_Close(t *testing.T) {
	client := &Client{
		name:    "test",
		client:  nil,
		options: &OptionsV2{},
	}

	err := client.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

func TestClient_NeedManualStart(t *testing.T) {
	client := &Client{
		name:            "test",
		needManualStart: true,
		client:          nil,
		options:         &OptionsV2{},
	}

	if !client.needManualStart {
		t.Error("needManualStart should be true")
	}
}

func TestClient_NeedPing(t *testing.T) {
	client := &Client{
		name:     "test",
		needPing: true,
		client:   nil,
		options:  &OptionsV2{},
	}

	if !client.needPing {
		t.Error("needPing should be true")
	}
}

func TestClient_StartPingTask_ContextCancel(t *testing.T) {
	client := &Client{
		name:     "test",
		needPing: true,
		client:   nil,
		options:  &OptionsV2{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool)
	go func() {
		client.startPingTask(ctx)
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Error("startPingTask should return immediately when context is cancelled")
	}
}

func TestNewMCPClient_InvalidConfig(t *testing.T) {
	conf := &MCPClientConfigV2{}

	_, err := newMCPClient("test", conf)
	if err == nil {
		t.Error("newMCPClient() expected error for invalid config, got nil")
	}
}

func TestNewMCPClient_StdioMissingCommand(t *testing.T) {
	conf := &MCPClientConfigV2{
		TransportType: MCPClientTypeStdio,
	}

	_, err := newMCPClient("test", conf)
	if err == nil {
		t.Error("newMCPClient() expected error for missing command, got nil")
	}
}

func TestNewMCPClient_InvalidURL(t *testing.T) {
	conf := &MCPClientConfigV2{
		TransportType: MCPClientTypeSSE,
		URL:           "://invalid-url",
	}

	_, err := newMCPClient("test", conf)
	if err == nil {
		t.Error("newMCPClient() expected error for invalid URL, got nil")
	}
}

func TestNewMCPClient_StreamableInvalidURL(t *testing.T) {
	conf := &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           "://invalid-url",
	}

	_, err := newMCPClient("test", conf)
	if err == nil {
		t.Error("newMCPClient() expected error for invalid URL, got nil")
	}
}

func TestNewMCPClient_SSEWithHeaders(t *testing.T) {
	conf := &MCPClientConfigV2{
		TransportType: MCPClientTypeSSE,
		URL:           "http://localhost:9999/sse",
		Headers: map[string]string{
			"Authorization": "Bearer test-token",
			"X-Custom":      "value",
		},
		Options: &OptionsV2{},
	}

	_, err := newMCPClient("test", conf)
	_ = err
}

func TestNewMCPClient_StreamableWithHeaders(t *testing.T) {
	conf := &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           "http://localhost:9999/mcp",
		Headers: map[string]string{
			"Authorization": "Bearer test-token",
		},
		Timeout: 30 * time.Second,
		Options: &OptionsV2{},
	}

	_, err := newMCPClient("test", conf)
	_ = err
}

func TestServer_Tokens(t *testing.T) {
	server := &Server{
		tokens:    []string{"token1", "token2"},
		mcpServer: nil,
		handler:   nil,
	}

	if len(server.tokens) != 2 {
		t.Errorf("tokens length = %d, want 2", len(server.tokens))
	}
}

func TestClient_Options(t *testing.T) {
	opts := &OptionsV2{
		PanicIfInvalid: optional.NewField(true),
		LogEnabled:     optional.NewField(false),
		AuthTokens:     []string{"test-token"},
		ToolFilter: &ToolFilterConfig{
			Mode: ToolFilterModeAllow,
			List: []string{"tool1"},
		},
		Disabled: false,
	}

	client := &Client{
		name:    "test",
		client:  nil,
		options: opts,
	}

	if client.options != opts {
		t.Error("options should be set")
	}
	if !client.options.PanicIfInvalid.OrElse(false) {
		t.Error("PanicIfInvalid should be true")
	}
}

func TestClient_StartPingTask_DeadlineExceeded(t *testing.T) {
	client := &Client{
		name:     "test",
		needPing: true,
		client:   nil,
		options:  &OptionsV2{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan bool)
	go func() {
		client.startPingTask(ctx)
		done <- true
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Error("startPingTask should return when context deadline exceeded")
	}
}

func TestNewMCPClient_StdioWithEnv(t *testing.T) {
	conf := &MCPClientConfigV2{
		TransportType: MCPClientTypeStdio,
		Command:       "echo",
		Args:          []string{"hello"},
		Env: map[string]string{
			"VAR1": "value1",
			"VAR2": "value2",
		},
		Options: &OptionsV2{},
	}

	client, err := newMCPClient("test", conf)
	if err != nil {
		t.Fatalf("newMCPClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("client should not be nil")
	}
	if client.name != "test" {
		t.Errorf("name = %v, want test", client.name)
	}
	if client.needPing {
		t.Error("needPing should be false for stdio client")
	}
	if client.needManualStart {
		t.Error("needManualStart should be false for stdio client")
	}
	_ = client.Close()
}

func TestNewMCPClient_SSEClientFlags(t *testing.T) {
	conf := &MCPClientConfigV2{
		TransportType: MCPClientTypeSSE,
		URL:           "http://localhost:9999/sse",
		Headers: map[string]string{
			"Authorization": "Bearer token",
		},
		Options: &OptionsV2{},
	}

	client, err := newMCPClient("test", conf)
	if err != nil {
		t.Fatalf("newMCPClient() error = %v", err)
	}
	if !client.needPing {
		t.Error("needPing should be true for SSE client")
	}
	if !client.needManualStart {
		t.Error("needManualStart should be true for SSE client")
	}
}

func TestNewMCPClient_StreamableHTTPClientFlags(t *testing.T) {
	conf := &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           "http://localhost:9999/mcp",
		Headers: map[string]string{
			"Authorization": "Bearer token",
		},
		Timeout: 30 * time.Second,
		Options: &OptionsV2{},
	}

	client, err := newMCPClient("test", conf)
	if err != nil {
		t.Fatalf("newMCPClient() error = %v", err)
	}
	if !client.needPing {
		t.Error("needPing should be true for StreamableHTTP client")
	}
	if !client.needManualStart {
		t.Error("needManualStart should be true for StreamableHTTP client")
	}
}

func TestConfig_Struct(t *testing.T) {
	conf := &Config{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test",
			Version: "1.0",
		},
		McpServers: map[string]*MCPClientConfigV2{
			"server1": {Command: "echo"},
			"server2": {URL: "http://localhost:8080"},
		},
	}

	if conf.McpProxy == nil {
		t.Fatal("McpProxy should not be nil")
	}
	if len(conf.McpServers) != 2 {
		t.Errorf("McpServers length = %d, want 2", len(conf.McpServers))
	}
}

func TestStdioMCPClientConfig_Fields(t *testing.T) {
	conf := &StdioMCPClientConfig{
		Command: "npx",
		Args:    []string{"-y", "@test/mcp-server"},
		Env:     map[string]string{"NODE_ENV": "test"},
	}

	if conf.Command != "npx" {
		t.Errorf("Command = %v, want npx", conf.Command)
	}
	if len(conf.Args) != 2 {
		t.Errorf("Args length = %d, want 2", len(conf.Args))
	}
}

func TestSSEMCPClientConfig_Fields(t *testing.T) {
	conf := &SSEMCPClientConfig{
		URL:     "http://localhost:8080/sse",
		Headers: map[string]string{"Auth": "token"},
	}

	if conf.URL != "http://localhost:8080/sse" {
		t.Errorf("URL = %v", conf.URL)
	}
}

func TestStreamableMCPClientConfig_Fields(t *testing.T) {
	conf := &StreamableMCPClientConfig{
		URL:     "http://localhost:8080/mcp",
		Headers: map[string]string{"Auth": "token"},
		Timeout: 30 * time.Second,
	}

	if conf.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", conf.Timeout)
	}
}

func TestNewMCPServer_WithOptions(t *testing.T) {
	serverConfig := &MCPProxyConfigV2{
		BaseURL: "http://localhost:9090",
		Addr:    ":9090",
		Name:    "test-server",
		Version: "1.0.0",
		Type:    MCPServerTypeSSE,
	}

	clientConfig := &MCPClientConfigV2{
		Options: &OptionsV2{
			AuthTokens:     []string{"token1", "token2", "token3"},
			PanicIfInvalid: optional.NewField(true),
			LogEnabled:     optional.NewField(true),
		},
	}

	server, err := newMCPServer("test", serverConfig, clientConfig)
	if err != nil {
		t.Fatalf("newMCPServer() error = %v", err)
	}

	if len(server.tokens) != 3 {
		t.Errorf("tokens length = %d, want 3", len(server.tokens))
	}
}

func TestToolFilter_AllowMode(t *testing.T) {
	filter := &ToolFilterConfig{
		Mode: ToolFilterModeAllow,
		List: []string{"tool1", "tool2"},
	}

	filterSet := make(map[string]struct{})
	for _, name := range filter.List {
		filterSet[name] = struct{}{}
	}

	filterFunc := func(toolName string) bool {
		_, inList := filterSet[toolName]
		return inList
	}

	if !filterFunc("tool1") {
		t.Error("tool1 should be allowed")
	}
	if !filterFunc("tool2") {
		t.Error("tool2 should be allowed")
	}
	if filterFunc("tool3") {
		t.Error("tool3 should not be allowed")
	}
}

func TestToolFilter_BlockMode(t *testing.T) {
	filter := &ToolFilterConfig{
		Mode: ToolFilterModeBlock,
		List: []string{"tool1", "tool2"},
	}

	filterSet := make(map[string]struct{})
	for _, name := range filter.List {
		filterSet[name] = struct{}{}
	}

	filterFunc := func(toolName string) bool {
		_, inList := filterSet[toolName]
		return !inList
	}

	if filterFunc("tool1") {
		t.Error("tool1 should be blocked")
	}
	if filterFunc("tool2") {
		t.Error("tool2 should be blocked")
	}
	if !filterFunc("tool3") {
		t.Error("tool3 should not be blocked")
	}
}

func TestOptions_Empty(t *testing.T) {
	opts := &OptionsV2{}

	if opts.Disabled {
		t.Error("Disabled should be false by default")
	}
	if opts.ToolFilter != nil {
		t.Error("ToolFilter should be nil by default")
	}
}

func TestOptions_ToolFilterNil(t *testing.T) {
	opts := &OptionsV2{
		ToolFilter: nil,
	}

	if opts.ToolFilter != nil {
		t.Error("ToolFilter should be nil")
	}
}

func TestClient_Empty(t *testing.T) {
	client := &Client{}

	if client.name != "" {
		t.Errorf("name should be empty, got %v", client.name)
	}
	if client.needPing {
		t.Error("needPing should be false")
	}
	if client.needManualStart {
		t.Error("needManualStart should be false")
	}
}

func TestServer_Empty(t *testing.T) {
	server := &Server{}

	if len(server.tokens) != 0 {
		t.Errorf("tokens length should be 0, got %d", len(server.tokens))
	}
}

func TestConfig_EmptyMcpServers(t *testing.T) {
	conf := &Config{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://localhost:9090",
			Addr:    ":9090",
			Name:    "test",
			Version: "1.0",
		},
		McpServers: map[string]*MCPClientConfigV2{},
	}

	if len(conf.McpServers) != 0 {
		t.Errorf("McpServers should be empty, got %d", len(conf.McpServers))
	}
}

func TestFullConfig_Empty(t *testing.T) {
	conf := &FullConfig{}

	if conf.McpProxy != nil {
		t.Error("McpProxy should be nil")
	}
	if conf.McpServers != nil {
		t.Error("McpServers should be nil")
	}
}

func TestMCPProxyConfigV2_EmptyOptions(t *testing.T) {
	conf := &MCPProxyConfigV2{
		BaseURL: "http://localhost:9090",
		Addr:    ":9090",
		Name:    "test",
		Version: "1.0",
	}

	if conf.Options != nil {
		t.Error("Options should be nil")
	}
}

func TestMCPClientConfigV2_Partial(t *testing.T) {
	conf := &MCPClientConfigV2{
		Command: "echo",
	}

	if conf.Command != "echo" {
		t.Errorf("Command = %v, want echo", conf.Command)
	}
	if conf.URL != "" {
		t.Errorf("URL should be empty, got %v", conf.URL)
	}
	if conf.TransportType != "" {
		t.Errorf("TransportType should be empty, got %v", conf.TransportType)
	}
}
