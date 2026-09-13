package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// These tests cover the outbound half of the proxy: connecting to a downstream
// MCP server over sse or streamable-http. The stdio tests never reach those
// transports, and they are the ones that carry the config's `headers` and the
// Accept-Encoding workaround.
//
// The downstream runs in this process behind httptest, which is still a real
// TCP/HTTP/JSON-RPC round trip - unlike stdio, there is nothing a separate
// process would add - and it lets the test inspect what the downstream received
// and take it away mid-run.

const downstreamAPIKey = "downstream-api-key"

const remoteConfigTemplate = `{
  "mcpProxy": {
    "baseURL": "http://%[1]s",
    "addr": "%[1]s",
    "name": "integration-proxy",
    "version": "9.9.9",
    "type": "streamable-http",
    "startupGracePeriod": "` + testGracePeriod + `",
    "options": {"pingInterval": "` + testPingInterval + `"}
  },
  "mcpServers": {
    "remote": {
      "transportType": "%[2]s",
      "url": "%[3]s",
      "headers": {"X-Api-Key": "%[4]s"}
    }
  }
}`

// downstreamRecorder captures the headers the downstream server was called
// with, so the test can assert on what the proxy sent rather than on what it
// was configured to send.
type downstreamRecorder struct {
	mu       sync.Mutex
	requests []http.Header
}

func (d *downstreamRecorder) record(header http.Header) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, header.Clone())
}

// values returns every value seen for a header across all requests.
func (d *downstreamRecorder) values(key string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var seen []string
	for _, header := range d.requests {
		seen = append(seen, header.Get(key))
	}
	return seen
}

// startDownstreamMCP serves a minimal MCP server over the given transport and
// returns the URL mcp-proxy should be pointed at.
func startDownstreamMCP(t *testing.T, transportType string) (string, *downstreamRecorder, *httptest.Server) {
	t.Helper()

	mcpServer := server.NewMCPServer("downstream", "1.0.0", server.WithToolCapabilities(true))
	mcpServer.AddTool(
		mcp.NewTool("echo", mcp.WithString("message", mcp.Required())),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			message, err := request.RequireString("message")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("downstream:" + message), nil
		},
	)

	var handler http.Handler
	suffix := ""
	if transportType == "sse" {
		handler = server.NewSSEServer(mcpServer)
		suffix = "/sse"
	} else {
		handler = server.NewStreamableHTTPServer(mcpServer, server.WithStateLess(true))
	}

	recorder := &downstreamRecorder{}
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r.Header)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(downstream.Close)

	return downstream.URL + suffix, recorder, downstream
}

func remoteConfig(t *testing.T, transportType, downstreamURL string) (string, string) {
	t.Helper()

	addr := freeAddr(t)
	return writeConfig(t, fmt.Sprintf(remoteConfigTemplate,
		addr, transportType, downstreamURL, downstreamAPIKey)), addr
}

func TestRemoteServerTransports(t *testing.T) {
	skipShort(t)
	t.Parallel()

	for _, transportType := range []string{"streamable-http", "sse"} {
		t.Run(transportType, func(t *testing.T) {
			t.Parallel()

			downstreamURL, recorder, _ := startDownstreamMCP(t, transportType)
			configPath, addr := remoteConfig(t, transportType, downstreamURL)
			proxy := startProxy(t, configPath, addr)

			mcpClient := proxy.connect(t, "streamable-http", "remote")
			got := callToolText(t, mcpClient, "echo", map[string]any{"message": "hi"})
			if want := "downstream:hi"; got != want {
				t.Errorf("echo through remote server = %q, want %q", got, want)
			}

			// The configured headers have to reach the downstream, which is how
			// API keys are passed to hosted MCP servers.
			for _, value := range recorder.values("X-Api-Key") {
				if value != downstreamAPIKey {
					t.Errorf("downstream saw X-Api-Key %q, want %q", value, downstreamAPIKey)
				}
			}

			// Some MCP servers otherwise reply with gzip that mcp-go's decoder
			// cannot read, so every request must opt out of compression.
			for _, value := range recorder.values("Accept-Encoding") {
				if value != "identity" {
					t.Errorf("downstream saw Accept-Encoding %q, want %q", value, "identity")
				}
			}
		})
	}
}

// One downstream that never answers used to hold readiness at "initializing"
// forever, even though the other servers were already mounted and serving.
func TestSlowRemoteServerDoesNotBlockReadiness(t *testing.T) {
	skipShort(t)
	t.Parallel()

	// A server that accepts the connection and then says nothing, which is how
	// a wedged remote MCP server behaves.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
	}))
	t.Cleanup(hanging.CloseClientConnections)

	goodURL, _, _ := startDownstreamMCP(t, "streamable-http")
	addr := freeAddr(t)
	configPath := writeConfig(t, fmt.Sprintf(`{
  "mcpProxy": {
    "baseURL": "http://%[1]s", "addr": "%[1]s", "name": "p", "version": "1",
    "type": "streamable-http", "startupGracePeriod": "`+testGracePeriod+`"
  },
  "mcpServers": {
    "good": {"transportType":"streamable-http","url":"%[2]s"},
    "slow": {"transportType":"streamable-http","url":"%[3]s"}
  }
}`, addr, goodURL, hanging.URL))

	// startProxy only returns once /_readyz is 200, which is the assertion: the
	// slow server is still connecting and no longer blocks it.
	proxy := startProxy(t, configPath, addr)

	mcpClient := proxy.connect(t, "streamable-http", "good")
	if got := callToolText(t, mcpClient, "echo", map[string]any{"message": "hi"}); got != "downstream:hi" {
		t.Errorf("echo through the healthy server = %q, want %q", got, "downstream:hi")
	}

	// The slow server has no route yet, and is not reported unhealthy: it has
	// never connected, so there is nothing to route around.
	if got := proxy.get(t, "/slow/mcp"); got != http.StatusNotFound {
		t.Errorf("slow server route = %d, want 404 while it is still connecting", got)
	}
	if _, body := proxy.ready(t); strings.Contains(body, "unhealthy") {
		t.Errorf("/_readyz body = %s, want no unhealthy list for a server that never connected", body)
	}
}

// With every downstream unreachable the proxy has no routes at all, so it must
// not report ready: no client can be named as broken, but everything 404s.
func TestProxyWithNothingMountedIsNotReady(t *testing.T) {
	skipShort(t)
	t.Parallel()

	// Nothing is listening here, so the only client never connects.
	dead := freeAddr(t)
	addr := freeAddr(t)
	configPath := writeConfig(t, fmt.Sprintf(`{
  "mcpProxy": {
    "baseURL": "http://%[1]s", "addr": "%[1]s", "name": "p", "version": "1",
    "type": "streamable-http", "startupGracePeriod": "`+testGracePeriod+`"
  },
  "mcpServers": {
    "gone": {"transportType":"streamable-http","url":"http://%[2]s/mcp"}
  }
}`, addr, dead))

	proxy := launchProxy(t, configPath, addr)
	body := proxy.waitForReadyBody(t, http.StatusServiceUnavailable, `"status":"unavailable"`)
	if strings.Contains(body, "unhealthy") {
		t.Errorf("/_readyz body = %s, want no unhealthy list for a server that never connected", body)
	}
}

// A remote downstream that goes away has to show up in readiness the same way a
// dead stdio subprocess does.
func TestRemoteServerHealthDegrades(t *testing.T) {
	skipShort(t)
	t.Parallel()

	downstreamURL, _, downstream := startDownstreamMCP(t, "streamable-http")
	configPath, addr := remoteConfig(t, "streamable-http", downstreamURL)
	proxy := startProxy(t, configPath, addr)

	if code, body := proxy.ready(t); code != http.StatusOK {
		t.Fatalf("/_readyz = %d %s while the downstream is healthy", code, body)
	}

	// CloseClientConnections first so in-flight keepalives fail immediately
	// rather than hanging on a half-open socket.
	downstream.CloseClientConnections()
	downstream.Close()

	body := proxy.waitForReadyState(t, http.StatusServiceUnavailable)
	if !strings.Contains(body, `"unhealthy":["remote"]`) {
		t.Errorf("/_readyz body = %s, want the remote server named", body)
	}
}

// The keepalive probe has two eras: a modern connection is probed with a real
// tools/list because 2026-07-28 removed ping, while a legacy connection is
// still pinged. SSE has no server/discover, so the proxy settles that
// connection on the legacy handshake - the ping branch the streamable-http
// degradation test above cannot reach.
func TestSSERemoteServerHealthDegrades(t *testing.T) {
	skipShort(t)
	t.Parallel()

	downstreamURL, _, downstream := startDownstreamMCP(t, "sse")
	configPath, addr := remoteConfig(t, "sse", downstreamURL)
	proxy := startProxy(t, configPath, addr)

	if code, body := proxy.ready(t); code != http.StatusOK {
		t.Fatalf("/_readyz = %d %s while the SSE downstream is healthy", code, body)
	}

	// CloseClientConnections first so in-flight keepalives fail immediately
	// rather than hanging on a half-open socket.
	downstream.CloseClientConnections()
	downstream.Close()

	body := proxy.waitForReadyState(t, http.StatusServiceUnavailable)
	if !strings.Contains(body, `"unhealthy":["remote"]`) {
		t.Errorf("/_readyz body = %s, want the remote server named", body)
	}
}
