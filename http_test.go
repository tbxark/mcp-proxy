package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestStartHTTPServerReturnsListenError(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	defer listener.Close()

	config := &Config{
		McpProxy: &MCPProxyConfigV2{
			BaseURL: "http://" + listener.Addr().String(),
			Addr:    listener.Addr().String(),
			Name:    "test",
			Version: "test",
			Type:    MCPServerTypeStreamable,
			Options: &OptionsV2{},
		},
		McpServers: map[string]*MCPClientConfigV2{},
	}

	err = startHTTPServer(config)
	if err == nil || !strings.Contains(err.Error(), "HTTP server failed") {
		t.Fatalf("startHTTPServer error = %v, want listen failure", err)
	}
}

// A server that fails to connect still gets its route mounted, responding
// with a clear 503 instead of a bare 404 - see docs/OAUTH_LIFECYCLE.md.
func TestConnectAndMountMountsNotReadyRouteOnFailure(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	clientConfig := &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           upstream.URL,
		Options:       &OptionsV2{},
	}
	mcpClient, err := newMCPClient("broken", clientConfig)
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer mcpClient.Close()

	proxyConfig := &MCPProxyConfigV2{
		Type:    MCPServerTypeStreamable,
		Version: "test",
	}
	srv, err := newMCPServer("broken", proxyConfig, clientConfig)
	if err != nil {
		t.Fatalf("newMCPServer: %v", err)
	}

	baseURL, err := url.Parse("http://example.com")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	httpMux := http.NewServeMux()

	route, connErr := connectAndMount(t.Context(), "broken", clientConfig, proxyConfig, mcp.Implementation{Name: "test"}, mcpClient, srv, baseURL, httpMux)
	if connErr == nil {
		t.Fatal("connectAndMount error = nil, want error from failed connect")
	}
	if route == nil {
		t.Fatal("connectAndMount route = nil, want a serverRoute mounted for the failed server")
	}

	req := httptest.NewRequest(http.MethodGet, "/broken/", nil)
	rec := httptest.NewRecorder()
	httpMux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "broken") {
		t.Fatalf("body = %q, want it to mention the server name", rec.Body.String())
	}
}

// The headline promise of docs/OAUTH_LIFECYCLE.md: once a server that
// failed to connect at startup becomes reachable, the very next request
// against its already-mounted route succeeds - no restart, no re-mount.
func TestServerRouteRecoversOnNextRequestOnceUpstreamWorks(t *testing.T) {
	t.Parallel()

	var authorized atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized.Load() {
			http.Error(w, "not authorized yet", http.StatusInternalServerError)
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
		w.Header().Set("Content-Type", "application/json")
		result := map[string]any{}
		if request.Method == string(mcp.MethodInitialize) {
			result = map[string]any{
				"protocolVersion": mcp.LATEST_PROTOCOL_VERSION,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "test", "version": "test"},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result":  result,
		})
	}))
	defer upstream.Close()

	clientConfig := &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           upstream.URL,
		Options:       &OptionsV2{},
	}
	proxyConfig := &MCPProxyConfigV2{
		Type:    MCPServerTypeStreamable,
		Version: "test",
	}
	baseURL, err := url.Parse("http://example.com")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	httpMux := http.NewServeMux()

	mcpClient, err := newMCPClient("retry", clientConfig)
	if err != nil {
		t.Fatalf("newMCPClient: %v", err)
	}
	defer mcpClient.Close()
	srv, err := newMCPServer("retry", proxyConfig, clientConfig)
	if err != nil {
		t.Fatalf("newMCPServer: %v", err)
	}

	route, connErr := connectAndMount(t.Context(), "retry", clientConfig, proxyConfig, mcp.Implementation{Name: "test"}, mcpClient, srv, baseURL, httpMux)
	if connErr == nil {
		t.Fatal("connectAndMount error = nil, want error before authorization")
	}
	if route == nil {
		t.Fatal("connectAndMount route = nil, want a serverRoute mounted for the failed server")
	}

	rec := httptest.NewRecorder()
	httpMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/retry/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before authorization: status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	// Simulate running `-authorize` in a separate process: the credential
	// the client uses now works. Nothing about the mounted route changes -
	// no re-mount, no restart.
	authorized.Store(true)

	// POST, not GET: a bare GET against the real StreamableHTTPServer opens
	// a long-lived SSE notification stream by design (streamable-http's
	// subscribe verb) and would hang the test waiting for it to close.
	// POST is the normal bounded request/response path.
	rec = httptest.NewRecorder()
	httpMux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/retry/", strings.NewReader("{}")))
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("after authorization: status = %d, want the route to have recovered, body: %s", rec.Code, rec.Body.String())
	}

	route.mu.RLock()
	ready := route.ready
	route.mu.RUnlock()
	if ready == nil {
		t.Fatal("route.ready is nil, want the retry to have populated it")
	}
}

// The stampede guard: a burst of concurrent requests against a not-yet-ready
// route must coalesce into a single connect attempt, not one per request -
// see docs/OAUTH_LIFECYCLE.md's discussion of why that matters (wasted
// duplicate work at best, tripping single-use refresh-token rotation at an
// upstream OAuth provider at worst).
func TestServerRouteCoalescesConcurrentConnectAttempts(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result := map[string]any{}
		if request.Method == string(mcp.MethodInitialize) {
			// Block here so every concurrent goroutine below is guaranteed
			// to have reached (and be waiting on) this one in-flight
			// attempt before any response lands - without this, a fast
			// round trip could complete before all goroutines even start,
			// making the test pass by luck even if coalescing were broken.
			attempts.Add(1)
			<-release
			result = map[string]any{
				"protocolVersion": mcp.LATEST_PROTOCOL_VERSION,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "test", "version": "test"},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result":  result,
		})
	}))
	defer upstream.Close()

	clientConfig := &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           upstream.URL,
		Options:       &OptionsV2{},
	}
	proxyConfig := &MCPProxyConfigV2{
		Type:    MCPServerTypeStreamable,
		Version: "test",
	}
	baseURL, err := url.Parse("http://example.com")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	httpMux := http.NewServeMux()
	route := newServerRoute(t.Context(), "concurrent", clientConfig, proxyConfig, mcp.Implementation{Name: "test"})
	httpMux.Handle(routeForServer(baseURL, "concurrent"), route)

	const n = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			httpMux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/concurrent/", strings.NewReader("{}")))
			codes[i] = rec.Code
		}(i)
	}
	close(start)
	time.Sleep(50 * time.Millisecond) // let every goroutine reach the block above
	close(release)
	wg.Wait()

	if got := attempts.Load(); got != 1 {
		t.Fatalf("upstream Initialize hits = %d, want exactly 1 (concurrent requests should coalesce into a single connect attempt)", got)
	}
	for i, code := range codes {
		if code == http.StatusServiceUnavailable {
			t.Errorf("request %d: status = %d, want the coalesced connect's result, not a not-ready response", i, code)
		}
	}
}
