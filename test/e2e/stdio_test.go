package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// The stdio tests run the real binary against a real MCP server subprocess and
// speak MCP to it over HTTP, so they cover what unit tests cannot: tool
// filtering, pagination, auth, env reaching the subprocess, readiness, and
// graceful shutdown.

func TestStdioServerOverHTTP(t *testing.T) {
	skipShort(t)
	t.Parallel()

	for _, serverType := range []string{"streamable-http", "sse"} {
		t.Run(serverType, func(t *testing.T) {
			t.Parallel()

			configPath, addr := stdioConfig(t, serverType)
			proxy := startProxy(t, configPath, addr)

			checkHealthEndpoints(t, proxy)
			checkAuth(t, proxy, serverType)
			if got := proxy.get(t, "/off/mcp"); got != http.StatusNotFound {
				t.Errorf("disabled server route = %d, want 404 (it must not be mounted)", got)
			}

			mcpClient := proxy.connect(t, serverType, "fixture")
			checkTools(t, mcpClient)
			checkPrompts(t, mcpClient)
			checkResources(t, mcpClient)

			proxy.stop(t)
			assertPortReleased(t, addr)
		})
	}
}

func checkHealthEndpoints(t *testing.T, proxy *proxy) {
	t.Helper()

	var health struct {
		Name        string `json:"name"`
		ServerCount int    `json:"serverCount"`
		Status      string `json:"status"`
		Version     string `json:"version"`
	}
	resp, err := http.Get(proxy.baseURL + "/_healthz")
	if err != nil {
		t.Fatalf("GET /_healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/_healthz status = %d, want 200", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode /_healthz body: %v", err)
	}
	if health.Name != "integration-proxy" || health.Version != "9.9.9" || health.Status != "ok" {
		t.Errorf("health document = %+v, want the configured name/version and status ok", health)
	}
	// "off" is disabled, so only "fixture" counts.
	if health.ServerCount != 1 {
		t.Errorf("health serverCount = %d, want 1 (disabled servers excluded)", health.ServerCount)
	}
}

func checkAuth(t *testing.T, proxy *proxy, serverType string) {
	t.Helper()

	endpoint := proxy.baseURL + mcpPath(serverType, "fixture")
	tests := []struct {
		name   string
		header string
		want   int
	}{
		{name: "no token", header: "", want: http.StatusUnauthorized},
		{name: "wrong token", header: "Bearer wrong-token", want: http.StatusUnauthorized},
		{name: "empty bearer", header: "Bearer ", want: http.StatusUnauthorized},
		// RFC 7235 makes the scheme case-insensitive.
		{name: "lowercase scheme", header: "bearer " + testAuthToken, want: http.StatusOK},
	}
	for _, tt := range tests {
		request, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatalf("%s: build request: %v", tt.name, err)
		}
		if tt.header != "" {
			request.Header.Set("Authorization", tt.header)
		}
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s: GET %s: %v", tt.name, endpoint, err)
		}
		_ = resp.Body.Close()
		if tt.want == http.StatusOK {
			// An accepted request must at least get past the auth middleware.
			if resp.StatusCode == http.StatusUnauthorized {
				t.Errorf("%s: status = 401, want the request to be authorized", tt.name)
			}
			continue
		}
		if resp.StatusCode != tt.want {
			t.Errorf("%s: status = %d, want %d", tt.name, resp.StatusCode, tt.want)
		}
	}
}

func checkTools(t *testing.T, mcpClient *client.Client) {
	t.Helper()

	tools, err := mcpClient.ListTools(testContext(t), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	// The fixture registers eight tools and serves them two per page, so a
	// complete list proves both that the block filter dropped "blocked" and
	// that multi-page listing works end to end.
	want := []string{"add", "echo", "fail", "getenv", "hang", "noisy_stderr", "pid"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v (blocked tool filtered, all pages fetched)", names, want)
	}

	t.Run("echo", func(t *testing.T) {
		if got := callToolText(t, mcpClient, "echo", map[string]any{"message": "hello proxy"}); got != "hello proxy" {
			t.Errorf("echo = %q, want %q", got, "hello proxy")
		}
	})

	t.Run("add", func(t *testing.T) {
		if got := callToolText(t, mcpClient, "add", map[string]any{"a": 2, "b": 40}); got != "42" {
			t.Errorf("add = %q, want %q", got, "42")
		}
	})

	// mcpServers.fixture.env has to reach the stdio subprocess.
	t.Run("env passed to subprocess", func(t *testing.T) {
		if got := callToolText(t, mcpClient, "getenv", map[string]any{"name": "MCP_PROXY_TEST_ENV"}); got != testEnvValue {
			t.Errorf("getenv = %q, want %q", got, testEnvValue)
		}
	})

	// A tool that fails in-band must arrive as an error result, not as a
	// transport failure.
	t.Run("tool error", func(t *testing.T) {
		result, err := callTool(t, mcpClient, "fail", nil)
		if err != nil {
			t.Fatalf("call fail: %v", err)
		}
		if !result.IsError {
			t.Errorf("fail result IsError = false, want true")
		}
		if text := resultText(t, result); !strings.Contains(text, "intentional tool failure") {
			t.Errorf("fail result = %q, want the downstream error message", text)
		}
	})

	t.Run("blocked tool unreachable", func(t *testing.T) {
		result, err := callTool(t, mcpClient, "blocked", nil)
		if err == nil && !result.IsError {
			t.Errorf("blocked tool was callable through the proxy: %+v", result)
		}
	})
}

func checkPrompts(t *testing.T, mcpClient *client.Client) {
	t.Helper()

	ctx := testContext(t)
	prompts, err := mcpClient.ListPrompts(ctx, mcp.ListPromptsRequest{})
	if err != nil {
		t.Fatalf("list prompts: %v", err)
	}
	names := make([]string, 0, len(prompts.Prompts))
	for _, prompt := range prompts.Prompts {
		names = append(names, prompt.Name)
	}
	slices.Sort(names)
	// Three prompts at two per page: another multi-page listing.
	if want := []string{"greeting", "summarize", "translate"}; !slices.Equal(names, want) {
		t.Fatalf("prompts = %v, want %v (all pages fetched)", names, want)
	}

	request := mcp.GetPromptRequest{}
	request.Params.Name = "greeting"
	request.Params.Arguments = map[string]string{"name": "mcp-proxy"}
	result, err := mcpClient.GetPrompt(ctx, request)
	if err != nil {
		t.Fatalf("get prompt: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("prompt messages = %d, want 1", len(result.Messages))
	}
	text, ok := mcp.AsTextContent(result.Messages[0].Content)
	if !ok {
		t.Fatalf("prompt content type = %T, want text", result.Messages[0].Content)
	}
	if text.Text != "Hello, mcp-proxy!" {
		t.Errorf("prompt text = %q, want %q", text.Text, "Hello, mcp-proxy!")
	}
}

func checkResources(t *testing.T, mcpClient *client.Client) {
	t.Helper()

	ctx := testContext(t)
	resources, err := mcpClient.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}
	uris := make([]string, 0, len(resources.Resources))
	for _, resource := range resources.Resources {
		uris = append(uris, resource.URI)
	}
	slices.Sort(uris)
	want := []string{"test://static/config", "test://static/notes", "test://static/readme"}
	if !slices.Equal(uris, want) {
		t.Fatalf("resources = %v, want %v (all pages fetched)", uris, want)
	}

	if got := readResourceText(t, mcpClient, "test://static/readme"); got != "static readme contents" {
		t.Errorf("readme contents = %q, want %q", got, "static readme contents")
	}

	templates, err := mcpClient.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
	if err != nil {
		t.Fatalf("list resource templates: %v", err)
	}
	if len(templates.ResourceTemplates) != 1 {
		t.Fatalf("resource templates = %d, want 1", len(templates.ResourceTemplates))
	}
	if uri := templates.ResourceTemplates[0].URITemplate.Raw(); uri != "test://echo/{word}" {
		t.Errorf("template URI = %q, want %q", uri, "test://echo/{word}")
	}
	if got := readResourceText(t, mcpClient, "test://echo/hi"); got != "echo:hi" {
		t.Errorf("templated resource = %q, want %q", got, "echo:hi")
	}
}

// Readiness has to track the downstream connection, not just startup: once the
// stdio subprocess dies every call through that route fails, and a load
// balancer should stop sending traffic here.
func TestReadinessReflectsDownstreamHealth(t *testing.T) {
	skipShort(t)
	t.Parallel()

	configPath, addr := stdioConfig(t, "streamable-http")
	proxy := startProxy(t, configPath, addr)

	mcpClient := proxy.connect(t, "streamable-http", "fixture")
	pid, err := strconv.Atoi(callToolText(t, mcpClient, "pid", nil))
	if err != nil {
		t.Fatalf("parse downstream pid: %v", err)
	}

	// A healthy server must survive several ping cycles: if pinging were broken
	// or unsupported, readiness would flap to 503 on its own.
	for range 5 {
		time.Sleep(250 * time.Millisecond)
		if code, body := proxy.ready(t); code != http.StatusOK {
			t.Fatalf("/_readyz = %d %s while the downstream is healthy", code, body)
		}
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill downstream pid %d: %v", pid, err)
	}

	body := proxy.waitForReadyState(t, http.StatusServiceUnavailable)
	if !strings.Contains(body, `"status":"degraded"`) || !strings.Contains(body, `"unhealthy":["fixture"]`) {
		t.Errorf("/_readyz body = %s, want degraded with the fixture named", body)
	}

	// Liveness is about the proxy process, so it stays OK.
	if got := proxy.get(t, "/_healthz"); got != http.StatusOK {
		t.Errorf("/_healthz = %d after the downstream died, want 200", got)
	}

	// A downstream that died must not make the proxy's own exit status non-zero.
	proxy.stop(t)
}
