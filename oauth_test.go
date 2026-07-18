package main

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
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
			path, addr, err := parseRedirectURI(tt.raw)
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
			_, _, err := parseRedirectURI(tt.raw)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
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
	defer listener.Close()

	_, err = startOAuthCallbackServer(listener.Addr().String(), "/callback", "state", make(chan map[string]string, 1))
	if err == nil {
		t.Fatal("start callback server succeeded on an address already in use")
	}
}

func closeResponse(t *testing.T, response *http.Response) {
	t.Helper()
	_, _ = io.Copy(io.Discard, response.Body)
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
}
