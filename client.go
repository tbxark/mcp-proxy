package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type Client struct {
	name            string
	needPing        bool
	needManualStart bool
	client          *client.Client
	options         *OptionsV2
}

func newMCPClient(name string, conf *MCPClientConfigV2) (*Client, error) {
	clientInfo, pErr := parseMCPClientConfigV2(conf)
	if pErr != nil {
		return nil, pErr
	}
	switch v := clientInfo.(type) {
	case *StdioMCPClientConfig:
		envs := make([]string, 0, len(v.Env))
		for kk, vv := range v.Env {
			envs = append(envs, fmt.Sprintf("%s=%s", kk, vv))
		}
		mcpClient, err := client.NewStdioMCPClient(v.Command, envs, v.Args...)
		if err != nil {
			return nil, err
		}

		return &Client{
			name:    name,
			client:  mcpClient,
			options: conf.Options,
		}, nil
	case *SSEMCPClientConfig:
		if v.OAuth != nil {
			oc, oErr := buildOAuthConfig(name, v.OAuth)
			if oErr != nil {
				return nil, oErr
			}
			var options []transport.ClientOption
			if len(v.Headers) > 0 {
				options = append(options, client.WithHeaders(v.Headers))
			}
			mcpClient, err := client.NewOAuthSSEClient(v.URL, oc, options...)
			if err != nil {
				return nil, err
			}
			return &Client{
				name:            name,
				needPing:        true,
				needManualStart: true,
				client:          mcpClient,
				options:         conf.Options,
			}, nil
		}
		var options []transport.ClientOption
		if len(v.Headers) > 0 {
			options = append(options, client.WithHeaders(v.Headers))
		}
		mcpClient, err := client.NewSSEMCPClient(v.URL, options...)
		if err != nil {
			return nil, err
		}
		return &Client{
			name:            name,
			needPing:        true,
			needManualStart: true,
			client:          mcpClient,
			options:         conf.Options,
		}, nil
	case *StreamableMCPClientConfig:
		if v.OAuth != nil {
			oc, oErr := buildOAuthConfig(name, v.OAuth)
			if oErr != nil {
				return nil, oErr
			}
			var options []transport.StreamableHTTPCOption
			if len(v.Headers) > 0 {
				options = append(options, transport.WithHTTPHeaders(v.Headers))
			}
			if v.Timeout > 0 {
				options = append(options, transport.WithHTTPTimeout(v.Timeout))
			}
			mcpClient, err := client.NewOAuthStreamableHttpClient(v.URL, oc, options...)
			if err != nil {
				return nil, err
			}
			return &Client{
				name:            name,
				needPing:        true,
				needManualStart: true,
				client:          mcpClient,
				options:         conf.Options,
			}, nil
		}
		var options []transport.StreamableHTTPCOption
		if len(v.Headers) > 0 {
			options = append(options, transport.WithHTTPHeaders(v.Headers))
		}
		if v.Timeout > 0 {
			options = append(options, transport.WithHTTPTimeout(v.Timeout))
		}
		mcpClient, err := client.NewStreamableHttpClient(v.URL, options...)
		if err != nil {
			return nil, err
		}
		return &Client{
			name:            name,
			needPing:        true,
			needManualStart: true,
			client:          mcpClient,
			options:         conf.Options,
		}, nil
	}
	return nil, errors.New("invalid client type")
}

func (c *Client) addToMCPServer(ctx context.Context, clientInfo mcp.Implementation, mcpServer *server.MCPServer) error {
	if c.needManualStart {
		err := c.client.Start(ctx)
		if err != nil {
			return oauthAwareError(c.name, err)
		}
	}
	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = clientInfo
	initRequest.Params.Capabilities = mcp.ClientCapabilities{
		Experimental: make(map[string]any),
		Roots:        nil,
		Sampling:     nil,
	}
	_, err := c.client.Initialize(ctx, initRequest)
	if err != nil {
		return oauthAwareError(c.name, err)
	}
	slog.Info("Successfully initialized MCP client", "client", c.name)

	err = c.addToolsToServer(ctx, mcpServer)
	if err != nil {
		return err
	}
	_ = c.addPromptsToServer(ctx, mcpServer)
	_ = c.addResourcesToServer(ctx, mcpServer)
	_ = c.addResourceTemplatesToServer(ctx, mcpServer)

	if c.needPing {
		go c.startPingTask(ctx)
	}
	return nil
}

func (c *Client) startPingTask(ctx context.Context) {
	interval := 30 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	failCount := 0
	for {
		select {
		case <-ctx.Done():
			slog.Debug("Context done, stopping ping", "client", c.name)
			return
		case <-ticker.C:
			if err := c.client.Ping(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				failCount++
				slog.Warn("MCP ping failed", "client", c.name, "err", err, "failures", failCount)
			} else if failCount > 0 {
				slog.Info("MCP ping recovered", "client", c.name, "failures", failCount)
				failCount = 0
			}
		}
	}
}

func (c *Client) addToolsToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	toolsRequest := mcp.ListToolsRequest{}
	filterFunc := func(toolName string) bool {
		return true
	}

	if c.options != nil && c.options.ToolFilter != nil && len(c.options.ToolFilter.List) > 0 {
		filterSet := make(map[string]struct{})
		mode := ToolFilterMode(strings.ToLower(string(c.options.ToolFilter.Mode)))
		for _, toolName := range c.options.ToolFilter.List {
			filterSet[toolName] = struct{}{}
		}
		switch mode {
		case ToolFilterModeAllow:
			filterFunc = func(toolName string) bool {
				_, inList := filterSet[toolName]
				if !inList {
					slog.Debug("Ignoring tool not in allow list", "client", c.name, "tool", toolName)
				}
				return inList
			}
		case ToolFilterModeBlock:
			filterFunc = func(toolName string) bool {
				_, inList := filterSet[toolName]
				if inList {
					slog.Debug("Ignoring tool in block list", "client", c.name, "tool", toolName)
				}
				return !inList
			}
		default:
			slog.Warn("Unknown tool filter mode, skipping tool filter", "client", c.name, "mode", mode)
		}
	}

	for {
		tools, err := c.client.ListTools(ctx, toolsRequest)
		if err != nil {
			return err
		}
		if tools == nil {
			return fmt.Errorf("<%s> ListTools returned nil response without error", c.name)
		}
		if len(tools.Tools) == 0 {
			break
		}
		slog.Debug("Successfully listed tools", "client", c.name, "count", len(tools.Tools))
		for _, tool := range tools.Tools {
			if filterFunc(tool.Name) {
				slog.Debug("Adding tool", "client", c.name, "tool", tool.Name)
				toolName := tool.Name
				mcpServer.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					result, err := c.client.CallTool(ctx, request)
					if err != nil {
						slog.Error("Tool call failed", "client", c.name, "tool", toolName, "error", err)
					}
					return result, err
				})
			}
		}
		if tools.NextCursor == "" {
			break
		}
		toolsRequest.Params.Cursor = tools.NextCursor
	}

	return nil
}

func (c *Client) addPromptsToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	promptsRequest := mcp.ListPromptsRequest{}
	for {
		prompts, err := c.client.ListPrompts(ctx, promptsRequest)
		if err != nil {
			return err
		}
		if prompts == nil {
			return fmt.Errorf("<%s> ListPrompts returned nil response without error", c.name)
		}
		if len(prompts.Prompts) == 0 {
			break
		}
		slog.Debug("Successfully listed prompts", "client", c.name, "count", len(prompts.Prompts))
		for _, prompt := range prompts.Prompts {
			slog.Debug("Adding prompt", "client", c.name, "prompt", prompt.Name)
			mcpServer.AddPrompt(prompt, c.client.GetPrompt)
		}
		if prompts.NextCursor == "" {
			break
		}
		promptsRequest.Params.Cursor = prompts.NextCursor
	}
	return nil
}

func (c *Client) addResourcesToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	resourcesRequest := mcp.ListResourcesRequest{}
	for {
		resources, err := c.client.ListResources(ctx, resourcesRequest)
		if err != nil {
			return err
		}
		if resources == nil {
			return fmt.Errorf("<%s> ListResources returned nil response without error", c.name)
		}
		if len(resources.Resources) == 0 {
			break
		}
		slog.Debug("Successfully listed resources", "client", c.name, "count", len(resources.Resources))
		for _, resource := range resources.Resources {
			slog.Debug("Adding resource", "client", c.name, "resource", resource.Name)
			mcpServer.AddResource(resource, func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				readResource, e := c.client.ReadResource(ctx, request)
				if e != nil {
					return nil, e
				}
				return readResource.Contents, nil
			})
		}
		if resources.NextCursor == "" {
			break
		}
		resourcesRequest.Params.Cursor = resources.NextCursor

	}
	return nil
}

func (c *Client) addResourceTemplatesToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	resourceTemplatesRequest := mcp.ListResourceTemplatesRequest{}
	for {
		resourceTemplates, err := c.client.ListResourceTemplates(ctx, resourceTemplatesRequest)
		if err != nil {
			return err
		}
		if resourceTemplates == nil || len(resourceTemplates.ResourceTemplates) == 0 {
			break
		}
		slog.Debug("Successfully listed resource templates", "client", c.name, "count", len(resourceTemplates.ResourceTemplates))
		for _, resourceTemplate := range resourceTemplates.ResourceTemplates {
			slog.Debug("Adding resource template", "client", c.name, "template", resourceTemplate.Name)
			mcpServer.AddResourceTemplate(resourceTemplate, func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				readResource, e := c.client.ReadResource(ctx, request)
				if e != nil {
					return nil, e
				}
				return readResource.Contents, nil
			})
		}
		if resourceTemplates.NextCursor == "" {
			break
		}
		resourceTemplatesRequest.Params.Cursor = resourceTemplates.NextCursor
	}
	return nil
}

func (c *Client) Close() error {
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

type Server struct {
	tokens    []string
	mcpServer *server.MCPServer
	handler   http.Handler
}

func newMCPServer(name string, serverConfig *MCPProxyConfigV2, clientConfig *MCPClientConfigV2) (*Server, error) {
	if serverConfig == nil {
		return nil, errors.New("server config is required")
	}
	if clientConfig == nil {
		return nil, errors.New("client config is required")
	}
	clientOptions := clientConfig.Options
	if clientOptions == nil {
		clientOptions = &OptionsV2{}
	}
	serverOpts := []server.ServerOption{
		server.WithResourceCapabilities(true, true),
		server.WithRecovery(),
	}

	if clientOptions.LogEnabled.OrElse(false) {
		serverOpts = append(serverOpts, server.WithLogging())
	}
	mcpServer := server.NewMCPServer(
		name,
		serverConfig.Version,
		serverOpts...,
	)

	var handler http.Handler

	switch serverConfig.Type {
	case MCPServerTypeSSE:
		handler = server.NewSSEServer(
			mcpServer,
			server.WithStaticBasePath(name),
			server.WithBaseURL(serverConfig.BaseURL),
		)
	case MCPServerTypeStreamable:
		handler = server.NewStreamableHTTPServer(
			mcpServer,
			server.WithStateLess(true),
		)
	default:
		return nil, fmt.Errorf("unknown server type: %s", serverConfig.Type)
	}
	srv := &Server{
		mcpServer: mcpServer,
		handler:   handler,
	}

	if len(clientOptions.AuthTokens) > 0 {
		srv.tokens = clientOptions.AuthTokens
	}

	return srv, nil
}
