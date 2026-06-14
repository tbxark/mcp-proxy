package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	pingInterval        = 30 * time.Second
	pingTimeout         = 10 * time.Second
	connectTimeout      = 30 * time.Second
	defaultReconnectGap = 15 * time.Second
)

type Client struct {
	name            string
	needPing        bool
	needManualStart bool
	options         *OptionsV2
	clientConf      any // *StdioMCPClientConfig | *SSEMCPClientConfig | *StreamableMCPClientConfig

	mu     sync.Mutex
	client *client.Client

	// remembered so the ping task can re-establish a dropped backend on its own.
	clientInfo  mcp.Implementation
	mcpServer   *server.MCPServer
	pingStarted bool
}

func newMCPClient(name string, conf *MCPClientConfigV2) (*Client, error) {
	clientInfo, pErr := parseMCPClientConfigV2(conf)
	if pErr != nil {
		return nil, pErr
	}
	c := &Client{
		name:       name,
		options:    conf.Options,
		clientConf: clientInfo,
	}
	switch clientInfo.(type) {
	case *StdioMCPClientConfig:
		// stdio backends are spawned by the client constructor (no manual Start),
		// but still ping them so a crashed subprocess is detected and respawned.
		c.needPing = true
	case *SSEMCPClientConfig, *StreamableMCPClientConfig:
		c.needPing = true
		c.needManualStart = true
	}
	return c, nil
}

// buildRawClient creates a fresh underlying transport client from the stored
// config. Called on the first connect and again on every reconnect, so a dead
// backend (closed Obsidian, crashed stdio server) is replaced rather than reused.
func (c *Client) buildRawClient() (*client.Client, error) {
	switch v := c.clientConf.(type) {
	case *StdioMCPClientConfig:
		envs := make([]string, 0, len(v.Env))
		for kk, vv := range v.Env {
			envs = append(envs, fmt.Sprintf("%s=%s", kk, vv))
		}
		return client.NewStdioMCPClient(v.Command, envs, v.Args...)
	case *SSEMCPClientConfig:
		var options []transport.ClientOption
		if len(v.Headers) > 0 {
			options = append(options, client.WithHeaders(v.Headers))
		}
		return client.NewSSEMCPClient(v.URL, options...)
	case *StreamableMCPClientConfig:
		var options []transport.StreamableHTTPCOption
		if len(v.Headers) > 0 {
			options = append(options, transport.WithHTTPHeaders(v.Headers))
		}
		if v.Timeout > 0 {
			options = append(options, transport.WithHTTPTimeout(v.Timeout))
		}
		return client.NewStreamableHttpClient(v.URL, options...)
	}
	return nil, errors.New("invalid client type")
}

func (c *Client) getClient() *client.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client
}

// establish builds a fresh client, initializes it, atomically swaps it in
// (closing the old one), then (re)registers tools/prompts/resources. Tool
// handlers resolve the live client via getClient at call time, so swapping the
// underlying connection is transparent to in-flight and future requests.
func (c *Client) establish(ctx context.Context, clientInfo mcp.Implementation, mcpServer *server.MCPServer) error {
	raw, err := c.buildRawClient()
	if err != nil {
		return err
	}

	startCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	if c.needManualStart {
		if sErr := raw.Start(startCtx); sErr != nil {
			_ = raw.Close()
			return sErr
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
	if _, iErr := raw.Initialize(startCtx, initRequest); iErr != nil {
		_ = raw.Close()
		return iErr
	}

	c.mu.Lock()
	old := c.client
	c.client = raw
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	log.Printf("<%s> Successfully initialized MCP client", c.name)

	if err = c.addToolsToServer(ctx, mcpServer); err != nil {
		return err
	}
	_ = c.addPromptsToServer(ctx, mcpServer)
	_ = c.addResourcesToServer(ctx, mcpServer)
	_ = c.addResourceTemplatesToServer(ctx, mcpServer)
	return nil
}

func (c *Client) addToMCPServer(ctx context.Context, clientInfo mcp.Implementation, mcpServer *server.MCPServer) error {
	c.clientInfo = clientInfo
	c.mcpServer = mcpServer

	if err := c.establish(ctx, clientInfo, mcpServer); err != nil {
		return err
	}

	if c.needPing && !c.pingStarted {
		c.pingStarted = true
		go c.startPingTask(ctx)
	}
	return nil
}

// reconnect rebuilds a dropped backend using the details remembered at first
// connect. Safe to call repeatedly from the ping task.
func (c *Client) reconnect(ctx context.Context) error {
	return c.establish(ctx, c.clientInfo, c.mcpServer)
}

func (c *Client) startPingTask(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	autoReconnect := c.options.AutoReconnect.OrElse(true)
	failCount := 0
	for {
		select {
		case <-ctx.Done():
			log.Printf("<%s> Context done, stopping ping", c.name)
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.getClient().Ping(pingCtx)
			cancel()
			if err == nil {
				if failCount > 0 {
					log.Printf("<%s> MCP Ping recovered after %d failures", c.name, failCount)
					failCount = 0
				}
				continue
			}
			// Only the parent context being cancelled means we should stop; a
			// per-ping timeout just signals an unhealthy backend to recover.
			if ctx.Err() != nil {
				return
			}
			failCount++
			log.Printf("<%s> MCP Ping failed: %v (count=%d)", c.name, err, failCount)
			if !autoReconnect {
				continue
			}
			if rErr := c.reconnect(ctx); rErr != nil {
				log.Printf("<%s> Reconnect failed: %v", c.name, rErr)
			} else {
				log.Printf("<%s> Reconnected after %d ping failure(s)", c.name, failCount)
				failCount = 0
			}
		}
	}
}

func (c *Client) addToolsToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	cl := c.getClient()
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
					log.Printf("<%s> Ignoring tool %s as it is not in allow list", c.name, toolName)
				}
				return inList
			}
		case ToolFilterModeBlock:
			filterFunc = func(toolName string) bool {
				_, inList := filterSet[toolName]
				if inList {
					log.Printf("<%s> Ignoring tool %s as it is in block list", c.name, toolName)
				}
				return !inList
			}
		default:
			log.Printf("<%s> Unknown tool filter mode: %s, skipping tool filter", c.name, mode)
		}
	}

	for {
		tools, err := cl.ListTools(ctx, toolsRequest)
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
				mcpServer.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return c.getClient().CallTool(ctx, request)
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
	cl := c.getClient()
	promptsRequest := mcp.ListPromptsRequest{}
	for {
		prompts, err := cl.ListPrompts(ctx, promptsRequest)
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
			mcpServer.AddPrompt(prompt, func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return c.getClient().GetPrompt(ctx, request)
			})
		}
		if prompts.NextCursor == "" {
			break
		}
		promptsRequest.Params.Cursor = prompts.NextCursor
	}
	return nil
}

func (c *Client) addResourcesToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	cl := c.getClient()
	resourcesRequest := mcp.ListResourcesRequest{}
	for {
		resources, err := cl.ListResources(ctx, resourcesRequest)
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
				readResource, e := c.getClient().ReadResource(ctx, request)
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
	cl := c.getClient()
	resourceTemplatesRequest := mcp.ListResourceTemplatesRequest{}
	for {
		resourceTemplates, err := cl.ListResourceTemplates(ctx, resourceTemplatesRequest)
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
				readResource, e := c.getClient().ReadResource(ctx, request)
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
	c.mu.Lock()
	cl := c.client
	c.mu.Unlock()
	if cl != nil {
		return cl.Close()
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

// connectWithRetry brings a backend online in the background. The HTTP route is
// already registered by the caller, so the endpoint exists immediately; this
// just keeps trying to populate it until the backend is reachable. Once
// connected, ongoing liveness/recovery is handled by the ping task.
func connectWithRetry(ctx context.Context, c *Client, info mcp.Implementation, mcpServer *server.MCPServer, opts *OptionsV2) {
	gap := opts.ReconnectInterval
	if gap <= 0 {
		gap = defaultReconnectGap
	}
	autoReconnect := opts.AutoReconnect.OrElse(true)

	attempt := 0
	for {
		attempt++
		log.Printf("<%s> Connecting (attempt %d)", c.name, attempt)
		err := c.addToMCPServer(ctx, info, mcpServer)
		if err == nil {
			log.Printf("<%s> Connected", c.name)
			return
		}
		log.Printf("<%s> Failed to connect: %v", c.name, err)
		if opts.PanicIfInvalid.OrElse(false) {
			log.Fatalf("<%s> Failed to add client to server: %v", c.name, err)
		}
		if !autoReconnect {
			log.Printf("<%s> Auto-reconnect disabled, giving up on this backend", c.name)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(gap):
		}
	}
}
