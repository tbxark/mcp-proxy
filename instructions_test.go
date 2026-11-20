package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func initializeInstructions(t *testing.T, s *server.MCPServer) string {
	t.Helper()
	msg := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	raw, err := json.Marshal(s.HandleMessage(context.Background(), msg))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out.Result.Instructions
}

func TestForwardInstructionsFromBackend(t *testing.T) {
	s := server.NewMCPServer("beis", "1.0.0")
	forwardInstructions(s, &mcp.InitializeResult{Instructions: "BEIS house lessons: ..."})
	if got := initializeInstructions(t, s); got != "BEIS house lessons: ..." {
		t.Fatalf("instructions = %q", got)
	}
}

func TestForwardInstructionsIgnoresEmptyAndNil(t *testing.T) {
	s := server.NewMCPServer("beis", "1.0.0", server.WithInstructions("keep"))
	forwardInstructions(s, nil)
	forwardInstructions(s, &mcp.InitializeResult{})
	if got := initializeInstructions(t, s); got != "keep" {
		t.Fatalf("instructions = %q", got)
	}
}
