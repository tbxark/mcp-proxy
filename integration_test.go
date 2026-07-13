package main

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/tbxark/optional-go"
)

func TestAddToMCPServer_WithTestServer(t *testing.T) {
	mcpSrv := server.NewMCPServer(
		"test-server",
		"1.0.0",
		server.WithResourceCapabilities(true, true),
	)

	mcpSrv.AddTool(mcp.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				mcp.TextContent{Type: "text", Text: "test result"},
			},
		}, nil
	})

	mcpSrv.AddPrompt(mcp.Prompt{
		Name:        "test_prompt",
		Description: "A test prompt",
	}, func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Description: "Test prompt",
			Messages: []mcp.PromptMessage{
				{
					Role: mcp.RoleAssistant,
					Content: mcp.TextContent{
						Type: "text",
						Text: "test prompt content",
					},
				},
			},
		}, nil
	})

	mcpSrv.AddResource(mcp.Resource{
		Name:     "test_resource",
		URI:      "test://resource",
		MIMEType: "text/plain",
	}, func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      "test://resource",
				MIMEType: "text/plain",
				Text:     "test resource content",
			},
		}, nil
	})

	testSrv := server.NewTestServer(mcpSrv)
	defer testSrv.Close()

	clientConfig := &MCPClientConfigV2{
		TransportType: MCPClientTypeSSE,
		URL:           testSrv.URL + "/sse",
		Options: &OptionsV2{
			LogEnabled: optional.NewField(false),
		},
	}

	client, err := newMCPClient("test", clientConfig)
	if err != nil {
		t.Fatalf("newMCPClient() error = %v", err)
	}
	defer client.Close()

	proxySrv := server.NewMCPServer(
		"proxy-server",
		"1.0.0",
		server.WithResourceCapabilities(true, true),
	)

	info := mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = client.addToMCPServer(ctx, info, proxySrv)
	if err != nil {
		t.Fatalf("addToMCPServer() error = %v", err)
	}
}

func TestAddToMCPServer_WithToolFilterAllow(t *testing.T) {
	mcpSrv := server.NewMCPServer(
		"test-server",
		"1.0.0",
		server.WithResourceCapabilities(true, true),
	)

	mcpSrv.AddTool(mcp.Tool{
		Name:        "allowed_tool",
		Description: "An allowed tool",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})

	mcpSrv.AddTool(mcp.Tool{
		Name:        "blocked_tool",
		Description: "A blocked tool",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})

	testSrv := server.NewTestServer(mcpSrv)
	defer testSrv.Close()

	clientConfig := &MCPClientConfigV2{
		TransportType: MCPClientTypeSSE,
		URL:           testSrv.URL + "/sse",
		Options: &OptionsV2{
			ToolFilter: &ToolFilterConfig{
				Mode: ToolFilterModeAllow,
				List: []string{"allowed_tool"},
			},
		},
	}

	client, err := newMCPClient("test", clientConfig)
	if err != nil {
		t.Fatalf("newMCPClient() error = %v", err)
	}
	defer client.Close()

	proxySrv := server.NewMCPServer(
		"proxy-server",
		"1.0.0",
		server.WithResourceCapabilities(true, true),
	)

	info := mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = client.addToMCPServer(ctx, info, proxySrv)
	if err != nil {
		t.Fatalf("addToMCPServer() error = %v", err)
	}
}

func TestAddToMCPServer_WithToolFilterBlock(t *testing.T) {
	mcpSrv := server.NewMCPServer(
		"test-server",
		"1.0.0",
		server.WithResourceCapabilities(true, true),
	)

	mcpSrv.AddTool(mcp.Tool{
		Name:        "tool1",
		Description: "Tool 1",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})

	mcpSrv.AddTool(mcp.Tool{
		Name:        "tool2",
		Description: "Tool 2",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})

	testSrv := server.NewTestServer(mcpSrv)
	defer testSrv.Close()

	clientConfig := &MCPClientConfigV2{
		TransportType: MCPClientTypeSSE,
		URL:           testSrv.URL + "/sse",
		Options: &OptionsV2{
			ToolFilter: &ToolFilterConfig{
				Mode: ToolFilterModeBlock,
				List: []string{"tool2"},
			},
		},
	}

	client, err := newMCPClient("test", clientConfig)
	if err != nil {
		t.Fatalf("newMCPClient() error = %v", err)
	}
	defer client.Close()

	proxySrv := server.NewMCPServer(
		"proxy-server",
		"1.0.0",
		server.WithResourceCapabilities(true, true),
	)

	info := mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = client.addToMCPServer(ctx, info, proxySrv)
	if err != nil {
		t.Fatalf("addToMCPServer() error = %v", err)
	}
}

func TestAddToMCPServer_EmptyServer(t *testing.T) {
	mcpSrv := server.NewMCPServer(
		"test-server",
		"1.0.0",
		server.WithToolCapabilities(true),
	)

	testSrv := server.NewTestServer(mcpSrv)
	defer testSrv.Close()

	clientConfig := &MCPClientConfigV2{
		TransportType: MCPClientTypeSSE,
		URL:           testSrv.URL + "/sse",
		Options:       &OptionsV2{},
	}

	client, err := newMCPClient("test", clientConfig)
	if err != nil {
		t.Fatalf("newMCPClient() error = %v", err)
	}
	defer client.Close()

	proxySrv := server.NewMCPServer(
		"proxy-server",
		"1.0.0",
		server.WithResourceCapabilities(true, true),
	)

	info := mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = client.addToMCPServer(ctx, info, proxySrv)
	if err != nil {
		t.Fatalf("addToMCPServer() error = %v", err)
	}
}
