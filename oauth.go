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
	clientID, clientSecret := conf.ClientID, conf.ClientSecret
	if clientID == "" && !conf.ignoreSavedClient {
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
		RedirectURI:           oauthRedirectURI(conf),
		Scopes:                conf.Scopes,
		TokenStore:            earlyRefreshTokenStore{NewFileTokenStore(tokenPath)},
		PKCEEnabled:           !conf.PKCEDisabled,
		AuthServerMetadataURL: conf.AuthServerMetadataURL,
	}, nil
}

// runAuthorize performs a one-time interactive OAuth authorization for a
// single configured server: it registers a dynamic client if needed, opens
// a browser to the provider's consent screen, waits for the local redirect
// callback, exchanges the code for a token, and persists it via the same
// FileTokenStore the daemon reads from. Intended to be run by hand, in a
// real desktop session with a browser - never by the unattended daemon. On
// a machine without a browser the URL it logs can be opened on another
// device, provided the callback reaches this process (oauth.callbackListen).
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
	switch v := clientInfo.(type) {
	case *StdioMCPClientConfig:
		return fmt.Errorf("server %q is a stdio server, OAuth authorization does not apply", serverName)
	case *SSEMCPClientConfig:
		if v.OAuth == nil {
			return fmt.Errorf("server %q has no oauth config; add mcpServers.%s.oauth to config.json first", serverName, serverName)
		}
		oauthConf = v.OAuth
	case *StreamableMCPClientConfig:
		if v.OAuth == nil {
			return fmt.Errorf("server %q has no oauth config; add mcpServers.%s.oauth to config.json first", serverName, serverName)
		}
		oauthConf = v.OAuth
	default:
		return errors.New("invalid client type")
	}
	// Decided before the client is built: a saved client that has to be
	// replaced must not lend its credentials to the registration of the new
	// one.
	oauthConf.ignoreSavedClient, err = savedClientNeedsReplacing(serverName, oauthConf)
	if err != nil {
		return err
	}
	mcpClient, err := newRawClient(serverName, clientInfo)
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}
	defer func() { _ = mcpClient.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := mcpClient.Start(ctx); err != nil {
		if authErr := authorizeInteractively(ctx, err, serverName, oauthConf); authErr != nil {
			return authErr
		}
		if err := mcpClient.Start(ctx); err != nil {
			return fmt.Errorf("failed to start client after authorization: %w", redactURLCredentials(err))
		}
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{Name: "mcp-proxy-authorize", Version: BuildVersion}
	if _, err := mcpClient.Initialize(ctx, initRequest); err != nil {
		if authErr := authorizeInteractively(ctx, err, serverName, oauthConf); authErr != nil {
			return authErr
		}
		if _, err := mcpClient.Initialize(ctx, initRequest); err != nil {
			return fmt.Errorf("failed to initialize after authorization: %w", redactURLCredentials(err))
		}
	}

	slog.Info("Authorization successful; restart the daemon to mount the server route", "server", serverName)
	return nil
}

// authorizeInteractively runs the browser + local-callback OAuth dance if
// err indicates authorization is required; otherwise it returns err (redacted,
// since a transport error embeds the downstream URL) as-is.
func authorizeInteractively(ctx context.Context, err error, serverName string, conf *OAuthClientConfig) error {
	if !client.IsOAuthAuthorizationRequiredError(err) {
		return redactURLCredentials(err)
	}
	oauthHandler := client.GetOAuthHandler(err)

	redirectURI := oauthRedirectURI(conf)
	callbackPath, addr, pErr := parseRedirectURI(redirectURI, conf.CallbackListen != "")
	if pErr != nil {
		return pErr
	}
	if conf.CallbackListen != "" {
		if addr, pErr = parseCallbackListen(conf.CallbackListen); pErr != nil {
			return pErr
		}
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
	defer func() { _ = srv.Close() }()

	saveOnApproval, err := ensureRegisteredClient(ctx, oauthHandler, serverName, redirectURI, conf.ignoreSavedClient)
	if err != nil {
		return err
	}

	authURL, err := oauthHandler.GetAuthorizationURL(ctx, state, codeChallenge)
	if err != nil {
		return fmt.Errorf("failed to build authorization URL: %w", err)
	}

	slog.Info("Opening browser for authorization", "server", serverName, "url", authURL)
	openBrowser(authURL)

	slog.Info("Waiting for authorization callback", "server", serverName, "redirect_uri", redirectURI, "listen", srv.Addr)
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
		// A client that replaces a saved one is saved only once its token
		// has been issued: until then the saved client and its token stay
		// as they were, for the daemon to keep using, and a failed exchange
		// cannot leave the old token paired with the new client.
		if saveOnApproval {
			if err := saveRegisteredClient(serverName, oauthHandler.GetClientID(), oauthHandler.GetClientSecret(), redirectURI); err != nil {
				return fmt.Errorf("failed to persist registered OAuth client: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for authorization callback: %w", ctx.Err())
	}
}

// oauthRedirectURI returns the redirect URI of an oauth block, or the default.
func oauthRedirectURI(conf *OAuthClientConfig) string {
	if conf.RedirectURI == "" {
		return defaultOAuthRedirectURI
	}
	return conf.RedirectURI
}

// savedClientNeedsReplacing reports whether -authorize has to register a new
// dynamic client instead of using the saved one. A client is registered for
// one redirect URI: for any other the provider refuses the authorization
// request, in the browser where -authorize cannot see it. So a saved client
// is only used when it was registered for the configured redirect URI, which
// rules out one saved before the redirect URI was recorded. A clientId from
// the config is the operator's own registration and is never replaced.
func savedClientNeedsReplacing(serverName string, conf *OAuthClientConfig) (bool, error) {
	if conf.ClientID != "" {
		return false, nil
	}
	saved, ok, err := readRegisteredClient(serverName)
	if err != nil || !ok {
		return false, err
	}
	return saved.RedirectURI != oauthRedirectURI(conf), nil
}

// ensureRegisteredClient registers a dynamic client when the handler has no
// client ID. A first registration is saved at once. One that replaces a saved
// client is not: saveOnApproval tells the caller to save it once the sign-in
// has been approved and its token issued.
func ensureRegisteredClient(ctx context.Context, oauthHandler *transport.OAuthHandler, serverName, redirectURI string, replacing bool) (saveOnApproval bool, err error) {
	if oauthHandler.GetClientID() != "" {
		return false, nil
	}
	if replacing {
		slog.Info("Registering a new OAuth client: the saved one was not registered for this redirect URI",
			"server", serverName, "redirect_uri", redirectURI)
	}
	if err := oauthHandler.RegisterClient(ctx, "mcp-proxy ("+serverName+")"); err != nil {
		return false, fmt.Errorf("failed to register OAuth client: %w", err)
	}
	if replacing {
		return true, nil
	}
	if err := saveRegisteredClient(serverName, oauthHandler.GetClientID(), oauthHandler.GetClientSecret(), redirectURI); err != nil {
		return false, fmt.Errorf("failed to persist registered OAuth client: %w", err)
	}
	return false, nil
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

// parseRedirectURI returns the callback path of the redirect URI and its
// host:port. By default the callback listener binds to that host:port, so the
// URI has to be an http loopback URL with an explicit port. With behindProxy
// (oauth.callbackListen is set) the listener binds elsewhere and the URI is
// only where the provider sends the browser: https on any host is allowed as
// well, and the port is optional.
func parseRedirectURI(redirectURI string, behindProxy bool) (path string, addr string, err error) {
	// Echo a redacted form: this error is printed and logged, and the URI could
	// carry a credential in its userinfo or query. A URL that does not parse is
	// replaced entirely, since its raw text cannot be redacted reliably.
	display := redactURLString(redirectURI)
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", "", fmt.Errorf("invalid redirect URI %q", display)
	}
	schemes := "http"
	if behindProxy {
		schemes = "http or https"
	}
	if u.Scheme != "http" && (u.Scheme != "https" || !behindProxy) {
		return "", "", fmt.Errorf("redirect URI %q must use %s", display, schemes)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("redirect URI %q cannot contain user info, query, or fragment", display)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return "", "", fmt.Errorf("redirect URI %q must include a host", display)
	}
	ip := net.ParseIP(hostname)
	if u.Scheme == "http" && !strings.EqualFold(hostname, "localhost") && (ip == nil || !ip.IsLoopback()) {
		if behindProxy {
			// The authorization code travels to this URL, so off loopback
			// it has to be encrypted.
			return "", "", fmt.Errorf("redirect URI %q must use https unless its host is localhost or a loopback IP", display)
		}
		return "", "", fmt.Errorf("redirect URI %q must use localhost or a loopback IP", display)
	}
	port := u.Port()
	if port == "" && !behindProxy {
		return "", "", fmt.Errorf("redirect URI %q must include an explicit port", display)
	}
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", "", fmt.Errorf("redirect URI %q contains an invalid port", display)
		}
	}
	if u.Path == "" || u.Path == "/" {
		return "", "", fmt.Errorf("redirect URI %q must include a callback path", display)
	}
	return u.Path, net.JoinHostPort(hostname, port), nil
}

// parseCallbackListen validates oauth.callbackListen and returns the address
// the callback listener binds to. Like the default address it has to be
// loopback: the listener never binds to a public interface.
func parseCallbackListen(callbackListen string) (addr string, err error) {
	host, port, err := net.SplitHostPort(callbackListen)
	if err != nil {
		return "", errors.New("callback listen address must be host:port")
	}
	if !isLoopbackHostname(host) {
		return "", errors.New("callback listen address must use localhost or a loopback IP")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", errors.New("callback listen address contains an invalid port")
	}
	return net.JoinHostPort(host, strconv.Itoa(portNumber)), nil
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
