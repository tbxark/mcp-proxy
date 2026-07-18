package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
