package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

	"github.com/mark3labs/mcp-go/client"
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
// A bare token with no scheme is also accepted.
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

// hostGuardMiddleware is the DNS-rebinding protection for the MCP routes,
// replacing the loopback-only guard mcp-go ships (disabled in newMCPServer)
// with one that also accepts the public host the proxy serves under.
//
// The guard only applies when the connection arrived on a loopback interface -
// a request that came in over a public interface already proves the client
// reached the right address. On loopback the Host header's hostname must be a
// loopback name itself (localhost, 127.0.0.0/8, ::1) or match allowedHost, the
// hostname of mcpProxy.baseURL, so a same-host reverse proxy can forward its
// public Host to a loopback listener.
func hostGuardMiddleware(allowedHost string) MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			localAddr, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
			if localAddr == nil || !isLoopbackAddr(localAddr) {
				next.ServeHTTP(w, r)
				return
			}
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
			if isLoopbackHostname(host) || strings.EqualFold(host, allowedHost) {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "Forbidden: invalid Host header", http.StatusForbidden)
		})
	}
}

// isLoopbackAddr reports whether addr names a loopback interface.
func isLoopbackAddr(addr net.Addr) bool {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP.IsLoopback()
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackHostname reports whether host is a loopback name: "localhost", an
// address in 127.0.0.0/8, or ::1.
func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// startupFailed reports a downstream that could not be started and will not be
// retried, so the rest of the proxy still serves. It is only for terminal
// failures: an attempt about to be retried logs its WARN retry line instead
// (an unreachable backend is not an error), and logging both would emit an
// ERROR for every retry of a backend that is simply not up yet.
//
// The error is redacted before it is logged or returned: it is propagated out
// of the startup errorGroup and logged by main, and a transport error embeds
// the downstream URL, which may carry a credential in its query. The returned
// error is non-nil when panicIfInvalid makes the failure fatal for the whole
// process.
func startupFailed(name string, clientConfig *MCPClientConfigV2, err error) error {
	slog.Error("Failed to start client", "client", name, "err", redactURLCredentials(err))
	if clientConfig.Options.panicIfInvalid() {
		return redactURLCredentials(err)
	}
	return nil
}

// permanentStartupError reports whether retrying cannot help: a downstream that
// needs interactive OAuth authorization stays unconnectable until someone runs
// `mcp-proxy -authorize`, so autoReconnect must stop rather than retry it every
// interval forever and keep hitting the provider. The message already carries
// the authorize command (see oauthAwareError), so the single ERROR is actionable.
//
// Only this case is classified. A plain 401 from a static bearer-token server is
// a transport error indistinguishable from any other connection failure, and a
// missing stdio command is expected to appear later, so both still retry.
func permanentStartupError(err error) bool {
	return client.IsOAuthAuthorizationRequiredError(err)
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

// readHeaderTimeout is how long a client may take to send request headers.
const readHeaderTimeout = 30 * time.Second

func startHTTPServer(config *Config) error {
	baseURL, uErr := url.Parse(config.McpProxy.BaseURL)
	if uErr != nil {
		// baseURL is validated in validateConfig, so this is belt-and-braces;
		// redact anyway since the parse error embeds the URL.
		return redactURLCredentials(uErr)
	}
	// The host DNS-rebinding protection accepts, besides loopback Host values:
	// the public hostname the proxy is published under, so a reverse proxy can
	// forward it to a loopback listener.
	allowedHost := baseURL.Hostname()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var errorGroup errgroup.Group
	httpMux := http.NewServeMux()
	httpServer := &http.Server{
		Addr:    config.McpProxy.Addr,
		Handler: httpMux,
		// Bounds only the request headers, so a client that opens connections
		// and trickles headers (slowloris) cannot pin goroutines and sockets.
		// Read/WriteTimeout stay unset: SSE and streamable-HTTP responses are
		// long-lived streams.
		ReadHeaderTimeout: readHeaderTimeout,
	}
	info := mcp.Implementation{
		Name:    config.McpProxy.Name,
		Version: config.McpProxy.Version,
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
		mountRoute := func(server *Server) {
			mountOnce.Do(func() {
				// Outermost first: recover also guards the middlewares below it,
				// and the logger records requests that auth rejects.
				middlewares := make([]MiddlewareFunc, 0)
				middlewares = append(middlewares, recoverMiddleware(name))
				middlewares = append(middlewares, hostGuardMiddleware(allowedHost))
				if clientConfig.Options.logEnabled() {
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
			})
		}

		errorGroup.Go(func() error {
			slog.Info("Connecting", "client", name)
			autoReconnect := clientConfig.Options.autoReconnect()
			interval := clientConfig.Options.reconnectInterval()

			// Build the server first: it depends only on the config, so it is
			// never retried - and building it before the client means a
			// construction failure cannot leave a stdio subprocess (spawned by
			// newMCPClient) running until shutdown.
			server, sErr := newMCPServer(name, config.McpProxy, clientConfig)
			if sErr != nil {
				return startupFailed(name, clientConfig, sErr)
			}

			var mcpClient *Client
			for {
				// Creating a stdio client already spawns the subprocess, so a
				// missing command fails here rather than while connecting. Both
				// have to obey the same panicIfInvalid policy.
				if mcpClient == nil {
					created, err := newMCPClient(name, clientConfig)
					if err != nil {
						if !autoReconnect || clientConfig.Options.panicIfInvalid() {
							return startupFailed(name, clientConfig, err)
						}
						slog.Warn("Retrying client creation", "client", name, "err", redactURLCredentials(err), "retryIn", interval)
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
				}

				err := mcpClient.addToMCPServer(ctx, info, server.mcpServer)
				if err == nil {
					slog.Info("Connected", "client", name)
					mountRoute(server)
					return nil
				}
				if clientConfig.Options.panicIfInvalid() {
					return startupFailed(name, clientConfig, err)
				}
				if mcpClient.closed.Load() {
					// Shutdown already closed this client; not a startup error.
					return nil
				}
				if !autoReconnect {
					return startupFailed(name, clientConfig, err)
				}
				if permanentStartupError(err) {
					// Retrying cannot help - interactive OAuth is needed. Report
					// it once, with the actionable message, and stop rather than
					// hammering the provider every interval.
					slog.Error("Not retrying, downstream cannot connect without intervention", "client", name, "err", redactURLCredentials(err))
					return nil
				}
				// The backend is simply not up yet: wait and try again. The
				// route is mounted only once it connects, so readiness keeps
				// reporting it as not mounted until then.
				slog.Warn("Retrying connection", "client", name, "err", redactURLCredentials(err), "retryIn", interval)
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
