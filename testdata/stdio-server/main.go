// Command stdio-server is a small but complete MCP server spoken over stdio,
// used as the downstream server in the end-to-end proxy test. It registers
// enough tools, prompts, resources and resource templates to exercise every
// path mcp-proxy copies onto its own server, and its pagination limit is small
// enough to force the proxy's cursor loops to run more than one iteration.
//
// It lives under testdata/ so `go build ./...` and the linters skip it; the
// test builds it by explicit path. It can also be run by hand for manual
// testing: `go run ./testdata/stdio-server` and speak JSON-RPC on stdin.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// paginationLimit is deliberately smaller than the number of registered tools
// and prompts so that ListTools/ListPrompts return a NextCursor.
const paginationLimit = 2

func main() {
	if err := server.ServeStdio(newServer()); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "stdio-server: %v\n", err)
		os.Exit(1)
	}
}

func newServer() *server.MCPServer {
	s := server.NewMCPServer(
		"stdio-test-server",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithPromptCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithPaginationLimit(paginationLimit),
	)
	addTools(s)
	addPrompts(s)
	addResources(s)
	return s
}

func addTools(s *server.MCPServer) {
	s.AddTool(
		mcp.NewTool("echo",
			mcp.WithDescription("Echo the given message back"),
			mcp.WithString("message", mcp.Required()),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			message, err := request.RequireString("message")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(message), nil
		},
	)

	s.AddTool(
		mcp.NewTool("add",
			mcp.WithDescription("Add two numbers"),
			mcp.WithNumber("a", mcp.Required()),
			mcp.WithNumber("b", mcp.Required()),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			a, err := request.RequireFloat("a")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, err := request.RequireFloat("b")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("%v", a+b)), nil
		},
	)

	// hang accepts the request and then never answers, which is what a
	// single-threaded stdio server looks like when a tool call wedges. The
	// proxy must bound the wait itself; the client cannot, because a stdio
	// downstream has no transport-level deadline of its own.
	s.AddTool(
		mcp.NewTool("hang",
			mcp.WithDescription("Accept the call and never respond"),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	)

	// noisy_stderr writes more than one pipe buffer's worth of stderr in a
	// single call. Nothing reads a stdio subprocess's stderr, so once the pipe
	// fills, write(2) blocks and the server stops answering entirely.
	s.AddTool(
		mcp.NewTool("noisy_stderr",
			mcp.WithDescription("Write the given number of kilobytes to stderr"),
			mcp.WithNumber("kilobytes", mcp.Required()),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			kb, err := request.RequireFloat("kilobytes")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			line := strings.Repeat("x", 1023) + "\n"
			for range int(kb) {
				if _, err := os.Stderr.WriteString(line); err != nil {
					return mcp.NewToolResultError(err.Error()), nil
				}
			}
			return mcp.NewToolResultText(fmt.Sprintf("wrote %dKB to stderr", int(kb))), nil
		},
	)

	// getenv reports a variable from this process's environment, so the test can
	// confirm mcpServers.<name>.env reached the subprocess.
	s.AddTool(
		mcp.NewTool("getenv",
			mcp.WithDescription("Return the value of an environment variable"),
			mcp.WithString("name", mcp.Required()),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name, err := request.RequireString("name")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(os.Getenv(name)), nil
		},
	)

	// pid lets the lifecycle test kill exactly this subprocess.
	s.AddTool(
		mcp.NewTool("pid", mcp.WithDescription("Return this server's process ID")),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(strconv.Itoa(os.Getpid())), nil
		},
	)

	// fail returns an is-error tool result, the in-band failure MCP defines for
	// tools; the proxy must forward it verbatim rather than turn it into a
	// transport error.
	s.AddTool(
		mcp.NewTool("fail", mcp.WithDescription("Always fail with a tool error")),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("intentional tool failure"), nil
		},
	)

	// blocked exists only to be excluded by the proxy's tool filter.
	s.AddTool(
		mcp.NewTool("blocked", mcp.WithDescription("Should be filtered out by the proxy")),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("should never be reachable through the proxy"), nil
		},
	)
}

func addPrompts(s *server.MCPServer) {
	s.AddPrompt(
		mcp.NewPrompt("greeting",
			mcp.WithArgument("name"),
		),
		func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			name := request.Params.Arguments["name"]
			if name == "" {
				name = "world"
			}
			return mcp.NewGetPromptResult(
				"A greeting prompt",
				[]mcp.PromptMessage{
					mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent("Hello, "+name+"!")),
				},
			), nil
		},
	)

	s.AddPrompt(
		mcp.NewPrompt("summarize"),
		func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return mcp.NewGetPromptResult(
				"A summarization prompt",
				[]mcp.PromptMessage{
					mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent("Summarize the text above.")),
				},
			), nil
		},
	)

	s.AddPrompt(
		mcp.NewPrompt("translate", mcp.WithArgument("lang")),
		func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			lang := request.Params.Arguments["lang"]
			if lang == "" {
				lang = "english"
			}
			return mcp.NewGetPromptResult(
				"A translation prompt",
				[]mcp.PromptMessage{
					mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent("Translate into "+lang+".")),
				},
			), nil
		},
	)
}

func addResources(s *server.MCPServer) {
	s.AddResource(
		mcp.NewResource("test://static/readme", "readme", mcp.WithMIMEType("text/plain")),
		func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      request.Params.URI,
					MIMEType: "text/plain",
					Text:     "static readme contents",
				},
			}, nil
		},
	)

	s.AddResource(
		mcp.NewResource("test://static/config", "config", mcp.WithMIMEType("application/json")),
		func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      request.Params.URI,
					MIMEType: "application/json",
					Text:     `{"static":true}`,
				},
			}, nil
		},
	)

	s.AddResource(
		mcp.NewResource("test://static/notes", "notes", mcp.WithMIMEType("text/plain")),
		func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      request.Params.URI,
					MIMEType: "text/plain",
					Text:     "some notes",
				},
			}, nil
		},
	)

	s.AddResourceTemplate(
		mcp.NewResourceTemplate("test://echo/{word}", "echo-resource",
			mcp.WithTemplateDescription("Echoes back the word in the URI")),
		func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			word := strings.TrimPrefix(request.Params.URI, "test://echo/")
			return []mcp.ResourceContents{
				mcp.TextResourceContents{
					URI:      request.Params.URI,
					MIMEType: "text/plain",
					Text:     "echo:" + word,
				},
			}, nil
		},
	)
}
