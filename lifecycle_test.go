package main

import (
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

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
