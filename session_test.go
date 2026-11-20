package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	optional "github.com/tbxark/optional-go"
)

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
