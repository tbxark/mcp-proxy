package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestMCPHTTPClientRequestsUncompressedResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(acceptEncodingHeader); got != "identity" {
			http.Error(w, "expected Accept-Encoding: identity, got "+got, http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("X-Test"); got != "value" {
			http.Error(w, "expected X-Test header, got "+got, http.StatusBadRequest)
			return
		}

		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request.Method != string(mcp.MethodInitialize) {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result": map[string]any{
				"protocolVersion": mcp.LATEST_PROTOCOL_VERSION,
				"capabilities":    map[string]any{},
				"serverInfo": map[string]string{
					"name":    "test",
					"version": "test",
				},
			},
		})
	}))
	defer server.Close()

	configuredHeaders := map[string]string{
		"accept-encoding": "gzip",
		"X-Test":          "value",
	}
	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           server.URL,
		Headers:       configuredHeaders,
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer mcpClient.Close()

	ctx := t.Context()
	if err := mcpClient.client.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	request := mcp.InitializeRequest{}
	request.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "test"}
	if _, err := mcpClient.client.Initialize(ctx, request); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	if got := configuredHeaders["accept-encoding"]; got != "gzip" {
		t.Fatalf("configured headers were mutated: got %q", got)
	}
}

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
