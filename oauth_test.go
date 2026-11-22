package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

func TestParseRedirectURI(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name     string
		raw      string
		wantPath string
		wantAddr string
	}{
		{name: "localhost", raw: "http://localhost:8090/oauth/callback", wantPath: "/oauth/callback", wantAddr: "localhost:8090"},
		{name: "uppercase localhost", raw: "http://LOCALHOST:8090/oauth/callback", wantPath: "/oauth/callback", wantAddr: "LOCALHOST:8090"},
		{name: "IPv4 loopback", raw: "http://127.0.0.1:9000/callback", wantPath: "/callback", wantAddr: "127.0.0.1:9000"},
		{name: "IPv6 loopback", raw: "http://[::1]:9001/callback", wantPath: "/callback", wantAddr: "[::1]:9001"},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			path, addr, err := parseRedirectURI(tt.raw, false)
			if err != nil {
				t.Fatalf("parse redirect URI: %v", err)
			}
			if path != tt.wantPath || addr != tt.wantAddr {
				t.Fatalf("got path=%q addr=%q, want path=%q addr=%q", path, addr, tt.wantPath, tt.wantAddr)
			}
		})
	}

	invalid := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "https", raw: "https://localhost:8090/callback", wantErr: "must use http"},
		{name: "non-loopback", raw: "http://example.com:8090/callback", wantErr: "loopback"},
		{name: "wildcard", raw: "http://0.0.0.0:8090/callback", wantErr: "loopback"},
		{name: "missing port", raw: "http://localhost/callback", wantErr: "explicit port"},
		{name: "missing path", raw: "http://localhost:8090", wantErr: "callback path"},
		{name: "root path", raw: "http://localhost:8090/", wantErr: "callback path"},
		{name: "query", raw: "http://localhost:8090/callback?x=1", wantErr: "query"},
		{name: "userinfo", raw: "http://user@localhost:8090/callback", wantErr: "user info"},
		{name: "invalid port", raw: "http://localhost:70000/callback", wantErr: "invalid port"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseRedirectURI(tt.raw, false)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

// With oauth.callbackListen the redirect URI is only where the provider sends
// the browser, so a public https URL is allowed.
func TestParseRedirectURIBehindProxy(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name     string
		raw      string
		wantPath string
	}{
		{name: "public https", raw: "https://gateway.example.net/oauth/callback", wantPath: "/oauth/callback"},
		{name: "public https with port", raw: "https://gateway.example.net:8443/cb", wantPath: "/cb"},
		{name: "loopback http", raw: "http://localhost:8090/cb", wantPath: "/cb"},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			path, _, err := parseRedirectURI(tt.raw, true)
			if err != nil {
				t.Fatalf("parse redirect URI: %v", err)
			}
			if path != tt.wantPath {
				t.Fatalf("path = %q, want %q", path, tt.wantPath)
			}
		})
	}

	invalid := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "public http", raw: "http://gateway.example.net/cb", wantErr: "must use https"},
		{name: "other scheme", raw: "ftp://gateway.example.net/cb", wantErr: "http or https"},
		{name: "invalid port", raw: "https://gateway.example.net:70000/cb", wantErr: "invalid port"},
		{name: "root path", raw: "https://gateway.example.net/", wantErr: "callback path"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseRedirectURI(tt.raw, true)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseCallbackListen(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"127.0.0.1:8090", "localhost:9000", "[::1]:9001"} {
		if got, err := parseCallbackListen(addr); err != nil || got != addr {
			t.Errorf("parseCallbackListen(%q) = (%q, %v), want it unchanged", addr, got, err)
		}
	}

	invalid := []struct {
		raw     string
		wantErr string
	}{
		{raw: "0.0.0.0:8090", wantErr: "loopback"},
		{raw: ":8090", wantErr: "loopback"},
		{raw: "127.0.0.1", wantErr: "host:port"},
		{raw: "127.0.0.1:70000", wantErr: "invalid port"},
	}
	for _, tt := range invalid {
		if _, err := parseCallbackListen(tt.raw); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("parseCallbackListen(%q) error = %v, want substring %q", tt.raw, err, tt.wantErr)
		}
	}
}

func TestOAuthCallbackServer(t *testing.T) {
	t.Parallel()

	callbacks := make(chan map[string]string, 1)
	srv, err := startOAuthCallbackServer("127.0.0.1:0", "/oauth/callback", "expected-state", callbacks)
	if err != nil {
		t.Fatalf("start callback server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	client := &http.Client{Timeout: 2 * time.Second}
	endpoint := "http://" + srv.Addr + "/oauth/callback"

	response, err := client.Get("http://" + srv.Addr + "/other")
	if err != nil {
		t.Fatalf("unexpected-path callback: %v", err)
	}
	closeResponse(t, response)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unexpected-path status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}

	request, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		t.Fatalf("create POST request: %v", err)
	}
	response, err = client.Do(request)
	if err != nil {
		t.Fatalf("POST callback: %v", err)
	}
	closeResponse(t, response)
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d", response.StatusCode, http.StatusMethodNotAllowed)
	}

	response, err = client.Get(endpoint + "?state=wrong&code=ignored")
	if err != nil {
		t.Fatalf("invalid-state callback: %v", err)
	}
	closeResponse(t, response)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid-state status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	select {
	case callback := <-callbacks:
		t.Fatalf("invalid state consumed callback: %#v", callback)
	default:
	}

	query := url.Values{"state": {"expected-state"}, "code": {"test-code"}}
	response, err = client.Get(endpoint + "?" + query.Encode())
	if err != nil {
		t.Fatalf("valid callback: %v", err)
	}
	closeResponse(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid callback status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	response, err = client.Get(endpoint + "?" + query.Encode())
	if err != nil {
		t.Fatalf("duplicate callback: %v", err)
	}
	closeResponse(t, response)
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate callback status = %d, want %d", response.StatusCode, http.StatusConflict)
	}

	select {
	case callback := <-callbacks:
		if callback["code"] != "test-code" || callback["state"] != "expected-state" {
			t.Fatalf("callback = %#v", callback)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback")
	}
}

func TestOAuthCallbackServerFailsWhenAddressInUse(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	defer func() { _ = listener.Close() }()

	_, err = startOAuthCallbackServer(listener.Addr().String(), "/callback", "state", make(chan map[string]string, 1))
	if err == nil {
		t.Fatal("start callback server succeeded on an address already in use")
	}
}

// -authorize reuses a saved dynamically registered client only when it was
// registered for the configured redirect URI. Otherwise it registers a new
// one, from clean credentials, and leaves the saved one in place until the
// provider has approved the sign-in.
func TestAuthorizeReplacesStaleRegisteredClient(t *testing.T) {
	isolateUserConfigDir(t)
	const serverName = "remote"
	const first = "http://localhost:8090/oauth/callback"
	const second = "http://localhost:8090/callback"

	type registration struct {
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	var registrations atomic.Int32
	var lastRegistration atomic.Pointer[registration]
	mux := http.NewServeMux()
	authServer := httptest.NewServer(mux)
	t.Cleanup(authServer.Close)
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 authServer.URL,
			"authorization_endpoint": authServer.URL + "/authorize",
			"token_endpoint":         authServer.URL + "/token",
			"registration_endpoint":  authServer.URL + "/register",
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var request registration
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		lastRegistration.Store(&request)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"client_id":"client-%d","client_secret":"secret"}`, registrations.Add(1))
	})

	// authorize does what -authorize does before it opens the browser. It
	// returns the client ID the handler ends with and whether that client
	// still has to be saved.
	authorize := func(conf OAuthClientConfig) (string, bool) {
		t.Helper()
		conf.AuthServerMetadataURL = authServer.URL + "/.well-known/oauth-authorization-server"
		replacing, err := savedClientNeedsReplacing(serverName, &conf)
		if err != nil {
			t.Fatalf("saved client needs replacing: %v", err)
		}
		conf.ignoreSavedClient = replacing
		oauthConfig, err := buildOAuthConfig(serverName, &conf)
		if err != nil {
			t.Fatalf("build OAuth config: %v", err)
		}
		handler := transport.NewOAuthHandler(oauthConfig)
		saveOnApproval, err := ensureRegisteredClient(context.Background(), handler, serverName, oauthConfig.RedirectURI, replacing)
		if err != nil {
			t.Fatalf("ensure registered client: %v", err)
		}
		return handler.GetClientID(), saveOnApproval
	}
	savedClient := func() registeredClient {
		t.Helper()
		saved, ok, err := readRegisteredClient(serverName)
		if err != nil || !ok {
			t.Fatalf("read saved client: ok=%v err=%v", ok, err)
		}
		return saved
	}

	if got, pending := authorize(OAuthClientConfig{RedirectURI: first}); got != "client-1" || pending {
		t.Fatalf("first run: client = %q pending = %v, want client-1 saved at once", got, pending)
	}
	if got, _ := authorize(OAuthClientConfig{RedirectURI: first}); got != "client-1" {
		t.Fatalf("same redirect URI: client = %q, want the saved client-1", got)
	}

	// A changed redirect URI gets a new client. It is registered like a first
	// one, without the old client's secret, and the saved client stays until
	// the sign-in is approved, so an abandoned sign-in changes nothing.
	if got, pending := authorize(OAuthClientConfig{RedirectURI: second}); got != "client-2" || !pending {
		t.Fatalf("changed redirect URI: client = %q pending = %v, want a new client-2 not yet saved", got, pending)
	}
	if request := lastRegistration.Load(); len(request.RedirectURIs) != 1 || request.RedirectURIs[0] != second || request.AuthMethod != "none" {
		t.Fatalf("replacement registration = %+v, want redirect URI %q and auth method none", *request, second)
	}
	if saved := savedClient(); saved.ClientID != "client-1" || saved.RedirectURI != first {
		t.Fatalf("saved client before approval = %+v, want client-1 registered with %q", saved, first)
	}

	// A clientId in the config is used as it is, even when it equals a saved
	// client that was registered with another redirect URI.
	if got, pending := authorize(OAuthClientConfig{ClientID: "client-1", RedirectURI: second}); got != "client-1" || pending {
		t.Fatalf("configured clientId: client = %q pending = %v, want client-1 kept", got, pending)
	}

	// A client saved before the redirect URI was recorded is replaced too.
	path, err := oauthClientPath(serverName)
	if err != nil {
		t.Fatalf("client path: %v", err)
	}
	writeFile(t, path, `{"clientId":"legacy"}`)
	if got, pending := authorize(OAuthClientConfig{RedirectURI: first}); got != "client-3" || !pending {
		t.Fatalf("client file without redirect URI: client = %q pending = %v, want a new client-3 not yet saved", got, pending)
	}
	if got := registrations.Load(); got != 3 {
		t.Fatalf("registrations = %d, want 3", got)
	}
}

func closeResponse(t *testing.T, response *http.Response) {
	t.Helper()
	_, _ = io.Copy(io.Discard, response.Body)
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
}

// -authorize returns the first Start/Initialize failure unchanged when
// it is not an auth-required error, so main.go logs it. It must be redacted.
func TestAuthorizeInteractivelyRedactsNonAuthError(t *testing.T) {
	t.Parallel()

	urlErr := &url.Error{Op: "Post", URL: "https://mcp.example.com/mcp?key=AUTH_SECRET", Err: errors.New("refused")}
	got := authorizeInteractively(context.Background(), urlErr, "server", &OAuthClientConfig{})
	if got == nil {
		t.Fatal("expected the non-auth error to be returned")
	}
	if strings.Contains(got.Error(), "AUTH_SECRET") {
		t.Fatalf("-authorize pass-through leaked the credential: %s", got)
	}
}
