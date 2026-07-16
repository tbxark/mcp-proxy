package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Some upstream MCP servers (e.g. mcp.slack.com) gzip their responses even when
// the client never asked for it. Go's transport only auto-decompresses when it
// added "Accept-Encoding: gzip" itself, so those responses reach the JSON
// decoder as raw gzip bytes. newHTTPClient installs a safety net for that.
func TestGzipSafetyNetDecodesUnrequestedGzip(t *testing.T) {
	t.Parallel()

	const body = `{"jsonrpc":"2.0","id":1,"result":{}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		_, _ = io.WriteString(gz, body)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// Setting Accept-Encoding explicitly is what disables Go's automatic
	// decompression, reproducing the case the safety net exists for.
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := newHTTPClient().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body not decoded\n got: %q\nwant: %q", got, body)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("Content-Encoding should be cleared after decoding, got %q", enc)
	}
}

// When Go's own decompression already ran, the safety net must stay out of the
// way rather than attempt a second decode.
func TestGzipSafetyNetDoesNotDoubleDecode(t *testing.T) {
	t.Parallel()

	const body = `{"jsonrpc":"2.0","id":2,"result":{}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") == "" {
			t.Errorf("expected Go to add Accept-Encoding: gzip")
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		_, _ = io.WriteString(gz, body)
	}))
	defer srv.Close()

	resp, err := newHTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body corrupted\n got: %q\nwant: %q", got, body)
	}
}

// Uncompressed responses must pass through untouched.
func TestGzipSafetyNetPassesThroughPlainResponses(t *testing.T) {
	t.Parallel()

	const body = `{"jsonrpc":"2.0","id":3,"result":{}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	resp, err := newHTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body altered\n got: %q\nwant: %q", got, body)
	}
}
