package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// buildOAuthConfig turns our JSON-facing OAuthClientConfig into the
// transport.OAuthConfig the mcp-go client library expects, backed by a
// FileTokenStore so tokens survive restarts and are shared between the
// one-off `-authorize` run and the long-running daemon.
//
// If conf has no static ClientID (i.e. dynamic client registration is in
// play), this loads whatever client was previously registered and
// persisted by an earlier `-authorize` run. Without this, a freshly
// started daemon would build an OAuthHandler with an empty ClientID and
// every token refresh would silently fail (client_id="" gets rejected by
// the provider) - the one-off authorize process's dynamic registration
// only ever lived in that process's memory otherwise.
func buildOAuthConfig(serverName string, conf *OAuthClientConfig) (transport.OAuthConfig, error) {
	tokenPath, err := oauthTokenPath(serverName)
	if err != nil {
		return transport.OAuthConfig{}, err
	}
	redirectURI := conf.RedirectURI
	if redirectURI == "" {
		redirectURI = defaultOAuthRedirectURI
	}
	clientID, clientSecret := conf.ClientID, conf.ClientSecret
	if clientID == "" {
		regClientID, regClientSecret, ok, rErr := loadRegisteredClient(serverName)
		if rErr != nil {
			return transport.OAuthConfig{}, rErr
		}
		if ok {
			clientID, clientSecret = regClientID, regClientSecret
		}
	}
	return transport.OAuthConfig{
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		RedirectURI:           redirectURI,
		Scopes:                conf.Scopes,
		TokenStore:            NewFileTokenStore(tokenPath),
		PKCEEnabled:           !conf.PKCEDisabled,
		AuthServerMetadataURL: conf.AuthServerMetadataURL,
	}, nil
}

// runAuthorize performs a one-time interactive OAuth authorization for a
// single configured server: it registers a dynamic client if needed, opens
// a browser to the provider's consent screen, waits for the local redirect
// callback, exchanges the code for a token, and persists it via the same
// FileTokenStore the daemon reads from. Intended to be run by hand, in a
// real desktop session with a browser - never by the unattended daemon.
func runAuthorize(configPath, serverName string, insecure, expandEnv bool, httpHeaders string, httpTimeout int) error {
	config, err := load(configPath, insecure, expandEnv, httpHeaders, httpTimeout)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	clientConfig, ok := config.McpServers[serverName]
	if !ok {
		return fmt.Errorf("no server named %q in config", serverName)
	}
	clientInfo, err := parseMCPClientConfigV2(clientConfig)
	if err != nil {
		return err
	}

	var oauthConf *OAuthClientConfig
	var mcpClient *client.Client
	switch v := clientInfo.(type) {
	case *StdioMCPClientConfig:
		return fmt.Errorf("server %q is a stdio server, OAuth authorization does not apply", serverName)
	case *SSEMCPClientConfig:
		if v.OAuth == nil {
			return fmt.Errorf("server %q has no oauth config; add mcpServers.%s.oauth to config.json first", serverName, serverName)
		}
		oauthConf = v.OAuth
		oc, bErr := buildOAuthConfig(serverName, oauthConf)
		if bErr != nil {
			return bErr
		}
		options := []transport.ClientOption{client.WithHeaders(mcpHTTPHeaders(v.Headers))}
		mcpClient, err = client.NewOAuthSSEClient(v.URL, oc, options...)
	case *StreamableMCPClientConfig:
		if v.OAuth == nil {
			return fmt.Errorf("server %q has no oauth config; add mcpServers.%s.oauth to config.json first", serverName, serverName)
		}
		oauthConf = v.OAuth
		oc, bErr := buildOAuthConfig(serverName, oauthConf)
		if bErr != nil {
			return bErr
		}
		options := []transport.StreamableHTTPCOption{transport.WithHTTPHeaders(mcpHTTPHeaders(v.Headers))}
		if v.Timeout > 0 {
			options = append(options, transport.WithHTTPTimeout(v.Timeout))
		}
		mcpClient, err = client.NewOAuthStreamableHttpClient(v.URL, oc, options...)
	default:
		return errors.New("invalid client type")
	}
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	defer mcpClient.Close()

	redirectURI := oauthConf.RedirectURI
	if redirectURI == "" {
		redirectURI = defaultOAuthRedirectURI
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := mcpClient.Start(ctx); err != nil {
		if authErr := authorizeInteractively(ctx, err, serverName, redirectURI); authErr != nil {
			return authErr
		}
		if err := mcpClient.Start(ctx); err != nil {
			return fmt.Errorf("failed to start client after authorization: %w", err)
		}
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{Name: "mcp-proxy-authorize", Version: BuildVersion}
	if _, err := mcpClient.Initialize(ctx, initRequest); err != nil {
		if authErr := authorizeInteractively(ctx, err, serverName, redirectURI); authErr != nil {
			return authErr
		}
		if _, err := mcpClient.Initialize(ctx, initRequest); err != nil {
			return fmt.Errorf("failed to initialize after authorization: %w", err)
		}
	}

	slog.Info("Authorization successful; restart the daemon to mount the server route", "server", serverName)
	return nil
}

// authorizeInteractively runs the browser + local-callback OAuth dance if
// err indicates authorization is required; otherwise it returns err as-is.
func authorizeInteractively(ctx context.Context, err error, serverName, redirectURI string) error {
	if !client.IsOAuthAuthorizationRequiredError(err) {
		return err
	}
	oauthHandler := client.GetOAuthHandler(err)

	callbackPath, addr, pErr := parseRedirectURI(redirectURI)
	if pErr != nil {
		return pErr
	}

	codeVerifier, err := client.GenerateCodeVerifier()
	if err != nil {
		return fmt.Errorf("failed to generate PKCE code verifier: %w", err)
	}
	codeChallenge := client.GenerateCodeChallenge(codeVerifier)

	state, err := client.GenerateState()
	if err != nil {
		return fmt.Errorf("failed to generate state: %w", err)
	}

	callbackChan := make(chan map[string]string, 1)
	srv, err := startOAuthCallbackServer(addr, callbackPath, state, callbackChan)
	if err != nil {
		return fmt.Errorf("failed to start OAuth callback server: %w", err)
	}
	defer srv.Close()

	if oauthHandler.GetClientID() == "" {
		if err := oauthHandler.RegisterClient(ctx, "mcp-proxy ("+serverName+")"); err != nil {
			return fmt.Errorf("failed to register OAuth client: %w", err)
		}
		if err := saveRegisteredClient(serverName, oauthHandler.GetClientID(), oauthHandler.GetClientSecret()); err != nil {
			return fmt.Errorf("failed to persist registered OAuth client: %w", err)
		}
	}

	authURL, err := oauthHandler.GetAuthorizationURL(ctx, state, codeChallenge)
	if err != nil {
		return fmt.Errorf("failed to build authorization URL: %w", err)
	}

	slog.Info("Opening browser for authorization", "server", serverName, "url", authURL)
	openBrowser(authURL)

	slog.Info("Waiting for authorization callback", "server", serverName, "redirect_uri", redirectURI)
	select {
	case params := <-callbackChan:
		if errMsg := params["error"]; errMsg != "" {
			return fmt.Errorf("authorization denied: %s: %s", errMsg, params["error_description"])
		}
		if params["state"] != state {
			return fmt.Errorf("state mismatch: possible CSRF, expected %s got %s", state, params["state"])
		}
		code := params["code"]
		if code == "" {
			return errors.New("no authorization code received in callback")
		}
		if err := oauthHandler.ProcessAuthorizationResponse(ctx, code, state, codeVerifier); err != nil {
			return fmt.Errorf("failed to exchange authorization code: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for authorization callback: %w", ctx.Err())
	}
}

// oauthAwareError rewrites an OAuthAuthorizationRequiredError into a message
// that tells the operator exactly what to run, instead of a generic
// connection failure. The daemon never attempts the interactive flow itself.
func oauthAwareError(serverName string, err error) error {
	if client.IsOAuthAuthorizationRequiredError(err) {
		return fmt.Errorf("not authorized yet, run: mcp-proxy -authorize %s -config <path>: %w", serverName, err)
	}
	return err
}

func parseRedirectURI(redirectURI string) (path string, addr string, err error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", "", fmt.Errorf("invalid redirect URI %q: %w", redirectURI, err)
	}
	if u.Scheme != "http" {
		return "", "", fmt.Errorf("redirect URI %q must use http", redirectURI)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("redirect URI %q cannot contain user info, query, or fragment", redirectURI)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return "", "", fmt.Errorf("redirect URI %q must include a host", redirectURI)
	}
	ip := net.ParseIP(hostname)
	if !strings.EqualFold(hostname, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", "", fmt.Errorf("redirect URI %q must use localhost or a loopback IP", redirectURI)
	}
	port := u.Port()
	if port == "" {
		return "", "", fmt.Errorf("redirect URI %q must include an explicit port", redirectURI)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", fmt.Errorf("redirect URI %q contains an invalid port", redirectURI)
	}
	if u.Path == "" || u.Path == "/" {
		return "", "", fmt.Errorf("redirect URI %q must include a callback path", redirectURI)
	}
	return u.Path, net.JoinHostPort(hostname, port), nil
}

func startOAuthCallbackServer(addr, callbackPath, expectedState string, callbackChan chan<- map[string]string) (*http.Server, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != callbackPath {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("state") != expectedState {
			http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
			return
		}
		params := make(map[string]string)
		for key, values := range r.URL.Query() {
			if len(values) > 0 {
				params[key] = values[0]
			}
		}
		select {
		case callbackChan <- params:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><body><h1>Authorization received</h1><p>You can close this window and return to the terminal.</p></body></html>`))
		default:
			http.Error(w, "OAuth callback already received", http.StatusConflict)
		}
	})
	srv := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("OAuth callback server error", "err", err)
		}
	}()
	return srv, nil
}

func openBrowser(rawURL string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", rawURL).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	case "darwin":
		err = exec.Command("open", rawURL).Start()
	default:
		err = fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}
	if err != nil {
		slog.Warn("Could not open browser automatically; open the URL manually", "err", err, "url", rawURL)
	}
}
