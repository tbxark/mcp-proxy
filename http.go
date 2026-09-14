package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/sync/errgroup"
)

type MiddlewareFunc func(http.Handler) http.Handler

// chainMiddleware wraps h so that middlewares run in the order given: the
// first one is outermost and sees every request, including those the ones
// after it reject or panic on.
func chainMiddleware(h http.Handler, middlewares ...MiddlewareFunc) http.Handler {
	for _, middleware := range slices.Backward(middlewares) {
		h = middleware(h)
	}
	return h
}

const bearerPrefix = "Bearer "

// bearerToken extracts the credentials from an Authorization header. RFC 7235
// makes the scheme name case-insensitive, so "bearer" is as valid as "Bearer".
// A bare token with no scheme is also accepted, which earlier versions allowed.
func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if len(header) >= len(bearerPrefix) && strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		header = header[len(bearerPrefix):]
	}
	return strings.TrimSpace(header)
}

// tokenAllowed compares against every configured token in constant time, so
// response timing cannot be used to recover a valid one.
func tokenAllowed(tokens []string, candidate string) bool {
	if candidate == "" {
		return false
	}
	allowed := false
	for _, token := range tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(candidate)) == 1 {
			allowed = true
		}
	}
	return allowed
}

// newAuthMiddleware is only attached when at least one token is configured.
func newAuthMiddleware(tokens []string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !tokenAllowed(tokens, bearerToken(r.Header.Get("Authorization"))) {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
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
// an empty body, so it can be used by Docker, reverse proxies, and monitoring
// without speaking MCP or providing the proxy auth token.
//
// A nil readiness func makes the handler report OK as soon as the process
// serves requests (liveness). The readiness endpoint passes one that reports
// 503 while clients are still mounting their routes, and again whenever a
// downstream connection that used to work has broken, so a load balancer can
// route around a proxy whose backends are gone.
func healthHandler(config *Config, readiness func() readinessReport) http.HandlerFunc {
	type healthResponse struct {
		Name        string   `json:"name"`
		ServerCount int      `json:"serverCount"`
		Status      string   `json:"status"`
		Unhealthy   []string `json:"unhealthy,omitempty"`
		Version     string   `json:"version"`
	}
	enabled := 0
	for _, clientConfig := range config.McpServers {
		if clientConfig.Options == nil || !clientConfig.Options.Disabled {
			enabled++
		}
	}
	body := healthResponse{
		Name:        config.McpProxy.Name,
		ServerCount: enabled,
		Status:      "ok",
		Version:     config.McpProxy.Version,
	}
	return func(w http.ResponseWriter, r *http.Request) {
		code, resp := http.StatusOK, body
		if readiness != nil {
			report := readiness()
			switch {
			case !report.started:
				code, resp.Status = http.StatusServiceUnavailable, "initializing"
			case len(report.unhealthy) > 0:
				code, resp.Status, resp.Unhealthy = http.StatusServiceUnavailable, "degraded", report.unhealthy
			case report.mounted == 0 && enabled > 0:
				// Nothing to name as broken, but every MCP route 404s, so this
				// proxy must not stay in a load balancer's rotation.
				code, resp.Status = http.StatusServiceUnavailable, "unavailable"
			}
		}
		// Registered under a "GET" pattern, so ServeMux answers 405 (with Allow)
		// for every other method before this runs. GET patterns match HEAD too.
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(resp)
		case http.MethodHead:
			w.WriteHeader(code)
		}
	}
}

// readinessReport is what /_readyz asks the running proxy about its clients.
type readinessReport struct {
	// started is false until every client has finished connecting, or the
	// startup grace period has expired.
	started bool
	// mounted counts the clients that connected at least once, and so have a
	// route. Zero means the proxy has nothing to serve at all.
	mounted int
	// unhealthy names the clients that connected and later failed their
	// keepalive ping.
	unhealthy []string
}

// fatalStartupError applies the panicIfInvalid half of the per-server failure
// policy: it returns err when that server asked for a fast, fatal failure, and
// nil otherwise. It deliberately does not log, because the caller knows whether
// the failure is terminal (log an error) or about to be retried (log nothing;
// the retry line carries the detail). Logging here unconditionally would emit
// an ERROR for every retry attempt of a backend that is simply not up yet.
func fatalStartupError(clientConfig *MCPClientConfigV2, err error) error {
	if clientConfig.Options.PanicIfInvalid.OrElse(false) {
		return err
	}
	return nil
}

// clientStartupError reports a downstream that could not be started and will
// not be retried, so the rest of the proxy still serves. It returns err when
// panicIfInvalid makes the failure fatal for the whole process.
func clientStartupError(name string, clientConfig *MCPClientConfigV2, err error) error {
	slog.Error("Failed to start client", "client", name, "err", err)
	return fatalStartupError(clientConfig, err)
}

// retryWait sleeps out one reconnect interval, reporting false if the context
// ended first (the proxy is shutting down).
func retryWait(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
	// clients is filled in from the per-server goroutines below.
	var clientsMu sync.Mutex
	clients := make(map[string]*Client, len(config.McpServers))

	// shuttingDown tells the startup goroutines below that shutdown has already
	// walked the clients map, so a client that finishes connecting after that
	// has to close itself instead of being left running.
	var shuttingDown atomic.Bool

	// Unauthenticated health endpoints for liveness/readiness probes.
	var started atomic.Bool
	readiness := func() readinessReport {
		if !started.Load() {
			return readinessReport{}
		}
		report := readinessReport{started: true}
		clientsMu.Lock()
		defer clientsMu.Unlock()
		for name, client := range clients {
			health := client.Health()
			if health == healthUnknown {
				// Created but never connected: it has no route, so there is
				// nothing to route around.
				continue
			}
			report.mounted++
			if health == healthFailed {
				report.unhealthy = append(report.unhealthy, name)
			}
		}
		slices.Sort(report.unhealthy)
		return report
	}
	httpMux.HandleFunc("GET /_healthz", healthHandler(config, nil))
	httpMux.HandleFunc("GET /_readyz", healthHandler(config, readiness))

	for name, clientConfig := range config.McpServers {
		if clientConfig.Options.Disabled {
			slog.Info("Disabled", "client", name)
			continue
		}
		// mountRoute publishes a client's endpoint once it has connected. It
		// runs exactly once per server; a backend that was down at startup and
		// connects later is mounted then, by the retry below.
		var mountOnce sync.Once
		mountRoute := func(mcpClient *Client, server *Server) {
			mountOnce.Do(func() {
				// Outermost first: recover also guards the middlewares below it,
				// and the logger records requests that auth rejects.
				middlewares := make([]MiddlewareFunc, 0)
				middlewares = append(middlewares, recoverMiddleware(name))
				if clientConfig.Options.LogEnabled.OrElse(false) {
					middlewares = append(middlewares, loggerMiddleware(name))
				}
				if len(clientConfig.Options.AuthTokens) > 0 {
					middlewares = append(middlewares, newAuthMiddleware(clientConfig.Options.AuthTokens))
				}
				mcpRoute := path.Join(baseURL.Path, name)
				if !strings.HasPrefix(mcpRoute, "/") {
					mcpRoute = "/" + mcpRoute
				}
				if !strings.HasSuffix(mcpRoute, "/") {
					mcpRoute += "/"
				}
				slog.Info("Handling requests", "client", name, "route", mcpRoute)
				httpMux.Handle(mcpRoute, chainMiddleware(server.handler, middlewares...))
				httpServer.RegisterOnShutdown(func() {
					slog.Info("Shutting down", "client", name)
					if cErr := mcpClient.Close(); cErr != nil {
						slog.Error("Failed to close client", "client", name, "err", cErr)
					}
				})
			})
		}

		errorGroup.Go(func() error {
			slog.Info("Connecting", "client", name)
			autoReconnect := clientConfig.Options.AutoReconnect.OrElse(false)
			interval := clientConfig.Options.reconnectInterval()

			var (
				mcpClient *Client
				server    *Server
			)
			for {
				// Creating a stdio client already spawns the subprocess, so a
				// missing command fails here rather than while connecting. Both
				// have to obey the same panicIfInvalid policy.
				if mcpClient == nil {
					created, err := newMCPClient(name, clientConfig)
					if err != nil {
						// Terminal unless it will be retried: log an error for a
						// failure that ends the attempt, and stay quiet on the
						// retry path (an unreachable backend is not an error).
						if fatal := fatalStartupError(clientConfig, err); fatal != nil {
							slog.Error("Failed to start client", "client", name, "err", err)
							return fatal
						}
						if !autoReconnect {
							slog.Error("Failed to start client", "client", name, "err", err)
							return nil
						}
						slog.Warn("Retrying client creation", "client", name, "err", err, "retryIn", interval)
						if !retryWait(ctx, interval) {
							return nil
						}
						continue
					}
					mcpClient = created
					clientsMu.Lock()
					if shuttingDown.Load() {
						clientsMu.Unlock()
						// shutdown already closed everything it knew about, and
						// this client is not in that map. Nobody else will close
						// it - and for stdio it owns a subprocess.
						slog.Info("Shutting down", "client", name)
						if cErr := mcpClient.Close(); cErr != nil {
							slog.Error("Failed to close client", "client", name, "err", cErr)
						}
						return nil
					}
					clients[name] = mcpClient
					clientsMu.Unlock()

					newServer, sErr := newMCPServer(name, config.McpProxy, clientConfig)
					if sErr != nil {
						// A malformed server definition will not fix itself, so
						// it is never retried.
						return clientStartupError(name, clientConfig, sErr)
					}
					server = newServer
				}

				err := mcpClient.addToMCPServer(ctx, info, server.mcpServer)
				if err == nil {
					slog.Info("Connected", "client", name)
					mountRoute(mcpClient, server)
					return nil
				}
				if fatal := fatalStartupError(clientConfig, err); fatal != nil {
					slog.Error("Failed to start client", "client", name, "err", err)
					return fatal
				}
				if mcpClient.closed.Load() {
					// Shutdown already closed this client; not a startup error.
					return nil
				}
				if !autoReconnect {
					slog.Error("Failed to start client", "client", name, "err", err)
					return nil
				}
				// The backend is simply not up yet: wait and try again. The
				// route is mounted only once it connects, so readiness keeps
				// reporting it as not mounted until then.
				slog.Warn("Retrying connection", "client", name, "err", err, "retryIn", interval)
				if !retryWait(ctx, interval) {
					return nil
				}
			}
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
		shuttingDown.Store(true)
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		var shutdownErrors []error
		if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// An SSE stream never goes idle, so a client that is still
			// connected will always outlast the grace period. Force those
			// connections closed rather than reporting a failed shutdown.
			slog.Warn("Graceful shutdown timed out, closing open connections", "err", err)
			if err := httpServer.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				shutdownErrors = append(shutdownErrors, err)
			}
		}
		clientsMu.Lock()
		defer clientsMu.Unlock()
		for name, client := range clients {
			slog.Info("Shutting down", "client", name)
			if err := client.Close(); err != nil {
				shutdownErrors = append(shutdownErrors, fmt.Errorf("close client %q: %w", name, err))
			}
		}
		return errors.Join(shutdownErrors...)
	}

	// A downstream can be slow for legitimate reasons (npx fetching a package on
	// first run) and the servers that did connect are already serving, so
	// readiness waits only this long for the stragglers.
	grace := config.McpProxy.startupGrace()
	graceTimer := time.NewTimer(grace)
	defer graceTimer.Stop()

	for {
		select {
		case err := <-initializationDone:
			initializationDone = nil
			if err != nil {
				_ = shutdown()
				return fmt.Errorf("failed to initialize clients: %w", err)
			}
			started.Store(true)
			slog.Info("All clients initialized")
		case <-graceTimer.C:
			// Never let one slow downstream keep the proxy out of rotation.
			if started.CompareAndSwap(false, true) {
				slog.Warn("Reporting ready while clients are still connecting", "grace", grace)
			}
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
