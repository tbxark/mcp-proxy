package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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
			return err
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
		return err
	}
	log.Printf("<%s> Successfully initialized MCP client", c.name)

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
			log.Printf("<%s> Context done, stopping ping", c.name)
			return
		case <-ticker.C:
			if err := c.client.Ping(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				failCount++
				log.Printf("<%s> MCP Ping failed: %v (count=%d)", c.name, err, failCount)
			} else if failCount > 0 {
				log.Printf("<%s> MCP Ping recovered after %d failures", c.name, failCount)
				failCount = 0
			}
		}
	}
}

func buildToolFilterFunc(clientName string, options *OptionsV2) func(toolName string) bool {
	filterFunc := func(toolName string) bool {
		return true
	}
	if options == nil || options.ToolFilter == nil {
		return filterFunc
	}

	filterList := normalizeToolFilterList(options.ToolFilter.List)
	if len(filterList) == 0 {
		return filterFunc
	}

	filterSet := make(map[string]struct{}, len(filterList))
	mode := ToolFilterMode(strings.ToLower(string(options.ToolFilter.Mode)))
	for _, toolName := range filterList {
		filterSet[toolName] = struct{}{}
	}
	log.Printf("<%s> Applying toolFilter mode=%s for tools: %s", clientName, mode, strings.Join(filterList, ", "))

	switch mode {
	case ToolFilterModeAllow:
		return func(toolName string) bool {
			_, inList := filterSet[toolName]
			if !inList {
				log.Printf("<%s> Ignoring tool %s as it is not in allow list", clientName, toolName)
			}
			return inList
		}
	case ToolFilterModeBlock:
		return func(toolName string) bool {
			_, inList := filterSet[toolName]
			if inList {
				log.Printf("<%s> Ignoring tool %s as it is in block list", clientName, toolName)
			}
			return !inList
		}
	default:
		log.Printf("<%s> Unknown tool filter mode: %s, skipping tool filter", clientName, mode)
		return filterFunc
	}
}

func (c *Client) addToolsToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	toolsRequest := mcp.ListToolsRequest{}
	filterFunc := buildToolFilterFunc(c.name, c.options)

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
		log.Printf("<%s> Successfully listed %d tools", c.name, len(tools.Tools))
		for _, tool := range tools.Tools {
			if filterFunc(tool.Name) {
				log.Printf("<%s> Adding tool %s", c.name, tool.Name)
				mcpServer.AddTool(tool, c.client.CallTool)
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
		log.Printf("<%s> Successfully listed %d prompts", c.name, len(prompts.Prompts))
		for _, prompt := range prompts.Prompts {
			log.Printf("<%s> Adding prompt %s", c.name, prompt.Name)
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
		log.Printf("<%s> Successfully listed %d resources", c.name, len(resources.Resources))
		for _, resource := range resources.Resources {
			log.Printf("<%s> Adding resource %s", c.name, resource.Name)
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
		log.Printf("<%s> Successfully listed %d resource templates", c.name, len(resourceTemplates.ResourceTemplates))
		for _, resourceTemplate := range resourceTemplates.ResourceTemplates {
			log.Printf("<%s> Adding resource template %s", c.name, resourceTemplate.Name)
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
	serverOpts := []server.ServerOption{
		server.WithResourceCapabilities(true, true),
		server.WithRecovery(),
	}

	if clientConfig.Options.LogEnabled.OrElse(false) {
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

	if clientConfig.Options != nil && len(clientConfig.Options.AuthTokens) > 0 {
		srv.tokens = clientConfig.Options.AuthTokens
	}

	return srv, nil
}
