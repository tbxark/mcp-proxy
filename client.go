package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
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
	needManualStart bool
	options         *OptionsV2
	health          atomic.Int32
	// requestTimeout bounds one forwarded request. It is only set for stdio
	// downstreams: the sse and streamable-http clients carry their timeout
	// inside the transport, but the stdio transport has no equivalent, so the
	// bound has to be applied to the context of each call instead.
	requestTimeout time.Duration

	// clientConf is the parsed transport config. It is kept so a dropped
	// downstream can be rebuilt from scratch instead of reusing a dead client.
	clientConf any

	// mu guards client. Forwarded calls resolve the live connection through
	// getClient at request time, so reconnecting can swap it under them.
	mu     sync.RWMutex
	client *client.Client

	// connectMu serializes connect attempts: the startup retry loop and the
	// ping-driven reconnect must never build a transport at the same time.
	connectMu sync.Mutex
	// hasConnected is false until the first connect succeeds. Until then the
	// transport built in newMCPClient is reused (stdio spawns its subprocess
	// there); afterwards every attempt rebuilds, so a dead one is never reused.
	hasConnected bool
	closed       atomic.Bool

	// life is cancelled by Close. Everything the client runs in the
	// background (the keepalive/reconnect loop, the long-lived Start context of
	// a transport) is bound to it as well as to the caller's context, because
	// the caller's context can outlive the client: Close can run while it is
	// still alive, e.g. when shutdown races a connect or in tests. Without
	// this the keepalive loop of a closed client probes a dead transport and
	// retries "client is closed" forever.
	life     context.Context
	stopLife context.CancelFunc

	// remembered from the last addToMCPServer so the ping task can reconnect.
	clientInfo mcp.Implementation
	mcpServer  *server.MCPServer
	pingOnce   sync.Once
	// pingDone is closed when the keepalive loop exits; nil if none started.
	pingDone chan struct{}

	// blockedURIs holds the normalized URIs a block-mode resourceFilter hides,
	// checked again on every read (see resourceReadAllowed). nil means no
	// block-mode filter. Rebuilt on every catalog registration.
	blockedMu   sync.RWMutex
	blockedURIs map[string]struct{}

	// lifeCtx is the long-lived context given to addToMCPServer. A rebuild
	// triggered from a forwarded request must not use that request's context:
	// Start ties the new transport to the context it receives.
	lifeCtx context.Context

	// hadSession holds the transports (*client.Client) that the downstream gave
	// a streamable-http session on initialize. It is keyed by transport, not
	// kept for the live one only, because requests still in flight on a
	// replaced transport must be recognized too. See sessionLost.
	hadSession sync.Map
	// recoverMu serializes rebuilds, so that callers failing together on the
	// same dead transport rebuild it once.
	recoverMu sync.Mutex

	// refreshMu serializes catalog refreshes triggered by downstream
	// list_changed notifications, so a burst of them re-lists one at a time
	// rather than interleaving Set calls.
	refreshMu sync.Mutex

	// retired holds replaced streamable-http transports until retireGrace has
	// passed; see retire.
	retiredMu sync.Mutex
	retired   map[*client.Client]*time.Timer
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
	c := &Client{
		name:       name,
		options:    conf.Options,
		clientConf: clientInfo,
	}
	c.life, c.stopLife = context.WithCancel(context.Background())
	switch v := clientInfo.(type) {
	case *StdioMCPClientConfig:
		c.requestTimeout = time.Duration(v.Timeout)
	case *SSEMCPClientConfig, *StreamableMCPClientConfig:
		c.needManualStart = true
	}
	// The first transport is built here: for stdio that spawns the subprocess,
	// so a missing command fails at startup rather than on first use.
	// Block listed URIs from the start, so a read through a template is refused
	// even if listing the downstream's resources fails.
	c.setBlockedURIs(nil)
	raw, err := c.buildRawClient()
	if err != nil {
		return nil, err
	}
	c.client = raw
	return c, nil
}

// buildRawClient creates a fresh underlying transport from the stored config.
// It is called once at construction and again on every reconnect, so a dead
// backend (closed Obsidian, crashed stdio server) is replaced rather than
// reused.
func (c *Client) buildRawClient(extra ...client.ClientOption) (*client.Client, error) {
	return newRawClient(c.name, c.clientConf, extra...)
}

// newRawClient builds an mcp-go client for one parsed transport config. It is
// shared by Client (connect and reconnect) and the one-off -authorize flow.
// extra carries client-level options the convenience constructors cannot take
// (a protocol version pin on the legacy fallback), so the transports are
// assembled by hand.
func newRawClient(name string, conf any, extra ...client.ClientOption) (*client.Client, error) {
	switch v := conf.(type) {
	case *StdioMCPClientConfig:
		envs := make([]string, 0, len(v.Env))
		for kk, vv := range v.Env {
			envs = append(envs, fmt.Sprintf("%s=%s", kk, vv))
		}
		var raw *client.Client
		if len(extra) == 0 {
			var err error
			raw, err = client.NewStdioMCPClient(v.Command, envs, v.Args...)
			if err != nil {
				return nil, err
			}
		} else {
			stdioTransport := transport.NewStdioWithOptions(v.Command, envs, v.Args)
			if err := stdioTransport.Start(context.Background()); err != nil {
				return nil, fmt.Errorf("failed to start stdio transport: %w", err)
			}
			raw = client.NewClient(stdioTransport, extra...)
		}
		drainStderr(name, raw)
		return raw, nil
	case *SSEMCPClientConfig:
		options := sseClientOptions(v)
		if v.OAuth != nil {
			oc, err := buildOAuthConfig(name, v.OAuth)
			if err != nil {
				return nil, err
			}
			options = append(options, transport.WithOAuth(oc))
		}
		sseTransport, err := transport.NewSSE(v.URL, options...)
		if err != nil {
			return nil, fmt.Errorf("failed to create SSE transport: %w", err)
		}
		return client.NewClient(sseTransport, extra...), nil
	case *StreamableMCPClientConfig:
		options := streamableClientOptions(v)
		if v.OAuth != nil {
			oc, err := buildOAuthConfig(name, v.OAuth)
			if err != nil {
				return nil, err
			}
			options = append(options, transport.WithHTTPOAuth(oc))
		}
		httpTransport, err := transport.NewStreamableHTTP(v.URL, options...)
		if err != nil {
			return nil, err
		}
		opts := extra
		if sessionID := httpTransport.GetSessionId(); sessionID != "" {
			opts = append([]client.ClientOption{client.WithSession()}, opts...)
		}
		return client.NewClient(httpTransport, opts...), nil
	}
	return nil, errors.New("invalid client type")
}

// getClient returns the live transport, or nil before the first connection.
// Forwarded calls and the health probe go through it, so reconnecting can swap
// the transport underneath them.
func (c *Client) getClient() *client.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.client
}

// connect builds a transport, initializes it, swaps it in (closing the previous
// one), then (re)registers the downstream's tools/prompts/resources. Tool
// handlers resolve the live client via getClient at call time, so the swap is
// transparent to in-flight and future requests.
func (c *Client) connect(ctx context.Context, clientInfo mcp.Implementation, mcpServer *server.MCPServer) error {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()

	if c.closed.Load() {
		return errClientClosed
	}

	// Reuse the transport built at construction for the very first attempt;
	// after that (or after a failed first attempt, which nils it out) build a
	// fresh one so a dead transport is never reused.
	raw := c.getClient()
	if raw == nil || c.hasConnected {
		var err error
		raw, err = c.buildRawClient()
		if err != nil {
			return err
		}
	}

	// A list_changed notification from the downstream refreshes the proxied
	// catalog. It is registered before Start so nothing the transport receives
	// while connecting is missed; onDownstreamNotification drops anything
	// arriving on a transport that has since been replaced.
	raw.OnNotification(func(n mcp.JSONRPCNotification) {
		c.onDownstreamNotification(raw, n)
	})

	connected := false
	// Until the swap below succeeds, this transport is not owned by the client
	// and must be closed on the way out.
	defer func() {
		if !connected {
			_ = raw.Close()
		}
	}()

	// Start is given the long-lived context, not a bounded one: mcp-go's SSE
	// transport ties its event stream to the context Start receives, and stdio
	// stores it for request handling, so a deadline there would tear the
	// session down as soon as it elapsed.
	if c.needManualStart {
		if err := raw.Start(ctx); err != nil {
			c.forget(raw)
			return oauthAwareError(c.name, err)
		}
	}

	// Initialize is a request/response, so it is bounded: a downstream that
	// accepts the connection and then goes silent must not wedge a retry loop
	// forever. Tool/prompt/resource listing below uses the caller's context,
	// since a large catalog can legitimately take longer than a handshake.
	initCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = clientInfo
	initRequest.Params.Capabilities = mcp.ClientCapabilities{
		Experimental: make(map[string]any),
		Roots:        nil,
		Sampling:     nil,
	}
	initResult, err := raw.Initialize(initCtx, initRequest)
	if err != nil {
		raw, initResult, err = c.retryPinnedLegacy(ctx, initCtx, raw, initRequest, err)
	}
	if err != nil {
		c.forget(raw)
		return oauthAwareError(c.name, err)
	}
	// First connect only: later, server.WithInstructions races handleInitialize.
	if !c.hasConnected {
		forwardInstructions(mcpServer, initResult)
	}

	if streamableSessionID(raw) != "" {
		c.hadSession.Store(raw, struct{}{})
	}
	c.mu.Lock()
	old := c.client
	c.client = raw
	c.mu.Unlock()
	connected = true
	// The transport is established now, so any later attempt must build a new
	// one rather than re-initializing this one.
	c.hasConnected = true
	if old != nil && old != raw {
		c.retire(old)
	}
	// Close does not hold connectMu, so it may have run while this transport was
	// being built - it would have closed the previous one, not this. Close this
	// one here rather than leak it (a stdio transport owns a subprocess).
	if c.closed.Load() {
		c.hadSession.Delete(raw)
		_ = raw.Close()
		return errClientClosed
	}
	slog.Info("Successfully initialized MCP client", "client", c.name)

	// Modern protocol versions require the client to opt in to the
	// notification types it wants: subscriptions/listen is how they arrive.
	// Opt in to the list_changed types the downstream says it can send (legacy
	// connections push them without an opt-in). lifeCtx is set by
	// addToMCPServer before connect runs; a nil lifeCtx means there is no
	// long-lived scope to hold the stream open, so skip.
	//
	// stdio is exempt: its request stream is bidirectional, so a downstream
	// pushes notifications over the same pipe without a stream to opt in to.
	// An mcp-go stdio downstream registers a session that receives broadcasts
	// unfiltered, and serving subscriptions/listen would occupy its serial
	// reader until shutdown, deadlocking every later request.
	_, stdio := c.clientConf.(*StdioMCPClientConfig)
	if c.lifeCtx != nil && !stdio && mcp.IsModernProtocol(raw.ProtocolVersion()) {
		var filter mcp.SubscriptionFilter
		capabilities := initResult.Capabilities
		if capabilities.Tools != nil && capabilities.Tools.ListChanged {
			filter.ToolsListChanged = true
		}
		if capabilities.Prompts != nil && capabilities.Prompts.ListChanged {
			filter.PromptsListChanged = true
		}
		if capabilities.Resources != nil && capabilities.Resources.ListChanged {
			filter.ResourcesListChanged = true
		}
		if !filter.IsEmpty() {
			_, _ = raw.ListenAsync(c.lifeCtx, filter, func(err error) {
				slog.Debug("Notification stream ended", "client", c.name, "err", redactURLCredentials(err))
			})
		}
	}

	// Bound the catalog discovery so a downstream that answers initialize and
	// then goes silent cannot wedge this attempt (and the retry loop with it).
	// The transports use this ctx per request, so it does not tear down the
	// connection the way bounding Start would.
	catalogCtx, cancelCatalog := context.WithTimeout(ctx, catalogTimeout)
	defer cancelCatalog()

	// Catalog registration copies descriptors that a downstream controls, so it
	// runs through the panic-guarded helper below: a future shape this code does
	// not anticipate must cost that backend its own route, not terminate the
	// process that serves every route.
	if err := c.registerCatalogSafely(func() error {
		return c.addToolsToServer(catalogCtx, mcpServer)
	}); err != nil {
		return err
	}
	_ = c.registerCatalogSafely(func() error {
		_ = c.addPromptsToServer(catalogCtx, mcpServer)
		_ = c.addResourcesToServer(catalogCtx, mcpServer)
		_ = c.addResourceTemplatesToServer(catalogCtx, mcpServer)
		return nil
	})

	c.health.Store(int32(healthOK))
	return nil
}

// retryPinnedLegacy retries the initialize handshake pinned to a legacy
// protocol version when raw's failure shows the downstream only speaks legacy
// versions. mcp-go probes server/discover during Initialize; a downstream
// that version-checks the request and rejects 2026-07-28 with an
// UnsupportedProtocolVersionError listing only legacy versions (mcp-go built
// with WithStreamableHTTPProtocolVersions restricted to the legacy set, for
// one) is classified there as a modern server that wants a different modern
// version, so no initialize handshake is ever tried. On success the returned
// client replaces raw — it is a different transport, already started and
// initialized.
func (c *Client) retryPinnedLegacy(ctx, initCtx context.Context, raw *client.Client, initRequest mcp.InitializeRequest, initErr error) (*client.Client, *mcp.InitializeResult, error) {
	unsupported, ok := errors.AsType[mcp.UnsupportedProtocolVersionError](initErr)
	if !ok {
		return raw, nil, initErr
	}
	pinned := mcp.NegotiateMutuallySupportedVersion(unsupported.Supported)
	if pinned == "" || mcp.IsModernProtocol(pinned) {
		return raw, nil, initErr
	}
	slog.Info("Downstream supports only legacy protocol versions, retrying the initialize handshake", "client", c.name, "version", pinned)
	_ = raw.Close()
	fallback, err := c.buildRawClient(client.WithProtocolVersion(pinned))
	if err != nil {
		return raw, nil, err
	}
	fallback.OnNotification(func(n mcp.JSONRPCNotification) {
		c.onDownstreamNotification(fallback, n)
	})
	if c.needManualStart {
		if err := fallback.Start(ctx); err != nil {
			c.forget(fallback)
			return fallback, nil, oauthAwareError(c.name, err)
		}
	}
	initResult, err := fallback.Initialize(initCtx, initRequest)
	if err != nil {
		c.forget(fallback)
	}
	return fallback, initResult, err
}

// onDownstreamNotification handles a notification received on transport raw.
// Notifications arriving on a retired transport are dropped: only the live
// one can refresh the catalog. The refresh runs in its own goroutine so the
// transport's reader is never blocked, and refreshes are serialized by
// refreshMu so a burst of list_changed notifications re-lists one at a time.
func (c *Client) onDownstreamNotification(raw *client.Client, n mcp.JSONRPCNotification) {
	if raw != c.getClient() {
		return
	}
	var refresh func(ctx context.Context, mcpServer *server.MCPServer) error
	var kind string
	switch n.Method {
	case string(mcp.MethodNotificationToolsListChanged):
		kind, refresh = "tools", c.addToolsToServer
	case string(mcp.MethodNotificationPromptsListChanged):
		kind, refresh = "prompts", c.addPromptsToServer
	case string(mcp.MethodNotificationResourcesListChanged):
		kind = "resources"
		refresh = func(ctx context.Context, mcpServer *server.MCPServer) error {
			if err := c.addResourcesToServer(ctx, mcpServer); err != nil {
				return err
			}
			return c.addResourceTemplatesToServer(ctx, mcpServer)
		}
	default:
		return
	}
	mcpServer := c.mcpServer
	if mcpServer == nil {
		return
	}
	go func() {
		lifeCtx := c.lifeCtx
		if lifeCtx == nil || lifeCtx.Err() != nil || c.closed.Load() {
			return
		}
		c.refreshMu.Lock()
		defer c.refreshMu.Unlock()
		ctx, cancel := context.WithTimeout(lifeCtx, catalogTimeout)
		defer cancel()
		if err := c.registerCatalogSafely(func() error {
			return refresh(ctx, mcpServer)
		}); err != nil {
			slog.Warn("Failed to refresh downstream catalog", "client", c.name, "kind", kind, "err", redactURLCredentials(err))
			return
		}
		slog.Info("Refreshed downstream catalog", "client", c.name, "kind", kind)
	}()
}

// registerCatalogSafely runs fn and converts a panic caused by
// downstream-supplied catalog data into an error, so a malformed descriptor
// cannot take down the shared process before it serves any route.
func (c *Client) registerCatalogSafely(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Recovered from panic while registering downstream catalog",
				"client", c.name, "err", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("downstream catalog registration panicked: %v", r)
		}
	}()
	return fn()
}

// forget drops a transport that failed to establish, so the next attempt
// builds a new one instead of reusing a broken one.
func (c *Client) forget(raw *client.Client) {
	c.mu.Lock()
	if c.client == raw {
		c.client = nil
	}
	c.mu.Unlock()
}

// forwardInstructions passes the backend's initialize instructions through to the
// proxy's own server. Without it the proxy answers initialize with no instructions,
// so clients never see the guidance the backend server provides.
func forwardInstructions(mcpServer *server.MCPServer, res *mcp.InitializeResult) {
	if mcpServer == nil || res == nil || res.Instructions == "" {
		return
	}
	server.WithInstructions(res.Instructions)(mcpServer)
}

// errClientClosed is returned by connect once Close has run. It is terminal:
// nothing retries a closed client.
var errClientClosed = errors.New("client is closed")

// withLifetime derives a context that ends when ctx ends or when the client is
// closed, whichever comes first. The returned cancel releases it early.
func (c *Client) withLifetime(ctx context.Context) (context.Context, context.CancelFunc) {
	bound, cancel := context.WithCancel(ctx)
	if c.life == nil {
		// A Client literal built by a test, not by newMCPClient.
		return bound, cancel
	}
	stop := context.AfterFunc(c.life, cancel)
	return bound, func() {
		stop()
		cancel()
	}
}

func (c *Client) addToMCPServer(ctx context.Context, clientInfo mcp.Implementation, mcpServer *server.MCPServer) error {
	c.clientInfo = clientInfo
	c.mcpServer = mcpServer

	// Bind everything this connection starts to the client's own lifetime,
	// not only to the caller's: the boot-time supervisor passes the proxy's
	// context, which outlives a client that a reload stops. On success the
	// bound context is deliberately kept - the SSE event stream and the
	// keepalive loop run under it - and Close is what ends it.
	ctx, release := c.withLifetime(ctx)
	// Rebuilds triggered by forwarded requests run under this same bound
	// context, so they end on Close too.
	c.lifeCtx = ctx
	if err := c.connect(ctx, clientInfo, mcpServer); err != nil {
		release()
		return err
	}

	c.pingOnce.Do(func() {
		c.pingDone = make(chan struct{})
		go func() {
			defer close(c.pingDone)
			c.startPingTask(ctx)
		}()
	})
	return nil
}

// reconnect rebuilds a dropped downstream using the details remembered at
// first connect. It runs from the ping task and, via recoverSession, from a
// forwarded request that found its session lost.
func (c *Client) reconnect(ctx context.Context) error {
	return c.connect(ctx, c.clientInfo, c.mcpServer)
}

// reconnectIfCurrent rebuilds the downstream only if raw is still the live
// transport. Several forwarded requests (and the ping task) can fail at the
// same time on the same dead transport; the first one rebuilds it and the rest
// find a new live client and use that, instead of tearing down a connection
// that was just made.
func (c *Client) reconnectIfCurrent(ctx context.Context, raw *client.Client) error {
	c.recoverMu.Lock()
	defer c.recoverMu.Unlock()
	if c.getClient() != raw {
		return nil
	}
	return c.reconnect(ctx)
}

// streamableSessionID returns the session id the downstream gave raw on
// initialize, or "" when raw is not streamable-http or the server is stateless.
func streamableSessionID(raw *client.Client) string {
	if raw == nil {
		return ""
	}
	if t, ok := raw.GetTransport().(*transport.StreamableHTTP); ok {
		return t.GetSessionId()
	}
	return ""
}

// sessionLost reports whether err, returned by a request sent on raw, means the
// streamable-http downstream no longer knows raw's session - which is what
// happens when it restarts (a redeploy, a crash and relaunch, a process
// manager bringing it back). The new process answers the old session id with
// 404; mcp-go turns that into ErrSessionTerminated and clears the id, and from
// then on every request goes out without one. A stateful server rejects those
// with a JSON-RPC error (the Python SDK: "Bad Request: Missing session ID"),
// which is not a transport error. mcp-go never re-initializes on its own, so
// the connection is dead for good, but the keepalive treated that second error
// as "alive": the route stayed broken - and /_readyz stayed ok - until the
// proxy was restarted.
//
// The first case is the typed ErrSessionTerminated. The second is detected by
// state rather than by matching the server's wording, which differs between
// SDKs: raw had a session after connect and has none now. raw need not be the
// live transport any more - a request that was in flight when another caller
// rebuilt the connection is recognized the same way.
func (c *Client) sessionLost(raw *client.Client, err error) bool {
	if err == nil || raw == nil {
		return false
	}
	if errors.Is(err, transport.ErrSessionTerminated) {
		return true
	}
	_, had := c.hadSession.Load(raw)
	return had && streamableSessionID(raw) == ""
}

// retireGrace is how long a replaced streamable-http transport stays open.
const retireGrace = 10 * time.Second

// retire closes a transport that connect has just replaced. stdio and sse ones
// close at once, as before. A streamable-http one is closed after retireGrace:
// closing it at once cancels the requests still in flight on it, and a
// "context canceled" from our own Close cannot be told apart from a request the
// server may have run, so forward could not retry it. Those requests were sent
// on the session the downstream had just dropped, so the server refuses them
// within milliseconds; left to finish, they come back with that refusal, which
// sessionLost recognizes and forward retries on the new transport. A streamable
// HTTP transport owns no process, so keeping it open a few more seconds costs
// nothing but a few idle sockets.
func (c *Client) retire(old *client.Client) {
	if _, ok := old.GetTransport().(*transport.StreamableHTTP); !ok {
		_ = old.Close()
		return
	}
	c.retiredMu.Lock()
	defer c.retiredMu.Unlock()
	if c.retired == nil {
		c.retired = map[*client.Client]*time.Timer{}
	}
	c.retired[old] = time.AfterFunc(retireGrace, func() {
		c.retiredMu.Lock()
		_, pending := c.retired[old]
		delete(c.retired, old)
		c.retiredMu.Unlock()
		if !pending {
			return // Close got to it first.
		}
		c.hadSession.Delete(old)
		_ = old.Close()
	})
}

// closeRetired closes every transport still waiting out retireGrace, so
// shutdown does not leave them open.
func (c *Client) closeRetired() {
	c.retiredMu.Lock()
	pending := c.retired
	c.retired = nil
	c.retiredMu.Unlock()
	for old, timer := range pending {
		timer.Stop()
		c.hadSession.Delete(old)
		_ = old.Close()
	}
}

// recoverSession rebuilds the downstream after a forwarded request found its
// session lost, and returns the live client to retry the request on once, or
// nil when the request must fail as it is. It only acts with autoReconnect: the
// option is the opt-in contract for in-place recovery, and without it an
// orchestrator restarting the proxy owns recovery (the probe still reports the
// client unhealthy).
//
// Retrying is safe because a request refused for an unknown (404) or missing
// (400) session is rejected by the server's transport before it reaches a
// handler - the Python and TypeScript SDKs and mcp-go's own server all
// validate the session first - so the tool did not run and cannot run twice.
// A request that fails any other way is never retried.
func (c *Client) recoverSession(raw *client.Client, err error) *client.Client {
	if !c.options.autoReconnect() || !c.sessionLost(raw, err) {
		return nil
	}
	ctx := c.lifeCtx
	if ctx == nil {
		return nil
	}
	slog.Warn("Downstream lost its session (restarted?), reconnecting", "client", c.name, "err", redactURLCredentials(err))
	if rErr := c.reconnectIfCurrent(ctx, raw); rErr != nil {
		slog.Warn("Failed to reconnect downstream", "client", c.name, "err", redactURLCredentials(rErr))
		return nil
	}
	live := c.getClient()
	if live == nil || live == raw {
		return nil
	}
	return live
}

// forward sends one request to the live downstream, bounded by requestTimeout
// when one is configured, and retries it once on a rebuilt connection if the
// first attempt found the session lost (see recoverSession).
func forward[T any](ctx context.Context, c *Client, do func(context.Context, *client.Client) (T, error)) (T, error) {
	var zero T
	cl := c.getClient()
	if cl == nil {
		return zero, errors.New("downstream is not connected")
	}
	callCtx, cancel := c.withRequestTimeout(ctx)
	result, err := do(callCtx, cl)
	cancel()
	if err == nil {
		return result, nil
	}
	if live := c.recoverSession(cl, err); live != nil {
		retryCtx, retryCancel := c.withRequestTimeout(ctx)
		defer retryCancel()
		result, err = do(retryCtx, live)
	}
	return result, redactURLCredentials(err)
}

// withRequestTimeout bounds ctx by the configured per-request timeout when one
// is set (stdio only), so every forwarded method - not just tool calls - can
// abandon a stalled request instead of occupying the single shared channel.
func (c *Client) withRequestTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.requestTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.requestTimeout)
}

// redactedError keeps the original error reachable through Unwrap while
// exposing a message with URL secrets removed.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactedURLPlaceholder stands in for a URL that could not be parsed. Raw
// text cannot be redacted reliably in that case: net/url hands back a string
// truncated at the first '#' (so a credential can look like the host or a
// port), and there is no way to tell a port from a truncated password. The
// only safe option is to not echo the input at all.
const redactedURLPlaceholder = "<redacted-url>"

// redactURLString returns a display form of raw with userinfo, query, and
// fragment removed. A string that does not parse as a URL is replaced entirely
// by redactedURLPlaceholder, because no part of it can be trusted.
func redactURLString(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return redactedURLPlaceholder
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// redactURLCredentials removes userinfo, query, and fragment values from any
// *url.Error in the chain. A downstream credential may be configured in the
// URL query (docs/CONFIGURATION.md documents that form), and Go's url.Error
// embeds the whole URL in its message while redacting only the userinfo
// password - so without this the proxy's own downstream secret would reach
// callers through a JSON-RPC error and the daemon log. errors.As still finds
// the wrapped transport.Error, so failure classification is unaffected.
func redactURLCredentials(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	if urlErr.Err == nil {
		return err
	}
	safe := redactURLString(urlErr.URL)
	if safe == urlErr.URL {
		// Nothing to redact (no userinfo, query, or fragment).
		return err
	}
	if safe == redactedURLPlaceholder {
		// The URL did not parse. Its raw text can also have contaminated the
		// reason - net/url reports a truncated credential as a port - so drop
		// the reason as well rather than half-redact the message.
		return &redactedError{msg: fmt.Sprintf("%s %s", urlErr.Op, redactedURLPlaceholder), err: err}
	}
	// Replace the RENDERED form of the url.Error, not the raw URL: url.Error
	// renders its URL with %q, so a credential containing a quote or backslash
	// does not appear verbatim in the message and a raw-URL replacement would
	// silently leave it in place.
	rawRendered := (&url.Error{Op: urlErr.Op, URL: urlErr.URL, Err: urlErr.Err}).Error()
	redactedRendered := (&url.Error{Op: urlErr.Op, URL: safe, Err: urlErr.Err}).Error()
	message := strings.ReplaceAll(err.Error(), rawRendered, redactedRendered)
	if message == err.Error() {
		// The wrapper did not render the url.Error verbatim; fall back to the
		// raw URL, which covers the remaining shapes.
		message = strings.ReplaceAll(err.Error(), urlErr.URL, safe)
	}
	return &redactedError{msg: message, err: err}
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
//
// A call that finds the downstream's session lost is retried once on a rebuilt
// connection when autoReconnect is on; see recoverSession for why that is safe.
func (c *Client) callTool(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return forward(ctx, c, func(ctx context.Context, cl *client.Client) (*mcp.CallToolResult, error) {
		request.Params.Meta = stripHopMeta(request.Params.Meta)
		return cl.CallTool(ctx, request)
	})
}

// hopMetaKeys are the _meta fields that protocol version 2026-07-28 uses to
// carry per-connection state (SEP-2575). On a proxied request they describe the
// caller's connection to this proxy, not the proxy's connection to the
// downstream, so they must not be forwarded:
//
//   - On a legacy downstream connection mcp-go adds no envelope of its own, so
//     a modern caller's protocolVersion reaches a server that negotiated an
//     older version, next to an Mcp-Protocol-Version header naming that older
//     version. Strict servers, mcp-go's own streamable HTTP server included,
//     reject every such request as a header mismatch.
//   - On a modern downstream connection mcp-go overwrites protocolVersion,
//     clientCapabilities and clientInfo with its own, but a caller's logLevel,
//     serverInfo or subscriptionId would still leak through.
//
// The matching inbound HTTP headers are already dropped: mcp-go does not reuse
// the Header of a request that was decoded from the wire (outboundHeader).
var hopMetaKeys = []string{
	mcp.MetaKeyProtocolVersion,
	mcp.MetaKeyClientInfo,
	mcp.MetaKeyClientCapabilities,
	mcp.MetaKeyServerInfo,
	mcp.MetaKeyLogLevel,
	mcp.MetaKeySubscriptionID,
}

// stripHopMeta returns meta without hopMetaKeys, leaving the caller's value
// untouched. Everything else, such as progressToken or a traceparent, passes
// through.
func stripHopMeta(meta *mcp.Meta) *mcp.Meta {
	if meta == nil || len(meta.AdditionalFields) == 0 {
		return meta
	}
	stripped := *meta
	stripped.AdditionalFields = maps.Clone(meta.AdditionalFields)
	for _, k := range hopMetaKeys {
		delete(stripped.AdditionalFields, k)
	}
	if len(stripped.AdditionalFields) == 0 {
		stripped.AdditionalFields = nil
	}
	if stripped.ProgressToken == nil && stripped.AdditionalFields == nil {
		return nil
	}
	return &stripped
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

// connectTimeout bounds one establish attempt. A downstream that accepts the
// connection and then never completes initialize must not wedge a retry loop.
const connectTimeout = 30 * time.Second

// catalogTimeout bounds the tool/prompt/resource discovery half of one connect
// attempt. The transports carry no timeout of their own for these calls (the
// stdio and streamable-http clients have none by default), so without this a
// downstream that completes initialize and then goes silent mid-listing would
// leave the route unmounted forever and never reach the retry loop. It is far
// looser than connectTimeout because a large catalog is legitimately slow.
//
// It is a variable so tests can shorten it; the e2e suite drives the real binary
// and so cannot, which is why the regression test for this lives in the package.
var catalogTimeout = 60 * time.Second

// probe liveness of a downstream connection. Protocol version 2026-07-28
// removed ping: the stateless protocol core has no connection to keep alive,
// and mcp-go's client answers its Ping with an immediate nil on a modern
// connection, which would make every probe look healthy. A real request
// doubles as the probe there. The failure classification is unchanged: a
// transport error means the connection is broken, while a JSON-RPC error
// response is proof the downstream answered and is therefore alive. ListTools
// is the one call every mounted client already answered during startup, so it
// works for both protocol eras.
func (c *Client) probe(ctx context.Context, cl *client.Client) error {
	if cl == nil {
		return errors.New("downstream is not connected")
	}
	if mcp.IsModernProtocol(cl.ProtocolVersion()) {
		_, err := cl.ListTools(ctx, mcp.ListToolsRequest{})
		return err
	}
	// Ping is deprecated because it is a no-op on modern connections - which is
	// exactly why probe() only reaches it on legacy ones. There is no
	// replacement liveness call for servers older than 2026-07-28.
	return cl.Ping(ctx) //nolint:staticcheck // intentional legacy fallback, see above
}

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

// repeatedFailureLogEvery is how often a failure that keeps repeating past
// pingFailureThreshold is logged at WARN; the others go to DEBUG.
const repeatedFailureLogEvery = 20

// shouldLogRepeatedFailure reports whether the failCount-th consecutive probe
// failure deserves a WARN line.
func shouldLogRepeatedFailure(failCount int) bool {
	return failCount <= pingFailureThreshold || failCount%repeatedFailureLogEvery == 0
}

func (c *Client) startPingTask(ctx context.Context) {
	ticker := time.NewTicker(c.options.pingInterval())
	defer ticker.Stop()

	autoReconnect := c.options.autoReconnect()
	failCount := 0
	for {
		select {
		case <-ctx.Done():
			slog.Debug("Context done, stopping ping", "client", c.name)
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			raw := c.getClient()
			err := c.probe(pingCtx, raw)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				if failCount > 0 {
					slog.Info("MCP health probe recovered", "client", c.name, "failures", failCount)
					failCount = 0
				}
				c.health.Store(int32(healthOK))
				continue
			}
			if c.sessionLost(raw, err) {
				// Definitive, unlike a missed probe: the downstream forgot the
				// session (it restarted) and no later probe can bring it back.
				// Skip the threshold: report unhealthy now and, with
				// autoReconnect, rebuild now instead of after
				// pingFailureThreshold probes - which the next probes would
				// never reach, since a stateful server answers a request without
				// a session with a JSON-RPC error, not a transport error.
				slog.Warn("Downstream lost its session (restarted?)", "client", c.name, "err", redactURLCredentials(err))
				c.health.Store(int32(healthFailed))
				if !autoReconnect {
					continue
				}
				rErr := c.reconnectIfCurrent(ctx, raw)
				if errors.Is(rErr, errClientClosed) || c.closed.Load() || ctx.Err() != nil {
					// Stopped while the reconnect ran: the client is gone for
					// good, so there is nothing left to keep alive.
					return
				}
				if rErr != nil {
					slog.Warn("Failed to reconnect downstream", "client", c.name, "err", redactURLCredentials(rErr))
					continue
				}
				slog.Info("Reconnected downstream", "client", c.name, "reason", "session lost")
				failCount = 0
				c.health.Store(int32(healthOK))
				continue
			}
			if !isTransportFailure(err) {
				// A downstream that answers with a JSON-RPC error is alive - it
				// may simply not implement the probe. Only a broken connection
				// counts against it.
				slog.Debug("MCP health probe answered with an error, treating as alive", "client", c.name, "err", redactURLCredentials(err))
				if failCount > 0 {
					slog.Info("MCP health probe recovered", "client", c.name, "failures", failCount)
					failCount = 0
				}
				c.health.Store(int32(healthOK))
				continue
			}

			failCount++
			// A downstream that stays down fails every tick for as long as it
			// is down. Every failure up to the threshold is worth a WARN, but
			// after that only every repeatedFailureLogEvery-th one is, so a
			// long outage does not flood the log.
			probeLog := slog.Debug
			if shouldLogRepeatedFailure(failCount) {
				probeLog = slog.Warn
			}
			probeLog("MCP health probe failed", "client", c.name, "err", redactURLCredentials(err), "failures", failCount)
			if failCount < pingFailureThreshold {
				continue
			}
			c.health.Store(int32(healthFailed))

			if !autoReconnect {
				continue
			}
			// Rebuild the downstream from scratch. connect() swaps the live
			// transport in, so requests that arrive during the rebuild keep
			// using the old one until the new one is ready. reconnectIfCurrent
			// skips the rebuild if a forwarded request already replaced raw.
			rErr := c.reconnectIfCurrent(ctx, raw)
			if errors.Is(rErr, errClientClosed) || c.closed.Load() || ctx.Err() != nil {
				// Stopped while the probe ran: the client is gone for good,
				// so there is nothing left to keep alive.
				return
			}
			if rErr != nil {
				reconnectLog := slog.Debug
				if shouldLogRepeatedFailure(failCount) {
					reconnectLog = slog.Warn
				}
				reconnectLog("Failed to reconnect downstream", "client", c.name, "err", redactURLCredentials(rErr), "failures", failCount)
				continue
			}
			slog.Info("Reconnected downstream", "client", c.name, "afterFailures", failCount)
			failCount = 0
			c.health.Store(int32(healthOK))
		}
	}
}

// toolFilterFunc builds the tool exposure predicate for one client.
//
// With no filter every tool is exposed. mode=allow exposes exactly the listed
// tools (validation requires a non-empty list) and mode=block hides the listed
// ones. An unrecognized mode cannot come from a validated config - it means
// the OptionsV2 was built programmatically - so it fails closed, exposing
// nothing, with a warning.
func toolFilterFunc(name string, options *OptionsV2) func(string) bool {
	if options == nil {
		return func(string) bool { return true }
	}
	filter := catalogFilterFunc(name, "toolFilter", "tool", options.ToolFilter)
	return func(toolName string) bool { return filter(toolName) }
}

// resourceFilterFunc builds the resource and resource-template exposure
// predicate. It is called with every key an entry can be listed under (URI and
// name for a resource, uriTemplate and name for a template).
func resourceFilterFunc(name string, options *OptionsV2) func(...string) bool {
	if options == nil {
		return func(...string) bool { return true }
	}
	return catalogFilterFunc(name, "resourceFilter", "resource", options.ResourceFilter)
}

// promptFilterFunc builds the prompt exposure predicate, keyed by prompt name.
func promptFilterFunc(name string, options *OptionsV2) func(...string) bool {
	if options == nil {
		return func(...string) bool { return true }
	}
	return catalogFilterFunc(name, "promptFilter", "prompt", options.PromptFilter)
}

// catalogFilterFunc builds an allow/block predicate over one kind of catalog
// entry. An entry passes in allow mode when any of its keys is listed, and is
// hidden in block mode when any of its keys is listed. mode=allow with an
// empty list is rejected by validation. An unrecognized mode cannot come from
// a validated config - it means the OptionsV2 was built programmatically - so
// it fails closed, exposing nothing, with a warning.
func catalogFilterFunc(name, field, kind string, filter *FilterConfig) func(...string) bool {
	if filter == nil {
		return func(...string) bool { return true }
	}
	filterSet := make(map[string]struct{}, len(filter.List))
	for _, entry := range filter.List {
		filterSet[entry] = struct{}{}
	}
	listed := func(keys []string) bool {
		for _, key := range keys {
			if _, ok := filterSet[key]; ok {
				return true
			}
		}
		return false
	}
	switch mode := FilterMode(strings.ToLower(string(filter.Mode))); mode {
	case FilterModeAllow:
		return func(keys ...string) bool {
			inList := listed(keys)
			if !inList {
				slog.Debug("Ignoring "+kind+" not in allow list", "client", name, "keys", keys)
			}
			return inList
		}
	case FilterModeBlock:
		return func(keys ...string) bool {
			inList := listed(keys)
			if inList {
				slog.Debug("Ignoring "+kind+" in block list", "client", name, "keys", keys)
			}
			return !inList
		}
	default:
		slog.Warn("Unknown "+field+" mode, hiding all "+kind+"s", "client", name, "mode", mode)
		return func(...string) bool { return false }
	}
}

// resourceReadAllowed reports whether a concrete URI may be read. Listed
// resources are filtered at registration, but mcp-go routes a read of a URI
// that is not registered to the first resource template that matches it, so a
// URI hidden by a block list could still be served through an exposed
// template. In block mode the URI is therefore checked again here, against the
// listed URIs and the URIs of resources that were hidden by name, after
// normalization (see normalizeResourceURI). Allow mode needs no check: a read
// can only reach a resource or template that was explicitly allowed.
func (c *Client) resourceReadAllowed(uri string) bool {
	c.blockedMu.RLock()
	defer c.blockedMu.RUnlock()
	if c.blockedURIs == nil {
		return true
	}
	_, blocked := c.blockedURIs[normalizeResourceURI(uri)]
	return !blocked
}

// setBlockedURIs records the normalized URIs a block-mode resourceFilter
// hides: the listed entries plus hidden resources' URIs. It is a no-op without
// a block-mode filter.
func (c *Client) setBlockedURIs(hidden []string) {
	if c.options == nil || c.options.ResourceFilter == nil ||
		FilterMode(strings.ToLower(string(c.options.ResourceFilter.Mode))) != FilterModeBlock {
		return
	}
	blocked := make(map[string]struct{}, len(c.options.ResourceFilter.List)+len(hidden))
	for _, uri := range c.options.ResourceFilter.List {
		blocked[normalizeResourceURI(uri)] = struct{}{}
	}
	for _, uri := range hidden {
		blocked[normalizeResourceURI(uri)] = struct{}{}
	}
	c.blockedMu.Lock()
	c.blockedURIs = blocked
	c.blockedMu.Unlock()
}

// normalizeResourceURI gives equivalent spellings of a URI one form, so a
// block list cannot be sidestepped by "test://echo/%73ecret" or
// "TEST://Host/secret": the fragment is dropped, scheme and host are
// lower-cased, percent-encoded unreserved characters are decoded and other
// percent-encodings are upper-cased (RFC 3986 section 6.2.2). The path keeps
// its case, since it is case-sensitive.
func normalizeResourceURI(raw string) string {
	// Decoding can form a new escape ("%7%30" becomes "%70"), so repeat until
	// the form is stable. Each pass shortens the string or leaves it unchanged.
	for {
		normalized := normalizeResourceURIOnce(raw)
		if normalized == raw {
			return normalized
		}
		raw = normalized
	}
}

func normalizeResourceURIOnce(raw string) string {
	raw, _, _ = strings.Cut(raw, "#")
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '%' && i+2 < len(raw) && isHexDigit(raw[i+1]) && isHexDigit(raw[i+2]) {
			decoded := unhex(raw[i+1])<<4 | unhex(raw[i+2])
			if isUnreserved(decoded) {
				b.WriteByte(decoded)
			} else {
				b.WriteByte('%')
				b.WriteString(strings.ToUpper(raw[i+1 : i+3]))
			}
			i += 2
			continue
		}
		b.WriteByte(raw[i])
	}
	uri := b.String()
	scheme, rest, ok := strings.Cut(uri, ":")
	if !ok || scheme == "" || strings.ContainsAny(scheme, "/?") {
		return uri
	}
	scheme = strings.ToLower(scheme)
	if authority, ok := strings.CutPrefix(rest, "//"); ok {
		end := strings.IndexAny(authority, "/?")
		if end < 0 {
			end = len(authority)
		}
		return scheme + "://" + strings.ToLower(authority[:end]) + authority[end:]
	}
	return scheme + ":" + rest
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func isUnreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

func (c *Client) addToolsToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	toolsRequest := mcp.ListToolsRequest{}
	filterFunc := toolFilterFunc(c.name, c.options)

	cl := c.getClient()
	if cl == nil {
		return errors.New("downstream is not connected")
	}
	// Collect first and replace the whole set at the end. AddTool is an upsert,
	// so adding into the existing set would leave a tool the downstream no
	// longer exposes registered after a reconnect (e.g. it was restarted on a
	// smaller tool set). SetTools replaces, which drops the vanished entries.
	tools := make([]server.ServerTool, 0)
	for {
		listed, err := cl.ListTools(ctx, toolsRequest)
		if err != nil {
			return err
		}
		if listed == nil {
			return fmt.Errorf("<%s> ListTools returned nil response without error", c.name)
		}
		if len(listed.Tools) == 0 {
			break
		}
		slog.Debug("Successfully listed tools", "client", c.name, "count", len(listed.Tools))
		for _, tool := range listed.Tools {
			if !filterFunc(tool.Name) {
				continue
			}
			slog.Debug("Adding tool", "client", c.name, "tool", tool.Name)
			// The proxy implements no tasks/* handling (newMCPServer never
			// enables task capabilities), so a peer-declared execution mode must
			// not select a dispatcher path the route server cannot honour: it
			// would let a plain tools/call be refused, or be accepted as a task
			// whose result can never be read or cancelled. Drop the
			// downstream-supplied execution metadata before republishing.
			tool.Execution = nil
			toolName := tool.Name
			tools = append(tools, server.ServerTool{Tool: tool, Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				result, err := c.callTool(ctx, request)
				if err != nil {
					slog.Error("Tool call failed", "client", c.name, "tool", toolName, "err", err)
				} else if result != nil && result.IsError {
					// A tool-level failure the downstream reported itself, not
					// a transport problem.
					slog.Warn("Tool call failed", "client", c.name, "tool", toolName, "isError", true)
				}
				return result, err
			}})
		}
		if listed.NextCursor == "" {
			break
		}
		toolsRequest.Params.Cursor = listed.NextCursor
	}
	mcpServer.SetTools(tools...)

	return nil
}

func (c *Client) addPromptsToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	cl := c.getClient()
	if cl == nil {
		return errors.New("downstream is not connected")
	}
	promptsRequest := mcp.ListPromptsRequest{}
	filterFunc := promptFilterFunc(c.name, c.options)
	// Collect and replace, so a prompt the downstream dropped is not left
	// registered after a reconnect.
	prompts := make([]server.ServerPrompt, 0)
	for {
		listed, err := cl.ListPrompts(ctx, promptsRequest)
		if err != nil {
			return err
		}
		if listed == nil {
			return fmt.Errorf("<%s> ListPrompts returned nil response without error", c.name)
		}
		if len(listed.Prompts) == 0 {
			break
		}
		slog.Debug("Successfully listed prompts", "client", c.name, "count", len(listed.Prompts))
		for _, prompt := range listed.Prompts {
			if !filterFunc(prompt.Name) {
				continue
			}
			slog.Debug("Adding prompt", "client", c.name, "prompt", prompt.Name)
			// Resolve the live client at call time so a reconnect is
			// transparent to the handler. The per-request bound is applied here
			// too: a stdio downstream shares one pipe for every request, so an
			// unanswered prompt wedges the whole route exactly as an unanswered
			// tool call would.
			prompts = append(prompts, server.ServerPrompt{Prompt: prompt, Handler: func(ctx context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return forward(ctx, c, func(ctx context.Context, live *client.Client) (*mcp.GetPromptResult, error) {
					request.Params.Meta = stripHopMeta(request.Params.Meta)
					return live.GetPrompt(ctx, request)
				})
			}})
		}
		if listed.NextCursor == "" {
			break
		}
		promptsRequest.Params.Cursor = listed.NextCursor
	}
	mcpServer.SetPrompts(prompts...)
	return nil
}

func (c *Client) addResourcesToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	cl := c.getClient()
	if cl == nil {
		return errors.New("downstream is not connected")
	}
	resourcesRequest := mcp.ListResourcesRequest{}
	filterFunc := resourceFilterFunc(c.name, c.options)
	// Collect and replace, so a resource the downstream dropped is not left
	// registered after a reconnect.
	resources := make([]server.ServerResource, 0)
	var hidden []string
	for {
		listed, err := cl.ListResources(ctx, resourcesRequest)
		if err != nil {
			return err
		}
		if listed == nil {
			return fmt.Errorf("<%s> ListResources returned nil response without error", c.name)
		}
		if len(listed.Resources) == 0 {
			break
		}
		slog.Debug("Successfully listed resources", "client", c.name, "count", len(listed.Resources))
		for _, resource := range listed.Resources {
			if !filterFunc(resource.URI, resource.Name) {
				hidden = append(hidden, resource.URI)
				continue
			}
			slog.Debug("Adding resource", "client", c.name, "resource", resource.Name)
			resources = append(resources, server.ServerResource{Resource: resource, Handler: func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return c.readResource(ctx, request)
			}})
		}
		if listed.NextCursor == "" {
			break
		}
		resourcesRequest.Params.Cursor = listed.NextCursor

	}
	c.setBlockedURIs(hidden)
	mcpServer.SetResources(resources...)
	return nil
}

func (c *Client) addResourceTemplatesToServer(ctx context.Context, mcpServer *server.MCPServer) error {
	cl := c.getClient()
	if cl == nil {
		return errors.New("downstream is not connected")
	}
	resourceTemplatesRequest := mcp.ListResourceTemplatesRequest{}
	filterFunc := resourceFilterFunc(c.name, c.options)
	// Collect and replace, so a template the downstream dropped is not left
	// registered after a reconnect.
	resourceTemplates := make([]server.ServerResourceTemplate, 0)
	for {
		listed, err := cl.ListResourceTemplates(ctx, resourceTemplatesRequest)
		if err != nil {
			return err
		}
		if listed == nil || len(listed.ResourceTemplates) == 0 {
			break
		}
		slog.Debug("Successfully listed resource templates", "client", c.name, "count", len(listed.ResourceTemplates))
		for _, resourceTemplate := range listed.ResourceTemplates {
			// A downstream that omits uriTemplate (or sends null) decodes to a
			// nil URITemplate, which mcp-go dereferences when it builds the
			// catalog key (entry.Template.URITemplate.Raw()). Registering it
			// panics the whole proxy - the shared process serves every route -
			// so a template without a usable URI template is skipped instead.
			if resourceTemplate.URITemplate == nil || resourceTemplate.URITemplate.Raw() == "" {
				slog.Warn("Skipping resource template without a uriTemplate", "client", c.name, "template", resourceTemplate.Name)
				continue
			}
			if !filterFunc(resourceTemplate.URITemplate.Raw(), resourceTemplate.Name) {
				continue
			}
			slog.Debug("Adding resource template", "client", c.name, "template", resourceTemplate.Name)
			resourceTemplates = append(resourceTemplates, server.ServerResourceTemplate{Template: resourceTemplate, Handler: func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return c.readResource(ctx, request)
			}})
		}
		if listed.NextCursor == "" {
			break
		}
		resourceTemplatesRequest.Params.Cursor = listed.NextCursor
	}
	mcpServer.SetResourceTemplates(resourceTemplates...)
	return nil
}

// readResource resolves the live client at call time so a reconnect is
// transparent to the handler. Like callTool it applies the configured
// per-request bound: an unanswered resource read occupies the single stdio
// channel and takes the whole downstream unhealthy for every caller.
func (c *Client) readResource(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	if !c.resourceReadAllowed(request.Params.URI) {
		return nil, fmt.Errorf("resource %q is not available", request.Params.URI)
	}
	readResource, err := forward(ctx, c, func(ctx context.Context, cl *client.Client) (*mcp.ReadResourceResult, error) {
		request.Params.Meta = stripHopMeta(request.Params.Meta)
		return cl.ReadResource(ctx, request)
	})
	if err != nil {
		return nil, err
	}
	return readResource.Contents, nil
}

func (c *Client) Close() error {
	c.closed.Store(true)
	// End the keepalive loop and any long-lived transport context before
	// closing the transport, so nothing races to probe or rebuild it.
	if c.stopLife != nil {
		c.stopLife()
	}
	c.closeRetired()
	cl := c.getClient()
	if cl == nil {
		return nil
	}
	err := cl.Close()
	// A stdio subprocess that already died, or that exits non-zero when its
	// stdin is closed (many MCP servers do), is not a failure to close it.
	// Reporting it as one would make the proxy exit non-zero after an
	// otherwise clean shutdown.
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
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
	serverOpts := []server.ServerOption{
		// The proxy does not forward resources/subscribe nor emit
		// resources/updated, so only listChanged is advertised.
		server.WithResourceCapabilities(false, true),
		// tools/list_changed and prompts/list_changed are emitted when a
		// downstream's list_changed notification re-registers the catalog.
		server.WithToolCapabilities(true),
		server.WithPromptCapabilities(true),
		server.WithRecovery(),
	}
	mcpServer := server.NewMCPServer(
		name,
		serverConfig.Version,
		serverOpts...,
	)

	var handler http.Handler

	// mcp-go's own DNS-rebinding guard is disabled on both handlers: it only
	// accepts loopback Host values, which breaks the common case of a reverse
	// proxy on the same host forwarding its public Host to a loopback listener.
	// hostGuardMiddleware in http.go enforces the same protection but also
	// allows the mcpProxy.baseURL host.
	switch serverConfig.Type {
	case MCPServerTypeSSE:
		handler = server.NewSSEServer(
			mcpServer,
			server.WithStaticBasePath(name),
			server.WithBaseURL(serverConfig.BaseURL),
			server.WithSSEDisableLocalhostProtection(true),
		)
	case MCPServerTypeStreamable:
		handler = server.NewStreamableHTTPServer(
			mcpServer,
			server.WithStateLess(true),
			server.WithDisableLocalhostProtection(true),
		)
	default:
		return nil, fmt.Errorf("unknown server type: %s", serverConfig.Type)
	}
	return &Server{
		mcpServer: mcpServer,
		handler:   handler,
	}, nil
}
