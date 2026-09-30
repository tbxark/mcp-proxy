package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestForwardedProtocolMetadata(t *testing.T) {
	t.Parallel()

	for _, downstreamVersion := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(downstreamVersion, func(t *testing.T) {
			t.Parallel()
			type capturedRequest struct {
				Method string
				Meta   map[string]any
				Header string
				Args   json.RawMessage
			}
			calls := make(chan capturedRequest, 12)
			downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params struct {
						Meta map[string]any  `json:"_meta"`
						Args json.RawMessage `json:"arguments"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if request.ID == nil {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
				var result any
				switch request.Method {
				case "server/discover":
					if downstreamVersion == "2025-11-25" {
						response["error"] = map[string]any{"code": -32601, "message": "legacy server"}
					} else {
						result = map[string]any{
							"supportedVersions": []string{downstreamVersion},
							"capabilities":      map[string]any{},
						}
					}
				case "initialize":
					result = map[string]any{
						"protocolVersion": downstreamVersion,
						"capabilities":    map[string]any{},
						"serverInfo":      map[string]any{"name": "strict-downstream", "version": "1"},
					}
				case "tools/list":
					result = map[string]any{"tools": []any{map[string]any{
						"name": "echo", "inputSchema": map[string]any{"type": "object"},
					}}}
				case "prompts/list":
					result = map[string]any{"prompts": []any{map[string]any{"name": "echo"}}}
				case "resources/list":
					result = map[string]any{"resources": []any{map[string]any{"name": "echo", "uri": "test://echo"}}}
				case "resources/templates/list":
					result = map[string]any{"resourceTemplates": []any{map[string]any{
						"name": "template", "uriTemplate": "test://template/{name}",
					}}}
				case "tools/call", "prompts/get", "resources/read":
					calls <- capturedRequest{request.Method, request.Params.Meta, r.Header.Get("Mcp-Protocol-Version"), request.Params.Args}
					version, _ := request.Params.Meta[mcp.MetaKeyProtocolVersion].(string)
					if version != "" && version != downstreamVersion {
						response["error"] = map[string]any{"code": -32602, "message": "negotiated protocol version mismatch"}
						break
					}
					switch request.Method {
					case "tools/call":
						result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
					case "prompts/get":
						result = map[string]any{"messages": []any{}}
					case "resources/read":
						result = map[string]any{"contents": []any{map[string]any{"uri": "test://echo", "text": "ok"}}}
					}
				default:
					response["error"] = map[string]any{"code": -32601, "message": "unknown method"}
				}
				if result != nil {
					response["result"] = result
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer downstream.Close()

			proxy, err := newMCPClient("test", &MCPClientConfigV2{
				TransportType: MCPClientTypeStreamable, URL: downstream.URL, Options: &OptionsV2{},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = proxy.Close() }()
			front := newProxyServerForTest(t)
			if err := proxy.addToMCPServer(t.Context(), mcp.Implementation{Name: "gateway", Version: "1"}, front); err != nil {
				t.Fatal(err)
			}
			if got := proxy.getClient().ProtocolVersion(); got != downstreamVersion {
				t.Fatalf("downstream negotiated %q, want %q", got, downstreamVersion)
			}
			transport := proxy.getClient()
			for _, upstreamVersion := range []string{"2026-07-28", "2025-11-25", "2026-07-28"} {
				for _, operation := range []struct {
					method string
					params map[string]any
				}{
					{"tools/call", map[string]any{"name": "echo", "arguments": json.RawMessage(`{"id":9007199254740993}`)}},
					{"prompts/get", map[string]any{"name": "echo"}},
					{"resources/read", map[string]any{"uri": "test://echo"}},
					{"resources/read", map[string]any{"uri": "test://template/example"}},
				} {
					meta := map[string]any{"progressToken": "keep-progress", "example/custom": "keep-custom"}
					if upstreamVersion == "2026-07-28" {
						meta[mcp.MetaKeyProtocolVersion] = upstreamVersion
						meta[mcp.MetaKeyClientInfo] = map[string]any{"name": "upstream", "version": "1"}
						meta[mcp.MetaKeyClientCapabilities] = map[string]any{"sampling": map[string]any{}}
					}
					operation.params["_meta"] = meta
					payload, err := json.Marshal(map[string]any{
						"jsonrpc": "2.0", "id": 1, "method": operation.method, "params": operation.params,
					})
					if err != nil {
						t.Fatal(err)
					}
					response := front.HandleMessage(t.Context(), payload)
					encoded, err := json.Marshal(response)
					if err != nil {
						t.Fatal(err)
					}
					var decoded struct {
						Error *mcp.JSONRPCErrorDetails `json:"error"`
					}
					if err := json.Unmarshal(encoded, &decoded); err != nil {
						t.Fatal(err)
					}
					if decoded.Error != nil {
						t.Fatalf("%s -> %s %s failed: %s", upstreamVersion, downstreamVersion, operation.method, encoded)
					}
					var got capturedRequest
					select {
					case got = <-calls:
					default:
						t.Fatalf("%s was not forwarded", operation.method)
					}
					if got.Header != downstreamVersion {
						t.Errorf("outbound header = %q, want %q", got.Header, downstreamVersion)
					}
					if got.Meta["progressToken"] != "keep-progress" || got.Meta["example/custom"] != "keep-custom" {
						t.Errorf("application metadata lost: %v", got.Meta)
					}
					if downstreamVersion == "2025-11-25" {
						for _, key := range []string{mcp.MetaKeyProtocolVersion, mcp.MetaKeyClientInfo, mcp.MetaKeyClientCapabilities} {
							if _, ok := got.Meta[key]; ok {
								t.Errorf("legacy downstream received hop-specific field %q", key)
							}
						}
					} else {
						if got.Meta[mcp.MetaKeyProtocolVersion] != downstreamVersion {
							t.Errorf("modern protocol metadata = %v", got.Meta)
						}
						info, ok := got.Meta[mcp.MetaKeyClientInfo].(map[string]any)
						if !ok || info["name"] != "gateway" {
							t.Errorf("modern downstream must see gateway identity: %v", got.Meta)
						}
						caps, ok := got.Meta[mcp.MetaKeyClientCapabilities].(map[string]any)
						if !ok || caps["sampling"] != nil {
							t.Errorf("upstream capabilities leaked: %v", got.Meta)
						}
					}
					if operation.method == "tools/call" && string(got.Args) != `{"id":9007199254740993}` {
						t.Errorf("tool argument precision changed: %s", got.Args)
					}
				}
			}
			if proxy.getClient() != transport {
				t.Error("mixed-version clients replaced the shared backend")
			}
		})
	}
}

func TestDownstreamRequestMetaDoesNotMutateCaller(t *testing.T) {
	t.Parallel()
	input := &mcp.Meta{ProgressToken: "p", AdditionalFields: map[string]any{
		mcp.MetaKeyProtocolVersion: "2026-07-28",
		mcp.MetaKeyClientInfo:      "upstream", mcp.MetaKeyClientCapabilities: "upstream",
		"traceparent": "keep", "custom": "keep",
	}}
	want := &mcp.Meta{ProgressToken: "p", AdditionalFields: map[string]any{"traceparent": "keep", "custom": "keep"}}
	before, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	got := downstreamRequestMeta(input)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	after, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("caller metadata was mutated")
	}
	if downstreamRequestMeta(nil) != nil {
		t.Fatal("nil metadata should remain nil")
	}
}
