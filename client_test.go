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

	assertEmptyJSONArray := func(t *testing.T, raw []byte, field, want string) {
		t.Helper()

		var decoded struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Result map[string]json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal response: %v\npayload: %s", err, string(raw))
		}
		if decoded.Error != nil {
			t.Fatalf("unexpected JSON-RPC error (code %d): %s\nfull response: %s",
				decoded.Error.Code, decoded.Error.Message, string(raw))
		}

		got, ok := decoded.Result[field]
		if !ok {
			t.Fatalf("response missing result.%s\nfull response: %s", field, string(raw))
		}
		if string(got) != want {
			t.Fatalf("expected result.%s to be %s, got %s\nfull response: %s",
				field, want, string(got), string(raw))
		}
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
		assertEmptyJSONArray(t, raw, "resources", "[]")
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
		assertEmptyJSONArray(t, raw, "resourceTemplates", "[]")
	})
}
