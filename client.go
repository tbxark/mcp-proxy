package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// clientHealth is the last known state of a downstream connection.
//
// A client that never connected stays healthUnknown and is left out of the
// readiness report: a server that is misconfigured or down at startup must not
// keep the whole proxy out of rotation, since the other servers still work. One
// that connected and later broke does report unhealthy, because that is a
// regression a load balancer should route around.
type clientHealth int32

const (
	healthUnknown clientHealth = iota
	healthOK
	healthFailed
)

type Client struct {
	name            string
	needPing        bool
	needManualStart bool
	client          *client.Client
	options         *OptionsV2
	health          atomic.Int32
	// requestTimeout bounds one forwarded request. It is only set for stdio
	// downstreams: the sse and streamable-http clients carry their timeout
	// inside the transport, but the stdio transport has no equivalent, so the
	// bound has to be applied to the context of each call instead.
	requestTimeout time.Duration
}

func (c *Client) Health() clientHealth {
	return clientHealth(c.health.Load())
}

const acceptEncodingHeader = "Accept-Encoding"

// mcpHTTPHeaders returns a copy of headers that explicitly opts out of response
// compression. Some MCP servers otherwise return gzip data that reaches the JSON
// decoder without being decompressed by Go's HTTP transport.
func mcpHTTPHeaders(headers map[string]string) map[string]string {
	result := make(map[string]string, len(headers)+1)
	for key, value := range headers {
		if strings.EqualFold(key, acceptEncodingHeader) {
			continue
		}
		result[key] = value
	}
	result[acceptEncodingHeader] = "identity"
	return result
}

// sseClientOptions and streamableClientOptions keep the plain and OAuth
// variants of each transport configured identically.
func sseClientOptions(conf *SSEMCPClientConfig) []transport.ClientOption {
	options := []transport.ClientOption{client.WithHeaders(mcpHTTPHeaders(conf.Headers))}
	if conf.Timeout > 0 {
		options = append(options, transport.WithResponseTimeout(time.Duration(conf.Timeout)))
	}
	return options
}

func streamableClientOptions(conf *StreamableMCPClientConfig) []transport.StreamableHTTPCOption {
	options := []transport.StreamableHTTPCOption{transport.WithHTTPHeaders(mcpHTTPHeaders(conf.Headers))}
	if conf.Timeout > 0 {
		options = append(options, transport.WithHTTPTimeout(time.Duration(conf.Timeout)))
	}
	return options
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
		drainStderr(name, mcpClient)

		// Stdio servers are pinged too: a crashed subprocess is the most
		// common way a downstream disappears at runtime.
		return &Client{
			name:           name,
			needPing:       true,
			client:         mcpClient,
			options:        conf.Options,
			requestTimeout: time.Duration(v.Timeout),
		}, nil
	case *SSEMCPClientConfig:
		if v.OAuth != nil {
			oc, oErr := buildOAuthConfig(name, v.OAuth)
			if oErr != nil {
				return nil, oErr
			}
			options := sseClientOptions(v)
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
		options := sseClientOptions(v)
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
			options := streamableClientOptions(v)
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
		options := streamableClientOptions(v)
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

	c.health.Store(int32(healthOK))
	if c.needPing {
		go c.startPingTask(ctx)
	}
	return nil
}

// callTool forwards a tool call to the downstream, bounded by requestTimeout
// when one is configured.
//
// The bound matters most for stdio: a stdio downstream shares one pipe for
// every request, so a call the server accepts but never answers keeps that pipe
// occupied. The keepalive ping then gets no reply either, and after
// pingFailureThreshold probes the client is marked unhealthy and stays there —
// the whole downstream is lost to every caller, not just the one that made the
// wedging call. sse and streamable-http already bound this inside their
// transports, so requestTimeout is left zero for them and the caller's context
// is forwarded unchanged.
func (c *Client) callTool(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if c.requestTimeout <= 0 {
		return c.client.CallTool(ctx, request)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.requestTimeout)
	defer cancel()
	return c.client.CallTool(callCtx, request)
}

// drainStderr keeps reading a stdio subprocess's stderr for as long as it runs.
//
// Start() hands the subprocess a StderrPipe that nothing consumes: mcp-go only
// exposes it through Stderr(). An OS pipe holds roughly 64KB, so a downstream
// that logs to stderr — most of them do — works until that buffer fills and then
// blocks inside write(2) forever. Nothing reports an error, because from here
// the server has simply gone silent: one stdio channel carries every request, so
// the keepalive ping stops being answered too and the client is marked unhealthy
// for every caller until the proxy restarts.
//
// The lines are logged rather than discarded, since a downstream's stderr is
// usually where it explains why it is unhappy.
func drainStderr(name string, mcpClient *client.Client) {
	stderr, ok := client.GetStderr(mcpClient)
	if !ok || stderr == nil {
		return
	}
	go func() {
		scanner := bufio.NewScanner(stderr)
		// Downstream servers can emit long single lines (a Python traceback
		// frame, a serialized payload); the default 64KB token limit would turn
		// one into a scan error and stop the drain.
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			slog.Debug("Downstream stderr", "client", name, "line", scanner.Text())
		}
	}()
}

// pingTimeout bounds a single probe, so a downstream that accepts the request
// and then stalls cannot block the health loop until shutdown.
const pingTimeout = 10 * time.Second

// pingFailureThreshold is how many probes in a row have to fail before the
// connection counts as broken. One is not enough: a single-threaded downstream
// (the common shape for Python and Node stdio servers) busy with a long tool
// call answers no pings at all, and taking the whole proxy out of rotation for
// that would pull a pod mid-request.
const pingFailureThreshold = 3

// isTransportFailure reports whether err means the connection itself is
// broken. A downstream that answers with a JSON-RPC error is still alive - it
// may simply not implement ping - so only transport errors count as unhealthy.
func isTransportFailure(err error) bool {
	var transportErr *transport.Error
	return errors.As(err, &transportErr)
}

func (c *Client) startPingTask(ctx context.Context) {
	ticker := time.NewTicker(c.options.pingInterval())
	defer ticker.Stop()

	failCount := 0
	for {
		select {
		case <-ctx.Done():
			slog.Debug("Context done, stopping ping", "client", c.name)
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.client.Ping(pingCtx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil && isTransportFailure(err) {
				failCount++
				slog.Warn("MCP ping failed", "client", c.name, "err", err, "failures", failCount)
				if failCount >= pingFailureThreshold {
					c.health.Store(int32(healthFailed))
				}
				continue
			}
			if err != nil {
				slog.Debug("MCP ping answered with an error, treating as alive", "client", c.name, "err", err)
			}
			if failCount > 0 {
				slog.Info("MCP ping recovered", "client", c.name, "failures", failCount)
				failCount = 0
			}
			c.health.Store(int32(healthOK))
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
				mcpServer.AddTool(tool, c.callTool)
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
	if c.client == nil {
		return nil
	}
	err := c.client.Close()
	// A stdio subprocess that already died, or that exits non-zero when its
	// stdin is closed (many MCP servers do), is not a failure to close it.
	// Reporting it as one would make the proxy exit non-zero after an
	// otherwise clean shutdown.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		slog.Warn("Downstream server exited with an error", "client", c.name, "err", err)
		return nil
	}
	return err
}

type Server struct {
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
	return &Server{
		mcpServer: mcpServer,
		handler:   handler,
	}, nil
}
