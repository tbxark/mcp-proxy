package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// newDownstreamHandler builds the minimal echo MCP server these tests proxy to,
// returning the handler and the route suffix its transport expects.
func newDownstreamHandler(transportType string) (http.Handler, string) {
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

	if transportType == "sse" {
		return server.NewSSEServer(mcpServer), "/sse"
	}
	return server.NewStreamableHTTPServer(mcpServer, server.WithStateLess(true)), "/mcp"
}

// startDownstreamOnAddr serves the standard echo downstream on a fixed address,
// so a test can take it away and put a replacement back on the same port. The
// bind is retried briefly: a listener closed moments ago may not be released
// yet, and the test only rebinds to prove the proxy recovered.
func startDownstreamOnAddr(t *testing.T, addr, transportType string) *httptest.Server {
	t.Helper()

	handler, _ := newDownstreamHandler(transportType)
	deadline := time.Now().Add(5 * time.Second)
	for {
		listener, err := net.Listen("tcp", addr)
		if err == nil {
			downstream := httptest.NewUnstartedServer(handler)
			downstream.Listener = listener
			downstream.Start()
			t.Cleanup(downstream.Close)
			return downstream
		}
		if time.Now().After(deadline) {
			t.Fatalf("listen on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startDownstreamMCP serves a minimal MCP server over the given transport and
// returns the URL mcp-proxy should be pointed at.
func startDownstreamMCP(t *testing.T, transportType string) (string, *downstreamRecorder, *httptest.Server) {
	t.Helper()

	handler, suffix := newDownstreamHandler(transportType)

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

// autoReconnectConfig points the proxy at a downstream URL with the given
// per-server options, so a test can toggle autoReconnect without a template.
func autoReconnectConfig(t *testing.T, downstreamURL string, extraOptions string) (string, string) {
	t.Helper()

	addr := freeAddr(t)
	configPath := writeConfig(t, fmt.Sprintf(`{
  "mcpProxy": {
    "baseURL": "http://%[1]s", "addr": "%[1]s", "name": "p", "version": "1",
    "type": "streamable-http", "startupGracePeriod": "`+testGracePeriod+`",
    "options": {"pingInterval": "`+testPingInterval+`"}
  },
  "mcpServers": {
    "remote": {
      "transportType": "streamable-http",
      "url": "%[2]s",
      "options": %[3]s
    }
  }
}`, addr, downstreamURL, extraOptions))
	return configPath, addr
}

// A backend that is down when the proxy starts must be mounted once it appears,
// instead of 404ing until the whole proxy is restarted. The route is published
// only after a successful connection, so autoReconnect keeps trying in the
// background until then.
func TestAutoReconnectMountsBackendThatStartsLater(t *testing.T) {
	skipShort(t)
	t.Parallel()

	// The downstream address is known up front but nothing is listening yet.
	downstreamAddr := freeAddr(t)
	downstreamURL := "http://" + downstreamAddr + "/mcp"
	configPath, addr := autoReconnectConfig(t, downstreamURL,
		`{"autoReconnect": true, "reconnectInterval": "100ms"}`)

	proxy := launchProxy(t, configPath, addr)

	// Nothing has connected, so the proxy reports that it has no routes and the
	// endpoint 404s - the state the retry exists to escape.
	proxy.waitForReadyBody(t, http.StatusServiceUnavailable, `"status":"unavailable"`)
	if got := proxy.get(t, "/remote/mcp"); got != http.StatusNotFound {
		t.Errorf("route before the backend exists = %d, want 404", got)
	}

	// Bring the backend up on the address the proxy is already retrying.
	startDownstreamOnAddr(t, downstreamAddr, "streamable-http")

	// The retry connects and mounts the route, which flips readiness to 200 and
	// makes the endpoint serve traffic.
	proxy.waitForReady(t)
	mcpClient := proxy.connect(t, "streamable-http", "remote")
	if got := callToolText(t, mcpClient, "echo", map[string]any{"message": "late"}); got != "downstream:late" {
		t.Errorf("echo after late connect = %q, want %q", got, "downstream:late")
	}
}

// A backend that drops after connecting has to be rebuilt in place, so the
// endpoint keeps serving from the same route instead of staying degraded until
// a restart.
func TestAutoReconnectRebuildsDroppedBackend(t *testing.T) {
	skipShort(t)
	t.Parallel()

	downstreamAddr := freeAddr(t)
	downstreamURL := "http://" + downstreamAddr + "/mcp"
	configPath, addr := autoReconnectConfig(t, downstreamURL,
		`{"autoReconnect": true, "reconnectInterval": "100ms"}`)

	downstream := startDownstreamOnAddr(t, downstreamAddr, "streamable-http")
	proxy := startProxy(t, configPath, addr)

	mcpClient := proxy.connect(t, "streamable-http", "remote")
	if got := callToolText(t, mcpClient, "echo", map[string]any{"message": "before"}); got != "downstream:before" {
		t.Fatalf("echo before the drop = %q, want %q", got, "downstream:before")
	}

	// Kill the backend, then take its place on the same address. The proxy
	// retries on the configured interval, so the route recovers on its own.
	downstream.CloseClientConnections()
	downstream.Close()
	downstream = startDownstreamOnAddr(t, downstreamAddr, "streamable-http")
	defer downstream.Close()

	proxy.waitForReady(t)
	reconnected := proxy.connect(t, "streamable-http", "remote")
	if got := callToolText(t, reconnected, "echo", map[string]any{"message": "after"}); got != "downstream:after" {
		t.Errorf("echo after reconnect = %q, want %q", got, "downstream:after")
	}
}

// A backend that is retried while down must not emit an ERROR per attempt: an
// unreachable backend is the expected state the retry exists for, and a false
// ERROR on every tick would fire alerting and bury real failures.
func TestAutoReconnectRetriesWithoutErrorLogging(t *testing.T) {
	skipShort(t)
	t.Parallel()

	// Nothing listens here, so every attempt fails and is retried.
	dead := freeAddr(t)
	addr := freeAddr(t)
	configPath := writeConfig(t, fmt.Sprintf(`{
  "mcpProxy": {
    "baseURL": "http://%[1]s", "addr": "%[1]s", "name": "p", "version": "1",
    "type": "streamable-http", "startupGracePeriod": "1s",
    "options": {"pingInterval": "`+testPingInterval+`"}
  },
  "mcpServers": {
    "remote": {
      "transportType": "streamable-http",
      "url": "http://%[2]s/mcp",
      "options": {"autoReconnect": true, "reconnectInterval": "100ms"}
    }
  }
}`, addr, dead))

	proxy := launchProxy(t, configPath, addr)

	// Wait for the proxy to be serving, then let it fail and retry several times.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := proxy.ready(t); code != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1 * time.Second)

	logs := proxy.stderr.String()
	retries := strings.Count(logs, "Retrying connection")
	startErrors := strings.Count(logs, "Failed to start client")
	if retries == 0 {
		t.Fatalf("expected at least one retry, got none\n%s", logs)
	}
	if startErrors != 0 {
		t.Errorf("retrying a down backend logged %d \"Failed to start client\" ERROR(s), want 0\n%s", startErrors, logs)
	}
}

// A server that opted out of autoReconnect and is down at startup is a terminal
// failure, so it must still be logged as an error exactly once.
func TestAutoReconnectOffStillLogsStartupError(t *testing.T) {
	skipShort(t)
	t.Parallel()

	dead := freeAddr(t)
	addr := freeAddr(t)
	configPath := writeConfig(t, fmt.Sprintf(`{
  "mcpProxy": {
    "baseURL": "http://%[1]s", "addr": "%[1]s", "name": "p", "version": "1",
    "type": "streamable-http", "startupGracePeriod": "1s"
  },
  "mcpServers": {
    "remote": {"transportType": "streamable-http", "url": "http://%[2]s/mcp"}
  }
}`, addr, dead))

	proxy := launchProxy(t, configPath, addr)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := proxy.ready(t); code != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)

	logs := proxy.stderr.String()
	if !strings.Contains(logs, "Failed to start client") {
		t.Errorf("a terminal startup failure was not logged as an error\n%s", logs)
	}
	if strings.Contains(logs, "Retrying connection") {
		t.Errorf("autoReconnect is off, so nothing should be retried\n%s", logs)
	}
}
