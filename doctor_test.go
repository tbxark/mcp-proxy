package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
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
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

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
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

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
