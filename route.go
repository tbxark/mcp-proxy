package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/sync/singleflight"
)

// routeForServer returns the HTTP path a named server's route is (or will
// be) mounted at under baseURL, always with a trailing slash so it matches
// as a subtree.
func routeForServer(baseURL *url.URL, name string) string {
	mcpRoute := path.Join(baseURL.Path, name)
	if !strings.HasPrefix(mcpRoute, "/") {
		mcpRoute = "/" + mcpRoute
	}
	if !strings.HasSuffix(mcpRoute, "/") {
		mcpRoute += "/"
	}
	return mcpRoute
}

// notReadyHandler responds to every request with a clear, immediate error
// instead of a bare 404, for a server with no working connection right now
// (most commonly: no valid OAuth token yet - see err, which for that case
// already carries the exact `-authorize` command to run, courtesy of
// oauthAwareError).
func notReadyHandler(name string, err error) http.Handler {
	message := fmt.Sprintf("mcp-proxy: server %q is not available: %v\n", name, err)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, message, http.StatusServiceUnavailable)
	})
}

// wrapHandler applies a server's configured middlewares (panic recovery,
// request logging, per-server bearer tokens) around its proxying handler.
func wrapHandler(name string, clientConfig *MCPClientConfigV2, srv *Server) http.Handler {
	middlewares := make([]MiddlewareFunc, 0)
	middlewares = append(middlewares, recoverMiddleware(name))
	if clientConfig.Options.LogEnabled.OrElse(false) {
		middlewares = append(middlewares, loggerMiddleware(name))
	}
	if len(clientConfig.Options.AuthTokens) > 0 {
		middlewares = append(middlewares, newAuthMiddleware(clientConfig.Options.AuthTokens))
	}
	return chainMiddleware(srv.handler, middlewares...)
}

// serverRoute is the handler mounted for a server that didn't successfully
// connect at startup. It implements the retry design in
// docs/OAUTH_LIFECYCLE.md: every request against it first does a purely
// local, no-network check (the same one -auth-status performs, via
// checkServerAuth) - if there's plausibly no usable credential yet, it
// fails immediately with no connection attempt, so retrying on every
// request to a still-unauthorized server costs nothing and leaks nothing.
// If a credential now looks present, it attempts a real connect, with
// concurrent requests coalesced through sf into a single attempt. On
// success it becomes a plain pass-through to the now-working handler, and
// this machinery is never consulted again for this server - route mounting
// happens exactly once, at startup; only the internal ready/not-ready state
// changes afterward.
type serverRoute struct {
	name         string
	clientConfig *MCPClientConfigV2
	proxyConfig  *MCPProxyConfigV2
	info         mcp.Implementation
	ctx          context.Context

	sf singleflight.Group

	mu     sync.RWMutex
	client *Client      // nil until a connect attempt succeeds
	ready  http.Handler // nil until a connect attempt succeeds
}

func newServerRoute(ctx context.Context, name string, clientConfig *MCPClientConfigV2, proxyConfig *MCPProxyConfigV2, info mcp.Implementation) *serverRoute {
	return &serverRoute{
		name:         name,
		clientConfig: clientConfig,
		proxyConfig:  proxyConfig,
		info:         info,
		ctx:          ctx,
	}
}

func (r *serverRoute) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	ready := r.ready
	r.mu.RUnlock()
	if ready != nil {
		ready.ServeHTTP(w, req)
		return
	}

	if auth := checkServerAuth(r.name, r.clientConfig); !auth.ok {
		notReadyHandler(r.name, errors.New(auth.status)).ServeHTTP(w, req)
		return
	}

	v, err, _ := r.sf.Do("connect", r.connect)
	if err != nil {
		notReadyHandler(r.name, err).ServeHTTP(w, req)
		return
	}
	v.(http.Handler).ServeHTTP(w, req)
}

// connect builds a fresh client and attempts to connect and initialize it.
// singleflight guarantees at most one call runs at a time per route:
// concurrent requests that find a connect already in progress wait for its
// result instead of each starting their own. On success the result is
// cached in r.ready and this is never called again for this route; on
// failure, the half-built client is closed here so a broken server being
// retried on every request doesn't leak a connection per attempt.
func (r *serverRoute) connect() (any, error) {
	r.mu.RLock()
	if r.ready != nil {
		h := r.ready
		r.mu.RUnlock()
		return h, nil
	}
	r.mu.RUnlock()

	mcpClient, err := newMCPClient(r.name, r.clientConfig)
	if err != nil {
		return nil, err
	}
	srv, err := newMCPServer(r.name, r.proxyConfig, r.clientConfig)
	if err != nil {
		_ = mcpClient.Close()
		return nil, err
	}
	slog.Info("Connecting (retry)", "client", r.name)
	if err := mcpClient.addToMCPServer(r.ctx, r.info, srv.mcpServer); err != nil {
		slog.Error("Retry failed to add client to server", "client", r.name, "err", err)
		_ = mcpClient.Close()
		return nil, err
	}
	slog.Info("Connected (retry)", "client", r.name)

	handler := wrapHandler(r.name, r.clientConfig, srv)
	r.mu.Lock()
	r.client = mcpClient
	r.ready = handler
	r.mu.Unlock()
	return handler, nil
}

// Close closes the underlying client if this route ever successfully
// connected; a no-op otherwise. Safe to call concurrently with an
// in-flight connect: the daemon shutdown path (http.go) cancels ctx before
// calling this, so an attempt racing shutdown will normally fail on the
// now-canceled context rather than land a client this never closes -
// see docs/OAUTH_LIFECYCLE.md.
func (r *serverRoute) Close() error {
	r.mu.RLock()
	c := r.client
	r.mu.RUnlock()
	if c == nil {
		return nil
	}
	return c.Close()
}
