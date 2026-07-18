package main

import (
	"net"
	"strings"
	"testing"
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
