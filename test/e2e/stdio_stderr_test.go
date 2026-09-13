package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// A stdio downstream that writes a lot to stderr must not deadlock.
//
// Start() gives the subprocess a StderrPipe and nothing ever reads it: mcp-go
// only exposes it via Stderr(), and mcp-proxy never calls that. An OS pipe holds
// about 64KB, so a server that logs to stderr keeps working until the buffer
// fills and then blocks forever inside write(2) — with no error anywhere, since
// from the proxy's side the server has merely gone quiet. The single stdio
// channel is then stuck: the keepalive ping stops being answered and the whole
// downstream is dropped as unhealthy for every caller until a restart.
//
// This needs no misbehaving tool and no timeout to trigger; ordinary logging is
// enough. The fixture's noisy_stderr tool writes past the pipe capacity in one
// call, which is the same thing a chatty real server does over its lifetime.
func TestStdioStderrDoesNotDeadlock(t *testing.T) {
	skipShort(t)
	t.Parallel()

	configPath, addr := stdioConfig(t, "streamable-http")
	proxy := startProxy(t, configPath, addr)
	mcpClient := proxy.connect(t, "streamable-http", "fixture")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Ask for more than one pipe buffer's worth of stderr. Without a drain the
	// subprocess blocks mid-write and never answers this call.
	noisy := mcp.CallToolRequest{}
	noisy.Params.Name = "noisy_stderr"
	noisy.Params.Arguments = map[string]any{"kilobytes": float64(256)}
	if _, err := mcpClient.CallTool(ctx, noisy); err != nil {
		t.Fatalf("CallTool(noisy_stderr): %v; the subprocess is wedged writing to an unread stderr pipe", err)
	}

	// The channel must still be usable afterwards. ListTools is a real
	// round-trip; Ping would not be one, since the protocol removed it and a
	// modern client answers it without sending anything.
	listCtx, listCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer listCancel()
	if _, err := mcpClient.ListTools(listCtx, mcp.ListToolsRequest{}); err != nil {
		t.Fatalf("ListTools after heavy stderr output: %v", err)
	}

	echoCtx, echoCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer echoCancel()
	echo := mcp.CallToolRequest{}
	echo.Params.Name = "echo"
	echo.Params.Arguments = map[string]any{"message": "alive"}
	if _, err := mcpClient.CallTool(echoCtx, echo); err != nil {
		t.Fatalf("echo after heavy stderr output: %v", err)
	}
}
