package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

func TestOAuthStorageName(t *testing.T) {
	t.Parallel()

	name, err := oauthStorageName("notion-prod_1")
	if err != nil {
		t.Fatalf("safe storage name: %v", err)
	}
	if name != "notion-prod_1" {
		t.Fatalf("safe name = %q", name)
	}

	unsafeName, err := oauthStorageName("../../credentials")
	if err != nil {
		t.Fatalf("unsafe storage name: %v", err)
	}
	if !strings.HasPrefix(unsafeName, "server-") || strings.ContainsAny(unsafeName, `/\\`) {
		t.Fatalf("unsafe name was not safely hashed: %q", unsafeName)
	}
	repeated, err := oauthStorageName("../../credentials")
	if err != nil || repeated != unsafeName {
		t.Fatalf("storage name is not deterministic: %q, %q, %v", unsafeName, repeated, err)
	}

	if _, err := oauthStorageName(""); err == nil {
		t.Fatal("empty server name was accepted")
	}
}

func TestFileTokenStoreSaveAndGet(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "token.json")
	store := NewFileTokenStore(path)
	ctx := context.Background()

	if _, err := store.GetToken(ctx); !errors.Is(err, transport.ErrNoToken) {
		t.Fatalf("missing token error = %v, want ErrNoToken", err)
	}
	if err := store.SaveToken(ctx, nil); err == nil {
		t.Fatal("saving nil token succeeded")
	}

	first := &transport.Token{AccessToken: "first", TokenType: "Bearer", RefreshToken: "refresh-1"}
	if err := store.SaveToken(ctx, first); err != nil {
		t.Fatalf("save first token: %v", err)
	}
	second := &transport.Token{AccessToken: "second", TokenType: "Bearer", RefreshToken: "refresh-2"}
	if err := store.SaveToken(ctx, second); err != nil {
		t.Fatalf("replace token: %v", err)
	}

	got, err := store.GetToken(ctx)
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	if got.AccessToken != second.AccessToken || got.RefreshToken != second.RefreshToken {
		t.Fatalf("token = %#v, want %#v", got, second)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat token: %v", err)
		}
		if permission := info.Mode().Perm(); permission != 0600 {
			t.Fatalf("token permissions = %o, want 600", permission)
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatalf("stat token directory: %v", err)
		}
		if permission := dirInfo.Mode().Perm(); permission != 0700 {
			t.Fatalf("token directory permissions = %o, want 700", permission)
		}
	}

	temporaryFiles, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".token.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob temporary files: %v", err)
	}
	if len(temporaryFiles) != 0 {
		t.Fatalf("temporary files were not cleaned up: %v", temporaryFiles)
	}
}

func TestFileTokenStoreHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewFileTokenStore(filepath.Join(t.TempDir(), "token.json"))

	if _, err := store.GetToken(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetToken error = %v, want context.Canceled", err)
	}
	if err := store.SaveToken(ctx, &transport.Token{AccessToken: "token"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveToken error = %v, want context.Canceled", err)
	}
}

// A token must be refreshed slightly before its recorded expiry: sent at the
// last instant it reaches the server expired, which the keepalive probe hit
// on every 900s rotation of nextcloud-mcp ("no valid token available").
func TestEarlyRefreshTokenStore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	inner := transport.NewMemoryTokenStore()
	store := earlyRefreshTokenStore{inner}

	cases := []struct {
		name        string
		expiresIn   int64
		remaining   time.Duration
		wantExpired bool
	}{
		{"inside the leeway", 900, 30 * time.Second, true},
		{"outside the leeway", 900, 2 * time.Minute, false},
		{"short-lived token keeps most of its life", 30, 20 * time.Second, false},
		{"short-lived token near expiry", 30, 2 * time.Second, true},
		{"unknown lifetime uses the full leeway", 0, 30 * time.Second, true},
	}
	for _, tc := range cases {
		real := time.Now().Add(tc.remaining)
		if err := inner.SaveToken(ctx, &transport.Token{AccessToken: "a", ExpiresIn: tc.expiresIn, ExpiresAt: real}); err != nil {
			t.Fatal(err)
		}
		got, err := store.GetToken(ctx)
		if err != nil {
			t.Fatalf("%s: GetToken: %v", tc.name, err)
		}
		if got.IsExpired() != tc.wantExpired {
			t.Errorf("%s: IsExpired = %v, want %v", tc.name, got.IsExpired(), tc.wantExpired)
		}
		stored, err := inner.GetToken(ctx)
		if err != nil || stored == nil {
			t.Fatalf("%s: inner GetToken: %v", tc.name, err)
		}
		if !stored.ExpiresAt.Equal(real) {
			t.Errorf("%s: the stored token's expiry was modified", tc.name)
		}
	}

	// A token without an expiry never expires, and errors pass through.
	_ = inner.SaveToken(ctx, &transport.Token{AccessToken: "a"})
	if got, _ := store.GetToken(ctx); got.IsExpired() {
		t.Error("a token without expiry was reported expired")
	}
	if _, err := (earlyRefreshTokenStore{transport.NewMemoryTokenStore()}).GetToken(ctx); !errors.Is(err, transport.ErrNoToken) {
		t.Errorf("empty store err = %v, want ErrNoToken", err)
	}
}

func TestOAuthTokenAndClientPathsDoNotAlias(t *testing.T) {
	isolateUserConfigDir(t)
	const serverName = "notion"
	const collidingName = serverName + ".client"

	tokenPath, err := oauthTokenPath(collidingName)
	if err != nil {
		t.Fatalf("oauthTokenPath: %v", err)
	}
	clientPath, err := oauthClientPath(serverName)
	if err != nil {
		t.Fatalf("oauthClientPath: %v", err)
	}
	if tokenPath == clientPath {
		t.Fatalf("token path of %q aliases the client path of %q: %s", collidingName, serverName, tokenPath)
	}

	// Distinct servers never share a file, in either direction.
	for _, name := range []string{serverName, collidingName, "a.b", "server-abc"} {
		path, err := oauthTokenPath(name)
		if err != nil {
			t.Fatalf("oauthTokenPath(%q): %v", name, err)
		}
		client, err := oauthClientPath(name)
		if err != nil {
			t.Fatalf("oauthClientPath(%q): %v", name, err)
		}
		if path == client {
			t.Errorf("token and client paths alias for %q: %s", name, path)
		}
	}

	// Round trip: each schema is written and read back intact, so neither write
	// clobbered the other's file.
	if err := saveRegisteredClient(serverName, "cid-1", "sec-1", "http://localhost:8090/oauth/callback"); err != nil {
		t.Fatalf("saveRegisteredClient: %v", err)
	}
	if err := NewFileTokenStore(tokenPath).SaveToken(context.Background(), &transport.Token{AccessToken: "tok", RefreshToken: "r"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	clientID, secret, ok, err := loadRegisteredClient(serverName)
	if err != nil || !ok || clientID != "cid-1" || secret != "sec-1" {
		t.Fatalf("registered client after token write = (%q,%q,%v,%v), want (cid-1,sec-1,true,nil)", clientID, secret, ok, err)
	}
	token, err := NewFileTokenStore(tokenPath).GetToken(context.Background())
	if err != nil || token.AccessToken != "tok" {
		t.Fatalf("token after client write = (%v,%v), want token tok", token, err)
	}
}

// A file that holds no access token and no refresh token is not a usable
// credential, so it reads as "no token" and the daemon asks for authorization.
func TestGetTokenRejectsNonTokenFile(t *testing.T) {
	isolateUserConfigDir(t)
	const serverName = "reviewfix-owner"

	path, err := oauthTokenPath(serverName)
	if err != nil {
		t.Fatalf("oauthTokenPath: %v", err)
	}
	writeFile(t, path, `{"clientId":"x"}`)

	if _, err := NewFileTokenStore(path).GetToken(context.Background()); !errors.Is(err, transport.ErrNoToken) {
		t.Fatalf("GetToken on a non-token file = %v, want ErrNoToken", err)
	}
}
