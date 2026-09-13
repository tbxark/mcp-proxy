package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// A stdio downstream that accepts a tool call and never answers must not be
// able to wedge the proxy.
//
// sse and streamable-http clients are built with a response timeout taken from
// mcpServers.<name>.timeout, but the stdio branch of newMCPClient passes no
// options at all, so a stdio call inherits only the caller's context. When the
// caller is patient (or its own deadline is long), the forwarded request waits
// forever: the single stdio channel stays occupied, the keepalive ping can no
// longer get a reply, and after pingFailureThreshold probes the client is
// marked unhealthy and stays that way until the process is restarted. Every
// caller of that server loses it, not just the one that made the bad call.
func TestStdioToolCallIsBounded(t *testing.T) {
	skipShort(t)
	t.Parallel()

	configPath, addr := stdioConfig(t, "streamable-http")
	proxy := startProxy(t, configPath, addr)
	mcpClient := proxy.connect(t, "streamable-http", "fixture")

	// A tool call that never gets answered downstream must still return to the
	// caller. Without a stdio deadline this blocks until the context expires,
	// so the assertion is that it comes back well before that.
	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := mcp.CallToolRequest{}
	req.Params.Name = "hang"

	start := time.Now()
	_, err := mcpClient.CallTool(callCtx, req)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("CallTool(hang) succeeded, want an error: the downstream never answers")
	}
	if elapsed > 25*time.Second {
		t.Errorf("CallTool(hang) took %v, want it bounded well under the caller's 30s deadline", elapsed)
	}

	// The point of the bound: the server survives. A wedged stdio channel takes
	// out every later call, which is the difference between one failed request
	// and an outage for all clients of this downstream. ListTools is a real
	// round-trip; Ping would not be one, since the protocol removed it and a
	// modern client answers it without sending anything.
	listCtx, listCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer listCancel()
	if _, err := mcpClient.ListTools(listCtx, mcp.ListToolsRequest{}); err != nil {
		t.Fatalf("ListTools after a hung tool call: %v; the downstream is unusable", err)
	}

	// And a normal call still works, so the recovery is real rather than the
	// channel merely accepting new writes.
	echoCtx, echoCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer echoCancel()
	echo := mcp.CallToolRequest{}
	echo.Params.Name = "echo"
	echo.Params.Arguments = map[string]any{"message": "alive"}
	if _, err := mcpClient.CallTool(echoCtx, echo); err != nil {
		t.Fatalf("echo after a hung tool call: %v; the downstream never recovered", err)
	}
}
