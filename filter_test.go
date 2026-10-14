package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestCatalogFilterFuncSemantics(t *testing.T) {
	t.Parallel()

	allow := func(list ...string) *ToolFilterConfig {
		return &ToolFilterConfig{Mode: ToolFilterModeAllow, List: list}
	}
	block := func(list ...string) *ToolFilterConfig {
		return &ToolFilterConfig{Mode: ToolFilterModeBlock, List: list}
	}
	tests := []struct {
		name   string
		filter *ToolFilterConfig
		keys   []string
		want   bool
	}{
		{name: "nil filter allows", filter: nil, keys: []string{"a"}, want: true},
		{name: "empty allow list exposes nothing", filter: allow(), keys: []string{"a"}, want: false},
		{name: "allow matches first key", filter: allow("uri://a"), keys: []string{"uri://a", "a"}, want: true},
		{name: "allow matches second key", filter: allow("a"), keys: []string{"uri://a", "a"}, want: true},
		{name: "allow excludes unlisted", filter: allow("b"), keys: []string{"uri://a", "a"}, want: false},
		{name: "empty block list allows", filter: block(), keys: []string{"a"}, want: true},
		{name: "block hides by any key", filter: block("a"), keys: []string{"uri://a", "a"}, want: false},
		{name: "block keeps unlisted", filter: block("b"), keys: []string{"uri://a", "a"}, want: true},
		{name: "mode is case-insensitive", filter: &ToolFilterConfig{Mode: "BLOCK", List: []string{"a"}}, keys: []string{"a"}, want: false},
		{name: "unknown mode fails closed", filter: &ToolFilterConfig{Mode: "nonsense", List: []string{"a"}}, keys: []string{"b"}, want: false},
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
		ResourceFilter: &ToolFilterConfig{Mode: ToolFilterModeAllow},
		PromptFilter:   &ToolFilterConfig{Mode: ToolFilterModeAllow},
	}
	if !toolFilterFunc("test", options)("any") {
		t.Fatal("resourceFilter/promptFilter must not affect tools")
	}
	if !resourceFilterFunc("test", &OptionsV2{ToolFilter: &ToolFilterConfig{Mode: ToolFilterModeAllow, List: []string{"x"}}})("any") {
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
			ResourceFilter: &ToolFilterConfig{Mode: ToolFilterModeBlock, List: []string{"test://secret", "dump"}},
			PromptFilter:   &ToolFilterConfig{Mode: ToolFilterModeAllow, List: []string{"greet"}},
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
			ResourceFilter: &ToolFilterConfig{Mode: ToolFilterModeAllow},
			PromptFilter:   &ToolFilterConfig{Mode: ToolFilterModeAllow},
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
			ResourceFilter: &ToolFilterConfig{Mode: ToolFilterModeBlock, List: []string{"test://echo/secret", "hidden-doc", "test://dump/{id}"}},
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
		Options:       &OptionsV2{ResourceFilter: &ToolFilterConfig{Mode: ToolFilterModeAllow, List: []string{"echo"}}},
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

func TestValidateConfigRejectsBadResourceAndPromptFilters(t *testing.T) {
	t.Parallel()

	for _, field := range []string{"resourceFilter", "promptFilter"} {
		for _, tc := range []struct {
			filter  *ToolFilterConfig
			wantErr string
		}{
			{filter: &ToolFilterConfig{Mode: "permit"}, wantErr: field + ".mode"},
			{filter: &ToolFilterConfig{Mode: ToolFilterModeBlock, List: []string{" "}}, wantErr: field + ".list[0]"},
		} {
			options := &OptionsV2{}
			if field == "resourceFilter" {
				options.ResourceFilter = tc.filter
			} else {
				options.PromptFilter = tc.filter
			}
			err := validateOptions("mcpServers[\"s\"].options", options)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want it to mention %q", field, err, tc.wantErr)
			}
		}
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
