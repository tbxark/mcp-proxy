package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The recover middleware is listed first so it wraps the others: a panic in
// auth or logging must still return 500 rather than kill the connection.
func TestChainMiddlewareRunsFirstOutermost(t *testing.T) {
	t.Parallel()

	var order []string
	trace := func(name string) MiddlewareFunc {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	handler := chainMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), trace("first"), trace("second"))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got := strings.Join(order, ","); got != "first,second,handler" {
		t.Fatalf("middleware order = %q, want %q", got, "first,second,handler")
	}
}

func TestChainMiddlewareRecoversFromPanicInLaterMiddleware(t *testing.T) {
	t.Parallel()

	panicking := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		})
	}
	handler := chainMiddleware(http.NotFoundHandler(), recoverMiddleware("test"), panicking)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

func TestAuthMiddleware(t *testing.T) {
	t.Parallel()

	handler := newAuthMiddleware([]string{"secret", "second"})(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{name: "canonical scheme", header: "Bearer secret", want: http.StatusTeapot},
		// RFC 7235: the scheme name is case-insensitive.
		{name: "lowercase scheme", header: "bearer secret", want: http.StatusTeapot},
		{name: "uppercase scheme", header: "BEARER secret", want: http.StatusTeapot},
		{name: "second token", header: "Bearer second", want: http.StatusTeapot},
		{name: "surrounding whitespace", header: "  Bearer   secret  ", want: http.StatusTeapot},
		// Accepting a bare token keeps older clients working.
		{name: "no scheme", header: "secret", want: http.StatusTeapot},
		{name: "wrong token", header: "Bearer nope", want: http.StatusUnauthorized},
		{name: "scheme only", header: "Bearer ", want: http.StatusUnauthorized},
		{name: "missing header", header: "", want: http.StatusUnauthorized},
		{name: "token prefix only", header: "Bearer sec", want: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != tt.want {
				t.Errorf("Authorization %q -> %d, want %d", tt.header, recorder.Code, tt.want)
			}
		})
	}
}

func TestHealthHandlerReadiness(t *testing.T) {
	t.Parallel()

	config := &Config{
		McpProxy: &MCPProxyConfigV2{Name: "test", Version: "test"},
		McpServers: map[string]*MCPClientConfigV2{
			"enabled":  {Command: "server", Options: &OptionsV2{}},
			"disabled": {Command: "server", Options: &OptionsV2{Disabled: true}},
		},
	}

	report := readinessReport{}
	readyz := healthHandler(config, func() readinessReport { return report })
	request := httptest.NewRequest(http.MethodGet, "/_readyz", nil)

	get := func() (int, string) {
		recorder := httptest.NewRecorder()
		readyz(recorder, request)
		return recorder.Code, recorder.Body.String()
	}

	if code, _ := get(); code != http.StatusServiceUnavailable {
		t.Fatalf("status before init = %d, want %d", code, http.StatusServiceUnavailable)
	}

	// Initialization finished, but nothing connected: every MCP route 404s, so
	// this is not a proxy a load balancer should send traffic to.
	report.started = true
	if code, body := get(); code != http.StatusServiceUnavailable || !strings.Contains(body, `"status":"unavailable"`) {
		t.Fatalf("status with no client mounted = %d %s, want %d and unavailable", code, body, http.StatusServiceUnavailable)
	}

	report.mounted = 1
	code, body := get()
	if code != http.StatusOK {
		t.Fatalf("status after init = %d, want %d", code, http.StatusOK)
	}
	if !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, `"serverCount":1`) {
		t.Fatalf("body = %s, want status ok and only the enabled server counted", body)
	}

	// A downstream that broke after startup takes the proxy out of rotation
	// and names itself in the report.
	report.unhealthy = []string{"enabled"}
	code, body = get()
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status with an unhealthy client = %d, want %d", code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(body, `"status":"degraded"`) || !strings.Contains(body, `"unhealthy":["enabled"]`) {
		t.Fatalf("body = %s, want degraded with the client named", body)
	}

	// Liveness never blocks on client health.
	recorder := httptest.NewRecorder()
	healthHandler(config, nil)(recorder, httptest.NewRequest(http.MethodGet, "/_healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d", recorder.Code, http.StatusOK)
	}
}

func TestStartHTTPServerReturnsListenError(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	defer func() { _ = listener.Close() }()

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
