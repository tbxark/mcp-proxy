package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/sync/errgroup"
)

type MiddlewareFunc func(http.Handler) http.Handler

func chainMiddleware(h http.Handler, middlewares ...MiddlewareFunc) http.Handler {
	for _, mw := range middlewares {
		h = mw(h)
	}
	return h
}

func newAuthMiddleware(tokens []string) MiddlewareFunc {
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		tokenSet[token] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(tokens) != 0 {
				token := r.Header.Get("Authorization")
				token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
				if token == "" {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
				if _, ok := tokenSet[token]; !ok {
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func loggerMiddleware(prefix string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slog.Info("Request", "client", prefix, "method", r.Method, "path", r.URL.Path)
			next.ServeHTTP(w, r)
		})
	}
}

func recoverMiddleware(prefix string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					slog.Error("Recovered from panic", "client", prefix, "err", err)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// healthHandler returns an unauthenticated handler for liveness/readiness
// probes. It responds to GET with a small JSON status document and to HEAD with
// an empty 200 body, so it can be used by Docker, reverse proxies, and
// monitoring without speaking MCP or providing the proxy auth token.
func healthHandler(config *Config) http.HandlerFunc {
	type healthResponse struct {
		Name        string `json:"name"`
		ServerCount int    `json:"serverCount"`
		Status      string `json:"status"`
		Version     string `json:"version"`
	}
	body := healthResponse{
		Name:        config.McpProxy.Name,
		ServerCount: len(config.McpServers),
		Status:      "ok",
		Version:     config.McpProxy.Version,
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(body)
		case http.MethodHead:
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	}
}

// connectAndMount runs a single server's connect/initialize sequence
// against an already-built client and server pair and mounts its HTTP
// route on httpMux either way: on success, the real proxying handler; on
// failure, a serverRoute that retries lazily on each subsequent request
// (see docs/OAUTH_LIFECYCLE.md and route.go) instead of requiring a
// restart. The route always exists either way, so a request against it
// always gets a clear answer instead of a bare 404. It's the per-server
// body of the startup connect loop, factored out so the same sequence can
// be re-run later for a single server without repeating the whole loop.
//
// Returns the serverRoute mounted on failure (nil on success) so the
// caller can track it and, at shutdown, close its client if it ever
// connects.
func connectAndMount(ctx context.Context, name string, clientConfig *MCPClientConfigV2, proxyConfig *MCPProxyConfigV2, info mcp.Implementation, mcpClient *Client, srv *Server, baseURL *url.URL, httpMux *http.ServeMux) (*serverRoute, error) {
	mcpRoute := routeForServer(baseURL, name)

	slog.Info("Connecting", "client", name)
	addErr := mcpClient.addToMCPServer(ctx, info, srv.mcpServer)
	if addErr != nil {
		slog.Error("Failed to add client to server", "client", name, "err", addErr)
		slog.Info("Mounting not-ready route", "client", name, "route", mcpRoute)
		route := newServerRoute(ctx, name, clientConfig, proxyConfig, info)
		httpMux.Handle(mcpRoute, route)
		return route, addErr
	}
	slog.Info("Connected", "client", name)
	slog.Info("Handling requests", "client", name, "route", mcpRoute)
	httpMux.Handle(mcpRoute, wrapHandler(name, clientConfig, srv))
	return nil, nil
}

func startHTTPServer(config *Config) error {
	baseURL, uErr := url.Parse(config.McpProxy.BaseURL)
	if uErr != nil {
		return uErr
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var errorGroup errgroup.Group
	httpMux := http.NewServeMux()
	httpServer := &http.Server{
		Addr:    config.McpProxy.Addr,
		Handler: httpMux,
	}
	info := mcp.Implementation{
		Name: config.McpProxy.Name,
	}
	clients := make(map[string]*Client, len(config.McpServers))
	var routesMu sync.Mutex
	var routes []*serverRoute

	// Unauthenticated health endpoints for liveness/readiness probes.
	health := healthHandler(config)
	httpMux.HandleFunc("/_healthz", health)
	httpMux.HandleFunc("/_readyz", health)

	for name, clientConfig := range config.McpServers {
		if clientConfig.Options.Disabled {
			slog.Info("Disabled", "client", name)
			continue
		}
		mcpClient, err := newMCPClient(name, clientConfig)
		if err != nil {
			return err
		}
		server, err := newMCPServer(name, config.McpProxy, clientConfig)
		if err != nil {
			return err
		}
		clients[name] = mcpClient
		errorGroup.Go(func() error {
			route, connErr := connectAndMount(ctx, name, clientConfig, config.McpProxy, info, mcpClient, server, baseURL, httpMux)
			if route != nil {
				routesMu.Lock()
				routes = append(routes, route)
				routesMu.Unlock()
			}
			if connErr != nil && clientConfig.Options.PanicIfInvalid.OrElse(false) {
				return connErr
			}
			return nil
		})
	}

	initializationDone := make(chan error, 1)
	go func() {
		initializationDone <- errorGroup.Wait()
	}()

	serverDone := make(chan error, 1)
	go func() {
		slog.Info("Starting server", "type", config.McpProxy.Type, "addr", config.McpProxy.Addr)
		hErr := httpServer.ListenAndServe()
		if errors.Is(hErr, http.ErrServerClosed) {
			hErr = nil
		}
		serverDone <- hErr
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	shutdown := func() error {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		var shutdownErrors []error
		if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			shutdownErrors = append(shutdownErrors, err)
		}
		for name, client := range clients {
			slog.Info("Shutting down", "client", name)
			if err := client.Close(); err != nil {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("close client %q: %w", name, err))
			}
		}
		for _, route := range routes {
			if err := route.Close(); err != nil {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("close client %q: %w", route.name, err))
			}
		}
		return errors.Join(shutdownErrors...)
	}

	for {
		select {
		case err := <-initializationDone:
			initializationDone = nil
			if err != nil {
				_ = shutdown()
				return fmt.Errorf("failed to initialize clients: %w", err)
			}
			slog.Info("All clients initialized")
		case err := <-serverDone:
			_ = shutdown()
			if err != nil {
				return fmt.Errorf("HTTP server failed: %w", err)
			}
			return nil
		case <-sigChan:
			slog.Info("Shutdown signal received")
			return shutdown()
		}
	}
}
