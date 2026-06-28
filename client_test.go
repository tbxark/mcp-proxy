package main

import (
	"context"
	"encoding/json"
	"testing"
)

// Regression for https://github.com/tbxark/mcp-proxy/issues/66 — JetBrains MCP
// clients reject null resource lists; empty collections must serialize as [].
func TestMCPServerListResourcesReturnsEmptyArrayNotNull(t *testing.T) {
	t.Parallel()

	server, err := newMCPServer("test", &MCPProxyConfigV2{
		Type:    MCPServerTypeStreamable,
		Version: "test",
		BaseURL: "http://localhost:9090",
	}, &MCPClientConfigV2{
		Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPServer: %v", err)
	}

	t.Run("resources/list", func(t *testing.T) {
		resp := server.mcpServer.HandleMessage(context.Background(), []byte(`{
			"jsonrpc": "2.0",
			"id": 1,
			"method": "resources/list",
			"params": {}
		}`))

		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}

		var decoded struct {
			Result struct {
				Resources json.RawMessage `json:"resources"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal response: %v\npayload: %s", err, string(raw))
		}
		if string(decoded.Result.Resources) != "[]" {
			t.Fatalf("expected resources to be [], got %s", string(decoded.Result.Resources))
		}
	})

	t.Run("resources/templates/list", func(t *testing.T) {
		resp := server.mcpServer.HandleMessage(context.Background(), []byte(`{
			"jsonrpc": "2.0",
			"id": 2,
			"method": "resources/templates/list",
			"params": {}
		}`))

		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}

		var decoded struct {
			Result struct {
				ResourceTemplates json.RawMessage `json:"resourceTemplates"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal response: %v\npayload: %s", err, string(raw))
		}
		if string(decoded.Result.ResourceTemplates) != "[]" {
			t.Fatalf("expected resourceTemplates to be [], got %s", string(decoded.Result.ResourceTemplates))
		}
	})
}
