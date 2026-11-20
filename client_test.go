package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/tbxark/optional-go"
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
	defer func() { _ = mcpClient.Close() }()

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

// A downstream that answers "method not found" is alive and simply does not
// implement ping; only a broken connection may mark a client unhealthy.
func TestIsTransportFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "transport closed", err: transport.NewError(errors.New("transport closed")), want: true},
		{name: "wrapped transport error", err: fmt.Errorf("ping: %w", transport.NewError(errors.New("eof"))), want: true},
		{name: "ping unsupported", err: mcp.ErrMethodNotFound, want: false},
		{name: "plain error", err: errors.New("boom"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransportFailure(tt.err); got != tt.want {
				t.Errorf("isTransportFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// rawDownstream is a hand-written streamable-http MCP endpoint, so a test can
// control exactly when tools/list answers (or does not). The real mcp-go server
// always answers, which is why the listing-hang case cannot use it.
type rawDownstream struct {
	url            string
	mu             sync.Mutex
	tools          []string
	hangToolsList  bool
	toolsListCalls int

	// Optional descriptor/metadata controls. Zero values preserve the original
	// empty-catalog behaviour.
	toolExecution     map[string]any    // attached to every listed tool when set
	toolsRaw          json.RawMessage   // verbatim tools array; overrides tools when set
	resourceTemplates []map[string]any  // raw templates; []any{} when nil
	prompts           []string          // prompt names; none when nil
	resources         []string          // resource URIs; none when nil
	resourceNames     map[string]string // resource URI -> name; the URI when unset
	hangMethods       map[string]bool   // methods that accept and never answer

	server *httptest.Server
}

func newRawDownstream(t *testing.T) *rawDownstream {
	t.Helper()

	d := &rawDownstream{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				URI string `json:"uri"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			// A notification (e.g. notifications/initialized): accept it.
			w.WriteHeader(http.StatusAccepted)
			return
		}

		if d.shouldHang(req.Method) {
			<-r.Context().Done()
			return
		}

		writeResult := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  result,
			})
		}

		switch req.Method {
		case "initialize":
			writeResult(map[string]any{
				"protocolVersion": mcp.LATEST_PROTOCOL_VERSION,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "downstream", "version": "1"},
			})
		case "tools/list":
			d.mu.Lock()
			d.toolsListCalls++
			hang := d.hangToolsList
			names := slices.Clone(d.tools)
			execution := d.toolExecution
			rawTools := slices.Clone(d.toolsRaw)
			d.mu.Unlock()
			if hang {
				<-r.Context().Done()
				return
			}
			// A test that cares about key order needs verbatim JSON: encoding a
			// map would sort the property names and erase the thing under test.
			if rawTools != nil {
				writeResult(map[string]any{"tools": rawTools})
				return
			}
			tools := make([]map[string]any, 0, len(names))
			for _, name := range names {
				tool := map[string]any{
					"name":        name,
					"inputSchema": map[string]any{"type": "object"},
				}
				if execution != nil {
					tool["execution"] = execution
				}
				tools = append(tools, tool)
			}
			writeResult(map[string]any{"tools": tools})
		case "prompts/list":
			d.mu.Lock()
			names := slices.Clone(d.prompts)
			d.mu.Unlock()
			prompts := make([]map[string]any, 0, len(names))
			for _, name := range names {
				prompts = append(prompts, map[string]any{"name": name})
			}
			writeResult(map[string]any{"prompts": prompts})
		case "resources/list":
			d.mu.Lock()
			uris := slices.Clone(d.resources)
			names := maps.Clone(d.resourceNames)
			d.mu.Unlock()
			resources := make([]map[string]any, 0, len(uris))
			for _, uri := range uris {
				name := uri
				if named, ok := names[uri]; ok {
					name = named
				}
				resources = append(resources, map[string]any{"uri": uri, "name": name})
			}
			writeResult(map[string]any{"resources": resources})
		case "resources/read":
			// Echo the URI back, so a test can tell a forwarded read from one the
			// proxy refused.
			writeResult(map[string]any{"contents": []map[string]any{{"uri": req.Params.URI, "text": "read " + req.Params.URI}}})
		case "resources/templates/list":
			d.mu.Lock()
			templates := d.resourceTemplates
			d.mu.Unlock()
			if templates == nil {
				templates = []map[string]any{}
			}
			writeResult(map[string]any{"resourceTemplates": templates})
		default:
			writeResult(map[string]any{})
		}
	})
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	d.url = s.URL
	d.server = s
	return d
}

func (d *rawDownstream) shouldHang(method string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hangMethods[method]
}

func (d *rawDownstream) setTools(names ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tools = slices.Clone(names)
}

// setRawTools replaces the tools/list payload with raw JSON, so a test can
// control key order (and anything else an encoded map would normalise).
func (d *rawDownstream) setRawTools(raw string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.toolsRaw = json.RawMessage(raw)
}

func (d *rawDownstream) setPrompts(names ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prompts = slices.Clone(names)
}

func (d *rawDownstream) setResources(uris ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resources = slices.Clone(uris)
}

func (d *rawDownstream) setResourceName(uri, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.resourceNames == nil {
		d.resourceNames = make(map[string]string)
	}
	d.resourceNames[uri] = name
}

func (d *rawDownstream) setToolExecution(execution map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.toolExecution = execution
}

func (d *rawDownstream) setResourceTemplates(templates ...map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resourceTemplates = slices.Clone(templates)
}

func (d *rawDownstream) hangMethodsNamed(methods ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hangMethods = make(map[string]bool, len(methods))
	for _, method := range methods {
		d.hangMethods[method] = true
	}
}

func (d *rawDownstream) hangToolsListCalls(hang bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hangToolsList = hang
}

// close shuts the fake downstream down, so a test can observe how a transport
// failure is reported (the URL in the error includes any configured query).
func (d *rawDownstream) close() {
	d.mu.Lock()
	server := d.server
	d.mu.Unlock()
	if server != nil {
		server.Close()
	}
}

// serverToolNames reads back the tools registered on the proxy-side server.
func serverToolNames(t *testing.T, mcpServer *server.MCPServer) []string {
	t.Helper()

	resp := mcpServer.HandleMessage(context.Background(), []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}
	}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal tools/list response: %v", err)
	}
	var decoded struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal tools/list response: %v", err)
	}
	names := make([]string, 0, len(decoded.Result.Tools))
	for _, tool := range decoded.Result.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// serverToolInputSchema reads back the raw inputSchema the proxy-side server
// serves for a tool. It stays raw because a decoded map would lose the property
// key order this is meant to inspect.
func serverToolInputSchema(t *testing.T, mcpServer *server.MCPServer, toolName string) string {
	t.Helper()

	resp := mcpServer.HandleMessage(context.Background(), []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}
	}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal tools/list response: %v", err)
	}
	var decoded struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal tools/list response: %v", err)
	}
	for _, tool := range decoded.Result.Tools {
		if tool.Name == toolName {
			return string(tool.InputSchema)
		}
	}
	t.Fatalf("tool %q not registered on proxy server", toolName)
	return ""
}

func newProxyServerForTest(t *testing.T) *server.MCPServer {
	t.Helper()

	proxyServer, err := newMCPServer("test", &MCPProxyConfigV2{
		Type:    MCPServerTypeStreamable,
		Version: "1",
		BaseURL: "http://localhost:9090",
	}, &MCPClientConfigV2{Options: &OptionsV2{}})
	if err != nil {
		t.Fatalf("newMCPServer: %v", err)
	}
	return proxyServer.mcpServer
}

// Regression for the reconnect path re-registering a shrunken catalog: AddTool
// is an upsert, so without replace semantics a tool the downstream dropped stays
// exposed. The registration must replace the whole set instead.
func TestCatalogRegistrationDropsStaleToolsOnReRegister(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha", "beta")

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	ctx := t.Context()
	if err := mcpClient.addToMCPServer(ctx, mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	if got := serverToolNames(t, proxyServer); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("initial tools = %v, want [alpha beta]", got)
	}

	// The downstream shrank; a reconnect re-runs registration. The dropped tool
	// must not survive.
	downstream.setTools("alpha")
	if err := mcpClient.addToolsToServer(ctx, proxyServer); err != nil {
		t.Fatalf("re-register tools: %v", err)
	}
	if got := serverToolNames(t, proxyServer); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("tools after re-registration = %v, want [alpha]", got)
	}
}

// The proxy re-serves the downstream's tool descriptors, so the inputSchema it
// publishes must survive the decode/re-encode round trip. mcp-go v1.1.0 began
// recording property key order on decode; before that the schema was rebuilt
// from a map and the downstream's order was replaced with sorted order.
func TestCatalogRegistrationPreservesInputSchemaPropertyOrder(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	// Deliberately not alphabetical: sorted order would be alpha, zeta.
	downstream.setRawTools(`[{"name":"ordered","inputSchema":{"type":"object","properties":{"zeta":{"type":"string"},"alpha":{"type":"number"}},"required":["zeta"]}}]`)

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	schema := serverToolInputSchema(t, proxyServer, "ordered")
	zeta := strings.Index(schema, `"zeta"`)
	alpha := strings.Index(schema, `"alpha"`)
	if zeta == -1 || alpha == -1 {
		t.Fatalf("inputSchema lost a property: %s", schema)
	}
	if zeta > alpha {
		t.Errorf("inputSchema property order was normalised: %s", schema)
	}
}

// Regression for the listing half of connect being unbounded: a downstream that
// completes initialize and then never answers tools/list would otherwise wedge
// the startup goroutine (and the retry loop) forever. catalogTimeout must turn
// that into a bounded error.
func TestCatalogTimeoutBoundsHungToolsList(t *testing.T) {
	// Not parallel: it shortens the package-level catalogTimeout.
	old := catalogTimeout
	catalogTimeout = 300 * time.Millisecond
	t.Cleanup(func() { catalogTimeout = old })

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.hangToolsListCalls(true)

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err = mcpClient.addToMCPServer(ctx, mcp.Implementation{Name: "test"}, proxyServer)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("addToMCPServer succeeded despite a tools/list that never answers")
	}
	if elapsed > 3*time.Second {
		t.Errorf("addToMCPServer took %v, want it bounded by catalogTimeout", elapsed)
	}
}

// Regression for a Client built without Options: newMCPClient permits a nil
// Options, and the keepalive task used to dereference it directly, panicking as
// soon as such a client connected. The option accessors are nil-safe, so a
// successful connection must stay up.
func TestNilOptionsClientDoesNotPanicOnConnect(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	ctx := t.Context()
	if err := mcpClient.addToMCPServer(ctx, mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer with nil Options: %v", err)
	}

	// Let the keepalive task run a tick; a nil dereference there crashes the
	// process rather than this goroutine.
	time.Sleep(100 * time.Millisecond)
}

// The proxy advertises only the capabilities it implements: no logging (it
// does not forward log levels) and no resources.subscribe (it never emits
// resources/updated nor forwards subscriptions).
func TestNewMCPServerAdvertisedCapabilities(t *testing.T) {
	t.Parallel()

	s := newProxyServerForTest(t)
	msg := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	raw, err := json.Marshal(s.HandleMessage(context.Background(), msg))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Result struct {
			Capabilities struct {
				Logging   *struct{} `json:"logging"`
				Resources *struct {
					Subscribe bool `json:"subscribe"`
				} `json:"resources"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	caps := out.Result.Capabilities
	if caps.Logging != nil {
		t.Error("logging capability must not be advertised: the proxy does not implement logging")
	}
	if caps.Resources != nil && caps.Resources.Subscribe {
		t.Error("resources.subscribe must not be advertised: the proxy does not forward subscriptions")
	}
}

// A downstream resource template without a uriTemplate used to reach
// mcp-go's SetResourceTemplates, which dereferences the nil URITemplate and
// panics the shared process. It must be skipped instead.
func TestResourceTemplateWithoutURITemplateIsSkipped(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.setResourceTemplates(
		map[string]any{"name": "missing", "description": "no uriTemplate"},
		map[string]any{"name": "explicit-null", "uriTemplate": nil},
		map[string]any{"name": "good", "uriTemplate": "test://echo/{word}"},
	)

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	got := serverResourceTemplateNames(t, proxyServer)
	if !slices.Equal(got, []string{"good"}) {
		t.Fatalf("registered resource templates = %v, want only [good]", got)
	}
}

// serverResourceTemplateNames reads back the resource templates registered on
// the proxy-side server.
func serverResourceTemplateNames(t *testing.T, mcpServer *server.MCPServer) []string {
	t.Helper()

	resp := mcpServer.HandleMessage(context.Background(), []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "resources/templates/list", "params": {}
	}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal templates/list response: %v", err)
	}
	var decoded struct {
		Result struct {
			ResourceTemplates []struct {
				Name string `json:"name"`
			} `json:"resourceTemplates"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal templates/list response: %v", err)
	}
	names := make([]string, 0, len(decoded.Result.ResourceTemplates))
	for _, template := range decoded.Result.ResourceTemplates {
		names = append(names, template.Name)
	}
	slices.Sort(names)
	return names
}

func TestToolFilterFunc(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options *OptionsV2
		tool    string
		want    bool
	}{
		{name: "nil options allows", options: nil, tool: "any", want: true},
		{name: "nil filter allows", options: &OptionsV2{}, tool: "any", want: true},
		{name: "allow list membership", options: &OptionsV2{ToolFilter: &FilterConfig{Mode: FilterModeAllow, List: []string{"a"}}}, tool: "a", want: true},
		{name: "allow list exclusion", options: &OptionsV2{ToolFilter: &FilterConfig{Mode: FilterModeAllow, List: []string{"a"}}}, tool: "b", want: false},
		{name: "empty block list allows", options: &OptionsV2{ToolFilter: &FilterConfig{Mode: FilterModeBlock}}, tool: "any", want: true},
		{name: "block list excludes", options: &OptionsV2{ToolFilter: &FilterConfig{Mode: FilterModeBlock, List: []string{"b"}}}, tool: "b", want: false},
		// Validation rejects unknown modes, so this is only reachable with a
		// programmatically built config; it fails closed.
		{name: "unknown mode hides all", options: &OptionsV2{ToolFilter: &FilterConfig{Mode: FilterMode("nonsense")}}, tool: "any", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolFilterFunc("test", tt.options)(tt.tool); got != tt.want {
				t.Errorf("toolFilterFunc(%s)(%s) = %v, want %v", tt.name, tt.tool, got, tt.want)
			}
		})
	}
}

func TestDownstreamExecutionMetadataIsStripped(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("danger")
	downstream.setToolExecution(map[string]any{"taskSupport": "required"})

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	resp := proxyServer.HandleMessage(context.Background(), []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}
	}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal tools/list response: %v", err)
	}
	if strings.Contains(string(raw), "taskSupport") || strings.Contains(string(raw), `"execution"`) {
		t.Fatalf("republished tool kept downstream execution metadata: %s", raw)
	}
	if !strings.Contains(string(raw), "danger") {
		t.Fatalf("the tool itself should still be advertised: %s", raw)
	}
}

// The configured per-request bound must apply to resource reads, not
// only tool calls. A downstream that accepts a read and never answers used to
// hold the shared channel for the caller's whole deadline.
func TestResourceReadHonorsRequestTimeout(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.setResources("test://hang")
	downstream.hangMethodsNamed("resources/read")

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	ctx := t.Context()
	if err := mcpClient.addToMCPServer(ctx, mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	// Bound is normally set only for stdio; the forwarding logic it drives is
	// shared, so exercising it on a streamable client keeps the test in-process.
	mcpClient.requestTimeout = 250 * time.Millisecond

	callerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err = mcpClient.readResource(callerCtx, mcp.ReadResourceRequest{
		Params: mcp.ReadResourceParams{URI: "test://hang"},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("readResource succeeded against a downstream that never answers")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("readResource took %v; it ignored the configured request timeout", elapsed)
	}
}

// The prompt path must be bounded the same way.
func TestPromptGetHonorsRequestTimeout(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.setPrompts("hang")
	downstream.hangMethodsNamed("prompts/get")

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	ctx := t.Context()
	if err := mcpClient.addToMCPServer(ctx, mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	mcpClient.requestTimeout = 250 * time.Millisecond

	callerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp := proxyServer.HandleMessage(callerCtx, []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "prompts/get",
		"params": {"name": "hang"}
	}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal prompts/get response: %v", err)
	}
	// The handler is bounded, so the call must have produced a JSON-RPC error
	// rather than blocking until the caller's 30s deadline.
	if !strings.Contains(string(raw), `"error"`) {
		t.Fatalf("prompts/get against a hanging downstream should error, got %s", raw)
	}
}

// A credential carried in a downstream URL query must not survive into
// an error message that reaches a caller or the log.
func TestRedactURLCredentials(t *testing.T) {
	t.Parallel()

	credentialed := `Post "https://mcp.example.com/mcp?key=SECRET&token=OTHER": dial tcp: refused`
	_ = credentialed
	err := &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp?key=SECRET&token=OTHER", Err: errors.New("dial tcp: refused")}

	redacted := redactURLCredentials(err)
	message := redacted.Error()
	for _, secret := range []string{"SECRET", "OTHER", "key=", "token="} {
		if strings.Contains(message, secret) {
			t.Errorf("redacted message still contains %q: %s", secret, message)
		}
	}
	if !strings.Contains(message, "mcp.example.com") {
		t.Errorf("redaction dropped too much, the host is not diagnostic: %s", message)
	}
	if !errors.Is(redacted, err) {
		t.Error("redaction must preserve the original error in the chain for errors.As/Is")
	}

	// A non-URL error is returned unchanged.
	plain := errors.New("boom")
	if got := redactURLCredentials(plain); got != plain {
		t.Errorf("plain error should pass through, got %v", got)
	}
	if got := redactURLCredentials(nil); got != nil {
		t.Errorf("nil error should stay nil, got %v", got)
	}
}

// End to end: the error a caller receives after a downstream transport
// failure must not contain the configured query credential.
func TestToolCallErrorRedactsURLCredential(t *testing.T) {
	t.Parallel()

	const secret = "SECRETTOKEN"
	downstream := newRawDownstream(t)
	downstream.setTools("alpha")

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		// The credential the docs recommend putting in the URL query.
		URL:     downstream.url + "?key=" + secret,
		Options: &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	ctx := t.Context()
	if err := mcpClient.addToMCPServer(ctx, mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	// Take the downstream away, then call the registered tool: the transport
	// failure embeds the credentialed URL in a *url.Error.
	downstream.close()

	resp := proxyServer.HandleMessage(ctx, []byte(`{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": {"name": "alpha", "arguments": {}}
	}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal tools/call response: %v", err)
	}
	message := string(raw)
	if strings.Contains(message, secret) {
		t.Fatalf("caller-visible error disclosed the URL query credential: %s", message)
	}
	if !strings.Contains(message, `"error"`) {
		t.Fatalf("a call to a dead downstream should return an error, got %s", message)
	}
}

// A URL that fails to parse must still be redacted. The first cut used
// url.Parse and silently returned the original text when parsing failed, which
// leaked the credential in exactly the malformed-URL case.
func TestRedactURLCredentialsHandlesUnparseableURL(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"http://ho st/mcp?key=PARSE_SECRET",
		"://?key=PARSE_SECRET",
		"http://user:pw@ho st/x?key=PARSE_SECRET#frag",
	} {
		urlErr := &url.Error{Op: "Post", URL: raw, Err: errors.New("boom")}
		got := redactURLCredentials(urlErr).Error()
		if strings.Contains(got, "PARSE_SECRET") {
			t.Errorf("unparseable URL %q leaked its query credential: %s", raw, got)
		}
		if strings.Contains(got, "pw@") {
			t.Errorf("unparseable URL %q leaked its userinfo: %s", raw, got)
		}
	}

	// A URL with no credential is left untouched, and the original error stays
	// in the chain so classification is preserved.
	clean := &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp", Err: errors.New("boom")}
	if got := redactURLCredentials(clean); got != error(clean) {
		t.Errorf("a credential-free URL should pass through unchanged, got %v", got)
	}
}

// The reconnect log and the -doctor live column are further sinks for a
// transport error, which embeds the credentialed downstream URL.
func TestURLRedactionCoversReconnectAndDoctorSinks(t *testing.T) {
	t.Parallel()

	urlErr := func() error {
		return &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp?key=SINK_SECRET", Err: errors.New("refused")}
	}

	// Reconnect path: the error that reaches the log must be redacted.
	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url + "?key=SINK_SECRET",
		Options:       &OptionsV2{},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, newProxyServerForTest(t)); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	downstream.close()
	reconnectErr := mcpClient.connect(t.Context(), mcp.Implementation{Name: "test"}, mcpClient.mcpServer)
	if reconnectErr == nil {
		t.Fatal("expected the reconnect to fail after the downstream closed")
	}
	if strings.Contains(redactURLCredentials(reconnectErr).Error(), "SINK_SECRET") {
		t.Error("the reconnect error still carries the URL query credential into the log")
	}

	// Doctor path: liveFailureMessage is printed to the report.
	if message := liveFailureMessage("test", urlErr()); strings.Contains(message, "SINK_SECRET") {
		t.Errorf("-doctor live column disclosed the URL query credential: %s", message)
	}
	if message := liveFailureMessage("test", urlErr()); !strings.Contains(message, "mcp.example.com") {
		t.Errorf("-doctor message lost the diagnostic host: %s", message)
	}
}

// Url.Error renders its URL with %q, so a credential containing a quote
// or backslash does not appear verbatim in the message. Replacing the raw URL
// silently did nothing and leaked it; redaction must rebuild the rendered form.
func TestRedactURLCredentialsHandlesQuotedSecret(t *testing.T) {
	t.Parallel()

	for _, secret := range []string{`SEC"RET`, `SEC\RET`, `SEC\"RET`, "SEC`RET"} {
		urlErr := &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp?key=" + secret, Err: errors.New("refused")}
		got := redactURLCredentials(urlErr).Error()
		if strings.Contains(got, secret) {
			t.Errorf("secret %q survived redaction: %s", secret, got)
		}
		// The failure-classification chain must survive too.
		var asURLErr *url.Error
		if !errors.As(redactURLCredentials(urlErr), &asURLErr) {
			t.Errorf("errors.As(*url.Error) failed after redaction of %q", secret)
		}
	}
}

// A transport error nested inside a wrapper (as the transports produce)
// must also be redacted, including a quoted credential.
func TestRedactURLCredentialsNestedWrapper(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("transport error: %w", &url.Error{Op: "Post", URL: `http://127.0.0.1:1/mcp?key=NEST"ED`, Err: errors.New("refused")})
	got := redactURLCredentials(wrapped).Error()
	if strings.Contains(got, `NEST"ED`) || strings.Contains(got, "key=") {
		t.Fatalf("nested transport error leaked the credential: %s", got)
	}
	if !strings.Contains(got, "transport error") || !strings.Contains(got, "127.0.0.1") {
		t.Fatalf("redaction lost the diagnostic context: %s", got)
	}
}

// The redactor itself must survive the truncated-userinfo shape that
// net/url produces for an unparseable URL.
func TestRedactURLStringTruncatedUserinfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw         string
		mustNotHave []string
	}{
		{"https://user:SUPERSECRET", []string{"SUPERSECRET"}},
		{"//user:NOSCHEMESECRET@ho st/mcp?key=Q", []string{"NOSCHEMESECRET", "Q"}},
		{"https://user:SEC#frag@example.com/mcp", []string{"SEC"}},
		{"scheme://user:pass@host:8080/path?k=v#f", []string{"pass", "k=v", "f"}},
	}
	for _, tt := range tests {
		got := redactURLString(tt.raw)
		for _, secret := range tt.mustNotHave {
			if strings.Contains(got, secret) {
				t.Errorf("redactURLString(%q) = %q still contains %q", tt.raw, got, secret)
			}
		}
	}

	// A credential-free URL is returned byte-identical, and the port survives
	// when there is a path, so diagnostics stay useful.
	for _, clean := range []string{"http://127.0.0.1:9/mcp", "https://mcp.example.com/sse"} {
		if got := redactURLString(clean); got != clean {
			t.Errorf("credential-free URL changed: %q -> %q", clean, got)
		}
	}
}

// A hostile matrix over every credential location. The redactor must
// not echo a secret from userinfo, query, or fragment - including for inputs
// that do not parse as URLs at all, which is where two earlier attempts leaked.
func TestRedactURLCredentialsDropsContaminatedReason(t *testing.T) {
	t.Parallel()

	// A malformed URL can contaminate the parser's REASON, not just the URL.
	// net/url truncates at the first '#' and then reports the credential as a
	// bad port. This mirrors the exact shape observed from
	// url.Parse("https://user:<secret>#frag@example.com/mcp"):
	//   parse "https://user:SUPERSECRET": invalid port ":SUPERSECRET" after host
	// Redacting only the URL would leave the secret in the reason, so the whole
	// reason is dropped. Built directly so the test does not depend on the
	// exact wording net/url happens to produce.
	urlErr := &url.Error{
		Op:  "parse",
		URL: "https://user:SUPERSECRET",
		Err: errors.New(`invalid port ":SUPERSECRET" after host`),
	}

	message := redactURLCredentials(urlErr).Error()
	if strings.Contains(message, "SUPERSECRET") {
		t.Fatalf("credential survived in the parse reason: %s", message)
	}
	if !strings.Contains(message, "redacted-url") {
		t.Fatalf("expected the URL to be replaced with the placeholder: %s", message)
	}
	// The parse error must still be reachable for callers that inspect it.
	var asURLErr *url.Error
	if !errors.As(redactURLCredentials(urlErr), &asURLErr) {
		t.Error("errors.As(*url.Error) must still succeed")
	}
}

func TestRedactURLStringCredentialLocations(t *testing.T) {
	t.Parallel()

	secrets := []string{"S3CRET", `S3"C`, `S3\C`, "S3\nC", "S3%C", "S3@C", "S3#C", "S3?C", "ключ", "S3 C"}
	// Only userinfo, query and fragment are credential locations; the path is
	// preserved on purpose because it is diagnostic and not a secret slot.
	templates := []string{
		"https://h/p?key=%s",
		"https://u:%s@h/p",
		"https://h/p?key=%s&key=y",
		"https://u:%s",
		"//u:%s@ho st/p?k=1",
		"https://h/p?a=%s#%s",
		"https://%s@h",
		"https://h:%s/p",
		"https://h/p#%s",
		"https://h/p?%s",
	}
	for _, secret := range secrets {
		for _, template := range templates {
			raw := strings.ReplaceAll(template, "%s", secret)
			if got := redactURLString(raw); strings.Contains(got, secret) {
				t.Errorf("template %q leaked secret %q -> %q", template, secret, got)
			}
		}
	}

	// Inputs that do not parse are replaced wholesale, never echoed.
	for _, raw := range []string{"not a url at all", ":://", "user:SUPERSECRET", "ho st", "//u:S@ho st/p"} {
		got := redactURLString(raw)
		if strings.Contains(got, "SUPERSECRET") || strings.Contains(got, "S@") {
			t.Errorf("unparseable %q leaked: %q", raw, got)
		}
		if got != redactedURLPlaceholder {
			t.Errorf("unparseable %q should become the placeholder, got %q", raw, got)
		}
	}

	// Credential-free URLs stay byte-identical so diagnostics remain useful.
	for _, clean := range []string{"http://127.0.0.1:9/mcp", "https://mcp.example.com/sse", "https://h:8443/p"} {
		if got := redactURLString(clean); got != clean {
			t.Errorf("clean URL changed: %q -> %q", clean, got)
		}
	}
}
func initializeInstructions(t *testing.T, s *server.MCPServer) string {
	t.Helper()
	msg := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	raw, err := json.Marshal(s.HandleMessage(context.Background(), msg))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out.Result.Instructions
}

func TestForwardInstructionsFromBackend(t *testing.T) {
	s := server.NewMCPServer("beis", "1.0.0")
	forwardInstructions(s, &mcp.InitializeResult{Instructions: "BEIS house lessons: ..."})
	if got := initializeInstructions(t, s); got != "BEIS house lessons: ..." {
		t.Fatalf("instructions = %q", got)
	}
}

func TestForwardInstructionsIgnoresEmptyAndNil(t *testing.T) {
	s := server.NewMCPServer("beis", "1.0.0", server.WithInstructions("keep"))
	forwardInstructions(s, nil)
	forwardInstructions(s, &mcp.InitializeResult{})
	if got := initializeInstructions(t, s); got != "keep" {
		t.Fatalf("instructions = %q", got)
	}
}

// lifecycleOptions probes fast and self-heals, which is the production shape
// (mcpProxy.options.autoReconnect=true) that exposed the leak.
func lifecycleOptions() *OptionsV2 {
	o := &OptionsV2{
		PingInterval:      Duration(20 * time.Millisecond),
		ReconnectInterval: Duration(20 * time.Millisecond),
	}
	o.AutoReconnect.Set(true)
	return o
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	if ch == nil {
		t.Fatalf("%s: channel is nil", what)
	}
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not end", what)
	}
}

// Regression: a closed client's keepalive loop kept running for as long as the
// context it was started with, which for boot-time servers is the proxy's own.
// In production a route removed or replaced by a hot reload kept logging
// "MCP health probe failed ... transport closed" and "Failed to reconnect
// downstream ... client is closed" every pingInterval, forever.
func TestCloseStopsKeepaliveLoopWhileParentContextLives(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")

	mcpClient, err := newMCPClient("lifecycle", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       lifecycleOptions(),
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}

	// The parent context stays alive for the whole test, like the proxy's.
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, newProxyServerForTest(t)); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	if err := mcpClient.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitClosed(t, mcpClient.pingDone, "keepalive loop of a closed client")
}

func TestShouldLogRepeatedFailure(t *testing.T) {
	t.Parallel()
	logged := 0
	for i := 1; i <= 100; i++ {
		if shouldLogRepeatedFailure(i) {
			logged++
		}
	}
	// 1..threshold, then every repeatedFailureLogEvery-th.
	want := pingFailureThreshold + 100/repeatedFailureLogEvery
	if logged != want {
		t.Errorf("logged %d of 100 failures, want %d", logged, want)
	}
}

// sessionDownstream is a stateful streamable-http endpoint that behaves like
// the Python SDK's default server (and the TypeScript SDK's stateful one): it
// issues a session id on initialize, answers an unknown session id with 404
// and a request without one with 400 and a JSON-RPC error body. restart()
// forgets every session, which is what a real server does when its process is
// replaced.
type sessionDownstream struct {
	mu       sync.Mutex
	sessions map[string]bool
	next     int
	inits    int
	// calls counts tools/call requests that reached the handler, so a test can
	// tell a retried call from one that ran twice.
	calls int
	url   string
}

func newSessionDownstream(t *testing.T) *sessionDownstream {
	t.Helper()

	d := &sessionDownstream{sessions: map[string]bool{}}
	writeJSON := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	rpcError := func(message string) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": "server-error", "error": map[string]any{"code": -32600, "message": message}}
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
		case http.MethodDelete:
			w.WriteHeader(http.StatusOK)
			return
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, rpcError("Parse error"))
			return
		}

		d.mu.Lock()
		if req.Method == "initialize" {
			d.next++
			d.inits++
			sid := fmt.Sprintf("session-%d", d.next)
			d.sessions[sid] = true
			d.mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sid)
			writeJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": mcp.ProtocolVersion20250618,
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]string{"name": "stateful-downstream", "version": "1"},
				},
			})
			return
		}
		sid := r.Header.Get("Mcp-Session-Id")
		if sid == "" {
			d.mu.Unlock()
			writeJSON(w, http.StatusBadRequest, rpcError("Bad Request: Missing session ID"))
			return
		}
		if !d.sessions[sid] {
			d.mu.Unlock()
			writeJSON(w, http.StatusNotFound, rpcError("Session not found"))
			return
		}
		if req.ID == nil {
			d.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "tools/call" {
			d.calls++
		}
		d.mu.Unlock()

		result := map[string]any{}
		switch req.Method {
		case "tools/list":
			result["tools"] = []map[string]any{{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}
		case "tools/call":
			result["content"] = []map[string]any{{"type": "text", "text": "ok"}}
		case "prompts/list":
			result["prompts"] = []any{}
		case "resources/list":
			result["resources"] = []any{}
		case "resources/templates/list":
			result["resourceTemplates"] = []any{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	})
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	d.url = s.URL
	return d
}

// restart drops every session, as a replaced server process would.
func (d *sessionDownstream) restart() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessions = map[string]bool{}
}

func (d *sessionDownstream) counts() (inits, calls int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inits, d.calls
}

func connectSessionClient(t *testing.T, d *sessionDownstream, options *OptionsV2) *Client {
	t.Helper()

	c, err := newMCPClient("stateful", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           d.url,
		Options:       options,
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, newProxyServerForTest(t)); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	if streamableSessionID(c.getClient()) == "" {
		t.Fatal("downstream did not hand out a session; the test cannot exercise session loss")
	}
	return c
}

func echoRequest() mcp.CallToolRequest {
	var request mcp.CallToolRequest
	request.Params.Name = "echo"
	return request
}

// A restarted downstream rejects the old session id. With autoReconnect the
// call that runs into it rebuilds the connection and is retried once - and the
// tool runs exactly once, because the rejected attempt never reached it.
func TestCallToolRecoversWhenDownstreamRestarts(t *testing.T) {
	t.Parallel()

	d := newSessionDownstream(t)
	// A long interval keeps the keepalive out of the way: the call itself must recover.
	c := connectSessionClient(t, d, &OptionsV2{AutoReconnect: optional.NewField(true), PingInterval: Duration(time.Hour)})
	d.restart()

	result, err := c.callTool(t.Context(), echoRequest())
	if err != nil {
		t.Fatalf("callTool after downstream restart: %v", err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	inits, calls := d.counts()
	if inits != 2 {
		t.Errorf("initialize requests = %d, want 2 (connect and one rebuild)", inits)
	}
	if calls != 1 {
		t.Errorf("tool executions = %d, want 1", calls)
	}
	if streamableSessionID(c.getClient()) == "" {
		t.Error("rebuilt connection has no session")
	}
}

// Without autoReconnect the proxy must not rebuild on its own: the call fails,
// as before, and nothing reconnects behind the operator's back.
func TestCallToolWithoutAutoReconnectDoesNotRebuild(t *testing.T) {
	t.Parallel()

	d := newSessionDownstream(t)
	c := connectSessionClient(t, d, &OptionsV2{PingInterval: Duration(time.Hour)})
	d.restart()

	if _, err := c.callTool(t.Context(), echoRequest()); err == nil {
		t.Fatal("callTool succeeded without autoReconnect; expected the lost-session error")
	}
	if inits, calls := d.counts(); inits != 1 || calls != 0 {
		t.Errorf("initialize requests = %d, tool executions = %d; want 1 and 0", inits, calls)
	}
}

// The keepalive is what finds a lost session when no request comes in. Before
// this fix it never did: after the 404 the next probe went out without a
// session, the server answered with a JSON-RPC error, the probe counted that as
// alive, and the failure count never reached the threshold.
func TestKeepaliveRebuildsLostSession(t *testing.T) {
	t.Parallel()

	d := newSessionDownstream(t)
	c := connectSessionClient(t, d, &OptionsV2{AutoReconnect: optional.NewField(true), PingInterval: Duration(20 * time.Millisecond)})
	d.restart()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if inits, _ := d.counts(); inits >= 2 && c.Health() == healthOK {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if inits, _ := d.counts(); inits < 2 {
		t.Fatalf("keepalive did not rebuild the lost session (initialize requests = %d)", inits)
	}
	if c.Health() != healthOK {
		t.Fatalf("health after rebuild = %v, want ok", c.Health())
	}
	if _, err := c.callTool(t.Context(), echoRequest()); err != nil {
		t.Fatalf("callTool after keepalive rebuild: %v", err)
	}
}

// Without autoReconnect a lost session must leave the client unhealthy - and
// keep it there. Before this fix /_readyz kept reporting ok, because every
// probe after the first answered with a JSON-RPC error.
func TestKeepaliveReportsLostSessionWithoutAutoReconnect(t *testing.T) {
	t.Parallel()

	d := newSessionDownstream(t)
	c := connectSessionClient(t, d, &OptionsV2{PingInterval: Duration(20 * time.Millisecond)})
	d.restart()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && c.Health() != healthFailed {
		time.Sleep(10 * time.Millisecond)
	}
	if c.Health() != healthFailed {
		t.Fatalf("health = %v, want failed after the downstream lost the session", c.Health())
	}
	// Several more probes: none of them may flip it back to ok.
	time.Sleep(200 * time.Millisecond)
	if c.Health() != healthFailed {
		t.Fatalf("health flipped back to %v while the session is still lost", c.Health())
	}
	if inits, _ := d.counts(); inits != 1 {
		t.Errorf("initialize requests = %d, want 1 (no rebuild without autoReconnect)", inits)
	}
}

// Concurrent calls that all hit the same lost session rebuild the connection
// once, and each of them runs exactly once.
func TestConcurrentCallsRebuildOnce(t *testing.T) {
	t.Parallel()

	d := newSessionDownstream(t)
	c := connectSessionClient(t, d, &OptionsV2{AutoReconnect: optional.NewField(true), PingInterval: Duration(time.Hour)})
	d.restart()

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			if _, err := c.callTool(t.Context(), echoRequest()); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("callTool: %v", err)
	}
	inits, calls := d.counts()
	if inits != 2 {
		t.Errorf("initialize requests = %d, want 2 (one rebuild for all callers)", inits)
	}
	if calls != n {
		t.Errorf("tool executions = %d, want %d", calls, n)
	}
}

// A replaced streamable-http transport waits out retireGrace before it is
// closed; shutting the client down must not leave it open meanwhile.
func TestCloseClosesRetiredTransports(t *testing.T) {
	t.Parallel()

	d := newSessionDownstream(t)
	c := connectSessionClient(t, d, &OptionsV2{AutoReconnect: optional.NewField(true), PingInterval: Duration(time.Hour)})
	old := c.getClient()
	d.restart()
	if _, err := c.callTool(t.Context(), echoRequest()); err != nil {
		t.Fatalf("callTool after downstream restart: %v", err)
	}
	c.retiredMu.Lock()
	_, waiting := c.retired[old]
	c.retiredMu.Unlock()
	if !waiting {
		t.Fatal("the replaced transport was not kept for retireGrace")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c.retiredMu.Lock()
	left := len(c.retired)
	c.retiredMu.Unlock()
	if left != 0 {
		t.Errorf("%d retired transport(s) still open after Close", left)
	}
	if _, had := c.hadSession.Load(old); had {
		t.Error("closed transport still recorded as having a session")
	}
}

func TestSessionLost(t *testing.T) {
	t.Parallel()

	d := newSessionDownstream(t)
	c := connectSessionClient(t, d, &OptionsV2{PingInterval: Duration(time.Hour)})
	raw := c.getClient()

	terminated := fmt.Errorf("failed to send request: %w", transport.ErrSessionTerminated)
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "session terminated", err: transport.NewError(terminated), want: true},
		{name: "json-rpc error with the session intact", err: mcp.ErrInvalidRequest, want: false},
		{name: "transport error with the session intact", err: transport.NewError(errors.New("eof")), want: false},
	}
	for _, tt := range tests {
		if got := c.sessionLost(raw, tt.err); got != tt.want {
			t.Errorf("%s: sessionLost = %v, want %v", tt.name, got, tt.want)
		}
	}

	// After the 404 mcp-go clears the session id; any later error on that
	// transport means the session is gone, whatever the server's wording.
	d.restart()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := raw.CallTool(ctx, echoRequest())
	if !errors.Is(err, transport.ErrSessionTerminated) {
		t.Fatalf("first call after restart: err = %v, want ErrSessionTerminated", err)
	}
	_, err = raw.CallTool(ctx, echoRequest())
	if err == nil || errors.Is(err, transport.ErrSessionTerminated) {
		t.Fatalf("second call after restart: err = %v, want the missing-session JSON-RPC error", err)
	}
	if !c.sessionLost(raw, err) {
		t.Errorf("sessionLost(missing-session error) = false, want true")
	}
	if c.sessionLost(nil, err) {
		t.Errorf("sessionLost(nil client) = true, want false")
	}
}

func TestStripHopMetaDropsConnectionEnvelope(t *testing.T) {
	t.Parallel()

	in := &mcp.Meta{
		ProgressToken: "p1",
		AdditionalFields: map[string]any{
			mcp.MetaKeyProtocolVersion:    mcp.ProtocolVersion20260728,
			mcp.MetaKeyClientInfo:         map[string]any{"name": "caller"},
			mcp.MetaKeyClientCapabilities: map[string]any{},
			mcp.MetaKeyServerInfo:         map[string]any{"name": "proxy"},
			mcp.MetaKeyLogLevel:           "debug",
			mcp.MetaKeySubscriptionID:     "7",
			"traceparent":                 "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		},
	}
	out := stripHopMeta(in)

	if out == nil || out.ProgressToken != "p1" {
		t.Fatalf("progressToken lost: %#v", out)
	}
	for _, key := range hopMetaKeys {
		if _, ok := out.AdditionalFields[key]; ok {
			t.Errorf("%s forwarded: %#v", key, out.AdditionalFields)
		}
	}
	if len(out.AdditionalFields) != 1 || out.AdditionalFields["traceparent"] == nil {
		t.Errorf("unrelated fields not passed through: %#v", out.AdditionalFields)
	}
	// The inbound request's meta belongs to the caller; the proxy only edits a copy.
	if len(in.AdditionalFields) != 7 {
		t.Errorf("caller's meta was mutated: %#v", in.AdditionalFields)
	}
}

func TestStripHopMetaEnvelopeOnly(t *testing.T) {
	t.Parallel()

	envelopeOnly := &mcp.Meta{AdditionalFields: map[string]any{mcp.MetaKeyProtocolVersion: mcp.ProtocolVersion20260728}}
	if out := stripHopMeta(envelopeOnly); out != nil {
		t.Errorf("envelope-only meta = %#v, want nil so no empty _meta is sent", out)
	}
	if out := stripHopMeta(nil); out != nil {
		t.Errorf("nil meta = %#v, want nil", out)
	}
	progressOnly := &mcp.Meta{ProgressToken: "p1"}
	if out := stripHopMeta(progressOnly); out != progressOnly {
		t.Errorf("meta without additional fields = %#v, want it returned unchanged", out)
	}
}

func TestCatalogFilterFuncSemantics(t *testing.T) {
	t.Parallel()

	allow := func(list ...string) *FilterConfig {
		return &FilterConfig{Mode: FilterModeAllow, List: list}
	}
	block := func(list ...string) *FilterConfig {
		return &FilterConfig{Mode: FilterModeBlock, List: list}
	}
	tests := []struct {
		name   string
		filter *FilterConfig
		keys   []string
		want   bool
	}{
		{name: "nil filter allows", filter: nil, keys: []string{"a"}, want: true},
		// An empty allow list cannot come from a validated config; a
		// programmatic one matches nothing and exposes nothing.
		{name: "empty allow list exposes nothing", filter: allow(), keys: []string{"a"}, want: false},
		{name: "allow matches first key", filter: allow("uri://a"), keys: []string{"uri://a", "a"}, want: true},
		{name: "allow matches second key", filter: allow("a"), keys: []string{"uri://a", "a"}, want: true},
		{name: "allow excludes unlisted", filter: allow("b"), keys: []string{"uri://a", "a"}, want: false},
		{name: "empty block list allows", filter: block(), keys: []string{"a"}, want: true},
		{name: "block hides by any key", filter: block("a"), keys: []string{"uri://a", "a"}, want: false},
		{name: "block keeps unlisted", filter: block("b"), keys: []string{"uri://a", "a"}, want: true},
		{name: "mode is case-insensitive", filter: &FilterConfig{Mode: "BLOCK", List: []string{"a"}}, keys: []string{"a"}, want: false},
		{name: "unknown mode fails closed", filter: &FilterConfig{Mode: "nonsense", List: []string{"a"}}, keys: []string{"b"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resourceFilterFunc("test", &OptionsV2{ResourceFilter: tt.filter})(tt.keys...)
			if got != tt.want {
				t.Errorf("resourceFilter = %v, want %v", got, tt.want)
			}
			got = promptFilterFunc("test", &OptionsV2{PromptFilter: tt.filter})(tt.keys...)
			if got != tt.want {
				t.Errorf("promptFilter = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToolFilterIgnoresResourceAndPromptFilters(t *testing.T) {
	t.Parallel()

	options := &OptionsV2{
		ResourceFilter: &FilterConfig{Mode: FilterModeAllow},
		PromptFilter:   &FilterConfig{Mode: FilterModeAllow},
	}
	if !toolFilterFunc("test", options)("any") {
		t.Fatal("resourceFilter/promptFilter must not affect tools")
	}
	if !resourceFilterFunc("test", &OptionsV2{ToolFilter: &FilterConfig{Mode: FilterModeAllow, List: []string{"x"}}})("any") {
		t.Fatal("toolFilter must not affect resources")
	}
}

func TestResourceAndPromptFiltersApplyToCatalog(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.setPrompts("greet", "internal")
	downstream.setResources("test://public", "test://secret")
	downstream.setResourceTemplates(
		map[string]any{"name": "echo", "uriTemplate": "test://echo/{word}"},
		map[string]any{"name": "dump", "uriTemplate": "test://dump/{id}"},
	)

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options: &OptionsV2{
			ResourceFilter: &FilterConfig{Mode: FilterModeBlock, List: []string{"test://secret", "dump"}},
			PromptFilter:   &FilterConfig{Mode: FilterModeAllow, List: []string{"greet"}},
		},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}

	if got := serverListNames(t, proxyServer, "resources/list", "resources", "uri"); !slices.Equal(got, []string{"test://public"}) {
		t.Errorf("resources = %v, want [test://public]", got)
	}
	if got := serverResourceTemplateNames(t, proxyServer); !slices.Equal(got, []string{"echo"}) {
		t.Errorf("resource templates = %v, want [echo]", got)
	}
	if got := serverListNames(t, proxyServer, "prompts/list", "prompts", "name"); !slices.Equal(got, []string{"greet"}) {
		t.Errorf("prompts = %v, want [greet]", got)
	}
}

// A URI hidden by a block list must not be readable through an exposed
// template that happens to match it, whatever way it is spelled.
func TestBlockedResourceIsNotReadableThroughTemplate(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.setResources("test://echo/secret", "test://echo/hidden")
	downstream.setResourceName("test://echo/hidden", "hidden-doc")
	downstream.setResourceTemplates(
		map[string]any{"name": "echo", "uriTemplate": "test://echo/{word}"},
		map[string]any{"name": "dump", "uriTemplate": "test://dump/{id}"},
	)

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options: &OptionsV2{
			ResourceFilter: &FilterConfig{Mode: FilterModeBlock, List: []string{"test://echo/secret", "hidden-doc", "test://dump/{id}"}},
		},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	if got := serverResourceTemplateNames(t, proxyServer); !slices.Equal(got, []string{"echo"}) {
		t.Fatalf("templates = %v, want [echo] (dump blocked by uriTemplate)", got)
	}

	// Each spelling must fail. Some are refused by the proxy's check, others
	// never match the template in mcp-go; either way none may be served.
	for _, uri := range []string{
		"test://echo/secret",
		"test://echo/%73ecret",
		"test://echo/secret#frag",
		"TEST://ECHO/secret",
		"test://echo/hidden", // hidden by name, not by URI
	} {
		if msg := readResourceError(t, proxyServer, uri); msg == "" {
			t.Errorf("read %q was served", uri)
		}
	}
	for _, uri := range []string{"test://echo/secret", "test://echo/%73ecret", "test://echo/hidden"} {
		if msg := readResourceError(t, proxyServer, uri); !strings.Contains(msg, "not available") {
			t.Errorf("read %q: error = %q, want the proxy's refusal", uri, msg)
		}
	}
	if msg := readResourceError(t, proxyServer, "test://echo/other"); msg != "" {
		t.Errorf("read of an unblocked URI failed: %q", msg)
	}
}

func TestNormalizeResourceURI(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"test://echo/secret":      "test://echo/secret",
		"TEST://Echo/Secret":      "test://echo/Secret",
		"test://echo/%73ecret":    "test://echo/secret",
		"test://echo/a%2fb":       "test://echo/a%2Fb",
		"test://echo/x#fragment":  "test://echo/x",
		"file:///etc/%70asswd":    "file:///etc/passwd",
		"urn:Example:%7Eitem":     "urn:Example:~item",
		"no-scheme/path%41":       "no-scheme/pathA",
		"test://h?q=%41#f":        "test://h?q=A",
		"test://echo/bad%zzvalue": "test://echo/bad%zzvalue",
		"test://echo/%7%30ass":    "test://echo/pass",
	}
	for in, want := range tests {
		if got := normalizeResourceURI(in); got != want {
			t.Errorf("normalizeResourceURI(%q) = %q, want %q", in, got, want)
		}
	}
}

// Allow mode exposes a template by name, and with it every URI it matches.
func TestAllowedTemplateByName(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.setResourceTemplates(
		map[string]any{"name": "echo", "uriTemplate": "test://echo/{word}"},
		map[string]any{"name": "dump", "uriTemplate": "test://dump/{id}"},
	)
	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options:       &OptionsV2{ResourceFilter: &FilterConfig{Mode: FilterModeAllow, List: []string{"echo"}}},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()
	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	if got := serverResourceTemplateNames(t, proxyServer); !slices.Equal(got, []string{"echo"}) {
		t.Fatalf("templates = %v, want [echo]", got)
	}
	if msg := readResourceError(t, proxyServer, "test://echo/anything"); msg != "" {
		t.Errorf("read through the allowed template failed: %q", msg)
	}
	if msg := readResourceError(t, proxyServer, "test://dump/1"); msg == "" {
		t.Error("read through a template that is not allowed succeeded")
	}
}

// serverListNames reads back one field of every entry a list method returns.
func serverListNames(t *testing.T, mcpServer *server.MCPServer, method, resultKey, field string) []string {
	t.Helper()

	resp := mcpServer.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{}}`))
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal %s response: %v", method, err)
	}
	var decoded struct {
		Result map[string][]map[string]any `json:"result"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal %s response: %v (%s)", method, err, raw)
	}
	names := make([]string, 0)
	for _, entry := range decoded.Result[resultKey] {
		if value, ok := entry[field].(string); ok {
			names = append(names, value)
		}
	}
	slices.Sort(names)
	return names
}

// readResourceError performs resources/read on the proxy-side server and
// returns the JSON-RPC error message, or "" on success.
func readResourceError(t *testing.T, mcpServer *server.MCPServer, uri string) string {
	t.Helper()

	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "resources/read",
		"params": map[string]any{"uri": uri},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	raw, err := json.Marshal(mcpServer.HandleMessage(context.Background(), request))
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var decoded struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if decoded.Error == nil {
		return ""
	}
	return decoded.Error.Message
}

// An empty allow list cannot come from a validated config; a programmatic one
// matches nothing, so resources, templates and prompts are all hidden while
// tools (which have no filter here) still show.
func TestEmptyAllowListHidesAllResourcesAndPrompts(t *testing.T) {
	t.Parallel()

	downstream := newRawDownstream(t)
	downstream.setTools("alpha")
	downstream.setPrompts("greet")
	downstream.setResources("test://public")
	downstream.setResourceTemplates(map[string]any{"name": "echo", "uriTemplate": "test://echo/{word}"})

	mcpClient, err := newMCPClient("test", &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           downstream.url,
		Options: &OptionsV2{
			ResourceFilter: &FilterConfig{Mode: FilterModeAllow},
			PromptFilter:   &FilterConfig{Mode: FilterModeAllow},
		},
	})
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer func() { _ = mcpClient.Close() }()

	proxyServer := newProxyServerForTest(t)
	if err := mcpClient.addToMCPServer(t.Context(), mcp.Implementation{Name: "test"}, proxyServer); err != nil {
		t.Fatalf("addToMCPServer: %v", err)
	}
	if got := serverListNames(t, proxyServer, "resources/list", "resources", "uri"); len(got) != 0 {
		t.Errorf("resources = %v, want none", got)
	}
	if got := serverResourceTemplateNames(t, proxyServer); len(got) != 0 {
		t.Errorf("resource templates = %v, want none", got)
	}
	if got := serverListNames(t, proxyServer, "prompts/list", "prompts", "name"); len(got) != 0 {
		t.Errorf("prompts = %v, want none", got)
	}
	if got := serverToolNames(t, proxyServer); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("tools = %v, want [alpha]", got)
	}
}
