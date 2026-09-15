package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/server"
)

func TestCheckServerAuthStdio(t *testing.T) {
	t.Parallel()

	res := checkServerAuth("tmux", &MCPClientConfigV2{Command: "/bin/true"})
	if res.transport != "stdio" || res.auth != "none" || !res.ok {
		t.Fatalf("stdio result = %+v", res)
	}
}

func TestCheckServerAuthStaticHeader(t *testing.T) {
	t.Parallel()

	res := checkServerAuth("linear", &MCPClientConfigV2{
		URL:     "https://mcp.example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer "},
	})
	if res.auth != "static header" || res.ok {
		t.Fatalf("empty header should be flagged missing, got %+v", res)
	}

	res = checkServerAuth("linear", &MCPClientConfigV2{
		URL:     "https://mcp.example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer sk-live-123"},
	})
	if res.auth != "static header" || !res.ok {
		t.Fatalf("present header should be ok, got %+v", res)
	}
}

func TestCheckServerAuthNoAuth(t *testing.T) {
	t.Parallel()

	res := checkServerAuth("public", &MCPClientConfigV2{URL: "https://mcp.example.com/mcp"})
	if res.auth != "none" || !res.ok {
		t.Fatalf("no-auth server result = %+v", res)
	}
}

func TestCheckServerAuthOAuth(t *testing.T) {
	isolateUserConfigDir(t)

	conf := &MCPClientConfigV2{
		URL:   "https://mcp.example.com/mcp",
		OAuth: &OAuthClientConfig{},
	}

	// No token file on disk yet.
	res := checkServerAuth("notauthed", conf)
	if res.auth != "oauth" || res.ok {
		t.Fatalf("missing token should not be ok, got %+v", res)
	}

	// Expired token, no refresh token.
	writeTestToken(t, "expired", &transport.Token{
		AccessToken: "a",
		ExpiresAt:   time.Now().Add(-time.Hour),
	})
	res = checkServerAuth("expired", conf)
	if res.ok {
		t.Fatalf("expired token without refresh should not be ok, got %+v", res)
	}

	// Expired token, but has a refresh token - still flagged, just with different guidance.
	writeTestToken(t, "expired-refreshable", &transport.Token{
		AccessToken:  "a",
		RefreshToken: "r",
		ExpiresAt:    time.Now().Add(-time.Hour),
	})
	res = checkServerAuth("expired-refreshable", conf)
	if res.ok {
		t.Fatalf("expired token should not be ok even with a refresh token, got %+v", res)
	}

	// Valid, unexpired token.
	writeTestToken(t, "valid", &transport.Token{
		AccessToken: "a",
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	res = checkServerAuth("valid", conf)
	if !res.ok {
		t.Fatalf("unexpired token should be ok, got %+v", res)
	}
}

func writeTestToken(t *testing.T, serverName string, token *transport.Token) {
	t.Helper()
	path, err := oauthTokenPath(serverName)
	if err != nil {
		t.Fatalf("oauthTokenPath: %v", err)
	}
	store := NewFileTokenStore(path)
	if err := store.SaveToken(context.Background(), token); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
}

func TestRunDoctorSkipsDisabledServers(t *testing.T) {
	dir := isolateUserConfigDir(t)

	configPath := dir + "/config.json"
	if err := os.WriteFile(configPath, []byte(`{
		"mcpProxy": {"baseURL":"http://127.0.0.1:9190","addr":":9190","name":"n","version":"1.0.0","type":"streamable-http"},
		"mcpServers": {
			"off": {"url":"https://mcp.example.com/mcp","options":{"disabled":true}}
		}
	}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ok, err := runDoctor(configPath, false, true, "", 10, false)
	if err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
	if !ok {
		t.Fatal("a disabled server should not fail the doctor check")
	}
}

func TestDoctorUpdatesAuthStatusAfterTokenRefresh(t *testing.T) {
	isolateUserConfigDir(t)
	mcpServer := server.NewStreamableHTTPServer(server.NewMCPServer("test", "1.0"))
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metadata":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                   "http://" + r.Host,
				"authorization_endpoint":   "http://" + r.Host + "/authorize",
				"token_endpoint":           "http://" + r.Host + "/token",
				"response_types_supported": []string{"code"},
			})
		case "/token":
			if r.PostFormValue("grant_type") != "refresh_token" || r.PostFormValue("refresh_token") != "refresh" {
				http.Error(w, "unexpected refresh request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`))
		default:
			if r.Header.Get("Authorization") != "Bearer fresh" {
				http.Error(w, "expired token", http.StatusUnauthorized)
				return
			}
			mcpServer.ServeHTTP(w, r)
		}
	}))
	defer remote.Close()
	conf := &MCPClientConfigV2{
		TransportType: MCPClientTypeStreamable,
		URL:           remote.URL + "/mcp",
		OAuth:         &OAuthClientConfig{ClientID: "test", AuthServerMetadataURL: remote.URL + "/metadata"},
	}
	writeTestToken(t, "refreshable", &transport.Token{
		AccessToken: "expired", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour),
	})
	res := checkServerAuth("refreshable", conf)
	if res.ok || !strings.HasPrefix(res.status, "EXPIRED") {
		t.Fatalf("expected expired token before live check, got %+v", res)
	}
	checkServerLive(&res, conf)
	if res.live != "ok (connected)" || !res.ok || !strings.HasPrefix(res.status, "ok (expires in") {
		t.Fatalf("expected refreshed auth status after live check, got %+v", res)
	}

	// A valid token must not hide a later connection failure.
	remote.Close()
	checkServerLive(&res, conf)
	if res.ok || !strings.HasPrefix(res.live, "FAILED:") {
		t.Fatalf("expected failed live check, got %+v", res)
	}
}
